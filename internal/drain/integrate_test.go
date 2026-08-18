package drain

import (
	"context"
	"os"
	"path/filepath"
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

// waveState records a finished wave: every issue done, nothing in flight.
func waveState(t *testing.T, repo, epic string, ids ...string) {
	t.Helper()
	st := runstate.New(epic, len(ids), "auto", 0)
	st.WaveIssues = append([]string(nil), ids...)
	for _, id := range ids {
		st.MarkDone(id)
	}
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}
}

// finishedWorker leaves behind exactly what a worker that passed leaves behind:
// a worktree on its own branch with one commit on it.
func finishedWorker(t *testing.T, repo string, cfg *config.Config, issue, file, body string) {
	t.Helper()
	wt, err := worktree.Ensure(repo, issue, cfg.Branch(issue), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "add", "-A")
	mustGit(t, wt, "commit", "--quiet", "-m", issue+": "+file)
}

// countingGate is a gate that records every run, so a test can assert how many
// times the barrier gated rather than only what it concluded. check is appended
// to it, so the same gate can also be made to fail.
func countingGate(cfg *config.Config, counter, check string) *config.Config {
	run := "printf 'x\\n' >> " + counter
	if check != "" {
		run += "; " + check
	}
	return withGate(cfg, "count", run)
}

func gateRuns(t *testing.T, counter string) int {
	t.Helper()
	raw, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "x")
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mergeOf(t *testing.T, rep IntegrateReport, issue string) Merge {
	t.Helper()
	for _, m := range rep.Merges {
		if m.Issue == issue {
			return m
		}
	}
	t.Fatalf("no merge recorded for %s in %+v", issue, rep.Merges)
	return Merge{}
}

// --- tests ---

// The whole barrier with nothing to judge: two independent branches, a green
// gate, and not one model spawned.
func TestCleanWaveIntegratesWithoutSpawningAModel(t *testing.T) {
	repo := testRepo(t)
	counter := filepath.Join(t.TempDir(), "gate-runs")
	cfg := countingGate(testCfg(3, 0), counter, "")
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	finishedWorker(t, repo, cfg, "t-2", "b.txt", "b\n")
	iss.set("t-1", "closed")
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1", "t-2")

	model := fake.New()
	e := engine(t, repo, cfg, iss, fake.New(), model)

	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if model.Calls() != 0 {
		t.Fatalf("a clean wave spawned %d model call(s); it must spawn none", model.Calls())
	}
	if got := rep.Merged(); len(got) != 2 {
		t.Fatalf("merged %v, want both branches", got)
	}
	for _, id := range []string{"t-1", "t-2"} {
		if m := mergeOf(t, rep, id); m.Outcome != MergeClean || m.Commit == "" {
			t.Fatalf("%s: outcome %s, commit %q; want a clean merge commit", id, m.Outcome, m.Commit)
		}
	}
	if !exists(filepath.Join(repo, "a.txt")) || !exists(filepath.Join(repo, "b.txt")) {
		t.Fatal("the merged result is missing one of the branches' work")
	}

	// One gate run on the merged result: each branch already passed alone, and
	// the barrier's whole question is whether they pass together.
	if n := gateRuns(t, counter); n != 1 {
		t.Fatalf("the gate ran %d times, want exactly 1 on the merged result", n)
	}
	if !rep.GatePassed {
		t.Fatal("the gate did not pass on the merged result")
	}

	for _, id := range []string{"t-1", "t-2"} {
		if exists(worktree.Path(repo, id)) {
			t.Fatalf("%s: the worktree survived a merged branch", id)
		}
		if branchExists(repo, cfg.Branch(id)) {
			t.Fatalf("%s: the branch survived being merged", id)
		}
	}

	if !rep.EpicClosed {
		t.Fatalf("the epic stayed open after the whole of it landed: %s", rep.EpicReason)
	}
	if _, _, _ = iss.snapshot(); len(iss.closed) != 1 || iss.closed[0] != "epic-1" {
		t.Fatalf("closed %v; the barrier closes the epic and nothing else", iss.closed)
	}
}

