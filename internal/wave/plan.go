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
			RetryContext: retryContext(src, iss.ID, attempt),
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
		s.Continuations = 0
		s.WaveIssues = nil
		for _, w := range issues {
			s.WaveIssues = append(s.WaveIssues, w.ID)
			s.Attempts[w.ID] = w.Attempt
			s.InFlight[w.ID] = runstate.Attempt{
				Branch:  w.Branch,
				Attempt: w.Attempt,
				Stage:   config.StageImplement,
			}
		}
		s.Note("wave %d dispatched: %s", s.Wave, strings.Join(s.WaveIssues, ", "))
		return nil
	})
}

// NoteMarker prefixes every failure note bd-auto records on an issue. It is
// what a retry looks for when reconstructing why the last attempt failed.
const NoteMarker = "bd-auto attempt"

// retryContext pulls the failure notes recorded on a previous attempt so the
// fresh worker starts informed.
func retryContext(src Source, id string, attempt int) string {
	if attempt <= 1 {
		return ""
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
