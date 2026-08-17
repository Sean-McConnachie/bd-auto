package scope

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"bd-auto/internal/bd"
)

// fakeBD is an epic and its children, with dependency edges.
type fakeBD struct {
	issues map[string]*bd.Issue
	kids   map[string][]string
	err    error
}

func repo() *fakeBD {
	return &fakeBD{issues: map[string]*bd.Issue{}, kids: map[string][]string{}}
}

func (f *fakeBD) add(epic, id, status string, priority int, deps ...bd.Ref) *fakeBD {
	f.issues[id] = &bd.Issue{ID: id, Title: "issue " + id, Status: status, Priority: priority, Dependencies: deps}
	f.kids[epic] = append(f.kids[epic], id)
	return f
}

func (f *fakeBD) Children(parent string) ([]bd.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []bd.Issue
	for _, id := range f.kids[parent] {
		out = append(out, *f.issues[id])
	}
	return out, nil
}

func (f *fakeBD) Show(id string) (*bd.Issue, error) {
	iss, ok := f.issues[id]
	if !ok {
		return nil, errors.New("no such issue " + id)
	}
	return iss, nil
}

func blocks(id, status string) bd.Ref { return bd.Ref{ID: id, Status: status, Type: DepBlocks} }

// The candidate set is what a human is asked to approve, so what it leaves out
// matters as much as what it contains.
func TestCandidatesAreTheOpenUnparkedChildren(t *testing.T) {
	f := repo().
		add("e", "a", "open", 1).
		add("e", "b", "closed", 0).
		add("e", "c", "blocked", 0).
		add("e", "d", "in_progress", 0)

	set, err := Candidates(f, "e")
	if err != nil {
		t.Fatal(err)
	}
	if got := set.IDs(); !reflect.DeepEqual(got, []string{"d", "a"}) {
		t.Fatalf("candidates %v, want the open children in priority order [d a]", got)
	}
	if set.Skipped["b"] == "" || set.Skipped["c"] == "" {
		t.Fatalf("a closed and a parked child must be accounted for, got %v", set.Skipped)
	}
}

// A dependency that is already closed cannot hold anything up, so carrying it
// would make the preview claim a wave it does not need.
func TestCandidatesCarryOnlyUnmetBlockingDependencies(t *testing.T) {
	f := repo().
		add("e", "a", "open", 0).
		add("e", "b", "open", 0,
			blocks("a", "open"),
			blocks("done", "closed"),
			bd.Ref{ID: "cousin", Status: "open", Type: "relates-to"})

	set, err := Candidates(f, "e")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := set.Get("b")
	if !reflect.DeepEqual(b.DependsOn, []string{"a"}) {
		t.Fatalf("depends_on %v, want only the unmet blocking edge [a]", b.DependsOn)
	}
}

func TestResolveRejectsWhatIsNotACandidate(t *testing.T) {
	f := repo().add("e", "a", "open", 0).add("e", "b", "closed", 0)
	set, err := Candidates(f, "e")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve(set, []string{"a"}); err != nil {
		t.Fatalf("a is a candidate: %v", err)
	}
	err = mustErr(t, func() error { _, e := Resolve(set, []string{"a", "b", "zz"}); return e })
	for _, want := range []string{"b", "closed", "zz"} {
		if !contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// The wave decomposition is the shape of the spend: how many rounds of the gate
// and the barrier a selection implies, not just how many issues it holds.
func TestWavesDecomposeTheSelection(t *testing.T) {
	f := repo().
		add("e", "a", "open", 0).
		add("e", "b", "open", 0).
		add("e", "c", "open", 0, blocks("a", "open")).
		add("e", "d", "open", 0, blocks("c", "open"))
	set, err := Candidates(f, "e")
	if err != nil {
		t.Fatal(err)
	}

	got := Waves(set, []string{"a", "b", "c", "d"}, 4)
	want := [][]string{{"a", "b"}, {"c"}, {"d"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("waves %v, want %v", got, want)
	}

	// Concurrency splits a wave that the DAG says is one.
	if got := Waves(set, []string{"a", "b"}, 1); !reflect.DeepEqual(got, [][]string{{"a"}, {"b"}}) {
		t.Fatalf("waves at concurrency 1: %v", got)
	}

	// A blocker left out of the selection is not a later wave. It is the run's
	// problem, and Blocked is what reports it.
	if got := Waves(set, []string{"c"}, 4); !reflect.DeepEqual(got, [][]string{{"c"}}) {
		t.Fatalf("an out-of-scope blocker must not create a wave: %v", got)
	}
}

// The whole reason this check exists: bd would keep such an issue out of every
// ready front, and the run would end with nothing recorded against it at all.
func TestBlockedFindsOutOfScopeDependencies(t *testing.T) {
	f := repo().
		add("e", "a", "open", 0).
		add("e", "b", "open", 0, blocks("a", "open")).
		add("e", "c", "open", 0, blocks("a", "closed"))

	got, err := Blocked(f, []string{"b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Issue != "b" || got[0].Dep != "a" {
		t.Fatalf("blockers %+v, want only b waiting on a", got)
	}
	if !contains(got[0].Reason, "out of scope") {
		t.Fatalf("reason %q does not say why", got[0].Reason)
	}

	// The same issue with its blocker in scope is not blocked at all.
	if got, err := Blocked(f, []string{"a", "b"}); err != nil || len(got) != 0 {
		t.Fatalf("with a in scope, b is not blocked: %+v (%v)", got, err)
	}
	// An empty scope is an unrestricted run, never an empty one.
	if got, err := Blocked(f, nil); err != nil || got != nil {
		t.Fatalf("an unrestricted run has nothing out of scope: %+v (%v)", got, err)
	}
}

func mustErr(t *testing.T, fn func() error) error {
	t.Helper()
	err := fn()
	if err == nil {
		t.Fatal("expected an error")
	}
	return err
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
