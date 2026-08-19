// Package similar decides whether two pieces of issue prose describe the same
// underlying problem.
//
// # Why title matching was not enough
//
// runstate.Discovery keyed on the normalised title, so two reports of one bug
// deduplicated only when two models happened to phrase the title the same way.
// This repo has the counterexample in its own backlog. beads-auto-imp-b6m and
// beads-auto-imp-1p8 are one race inside .git/worktrees, filed 23 hours apart:
//
//	"Flaky: concurrent git worktree add fails on another worktree's commondir"
//	"Concurrent git worktree add races, and a wave of five can lose an issue
//	 to it before any model runs"
//
// Title matching has nothing to work with there. The bodies do: both quote the
// same error, "failed to read .git/worktrees/<name>/commondir", and both name
// internal/worktree.
//
// # What carries identity
//
// The rare tokens two issues share, not the common ones. Every issue in this
// tracker says "issue" and most say "worker"; two sharing those say nothing
// about each other. Two sharing "commondir" are about the same thing.
//
// So each token is weighted by how rare it is across the backlog, and the score
// is the weight two texts share over the weight of the lighter one. Dividing by
// the lighter side rather than by the union is deliberate: a three-line
// discovery genuinely can be a duplicate of a thirty-line issue, and scoring
// their overlap against the union would bury it under prose the discovery had
// no chance to contain.
//
// It is lexical and offline on purpose. This runs at a wave barrier, where a
// model call is one more thing that fails at the worst moment, and the pair it
// has to separate is separable on the words alone.
package similar

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// stop are function words carrying no identity. Rarity weighting already
// discounts them in a real backlog, but a corpus of two documents has no rarity
// to measure, and these are noise at every corpus size.
var stop = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "but": true, "by": true, "can": true, "do": true,
	"does": true, "for": true, "from": true, "had": true, "has": true,
	"have": true, "how": true, "in": true, "into": true, "is": true, "it": true,
	"its": true, "no": true, "not": true, "of": true, "on": true, "one": true,
	"or": true, "so": true, "than": true, "that": true, "the": true,
	"their": true, "them": true, "then": true, "there": true, "they": true,
	"this": true, "to": true, "was": true, "were": true, "what": true,
	"when": true, "which": true, "while": true, "will": true, "with": true,
	"would": true, "you": true,
}

// Tokens reduces text to the set of terms that might carry identity.
//
// A run of identifier characters yields the whole run and also its parts, so
// ".git/worktrees/t-2/commondir" contributes "commondir" and "worktrees" as
// well as the full path. That matters because the full paths in two reports of
// one bug rarely match — one says t-2 and the other says <other-issue> — while
// the last segment is the name of the thing that broke.
//
// camelCase is split for the same reason: "roleAliases" is findable as
// "aliases" by somebody who wrote "role aliases".
func Tokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, run := range fields(text) {
		add(out, run)
		for _, part := range strings.FieldsFunc(run, func(r rune) bool {
			return r == '.' || r == '/' || r == '_' || r == '-'
		}) {
			add(out, part)
			for _, word := range splitCamel(part) {
				add(out, word)
			}
		}
	}
	return out
}

// fields splits on everything that cannot appear inside an identifier or a
// path. Case is kept here and dropped in add, because splitCamel needs the case
// and everything downstream needs it gone.
func fields(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case '.', '/', '_', '-':
			return false
		}
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// add keeps a token if it could distinguish anything. Single and two-character
// tokens cannot, nor can short bare numbers, and a leading or trailing
// separator is punctuation that survived the split.
func add(set map[string]bool, tok string) {
	tok = strings.ToLower(strings.Trim(tok, "./_-"))
	if len(tok) < 3 || stop[tok] {
		return
	}
	if allDigits(tok) && len(tok) < 4 {
		return
	}
	set[tok] = true
}

func allDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// splitCamel turns "roleAliases" into role and aliases, and leaves a word that
// is not camelCase alone rather than returning it twice.
func splitCamel(s string) []string {
	var parts []string
	start := 0
	runes := []rune(s)
	for i := 1; i < len(runes); i++ {
		if unicode.IsUpper(runes[i]) && !unicode.IsUpper(runes[i-1]) {
			parts = append(parts, strings.ToLower(string(runes[start:i])))
			start = i
		}
	}
	if start == 0 {
		return nil
	}
	return append(parts, strings.ToLower(string(runes[start:])))
}

// Doc is one thing already in the backlog.
type Doc struct {
	ID   string
	Text string
}

// Index holds the backlog and what each term in it is worth.
type Index struct {
	docs    []indexed
	weights map[string]float64
	n       int
	// floor is the least distinctive weight a document may be scored against.
	// See NewIndex.
	floor float64
}

type indexed struct {
	id     string
	tokens map[string]bool
	// mass is precomputed, both to save recomputing it once per candidate and
	// because summing it once is one fewer place to get the order wrong. See
	// sumWeights.
	mass float64
}

// NewIndex weighs every term by how much of the backlog does not contain it.
//
// The weight is log((N+1)/(df+0.5)). A term in every document lands at almost
// exactly zero, which is what "worker" and "issue" are worth here. A term in
// one document lands near log(2N), and a term in none — a term only the
// candidate has — lands highest of all, because a word nothing else in the
// backlog uses is the strongest evidence there is.
func NewIndex(docs []Doc) *Index {
	ix := &Index{weights: map[string]float64{}, n: len(docs)}
	df := map[string]int{}
	for _, d := range docs {
		toks := Tokens(d.Text)
		if len(toks) == 0 {
			continue
		}
		ix.docs = append(ix.docs, indexed{id: d.ID, tokens: toks})
		for t := range toks {
			df[t]++
		}
	}
	n := float64(len(ix.docs))
	for t, c := range df {
		ix.weights[t] = math.Log((n + 1) / (float64(c) + 0.5))
	}
	// Sorted once here rather than per query, so Best walks candidates in a
	// fixed order and a tie always resolves to the same issue.
	sort.Slice(ix.docs, func(i, j int) bool { return ix.docs[i].id < ix.docs[j].id })
	for i := range ix.docs {
		ix.docs[i].mass = ix.sumWeights(ix.docs[i].tokens)
	}
	ix.floor = ix.evidenceFloor()
	return ix
}

// evidenceFloor is the tenth percentile of how much distinctive weight the
// documents in this backlog carry.
//
// It exists because dividing by the lighter of two texts rewards a text for
// being short. A one-line issue has so little to say that almost any overlap is
// most of it, and without a floor those score highest of all: measured on this
// repo's backlog, "Record usage and turns for both runs" scored 0.55 against
// the ask_user tool and 0.46 against the runner adapters, on shared weights of
// 18 and 15 — while the two genuine duplicates shared 74 and 78.
//
// So the denominator never falls below what a real issue in this tracker is
// worth. A short text can still match, but only by presenting as much shared
// evidence as a whole issue would, rather than by having little to disagree
// with. The tenth percentile rather than the median because the floor is meant
// to exclude the runts, not to penalise everything below average.
func (ix *Index) evidenceFloor() float64 {
	if len(ix.docs) == 0 {
		return 0
	}
	masses := make([]float64, 0, len(ix.docs))
	for _, d := range ix.docs {
		masses = append(masses, d.mass)
	}
	sort.Float64s(masses)
	return masses[len(masses)/10]
}

// weight of a term the corpus has never seen: maximal, by the same formula
// with a document frequency of zero.
func (ix *Index) weight(t string) float64 {
	if w, ok := ix.weights[t]; ok {
		return w
	}
	n := float64(len(ix.docs))
	return math.Log((n + 1) / 0.5)
}

