package drain

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bd-auto/internal/config"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
)

// --- harness ---

// fakeForge records what a run tried to publish, and can be made to refuse at
// each of the three points a real forge can: unavailable, a failed push, a
// failed create.
type fakeForge struct {
	mu sync.Mutex

	// unavailable, pushErr and openErr are the three refusals.
	unavailable string
	pushErr     error
	openErr     error
	// existing makes Open find a pull request instead of creating one.
	existing string

	pushes []string
	opened []PullRequest
	url    string
}

func newForge() *fakeForge { return &fakeForge{url: "https://example.invalid/pr/1"} }

func (f *fakeForge) Available(_, _ string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unavailable
}

func (f *fakeForge) Push(_ context.Context, _, remote, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushes = append(f.pushes, remote+" "+branch)
	return nil
}

func (f *fakeForge) Open(_ context.Context, _ string, pr PullRequest) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return "", false, f.openErr
	}
	f.opened = append(f.opened, pr)
	if f.existing != "" {
		return f.existing, false, nil
	}
	return f.url, true, nil
}

func (f *fakeForge) calls() (pushes []string, opened []PullRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pushes...), append([]PullRequest(nil), f.opened...)
}

// headOf returns the commit a branch points at, or "" when it does not exist.
func headOf(t *testing.T, repo, branch string) string {
	t.Helper()
	out, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return out
}

// stagedBranch is the epic branch a finished run recorded, or "" for none.
func stagedBranch(t *testing.T, repo string) string {
	t.Helper()
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	return st.EpicBranch
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// --- tests ---

// The default shape of a run, end to end: nothing reaches the base branch, the
// work is staged on one epic branch, and the only thing published is a pull
// request asking a human to land it.
func TestACleanRunStagesOnAnEpicBranchAndOpensAPullRequest(t *testing.T) {
	repo := testRepo(t)
	mainHead := headOf(t, repo, "main")
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2").dependsOn("t-2", "t-1")

	cfg := withGate(testCfg(1, 0), "build", "true")
	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))
	workers.script("t-2", closeAndCommit(iss, "t-2", "two.txt"))

	e := drainEngine(t, repo, cfg, iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"}, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("run outcome %s (%s)", rep.Outcome, rep.Reason)
	}

	// The whole promise: main is exactly where it was.
	if now := headOf(t, repo, "main"); now != mainHead {
		t.Fatalf("main moved from %s to %s; a run must not write to its base branch", mainHead, now)
	}
	branch := stagedBranch(t, repo)
	if branch == "" || !strings.HasPrefix(branch, config.DefaultEpicBranchPrefix) {
		t.Fatalf("epic branch %q does not look like a staging branch", branch)
	}
	if !strings.Contains(branch, "epic-1") {
		t.Fatalf("epic branch %q does not name the epic", branch)
	}
	if got := currentBranch(repo); got != branch {
		t.Fatalf("the checkout is on %s, not the epic branch %s", got, branch)
	}
	// Both issues are in the merged result, and it is the epic branch that has
	// them: a second wave that branched from main would have lost the first.
	for _, f := range []string{"one.txt", "two.txt"} {
		if !exists(filepath.Join(repo, f)) {
			t.Fatalf("%s is missing from the staged result", f)
		}
	}
	if rep.Waves != 2 {
		t.Fatalf("ran %d wave(s); t-2 depends on t-1, so it takes two", rep.Waves)
	}

	pushes, opened := forge.calls()
	if len(pushes) != 1 || pushes[0] != "origin "+branch {
		t.Fatalf("pushed %v, want the epic branch once to origin", pushes)
	}
	if len(opened) != 1 {
		t.Fatalf("opened %d pull request(s), want exactly 1", len(opened))
	}
	pr := opened[0]
	if pr.Base != "main" || pr.Head != branch {
		t.Fatalf("pull request is %s <- %s, want main <- %s", pr.Base, pr.Head, branch)
	}
	if !strings.Contains(pr.Title, "epic-1") {
		t.Fatalf("the title does not name the epic: %q", pr.Title)
	}
	for _, want := range []string{"t-1", "t-2", "main", "## Gate", "build"} {
		if !strings.Contains(pr.Body, want) {
			t.Fatalf("the pull request body never mentions %q:\n%s", want, pr.Body)
		}
	}

	if rep.Handoff == nil || rep.Handoff.URL != forge.url || !rep.Handoff.Created {
		t.Fatalf("the report does not carry the pull request: %+v", rep.Handoff)
	}
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.PR != forge.url || st.Base != "main" {
		t.Fatalf("run state does not record the handoff: pr=%q base=%q", st.PR, st.Base)
	}
}

