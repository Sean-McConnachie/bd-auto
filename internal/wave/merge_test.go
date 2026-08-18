package wave

import "testing"

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

func ids(in []Candidate) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = c.Issue
	}
	return out
}

func indexOf(in []Candidate, id string) int {
	for i, c := range in {
		if c.Issue == id {
			return i
		}
	}
	return -1
}

func TestOrderRespectsDependencies(t *testing.T) {
	in := []Candidate{
		{Issue: "c", DependsOn: []string{"b"}},
		{Issue: "b", DependsOn: []string{"a"}},
		{Issue: "a"},
	}
	got := Order(in)
	if len(got) != 3 {
		t.Fatalf("lost candidates: %v", ids(got))
	}
	if indexOf(got, "a") > indexOf(got, "b") || indexOf(got, "b") > indexOf(got, "c") {
		t.Fatalf("dependency order violated: %v", ids(got))
	}
}

func TestOrderIgnoresDepsOutsideTheWave(t *testing.T) {
	// Dependencies on already-merged issues from earlier waves must not stall
	// the ordering.
	in := []Candidate{
		{Issue: "b", DependsOn: []string{"merged-earlier"}},
		{Issue: "a", DependsOn: []string{"also-merged"}},
	}
	got := Order(in)
	if len(got) != 2 {
		t.Fatalf("want 2, got %v", ids(got))
	}
}

func TestOrderIsDeterministic(t *testing.T) {
	in := []Candidate{{Issue: "z"}, {Issue: "m"}, {Issue: "a"}}
	first := ids(Order(in))
	for i := 0; i < 20; i++ {
		if got := ids(Order(in)); !equal(got, first) {
			t.Fatalf("order not deterministic: %v then %v", first, got)
		}
	}
	if first[0] != "a" {
		t.Fatalf("independent candidates should sort by ID, got %v", first)
	}
}

// A dependency cycle is a bad DAG, but the integrator must still see every
// branch rather than silently dropping work.
func TestOrderKeepsEverythingOnACycle(t *testing.T) {
	in := []Candidate{
		{Issue: "a", DependsOn: []string{"b"}},
		{Issue: "b", DependsOn: []string{"a"}},
		{Issue: "c"},
	}
	got := Order(in)
	if len(got) != 3 {
		t.Fatalf("cycle must not drop candidates, got %v", ids(got))
	}
}

func TestOrderEmpty(t *testing.T) {
	if got := Order(nil); len(got) != 0 {
		t.Fatalf("want empty, got %v", ids(got))
	}
}

func equal(a, b []string) bool {
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
