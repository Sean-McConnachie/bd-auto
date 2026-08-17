// Package scope decides what a run is allowed to touch, and it is the only
// bound bd-auto has.
//
// There is no budget, no per-request timeout and no circuit breaker anywhere in
// the engine, so the limit on a run's spend is applied once, up front, by a
// human who can see what they are agreeing to. `bd-auto drain` does not drain an
// epic; it drains a set of issues somebody named. This package computes the
// candidate set, shows the shape of the work it implies, and reports the one
// thing a scoped run has to explain for itself: an issue inside the scope whose
// dependency is outside it.
//
// Everything here is a pure function over a bd source. Reading a terminal,
// writing run state and spawning anything all belong to the caller.
package scope

import (
	"fmt"
	"sort"
	"strings"

	"bd-auto/internal/bd"
)

// Source is the slice of bd scope selection needs. *bd.Client satisfies it.
type Source interface {
	// Children returns every issue under a parent, closed ones included.
	Children(parent string) ([]bd.Issue, error)
	// Show returns one issue with its dependency lists filled in.
	Show(id string) (*bd.Issue, error)
}

// DepBlocks is bd's dependency type for a real blocker. The others it records —
// parent-child, relates-to, discovered-from — describe how issues are related,
// not what has to happen first, and treating them as blockers would park work
// that was never blocked.
const DepBlocks = "blocks"

// blocking reports whether a dependency edge is one that has to be satisfied
// before its issue can be worked. An empty type is bd's default, which is
// blocks.
func blocking(depType string) bool {
	return depType == "" || depType == DepBlocks
}

// Issue is one issue a run could be scoped to, with what the preview and the
// dependency checks need.
type Issue struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Type     string `json:"type,omitempty"`
	Priority int    `json:"priority"`
	Status   string `json:"status"`
	// DependsOn is every unclosed blocking dependency. A closed one is already
	// satisfied and cannot hold anything up, so it is not carried.
	DependsOn []string `json:"depends_on,omitempty"`
}

// Set is an epic's candidate set: the issues a run could be scoped to right
// now.
type Set struct {
	Epic string `json:"epic"`
	// Issues are the open, unparked children, in bd's priority order.
	Issues []Issue `json:"issues"`
	// Skipped names the children that are not candidates and why, so the
	// preview can account for an epic's whole child list rather than silently
	// showing a shorter one.
	Skipped map[string]string `json:"skipped,omitempty"`
}

// IDs returns the candidate issue IDs in order.
func (s Set) IDs() []string {
	out := make([]string, 0, len(s.Issues))
	for _, i := range s.Issues {
		out = append(out, i.ID)
	}
	return out
}

// Get returns the candidate with an ID.
func (s Set) Get(id string) (Issue, bool) {
	for _, i := range s.Issues {
		if i.ID == id {
			return i, true
		}
	}
	return Issue{}, false
}

// Candidates returns the epic's open, unparked children.
//
// A closed child is done and a blocked one was parked for a human, so neither is
// something to offer. Dependencies are read per issue rather than taken from the
// listing, because the listing does not carry them and the wave preview is the
// whole reason a human can tell what they are approving.
func Candidates(src Source, epic string) (Set, error) {
	if epic == "" {
		return Set{}, fmt.Errorf("scope: an epic is required")
	}
	children, err := src.Children(epic)
	if err != nil {
		return Set{}, fmt.Errorf("scope: list children of %s: %w", epic, err)
	}

	set := Set{Epic: epic, Skipped: map[string]string{}}
	for _, c := range children {
		if c.ID == "" || c.ID == epic {
			continue
		}
		switch {
		case c.Closed():
			set.Skipped[c.ID] = "closed"
			continue
		case c.Blocked():
			set.Skipped[c.ID] = "parked for a human"
			continue
		}
		iss := Issue{ID: c.ID, Title: c.Title, Type: c.IssueType, Priority: c.Priority, Status: c.Status}
		if full, err := src.Show(c.ID); err == nil && full != nil {
			iss.DependsOn = unmetDeps(full)
			if full.Title != "" {
				iss.Title = full.Title
			}
		}
		set.Issues = append(set.Issues, iss)
	}

	sort.SliceStable(set.Issues, func(i, j int) bool {
		if set.Issues[i].Priority != set.Issues[j].Priority {
			return set.Issues[i].Priority < set.Issues[j].Priority
		}
		return set.Issues[i].ID < set.Issues[j].ID
	})
	return set, nil
}