// sumWeights adds the weight of every term in a set, in sorted order.
//
// The order matters. Floating-point addition is not associative, and Go
// randomises map iteration, so summing straight out of the map gave a score
// that differed in its last bit between two runs over the same backlog — enough
// to swap two near-tied candidates and report a different duplicate the second
// time. A caller comparing against a threshold would see it as a flake.
func (ix *Index) sumWeights(toks map[string]bool) float64 {
	terms := make([]string, 0, len(toks))
	for t := range toks {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	var sum float64
	for _, t := range terms {
		sum += ix.weight(t)
	}
	return sum
}

func (ix *Index) mass(toks map[string]bool) float64 { return ix.sumWeights(toks) }

// Score is the weight two texts share over the weight of the lighter one, in
// [0,1]. Two empty texts share nothing and score zero rather than one.
//
// Dividing by the lighter side rather than by the union is deliberate: a
// three-line discovery genuinely can be a duplicate of a thirty-line issue, and
// scoring their overlap against the union would bury it under prose the
// discovery had no chance to contain.
func (ix *Index) Score(a, b string) float64 {
	return ix.score(Tokens(a), Tokens(b))
}

func (ix *Index) score(ta, tb map[string]bool) float64 {
	return ix.scoreWithMass(ta, ix.mass(ta), tb, ix.mass(tb))
}

func (ix *Index) scoreWithMass(ta map[string]bool, ma float64, tb map[string]bool, mb float64) float64 {
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	lighter := math.Max(math.Min(ma, mb), ix.floor)
	if lighter <= 0 {
		return 0
	}
	return ix.sumWeights(intersect(ta, tb)) / lighter
}

// Match is the backlog document this text most looks like, and how much.
//
// Ties break on the document ID so a run twice over the same backlog names the
// same duplicate twice. A caller comparing against a threshold gets the best
// candidate either way and can report the score it rejected.
type Match struct {
	ID    string
	Score float64
}

// Best returns the closest document in the backlog, or an empty Match when the
// backlog is empty. The caller applies the threshold: Best always reports what
// it found, so a near miss can be logged rather than vanishing.
func (ix *Index) Best(text string) Match {
	toks := Tokens(text)
	mass := ix.mass(toks)
	var best Match
	for _, d := range ix.docs {
		s := ix.scoreWithMass(toks, mass, d.tokens, d.mass)
		if s > best.Score {
			best = Match{ID: d.id, Score: s}
		}
	}
	return best
}

// The two thresholds, and why there are two.
//
// Both were set by measuring, not by taste. similar_test.go scores this repo's
// whole 93-issue backlog — every pair, 4371 of them — and the numbers say
// something worth being plain about: lexical evidence separates one of the two
// real duplicate pairs cleanly and the other one only partly.
//
//	0.632  6up <-> pzi     the same "drop the plugin-era role aliases", twice
//	0.318  1p8 <-> b6m     the same .git/worktrees race, twice
//
// 6up/pzi stands alone at the top. 1p8/b6m sits in a band around 0.30 shared
// with pairs that are related but are not duplicates at all — an epic and its
// own child (j5a/j5a.2, dql/dql.1), an issue and the issue it was discovered
// from (vz7/wz9.2). No threshold separates 1p8/b6m from those, because on the
// words alone they are the same kind of thing: two documents about one area of
// the code.
//
// Tuning until the fixture passed would have bought a number that fits this
// backlog and drops real work in the next one. So the band is reported instead
// of resolved:
//
// DefaultThreshold is where two texts are treated as the same problem and the
// second one is not filed. It is set above the band, so a discovery is only
// discarded on evidence that stands alone.
const DefaultThreshold = 0.45

// HintThreshold is where a match is worth a human's glance. Between it and
// DefaultThreshold a discovery is still filed or still staged, with the issue
// it resembles recorded against it — which is all that was ever needed for
// b6m: a human reading the two side by side closed one in seconds.
const HintThreshold = 0.28

// intersect is the set of terms two texts share.
func intersect(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a))
	for t := range a {
		if b[t] {
			out[t] = true
		}
	}
	return out
}
