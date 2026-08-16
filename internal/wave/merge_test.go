package wave

import "testing"

// Order itself is covered by the ordering tests in internal/cmds, which call it
// through the topoOrder wrapper. These cover the rest of the merge helpers.

func TestCandidateIDsDefaultsToTheCurrentWave(t *testing.T) {
	st := state()
	st.WaveIssues = []string{"c", "a"}
	st.Done = []string{"z"}
	got := CandidateIDs(st, false)
	if len(got) != 2 || got[0] != "c" || got[1] != "a" {
		t.Fatalf("want the wave in order, got %v", got)
	}
}

func TestCandidateIDsAllDeduplicates(t *testing.T) {
	st := state()
	st.WaveIssues = []string{"b", "a"}
	st.Done = []string{"a", "z"}
	got := CandidateIDs(st, true)
	want := []string{"b", "a", "z"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

func TestMergeableKeepsOnlyBranchesWithWork(t *testing.T) {
	in := []Candidate{
		{Issue: "a", Exists: true, Commits: 2},
		{Issue: "b", Exists: true, Commits: 0},
		{Issue: "c", Exists: false, Commits: 3},
	}
	got := Mergeable(in)
	if len(got) != 1 || got[0].Issue != "a" {
		t.Fatalf("want only a, got %+v", got)
	}
}

func TestMergeablePreservesOrder(t *testing.T) {
	in := Order([]Candidate{
		{Issue: "b", Exists: true, Commits: 1, DependsOn: []string{"a"}},
		{Issue: "a", Exists: true, Commits: 1},
	})
	got := Mergeable(in)
	if len(got) != 2 || got[0].Issue != "a" || got[1].Issue != "b" {
		t.Fatalf("dependency order lost: %+v", got)
	}
}