// A red gate at the barrier parks the branch that caused it, and a parked issue
// is required work that did not get done. Nothing is published, and the branch
// is left exactly where it is so a human can see what happened.
func TestARedGateOpensNoPullRequestAndKeepsTheBranch(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	// The gate passes in the worker's worktree and fails on the merged result:
	// the file only exists once the branch has landed somewhere the gate can see
	// it as merged work.
	cfg := withGate(testCfg(1, 0), "no-bad", "test ! -f bad.txt")
	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "bad.txt"))

	e := drainEngine(t, repo, cfg, iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	pushes, opened := forge.calls()
	if len(pushes) != 0 || len(opened) != 0 {
		t.Fatalf("a red gate published something: pushes=%v prs=%d", pushes, len(opened))
	}
	branch := stagedBranch(t, repo)
	if branch == "" || headOf(t, repo, branch) == "" {
		t.Fatalf("the epic branch %q was not left in place for inspection", branch)
	}
	if rep.Handoff == nil || rep.Handoff.URL != "" {
		t.Fatalf("a pull request was reported for a red run: %+v", rep.Handoff)
	}
	if !strings.Contains(rep.Handoff.Reason, "parked") {
		t.Fatalf("the reason does not say what stopped the handoff: %q", rep.Handoff.Reason)
	}
}

// A parked issue is the other half of the same rule, reached without any gate:
// a worker that finishes nothing parks, and a run with parked work is not a
// result to ask anyone to review.
func TestAParkedIssueOpensNoPullRequest(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))
	// t-2's worker changes nothing and closes nothing: one attempt, no retries.
	workers.script("t-2", fake.Step{Text: "I had a think about it"})

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"}, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !has(rep.Parked, "t-2") {
		t.Fatalf("t-2 did nothing and must be parked: %+v", rep.Parked)
	}

	pushes, opened := forge.calls()
	if len(pushes) != 0 || len(opened) != 0 {
		t.Fatalf("a run with parked work published something: pushes=%v prs=%d", pushes, len(opened))
	}
	// t-1 still landed, and it is still on the branch: parking t-2 does not
	// throw away the work that did finish.
	branch := stagedBranch(t, repo)
	if branch == "" {
		t.Fatal("no epic branch was produced")
	}
	if !exists(filepath.Join(repo, "one.txt")) {
		t.Fatal("the work that did land is not on the epic branch")
	}
	if !strings.Contains(rep.Handoff.Reason, "t-2") {
		t.Fatalf("the reason does not name the parked issue: %q", rep.Handoff.Reason)
	}
}

// The switch the issue asks for: pull requests off still produces the epic
// branch, with everything on it, and touches no remote at all.
func TestPullRequestsOffStillProduceTheEpicBranch(t *testing.T) {
	repo := testRepo(t)
	mainHead := headOf(t, repo, "main")
	iss := newIssues("t-1").under("epic-1", "t-1")

	cfg := testCfg(1, 0)
	cfg.Handoff.PR = config.No()
	if cfg.StageOnBranch() != true || cfg.OpenPR() != false {
		t.Fatalf("turning the pull request off must leave the branch on: branch=%v pr=%v",
			cfg.StageOnBranch(), cfg.OpenPR())
	}

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))

	e := drainEngine(t, repo, cfg, iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone || len(rep.Parked) > 0 {
		t.Fatalf("the run did not finish clean: %s %v", rep.Outcome, rep.Parked)
	}

	pushes, opened := forge.calls()
	if len(pushes) != 0 || len(opened) != 0 {
		t.Fatalf("pr: false still published: pushes=%v prs=%d", pushes, len(opened))
	}
	branch := stagedBranch(t, repo)
	if branch == "" || headOf(t, repo, branch) == "" {
		t.Fatal("pr: false must still produce the epic branch")
	}
	if now := headOf(t, repo, "main"); now != mainHead {
		t.Fatal("pr: false still wrote to the base branch")
	}
	if !exists(filepath.Join(repo, "one.txt")) {
		t.Fatal("the epic branch does not carry the work")
	}
	if !strings.Contains(rep.Handoff.Reason, "handoff.pr") {
		t.Fatalf("the reason does not say the pull request was switched off: %q", rep.Handoff.Reason)
	}
}

