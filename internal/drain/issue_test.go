package drain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
	"bd-auto/internal/worktree"
)

// --- harness ---

// testRepo builds a throwaway repo with one commit. Global and system git
// config are pointed at /dev/null so the developer's own settings, hooks
// included, cannot change what these tests observe.
func testRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet", "-b", "main", ".")
	mustGit(t, dir, "config", "user.name", "bd-auto test")
	mustGit(t, dir, "config", "user.email", "test@example.invalid")
	mustGit(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "--quiet", "-m", "seed")
	return dir
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// fakeIssues is the bd half of the harness. It is a map rather than a process
// because every property under test here is a property of the loop, and a real
// bd would make each of these tests a database fixture.
type fakeIssues struct {
	mu     sync.Mutex
	status map[string]string
	titles map[string]string
	parent map[string]string
	deps   map[string][]bd.Ref
	// defers is what bd hides from a ready front until a date. It is a map
	// rather than a flag on the status because a deferred issue is still open
	// everywhere else bd reports on it.
	defers    map[string]time.Time
	notes     []string
	parked    []string
	closed    []string
	claimed   []string
	resets    int
	fail      error
	closeFail map[string]error

	// created records every Create request, and createFail makes them all fail
	// — which is how a test reaches the barrier's "bd refused the create" path
	// without an unreachable database.
	created    []bd.NewIssue
	createFail error

	// notesFail makes AppendNotes fail, which is how the fake reproduces the
	// one property of bd the engine may not rely on: a note write that does not
	// stick. Show does not return Notes either, by default, so nothing the
	// engine writes to the issue can be read back — the same hole beads'
	// post-checkout hook leaves when it imports issues.jsonl over the database.
	// showsNotes opens it, for the one read that happens in the same round as
	// the write and therefore has no worktree creation to be reverted by.
	notesFail  error
	issueNotes map[string]string
	notesShown bool

	// onShow runs at the start of every Show, and is how a test reproduces the
	// one thing real bd does that the engine cannot see coming: a read command
	// re-exports .beads/issues.jsonl and stages it. It is set before the engine
	// runs and never after, so it is read without the lock.
	onShow func(id string)

	// readyCalls counts Ready, and readyFailFrom is the call it starts failing
	// on. Together they make bd go unreachable at a chosen point in a run —
	// between two waves, say — which is the only way to reach the wave loop's
	// error exits without an unreachable database.
	readyCalls    int
	readyFailFrom int
	readyErr      error

	// descs is what an issue says, which the lookalike index in triage.go reads
	// and nothing else does. Empty for every issue a test does not set, so the
	// index sees titles alone unless a test is about the descriptions.
	descs map[string]string
}

func (f *fakeIssues) Claim(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if f.status[id] != "open" {
		return fmt.Errorf("%s is %s, not open", id, f.status[id])
	}
	f.status[id] = "in_progress"
	f.claimed = append(f.claimed, id)
	return nil
}

// describe gives an issue the text a duplicate would be matched against.
func (f *fakeIssues) describe(id, title, desc string) *fakeIssues {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.descs == nil {
		f.descs = map[string]string{}
	}
	f.titles[id] = title
	f.descs[id] = desc
	return f
}

// notesOf is everything written to one issue's notes, joined as bd joins them.
func (f *fakeIssues) notesOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issueNotes[id]
}

// onEveryShow installs a side effect on the fake's reads.
func (f *fakeIssues) onEveryShow(fn func(id string)) *fakeIssues {
	f.onShow = fn
	return f
}

// showsNotes makes Show return what was appended to an issue, which is what bd
// does for a note read back before anything reimports over it.
func (f *fakeIssues) showsNotes() *fakeIssues {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notesShown = true
	return f
}

// readyCount is how many times the planner has asked bd what is ready. It is
// what says a wave that tops itself up asks once per freed slot rather than
// polling.
func (f *fakeIssues) readyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readyCalls
}

// failReadyFrom makes the nth Ready call, and every one after it, fail.
func (f *fakeIssues) failReadyFrom(n int, err error) *fakeIssues {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readyFailFrom, f.readyErr = n, err
	return f
}

func newIssues(ids ...string) *fakeIssues {
	f := &fakeIssues{
		status: map[string]string{}, titles: map[string]string{},
		parent: map[string]string{}, deps: map[string][]bd.Ref{},
		defers: map[string]time.Time{}, issueNotes: map[string]string{}, closeFail: map[string]error{},
	}
	for _, id := range ids {
		f.status[id] = "open"
		f.titles[id] = "test issue " + id
	}
	return f
}

// dependsOn records a blocking edge, which is what makes the fake a DAG rather
// than a bag: readiness, wave order and the out-of-scope check all read it.
func (f *fakeIssues) dependsOn(id string, on ...string) *fakeIssues {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range on {
		f.deps[id] = append(f.deps[id], bd.Ref{ID: d, Type: "blocks"})
	}
	return f
}

// deferredUntil puts an issue on ice, the way `bd defer` does. It is what makes
// the fake model bd's one asymmetry: a deferred issue reads as open through
// Show and appears in every count, and is offered by no ready front at all.
func (f *fakeIssues) deferredUntil(id string, t time.Time) *fakeIssues {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defers[id] = t
	return f
}