// A conflict is the one thing integration cannot decide, so it spawns exactly
// one model — and only for the branch that conflicted.
func TestConflictSpawnsExactlyOneModel(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "seed.txt", "one\n")
	finishedWorker(t, repo, cfg, "t-2", "seed.txt", "two\n")
	iss.set("t-1", "closed")
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1", "t-2")

	model := fake.New(fake.Step{
		Text: "kept both lines",
		Do: func(_ context.Context, req runner.Request) error {
			if err := os.WriteFile(filepath.Join(req.Dir, "seed.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
				return err
			}
			_, err := workerGit(req.Dir, "add", "seed.txt")
			return err
		},
	})
	e := engine(t, repo, cfg, iss, fake.New(), model)

	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if model.Calls() != 1 {
		t.Fatalf("one conflict spawned %d model call(s), want exactly 1", model.Calls())
	}
	if m := mergeOf(t, rep, "t-1"); m.Outcome != MergeClean {
		t.Fatalf("t-1: outcome %s, want the first branch to merge cleanly", m.Outcome)
	}
	m := mergeOf(t, rep, "t-2")
	if m.Outcome != MergeResolved || m.Commit == "" {
		t.Fatalf("t-2: outcome %s (%s), want a resolved merge", m.Outcome, m.Reason)
	}
	if len(m.Conflicts) != 1 || m.Conflicts[0] != "seed.txt" {
		t.Fatalf("conflicted files %v, want seed.txt", m.Conflicts)
	}
	if got := read(t, filepath.Join(repo, "seed.txt")); got != "one\ntwo\n" {
		t.Fatalf("the resolution was not committed: seed.txt is %q", got)
	}
	if mergeInProgress(repo) {
		t.Fatal("the checkout is still mid-merge")
	}

	// The model is given the conflict rather than left to find it, and it runs
	// where the conflict is: the main checkout.
	req := model.Requests()[0]
	if req.Dir != repo {
		t.Fatalf("the integrator ran in %q, not the main checkout %q", req.Dir, repo)
	}
	if !strings.Contains(req.Prompt, "seed.txt") || !strings.Contains(req.Prompt, cfg.Branch("t-2")) {
		t.Fatalf("the conflict prompt names neither the file nor the branch:\n%s", req.Prompt)
	}
	if !rep.EpicClosed {
		t.Fatalf("the epic stayed open: %s", rep.EpicReason)
	}
}

// A conflict the model could not resolve parks that branch and nothing else.
// The rest of the wave still lands: one bad branch must never block the others.
func TestUnresolvedConflictParksOnlyThatBranch(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "seed.txt", "one\n")
	finishedWorker(t, repo, cfg, "t-2", "seed.txt", "two\n")
	iss.set("t-1", "closed")
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1", "t-2")

	model := fake.New(fake.Step{Text: "the two changes contradict each other; I could not keep both"})
	e := engine(t, repo, cfg, iss, fake.New(), model)

	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if m := mergeOf(t, rep, "t-1"); m.Outcome != MergeClean {
		t.Fatalf("t-1: outcome %s; the rest of the wave must still land", m.Outcome)
	}
	m := mergeOf(t, rep, "t-2")
	if m.Outcome != MergeParked {
		t.Fatalf("t-2: outcome %s, want parked", m.Outcome)
	}
	if !strings.Contains(m.Reason, "could not keep both") {
		t.Fatalf("the park reason drops the integrator's account: %s", m.Reason)
	}
	if mergeInProgress(repo) {
		t.Fatal("the failed merge was left in progress instead of aborted")
	}
	if got := read(t, filepath.Join(repo, "seed.txt")); got != "one\n" {
		t.Fatalf("seed.txt is %q; the aborted merge left something behind", got)
	}

	// The work is intact: parking sets a branch aside, it does not destroy it.
	if !branchExists(repo, cfg.Branch("t-2")) || !exists(worktree.Path(repo, "t-2")) {
		t.Fatal("parking removed the branch or worktree of the work that did not land")
	}

	if _, parked, _ := iss.snapshot(); len(parked) != 1 || parked[0] != "t-2" {
		t.Fatalf("parked %v in bd, want just t-2", parked)
	}
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsParked("t-2") || st.IsDone("t-2") {
		t.Fatalf("run state still calls t-2 done: %+v", st)
	}
	if rep.EpicClosed {
		t.Fatal("the epic closed over a parked child")
	}
	if !strings.Contains(rep.EpicReason, "parked") {
		t.Fatalf("the epic reason does not name the parked issue: %s", rep.EpicReason)
	}
}