// The other switch, and the escape hatch back to the old behaviour: no epic
// branch means the merges land on the branch the run started on.
func TestStagingOffMergesStraightIntoTheBaseBranch(t *testing.T) {
	repo := testRepo(t)
	mainHead := headOf(t, repo, "main")
	iss := newIssues("t-1").under("epic-1", "t-1")

	cfg := testCfg(1, 0)
	cfg.Handoff.Branch, cfg.Handoff.PR = config.No(), config.No()

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))

	e := drainEngine(t, repo, cfg, iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if currentBranch(repo) != "main" {
		t.Fatalf("the checkout left main for %s", currentBranch(repo))
	}
	if headOf(t, repo, "main") == mainHead {
		t.Fatal("nothing was merged into main, which is the whole of this mode")
	}
	if stagedBranch(t, repo) != "" {
		t.Fatalf("an epic branch was created anyway: %s", stagedBranch(t, repo))
	}
	if pushes, opened := forge.calls(); len(pushes) != 0 || len(opened) != 0 {
		t.Fatalf("an unstaged run published something: pushes=%v prs=%d", pushes, len(opened))
	}
	if !strings.Contains(rep.Handoff.Reason, "main") {
		t.Fatalf("the reason does not say where the run went: %q", rep.Handoff.Reason)
	}
}

// The dangerous case, and the reason the run's error is threaded through its
// single exit: a drain that aborts part-way, AFTER a barrier has already merged
// a wave green, has everything a naive check looks for — issues landed, a green
// gate, nothing parked yet — and none of what those facts are supposed to mean.
// Publishing there asks a human to review an epic the run abandoned, with a body
// that says nothing is parked because nothing had got as far as parking.
func TestARunThatAbortsAfterAGreenBarrierPublishesNothing(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2").dependsOn("t-2", "t-1")
	// bd goes unreachable between the two waves: the first wave plans and lands,
	// and the planning call for the second never comes back.
	iss.failReadyFrom(2, errors.New("bd is unreachable"))

	cfg := withGate(testCfg(1, 0), "build", "true")
	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))
	workers.script("t-2", closeAndCommit(iss, "t-2", "two.txt"))

	e := drainEngine(t, repo, cfg, iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"}, Concurrency: 2,
	})
	if err == nil {
		t.Fatal("the drain swallowed the error that stopped it")
	}

	// The setup has to have reached the dangerous state, or this test proves
	// nothing: one wave merged, green, and nothing parked.
	if len(rep.Integrations) != 1 || !rep.Integrations[0].GatePassed {
		t.Fatalf("the first barrier did not merge green, so the risky case was never reached: %+v", rep.Integrations)
	}
	if len(rep.Landed()) != 1 || len(rep.Parked) != 0 {
		t.Fatalf("want exactly one issue landed and nothing parked, got landed=%v parked=%v",
			rep.Landed(), rep.Parked)
	}

	if pushes, opened := forge.calls(); len(pushes) != 0 || len(opened) != 0 {
		t.Fatalf("an abandoned run published itself: pushes=%v prs=%d", pushes, len(opened))
	}
	if rep.Outcome == OutcomeDone {
		t.Fatalf("a run that stopped on an error reported itself done: %+v", rep.Reason)
	}
	if rep.Handoff == nil || rep.Handoff.URL != "" {
		t.Fatalf("a pull request was reported for an abandoned run: %+v", rep.Handoff)
	}
	if !strings.Contains(rep.Handoff.Reason, string(rep.Outcome)) {
		t.Fatalf("the reason does not say the run did not finish: %q", rep.Handoff.Reason)
	}
	// t-2 never ran, and the epic is not closed over it.
	if rep.EpicClosed {
		t.Fatal("an abandoned run closed its epic")
	}
	// The branch is intact, as it is after every refused handoff.
	if headOf(t, repo, stagedBranch(t, repo)) == "" {
		t.Fatal("the epic branch was not left in place")
	}
}

// A forge that is not there is not a failed run. The work is already committed
// to a branch by the time the handoff runs, so an absent gh costs a line of
// explanation and nothing else.
func TestAnUnavailableForgeLeavesTheBranchAndSaysWhy(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	forge := newForge()
	forge.unavailable = "the gh CLI is not on PATH"
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("a missing forge failed the run: %s (%s)", rep.Outcome, rep.Reason)
	}
	if pushes, _ := forge.calls(); len(pushes) != 0 {
		t.Fatalf("something was pushed to a forge that said it was unavailable: %v", pushes)
	}
	if !strings.Contains(rep.Handoff.Reason, "gh CLI") || rep.Handoff.Pushed {
		t.Fatalf("the handoff does not explain itself: %+v", rep.Handoff)
	}
	if headOf(t, repo, stagedBranch(t, repo)) == "" {
		t.Fatal("the branch was not left in place")
	}
}

