// Package wave decides what a run does next: which issues make up the next
// wave, and in what order the resulting branches merge.
//
// Everything here is a function over explicit inputs — a bd source, the run
// state, a few resolved knobs — so both the CLI commands and the drain engine
// can call it directly. The package deliberately runs no processes and reads no
// flags; that stays with the caller.
package wave

import (
	"sort"
	"strings"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/runstate"
)

// Issue is one issue the engine should dispatch a worker for.
type Issue struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Type     string `json:"type"`
	Priority int    `json:"priority"`
	Branch   string `json:"branch"`
	Attempt  int    `json:"attempt"`
	// RetryContext carries why the previous attempt failed. Empty on a first
	// attempt. This is what makes a retry informed rather than a repeat.
	RetryContext string `json:"retry_context,omitempty"`
}

// Source is the slice of bd the planner needs. *bd.Client satisfies it.
type Source interface {
	Ready(parent string, limit int) ([]bd.Issue, error)
	Show(id string) (*bd.Issue, error)
}

// Options are the resolved run knobs the planner works from.
type Options struct {
	// Concurrency caps the size of one wave. A non-positive cap plans an empty
	// wave, so resolve it from run state before calling.
	Concurrency int
	// Branch names the worker branch for an issue. Defaults to the issue ID.
	Branch func(issueID string) string
}

// Result is one computed wave.
type Result struct {
	// Issues is the wave, in bd's priority order and capped by Concurrency.
	Issues []Issue
	// Drained reports that nothing is left to dispatch and nothing is still in
	// flight, which is the run's terminating condition.
	Drained bool
}

// Plan computes the next wave for a run.
//
// Readiness is not recomputed here. bd ready is already blocker-aware, so this
// asks bd and then subtracts what this run has already handled.
//
// The scope intersection is not an optimisation. A run's scope is the set of
// issues a human approved before anything was spawned, and it is a hard
// allowlist for the run's whole life: an issue outside it is never dispatched,
// whatever bd says about its readiness. That is what keeps discovered work out
// of a run by construction — a worker files it, bd may well report it ready, and
// it was not in the list the human agreed to.
func Plan(src Source, st *runstate.State, opt Options) (Result, error) {
	ready, err := src.Ready(st.Epic, 0)
	if err != nil {
		return Result{}, err
	}

	var issues []Issue
	for _, iss := range ready {
		if len(issues) >= opt.Concurrency {
			break
		}
		if !st.InScope(iss.ID) || st.Excluded(iss.ID) {
			continue
		}
		attempt := st.Attempts[iss.ID] + 1
		issues = append(issues, Issue{
			ID:           iss.ID,
			Title:        iss.Title,
			Type:         iss.IssueType,
			Priority:     iss.Priority,
			Branch:       opt.branch(iss.ID),
			Attempt:      attempt,
			RetryContext: retryContext(src, st, iss.ID, attempt),
		})
	}

	return Result{
		Issues:  issues,
		Drained: len(issues) == 0 && len(st.InFlight) == 0,
	}, nil
}