// The gate is what the barrier exists for: each branch passed alone, and this is
// the run that asks whether they pass together. A red result parks the branch
// that caused it, and takes it back out of the tree.
func TestRedGateParksTheOffendingBranch(t *testing.T) {
	repo := testRepo(t)
	counter := filepath.Join(t.TempDir(), "gate-runs")
	cfg := countingGate(testCfg(3, 0), counter, "test ! -f bad.txt")
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	finishedWorker(t, repo, cfg, "t-2", "bad.txt", "boom\n")
	iss.set("t-1", "closed")
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1", "t-2")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())

	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if m := mergeOf(t, rep, "t-2"); m.Outcome != MergeParked {
		t.Fatalf("t-2: outcome %s, want the offending branch parked", m.Outcome)
	}
	if m := mergeOf(t, rep, "t-1"); m.Outcome != MergeClean {
		t.Fatalf("t-1: outcome %s; an innocent branch must keep its merge", m.Outcome)
	}
	if !exists(filepath.Join(repo, "a.txt")) || exists(filepath.Join(repo, "bad.txt")) {
		t.Fatal("the rollback took the wrong branch out of the merged result")
	}
	// One run on the merged result, then one more to prove the rollback fixed
	// it. A red gate is the only thing that ever gates twice.
	if n := gateRuns(t, counter); n != 2 {
		t.Fatalf("the gate ran %d times, want 1 on the merged result and 1 after the rollback", n)
	}
	if _, parked, _ := iss.snapshot(); len(parked) != 1 || parked[0] != "t-2" {
		t.Fatalf("parked %v in bd, want just t-2", parked)
	}
	if rep.EpicClosed {
		t.Fatal("a red gate closed the epic")
	}
}

// Peeling merges back newest first is what finds the offender, and it takes
// every branch merged after it off the tree as well. Those branches are not
// what the gate was red about, so they go back on and the tree is gated once
// more: only the offender is parked, and the innocent branch that happened to
// follow it is in the merged result.
func TestABranchRolledBackWithTheOffenderIsMergedAgain(t *testing.T) {
	repo := testRepo(t)
	counter := filepath.Join(t.TempDir(), "gate-runs")
	cfg := countingGate(testCfg(3, 0), counter, "test ! -f bad.txt")
	iss := newIssues("t-1", "t-2", "t-3").under("epic-1", "t-1", "t-2", "t-3")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	finishedWorker(t, repo, cfg, "t-2", "bad.txt", "boom\n")
	finishedWorker(t, repo, cfg, "t-3", "c.txt", "c\n")
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		iss.set(id, "closed")
	}
	waveState(t, repo, "epic-1", "t-1", "t-2", "t-3")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())

	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if m := mergeOf(t, rep, "t-2"); m.Outcome != MergeParked {
		t.Fatalf("t-2: outcome %s, want the offending branch parked", m.Outcome)
	}
	if m := mergeOf(t, rep, "t-3"); !m.Outcome.landedOutcome() {
		t.Fatalf("t-3: outcome %s; a branch peeled off with the offender must be merged again", m.Outcome)
	}
	if !rep.GatePassed {
		t.Fatalf("the gate is reported red after the re-merge: %q", rep.Reason)
	}
	if !exists(filepath.Join(repo, "a.txt")) || !exists(filepath.Join(repo, "c.txt")) {
		t.Fatal("an innocent branch is missing from the merged result")
	}
	if exists(filepath.Join(repo, "bad.txt")) {
		t.Fatal("the offending branch is still in the merged result")
	}
	// One on the merged result, one per branch peeled off, and one more on the
	// re-merged tree. That last run is the whole cost of not parking t-3.
	if n := gateRuns(t, counter); n != 4 {
		t.Fatalf("the gate ran %d times, want 1 merged, 2 peeling and 1 after the re-merge", n)
	}
	if _, parked, _ := iss.snapshot(); len(parked) != 1 || parked[0] != "t-2" {
		t.Fatalf("parked %v in bd, want just the offender t-2", parked)
	}
	// The branch that went back on has to be told so: its row was last heard of
	// being rolled back off a tree it is now in.
	if !strings.Contains(rep.Reason, "merged again") {
		t.Fatalf("the report does not say the peeled branch was merged again: %q", rep.Reason)
	}
}