// Ready is bd's blocker-aware ready front: open, undeferred issues under the
// parent whose blocking dependencies are all closed, in ID order.
func (f *fakeIssues) Ready(parent string, limit int) ([]bd.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readyCalls++
	if f.readyErr != nil && f.readyCalls >= f.readyFailFrom {
		return nil, f.readyErr
	}
	if f.fail != nil {
		return nil, f.fail
	}
	var out []bd.Issue
	for id, st := range f.status {
		if st != "open" || (parent != "" && f.parent[id] != parent) {
			continue
		}
		if until, ok := f.defers[id]; ok && until.After(time.Now()) {
			continue
		}
		blocked := false
		for _, d := range f.deps[id] {
			if f.status[d.ID] != "closed" {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		out = append(out, bd.Issue{ID: id, Title: f.titles[id], Status: st})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// under makes ids children of an epic, which is what the close decision reads.
func (f *fakeIssues) under(epic string, ids ...string) *fakeIssues {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.parent[id] = epic
	}
	return f
}

func (f *fakeIssues) Show(id string) (*bd.Issue, error) {
	if f.onShow != nil {
		f.onShow(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	st, ok := f.status[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	deps := append([]bd.Ref(nil), f.deps[id]...)
	for i := range deps {
		deps[i].Status = f.status[deps[i].ID]
		deps[i].DeferUntil = f.defers[deps[i].ID]
	}
	out := &bd.Issue{
		ID: id, Title: f.titles[id], Status: st, Parent: f.parent[id],
		Dependencies: deps, DeferUntil: f.defers[id],
	}
	if f.notesShown {
		out.Notes = f.issueNotes[id]
	}
	return out, nil
}

func (f *fakeIssues) AppendNotes(id, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notesFail != nil {
		return f.notesFail
	}
	f.notes = append(f.notes, note)
	f.appendNote(id, note)
	return nil
}

// appendNote joins notes the way bd does: one blob per issue, newest last. The
// caller holds the lock.
func (f *fakeIssues) appendNote(id, note string) {
	if f.issueNotes[id] != "" {
		f.issueNotes[id] += "\n\n"
	}
	f.issueNotes[id] += note
}

// Park records the reason on the issue as well as the status, exactly as
// bd.Client.Park does: the note is the durable half, and a fake that dropped it
// would let a park with no explanation pass.
func (f *fakeIssues) Park(id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, reason)
	f.appendNote(id, reason)
	f.parked = append(f.parked, id)
	f.status[id] = "blocked"
	return nil
}

func (f *fakeIssues) Reset(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets++
	f.status[id] = "open"
	return nil
}

func (f *fakeIssues) Children(parent string) ([]bd.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	var out []bd.Issue
	for id, st := range f.status {
		if id == parent || f.parent[id] != parent {
			continue
		}
		out = append(out, bd.Issue{ID: id, Title: f.titles[id], Status: st})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeIssues) Close(id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.closeFail[id]; err != nil {
		return err
	}
	f.status[id] = "closed"
	f.closed = append(f.closed, id)
	return nil
}

// All is every issue the fake holds, which is what discovery deduplication is
// checked against.
func (f *fakeIssues) All() ([]bd.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	var out []bd.Issue
	for id, st := range f.status {
		out = append(out, bd.Issue{ID: id, Title: f.titles[id], Description: f.descs[id],
			Status: st, Parent: f.parent[id]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Create files an issue with a generated ID, and records the whole request so a
// test can assert on the deps, labels and deferral a discovery is filed with —
// which is most of what "filed correctly" means here.
func (f *fakeIssues) Create(n bd.NewIssue) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createFail != nil {
		return "", f.createFail
	}
	f.created = append(f.created, n)
	id := fmt.Sprintf("disc-%d", len(f.created))
	f.status[id] = "open"
	f.titles[id] = n.Title
	return id, nil
}

// createdIssues returns a copy of everything Create was asked for, in order.
func (f *fakeIssues) createdIssues() []bd.NewIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bd.NewIssue(nil), f.created...)
}

func (f *fakeIssues) set(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = status
}

func (f *fakeIssues) snapshot() (notes, parked []string, resets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notes...), append([]string(nil), f.parked...), f.resets
}

// testCfg is a config with no surprises: no gate, no stages beyond implement,
// and budgets the caller sets.
func testCfg(rounds, retry int) *config.Config {
	cfg := config.Default()
	cfg.Gate = nil
	cfg.Pipeline = []config.Stage{{Stage: config.StageImplement}}
	cfg.MaxRounds = rounds
	cfg.Retry = retry
	return cfg
}

func withGate(cfg *config.Config, name, cmd string) *config.Config {
	cfg.Gate = append(cfg.Gate, config.Command{Name: name, Run: cmd})
	cfg.Pipeline = append(cfg.Pipeline, config.Stage{Stage: config.StageGate})
	return cfg
}

func withReview(cfg *config.Config) *config.Config {
	cfg.Pipeline = append(cfg.Pipeline, config.Stage{Stage: "review", Agent: "reviewer"})
	return cfg
}

// engine wires an engine onto a repo with per-role fakes and no waiting.
//
// The forge is always a fake, and unconditionally rather than per test. It is
// the one part of the engine that reaches the network and somebody's GitHub
// account, so no test is allowed to leave it resolving to the real one by
// omission.
func engine(t *testing.T, repo string, cfg *config.Config, iss Issues, worker, reviewer runner.Runner) *Engine {
	t.Helper()
	return &Engine{
		RepoRoot: repo,
		Cfg:      cfg,
		BD:       iss,
		Forge:    newForge(),
		NewRunner: func(role runner.Role, _ runner.Spec) (runner.Runner, error) {
			if role == runner.RoleWorker {
				return worker, nil
			}
			return reviewer, nil
		},
		Prompt:  func(role runner.Role) (string, error) { return "system prompt for " + string(role), nil },
		Sleep:   func(context.Context, time.Duration) error { return nil },
		Backoff: func(int) time.Duration { return 0 },
	}
}

// workerGit runs git the way a WORKER runs it: with the worktree's hooks live.
//
// The engine's own git() suppresses hooks (see internal/gitx), which is right
// for the engine and wrong for anything standing in for a worker. A worker is
// the claude CLI running plain git inside a guarded worktree, so gitguard's
// prepare-commit-msg fires and stamps the attempt trailer. A scripted worker
// that skipped it would produce commits the guard then rejects as foreign, and
// the test would be measuring the stand-in rather than the engine.
func workerGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// commitWork is the historical name used throughout the harness. Workers now
// leave this work dirty and the orchestrator creates the commit after approval.
func commitWork(name string) func(context.Context, runner.Request) error {
	return func(_ context.Context, req runner.Request) error {
		return os.WriteFile(filepath.Join(req.Dir, name), []byte(name+"\n"), 0o644)
	}
}

// steps chains scripted side effects into one step body.
func steps(fns ...func(context.Context, runner.Request) error) func(context.Context, runner.Request) error {
	return func(ctx context.Context, req runner.Request) error {
		for _, fn := range fns {
			if err := fn(ctx, req); err != nil {
				return err
			}
		}
		return nil
	}
}

func closes(iss *fakeIssues, id string) func(context.Context, runner.Request) error {
	return func(context.Context, runner.Request) error {
		// Kept as a no-op so older fixtures describe the same point in their
		// script. Only the orchestrator closes the issue after integration.
		return nil
	}
}

// blocks is step 7 of prompts/worker.md for a worker with nowhere to go: the
// issue set blocked, with what stopped it said on the issue itself.
func parksItself(iss *fakeIssues, id, note string) func(context.Context, runner.Request) error {
	return func(context.Context, runner.Request) error {
		return nil
	}
}

func pass() *fake.Runner   { return fake.New(fake.Step{Text: "VERDICT: pass"}) }
func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// --- tests ---

func TestIssueRunDrivesImplementGateAndReview(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	var sessionOnDisk string

	worker := fake.New(fake.Step{
		Text: "implemented it",
		Do: steps(commitWork("a.txt"), closes(iss, "t-1"),
			func(_ context.Context, req runner.Request) error {
				// The session must already be in run state by the time the
				// process is running: recorded afterwards, an interrupt loses
				// exactly the session that would let it resume.
				st, err := runstate.Load(repo)
				if err != nil {
					return err
				}
				sessionOnDisk = st.InFlight["t-1"].WorkerSession
				return nil
			}),
	})
	reviewer := pass()

	cfg := withReview(withGate(testCfg(3, 0), "build", "true"))
	e := engine(t, repo, cfg, iss, worker, reviewer)

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
	}
	if worker.Calls() != 1 || reviewer.Calls() != 1 {
		t.Fatalf("worker %d calls, reviewer %d calls; want 1 and 1", worker.Calls(), reviewer.Calls())
	}
	if got := worker.Requests()[0].SessionID; sessionOnDisk != got || got == "" {
		t.Fatalf("run state held session %q while the worker ran under %q", sessionOnDisk, got)
	}
	if notes := ReviewNotesPath(repo, "t-1"); !exists(notes) {
		t.Fatalf("no review notes at %s", notes)
	}
	if log := worker.Requests()[0].LogPath; !strings.Contains(log, "t-1-a1-r0-worker") {
		t.Fatalf("transcript path %q does not name the issue, attempt and round", log)
	}
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsDone("t-1") {
		t.Fatal("run state does not record the issue as done")
	}
}

func TestApprovedSnapshotIsTheOnlyIssueCommit(t *testing.T) {
	repo := testRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "delete.txt"), []byte("delete me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "delete.txt")
	mustGit(t, repo, "commit", "--quiet", "-m", "add deletion fixture")
	base := mustGit(t, repo, "rev-parse", "HEAD")

	iss := newIssues("t-1")
	var claimedBeforeWork bool
	worker := fake.New(fake.Step{Do: func(_ context.Context, req runner.Request) error {
		iss.mu.Lock()
		claimedBeforeWork = iss.status["t-1"] == "in_progress"
		iss.mu.Unlock()
		if err := os.WriteFile(filepath.Join(req.Dir, "seed.txt"), []byte("changed\n"), 0o644); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(req.Dir, "delete.txt")); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(req.Dir, "new.txt"), []byte("new\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(req.Dir, "binary.dat"), []byte{0, 1, 2, 3}, 0o644); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(req.Dir, ".beads", "auto"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(req.Dir, ".beads", "issues.jsonl"), []byte("generated\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.Dir, ".beads", "auto", "handoff.json"), []byte("runtime\n"), 0o644)
	}})
	reviewer := pass()
	e := engine(t, repo, withReview(testCfg(1, 0)), iss, worker, reviewer)

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil || rep.Outcome != OutcomeDone {
		t.Fatalf("Issue = %+v, %v", rep, err)
	}
	if !claimedBeforeWork || len(iss.claimed) != 1 || iss.claimed[0] != "t-1" {
		t.Fatalf("worker started before claim: claimed=%v observed=%v", iss.claimed, claimedBeforeWork)
	}
	if current, _ := iss.Show("t-1"); current.Status != "in_progress" {
		t.Fatalf("issue status after review = %q, want in_progress until integration", current.Status)
	}

	prompt := reviewer.Requests()[0].Prompt
	for _, path := range []string{"seed.txt", "delete.txt", "new.txt", "binary.dat"} {
		if !strings.Contains(prompt, path) {
			t.Errorf("reviewer patch omitted %s", path)
		}
	}
	if !strings.Contains(prompt, "GIT binary patch") && !strings.Contains(prompt, "Binary files") {
		t.Errorf("reviewer patch omitted binary content:\n%s", prompt)
	}

	branch := e.Cfg.Branch("t-1")
	if got := mustGit(t, repo, "rev-list", "--count", base+".."+branch); got != "1" {
		t.Fatalf("issue branch has %s commits, want exactly one", got)
	}
	message := mustGit(t, repo, "show", "-s", "--format=%B", branch)
	for _, want := range []string{"t-1: test issue t-1", "Bd-Auto: t-1/1"} {
		if !strings.Contains(message, want) {
			t.Errorf("commit message omitted %q:\n%s", want, message)
		}
	}
	tree := mustGit(t, repo, "ls-tree", "-r", "--name-only", branch)
	for _, excluded := range []string{".beads/issues.jsonl", ".beads/auto/handoff.json"} {
		if strings.Contains(tree, excluded) {
			t.Errorf("orchestrator committed excluded runtime path %s:\n%s", excluded, tree)
		}
	}
}

func TestReviewerMutationIsRejectedWithoutACommit(t *testing.T) {
	repo := testRepo(t)
	base := mustGit(t, repo, "rev-parse", "HEAD")
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Do: commitWork("a.txt")})
	reviewer := fake.New(fake.Step{Text: "VERDICT: pass", Do: func(_ context.Context, req runner.Request) error {
		return os.WriteFile(filepath.Join(req.Dir, "a.txt"), []byte("reviewer changed it\n"), 0o644)
	}})
	e := engine(t, repo, withReview(testCfg(1, 0)), iss, worker, reviewer)

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != OutcomeParked || rep.Stage != "review" || !strings.Contains(rep.Reason, "reviewer changed") {
		t.Fatalf("reviewer mutation outcome = %+v", rep)
	}
	if got := mustGit(t, repo, "rev-parse", e.Cfg.Branch("t-1")); got != base {
		t.Fatalf("reviewer-mutated snapshot was committed: %s != %s", got, base)
	}
}

func TestWorkerCreatedCommitFailsBeforeReview(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Do: func(_ context.Context, req runner.Request) error {
		if err := os.WriteFile(filepath.Join(req.Dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
			return err
		}
		if _, err := workerGit(req.Dir, "add", "-A"); err != nil {
			return err
		}
		_, err := workerGit(req.Dir, "-c", "core.hooksPath=", "commit", "--quiet", "-m", "worker commit")
		return err
	}})
	reviewer := pass()
	e := engine(t, repo, withReview(testCfg(1, 0)), iss, worker, reviewer)

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != OutcomeParked || rep.Stage != StageGuard || !strings.Contains(rep.Reason, "branch-moved") {
		t.Fatalf("worker commit outcome = %+v", rep)
	}
	if reviewer.Calls() != 0 {
		t.Fatalf("reviewer ran %d times on unauthorized history", reviewer.Calls())
	}
	if !exists(worktree.Path(repo, "t-1")) {
		t.Fatal("failed attempt artifacts were removed")
	}
}