// Resume returns the issues an interrupted run left in flight, so a re-run
// picks them up rather than planning around them.
//
// It is not a planning decision and deliberately does not consult bd. An
// in-flight issue is one this run already dispatched, which is exactly why Plan
// excludes it: asking bd again would either offer it as new work — resetting the
// attempt it was on — or, more likely, not offer it at all, because a claimed
// in-progress issue is not in a ready front. Either way the work that was
// interrupted would be silently dropped.
//
// The recorded attempt number and branch come back untouched. An interrupt is
// not a verdict, so it consumes neither.
func Resume(st *runstate.State, opt Options) []Issue {
	var out []Issue
	for id, a := range st.InFlight {
		if !st.InScope(id) || st.IsDone(id) || st.IsParked(id) {
			continue
		}
		branch := a.Branch
		if branch == "" {
			branch = opt.branch(id)
		}
		attempt := a.Attempt
		if attempt < 1 {
			attempt = 1
		}
		out = append(out, Issue{ID: id, Branch: branch, Attempt: attempt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (o Options) branch(issueID string) string {
	if o.Branch == nil {
		return issueID
	}
	return o.Branch(issueID)
}

// Record marks a planned wave as in-flight and returns the updated state.
//
// Done under lock because workers write the same file concurrently.
func Record(repoRoot string, issues []Issue) (*runstate.State, error) {
	return runstate.Update(repoRoot, false, func(s *runstate.State) error {
		s.Wave++
		s.LastWaveChange = s.Wave
		s.WaveIssues = nil
		dispatch(s, issues)
		s.Note("wave %d dispatched: %s", s.Wave, strings.Join(s.WaveIssues, ", "))
		return nil
	})
}

// Join adds issues to the wave that is already running, rather than opening a
// new one.
//
// It is the other half of Record and exists because a wave grows: a worker that
// finishes frees a slot, and whatever goes into that slot belongs to the wave in
// flight, not to the next one. Record cannot be reused for it — advancing the
// counter and clearing WaveIssues would drop the running wave's own issues out
// of the barrier that is about to merge them.
func Join(repoRoot string, issues []Issue) (*runstate.State, error) {
	return runstate.Update(repoRoot, false, func(s *runstate.State) error {
		dispatch(s, issues)
		s.Note("wave %d topped up: %s", s.Wave, strings.Join(IDs(issues), ", "))
		return nil
	})
}

// dispatch marks issues in flight for the current wave. Record and Join differ
// only in whether they open that wave or add to it.
func dispatch(s *runstate.State, issues []Issue) {
	for _, w := range issues {
		if !inList(s.WaveIssues, w.ID) {
			s.WaveIssues = append(s.WaveIssues, w.ID)
		}
		s.Attempts[w.ID] = w.Attempt
		s.InFlight[w.ID] = runstate.Attempt{
			Branch:  w.Branch,
			Attempt: w.Attempt,
			Stage:   config.StageImplement,
		}
	}
}

// Joinable filters a plan down to what may join a wave that is already running.
//
// bd's ready front stays the authority on what may start, and this does not
// second-guess it: an issue is held back here for one reason, and it is about
// where its worker would have to start from. A worker branches from the main
// checkout's HEAD, and this wave's branches are not in it until the barrier
// merges them — so an issue depending on one of this wave's own issues would be
// implemented against a tree its dependency's work is missing from. bd cannot
// see that. It sees a closed issue and a dependent that is now ready, which is
// exactly right for the next wave and a wasted attempt in this one.
//
// Between waves nothing is filtered: Plan is what the barrier's merge feeds,
// and by then the dependency is in HEAD.
func Joinable(src Source, st *runstate.State, issues []Issue) []Issue {
	if len(issues) == 0 {
		return nil
	}
	unmerged := map[string]bool{}
	for _, id := range st.WaveIssues {
		unmerged[id] = true
	}
	var out []Issue
	for _, w := range issues {
		iss, err := src.Show(w.ID)
		if err != nil || iss == nil {
			// Unreadable is not joinable. A wave already running loses nothing
			// by leaving this one for the next wave to plan properly.
			continue
		}
		if dependsOn(iss.Dependencies, unmerged) {
			continue
		}
		out = append(out, w)
	}
	return out
}

func dependsOn(deps []bd.Ref, set map[string]bool) bool {
	for _, d := range deps {
		if set[d.ID] {
			return true
		}
	}
	return false
}

// IDs names a set of planned issues, in order.
func IDs(in []Issue) []string {
	out := make([]string, 0, len(in))
	for _, i := range in {
		out = append(out, i.ID)
	}
	return out
}

func inList(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

// NoteMarker prefixes every failure note bd-auto records on an issue. It is
// what a retry looks for when reconstructing why the last attempt failed.
const NoteMarker = "bd-auto attempt"

// retryContext says why the previous attempt failed, so the fresh worker starts
// informed.
//
// Run state is asked first, and the issue's notes are only a fallback. The note
// is the copy bd-auto does not control: beads' post-checkout hook imports
// .beads/issues.jsonl over its database, so the note written after an attempt's
// last commit is gone the moment the next attempt's worktree is created.
// Reading the answer back out of a store that demonstrably loses the write is
// what made a fresh retry start blind. The fallback stays for a run whose state
// predates this field, where a surviving note is better than nothing.
func retryContext(src Source, st *runstate.State, id string, attempt int) string {
	if attempt <= 1 {
		return ""
	}
	if f, ok := st.LastFailure(id); ok && f.Reason != "" {
		return NoteMarker + " " + f.Summary()
	}
	iss, err := src.Show(id)
	if err != nil || iss == nil {
		return ""
	}
	return RetryContext(iss.Notes)
}

// RetryContext extracts the last bd-auto failure note from an issue's notes.
// Empty when the notes hold none.
func RetryContext(notes string) string {
	idx := strings.LastIndex(notes, NoteMarker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(notes[idx:])
}