// The re-merge is not a second round of blame. If the branches that came off
// with the offender are still red once they go back on, they come off again and
// are parked exactly as they were before, and the tree is left green.
func TestAStillRedRemergeParksThePeeledBranchesAsBefore(t *testing.T) {
	repo := testRepo(t)
	counter := filepath.Join(t.TempDir(), "gate-runs")
	cfg := countingGate(testCfg(3, 0), counter, "test ! -f bad.txt && test ! -f alsobad.txt")
	iss := newIssues("t-1", "t-2", "t-3").under("epic-1", "t-1", "t-2", "t-3")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	finishedWorker(t, repo, cfg, "t-2", "bad.txt", "boom\n")
	finishedWorker(t, repo, cfg, "t-3", "alsobad.txt", "boom too\n")
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		iss.set(id, "closed")
	}
	waveState(t, repo, "epic-1", "t-1", "t-2", "t-3")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())

	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if m := mergeOf(t, rep, "t-1"); m.Outcome != MergeClean {
		t.Fatalf("t-1: outcome %s; the branch under the offender is untouched by any of this", m.Outcome)
	}
	for _, id := range []string{"t-2", "t-3"} {
		if m := mergeOf(t, rep, id); m.Outcome != MergeParked {
			t.Fatalf("%s: outcome %s, want it parked once the re-merge stayed red", id, m.Outcome)
		}
	}
	if !rep.GatePassed {
		t.Fatalf("the barrier reports a red gate over a tree it restored to green: %q", rep.Reason)
	}
	if !exists(filepath.Join(repo, "a.txt")) {
		t.Fatal("the innocent branch was taken out with the red ones")
	}
	if exists(filepath.Join(repo, "bad.txt")) || exists(filepath.Join(repo, "alsobad.txt")) {
		t.Fatal("a red branch was left in the merged result")
	}
	if _, parked, _ := iss.snapshot(); !equalStrings(sorted(parked), []string{"t-2", "t-3"}) {
		t.Fatalf("parked %v in bd, want both branches the re-merge could not save", parked)
	}
	if rep.EpicClosed {
		t.Fatal("the epic closed over parked work")
	}
}