// Only a model stage says anything for itself. The gate spawns no runner at
// all, so without a boundary on the bus a watcher sees the worker's last tool
// call for the whole of it, clock climbing, and reads a stalled worker — which
// is exactly what the run: stage a repo adds would look like too.
func TestEveryStageAnnouncesItselfOnTheBus(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	worker := fake.New(fake.Step{Text: "implemented it",
		Do: steps(commitWork("a.txt"), closes(iss, "t-1"))})

	cfg := withReview(withGate(testCfg(3, 0), "build", "true"))
	cfg.Pipeline = append(cfg.Pipeline, config.Stage{Stage: "lint", Run: "true"})

	var mu sync.Mutex
	var got []Event
	e := engine(t, repo, cfg, iss, worker, pass())
	e.Bus = NewBus(ObserverFunc(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	}))

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
	}

	// Every stage after implement, opened and closed, in pipeline order. The
	// implement stage is absent on purpose: it is the worker, and the worker
	// has been streaming its own activity all along.
	var boundaries []string
	for _, ev := range got {
		switch ev.Kind {
		case EventStageStart:
			boundaries = append(boundaries, "start:"+ev.Stage)
		case EventStageEnd:
			boundaries = append(boundaries, "end:"+ev.Stage+":"+passFail(ev.Passed))
		}
	}
	want := []string{
		"start:gate", "end:gate:passed",
		"start:review", "end:review:passed",
		"start:lint", "end:lint:passed",
	}
	if strings.Join(boundaries, " ") != strings.Join(want, " ") {
		t.Fatalf("the bus carried %v, want %v", boundaries, want)
	}

	// A model stage names its role; the two that run no model must not, because
	// naming one would invent it.
	roles := map[string]runner.Role{}
	for _, ev := range got {
		if ev.Kind == EventStageStart {
			roles[ev.Stage] = ev.Role
		}
	}
	if roles["review"] != runner.RoleReviewer {
		t.Fatalf("the review stage ran as %q, want reviewer", roles["review"])
	}
	for _, stage := range []string{"gate", "lint"} {
		if roles[stage] != "" {
			t.Fatalf("the %s stage claims role %q; it spawns no model", stage, roles[stage])
		}
	}
}

// agent: on the implement stage used to be read and dropped: Stage.Kind matched
// the name first, so a repo could name its own implementer and get the worker
// anyway. The role now reaches everything the work is done under — which runner
// is built, which prompt it is given, which name its transcript is filed under —
// while the worktree, the branch and the resumable session stay with the stage
// rather than following the role.
func TestTheImplementStageRunsTheRoleItNames(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	// Read while the call is in flight: recordSession writes before the process
	// starts, and the entry is cleared again once the issue is done.
	var inFlight runstate.Attempt
	builder := fake.New(fake.Step{Text: "built it", Do: steps(
		func(context.Context, runner.Request) error {
			st, err := runstate.Load(repo)
			if err != nil {
				return err
			}
			inFlight = st.InFlight["t-1"]
			return nil
		},
		commitWork("a.txt"), closes(iss, "t-1"))})

	cfg := withGate(testCfg(3, 0), "build", "true")
	cfg.Pipeline[0].Agent = "builder"
	cfg.Runners = map[string]config.RunnerSpec{"builder": {Model: "sonnet"}}

	var spawned []runner.Role
	e := engine(t, repo, cfg, iss, builder, pass())
	e.NewRunner = func(role runner.Role, _ runner.Spec) (runner.Runner, error) {
		spawned = append(spawned, role)
		return builder, nil
	}

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
	}

	if len(spawned) != 1 || spawned[0] != "builder" {
		t.Fatalf("the engine spawned %v, want one builder", spawned)
	}
	reqs := builder.Requests()
	if len(reqs) != 1 {
		t.Fatalf("want one request, got %d", len(reqs))
	}
	if reqs[0].Role != "builder" {
		t.Fatalf("the request ran as %q, want builder", reqs[0].Role)
	}
	if want := "system prompt for builder"; reqs[0].SystemPrompt != want {
		t.Fatalf("system prompt %q, want %q", reqs[0].SystemPrompt, want)
	}
	if !strings.HasSuffix(reqs[0].LogPath, "-builder.jsonl") {
		t.Fatalf("transcript %q should be filed under the role that ran", reqs[0].LogPath)
	}

	// The lifecycle did not move with the role: the session an interrupt would
	// resume is still filed as the implement stage's, not as a review session.
	if inFlight.WorkerSession == "" || inFlight.Stage != StageImplement {
		t.Fatalf("the implement session was filed as %+v", inFlight)
	}
}

// A role with no prompt of its own falls back to the worker's on implement, not
// to the reviewer's a judging stage gets. A custom implementer does the work;
// handing it a "read this diff and return a verdict" prompt would be worse than
// handing it nothing.
func TestACustomImplementerFallsBackToTheWorkerPrompt(t *testing.T) {
	e := &Engine{Prompt: func(role runner.Role) (string, error) {
		switch role {
		case runner.RoleWorker, runner.RoleReviewer:
			return "prompt for " + string(role), nil
		}
		return "", errors.New("no prompt for " + string(role))
	}}
	if got := e.implementPrompt("builder"); got != "prompt for worker" {
		t.Fatalf("implementPrompt = %q, want the worker's", got)
	}
	if got := e.promptFor("auditor"); got != "prompt for reviewer" {
		t.Fatalf("promptFor = %q, want the reviewer's", got)
	}
	if got := e.implementPrompt(runner.RoleReviewer); got != "prompt for reviewer" {
		t.Fatalf("implementPrompt = %q; a role with its own prompt keeps it", got)
	}
}

