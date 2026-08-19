package drain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
)

// The barrier is where a drain's separately-correct work meets, and it is the
// one place that has repeatedly turned finished work into parked work. These
// build the states that did it -- overlapping conflicts, branches carrying
// beads' exports, bd rewriting those exports underneath the barrier, and every
// combination of the three -- and assert the two things that matter each time:
// that a real conflict reaches an integrator, and that nothing else does.

// resolvingIntegrator is a model that resolves whatever git left conflicted, by
// keeping both sides in the order they appear. It is deliberately dumb: what is
// under test is which branches reach it and what the barrier does with the tree
// it leaves, not the quality of a resolution.
func resolvingIntegrator(t *testing.T, calls *int32) *fake.Runner {
	t.Helper()
	return fake.New(fake.Step{
		Text:   "kept both sides",
		Events: []runner.Event{{Kind: runner.EventToolUse, Tool: "Edit"}},
		Do: func(_ context.Context, req runner.Request) error {
			atomic.AddInt32(calls, 1)
			out, err := gitOut(req.Dir, "diff", "--name-only", "--diff-filter=U")
			if err != nil {
				return err
			}
			for _, p := range strings.Fields(out) {
				full := filepath.Join(req.Dir, p)
				body, err := os.ReadFile(full)
				if err != nil {
					return err
				}
				if err := os.WriteFile(full, []byte(dropMarkers(string(body))), 0o644); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

// dropMarkers keeps both sides of every conflict hunk and removes the markers,
// which is the smallest resolution that leaves a file git will accept.
func dropMarkers(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "<<<<<<<"),
			strings.HasPrefix(line, "======="),
			strings.HasPrefix(line, ">>>>>>>"):
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func gitOut(dir string, args ...string) (string, error) { return git(dir, args...) }

// branchSpec is one finished worker: the files it wrote, and whether it also
// committed beads' export the way a branch cut from a busy main does.
type branchSpec struct {
	id     string
	files  map[string]string
	export string
}

// stressRepo builds a repo with a committed export and one branch per spec, all
// cut from the same base so that any two writing the same file collide.
func stressRepo(t *testing.T, specs []branchSpec, seed map[string]string) (string, *fakeIssues) {
	t.Helper()
	repo := testRepo(t)
	cfg := testCfg(3, 0)

	seedExport(t, repo, `{"id":"seed","status":"open"}`+"\n")
	for name, body := range seed {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, repo, "add", name)
	}
	if len(seed) > 0 {
		mustGit(t, repo, "commit", "--quiet", "-m", "the base every branch is cut from")
	}

	ids := make([]string, 0, len(specs))
	for _, s := range specs {
		ids = append(ids, s.id)
	}
	iss := newIssues(ids...).under("epic-1", ids...)

	for _, s := range specs {
		first := true
		for _, name := range sortedKeys(s.files) {
			if first {
				finishedWorker(t, repo, cfg, s.id, name, s.files[name])
				first = false
				continue
			}
			commitInWorktree(t, repo, s.id, name, s.files[name])
		}
		if first {
			// A branch with no file of its own still needs to exist.
			finishedWorker(t, repo, cfg, s.id, s.id+".txt", s.id+"\n")
		}
		if s.export != "" {
			commitInWorktree(t, repo, s.id, ".beads/issues.jsonl", s.export)
		}
		iss.set(s.id, "closed")
	}
	waveState(t, repo, "epic-1", ids...)
	return repo, iss
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// commitInWorktree adds one more commit to a branch whose worktree already
// exists, which is how a branch gets more than one file or an export of its own.
func commitInWorktree(t *testing.T, repo, issue, file, body string) {
	t.Helper()
	wt := filepath.Join(repo, ".beads", "auto", "wt", issue)
	full := filepath.Join(wt, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "add", file)
	mustGit(t, wt, "commit", "--quiet", "-m", issue+": "+file)
}

func mergeByIssue(rep IntegrateReport, id string) (Merge, bool) {
	for _, m := range rep.Merges {
		if m.Issue == id {
			return m, true
		}
	}
	return Merge{}, false
}

func summarise(rep IntegrateReport) string {
	var b strings.Builder
	for _, m := range rep.Merges {
		fmt.Fprintf(&b, "\n  %-6s %-9s settled=%v conflicts=%v %s", m.Issue, m.Outcome, m.Settled, m.Conflicts, m.Reason)
	}
	return b.String()
}

// Five branches all rewriting the same file. The first merges cleanly and every
// one after it collides with the result of the last, so the integrator is asked
// five times minus the clean one -- and the file at the end holds every line.
//
// This is the shape a wave of parallel workers on one subsystem produces, and
// the one where parking on the first refusal costs the most.
func TestEveryBranchAfterTheFirstConflictsAndEveryOneReachesTheIntegrator(t *testing.T) {
	specs := []branchSpec{
		{id: "t-1", files: map[string]string{"hot.txt": "one\n"}},
		{id: "t-2", files: map[string]string{"hot.txt": "two\n"}},
		{id: "t-3", files: map[string]string{"hot.txt": "three\n"}},
		{id: "t-4", files: map[string]string{"hot.txt": "four\n"}},
		{id: "t-5", files: map[string]string{"hot.txt": "five\n"}},
	}
	repo, iss := stressRepo(t, specs, map[string]string{"hot.txt": "base\n"})

	var calls int32
	e := engine(t, repo, testCfg(3, 0), iss, fake.New(), resolvingIntegrator(t, &calls))
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != "" {
		t.Fatalf("the barrier stopped: %s (%s)%s", rep.Stopped, rep.Reason, summarise(rep))
	}
	if len(iss.parked) != 0 {
		t.Fatalf("parked %v; every one of these was resolvable%s", iss.parked, summarise(rep))
	}
	if got := int(calls); got != len(specs)-1 {
		t.Fatalf("the integrator ran %d times, want %d -- one per branch that actually collided%s",
			got, len(specs)-1, summarise(rep))
	}
	body := read(t, filepath.Join(repo, "hot.txt"))
	for _, want := range []string{"one", "two", "three", "four", "five"} {
		if !strings.Contains(body, want) {
			t.Fatalf("hot.txt lost %q:\n%s", want, body)
		}
	}
}

// Every branch carries a commit to beads' export, and only to that -- which is
// what a wave of branches cut from a main that has been closing issues looks
// like. None of them is a disagreement about anything, so none of them may cost
// a model call.
func TestAWaveOfExportOnlyConflictsSpawnsNoIntegratorAtAll(t *testing.T) {
	specs := []branchSpec{
		{id: "t-1", files: map[string]string{"a.txt": "a\n"}, export: `{"id":"t-1","status":"closed"}` + "\n"},
		{id: "t-2", files: map[string]string{"b.txt": "b\n"}, export: `{"id":"t-2","status":"closed"}` + "\n"},
		{id: "t-3", files: map[string]string{"c.txt": "c\n"}, export: `{"id":"t-3","status":"closed"}` + "\n"},
		{id: "t-4", files: map[string]string{"d.txt": "d\n"}, export: `{"id":"t-4","status":"closed"}` + "\n"},
	}
	repo, iss := stressRepo(t, specs, nil)

	var calls int32
	e := engine(t, repo, testCfg(3, 0), iss, fake.New(), resolvingIntegrator(t, &calls))
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != "" {
		t.Fatalf("the barrier stopped: %s (%s)%s", rep.Stopped, rep.Reason, summarise(rep))
	}
	if len(iss.parked) != 0 {
		t.Fatalf("parked %v%s", iss.parked, summarise(rep))
	}
	if calls != 0 {
		t.Fatalf("the integrator ran %d times for conflicts beads regenerates%s", calls, summarise(rep))
	}
	for _, s := range specs {
		if !exists(filepath.Join(repo, sortedKeys(s.files)[0])) {
			t.Fatalf("%s did not land%s", s.id, summarise(rep))
		}
	}
}

// The mixed wave, which is the one that tells the two paths apart: some
// branches carry only the export, one carries a real disagreement as well. The
// export must never reach a model and the disagreement must always reach one,
// in the same barrier.
func TestAnExportConflictAndARealOneInTheSameWaveGoToDifferentPlaces(t *testing.T) {
	specs := []branchSpec{
		{id: "t-1", files: map[string]string{"hot.txt": "one\n"}, export: `{"id":"t-1","status":"closed"}` + "\n"},
		{id: "t-2", files: map[string]string{"quiet.txt": "q\n"}, export: `{"id":"t-2","status":"closed"}` + "\n"},
		{id: "t-3", files: map[string]string{"hot.txt": "three\n"}, export: `{"id":"t-3","status":"closed"}` + "\n"},
	}
	repo, iss := stressRepo(t, specs, map[string]string{"hot.txt": "base\n"})

	var calls int32
	e := engine(t, repo, testCfg(3, 0), iss, fake.New(), resolvingIntegrator(t, &calls))
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != "" {
		t.Fatalf("the barrier stopped: %s (%s)%s", rep.Stopped, rep.Reason, summarise(rep))
	}
	if len(iss.parked) != 0 {
		t.Fatalf("parked %v%s", iss.parked, summarise(rep))
	}
	if calls != 1 {
		t.Fatalf("the integrator ran %d times, want once -- for t-3's hot.txt and nothing else%s",
			calls, summarise(rep))
	}
	m, ok := mergeByIssue(rep, "t-3")
	if !ok {
		t.Fatalf("t-3 never reached the barrier%s", summarise(rep))
	}
	if len(m.Settled) == 0 {
		t.Fatalf("t-3 settled no export, so the two kinds were never told apart%s", summarise(rep))
	}
	for _, p := range m.Conflicts {
		if beadsExports[p] {
			t.Fatalf("t-3 sent %s to a model%s", p, summarise(rep))
		}
	}
}

// Everything at once, which is the state beads-auto-imp-04l was actually in:
// branches carrying their own exports, real disagreements between them, and bd
// rewriting both export files in the working tree on every read the barrier
// makes -- unstaged, so there is nothing to unstage.
//
// Each of those alone is handled. The reason to build all three together is
// that the barrier clears the export at three separate points and reads bd
// between them, so the window this reopens is only visible when something is
// writing into it continuously.
func TestBDRewritingBothExportsThroughoutDoesNotCostAWaveItsWork(t *testing.T) {
	specs := []branchSpec{
		{id: "t-1", files: map[string]string{"hot.txt": "one\n"}, export: `{"id":"t-1","status":"closed"}` + "\n"},
		{id: "t-2", files: map[string]string{"hot.txt": "two\n"}},
		{id: "t-3", files: map[string]string{"warm.txt": "three\n"}, export: `{"id":"t-3","status":"closed"}` + "\n"},
		{id: "t-4", files: map[string]string{"hot.txt": "four\n", "warm.txt": "four\n"}},
	}
	repo, iss := stressRepo(t, specs, map[string]string{"hot.txt": "base\n", "warm.txt": "base\n"})

	// interactions.jsonl is tracked too, and bd rewrites it just as often.
	inter := filepath.Join(repo, ".beads", "interactions.jsonl")
	if err := os.WriteFile(inter, []byte(`{"seq":0}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", ".beads/interactions.jsonl")
	mustGit(t, repo, "commit", "--quiet", "-m", "the interaction log, as any beads repo tracks it")

	var writes int32
	iss.onEveryShow(func(string) {
		n := atomic.AddInt32(&writes, 1)
		// Unstaged, both files, different every time -- a bd read, not a commit.
		if err := os.WriteFile(filepath.Join(repo, ".beads", "issues.jsonl"),
			[]byte(fmt.Sprintf(`{"read":%d}`+"\n", n)), 0o644); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(inter, []byte(fmt.Sprintf(`{"seq":%d}`+"\n", n)), 0o644); err != nil {
			t.Error(err)
		}
	})

	var calls int32
	e := engine(t, repo, testCfg(3, 0), iss, fake.New(), resolvingIntegrator(t, &calls))
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != "" {
		t.Fatalf("the barrier stopped: %s (%s)%s", rep.Stopped, rep.Reason, summarise(rep))
	}
	if len(iss.parked) != 0 {
		t.Fatalf("parked %v; nothing here is a fault in a branch%s", iss.parked, summarise(rep))
	}
	if writes == 0 {
		t.Fatal("the barrier never read an issue, so the export was never rewritten under it")
	}
	for _, s := range specs {
		if m, ok := mergeByIssue(rep, s.id); !ok || (m.Outcome != MergeClean && m.Outcome != MergeResolved) {
			t.Fatalf("%s did not land%s", s.id, summarise(rep))
		}
	}
	body := read(t, filepath.Join(repo, "hot.txt"))
	for _, want := range []string{"one", "two", "four"} {
		if !strings.Contains(body, want) {
			t.Fatalf("hot.txt lost %q:\n%s%s", want, body, summarise(rep))
		}
	}
}

// An integrator that leaves the markers in has not resolved anything, and the
// barrier must say so about that branch and no other. This is the park that is
// supposed to happen, and it has to stay reachable now that a dirty checkout
// takes a different path out.
func TestAnIntegratorThatLeavesMarkersParksOnlyItsOwnBranch(t *testing.T) {
	specs := []branchSpec{
		{id: "t-1", files: map[string]string{"hot.txt": "one\n"}},
		{id: "t-2", files: map[string]string{"hot.txt": "two\n"}},
		{id: "t-3", files: map[string]string{"cold.txt": "three\n"}},
	}
	repo, iss := stressRepo(t, specs, map[string]string{"hot.txt": "base\n"})

	lazy := fake.New(fake.Step{Text: "I looked at it"})
	e := engine(t, repo, testCfg(3, 0), iss, fake.New(), lazy)
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != "" {
		t.Fatalf("the barrier stopped on %s; one unresolved branch is not an outage%s", rep.Stopped, summarise(rep))
	}
	m, ok := mergeByIssue(rep, "t-2")
	if !ok || m.Outcome != MergeParked {
		t.Fatalf("t-2 outcome %v; an unresolved conflict is a park%s", m.Outcome, summarise(rep))
	}
	if !strings.Contains(m.Reason, "marker") {
		t.Fatalf("t-2 parked for %q; it must say the markers are still there", m.Reason)
	}
	if len(iss.parked) != 1 || iss.parked[0] != "t-2" {
		t.Fatalf("parked %v, want only t-2%s", iss.parked, summarise(rep))
	}
	// And the barrier carried on: t-3 has nothing to do with t-2's conflict.
	if !exists(filepath.Join(repo, "cold.txt")) {
		t.Fatalf("t-3 was dropped because t-2 could not be resolved%s", summarise(rep))
	}
}

// The peel-back path with bd writing underneath it, which is the third place
// the barrier has to clear the export and the one that runs only after
// something has already gone wrong.
//
// A red gate takes every merge back off newest first, parks the offender, and
// puts the innocent branches back on. Each of those re-merges is a git merge in
// the main checkout, each is preceded by a bd write -- parking the offender
// goes through bd -- and the branches carry their own exports as well. If the
// clear is missing here the failing path fails differently, which is the worst
// place to find out.
func TestARedGatePeelsBackAndRemergesWithBDWritingUnderneath(t *testing.T) {
	repo := testRepo(t)
	counter := filepath.Join(t.TempDir(), "gate-runs")
	cfg := countingGate(testCfg(3, 0), counter, "test ! -f bad.txt")
	iss := newIssues("t-1", "t-2", "t-3", "t-4").under("epic-1", "t-1", "t-2", "t-3", "t-4")

	seedExport(t, repo, `{"id":"seed","status":"open"}`+"\n")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	finishedWorker(t, repo, cfg, "t-2", "bad.txt", "boom\n")
	finishedWorker(t, repo, cfg, "t-3", "c.txt", "c\n")
	finishedWorker(t, repo, cfg, "t-4", "d.txt", "d\n")
	// Two of them carry an export of their own, so the re-merges have one to
	// settle as well as one to be blocked by.
	commitInWorktree(t, repo, "t-3", ".beads/issues.jsonl", `{"id":"t-3","status":"closed"}`+"\n")
	commitInWorktree(t, repo, "t-4", ".beads/issues.jsonl", `{"id":"t-4","status":"closed"}`+"\n")
	for _, id := range []string{"t-1", "t-2", "t-3", "t-4"} {
		iss.set(id, "closed")
	}
	waveState(t, repo, "epic-1", "t-1", "t-2", "t-3", "t-4")

	var writes int32
	iss.onEveryShow(func(string) {
		n := atomic.AddInt32(&writes, 1)
		if err := os.WriteFile(filepath.Join(repo, ".beads", "issues.jsonl"),
			[]byte(fmt.Sprintf(`{"read":%d}`+"\n", n)), 0o644); err != nil {
			t.Error(err)
		}
	})

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != "" {
		t.Fatalf("the barrier stopped: %s (%s)%s", rep.Stopped, rep.Reason, summarise(rep))
	}
	if m, _ := mergeByIssue(rep, "t-2"); m.Outcome != MergeParked {
		t.Fatalf("t-2: outcome %s, want the offender parked%s", m.Outcome, summarise(rep))
	}
	for _, id := range []string{"t-1", "t-3", "t-4"} {
		m, ok := mergeByIssue(rep, id)
		if !ok || !m.Outcome.landedOutcome() {
			t.Fatalf("%s: outcome %v; only the offender may be parked%s", id, m.Outcome, summarise(rep))
		}
	}
	if !rep.GatePassed {
		t.Fatalf("the gate is red after the re-merge: %q%s", rep.Reason, summarise(rep))
	}
	if len(iss.parked) != 1 || iss.parked[0] != "t-2" {
		t.Fatalf("parked %v in bd, want just t-2%s", iss.parked, summarise(rep))
	}
	if !exists(filepath.Join(repo, "c.txt")) || !exists(filepath.Join(repo, "d.txt")) {
		t.Fatalf("a peeled branch did not come back on%s", summarise(rep))
	}
	if exists(filepath.Join(repo, "bad.txt")) {
		t.Fatalf("the offender is still in the tree%s", summarise(rep))
	}
}

// Barriers in sequence, which is where beads-auto-imp-04l actually died. The
// first barrier makes the epic branch; between barriers the checkout is left
// somewhere else and bd rewrites the export; the second barrier has to switch
// back onto a branch whose committed copy of that file is different, merge
// branches that carry their own, and resolve a real conflict as well.
//
// One barrier at a time never reproduced this: the switch only happens from the
// second barrier onwards, and it is the one git command in the barrier that
// runs before any merge and fails the whole run rather than one branch.
func TestBarriersInSequenceSurviveADirtyExportBetweenThem(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2", "t-3").under("epic-1", "t-1", "t-2", "t-3")
	seedExport(t, repo, `{"id":"seed","status":"open"}`+"\n")

	if err := os.WriteFile(filepath.Join(repo, "hot.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "hot.txt")
	mustGit(t, repo, "commit", "--quiet", "-m", "hot.txt")

	// Barrier one.
	finishedWorker(t, repo, cfg, "t-1", "hot.txt", "one\n")
	commitInWorktree(t, repo, "t-1", ".beads/issues.jsonl", `{"id":"t-1","status":"closed"}`+"\n")
	iss.set("t-1", "closed")
	waveState(t, repo, "epic-1", "t-1")

	var calls int32
	first := engine(t, repo, cfg, iss, fake.New(), resolvingIntegrator(t, &calls))
	rep1, err := first.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("barrier one: %v", err)
	}
	epic := rep1.EpicBranch
	if epic == "" {
		t.Fatal("barrier one staged nothing")
	}

	// Between barriers: the checkout wanders off, and bd writes the export.
	mustGit(t, repo, "switch", "--quiet", "--detach", "HEAD~1")
	if err := os.WriteFile(filepath.Join(repo, ".beads", "issues.jsonl"),
		[]byte(`{"read":"between barriers"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Barrier two, with a real conflict against what barrier one landed.
	finishedWorker(t, repo, cfg, "t-2", "hot.txt", "two\n")
	finishedWorker(t, repo, cfg, "t-3", "cool.txt", "three\n")
	commitInWorktree(t, repo, "t-3", ".beads/issues.jsonl", `{"id":"t-3","status":"closed"}`+"\n")
	iss.set("t-2", "closed")
	iss.set("t-3", "closed")

	st := mustLoadState(t, repo)
	st.EpicBranch = epic
	st.WaveIssues = []string{"t-2", "t-3"}
	st.MarkDone("t-2")
	st.MarkDone("t-3")
	mustSaveState(t, repo, st)

	iss.onEveryShow(func(string) {
		_ = os.WriteFile(filepath.Join(repo, ".beads", "issues.jsonl"),
			[]byte(`{"read":"during barrier two"}`+"\n"), 0o644)
	})

	second := engine(t, repo, cfg, iss, fake.New(), resolvingIntegrator(t, &calls))
	rep2, err := second.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("barrier two could not switch onto %s: %v", epic, err)
	}
	if rep2.Stopped != "" {
		t.Fatalf("barrier two stopped: %s (%s)%s", rep2.Stopped, rep2.Reason, summarise(rep2))
	}
	if len(iss.parked) != 0 {
		t.Fatalf("parked %v across two barriers%s", iss.parked, summarise(rep2))
	}
	body := read(t, filepath.Join(repo, "hot.txt"))
	if !strings.Contains(body, "one") || !strings.Contains(body, "two") {
		t.Fatalf("hot.txt lost a side across the two barriers:\n%s%s", body, summarise(rep2))
	}
	if !exists(filepath.Join(repo, "cool.txt")) {
		t.Fatalf("t-3 did not land%s", summarise(rep2))
	}
}

// The lane barrier: continuous scheduling integrates one issue at a time beside
// the workers rather than gathering them, so the same clearing has to happen on
// a path that skips reconcile, the discovery filing and the epic decision.
func TestLaneBarriersClearTheExportToo(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")
	seedExport(t, repo, `{"id":"seed","status":"open"}`+"\n")

	for _, id := range []string{"t-1", "t-2"} {
		finishedWorker(t, repo, cfg, id, id+".txt", id+"\n")
		commitInWorktree(t, repo, id, ".beads/issues.jsonl", `{"id":"`+id+`","status":"closed"}`+"\n")
		iss.set(id, "closed")
	}
	waveState(t, repo, "epic-1", "t-1", "t-2")

	iss.onEveryShow(func(string) {
		_ = os.WriteFile(filepath.Join(repo, ".beads", "issues.jsonl"),
			[]byte(`{"read":"mid-lane"}`+"\n"), 0o644)
	})

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	for _, id := range []string{"t-1", "t-2"} {
		rep, err := e.Integrate(context.Background(), IntegrateOptions{Only: []string{id}, Lane: true})
		if err != nil {
			t.Fatalf("the lane barrier for %s: %v", id, err)
		}
		if rep.Stopped != "" {
			t.Fatalf("%s: the lane barrier stopped: %s (%s)%s", id, rep.Stopped, rep.Reason, summarise(rep))
		}
		if m, ok := mergeByIssue(rep, id); !ok || !m.Outcome.landedOutcome() {
			t.Fatalf("%s: outcome %v in a lane barrier%s", id, m.Outcome, summarise(rep))
		}
	}
	if len(iss.parked) != 0 {
		t.Fatalf("parked %v across two lane barriers", iss.parked)
	}
	for _, id := range []string{"t-1", "t-2"} {
		if !exists(filepath.Join(repo, id+".txt")) {
			t.Fatalf("%s did not land in its lane", id)
		}
	}
}

func mustLoadState(t *testing.T, repo string) *runstate.State {
	t.Helper()
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustSaveState(t *testing.T, repo string, st *runstate.State) {
	t.Helper()
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}
}

// A branch that deletes the export while the epic branch changed it is a
// delete/modify, and it is the one export conflict the settling rule refuses:
// there is no --ours stage to keep, so "keep the copy this branch already had"
// has nothing to name. It is supposed to fall through to a model like any other
// disagreement rather than be silently resolved one way.
func TestADeletedExportIsNotSettledSilently(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")
	seedExport(t, repo, `{"id":"seed","status":"open"}`+"\n")

	// t-1 changes the export, t-2 deletes it. Whichever lands first, the other
	// is a delete/modify.
	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	commitInWorktree(t, repo, "t-1", ".beads/issues.jsonl", `{"id":"t-1","status":"closed"}`+"\n")

	finishedWorker(t, repo, cfg, "t-2", "b.txt", "b\n")
	wt2 := filepath.Join(repo, ".beads", "auto", "wt", "t-2")
	mustGit(t, wt2, "rm", "--quiet", ".beads/issues.jsonl")
	mustGit(t, wt2, "commit", "--quiet", "-m", "t-2: and the export gone")

	iss.set("t-1", "closed")
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1", "t-2")

	var calls int32
	e := engine(t, repo, cfg, iss, fake.New(), resolvingIntegrator(t, &calls))
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != "" {
		t.Fatalf("the barrier stopped: %s (%s)%s", rep.Stopped, rep.Reason, summarise(rep))
	}
	m, ok := mergeByIssue(rep, "t-2")
	if !ok {
		t.Fatalf("t-2 never reached the barrier%s", summarise(rep))
	}
	// Either a model was asked, or it was parked. What it must not be is
	// settled: the rule that settles an export says both sides are the same
	// database, and a deletion is not that.
	if len(m.Settled) != 0 {
		t.Fatalf("t-2 settled %v without a model; a delete/modify is not two views of one database%s",
			m.Settled, summarise(rep))
	}
	if m.Outcome != MergeParked && calls == 0 {
		t.Fatalf("t-2 landed as %s with no model and nothing settled%s", m.Outcome, summarise(rep))
	}
}

// Two branches adding the same path with different content, which git reports
// as an add/add rather than a content conflict. It is a real disagreement and
// has to reach a model like one.
func TestAnAddAddConflictReachesTheIntegrator(t *testing.T) {
	specs := []branchSpec{
		{id: "t-1", files: map[string]string{"new.txt": "from one\n"}},
		{id: "t-2", files: map[string]string{"new.txt": "from two\n"}},
	}
	repo, iss := stressRepo(t, specs, nil)

	var calls int32
	e := engine(t, repo, testCfg(3, 0), iss, fake.New(), resolvingIntegrator(t, &calls))
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != "" {
		t.Fatalf("the barrier stopped: %s (%s)%s", rep.Stopped, rep.Reason, summarise(rep))
	}
	if calls != 1 {
		t.Fatalf("the integrator ran %d times for an add/add, want once%s", calls, summarise(rep))
	}
	if len(iss.parked) != 0 {
		t.Fatalf("parked %v%s", iss.parked, summarise(rep))
	}
	body := read(t, filepath.Join(repo, "new.txt"))
	if !strings.Contains(body, "from one") || !strings.Contains(body, "from two") {
		t.Fatalf("new.txt kept only one side:\n%s", body)
	}
}

// A delete/modify on ordinary work: one branch edits a file, another removes
// it. git leaves the path conflicted with no content to merge, so an integrator
// that only strips markers cannot resolve it -- and the barrier must park that
// branch and carry on rather than stop or lose the rest of the wave.
func TestADeleteModifyConflictParksItsBranchAndNoOther(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2", "t-3").under("epic-1", "t-1", "t-2", "t-3")

	if err := os.WriteFile(filepath.Join(repo, "doomed.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "doomed.txt")
	mustGit(t, repo, "commit", "--quiet", "-m", "doomed.txt")

	finishedWorker(t, repo, cfg, "t-1", "doomed.txt", "edited\n")
	finishedWorker(t, repo, cfg, "t-2", "keep.txt", "k\n")
	wt2 := filepath.Join(repo, ".beads", "auto", "wt", "t-2")
	mustGit(t, wt2, "rm", "--quiet", "doomed.txt")
	mustGit(t, wt2, "commit", "--quiet", "-m", "t-2: doomed.txt removed")
	finishedWorker(t, repo, cfg, "t-3", "later.txt", "l\n")

	for _, id := range []string{"t-1", "t-2", "t-3"} {
		iss.set(id, "closed")
	}
	waveState(t, repo, "epic-1", "t-1", "t-2", "t-3")

	var calls int32
	e := engine(t, repo, cfg, iss, fake.New(), resolvingIntegrator(t, &calls))
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Stopped != "" {
		t.Fatalf("the barrier stopped on %s; one unresolvable branch is not an outage%s", rep.Stopped, summarise(rep))
	}
	if calls == 0 {
		t.Fatalf("a delete/modify never reached a model%s", summarise(rep))
	}
	// t-3 has nothing to do with any of it and must be in the tree.
	if !exists(filepath.Join(repo, "later.txt")) {
		t.Fatalf("t-3 was lost to somebody else's delete/modify%s", summarise(rep))
	}
	if len(iss.parked) > 1 {
		t.Fatalf("parked %v; at most the branch that could not be resolved%s", iss.parked, summarise(rep))
	}
}