// A base that was already red is not any branch's fault. Nothing is parked, and
// the wave that merged fine stays merged.
func TestRedBaseParksNothing(t *testing.T) {
	repo := testRepo(t)
	counter := filepath.Join(t.TempDir(), "gate-runs")
	cfg := countingGate(testCfg(3, 0), counter, "test ! -f red.txt")
	iss := newIssues("t-1").under("epic-1", "t-1")

	if err := os.WriteFile(filepath.Join(repo, "red.txt"), []byte("broken before the wave\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "--quiet", "-m", "a red base")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	waveState(t, repo, "epic-1", "t-1")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())

	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.GatePassed {
		t.Fatal("the gate is reported as passing on a red base")
	}
	if _, parked, _ := iss.snapshot(); len(parked) != 0 {
		t.Fatalf("parked %v; a broken base is nobody's branch", parked)
	}
	if !strings.Contains(rep.Reason, "already red") {
		t.Fatalf("the report does not say the base was red: %q", rep.Reason)
	}
	if !exists(filepath.Join(repo, "a.txt")) {
		t.Fatal("the merge was rolled back and never restored")
	}
	if rep.EpicClosed {
		t.Fatal("the epic closed over a red tree")
	}
}

// An interrupt is not a verdict on anyone's branch: the merge is abandoned, the
// wave is left alone, and nothing is parked or closed.
func TestInterruptedConflictStopsWithoutParking(t *testing.T) {
	repo := testRepo(t)
	counter := filepath.Join(t.TempDir(), "gate-runs")
	cfg := countingGate(testCfg(3, 0), counter, "")
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "seed.txt", "one\n")
	finishedWorker(t, repo, cfg, "t-2", "seed.txt", "two\n")
	iss.set("t-1", "closed")
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1", "t-2")

	model := fake.New(fake.Step{Class: runner.ClassInterrupted})
	e := engine(t, repo, cfg, iss, fake.New(), model)

	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != OutcomeInterrupted {
		t.Fatalf("stopped %q, want interrupted", rep.Stopped)
	}
	if m := mergeOf(t, rep, "t-2"); m.Outcome != MergeSkipped {
		t.Fatalf("t-2: outcome %s, want skipped", m.Outcome)
	}
	if mergeInProgress(repo) {
		t.Fatal("the interrupted merge was left in progress")
	}
	if _, parked, _ := iss.snapshot(); len(parked) != 0 {
		t.Fatalf("an interrupt parked %v", parked)
	}
	if n := gateRuns(t, counter); n != 0 {
		t.Fatalf("the gate ran %d times after an interrupt; there is nothing to gate", n)
	}
	if rep.EpicClosed {
		t.Fatal("an interrupted barrier closed the epic")
	}
}

// The close predicate. It is the one decision in the barrier that used to be
// prose in an agent file, and every case below is a way of closing an epic that
// is not finished.
func TestEpicComplete(t *testing.T) {
	// now is fixed so that "deferred" is a property of the fixture rather than
	// of the day the suite runs.
	now := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	later := now.AddDate(3, 0, 0)

	kids := func(pairs ...string) []bd.Issue {
		var out []bd.Issue
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, bd.Issue{ID: pairs[i], Status: pairs[i+1]})
		}
		return out
	}
	// deferChild marks one of kids' issues as deferred past now.
	deferChild := func(in []bd.Issue, ids ...string) []bd.Issue {
		for _, id := range ids {
			for i := range in {
				if in[i].ID == id {
					in[i].DeferUntil = later
				}
			}
		}
		return in
	}

	cases := []struct {
		name      string
		state     func(*runstate.State)
		children  []bd.Issue
		gateGreen bool
		close     bool
		reasonHas string
	}{
		{
			name:      "every child done",
			children:  kids("a", "closed", "b", "closed"),
			gateGreen: true,
			close:     true,
			reasonHas: "2 child issues completed",
		},
		{
			name:      "one child parked",
			state:     func(s *runstate.State) { s.Park("b", "gate never passed", "gate") },
			children:  kids("a", "closed", "b", "blocked"),
			gateGreen: true,
			reasonHas: "parked",
		},
		{
			name: "scope was a subset and the rest is still open",
			state: func(s *runstate.State) {
				s.Scope = []string{"a", "b"}
			},
			children:  kids("a", "closed", "b", "closed", "c", "open", "d", "open"),
			gateGreen: true,
			reasonHas: "scope covered 2 of 4",
		},
		{
			name: "an in-scope child is still open",
			state: func(s *runstate.State) {
				s.Scope = []string{"a", "b"}
			},
			children:  kids("a", "closed", "b", "open"),
			gateGreen: true,
			reasonHas: "still open: b",
		},
		{
			name:      "something still in flight",
			state:     func(s *runstate.State) { s.InFlight["b"] = runstate.Attempt{Branch: "bd-auto/b"} },
			children:  kids("a", "closed", "b", "closed"),
			gateGreen: true,
			reasonHas: "in flight",
		},
		{
			name:      "the gate is red",
			children:  kids("a", "closed"),
			gateGreen: false,
			reasonHas: "gate is red",
		},
		{
			name:      "no children to judge",
			children:  nil,
			gateGreen: true,
			reasonHas: "no child issues",
		},
		{
			// bd counts a deferred issue as open, so without this an epic that
			// a run finished stays open forever over discovered work that is
			// hidden from bd ready until 2029 and belongs to nobody's run.
			name:      "the only unclosed children are deferred",
			children:  deferChild(kids("a", "closed", "b", "open", "c", "open"), "b", "c"),
			gateGreen: true,
			close:     true,
			reasonHas: "2 deferred child issue(s) are not in this run's way: b, c",
		},
		{
			// A deferred child inside the scope is still not something to wait
			// for, but it must not let an epic close on no work at all.
			name:      "every child is deferred and none was finished",
			children:  deferChild(kids("a", "open", "b", "open"), "a", "b"),
			gateGreen: true,
			reasonHas: "all 2 child issue(s) are deferred",
		},
		{
			// The deferral must not shadow a real open child. Nothing closed
			// and something deferred is not the all-deferred case unless the
			// deferred ones are the only unclosed children there are.
			name:      "nothing closed, one child deferred and one genuinely open",
			children:  deferChild(kids("a", "open", "b", "open"), "b"),
			gateGreen: true,
			reasonHas: "1 child issue(s) are still open: a",
		},
		{
			name:      "the epic itself is not a child of itself",
			children:  kids("epic-1", "open", "a", "closed"),
			gateGreen: true,
			close:     true,
			reasonHas: "1 child issues completed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := runstate.New("epic-1", 1, "auto", 0)
			if tc.state != nil {
				tc.state(st)
			}
			v := EpicComplete(st, tc.children, tc.gateGreen, now)
			if v.Close != tc.close {
				t.Fatalf("close=%v, want %v (reason: %s)", v.Close, tc.close, v.Reason)
			}
			if !strings.Contains(v.Reason, tc.reasonHas) {
				t.Fatalf("reason %q does not contain %q", v.Reason, tc.reasonHas)
			}
		})
	}
}

