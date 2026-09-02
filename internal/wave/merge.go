package wave

import (
	"sort"

	"bd-auto/internal/bd"
	"bd-auto/internal/runstate"
)

// Candidate is one worker branch waiting to be merged.
type Candidate struct {
	Issue   string `json:"issue"`
	Branch  string `json:"branch"`
	Exists  bool   `json:"exists"`
	Commits int    `json:"commits"`
	// Landed means the branch tip is already an ancestor of the merge target.
	// It is retained as a closure-only candidate after an interrupted barrier.
	Landed    bool     `json:"landed,omitempty"`
	Worktree  string   `json:"worktree,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	Status    string   `json:"issue_status"`
	// Detail is the complete issue record captured before integration changes
	// Git state. It supplies a conflict editor without a mid-merge Beads read.
	Detail *bd.Issue `json:"-"`
}

// CandidateIDs returns the issues whose branches the integrator should consider.
// By default that is the current wave; all widens it to every issue the run has
// touched, deduplicated and in the order they were seen.
func CandidateIDs(st *runstate.State, all bool) []string {
	if !all {
		return st.WaveIssues
	}
	seen := map[string]bool{}
	var ids []string
	for _, l := range [][]string{st.WaveIssues, st.Done} {
		for _, id := range l {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// Order sorts candidates so a branch never merges before one it depends on.
// Ties break on issue ID for determinism.
//
// Within a wave the issues are a ready front and therefore mutually
// independent, so order usually does not matter. It does after retries, when a
// requeued issue can land in a later wave than something that depends on it,
// so the ordering is computed rather than assumed.
func Order(in []Candidate) []Candidate {
	inSet := map[string]bool{}
	for _, c := range in {
		inSet[c.Issue] = true
	}
	byID := map[string]Candidate{}
	for _, c := range in {
		byID[c.Issue] = c
	}

	// Kahn's algorithm over the subgraph induced by this wave.
	indeg := map[string]int{}
	children := map[string][]string{}
	for _, c := range in {
		indeg[c.Issue] += 0
		for _, d := range c.DependsOn {
			if inSet[d] {
				indeg[c.Issue]++
				children[d] = append(children[d], c.Issue)
			}
		}
	}

	var ready []string
	for id, n := range indeg {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	var out []Candidate
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, byID[id])
		var freed []string
		for _, ch := range children[id] {
			indeg[ch]--
			if indeg[ch] == 0 {
				freed = append(freed, ch)
			}
		}
		sort.Strings(freed)
		ready = append(ready, freed...)
		sort.Strings(ready)
	}

	// A cycle would drop issues; append any stragglers so nothing is lost.
	if len(out) < len(in) {
		seen := map[string]bool{}
		for _, o := range out {
			seen[o.Issue] = true
		}
		for _, c := range in {
			if !seen[c.Issue] {
				out = append(out, c)
			}
		}
	}
	return out
}

// Mergeable keeps the candidates that actually have something to merge: a
// branch that exists and is ahead of the base.
func Mergeable(in []Candidate) []Candidate {
	var out []Candidate
	for _, m := range in {
		if m.Exists && (m.Commits > 0 || m.Landed) {
			out = append(out, m)
		}
	}
	return out
}
