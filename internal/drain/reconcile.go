package drain

import (
	"fmt"

	"bd-auto/internal/runstate"
)

// Reconciliation is what a barrier had to put back into bd.
//
// Every entry is an issue whose status in bd disagreed with what this run knows
// about it, and disagreed in the one direction that is never a legitimate
// update: work the run finished reading as unfinished. A non-empty
// reconciliation is a report of something having reverted bd underneath a live
// run, so it is carried in the barrier's report rather than only logged.
type Reconciliation struct {
	// Closed is the issues this run completed that bd no longer had as closed.
	Closed []string `json:"closed,omitempty"`
	// Parked is the issues this run set aside that bd no longer had as blocked.
	Parked []string `json:"parked,omitempty"`
	// Failed is the issues that could not be read or written at all. bd being
	// unreachable is not this function's problem to solve, but silently
	// reporting a clean reconciliation over it would be.
	Failed []string `json:"failed,omitempty"`
}

// Empty reports whether bd already agreed with run state about everything.
func (r Reconciliation) Empty() bool {
	return len(r.Closed) == 0 && len(r.Parked) == 0 && len(r.Failed) == 0
}

// Total is how many issues this reconciliation touched.
func (r Reconciliation) Total() int { return len(r.Closed) + len(r.Parked) }

// reconcile re-asserts run state onto bd, for child closures and parks the
// orchestrator already completed.
//
// # Why a run has to do this at all
//
// run.json is bd-auto's own record and lives under .beads/auto/, which is
// gitignored — so it is neither exported to .beads/issues.jsonl nor imported
// over. bd's database has no such protection: beads' post-checkout and
// post-merge hooks import the jsonl over it, replaying whatever was exported
// onto whatever is actually there, and every bd write since that export is
// reverted with a zero exit code. Observed in this repo: one `git pull
// --rebase` in the main checkout reverted eight issues from closed to open.
//
// internal/gitx stops orchestrator Git commands from firing those hooks. This
// pass handles external imports that the run does not control. It does not use
// an approved-but-unmerged result as evidence that a child should be closed.
//
// # Why it runs at the barrier, and before the epic close
//
// The barrier is where the run's own view is complete for the wave — every
// branch merged or parked, every park recorded — and it is the last point
// before that view is used to decide something irreversible. EpicComplete
// requires every child issue to read as closed, so an epic whose children were
// silently reopened stays open forever and the drain never reaches an end.
// Reconciling first makes that decision read the run's own record rather than
// whatever last wrote to the database.
//
// # What it will not do
//
// Only in the direction of a recorded closure or park. An approved issue that
// has not merged is not touched. Closing from the broader Done list would cross
// the lifecycle boundary this package enforces.
func (e *Engine) reconcile() Reconciliation {
	var rec Reconciliation

	// Reloaded rather than taken from the caller: the merges and the gate
	// blame above have been parking issues, and this must see the run as it
	// stands now.
	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		e.logf("warning: could not re-read the run state to reconcile bd: %v", err)
		return rec
	}

	for _, id := range st.Closed {
		iss, err := e.BD.Show(id)
		if err != nil {
			e.logf("warning: could not read %s to reconcile it: %v", id, err)
			rec.Failed = append(rec.Failed, id)
			continue
		}
		if iss.Closed() {
			continue
		}
		if err := e.BD.Close(id, reconcileClosedReason(id, iss.Status)); err != nil {
			e.logf("warning: could not re-close %s: %v", id, err)
			rec.Failed = append(rec.Failed, id)
			continue
		}
		e.logf("reconciled %s: this run completed it, but bd had it %s", id, iss.Status)
		rec.Closed = append(rec.Closed, id)
	}

	for _, p := range st.Parked {
		iss, err := e.BD.Show(p.ID)
		if err != nil {
			e.logf("warning: could not read %s to reconcile it: %v", p.ID, err)
			rec.Failed = append(rec.Failed, p.ID)
			continue
		}
		if iss.Blocked() {
			continue
		}
		if err := e.BD.Park(p.ID, reconcileParkedReason(p, iss.Status)); err != nil {
			e.logf("warning: could not re-park %s: %v", p.ID, err)
			rec.Failed = append(rec.Failed, p.ID)
			continue
		}
		e.logf("reconciled %s: this run parked it, but bd had it %s", p.ID, iss.Status)
		rec.Parked = append(rec.Parked, p.ID)
	}

	return rec
}

// reconcileClosedReason says what happened, in the close reason a human will
// read off the issue. It names the mechanism because the alternative reading —
// that bd-auto closes issues it has not finished — is much worse than the truth.
func reconcileClosedReason(id, status string) string {
	return fmt.Sprintf(
		"bd-auto completed %s in this run, but bd had it %s again by the barrier. "+
			"beads' post-checkout and post-merge hooks import .beads/issues.jsonl over the "+
			"database, which reverts bd writes made since that export; this re-asserts the "+
			"run's own record.", id, status)
}

func reconcileParkedReason(p runstate.Parked, status string) string {
	return fmt.Sprintf(
		"bd-auto parked %s in this run after %d attempt(s) (%s), but bd had it %s again by "+
			"the barrier. This re-asserts the run's own record; the original reason was: %s",
		p.ID, p.Attempts, stageOr(p.Stage), status, p.Reason)
}
