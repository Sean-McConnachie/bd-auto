package similar_test

import (
	"math"
	"sort"
	"testing"

	"bd-auto/internal/similar"
)

// backlog is the corpus every test scores against: this repository's real
// issues. Rarity weighting only means anything against a corpus the size of a
// real backlog, so the tests use one rather than a handful of samples.
func backlog(t *testing.T) *similar.Index {
	t.Helper()
	docs := make([]similar.Doc, 0, len(corpusFixture))
	for id, txt := range corpusFixture {
		docs = append(docs, similar.Doc{ID: id, Text: txt})
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	return similar.NewIndex(docs)
}

func score(t *testing.T, ix *similar.Index, a, b string) float64 {
	t.Helper()
	ta, ok := corpusFixture[a]
	if !ok {
		t.Fatalf("fixture %q is missing", a)
	}
	tb, ok := corpusFixture[b]
	if !ok {
		t.Fatalf("fixture %q is missing", b)
	}
	return ix.Score(ta, tb)
}

// The pair that cost a whole drain attempt: two workers three waves apart filed
// the same roleAliases removal, and one was worked before anybody noticed.
func TestTheDuplicateThatCostAnAttemptIsCaughtOutright(t *testing.T) {
	ix := backlog(t)
	got := score(t, ix, "6up", "pzi")
	if got < similar.DefaultThreshold {
		t.Fatalf("6up/pzi scored %.3f, below the %.2f at which a duplicate is not filed;\n"+
			"these are the same issue filed twice and the whole mechanism exists for them",
			got, similar.DefaultThreshold)
	}
}

// The pair title matching could never have caught. It does not clear
// DefaultThreshold and is not expected to — see the comment on the thresholds —
// but it must clear HintThreshold, because putting the two in front of a human
// is what actually resolved it.
func TestTheWorktreeRaceFiledTwiceIsAtLeastFlagged(t *testing.T) {
	ix := backlog(t)
	got := score(t, ix, "1p8", "b6m")
	if got < similar.HintThreshold {
		t.Fatalf("1p8/b6m scored %.3f, below the %.2f at which a human is shown the match;\n"+
			"these are one race inside .git/worktrees, filed 23 hours apart in words\n"+
			"that share almost nothing but the error string", got, similar.HintThreshold)
	}
	if got >= similar.DefaultThreshold {
		t.Fatalf("1p8/b6m scored %.3f, at or above the %.2f at which a discovery is dropped\n"+
			"without a human. The thresholds are documented as straddling this pair; if a\n"+
			"change genuinely made the lexical evidence this strong, re-run the whole-backlog\n"+
			"sweep in TestNothingUnrelatedOutranksTheRealDuplicates before moving the constant.",
			got, similar.DefaultThreshold)
	}
}

// Discrimination, not matching. The scorer earns nothing by calling everything
// a duplicate, so the test that matters is the one over pairs that are close
// but are different work.
func TestPairsThatAreAdjacentButNotDuplicatesStayBelowTheHint(t *testing.T) {
	ix := backlog(t)
	// Each of these shares a subsystem, a vocabulary or both with its partner,
	// and none is a second report of the other.
	pairs := [][2]string{
		{"1p8", "7tu"}, // both concurrency in tests; different races
		{"1xg", "xhw"}, // both the code index; build it vs refresh it
		{"84g", "txg"}, // both permission denials; report them vs act on them
		{"tpk", "1n4"}, // both smoke.sh; a drain case vs an isolated mode
		{"cx0", "r4h"}, // both the view; no events vs too many rows
		{"xdv", "joa"}, // both the beads export in git; different mechanisms
		{"so5", "o4q"}, // both discovered from wz9.7; unrelated faults
	}
	for _, p := range pairs {
		if got := score(t, ix, p[0], p[1]); got >= similar.HintThreshold {
			t.Errorf("%s/%s scored %.3f, at or above the %.2f hint: these are different work\n"+
				"and flagging them trains a human to ignore the flag",
				p[0], p[1], got, similar.HintThreshold)
		}
	}
}

// Two halves of one defect, which is what the hint level is for. vzz was
// discovered from gvg and they are the same misreading of run.json in two
// commands: gvg is `run status` calling a statusless file active, vzz is
// `worker status` calling any file on disk active. A human shown the pair
// would fix them together, and the scorer finds them without being told about
// the discovered-from edge.
func TestTwoHalvesOfOneDefectAreHinted(t *testing.T) {
	ix := backlog(t)
	for _, p := range [][2]string{{"gvg", "vzz"}, {"1p8", "b6m"}} {
		got := score(t, ix, p[0], p[1])
		if got < similar.HintThreshold {
			t.Errorf("%s/%s scored %.3f, below the %.2f hint", p[0], p[1], got, similar.HintThreshold)
		}
		if got >= similar.DefaultThreshold {
			t.Errorf("%s/%s scored %.3f, at or above the %.2f drop threshold: these are "+
				"related, not interchangeable, and dropping one loses work",
				p[0], p[1], got, similar.DefaultThreshold)
		}
	}
}

// The whole-backlog sweep. Nothing may outrank the one pair that is meant to be
// caught outright, which is the property a threshold above the band depends on.
func TestNothingUnrelatedOutranksTheRealDuplicates(t *testing.T) {
	ix := backlog(t)
	ids := make([]string, 0, len(corpusFixture))
	for id := range corpusFixture {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var over []string
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			s := ix.Score(corpusFixture[ids[i]], corpusFixture[ids[j]])
			if s >= similar.DefaultThreshold && !(ids[i] == "6up" && ids[j] == "pzi") {
				over = append(over, ids[i]+"/"+ids[j])
			}
		}
	}
	if len(over) > 0 {
		t.Fatalf("%d pair(s) besides 6up/pzi reached the drop threshold %.2f: %v\n"+
			"every one of these would be a discovery silently not filed",
			len(over), similar.DefaultThreshold, over)
	}
}