// The barrier merges in the main checkout, and the main checkout is where a
// worker's commit leaves the beads export staged: beads' pre-commit hook fires
// inside the worktree but stages into the index that owns .beads, which is this
// one. git will not merge over a staged path even when the branch being merged
// never touches it, so without this every issue after the first worker commit
// is parked with its work already finished.
func TestAStagedBeadsExportDoesNotStopTheBarrier(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1").under("epic-1", "t-1")

	beads := filepath.Join(repo, ".beads")
	if err := os.MkdirAll(beads, 0o755); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(beads, "issues.jsonl")
	if err := os.WriteFile(export, []byte(`{"id":"t-1","status":"open"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", ".beads/issues.jsonl")
	mustGit(t, repo, "commit", "--quiet", "-m", "the export, as any beads repo tracks it")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	waveState(t, repo, "epic-1", "t-1")

	// What a worker's commit leaves behind: re-exported, and staged here.
	after := `{"id":"t-1","status":"closed"}` + "\n"
	if err := os.WriteFile(export, []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", ".beads/issues.jsonl")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if m := mergeOf(t, rep, "t-1"); m.Outcome != MergeClean {
		t.Fatalf("t-1: outcome %s (%s); a staged export must not park finished work", m.Outcome, m.Reason)
	}
	if !exists(filepath.Join(repo, "a.txt")) {
		t.Fatal("the branch did not land")
	}

	// Unstaged, not discarded and not committed: the export is a passive
	// re-export, and what is in the working tree is what bd last wrote.
	if got := read(t, export); got != after {
		t.Fatalf("the working tree export is %q, want it untouched at %q", got, after)
	}
	if staged := mustGit(t, repo, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Fatalf("still staged after the barrier: %q", staged)
	}
	if tracked := mustGit(t, repo, "show", "HEAD:.beads/issues.jsonl"); strings.Contains(tracked, "closed") {
		t.Fatal("the barrier committed the export on the repo's behalf; it must only unstage it")
	}
}

// TestBDStagingTheExportAgainMidBarrierDoesNotStopTheMerge is the other half of
// the same story, and the half a live run found the hard way.
//
// Unstaging once, on the way into the barrier, fixes nothing: bd re-exports and
// stages on every command including the read ones, and the barrier reads one
// issue per candidate between the unstage and the merge. A run whose wave was
// finished, gated and reviewed then parked all of it at integration.
func TestBDStagingTheExportAgainMidBarrierDoesNotStopTheMerge(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1").under("epic-1", "t-1")

	beads := filepath.Join(repo, ".beads")
	if err := os.MkdirAll(beads, 0o755); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(beads, "issues.jsonl")
	if err := os.WriteFile(export, []byte(`{"id":"t-1","status":"open"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", ".beads/issues.jsonl")
	mustGit(t, repo, "commit", "--quiet", "-m", "the export, as any beads repo tracks it")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	waveState(t, repo, "epic-1", "t-1")

	// What `bd show` does, every time the barrier asks an issue its status.
	after := `{"id":"t-1","status":"closed"}` + "\n"
	iss.onEveryShow(func(string) {
		if err := os.WriteFile(export, []byte(after), 0o644); err != nil {
			t.Error(err)
			return
		}
		mustGit(t, repo, "add", ".beads/issues.jsonl")
	})

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if m := mergeOf(t, rep, "t-1"); m.Outcome != MergeClean {
		t.Fatalf("t-1: outcome %s (%s); a bd read between the unstage and the merge must not park finished work",
			m.Outcome, m.Reason)
	}
	if !exists(filepath.Join(repo, "a.txt")) {
		t.Fatal("the branch did not land")
	}
	if got := read(t, export); got != after {
		t.Fatalf("the working tree export is %q, want what bd last wrote, %q", got, after)
	}
}

// The barrier can run for minutes: it merges in order, and a conflict spawns a
// model. Everything it does has to reach the bus, or a live view watching a run
// whose workers have all finished has nothing to distinguish integrating from
// hung.
func TestTheBarrierAnnouncesItselfAndTagsTheIntegrator(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "seed.txt", "one\n")
	finishedWorker(t, repo, cfg, "t-2", "seed.txt", "two\n")
	iss.set("t-1", "closed")
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1", "t-2")

	model := fake.New(fake.Step{
		Text:   "kept both lines",
		Events: []runner.Event{{Kind: runner.EventToolUse, Tool: "Edit"}},
		Do: func(_ context.Context, req runner.Request) error {
			if err := os.WriteFile(filepath.Join(req.Dir, "seed.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
				return err
			}
			_, err := workerGit(req.Dir, "add", "seed.txt")
			return err
		},
	})

	var mu sync.Mutex
	var opened []string
	var tagged []string
	e := engine(t, repo, cfg, iss, fake.New(), model)
	e.Bus = NewBus(ObserverFunc(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		switch ev.Kind {
		case EventWaveIntegrating:
			opened = append(opened, ev.Issues...)
		case EventActivity:
			if ev.Role == runner.RoleIntegrator {
				tagged = append(tagged, ev.Issue)
			}
		}
	}))

	if _, err := e.Integrate(context.Background(), IntegrateOptions{}); err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !equalStrings(opened, []string{"t-1", "t-2"}) {
		t.Fatalf("the barrier opened on %v, want both branches it was about to merge", opened)
	}
	// Not "the integrator" in the abstract: the issue whose branch it resolved,
	// which is the row a watcher would look for it on. Every event it produced,
	// not merely the first.
	if len(tagged) == 0 {
		t.Fatal("the integrator's activity never reached the bus")
	}
	for _, id := range tagged {
		if id != "t-2" {
			t.Fatalf("the integrator's activity was tagged %v, want every event on t-2", tagged)
		}
	}
}

// A barrier is not one event at each end with minutes of nothing between them.
// It merges branch by branch, gates the merged result, and when that gate comes
// back red it peels merges off one at a time until it finds the branch to
// blame — and every one of those steps has to reach whoever is watching, or a
// display can only show the barrier starting and, eventually, its verdict.
func TestTheBarrierSaysWhatItIsDoingBranchByBranch(t *testing.T) {
	repo := testRepo(t)
	counter := filepath.Join(t.TempDir(), "gate-runs")
	cfg := countingGate(testCfg(3, 0), counter, "test ! -f bad.txt")
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	finishedWorker(t, repo, cfg, "t-2", "bad.txt", "boom\n")
	iss.set("t-1", "closed")
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1", "t-2")

	var mu sync.Mutex
	var seen []Event
	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	e.Bus = NewBus(ObserverFunc(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ev)
	}))

	if _, err := e.Integrate(context.Background(), IntegrateOptions{}); err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	kinds := map[EventKind]int{}
	for _, ev := range seen {
		kinds[ev.Kind]++
	}
	for kind, want := range map[EventKind]int{
		EventMergeStart: 2,
		// Three: one per branch, and one more for the branch the rollback
		// blamed, which landed and was then taken back out.
		EventMergeEnd:      3,
		EventWaveGateStart: 2,
		EventWaveGateEnd:   2,
		EventWaveRollback:  1,
	} {
		if kinds[kind] != want {
			t.Fatalf("the barrier raised %d %s event(s), want %d", kinds[kind], kind, want)
		}
	}

	// The gate the barrier ran on the merged result, and the one that proved
	// the rollback fixed it.
	var verdicts []bool
	for _, ev := range seen {
		if ev.Kind == EventWaveGateEnd {
			verdicts = append(verdicts, ev.Passed)
		}
	}
	if len(verdicts) != 2 || verdicts[0] || !verdicts[1] {
		t.Fatalf("the gate reported %v, want red on the merged result and green after the rollback", verdicts)
	}

	// The branch the gate was blamed on, said twice: it landed, and then the
	// gate took it back out. The second word is the one that stands.
	var outcomes []MergeOutcome
	for _, ev := range seen {
		if ev.Kind == EventMergeEnd && ev.Issue == "t-2" {
			if ev.Merge == nil {
				t.Fatal("a merge-end carried no merge; a watcher has nothing to render")
			}
			outcomes = append(outcomes, ev.Merge.Outcome)
		}
	}
	if len(outcomes) != 2 || !outcomes[0].landedOutcome() || outcomes[1] != MergeParked {
		t.Fatalf("t-2 ended as %v, want it landing and then being parked by the gate", outcomes)
	}
	for _, ev := range seen {
		if ev.Kind == EventWaveRollback && ev.Issue != "t-2" {
			t.Fatalf("the barrier rolled back %s, want the branch it went on to blame", ev.Issue)
		}
	}
}

