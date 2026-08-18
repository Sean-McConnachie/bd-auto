package drain

import (
	"context"
	"errors"
	"fmt"

	"bd-auto/internal/gitx"
	"bd-auto/internal/pipeline"
	"bd-auto/internal/runstate"
)

// Handing over a run that is already over.
//
// Engine.Handoff runs from inside a drain, at finish, off the report that drain
// spent the whole run building. A run that was interrupted, or one whose parked
// issue a human unparked and fixed by hand, has an epic branch with everything
// on it and no report anywhere: the process that held it is gone. Without this
// the only ways to that pull request are re-running the entire drain, or `gh pr
// create` by hand — which publishes the branch with none of the handoff document
// that makes it reviewable.
//
// So this rebuilds what the predicate reads, from what the run wrote down, and
// then goes through exactly the same predicate, the same forge and the same
// document. What it will not do is invent the rest. Run state does not record
// which merges a model resolved or what the run cost, and those parts of the
// document are absent rather than filled in with a confident zero — the pull
// request says so itself.

// HandoffFromState hands over a run that has already finished, from the state it
// left behind rather than from a report nobody holds any more.
//
// It returns an error only for what is not a verdict on the work: no run, no
// epic branch, a branch that is gone, a checkout standing somewhere else. A
// refusal to publish is not one of those — it comes back in the report, for the
// same reason Handoff itself never errors.
func (e *Engine) HandoffFromState(ctx context.Context, opts HandoffOptions) (HandoffReport, error) {
	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		return HandoffReport{}, err
	}
	rep := e.stateReport(st)
	switch {
	case rep.EpicBranch == "":
		return HandoffReport{}, errors.New(
			"this run was not staged on an epic branch, so there is nothing to open a pull request from")
	case !gitx.BranchExists(e.RepoRoot, rep.EpicBranch):
		return HandoffReport{}, fmt.Errorf(
			"the epic branch %s is not in this checkout any more", rep.EpicBranch)
	}
	// The gate runs on the working tree, so a checkout standing anywhere else
	// would prove a different branch and report the answer as this one's. That
	// is a switch away, and it is the human's to make: a checkout holding a
	// whole run is not something to move on somebody's behalf.
	if cur := gitx.CurrentBranch(e.RepoRoot); cur != rep.EpicBranch {
		return HandoffReport{}, fmt.Errorf(
			"the checkout is on %s, and the gate would prove that instead of the run's result; "+
				"`git switch %s` first", cur, rep.EpicBranch)
	}

	// Re-gated rather than remembered. The whole point of handing over after the
	// fact is that something happened after the fact — a human fixing a parked
	// issue on the branch, most of all — and a verdict from before that is a
	// verdict about a different tree.
	in := &rep.Integrations[len(rep.Integrations)-1]
	in.Gate = e.gateRepo(st.Wave)
	in.GatePassed = pipeline.Passed(in.Gate)
	in.Head, _ = git(e.RepoRoot, "rev-parse", rep.EpicBranch)

	opts.ByHand = true
	return e.HandoffWith(ctx, rep, opts), nil
}

// stateReport rebuilds the part of a drain report a handoff reads.
//
// Everything here is read from run state or from git, and the one field neither
// of them holds is what actually landed. Run state's done list is what workers
// finished, which is not the same set: a run that stopped between a worker
// closing its issue and the barrier that would have merged its branch has an
// issue that is done and not on the branch. So git is asked instead, per issue,
// and the answer is trusted over the list.
func (e *Engine) stateReport(st *runstate.State) DrainReport {
	rep := DrainReport{
		Epic:       st.Epic,
		Scope:      append([]string(nil), st.Scope...),
		Waves:      st.Wave,
		Base:       st.Base,
		EpicBranch: st.EpicBranch,
		Done:       append([]string(nil), st.Done...),
		Parked:     parkedIDs(st),
		Outcome:    stateOutcome(st.Status),
	}
	if !st.StartedAt.IsZero() && st.UpdatedAt.After(st.StartedAt) {
		rep.Seconds = st.UpdatedAt.Sub(st.StartedAt).Seconds()
	}

	// One synthesised barrier, standing for every barrier the run actually ran.
	// Landed reads its merges, and the handoff document reads Landed; the merges
	// are recorded clean because run state does not say which of them a model
	// resolved, and a resolved merge claimed as clean would send a reviewer to
	// the wrong part of the diff.
	in := IntegrateReport{
		Epic: st.Epic, Wave: st.Wave,
		Base: st.EpicBranch, EpicBranch: st.EpicBranch, Target: st.Base,
	}
	for _, id := range rep.Done {
		branch := e.Cfg.Branch(id)
		if !landedOn(e.RepoRoot, branch, st.EpicBranch) {
			continue
		}
		in.Merges = append(in.Merges, Merge{Issue: id, Branch: branch, Outcome: MergeClean})
	}
	rep.Integrations = []IntegrateReport{in}
	return rep
}

// stateOutcome reads a run's status as the outcome the predicate wants. Only a
// run recorded as done finished; anything else is still armed, which is a run
// that stopped rather than one that ended.
func stateOutcome(status string) Outcome {
	if status == runstate.StatusDone {
		return OutcomeDone
	}
	return OutcomeInterrupted
}

// landedOn reports whether an issue's branch is in the epic branch.
//
// A branch that is gone counts as landed, and that is not a guess: the barrier
// deletes a worker branch only after merging it, so a branch this run finished
// and git no longer has is one the barrier cleaned up. A branch that is still
// there is asked directly, and a "no" is the honest answer for a branch whose
// barrier never came.
func landedOn(repoRoot, branch, epic string) bool {
	if !gitx.BranchExists(repoRoot, branch) {
		return true
	}
	_, err := git(repoRoot, "merge-base", "--is-ancestor", branch, epic)
	return err == nil
}