// A failed stage carries the reason back on the bus as well as back to the
// worker: the round that follows it looks arbitrary without it.
func TestAFailedStageCarriesItsFeedbackOnTheBus(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	worker := fake.New(
		fake.Step{Text: "first pass", Do: steps(commitWork("a.txt"), closes(iss, "t-1"))},
		fake.Step{Text: "fixed it", Do: commitWork("b.txt")},
	)

	var mu sync.Mutex
	var ends []Event
	e := engine(t, repo, failingThenPassingGate(t, testCfg(3, 0)), iss, worker, pass())
	e.Bus = NewBus(ObserverFunc(func(ev Event) {
		if ev.Kind != EventStageEnd {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		ends = append(ends, ev)
	}))

	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(ends) < 1 {
		t.Fatal("the gate never reported a verdict")
	}
	first := ends[0]
	if first.Passed {
		t.Fatalf("the first gate run passed; the fixture makes it fail")
	}
	if !strings.Contains(first.Text, "build") {
		t.Fatalf("the failed gate reported %q, which does not name the command that failed", first.Text)
	}
}

// failingThenPassingGate is a gate that fails once and passes afterwards, by
// way of a marker file the first run leaves behind.
func failingThenPassingGate(t *testing.T, cfg *config.Config) *config.Config {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "gate-ran")
	return withGate(cfg, "build",
		fmt.Sprintf("test -f %s || { touch %s; exit 1; }", marker, marker))
}

// The three feedback paths the whole design rests on, asserted the same way:
// one more round, on the SAME session, in a worktree that survived.
func TestFailingChecksResumeTheSameSession(t *testing.T) {
	cases := []struct {
		name    string
		cfg     func() *config.Config
		round1  func(iss *fakeIssues) fake.Step
		round2  func(iss *fakeIssues) fake.Step
		wantIn  string // must appear in round 2's prompt
		reviews []fake.Step
	}{
		{
			name: "gate",
			cfg:  func() *config.Config { return withGate(testCfg(3, 0), "marker", "test -f done.txt") },
			round1: func(iss *fakeIssues) fake.Step {
				return fake.Step{Do: steps(commitWork("a.txt"), closes(iss, "t-1"))}
			},
			round2: func(*fakeIssues) fake.Step { return fake.Step{Do: commitWork("done.txt")} },
			wantIn: "gate failed",
		},
		{
			name: "review",
			cfg:  func() *config.Config { return withReview(testCfg(3, 0)) },
			round1: func(iss *fakeIssues) fake.Step {
				return fake.Step{Do: steps(commitWork("a.txt"), closes(iss, "t-1"))}
			},
			round2:  func(*fakeIssues) fake.Step { return fake.Step{Do: commitWork("b.txt")} },
			wantIn:  "os.Open at cmd/run.go:42 is discarded",
			reviews: []fake.Step{{Text: "VERDICT: fail\n- the error from os.Open at cmd/run.go:42 is discarded"}, {Text: "VERDICT: pass"}},
		},
		{
			name: "invalid status",
			cfg:  func() *config.Config { return testCfg(3, 0) },
			round1: func(*fakeIssues) fake.Step {
				return fake.Step{Text: "WORKER_STATUS: almost", Do: commitWork("a.txt")}
			},
			round2: func(*fakeIssues) fake.Step {
				return fake.Step{Do: commitWork("b.txt")}
			},
			wantIn: "not ready or blocked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testRepo(t)
			iss := newIssues("t-1")

			var survived bool
			r2 := tc.round2(iss)
			inner := r2.Do
			r2.Do = func(ctx context.Context, req runner.Request) error {
				// The worktree must still hold round one's work. Wiping it
				// between rounds is what would make resuming pointless.
				survived = exists(filepath.Join(req.Dir, "a.txt"))
				if inner == nil {
					return nil
				}
				return inner(ctx, req)
			}

			worker := fake.New(tc.round1(iss), r2)
			reviewer := pass()
			if tc.reviews != nil {
				reviewer = fake.New(tc.reviews...)
			}
			e := engine(t, repo, tc.cfg(), iss, worker, reviewer)

			rep, err := e.Issue(context.Background(), "t-1")
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			if rep.Outcome != OutcomeDone {
				t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
			}
			if worker.Calls() != 2 {
				t.Fatalf("worker ran %d times, want 2", worker.Calls())
			}
			reqs := worker.Requests()
			if !reqs[1].Resume {
				t.Fatal("round 2 did not resume")
			}
			if reqs[0].SessionID != reqs[1].SessionID {
				t.Fatalf("round 2 resumed session %q, not round 1's %q", reqs[1].SessionID, reqs[0].SessionID)
			}
			if !strings.Contains(reqs[1].Prompt, tc.wantIn) {
				t.Fatalf("round 2 prompt does not carry the feedback (%q):\n%s", tc.wantIn, reqs[1].Prompt)
			}
			if !survived {
				t.Fatal("the worktree did not survive between rounds")
			}
			if len(rep.Attempts) != 1 || rep.Attempts[0].Rounds != 2 {
				t.Fatalf("attempts %+v, want one attempt of two rounds", rep.Attempts)
			}
		})
	}
}

// A reviewer judges fresh by default: a resumed one carries its own previous
// VERDICT: fail and checks whether its findings were addressed instead of
// re-judging the diff.
func TestReviewerRunsFreshEveryRound(t *testing.T) {
	repo := testRepo(t)
	base := mustGit(t, repo, "rev-parse", "HEAD")
	iss := newIssues("t-1")
	worker := fake.New(
		fake.Step{Do: steps(commitWork("a.txt"), closes(iss, "t-1"))},
		fake.Step{Do: commitWork("b.txt")},
	)
	reviewer := fake.New(
		fake.Step{Text: "VERDICT: fail\n- fix the thing"},
		fake.Step{Text: "VERDICT: pass"},
	)
	e := engine(t, repo, withReview(testCfg(3, 0)), iss, worker, reviewer)

	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	reqs := reviewer.Requests()
	if len(reqs) != 2 {
		t.Fatalf("reviewer ran %d times, want 2", len(reqs))
	}
	for i, r := range reqs {
		if r.Resume {
			t.Fatalf("review %d resumed; reviewers default to a fresh session", i+1)
		}
	}
	if reqs[0].SessionID == reqs[1].SessionID {
		t.Fatal("both reviews used one session id; a fresh session needs a fresh id")
	}
	if !strings.Contains(reqs[0].Prompt, "a.txt") || strings.Contains(reqs[0].Prompt, "b.txt") {
		t.Fatalf("first review did not receive only the first dirty snapshot:\n%s", reqs[0].Prompt)
	}
	if !strings.Contains(reqs[1].Prompt, "a.txt") || !strings.Contains(reqs[1].Prompt, "b.txt") {
		t.Fatalf("second review did not receive the same worktree with the fix:\n%s", reqs[1].Prompt)
	}
	if got := mustGit(t, repo, "rev-list", "--count", base+".."+e.Cfg.Branch("t-1")); got != "1" {
		t.Fatalf("review feedback produced %s commits, want one commit after approval", got)
	}
}

// A round that returns ok having done nothing fails the attempt outright. Every
// check after the progress check is satisfiable by the previous round's state,
// so letting it through would spend the whole budget re-judging one diff.
func TestNoProgressFailsTheAttemptOutright(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Text: "I looked at it"}) // no commit, no file
	e := engine(t, repo, withReview(testCfg(3, 0)), iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeParked {
		t.Fatalf("outcome %s, want parked", rep.Outcome)
	}
	if worker.Calls() != 1 {
		t.Fatalf("worker ran %d times; a no-op round must not buy the remaining rounds", worker.Calls())
	}
	if rep.Attempts[0].Stage != StageImplement || !strings.Contains(rep.Reason, "without changing anything") {
		t.Fatalf("failed at %q: %s", rep.Attempts[0].Stage, rep.Reason)
	}
	if _, parked, _ := iss.snapshot(); len(parked) != 1 {
		t.Fatalf("parked %v, want the issue parked in bd", parked)
	}
}

// The same empty worktree, but the process itself failed before it could reach
// a model. "Returned without changing anything" is then the symptom standing in
// for the cause, and on 2026-08-18 that sentence was what five rate-limited
// workers were parked under while the message that explained them — a session
// limit — reached bd nowhere at all.
func TestAnEmptyWorktreeAfterAFailedProcessNamesTheFailure(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{
		Class: runner.ClassWorkFailed,
		Err:   errors.New("claude: the API call failed (HTTP 429): You've hit your session limit"),
	})
	e := engine(t, repo, withReview(testCfg(3, 0)), iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeParked {
		t.Fatalf("outcome %s, want parked", rep.Outcome)
	}
	if strings.Contains(rep.Reason, "without changing anything") {
		t.Errorf("the reason describes the empty worktree and not what emptied it: %s", rep.Reason)
	}
	for _, want := range []string{"session limit", "429"} {
		if !strings.Contains(rep.Reason, want) {
			t.Errorf("reason should quote what the process said (%q), got: %s", want, rep.Reason)
		}
	}
}