// unmetDeps lists the blocking dependencies an issue is still waiting on.
func unmetDeps(iss *bd.Issue) []string {
	var out []string
	for _, d := range iss.Dependencies {
		if d.ID == "" || !blocking(d.Type) || d.Status == "closed" {
			continue
		}
		out = append(out, d.ID)
	}
	return out
}

// Resolve turns a requested selection into the scope a run will record.
//
// Requested IDs are checked against the candidate set rather than trusted: an ID
// that is not a candidate is a typo, a closed issue or something from another
// epic, and every one of those is better said now than discovered by a worker.
func Resolve(set Set, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return nil, fmt.Errorf("scope: nothing selected")
	}
	seen := map[string]bool{}
	var out []string
	var unknown []string
	for _, raw := range requested {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := set.Get(id); !ok {
			if why, skipped := set.Skipped[id]; skipped {
				unknown = append(unknown, fmt.Sprintf("%s (%s)", id, why))
			} else {
				unknown = append(unknown, id)
			}
			continue
		}
		out = append(out, id)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("scope: not a candidate under %s: %s", set.Epic, strings.Join(unknown, ", "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("scope: nothing selected")
	}
	return out, nil
}

// Waves decomposes a selection into the waves it would actually run in.
//
// It is a preview, not a plan: the engine asks bd for readiness every wave, and
// a retry can move an issue later than this says. What it is for is showing a
// human the shape of the spend before they approve it — how many rounds of
// concurrency the selection implies, and which issues wait on which.
//
// Dependencies outside the selection are ignored here on purpose. An issue whose
// blocker is out of scope never becomes ready at all, and the run parks it with
// that reason rather than running it in a later wave; Blocked reports those
// separately so the preview can show them as what they are.
func Waves(set Set, selected []string, concurrency int) [][]string {
	if concurrency <= 0 {
		concurrency = 1
	}
	inScope := map[string]bool{}
	for _, id := range selected {
		inScope[id] = true
	}

	pending := make([]Issue, 0, len(selected))
	for _, iss := range set.Issues {
		if inScope[iss.ID] {
			pending = append(pending, iss)
		}
	}

	done := map[string]bool{}
	var waves [][]string
	for len(pending) > 0 {
		var wave []string
		var rest []Issue
		for _, iss := range pending {
			if len(wave) < concurrency && ready(iss, inScope, done) {
				wave = append(wave, iss.ID)
				continue
			}
			rest = append(rest, iss)
		}
		if len(wave) == 0 {
			// Nothing can start: a cycle, or every remaining issue waits on
			// something out of scope. Report the rest as one final wave rather
			// than looping — the preview's job is to show the work, and the
			// engine is what refuses to run it.
			var stuck []string
			for _, iss := range rest {
				stuck = append(stuck, iss.ID)
			}
			return append(waves, stuck)
		}
		for _, id := range wave {
			done[id] = true
		}
		waves = append(waves, wave)
		pending = rest
	}
	return waves
}

// ready reports whether every in-scope blocker of an issue has already run.
func ready(iss Issue, inScope, done map[string]bool) bool {
	for _, d := range iss.DependsOn {
		if inScope[d] && !done[d] {
			return false
		}
	}
	return true
}

// Blocker is an in-scope issue whose blocking dependency the run was never
// allowed to touch.
type Blocker struct {
	Issue string `json:"issue"`
	Dep   string `json:"dep"`
	// Reason is what gets recorded on the issue, so a human reading bd sees the
	// same sentence the run reported.
	Reason string `json:"reason"`
}

// Blocked finds the scoped issues that can never become ready, because
// something they depend on is unmet and outside the scope.
//
// Without this they would sit unready for the whole run and end it unable to
// explain themselves: bd would keep them out of every ready front, the engine
// would never dispatch them, and the run would finish reporting nothing about
// them at all. Parking them at the start turns silence into a reason.
//
// An empty scope is an unrestricted run, so nothing is out of it.
func Blocked(src Source, selected []string) ([]Blocker, error) {
	if len(selected) == 0 {
		return nil, nil
	}
	inScope := map[string]bool{}
	for _, id := range selected {
		inScope[id] = true
	}

	var out []Blocker
	for _, id := range selected {
		iss, err := src.Show(id)
		if err != nil {
			return nil, fmt.Errorf("scope: %s: %w", id, err)
		}
		if iss == nil || iss.Terminal() {
			continue
		}
		for _, d := range unmetDeps(iss) {
			if inScope[d] {
				continue
			}
			out = append(out, Blocker{
				Issue:  id,
				Dep:    d,
				Reason: fmt.Sprintf("dependency %s is out of scope for this run and is not closed", d),
			})
			break // one reason is enough; the first unmet blocker is the answer
		}
	}
	return out, nil
}