// Rarity is the whole idea: what two issues share has to be worth something.
func TestCommonWordsAreWorthAlmostNothing(t *testing.T) {
	ix := backlog(t)
	// Two texts built only from words this backlog uses everywhere.
	a := "The worker runs the issue and the run reports what the worker did."
	b := "A run reports the issue a worker did not finish, and the worker is done."
	if got := ix.Score(a, b); got >= similar.HintThreshold {
		t.Fatalf("two texts sharing only the tracker's own register scored %.3f; "+
			"common words must not carry identity", got)
	}
}

func TestOneRareTokenOutweighsAPileOfCommonOnes(t *testing.T) {
	ix := backlog(t)
	common := "the worker runs an issue in a run and reports the result to the wave"
	withRare := common + " commondir"
	otherRare := common + " roleAliases"
	shared := ix.Score(withRare, common+" commondir")
	split := ix.Score(withRare, otherRare)
	if shared <= split {
		t.Fatalf("sharing the rare token scored %.3f and differing on it scored %.3f; "+
			"the rare token must be what moves the score", shared, split)
	}
}

// A path is not the same as its parts, and the parts are what survive two
// people describing one bug: t-2 and <other-issue> differ, commondir does not.
func TestAPathContributesItsSegmentsAsWellAsItself(t *testing.T) {
	toks := similar.Tokens("fatal: failed to read .git/worktrees/t-2/commondir: Success")
	for _, want := range []string{"commondir", "worktrees", "git/worktrees/t-2/commondir"} {
		if !toks[want] {
			t.Errorf("tokens are missing %q; got %v", want, keys(toks))
		}
	}
}

func TestCamelCaseIsFindableByItsWords(t *testing.T) {
	toks := similar.Tokens("internal/config/runners.go keeps roleAliases mapping")
	for _, want := range []string{"rolealiases", "role", "aliases", "runners", "config"} {
		if !toks[want] {
			t.Errorf("tokens are missing %q; got %v", want, keys(toks))
		}
	}
}

func TestScoringIsSymmetricAndSelfIsOne(t *testing.T) {
	ix := backlog(t)
	a, b := corpusFixture["1p8"], corpusFixture["b6m"]
	if ab, ba := ix.Score(a, b), ix.Score(b, a); math.Abs(ab-ba) > 1e-9 {
		t.Fatalf("score is not symmetric: %.6f vs %.6f", ab, ba)
	}
	if got := ix.Score(a, a); got < 0.999 {
		t.Fatalf("a text scored %.3f against itself, want 1", got)
	}
}

func TestEmptyTextMatchesNothing(t *testing.T) {
	ix := backlog(t)
	if got := ix.Score("", corpusFixture["1p8"]); got != 0 {
		t.Fatalf("empty text scored %.3f", got)
	}
	if got := ix.Score("", ""); got != 0 {
		t.Fatalf("two empty texts scored %.3f; nothing in common is not everything in common", got)
	}
}

// A short text must not score by having little to disagree with. This is the
// evidence floor, and without it "Record usage and turns for both runs" was the
// backlog's second-best match for the ask_user tool.
func TestAShortTextCannotMatchOnBrevityAlone(t *testing.T) {
	ix := backlog(t)
	if got := score(t, ix, "dql.5", "buu"); got >= similar.HintThreshold {
		t.Fatalf("dql.5/buu scored %.3f: a one-line issue matched an unrelated feature "+
			"because it had almost nothing to say", got)
	}
}

func TestBestNamesTheClosestIssueAndIsStable(t *testing.T) {
	// The backlog without 6up, because a discovery being matched has not been
	// filed yet: Best is asked what an unfiled text most resembles.
	docs := make([]similar.Doc, 0, len(corpusFixture))
	for id, txt := range corpusFixture {
		if id == "6up" {
			continue
		}
		docs = append(docs, similar.Doc{ID: id, Text: txt})
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	ix := similar.NewIndex(docs)
	first := ix.Best(corpusFixture["6up"])
	if first.ID != "pzi" {
		t.Fatalf("the closest issue to 6up is %q at %.3f, want pzi", first.ID, first.Score)
	}
	if second := ix.Best(corpusFixture["6up"]); second != first {
		t.Fatalf("Best is not deterministic: %+v then %+v", first, second)
	}
}

func TestAnEmptyBacklogMatchesNothingRatherThanPanicking(t *testing.T) {
	ix := similar.NewIndex(nil)
	if got := ix.Best("anything at all"); got.ID != "" || got.Score != 0 {
		t.Fatalf("empty index returned %+v", got)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
