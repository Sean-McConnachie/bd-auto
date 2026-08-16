package wave

import (
	"errors"
	"testing"

	"bd-auto/internal/bd"
	"bd-auto/internal/runstate"
)

// fakeSource stands in for bd. Ready returns a fixed front; Show serves notes.
type fakeSource struct {
	ready []bd.Issue
	notes map[string]string
	err   error
	shown []string
}

func (f *fakeSource) Ready(parent string, limit int) ([]bd.Issue, error) {
	return f.ready, f.err
}

func (f *fakeSource) Show(id string) (*bd.Issue, error) {
	f.shown = append(f.shown, id)
	n, ok := f.notes[id]
	if !ok {
		return nil, errors.New("no such issue")
	}
	return &bd.Issue{ID: id, Notes: n}, nil
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