// The counterpart, and the reason the check above is on the error rather than
// on the class: a model that really did run and change nothing must still be
// told so, because that is a finding about the work.
func TestAnEmptyWorktreeWithNoProcessFailureStillReadsAsNoProgress(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Text: "I looked at it"})
	e := engine(t, repo, withReview(testCfg(3, 0)), iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !strings.Contains(rep.Reason, "without changing anything") {
		t.Errorf("reason = %s, want the no-progress finding", rep.Reason)
	}
}

// A worker that sets its own issue to blocked has said it cannot do the work.
// That is a verdict, so the issue is parked with what the worker said — not run
// on through the guard, the gate and the review and recorded as done, which is
// what reading blocked as terminal used to do.
//
// The three cases are the three moments it can happen: straight away, after the
// gate has already passed on a round the worker then gave up on, and after the
// issue had been closed once already.
func TestABlockedWorkerParksTheIssueRatherThanFinishingIt(t *testing.T) {
	const (
		note = "the schema this needs belongs to t-9, which has not landed"
		said = "I could not finish: nothing here compiles until t-9 lands."
	)
	cases := []struct {
		name string
		cfg  func() *config.Config
		// notes says whether bd hands the note back on the next read. Where it
		// does not, the worker's final message is the only account left.
		notes    bool
		steps    func(iss *fakeIssues) []fake.Step
		reviews  []fake.Step
		rounds   int
		wantSaid string
	}{
		{
			name:  "round one",
			cfg:   func() *config.Config { return withReview(testCfg(3, 1)) },
			notes: true,
			steps: func(iss *fakeIssues) []fake.Step {
				return []fake.Step{{Text: said + "\nWORKER_STATUS: blocked\nWORKER_REASON: " + note, Do: steps(commitWork("a.txt"), parksItself(iss, "t-1", note))}}
			},
			rounds:   1,
			wantSaid: note,
		},
		{
			name: "after a passing gate",
			cfg: func() *config.Config {
				return withReview(withGate(testCfg(3, 1), "marker", "test -f done.txt"))
			},
			notes: false, // bd lost the note, as beads' import hook does
			steps: func(iss *fakeIssues) []fake.Step {
				return []fake.Step{
					{Text: "gate is green", Do: steps(commitWork("done.txt"), closes(iss, "t-1"))},
					{Text: said + "\nWORKER_STATUS: blocked\nWORKER_REASON: " + note, Do: steps(commitWork("b.txt"), parksItself(iss, "t-1", note))},
				}
			},
			reviews:  []fake.Step{{Text: "VERDICT: fail\n- the error from os.Open is discarded"}},
			rounds:   2,
			wantSaid: note,
		},
		{
			// The ordinary shape of a worker that cannot do the work: it read
			// the issue, found nowhere to go, and has nothing to commit. The
			// progress check would fail this attempt and buy the retry that a
			// self-park exists to avoid.
			name:  "nothing committed",
			cfg:   func() *config.Config { return withReview(testCfg(3, 1)) },
			notes: true,
			steps: func(iss *fakeIssues) []fake.Step {
				return []fake.Step{{Text: said + "\nWORKER_STATUS: blocked\nWORKER_REASON: " + note, Do: parksItself(iss, "t-1", note)}}
			},
			rounds:   1,
			wantSaid: note,
		},
		{
			name:  "closed, then blocked",
			cfg:   func() *config.Config { return withReview(testCfg(3, 1)) },
			notes: true,
			steps: func(iss *fakeIssues) []fake.Step {
				return []fake.Step{
					{Text: "closed it", Do: steps(commitWork("a.txt"), closes(iss, "t-1"))},
					{Text: said + "\nWORKER_STATUS: blocked\nWORKER_REASON: " + note, Do: steps(commitWork("b.txt"), parksItself(iss, "t-1", note))},
				}
			},
			reviews:  []fake.Step{{Text: "VERDICT: fail\n- the error from os.Open is discarded"}},
			rounds:   2,
			wantSaid: note,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testRepo(t)
			iss := newIssues("t-1")
			if tc.notes {
				iss.showsNotes()
			}
			reviewer := pass()
			if tc.reviews != nil {
				reviewer = fake.New(tc.reviews...)
			}
			worker := fake.New(tc.steps(iss)...)
			e := engine(t, repo, tc.cfg(), iss, worker, reviewer)

			rep, err := e.Issue(context.Background(), "t-1")
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}

			if rep.Outcome != OutcomeParked {
				t.Fatalf("outcome %s, want parked: a blocked worker did not finish the work", rep.Outcome)
			}
			if rep.Stage != StageImplement {
				t.Fatalf("stage %q, want %q", rep.Stage, StageImplement)
			}
			if !strings.Contains(rep.Reason, tc.wantSaid) {
				t.Fatalf("reason does not carry what the worker said (%q):\n%s", tc.wantSaid, rep.Reason)
			}

			// One attempt, of exactly the rounds the script wrote. retry is 1,
			// so a second attempt was available and must not have been spent:
			// a fresh worker reads the same issue and says the same thing.
			if len(rep.Attempts) != 1 {
				t.Fatalf("%d attempt(s), want 1: a self-park must not buy another", len(rep.Attempts))
			}
			if rep.Attempts[0].Outcome != OutcomeBlocked {
				t.Fatalf("attempt outcome %s, want blocked", rep.Attempts[0].Outcome)
			}
			if worker.Calls() != tc.rounds {
				t.Fatalf("worker ran %d times, want %d", worker.Calls(), tc.rounds)
			}

			// Nothing after the implement check runs on a parked worker's
			// branch, so the last round is never reviewed.
			if got, want := reviewer.Calls(), len(tc.reviews); got != want {
				t.Fatalf("the reviewer ran %d times, want %d", got, want)
			}

			notes, parked, resets := iss.snapshot()
			if !has(parked, "t-1") {
				t.Fatalf("parked %v in bd, want t-1: the human label is how bd human list finds it", parked)
			}
			if !strings.Contains(strings.Join(notes, "\n"), tc.wantSaid) {
				t.Fatalf("park note does not preserve the required worker reason %q: %v", tc.wantSaid, notes)
			}
			if resets != 0 {
				t.Fatalf("the issue was returned to the ready queue %d time(s); nothing is being retried", resets)
			}

			st, err := runstate.Load(repo)
			if err != nil {
				t.Fatal(err)
			}
			if !st.IsParked("t-1") || st.IsDone("t-1") {
				t.Fatalf("run state has t-1 done=%v parked=%v; the barrier reads this to decide what merges",
					st.IsDone("t-1"), st.IsParked("t-1"))
			}
		})
	}
}

