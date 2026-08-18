package wave

import (
	"errors"
	"strings"
	"testing"

	"bd-auto/internal/bd"
	"bd-auto/internal/runstate"
)

// fakeSource stands in for bd. Ready returns a fixed front; Show serves notes
// and dependencies. An id in neither map is one bd does not have.
type fakeSource struct {
	ready []bd.Issue
	notes map[string]string
	deps  map[string][]bd.Ref
	err   error
	shown []string
}

func (f *fakeSource) Ready(parent string, limit int) ([]bd.Issue, error) {
	return f.ready, f.err
}

func (f *fakeSource) Show(id string) (*bd.Issue, error) {
	f.shown = append(f.shown, id)
	n, hasNotes := f.notes[id]
	d, hasDeps := f.deps[id]
	if !hasNotes && !hasDeps {
		return nil, errors.New("no such issue")
	}
	return &bd.Issue{ID: id, Notes: n, Dependencies: d}, nil
}

func state() *runstate.State { return runstate.New("epic-1", 3, "auto", 1) }

func opts(concurrency int) Options {
	return Options{Concurrency: concurrency, Branch: func(id string) string { return "bd-auto/" + id }}
}

func TestPlanTakesTheReadyFrontUpToTheCap(t *testing.T) {
	src := &fakeSource{ready: []bd.Issue{
		{ID: "a", Title: "A", IssueType: "task", Priority: 1},
		{ID: "b"},
		{ID: "c"},
	}}
	res, err := Plan(src, state(), opts(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 2 {
		t.Fatalf("want 2 issues, got %d", len(res.Issues))
	}
	got := res.Issues[0]
	if got.ID != "a" || got.Title != "A" || got.Type != "task" || got.Priority != 1 {
		t.Fatalf("issue fields not carried through: %+v", got)
	}
	if got.Branch != "bd-auto/a" {
		t.Fatalf("branch not from Options.Branch: %q", got.Branch)
	}
	if got.Attempt != 1 || got.RetryContext != "" {
		t.Fatalf("first attempt should carry no retry context: %+v", got)
	}
	if res.Drained {
		t.Fatal("a wave with issues is not drained")
	}
}

func TestPlanSkipsWhatTheRunAlreadyHandled(t *testing.T) {
	st := state()
	st.Done = []string{"a"}
	st.Park("b", "failed", "implement")
	st.InFlight["c"] = runstate.Attempt{Branch: "bd-auto/c"}
	src := &fakeSource{ready: []bd.Issue{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}}

	res, err := Plan(src, st, opts(5))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 1 || res.Issues[0].ID != "d" {
		t.Fatalf("want only d, got %+v", res.Issues)
	}
	if res.Drained {
		t.Fatal("not drained while work remains")
	}
}

// Drained is the run's terminating condition, so it must not fire while a
// worker is still out.
func TestPlanDrained(t *testing.T) {
	st := state()
	src := &fakeSource{}
	res, err := Plan(src, st, opts(3))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Drained {
		t.Fatal("nothing ready and nothing in flight is drained")
	}

	st.InFlight["a"] = runstate.Attempt{Branch: "bd-auto/a"}
	res, err = Plan(src, st, opts(3))
	if err != nil {
		t.Fatal(err)
	}
	if res.Drained {
		t.Fatal("an in-flight issue means the run is not drained")
	}
}

func TestPlanCarriesRetryContextOnASecondAttempt(t *testing.T) {
	st := state()
	st.Attempts["a"] = 1
	src := &fakeSource{
		ready: []bd.Issue{{ID: "a"}},
		notes: map[string]string{"a": "earlier hand-written note\nbd-auto attempt 1: gate failed\n"},
	}
	res, err := Plan(src, st, opts(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 1 || res.Issues[0].Attempt != 2 {
		t.Fatalf("want a second attempt, got %+v", res.Issues)
	}
	if res.Issues[0].RetryContext != "bd-auto attempt 1: gate failed" {
		t.Fatalf("retry context not carried: %q", res.Issues[0].RetryContext)
	}
}

// Run state wins over the notes, and it is the only source that has to work.
//
// The note on the issue is reverted by beads' post-checkout hook when the next
// attempt's worktree is created, so a planner that trusted it would report a
// retry as informed when it is not. Here the notes hold a stale account and bd
// would happily serve it; the planner must prefer bd-auto's own record, and
// must still produce one when the issue has no note left at all.
func TestPlanPrefersRunStateOverTheIssueNotes(t *testing.T) {
	st := state()
	st.Attempts["a"] = 1
	st.RecordFailure("a", runstate.Failure{
		Attempt: 1, Of: 2, Stage: "gate", Reason: "go test ./... failed in internal/drain",
	})
	src := &fakeSource{
		ready: []bd.Issue{{ID: "a"}},
		notes: map[string]string{"a": "bd-auto attempt 1: a stale account from an older run"},
	}
	res, err := Plan(src, st, opts(3))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Issues[0].RetryContext
	if !strings.Contains(got, "internal/drain") || strings.Contains(got, "stale account") {
		t.Fatalf("retry context came from the notes, not from run state: %q", got)
	}

	// And with the note gone entirely — which is the state the hook leaves the
	// issue in — the retry context is still there.
	src.notes = map[string]string{"a": "no attempt history here at all"}
	res, err = Plan(src, st, opts(3))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Issues[0].RetryContext, "internal/drain") {
		t.Fatalf("a wiped note left the retry blind: %q", res.Issues[0].RetryContext)
	}
}

// A first attempt must not pay for a bd show it cannot use.
func TestPlanDoesNotLookUpNotesOnAFirstAttempt(t *testing.T) {
	src := &fakeSource{ready: []bd.Issue{{ID: "a"}}}
	if _, err := Plan(src, state(), opts(3)); err != nil {
		t.Fatal(err)
	}
	if len(src.shown) != 0 {
		t.Fatalf("unexpected bd show calls: %v", src.shown)
	}
}

func TestPlanPropagatesReadyErrors(t *testing.T) {
	src := &fakeSource{err: errors.New("bd exploded")}
	if _, err := Plan(src, state(), opts(3)); err == nil {
		t.Fatal("want the bd error back")
	}
}

func TestRetryContextTakesTheLastNote(t *testing.T) {
	cases := map[string]string{
		"":                          "",
		"nothing from bd-auto here": "",
		"bd-auto attempt 1: a\nbd-auto attempt 2: b": "bd-auto attempt 2: b",
	}
	for in, want := range cases {
		if got := RetryContext(in); got != want {
			t.Fatalf("RetryContext(%q) = %q, want %q", in, got, want)
		}
	}
}

// The scope is a hard allowlist, and this is the assertion that says so: bd is
// offering the issue, the run has never touched it, and it is still not
// dispatched. That is what keeps discovered work — filed by a worker mid-run,
// deferred, and perfectly ready as far as bd is concerned — out of a run whose
// size a human already agreed to.
func TestPlanNeverDispatchesOutsideTheScope(t *testing.T) {
	st := state()
	st.Scope = []string{"a", "c"}
	src := &fakeSource{ready: []bd.Issue{{ID: "a"}, {ID: "b"}, {ID: "discovered-1"}, {ID: "c"}}}

	res, err := Plan(src, st, opts(5))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range res.Issues {
		got = append(got, i.ID)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("planned %v, want only the scoped issues [a c]", got)
	}
}

// An empty scope is an unrestricted run, never an empty one. A run recorded
// before scope selection existed has no list to read, and reading that as
// "nothing is allowed" would stop the run dead.
func TestPlanTreatsAnEmptyScopeAsUnrestricted(t *testing.T) {
	src := &fakeSource{ready: []bd.Issue{{ID: "a"}, {ID: "b"}}}
	res, err := Plan(src, state(), opts(5))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 2 {
		t.Fatalf("planned %d issues, want both", len(res.Issues))
	}
}

// An interrupted run leaves its work in flight, which is exactly what Plan
// excludes. Resume is what stops that work from being silently dropped.
func TestResumeReturnsWhatAnInterruptLeftInFlight(t *testing.T) {
	st := state()
	st.InFlight["a"] = runstate.Attempt{Branch: "bd-auto/a", Attempt: 2}
	st.InFlight["b"] = runstate.Attempt{Branch: "bd-auto/b", Attempt: 1}
	st.InFlight["gone"] = runstate.Attempt{Branch: "bd-auto/gone", Attempt: 1}
	st.Done = append(st.Done, "gone")

	got := Resume(st, opts(5))
	if len(got) != 2 {
		t.Fatalf("resumed %+v, want a and b", got)
	}
	if got[0].ID != "a" || got[0].Attempt != 2 || got[0].Branch != "bd-auto/a" {
		t.Fatalf("the recorded attempt and branch must come back untouched: %+v", got[0])
	}

	// Scope outranks the record: an issue in flight before the scope narrowed is
	// still not this run's to touch.
	st.Scope = []string{"b"}
	if got := Resume(st, opts(5)); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("resume ignored the scope: %+v", got)
	}
}

func TestRecordMarksTheWaveInFlight(t *testing.T) {
	root := t.TempDir()
	if err := runstate.Save(root, state()); err != nil {
		t.Fatal(err)
	}

	st, err := Record(root, []Issue{{ID: "a", Branch: "bd-auto/a", Attempt: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if st.Wave != 1 || st.LastWaveChange != 1 {
		t.Fatalf("wave not advanced: %+v", st)
	}
	if len(st.WaveIssues) != 1 || st.WaveIssues[0] != "a" {
		t.Fatalf("wave issues not recorded: %v", st.WaveIssues)
	}
	if st.Attempts["a"] != 2 {
		t.Fatalf("attempt count not recorded: %v", st.Attempts)
	}
	if got := st.InFlight["a"]; got.Branch != "bd-auto/a" || got.Attempt != 2 {
		t.Fatalf("in-flight entry wrong: %+v", got)
	}

	// The write must survive a reload: this is what makes a run resumable.
	reloaded, err := runstate.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.InFlight["a"]; !ok {
		t.Fatal("in-flight entry did not persist")
	}
}

func TestJoinAddsToTheWaveAlreadyRunning(t *testing.T) {
	root := t.TempDir()
	if err := runstate.Save(root, state()); err != nil {
		t.Fatal(err)
	}
	if _, err := Record(root, []Issue{{ID: "a", Branch: "bd-auto/a", Attempt: 1}}); err != nil {
		t.Fatal(err)
	}

	st, err := Join(root, []Issue{{ID: "b", Branch: "bd-auto/b", Attempt: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if st.Wave != 1 {
		t.Fatalf("wave advanced to %d; a top-up joins the wave that is running", st.Wave)
	}
	if len(st.WaveIssues) != 2 || st.WaveIssues[0] != "a" || st.WaveIssues[1] != "b" {
		t.Fatalf("wave issues %v; the barrier merges this list, so it must hold both", st.WaveIssues)
	}
	if got := st.InFlight["b"]; got.Branch != "bd-auto/b" || got.Attempt != 2 {
		t.Fatalf("in-flight entry wrong: %+v", got)
	}
	if st.Attempts["b"] != 2 {
		t.Fatalf("attempt count not recorded: %v", st.Attempts)
	}

	// Joining twice must not double the row, or the barrier would try to merge
	// one branch twice.
	st, err = Join(root, []Issue{{ID: "b", Branch: "bd-auto/b", Attempt: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.WaveIssues) != 2 {
		t.Fatalf("wave issues %v after re-joining b", st.WaveIssues)
	}
}

func TestJoinableHoldsBackWhatDependsOnTheRunningWave(t *testing.T) {
	src := &fakeSource{deps: map[string][]bd.Ref{
		"free":    nil,
		"blocked": {{ID: "running", Type: "blocks"}},
	}}
	st := state()
	st.WaveIssues = []string{"running"}

	got := Joinable(src, st, []Issue{{ID: "free"}, {ID: "blocked"}, {ID: "unknown"}})
	if len(got) != 1 || got[0].ID != "free" {
		t.Fatalf("joinable %v; only an issue whose dependencies are already in HEAD may join a "+
			"wave whose branches have not been merged yet", IDs(got))
	}
}

func TestJoinableIsEmptyForAnEmptyPlan(t *testing.T) {
	src := &fakeSource{}
	if got := Joinable(src, state(), nil); len(got) != 0 {
		t.Fatalf("joinable %v, want none", IDs(got))
	}
	if len(src.shown) != 0 {
		t.Fatalf("bd was asked about %v; nothing was planned, so nothing needs checking", src.shown)
	}
}
