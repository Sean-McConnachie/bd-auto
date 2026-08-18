package scope

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

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

// now is the clock every test reads. Deferral is a date comparison, so a test
// that used the real one would start passing or failing on the calendar.
var now = time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)

// later is a defer date this clock has not reached.
var later = now.AddDate(3, 0, 0)

// defer marks an already-added issue as deferred until t.
func (f *fakeBD) deferred(id string, t time.Time) *fakeBD {
	f.issues[id].DeferUntil = t
	return f
}

// The candidate set is what a human is asked to approve, so what it leaves out
// matters as much as what it contains.
func TestCandidatesAreTheOpenUnparkedChildren(t *testing.T) {
	f := repo().
		add("e", "a", "open", 1).
		add("e", "b", "closed", 0).
		add("e", "c", "blocked", 0).
		add("e", "d", "in_progress", 0)

	set, err := Candidates(f, "e", now)
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

	set, err := Candidates(f, "e", now)
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
	set, err := Candidates(f, "e", now)
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
	set, err := Candidates(f, "e", now)
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

	got, err := Blocked(f, []string{"b", "c"}, now)
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
	if got, err := Blocked(f, []string{"a", "b"}, now); err != nil || len(got) != 0 {
		t.Fatalf("with a in scope, b is not blocked: %+v (%v)", got, err)
	}
	// An empty scope is an unrestricted run, never an empty one.
	if got, err := Blocked(f, nil, now); err != nil || got != nil {
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

// bd counts a deferred issue as open and ready — measured here on 2026-08-18,
// bd stats said "Ready to Work: 19" over 19 open issues of which 16 were
// deferred to 2029 and bd ready offered 2 — so the candidate set is where the
// distinction has to be made. Offer one and a human scopes a run to work bd
// will never put in a ready front.
func TestCandidatesLeaveOutDeferredChildren(t *testing.T) {
	f := repo().
		add("e", "a", "open", 0).
		add("e", "b", "open", 0).
		deferred("b", later).
		add("e", "c", "open", 0).
		deferred("c", now.AddDate(-1, 0, 0))

	set, err := Candidates(f, "e", now)
	if err != nil {
		t.Fatal(err)
	}
	// c's defer date has passed, so bd offers it again and so does this.
	if got := set.IDs(); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatalf("candidates %v, want the two bd would offer [a c]", got)
	}
	// Accounted for rather than silently dropped: the preview has to add up to
	// the epic's whole child list or a human cannot tell what they are missing.
	if why := set.Skipped["b"]; !contains(why, "deferred") || !contains(why, "2029-08-18") {
		t.Fatalf("skipped reason %q must say deferred and until when", why)
	}
	// And Resolve repeats that reason to anyone who names it anyway.
	err = mustErr(t, func() error { _, e := Resolve(set, []string{"b"}); return e })
	if !contains(err.Error(), "deferred") {
		t.Fatalf("error %q does not say b is deferred", err)
	}
}

// A blocker inside the scope but deferred can never become ready either, and it
// is the case parkStranded cannot explain: it ends the run saying only "never
// became ready, and the run drained without bd ever offering it".
func TestBlockedFindsDeferredInScopeDependencies(t *testing.T) {
	f := repo().
		add("e", "a", "open", 0).
		deferred("a", later).
		add("e", "b", "open", 0, blocks("a", "open"))
	// bd show embeds the whole depended-on issue in each dependency entry, so
	// the ref carries a's defer date the same way it carries a's status.
	f.issues["b"].Dependencies[0].DeferUntil = later

	got, err := Blocked(f, []string{"a", "b"}, now)
	if err != nil {
		t.Fatal(err)
	}
	// a is reported too, for its own deferral; this test is about b, which is
	// stopped by a's deferral rather than by its own.
	b := blockerFor(t, got, "b")
	if b.Dep != "a" {
		t.Fatalf("blocker %+v, want b waiting on the deferred a", b)
	}
	if !contains(b.Reason, "deferred") || contains(b.Reason, "out of scope") {
		t.Fatalf("reason %q must blame the deferral, not the scope", b.Reason)
	}
	// Widening the scope would not help, so the fix must not say to.
	if !contains(b.Fix, "--defer=") || contains(b.Fix, "Widen") {
		t.Fatalf("fix %q must tell a human to undefer", b.Fix)
	}

	// Past its date it is an ordinary in-scope blocker again, and a later wave
	// is where it belongs.
	if got, err := Blocked(f, []string{"a", "b"}, later.AddDate(1, 0, 0)); err != nil || len(got) != 0 {
		t.Fatalf("once the defer date passes b is not blocked: %+v (%v)", got, err)
	}
}

// A deferred issue does not need a dependency to be unrunnable: bd keeps it out
// of every ready front on its own account, so a scope that names one spends a
// whole run never being offered it and falls to the end-of-run sweep. The
// candidate set filters deferred children out of an epic, but a scope reaches
// the engine by other routes — named issue by issue, or reloaded from a run
// resumed after somebody deferred one of its issues — and this is the check
// that holds on all of them.
func TestBlockedFindsAnIssueDeferredInItsOwnRight(t *testing.T) {
	f := repo().
		add("e", "a", "open", 0).
		deferred("a", later).
		add("e", "b", "open", 0, blocks("done", "closed"))

	got, err := Blocked(f, []string{"a", "b"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("blockers %+v, want only the deferred a; b has nothing unmet", got)
	}
	a := blockerFor(t, got, "a")
	// Its own deferral, so there is no other issue to send a human to.
	if a.Dep != "" {
		t.Fatalf("blocker %+v names dependency %q; a is stopped by itself", a, a.Dep)
	}
	if !contains(a.Reason, "deferred until 2029-08-18") || contains(a.Reason, "dependency") {
		t.Fatalf("reason %q must name a's own deferral and its date", a.Reason)
	}
	if !contains(a.Fix, "bd update a --defer=") {
		t.Fatalf("fix %q must tell a human how to undefer a", a.Fix)
	}

	// Past its date bd offers it again, and so does this: nothing is blocked.
	if got, err := Blocked(f, []string{"a", "b"}, later.AddDate(1, 0, 0)); err != nil || len(got) != 0 {
		t.Fatalf("once the defer date passes a is runnable: %+v (%v)", got, err)
	}
	// A deferred issue that is already closed or parked is not the run's
	// problem, and reporting it would park something a human already dealt with.
	f.issues["a"].Status = "closed"
	if got, err := Blocked(f, []string{"a"}, now); err != nil || len(got) != 0 {
		t.Fatalf("a closed issue is not blocked, deferred or not: %+v (%v)", got, err)
	}
}

// blockerFor returns the one blocker reported against an issue.
func blockerFor(t *testing.T, got []Blocker, issue string) Blocker {
	t.Helper()
	var found []Blocker
	for _, b := range got {
		if b.Issue == issue {
			found = append(found, b)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d blocker(s) for %s in %+v, want exactly 1", len(found), issue, got)
	}
	return found[0]
}