// The same empty round, but the worker was refused the tools it needed. That is
// not the model failing the work, it is the model not being allowed to do it,
// and no fresh attempt under the same permission level ends differently — so the
// run stops on the environment instead of parking a perfectly good issue.
func TestARefusedWorkerIsAnEnvironmentFailure(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	// Exactly what claude reports for this: a successful run that changed
	// nothing, with the refusal visible only in the denial list.
	worker := fake.New(fake.Step{
		Text:    "I've requested permission to write. Please approve it when ready.",
		Denials: []string{"Write", "Bash"},
	})
	e := engine(t, repo, withReview(testCfg(3, 1)), iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeInfra {
		t.Fatalf("outcome %s, want infra-failed", rep.Outcome)
	}
	if worker.Calls() != 1 {
		t.Fatalf("worker ran %d times; retrying a refusal is refused identically", worker.Calls())
	}
	if len(rep.Attempts) != 1 {
		t.Fatalf("%d attempts, want 1: a refusal must not burn the retry", len(rep.Attempts))
	}
	// The reason has to be the fix, since nothing the model does can be one.
	for _, want := range []string{"Write", "Bash", "bypass", "--dangerously-skip-permissions"} {
		if !strings.Contains(rep.Reason, want) {
			t.Fatalf("reason should name %q, got: %s", want, rep.Reason)
		}
	}
	if _, parked, _ := iss.snapshot(); len(parked) != 0 {
		t.Fatalf("parked %v; an issue nobody was allowed to work on is not a failed issue", parked)
	}
}

// The fix depends on the level that was refused, and the one level bd-auto
// cannot fix is the one it has already widened as far as it goes. A run on
// bypass that is still refused was refused by something else, so pointing at
// .beads-auto.yaml would point at the one file that cannot be the cause.
func TestARefusalOnBypassBlamesSomethingOtherThanTheConfig(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	cfg := withReview(testCfg(3, 0))
	cfg.ForcePermissions = runner.PermBypass
	worker := fake.New(fake.Step{Denials: []string{"Write"}})
	e := engine(t, repo, cfg, iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeInfra {
		t.Fatalf("outcome %s, want infra-failed", rep.Outcome)
	}
	if strings.Contains(rep.Reason, "--dangerously-skip-permissions") {
		t.Fatalf("advising the flag to a run already using it:\n%s", rep.Reason)
	}
	for _, want := range []string{"Write", "PreToolUse"} {
		if !strings.Contains(rep.Reason, want) {
			t.Fatalf("reason should name %q, got: %s", want, rep.Reason)
		}
	}
}

// A reviewer is refused things by design, so a refusal must not fail the stage
// — but a verdict reached without the evidence the reviewer went looking for is
// not the same verdict, and the difference is invisible unless somebody writes
// it down. So it goes beside the verdict it produced, in the notes that outlive
// the round and in the run's log.
func TestARefusedReviewerStillVerdictsAndSaysWhatItWasRefused(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Do: steps(commitWork("a.txt"), closes(iss, "t-1"))})
	// What the reviewer's deny rules produce: a reviewer that reached for the
	// record of the issue it was judging and was turned down by the harness.
	reviewer := fake.New(fake.Step{Text: "VERDICT: pass", Denials: []string{"Bash(bd close:*)"}})

	var logged []string
	e := engine(t, repo, withReview(testCfg(3, 0)), iss, worker, reviewer)
	e.Log = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done: a scoped denial is not a failure",
			rep.Outcome, rep.Stage, rep.Reason)
	}

	notes, err := os.ReadFile(ReviewNotesPath(repo, "t-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Bash(bd close:*)", "VERDICT: pass"} {
		if !strings.Contains(string(notes), want) {
			t.Fatalf("review notes do not mention %q:\n%s", want, notes)
		}
	}
	if !strings.Contains(strings.Join(logged, "\n"), "Bash(bd close:*)") {
		t.Fatalf("the run log never says the review was refused anything:\n%s",
			strings.Join(logged, "\n"))
	}
}

// A denial the worker routed around is not a failure at all. The signal is the
// pair — refused and changed nothing — so a run that got its work done despite
// being refused something must finish normally.
func TestADenialWithProgressIsNotAFailure(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{
		Denials: []string{"WebFetch"},
		Do:      steps(commitWork("a.txt"), closes(iss, "t-1")),
	})
	e := engine(t, repo, withReview(testCfg(3, 0)), iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
	}
}

// An infra failure is not a verdict: the same round runs again, and neither the
// round nor the attempt counter moves.
func TestInfraFailureRerunsTheSameRound(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(
		fake.Step{Class: runner.ClassInfraFailed},
		fake.Step{Do: steps(commitWork("a.txt"), closes(iss, "t-1"))},
	)
	e := engine(t, repo, withReview(testCfg(3, 0)), iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
	}
	reqs := worker.Requests()
	if len(reqs) != 2 {
		t.Fatalf("worker ran %d times, want 2", len(reqs))
	}
	if reqs[0].LogPath != reqs[1].LogPath {
		t.Fatalf("the retry ran as a different round: %q then %q", reqs[0].LogPath, reqs[1].LogPath)
	}
	if len(rep.Attempts) != 1 {
		t.Fatalf("%d attempts, want 1: an infra failure must not consume one", len(rep.Attempts))
	}
	if rep.Attempts[0].Rounds != 1 {
		t.Fatalf("%d rounds, want 1: an infra failure must not consume one", rep.Attempts[0].Rounds)
	}
	// It cost a process even so, and that is reported separately: a cost
	// comparison between resume and fresh is meaningless if the two are mixed.
	if rep.Attempts[0].InfraRetries != 1 {
		t.Fatalf("%d infra retries recorded, want 1", rep.Attempts[0].InfraRetries)
	}
}

// Infra failures that keep coming stop the run rather than converting an outage
// into a pile of parked issues.
func TestRepeatedInfraFailureStopsWithoutParking(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Class: runner.ClassInfraFailed})
	e := engine(t, repo, testCfg(3, 1), iss, worker, pass())
	e.InfraRetries = 3

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeInfra {
		t.Fatalf("outcome %s, want infra-failed", rep.Outcome)
	}
	if worker.Calls() != 3 {
		t.Fatalf("worker ran %d times, want InfraRetries=3", worker.Calls())
	}
	notes, parked, _ := iss.snapshot()
	if len(parked) != 0 || len(notes) != 0 {
		t.Fatalf("an outage parked the issue (%v) or blamed the worker (%v)", parked, notes)
	}
	if len(rep.Attempts) != 1 {
		t.Fatalf("%d attempts, want 1: an outage must not spend the retry budget", len(rep.Attempts))
	}
}

// An interrupt is not a verdict either. The worktree stays, so the next run can
// pick the session back up.
func TestInterruptLeavesTheAttemptUntouched(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Class: runner.ClassInterrupted})
	e := engine(t, repo, testCfg(3, 1), iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeInterrupted {
		t.Fatalf("outcome %s, want interrupted", rep.Outcome)
	}
	if len(rep.Attempts) != 1 || worker.Calls() != 1 {
		t.Fatalf("%d attempts over %d calls, want one of each", len(rep.Attempts), worker.Calls())
	}
	if _, parked, _ := iss.snapshot(); len(parked) != 0 {
		t.Fatalf("an interrupt parked the issue: %v", parked)
	}
	if !exists(worktree.Path(repo, "t-1")) {
		t.Fatal("the worktree was removed, so the session cannot be resumed")
	}
}

// A backend that cannot resume reaches the same outcomes through fresh
// processes carrying the feedback in their prompts. Correct everywhere, just
// more expensive.
func TestFreshFallbackWhereResumeIsUnavailable(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	var survived bool

	worker := fake.New(
		fake.Step{Do: steps(commitWork("a.txt"), closes(iss, "t-1"))},
		fake.Step{Do: steps(func(_ context.Context, req runner.Request) error {
			survived = exists(filepath.Join(req.Dir, "a.txt"))
			return nil
		}, commitWork("done.txt"))},
	)
	worker.SetCaps(runner.Capabilities{Stream: true, ReportsUsage: true, Permissions: runner.AllPermissions()})

	cfg := withGate(testCfg(3, 0), "marker", "test -f done.txt")
	e := engine(t, repo, cfg, iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
	}
	reqs := worker.Requests()
	if len(reqs) != 2 {
		t.Fatalf("worker ran %d times, want 2", len(reqs))
	}
	if reqs[1].Resume {
		t.Fatal("round 2 resumed against a backend that cannot")
	}
	if reqs[0].SessionID == reqs[1].SessionID {
		t.Fatal("the fresh process reused the first session id")
	}
	// A fresh process has no memory, so it needs the whole task AND the
	// feedback, not just the feedback.
	if !strings.Contains(reqs[1].Prompt, "gate failed") || !strings.Contains(reqs[1].Prompt, "Issue data:") {
		t.Fatalf("the fresh round 2 prompt is missing the task or the feedback:\n%s", reqs[1].Prompt)
	}
	if !survived {
		t.Fatal("the worktree did not survive between rounds")
	}
}

