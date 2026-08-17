package drain

import (
	"context"
	"errors"
	"testing"

	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
)

// The failure these tests describe is not hypothetical. beads' post-checkout
// and post-merge hooks import .beads/issues.jsonl over its database, so any bd
// write made since that export is reverted with a zero exit code — observed in
// this repo as one `git pull --rebase` taking eight issues from closed back to
// open. internal/gitx stops bd-auto's own git from firing those hooks; the
// barrier's reconcile pass covers everything else that can, which is why the
// revert here is injected directly rather than through a git command.

// revert stands in for whatever put an issue back: the hook, a human, a beads
// import. What did it is not the barrier's business — only that run state and
// bd now disagree, in the direction that says finished work is unfinished.
func revert(iss *fakeIssues, id, to string) { iss.set(id, to) }

// TestTheBarrierReClosesWorkThatWasRevertedUnderIt is the whole point: a run
// that finished an issue leaves bd saying so, whatever happened in between.
func TestTheBarrierReClosesWorkThatWasRevertedUnderIt(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	finishedWorker(t, repo, cfg, "t-2", "b.txt", "b\n")
	iss.set("t-1", "closed")
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1", "t-2")

	// Both were closed by their workers; something takes one of them back.
	revert(iss, "t-1", "open")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	if got := rep.Reconciled.Closed; len(got) != 1 || got[0] != "t-1" {
		t.Fatalf("reconciled %v, want exactly [t-1]", got)
	}
	if cur, _ := iss.Show("t-1"); !cur.Closed() {
		t.Fatalf("t-1 is %q in bd after the barrier; the reconcile did not stick", cur.Status)
	}

	// And the consequence that actually costs a run: with a child reading as
	// open, the epic can never close and the drain never reaches an end.
	if !rep.EpicClosed {
		t.Fatalf("the epic stayed open: %s", rep.EpicReason)
	}
}

// TestTheBarrierReParksWorkItSetAside is the same property for the other
// terminal state. A parked issue that quietly reads as open again is offered to
// the next wave as ready work, and the run pays to fail it a second time.
func TestTheBarrierReParksWorkItSetAside(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")

	st := runstate.New("epic-1", 2, "auto", 0)
	st.WaveIssues = []string{"t-1", "t-2"}
	st.MarkDone("t-1")
	st.Park("t-2", "the gate stayed red", "gate")
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}
	iss.set("t-2", "blocked")

	revert(iss, "t-2", "open")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	if got := rep.Reconciled.Parked; len(got) != 1 || got[0] != "t-2" {
		t.Fatalf("re-parked %v, want exactly [t-2]", got)
	}
	if cur, _ := iss.Show("t-2"); !cur.Blocked() {
		t.Fatalf("t-2 is %q in bd after the barrier; the reconcile did not stick", cur.Status)
	}
}

// TestAnAgreeingBarrierReconcilesNothing keeps the pass honest. If it wrote to
// bd on every barrier regardless, the tests above would pass while the feature
// was a no-op wrapped around an unconditional close.
func TestAnAgreeingBarrierReconcilesNothing(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1").under("epic-1", "t-1")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	waveState(t, repo, "epic-1", "t-1")

	before, _, _ := iss.snapshot()
	closedBefore := len(iss.closed)

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	if !rep.Reconciled.Empty() {
		t.Fatalf("a barrier bd already agreed with reconciled %+v", rep.Reconciled)
	}
	// The epic close is the one legitimate write, so it is the one allowed.
	if got := len(iss.closed) - closedBefore; got > 1 {
		t.Fatalf("the barrier wrote %d closes; only the epic's is expected", got)
	}
	if after, _, _ := iss.snapshot(); len(after) != len(before) {
		t.Fatalf("the barrier appended %d note(s) to issues it agreed about", len(after)-len(before))
	}
}

// TestReconcileLeavesIssuesTheRunNeverJudgedAlone is the limit. A human
// reopening an issue mid-run is making a decision; an issue this run reached no
// verdict on is not the barrier's to re-assert, and a pass that fought either
// would be worse than the problem it fixes.
func TestReconcileLeavesIssuesTheRunNeverJudgedAlone(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	// t-2 is in the epic but this run never touched it, and it is closed in bd
	// for reasons of its own.
	iss.set("t-2", "closed")
	waveState(t, repo, "epic-1", "t-1")

	revert(iss, "t-2", "open")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	if !rep.Reconciled.Empty() {
		t.Fatalf("the barrier re-asserted an issue the run never judged: %+v", rep.Reconciled)
	}
	if cur, _ := iss.Show("t-2"); cur.Closed() {
		t.Fatal("the barrier closed t-2, which this run never worked on")
	}
}

// TestAnUnreadableIssueIsReportedRatherThanAssumedClean covers bd going away
// mid-barrier. Reporting a clean reconciliation over an unreachable database
// would be the same silent-success failure this whole pass exists to undo.
//
// It drives reconcile directly rather than through Integrate: a bd that fails
// everything would also fail the candidate gathering and the epic close, and
// the property under test is this pass's alone.
func TestAnUnreadableIssueIsReportedRatherThanAssumedClean(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1").under("epic-1", "t-1")
	waveState(t, repo, "epic-1", "t-1")

	iss.fail = errors.New("bd is unreachable")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rec := e.reconcile()

	if got := rec.Failed; len(got) != 1 || got[0] != "t-1" {
		t.Fatalf("failed %v, want exactly [t-1]; an unreadable issue must not read as reconciled", got)
	}
	if rec.Empty() {
		t.Fatal("a reconciliation that could not read bd reported itself as clean")
	}
	if len(rec.Closed) != 0 {
		t.Fatalf("closed %v without being able to read bd", rec.Closed)
	}
}
