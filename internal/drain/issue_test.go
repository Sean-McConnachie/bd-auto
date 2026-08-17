package drain

import (
	"context"
	"os"
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
	notes  []string
	parked []string
	closed []string
	resets int
	fail   error
}

func newIssues(ids ...string) *fakeIssues {
	f := &fakeIssues{
		status: map[string]string{}, titles: map[string]string{},
		parent: map[string]string{}, deps: map[string][]bd.Ref{},
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

// Ready is bd's blocker-aware ready front: open issues under the parent whose
// blocking dependencies are all closed, in ID order.
func (f *fakeIssues) Ready(parent string, limit int) ([]bd.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	var out []bd.Issue
	for id, st := range f.status {
		if st != "open" || (parent != "" && f.parent[id] != parent) {
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
	}
	return &bd.Issue{ID: id, Title: f.titles[id], Status: st, Parent: f.parent[id], Dependencies: deps}, nil
}

func (f *fakeIssues) AppendNotes(id, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, note)
	return nil
}

// Park records the reason on the issue as well as the status, exactly as
// bd.Client.Park does: the note is the durable half, and a fake that dropped it
// would let a park with no explanation pass.
func (f *fakeIssues) Park(id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, reason)
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
	f.status[id] = "closed"
	f.closed = append(f.closed, id)
	return nil
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
func engine(t *testing.T, repo string, cfg *config.Config, iss Issues, worker, reviewer runner.Runner) *Engine {
	t.Helper()
	return &Engine{
		RepoRoot: repo,
		Cfg:      cfg,
		BD:       iss,
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

// commitWork is a scripted worker that writes a file and commits it through the
// worktree's own hooks, so the guard's trailer is stamped exactly as it would be
// for a real one.
func commitWork(name string) func(context.Context, runner.Request) error {
	return func(_ context.Context, req runner.Request) error {
		if err := os.WriteFile(filepath.Join(req.Dir, name), []byte(name+"\n"), 0o644); err != nil {
			return err
		}
		if _, err := git(req.Dir, "add", "-A"); err != nil {
			return err
		}
		_, err := git(req.Dir, "commit", "--quiet", "-m", "work: "+name)
		return err
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
		iss.set(id, "closed")
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
			name: "guard",
			cfg:  func() *config.Config { return testCfg(3, 0) },
			round1: func(iss *fakeIssues) fake.Step {
				// Commit with the hooks bypassed: no trailer, so the commit
				// looks exactly like one a rebase dragged in from elsewhere.
				return fake.Step{Do: steps(func(_ context.Context, req runner.Request) error {
					if err := os.WriteFile(filepath.Join(req.Dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
						return err
					}
					if _, err := git(req.Dir, "add", "-A"); err != nil {
						return err
					}
					_, err := git(req.Dir, "-c", "core.hooksPath=", "commit", "--quiet", "-m", "untrailed")
					return err
				}, closes(iss, "t-1"))}
			},
			round2: func(*fakeIssues) fake.Step {
				return fake.Step{Do: steps(func(_ context.Context, req runner.Request) error {
					_, err := git(req.Dir, "reset", "--hard", "--quiet", "HEAD~1")
					return err
				}, commitWork("a.txt"))}
			},
			wantIn: "foreign-commit",
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
			name: "not closed",
			cfg:  func() *config.Config { return testCfg(3, 0) },
			round1: func(*fakeIssues) fake.Step {
				return fake.Step{Do: commitWork("a.txt")} // never closes the issue
			},
			round2: func(iss *fakeIssues) fake.Step {
				return fake.Step{Do: steps(commitWork("b.txt"), closes(iss, "t-1"))}
			},
			wantIn: "stopped without finishing",
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
	if !strings.Contains(reqs[1].Prompt, "gate failed") || !strings.Contains(reqs[1].Prompt, "bd show t-1") {
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
		// Attempt 1, round 1: work, but the issue is left open.
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
	e := engine(t, repo, testCfg(2, 1), iss, worker, pass())

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
	if resets != 1 {
		t.Fatalf("%d resets, want the issue returned to the ready queue once", resets)
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