// discardAttempt fires between attempts and nowhere else: wiping the worktree
// between rounds is what makes a resumed session pointless.
func TestDiscardHappensBetweenAttemptsOnly(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	var round2SawWork, attempt2SawWork bool

	worker := fake.New(
		// Attempt 1, round 1: work that the reviewer rejects.
		fake.Step{Do: commitWork("marker.txt")},
		// Attempt 1, round 2: nothing at all, which fails the attempt.
		fake.Step{Do: func(_ context.Context, req runner.Request) error {
			round2SawWork = exists(filepath.Join(req.Dir, "marker.txt"))
			return nil
		}},
		// Attempt 2, round 1: a clean worktree, and a finished issue.
		fake.Step{Do: steps(func(_ context.Context, req runner.Request) error {
			attempt2SawWork = exists(filepath.Join(req.Dir, "marker.txt"))
			return nil
		}, commitWork("b.txt"), closes(iss, "t-1"))},
	)
	e := engine(t, repo, withReview(testCfg(2, 1)), iss, worker,
		fake.New(fake.Step{Text: "VERDICT: fail\n- add coverage"}, fake.Step{Text: "VERDICT: pass"}))

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
	}
	if !round2SawWork {
		t.Fatal("round 2 found a wiped worktree; discard must not fire between rounds")
	}
	if attempt2SawWork {
		t.Fatal("attempt 2 inherited attempt 1's worktree; discard must fire between attempts")
	}
	if len(rep.Attempts) != 2 {
		t.Fatalf("%d attempts, want 2", len(rep.Attempts))
	}
	if rep.Attempts[0].Outcome != OutcomeFailed || rep.Attempts[1].Outcome != OutcomeDone {
		t.Fatalf("attempt outcomes %s then %s", rep.Attempts[0].Outcome, rep.Attempts[1].Outcome)
	}
	notes, _, resets := iss.snapshot()
	if len(notes) != 1 || !strings.Contains(notes[0], "bd-auto attempt 1/2") {
		t.Fatalf("the failed attempt was not recorded on the issue: %v", notes)
	}
	if resets != 0 {
		t.Fatalf("%d resets, want the orchestrator to keep the issue in progress", resets)
	}
}

// A fresh attempt is told why the one before it failed, and it is told by
// bd-auto rather than by bd.
//
// This is the regression test for beads-auto-imp-so5. bd-auto writes the
// failure to the issue's notes, but that write does not survive: beads' own
// post-checkout hook imports .beads/issues.jsonl over its database when the
// next attempt's worktree is created, reverting everything written since the
// worker's last commit. The fake reproduces that exactly — AppendNotes fails
// outright and Show never returns notes — so a retry that reads its history
// back off the issue gets nothing, and this test fails.
func TestFreshRetryIsToldWhyTheLastAttemptFailed(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	iss.notesFail = errors.New("bd lost the note")

	worker := fake.New(
		// Attempt 1: closes the issue, but never satisfies the gate.
		fake.Step{Do: steps(commitWork("a.txt"), closes(iss, "t-1"))},
		// Attempt 2: a clean worktree, and this time the gate's marker.
		fake.Step{Do: steps(commitWork("done.txt"), closes(iss, "t-1"))},
	)
	cfg := withGate(testCfg(1, 1), "marker", "test -f done.txt")
	e := engine(t, repo, cfg, iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
	}
	reqs := worker.Requests()
	if len(reqs) != 2 {
		t.Fatalf("worker ran %d times, want one round per attempt", len(reqs))
	}

	p := reqs[1].Prompt
	for _, want := range []string{
		"This is a retry",              // it knows which kind of turn this is
		"did not clear the gate stage", // and what actually went wrong
		"starting again from the base commit",
		"Issue data:", // still the whole task: the worktree is new
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("attempt 2's prompt is missing %q; a fresh retry started blind:\n%s", want, p)
		}
	}
	if len(rep.Attempts) != 2 || rep.Attempts[1].Blind {
		t.Fatalf("attempt 2 reported blind=%v over %d attempts, want an informed second attempt",
			rep.Attempts[1].Blind, len(rep.Attempts))
	}
	// Nothing reached bd, and the retry was informed anyway. That is the point.
	if notes, _, _ := iss.snapshot(); len(notes) != 0 {
		t.Fatalf("the fake accepted notes it was meant to lose: %v", notes)
	}
}

// The carried failure outlives the process, not just the loop.
//
// A run killed between two attempts starts the next one from run state with
// nothing in memory, which is the case an in-process hand-off would miss.
func TestARestartedRunStillCarriesTheFailure(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	// What the killed process left behind: attempt 1 spent and recorded.
	if _, err := runstate.Update(repo, true, func(s *runstate.State) error {
		s.InFlight["t-1"] = runstate.Attempt{Branch: "t-1", Attempt: 2}
		s.Attempts["t-1"] = 1
		s.RecordFailure("t-1", runstate.Failure{
			Attempt: 1, Of: 2, Stage: "review",
			Reason: "the reviewer rejected the error handling in cmd/run.go",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	worker := fake.New(fake.Step{Do: steps(commitWork("a.txt"), closes(iss, "t-1"))})
	e := engine(t, repo, testCfg(1, 1), iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s (%s: %s), want done", rep.Outcome, rep.Stage, rep.Reason)
	}
	if rep.Attempts[0].Attempt != 2 {
		t.Fatalf("resumed at attempt %d, want 2", rep.Attempts[0].Attempt)
	}
	if p := worker.Requests()[0].Prompt; !strings.Contains(p, "cmd/run.go") {
		t.Fatalf("the restarted run did not carry attempt 1's failure:\n%s", p)
	}
}

// A retry with nothing to carry is a fact about the run, so it is recorded
// rather than logged.
//
// Log is left nil here on purpose: that is what --quiet gives the engine, and a
// warning written only there is a warning nobody reads. The report goes to
// stdout and run state goes to disk, so both still say what happened.
func TestABlindRetryIsRecordedNotSilent(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	// Attempt 1 was spent, and nothing about it was kept.
	if _, err := runstate.Update(repo, true, func(s *runstate.State) error {
		s.InFlight["t-1"] = runstate.Attempt{Branch: "t-1", Attempt: 2}
		s.Attempts["t-1"] = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	worker := fake.New(fake.Step{Text: "I looked at it"}) // no progress: the attempt fails
	e := engine(t, repo, testCfg(1, 1), iss, worker, pass())
	e.Log = nil

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(rep.Attempts) != 1 || !rep.Attempts[0].Blind {
		t.Fatalf("attempts %+v; a retry with nothing carried must say so in the report", rep.Attempts)
	}
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(st.Notes, "\n"), "started blind") {
		t.Fatalf("run state does not record the blind retry: %v", st.Notes)
	}
}

// A first attempt is told nothing, and is not reported blind for it.
func TestAFirstAttemptCarriesNothingAndIsNotBlind(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Do: steps(commitWork("a.txt"), closes(iss, "t-1"))})
	e := engine(t, repo, testCfg(1, 0), iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Attempts[0].Blind {
		t.Fatal("a first attempt has no previous attempt to be blind to")
	}
	if p := worker.Requests()[0].Prompt; strings.Contains(p, "This is a retry") {
		t.Fatalf("a first attempt was told it is a retry:\n%s", p)
	}
}

// Rounds that keep failing the same stage end the attempt with that stage
// named, rather than reporting the last thing that happened to be checked.
func TestRoundsExhaustedParksWithTheFailingStage(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Do: steps(commitWorkNumbered(), closes(iss, "t-1"))})
	cfg := withGate(testCfg(2, 0), "marker", "test -f never.txt")
	e := engine(t, repo, cfg, iss, worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeParked {
		t.Fatalf("outcome %s, want parked", rep.Outcome)
	}
	if worker.Calls() != 2 {
		t.Fatalf("worker ran %d times, want the 2 configured rounds", worker.Calls())
	}
	if rep.Stage != config.StageGate {
		t.Fatalf("parked at stage %q, want gate", rep.Stage)
	}
	if !strings.Contains(rep.Reason, "did not clear the gate stage") {
		t.Fatalf("reason does not name the stage: %s", rep.Reason)
	}
}

// commitWorkNumbered makes a different commit every call, so a loop under test
// keeps making progress without the script having to enumerate its rounds.
func commitWorkNumbered() func(context.Context, runner.Request) error {
	var n int
	return func(ctx context.Context, req runner.Request) error {
		n++
		return commitWork("work"+string(rune('0'+n))+".txt")(ctx, req)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		pass    bool
		found   bool
		bodyHas string
	}{
		{name: "pass", text: "VERDICT: pass", pass: true, found: true},
		{name: "pass with trailing noise", text: "VERDICT: pass\n\nlooks good", pass: true, found: true, bodyHas: "looks good"},
		{name: "fail with findings", text: "VERDICT: fail\n- x.go:1 — do y", found: true, bodyHas: "do y"},
		{name: "case insensitive", text: "verdict:  PASS", pass: true, found: true},
		{name: "preamble then verdict", text: "here you go\nVERDICT: fail\n- a", found: true, bodyHas: "- a"},
		{name: "no verdict fails closed", text: "the change looks fine to me", bodyHas: "looks fine"},
		{name: "empty fails closed", text: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ParseVerdict(tc.text)
			if v.Pass != tc.pass || v.Found != tc.found {
				t.Fatalf("pass=%v found=%v, want pass=%v found=%v", v.Pass, v.Found, tc.pass, tc.found)
			}
			if tc.bodyHas != "" && !strings.Contains(v.Body, tc.bodyHas) {
				t.Fatalf("body %q does not contain %q", v.Body, tc.bodyHas)
			}
		})
	}
}

func TestSessionIDIsAUniqueUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newSessionID()
		if len(id) != 36 || strings.Count(id, "-") != 4 {
			t.Fatalf("%q is not a uuid", id)
		}
		if seen[id] {
			t.Fatalf("%q generated twice", id)
		}
		seen[id] = true
	}
}