// A base that was already red blames nobody, and the wave goes back on. Every
// row this said was rolled back has to be told it landed after all, or a
// display leaves the whole wave showing as rolled back over a tree that has all
// of it in.
func TestARedBaseTellsTheRowsTheyLandedAfterAll(t *testing.T) {
	repo := testRepo(t)
	counter := filepath.Join(t.TempDir(), "gate-runs")
	cfg := countingGate(testCfg(3, 0), counter, "test ! -f red.txt")
	iss := newIssues("t-1").under("epic-1", "t-1")

	if err := os.WriteFile(filepath.Join(repo, "red.txt"), []byte("broken before the wave\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "--quiet", "-m", "a red base")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	waveState(t, repo, "epic-1", "t-1")

	var mu sync.Mutex
	var ends []MergeOutcome
	var rollbacks int
	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	e.Bus = NewBus(ObserverFunc(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		switch ev.Kind {
		case EventMergeEnd:
			if ev.Merge != nil {
				ends = append(ends, ev.Merge.Outcome)
			}
		case EventWaveRollback:
			rollbacks++
		}
	}))

	if _, err := e.Integrate(context.Background(), IntegrateOptions{}); err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if rollbacks != 1 {
		t.Fatalf("the barrier rolled back %d branch(es), want the one it had to try", rollbacks)
	}
	if len(ends) != 2 || !ends[0].landedOutcome() || !ends[1].landedOutcome() {
		t.Fatalf("t-1 ended as %v, want it landing and being put back after the base was blamed", ends)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
