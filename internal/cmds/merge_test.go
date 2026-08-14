package cmds

import "testing"

func ids(in []MergeCandidate) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = c.Issue
	}
	return out
}

func indexOf(in []MergeCandidate, id string) int {
	for i, c := range in {
		if c.Issue == id {
			return i
		}
	}
	return -1
}

func TestTopoOrderRespectsDependencies(t *testing.T) {
	in := []MergeCandidate{
		{Issue: "c", DependsOn: []string{"b"}},
		{Issue: "b", DependsOn: []string{"a"}},
		{Issue: "a"},
	}
	got := topoOrder(in)
	if len(got) != 3 {
		t.Fatalf("lost candidates: %v", ids(got))
	}
	if indexOf(got, "a") > indexOf(got, "b") || indexOf(got, "b") > indexOf(got, "c") {
		t.Fatalf("dependency order violated: %v", ids(got))
	}
}

func TestTopoOrderIgnoresDepsOutsideTheWave(t *testing.T) {
	// Dependencies on already-merged issues from earlier waves must not stall
	// the ordering.
	in := []MergeCandidate{
		{Issue: "b", DependsOn: []string{"merged-earlier"}},
		{Issue: "a", DependsOn: []string{"also-merged"}},
	}
	got := topoOrder(in)
	if len(got) != 2 {
		t.Fatalf("want 2, got %v", ids(got))
	}
}

func TestTopoOrderIsDeterministic(t *testing.T) {
	in := []MergeCandidate{{Issue: "z"}, {Issue: "m"}, {Issue: "a"}}
	first := ids(topoOrder(in))
	for i := 0; i < 20; i++ {
		if got := ids(topoOrder(in)); !equal(got, first) {
			t.Fatalf("order not deterministic: %v then %v", first, got)
		}
	}
	if first[0] != "a" {
		t.Fatalf("independent candidates should sort by ID, got %v", first)
	}
}

// A dependency cycle is a bad DAG, but the integrator must still see every
// branch rather than silently dropping work.
func TestTopoOrderKeepsEverythingOnACycle(t *testing.T) {
	in := []MergeCandidate{
		{Issue: "a", DependsOn: []string{"b"}},
		{Issue: "b", DependsOn: []string{"a"}},
		{Issue: "c"},
	}
	got := topoOrder(in)
	if len(got) != 3 {
		t.Fatalf("cycle must not drop candidates, got %v", ids(got))
	}
}

func TestTopoOrderEmpty(t *testing.T) {
	if got := topoOrder(nil); len(got) != 0 {
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