// A worker cannot be blocked by an issue running beside it: a wave is bd's own
// ready front narrowed to the run's scope, so no member of one holds a blocking
// edge over another. When a park reason names a sibling anyway, the run says so
// as what it is — an edge the graph is missing — and prints the command a human
// would run. It never runs that command itself.
func TestAParkNamingAWaveSiblingIsReportedAsAMissingEdge(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2", "t-3")

	st := runstate.New("epic-1", 2, "auto", 0)
	st.WaveIssues = []string{"t-1", "t-2"}
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}

	const note = "t-2 owns the schema this needs, and t-3 is where the loader lives"
	worker := fake.New(fake.Step{Text: "I stopped\nWORKER_STATUS: blocked\nWORKER_REASON: " + note, Do: parksItself(iss, "t-1", note)})
	e := engine(t, repo, withReview(testCfg(3, 1)), iss.showsNotes(), worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeParked {
		t.Fatalf("outcome %s, want parked", rep.Outcome)
	}

	// t-2 ran beside it and so cannot have blocked it. t-3 was named in the
	// same sentence and is not in the wave, so bd's ready front already
	// accounts for it and there is nothing to report.
	if len(rep.MissingDeps) != 1 {
		t.Fatalf("missing deps %+v, want exactly t-2: only wave members are reportable", rep.MissingDeps)
	}
	got := rep.MissingDeps[0]
	if got.Issue != "t-1" || got.Sibling != "t-2" {
		t.Fatalf("missing dep %+v, want t-1 naming t-2", got)
	}
	if got.Command != "bd dep add t-1 t-2" {
		t.Fatalf("command %q; a human has to be able to paste it", got.Command)
	}

	after, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	var noted string
	for _, n := range after.Notes {
		if strings.Contains(n, "bd dep add t-1 t-2") {
			noted = n
		}
	}
	if noted == "" {
		t.Fatalf("the run's notes do not carry the missing edge:\n%s", strings.Join(after.Notes, "\n"))
	}
	if !strings.Contains(noted, "has not added it") {
		t.Fatalf("the note does not say the edge was left alone: %s", noted)
	}

	// The whole reason this is a report and not a repair: a graph edited on the
	// strength of one model's sentence is believed by every run after it.
	iss.mu.Lock()
	defer iss.mu.Unlock()
	if len(iss.deps["t-1"]) != 0 {
		t.Fatalf("t-1 depends on %v; bd-auto must never add an edge itself", iss.deps["t-1"])
	}
}

// A park that named nobody in its wave is an ordinary park, and reporting a
// missing edge for it would send a human to add a dependency nobody asked for.
func TestAnOrdinaryParkReportsNoMissingEdge(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2")

	st := runstate.New("epic-1", 2, "auto", 0)
	st.WaveIssues = []string{"t-1", "t-2"}
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}

	worker := fake.New(fake.Step{
		Text: "I stopped\nWORKER_STATUS: blocked\nWORKER_REASON: the acceptance criteria contradict the design",
		Do:   parksItself(iss, "t-1", "the acceptance criteria contradict the design"),
	})
	e := engine(t, repo, withReview(testCfg(3, 1)), iss.showsNotes(), worker, pass())

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeParked {
		t.Fatalf("outcome %s, want parked", rep.Outcome)
	}
	if len(rep.MissingDeps) != 0 {
		t.Fatalf("missing deps %+v, want none", rep.MissingDeps)
	}
	after, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range after.Notes {
		if strings.Contains(n, "bd dep add") {
			t.Fatalf("the run's notes invented a missing edge: %s", n)
		}
	}
}

// The search for a sibling in a park reason runs over prose a model wrote, and
// the two ways it can be wrong are both expensive: a miss loses the one signal
// that says an edge is missing, and a false hit sends a human to add an edge
// between issues nobody mentioned.
func TestMissingDepsFindsWaveSiblingsNamedInAParkReason(t *testing.T) {
	waveIDs := []string{"t-1", "x-j5a", "x-j5a.4", "t-10"}
	cases := []struct {
		name   string
		reason string
		want   []string
	}{
		{
			name:   "named plainly",
			reason: "nothing here compiles until x-j5a.4 lands",
			want:   []string{"x-j5a.4"},
		},
		{
			// The one a bare strings.Contains gets wrong: every mention of a
			// child is also a mention of its parent's ID.
			name:   "a child is not its parent",
			reason: "waiting on x-j5a.4",
			want:   []string{"x-j5a.4"},
		},
		{
			name:   "the parent named on its own is the parent",
			reason: "the epic x-j5a has not been split yet",
			want:   []string{"x-j5a"},
		},
		{
			name:   "a longer ID is not a shorter one",
			reason: "blocked by t-10",
			want:   []string{"t-10"},
		},
		{
			name:   "punctuation ends an ID",
			reason: "I need t-10, x-j5a.4 and nothing else.",
			want:   []string{"x-j5a.4", "t-10"},
		},
		{
			name:   "the issue's own ID is not a sibling",
			reason: "the worker set t-1 to blocked rather than closing it",
			want:   nil,
		},
		{
			name:   "an issue outside the wave is bd's business, not ours",
			reason: "the loader belongs to t-77",
			want:   nil,
		},
		{
			name:   "case is not the point",
			reason: "X-J5A.4 has to land first",
			want:   []string{"x-j5a.4"},
		},
		{
			name:   "no reason at all",
			reason: "   ",
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingDeps("t-1", tc.reason, waveIDs)
			var ids []string
			for _, d := range got {
				if d.Issue != "t-1" {
					t.Fatalf("missing dep %+v is not about t-1", d)
				}
				if d.Command != "bd dep add t-1 "+d.Sibling {
					t.Fatalf("command %q for sibling %s", d.Command, d.Sibling)
				}
				ids = append(ids, d.Sibling)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("found %v, want %v", ids, tc.want)
			}
		})
	}
}

// The run-level list is what a human reads at the end, so it holds every pair
// once however many attempts or waves mentioned it.
func TestMergeMissingDepsKeepsEachPairOnce(t *testing.T) {
	dep := func(id, sib string) MissingDep {
		return MissingDep{Issue: id, Sibling: sib, Command: "bd dep add " + id + " " + sib}
	}
	got := mergeMissingDeps([]Report{
		{Issue: "t-1", MissingDeps: []MissingDep{dep("t-1", "t-2"), dep("t-1", "t-3")}},
		{Issue: "t-4"},
		{Issue: "t-1", MissingDeps: []MissingDep{dep("t-1", "t-2")}},
		{Issue: "t-5", MissingDeps: []MissingDep{dep("t-5", "t-2")}},
	})
	var ids []string
	for _, d := range got {
		ids = append(ids, d.Issue+"->"+d.Sibling)
	}
	want := "t-1->t-2,t-1->t-3,t-5->t-2"
	if strings.Join(ids, ",") != want {
		t.Fatalf("merged %v, want %s", ids, want)
	}
}

// --- what a judging stage was refused ---

// beads-auto-imp-84g asked for the refused tools to be reported. They already
// are, and TestARefusedReviewerStillVerdictsAndSaysWhatItWasRefused above covers
// the reporting: the notes carry them, the log names them, the verdict stands.
// What was missing is the other half — that a reviewer refused nothing says so
// by saying nothing.

// A reviewer that was refused nothing says nothing about refusals, or the note
// becomes noise a reader learns to skip.
func TestAReviewerRefusedNothingWritesNoRefusalLine(t *testing.T) {
	repo := testRepo(t)
	cfg := withReview(testCfg(3, 0))
	iss := newIssues("t-1")

	e := engine(t, repo, cfg, iss,
		fake.New(fake.Step{Text: "done", Do: steps(commitWork("a.txt"), closes(iss, "t-1"))}),
		fake.New(fake.Step{Text: "VERDICT: PASS"}))
	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	notes, err := os.ReadFile(ReviewNotesPath(repo, "t-1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(notes), "refused") {
		t.Fatalf("a reviewer refused nothing still wrote a refusal line:\n%s", notes)
	}
}