// Re-running a handoff over a branch that already has a pull request finds it
// rather than failing. The branch was just pushed again, so the request that is
// open now carries this run's work.
func TestAnExistingPullRequestIsFoundRatherThanFailed(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	forge := newForge()
	forge.existing = "https://example.invalid/pr/7"
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Handoff.URL != forge.existing || rep.Handoff.Created {
		t.Fatalf("an existing pull request was not reported as one: %+v", rep.Handoff)
	}
	if !strings.Contains(rep.Handoff.Reason, "already open") {
		t.Fatalf("the reason does not say the request already existed: %q", rep.Handoff.Reason)
	}
}

// The predicate on its own. Every case here is a way of asking a human to
// review a run that is not finished.
func TestHandoffReady(t *testing.T) {
	green := IntegrateReport{
		GatePassed: true,
		Merges:     []Merge{{Issue: "a", Outcome: MergeClean}},
	}
	done := func(mod func(*DrainReport)) DrainReport {
		r := DrainReport{
			Outcome: OutcomeDone, Base: "main",
			Integrations: []IntegrateReport{green},
		}
		if mod != nil {
			mod(&r)
		}
		return r
	}

	cases := []struct {
		name      string
		rep       DrainReport
		staged    string
		pr        bool
		open      bool
		reasonHas string
	}{
		{
			name: "everything landed green", rep: done(nil), staged: "epic-br", pr: true,
			open: true, reasonHas: "1 issue(s) landed",
		},
		{
			name: "the run was not staged", rep: done(nil), staged: "", pr: true,
			reasonHas: "no epic branch",
		},
		{
			name: "pull requests are off", rep: done(nil), staged: "epic-br", pr: false,
			reasonHas: "handoff.pr",
		},
		{
			name:   "the run was interrupted",
			rep:    done(func(r *DrainReport) { r.Outcome = OutcomeInterrupted }),
			staged: "epic-br", pr: true, reasonHas: "interrupted",
		},
		{
			name:   "an issue is parked",
			rep:    done(func(r *DrainReport) { r.Parked = []string{"b"} }),
			staged: "epic-br", pr: true, reasonHas: "parked",
		},
		{
			name: "the gate is red",
			rep: done(func(r *DrainReport) {
				r.Integrations = []IntegrateReport{{GatePassed: false, Merges: green.Merges}}
			}),
			staged: "epic-br", pr: true, reasonHas: "gate is red",
		},
		{
			name:   "no barrier ran",
			rep:    done(func(r *DrainReport) { r.Integrations = nil }),
			staged: "epic-br", pr: true, reasonHas: "no wave reached a barrier",
		},
		{
			name: "nothing landed",
			rep: done(func(r *DrainReport) {
				r.Integrations = []IntegrateReport{{GatePassed: true,
					Merges: []Merge{{Issue: "a", Outcome: MergeParked}}}}
			}),
			staged: "epic-br", pr: true, reasonHas: "nothing to hand over",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := HandoffReady(tc.rep, tc.staged, tc.pr)
			if v.Open != tc.open {
				t.Fatalf("open=%v, want %v (reason: %s)", v.Open, tc.open, v.Reason)
			}
			if !strings.Contains(v.Reason, tc.reasonHas) {
				t.Fatalf("reason %q does not contain %q", v.Reason, tc.reasonHas)
			}
		})
	}
}

// The branch name has to be unique across runs: a second run over the same epic
// must never land on a branch that already has a pull request open against it.
func TestEpicBranchNameIsUniquePerRun(t *testing.T) {
	at := mustTime(t, "2026-08-17T14:12:30Z")
	got := EpicBranchName(config.DefaultEpicBranchPrefix, "epic-1", at)
	if got != "bd-auto/epic/epic-1-20260817-141230" {
		t.Fatalf("branch name %q", got)
	}
	if later := EpicBranchName(config.DefaultEpicBranchPrefix, "epic-1", at.Add(60e9)); later == got {
		t.Fatalf("two runs a minute apart produced the same branch %q", got)
	}
	// A run scoped issue by issue has no epic, and still needs a name.
	if noEpic := EpicBranchName(config.DefaultEpicBranchPrefix, "", at); noEpic != "bd-auto/epic/run-20260817-141230" {
		t.Fatalf("a run with no epic got %q", noEpic)
	}
}
