package tui

// The barrier, in the table.
//
// A wave barrier merges every branch, spawns a model for any conflict, gates
// the merged result and peels merges back off it when that gate goes red. It
// takes minutes and it spends real money, and until it had rows of its own the
// whole of it was one status line above a table of frozen workers — with no way
// to tell a barrier resolving a conflict from one that had hung.
//
// So it gets a block of its own, under the wave and in the same columns: a row
// per branch and a row for the gate, carrying the same state, time, cost and
// activity cells a worker's row carries. A branch whose conflict a model is
// resolving shows that model's live tool calls, exactly as a wave row shows its
// worker's — and enter on it opens the integrator's transcript, because the
// integrator writes into the transcript of the issue whose branch it is
// merging.

import (
	"fmt"
	"strings"
	"time"

	"bd-auto/internal/drain"
	"bd-auto/internal/runner"
)

// The states a barrier row takes. They are the barrier's own vocabulary for the
// same reason the wave rows have theirs: a merge is not a worker, and calling
// it "running" would throw away the one distinction the block exists to draw —
// between a merge git did on its own and one a model is still resolving.
const (
	StateMerging    State = "merging"
	StateResolving  State = "resolving"
	StateMerged     State = "merged"
	StateSkipped    State = "skipped"
	StateRolledBack State = "rolled back"
	// StatePassed is the gate's own green. The gate row borrows StateRunning
	// while it runs and StateFailed when it does not pass, which are the words
	// the rest of the table already uses for both.
	StatePassed State = "passed"
)

// gateRowName is what the gate is called in the ISSUE column. It is not an
// issue and it never will be, which is why it is also the row that has no
// transcript to open.
const gateRowName = "gate"

// barrier is one wave's trip through the merge, as the table shows it.
type barrier struct {
	wave int
	// merges is a row per branch, in the order the barrier tried them, and gate
	// the single row for the gate on the merged result. The gate is held apart
	// rather than appended so it stays at the foot of the block: a barrier
	// settled from its report can name branches it never emitted a merge for,
	// and those rows arrive after the gate has already run.
	merges []*Row
	gate   *Row

	// lane says this block is a continuous run's integration lane rather than a
	// barrier between two waves. There is no boundary to draw for one: the
	// workers above it never stopped, and merges arrive into it one at a time
	// for as long as the run lasts.
	lane bool

	// settled is whether the barrier has ended, and usage what it reported
	// spending. Once it has, that figure wins over whatever the rows accrued
	// live: it is the number in the report, and the two must not disagree.
	//
	// A lane settles once per merge and re-opens on the next one, so usage
	// accumulates rather than replacing. A wave barrier ends once, where
	// accumulating and replacing are the same thing.
	settled bool
	usage   runner.Usage
	// merged and parked are what this block has decided about, across every
	// integration it has seen. A wave barrier sees exactly one.
	merged, parked int
	gatePassed     bool
	// verdict is how the barrier went, kept on the block rather than only in
	// the status line, which the next wave overwrites.
	verdict string
	// blamed is the branch the last rollback took off. A red gate that goes
	// green is green because of whichever branch came off last, so this is the
	// branch the barrier is about to park — and the one fact a human watching a
	// red gate has to be able to read.
	blamed string
}

// rows is the block, in display order.
func (b *barrier) rows() []*Row {
	if b.gate == nil {
		return b.merges
	}
	return append(append(make([]*Row, 0, len(b.merges)+1), b.merges...), b.gate)
}

// row finds or creates one branch's row.
func (b *barrier) row(issue string) *Row {
	for _, r := range b.merges {
		if r.Issue == issue {
			return r
		}
	}
	r := &Row{Issue: issue, Wave: b.wave, State: StateWaiting, Detail: "queued",
		logIssue: issue, barrier: true}
	b.merges = append(b.merges, r)
	return r
}

// gateRow finds or creates the gate's row.
func (b *barrier) gateRow() *Row {
	if b.gate == nil {
		b.gate = &Row{Issue: gateRowName, Wave: b.wave, State: StateWaiting, barrier: true}
	}
	return b.gate
}

// cost is what this barrier has spent.
func (b *barrier) cost() float64 {
	if b.settled {
		return b.usage.CostUSD
	}
	var total float64
	for _, r := range b.merges {
		total += r.Cost()
	}
	return total
}

// integratorName is what this run calls the thing that merges: a barrier
// between waves, or a lane beside workers that never stop.
func (m *Model) integratorName() string {
	if m.lane {
		return "integration"
	}
	return "barrier"
}

// label heads the block.
func (b *barrier) label() string {
	head := fmt.Sprintf("wave %d barrier", b.wave)
	if b.lane {
		head = "integration"
	}
	if b.verdict != "" {
		head += " · " + b.verdict
	}
	return head
}

// --- folding events in ---

// barrierFor finds or opens the block for one wave's barrier.
func (m *Model) barrierFor(wave int) *barrier {
	for _, b := range m.barriers {
		if b.wave == wave {
			return b
		}
	}
	b := &barrier{wave: wave}
	m.barriers = append(m.barriers, b)
	return b
}

// integrating opens the block with a row per branch the barrier is about to
// try, all of them queued. They are drawn before anything has happened to them
// because that is the point: the block says how much work the barrier has left,
// not only which piece of it is in flight.
func (m *Model) integrating(e drain.Event) {
	b := m.barrierFor(e.Wave)
	b.lane, m.lane = b.lane || e.Lane, m.lane || e.Lane
	// A block that has been handed another branch to merge is not finished,
	// whatever it said last time. A lane says it is done after every merge and
	// then gets the next one; leaving it settled would freeze the rows and stop
	// the cost accruing for the rest of the run.
	b.settled = false
	for _, id := range e.Issues {
		b.row(id)
	}
	if b.lane {
		if len(e.Issues) > 0 {
			m.status = "integrating " + strings.Join(e.Issues, ", ") + " while the other workers run"
		}
		return
	}
	m.status = fmt.Sprintf("wave %d integrating: merging %s", e.Wave, branches(len(e.Issues)))
}

// mergeStart is one branch's row going live.
func (m *Model) mergeStart(e drain.Event) {
	r := m.barrierFor(e.Wave).row(e.Issue)
	r.State, r.Started, r.Ended, r.final = StateMerging, e.At, time.Time{}, false
	r.Role, r.Stage = "", ""
	r.say("merging " + branchOf(e))
}

// mergeConflict is git stopping, and a model being spawned to finish the merge.
func (m *Model) mergeConflict(e drain.Event) {
	r := m.barrierFor(e.Wave).row(e.Issue)
	if r.Started.IsZero() {
		r.Started = e.At
	}
	r.State, r.Role = StateResolving, e.Role
	r.say(conflictDetail(e))
}

// mergeEnd is one branch's verdict, on its barrier row and on its own row in
// the wave table above.
func (m *Model) mergeEnd(e drain.Event) {
	if e.Merge == nil {
		return
	}
	m.barrierFor(e.Wave).row(e.Issue).merged(e.Merge, e.At)
	m.settle(*e.Merge)
}

// rolledBack is a merge peeled back off the merged result to find out which
// branch a red gate is about. It is not a verdict: the branch is either about
// to be blamed, or about to be put back, and the merge-end that follows says
// which.
func (m *Model) rolledBack(e drain.Event) {
	b := m.barrierFor(e.Wave)
	b.blamed = branchOf(e)
	r := b.row(e.Issue)
	r.State, r.final = StateRolledBack, false
	r.say(detailOr(e, "rolled back off the merged result"))
}

// gateStart is the gate beginning on the merged result. It restarts the row
// rather than adding another, because the peeling a red gate does runs it again
// for every branch it takes back out and a row per run would be a list of the
// same command.
func (m *Model) gateStart(e drain.Event) {
	g := m.barrierFor(e.Wave).gateRow()
	g.State, g.Started, g.Ended = StateRunning, e.At, time.Time{}
	g.say(detailOr(e, "the gate is running on the merged result"))
}

// gateEnd is that gate's verdict.
func (m *Model) gateEnd(e drain.Event) {
	b := m.barrierFor(e.Wave)
	g := b.gateRow()
	if g.Started.IsZero() {
		g.Started = e.At
	}
	g.State, g.Ended = gateState(e.Passed), e.At
	g.say(b.gateDetail(e))
}

// waveEnd settles the block against the report.
//
// Everything here has usually been said already, one event at a time, and it is
// said again because the report is the authority and because a view can be
// watching a barrier whose events it missed: a run resumed into, a `bd-auto
// integrate` on its own, a barrier asked to settle every branch the run ever
// touched. Folding the report in is what makes the block right in all of those.
func (m *Model) waveEnd(e drain.Event) {
	b := m.barrierFor(e.Wave)
	b.lane, m.lane = b.lane || e.Lane, m.lane || e.Lane
	if rep := e.Integration; rep != nil {
		for i := range rep.Merges {
			mg := rep.Merges[i]
			b.row(mg.Issue).merged(&mg, e.At)
			m.settle(mg)
		}
		if len(rep.Gate) > 0 {
			g := b.gateRow()
			if g.Started.IsZero() {
				g.Started = e.At
			}
			if g.Ended.IsZero() {
				g.Ended = e.At
			}
			g.State = gateState(rep.GatePassed)
			g.say(b.reportGateDetail(rep))
		}
		b.merged += len(rep.Merged())
		b.parked += len(rep.Parked())
		b.gatePassed = rep.GatePassed
		b.verdict = fmt.Sprintf("%d merged, %d parked, gate %s",
			b.merged, b.parked, passFail(b.gatePassed))
		if b.lane {
			m.status = "integrated: " + b.verdict
			if merged := rep.Merged(); len(merged) > 0 {
				m.status = "integrated " + strings.Join(merged, ", ") + ": " + b.verdict
			}
		} else {
			m.status = fmt.Sprintf("wave %d integrated: %s", e.Wave, b.verdict)
		}
		// The barrier's other two jobs, said only when they had anything to
		// do. Both are rare and neither belongs to a row: filing is work this
		// wave's workers found and a human will schedule, and a reconciliation
		// means something outside the run reverted bd underneath it.
		b.verdict += bookkeeping(rep)
	}
	// Last, so the rows above accrue nothing more: from here the block's cost
	// is the figure the reports carried. Added rather than assigned, for the
	// lane: every integration reports its own spend, and the block is the sum
	// of them.
	b.settled, b.usage = true, b.usage.Add(e.Usage)
}

// settle folds one merge back into the issue's own row in the wave table.
//
// A worker finishing and its work landing are two different things, and the
// barrier is where they can part company: a branch git will not merge parks an
// issue whose worker was done, gated and reviewed. Without this the row keeps
// saying done and every count taken from the rows keeps counting it, so a run
// ends with the table saying "1 done · 0 parked" directly above the run's own
// verdict, "3 done, 1 parked".
//
// Only rows the table already has, and only the ones that finished done. A
// barrier asked to settle every branch the run ever touched can name issues
// from waves this view never watched, and inventing rows for them at the end
// would be a different kind of lie; a row that was killed or failed says how it
// ended already, and the barrier has nothing to add to it.
func (m *Model) settle(mg drain.Merge) {
	r, ok := m.rows[mg.Issue]
	if !ok || r.State != StateDone {
		return
	}
	switch mg.Outcome {
	case drain.MergeParked:
		r.State = StateParked
		if mg.Reason != "" {
			r.say(firstLine(mg.Reason))
		}
	case drain.MergeResolved:
		// A resolved merge used to leave this row showing the integrator's last
		// tool call, which was true a second ago and is now just stale. What it
		// should say is what happened.
		r.say("merged; " + resolvedBy(&mg))
	}
}

// resolving is the barrier row an integrator's activity belongs to, or nil.
//
// A barrier works one branch at a time, and that branch's row is the only place
// its tool calls mean anything: the issue's own row in the wave table finished
// minutes ago, and writing the integrator's activity there is what used to
// leave a done row showing a tool call its worker never ran.
func (m *Model) resolving(issue string) *Row {
	for i := len(m.barriers) - 1; i >= 0; i-- {
		b := m.barriers[i]
		if b.settled {
			continue
		}
		for _, r := range b.merges {
			if r.Issue != issue {
				continue
			}
			if r.State == StateMerging || r.State == StateResolving {
				return r
			}
		}
	}
	return nil
}

// merged is one branch's verdict, on the barrier row.
func (r *Row) merged(mg *drain.Merge, at time.Time) {
	r.State = mergeState(mg.Outcome)
	r.Role, r.Stage = "", ""
	// The merge knows how long it took, so a row settled from a report rather
	// than watched live still has a time to show.
	if mg.Seconds > 0 {
		r.Started = at.Add(-time.Duration(mg.Seconds * float64(time.Second)))
	} else if r.Started.IsZero() {
		r.Started = at
	}
	r.Ended, r.total, r.final = at, mg.Usage, true
	r.settled, r.live = runner.Usage{}, runner.Usage{}
	r.say(mergeDetail(mg))
}

// --- rendering ---

// barrierRows is every barrier row a cursor can be on, in display order.
func (m *Model) barrierRows() []*Row {
	var out []*Row
	for _, b := range m.barriers {
		out = append(out, b.rows()...)
	}
	return out
}

// barrierLines renders the barriers under the wave table, and says which of the
// lines are selectable rows.
//
// at is the cursor index the first barrier row takes, continuing from the issue
// rows above. The returned map goes from cursor index to the index of its line,
// which is what lets the table be windowed with the cursor kept in view: a
// barrier contributes rules as well as rows, so a cursor index and a line index
// are not the same number and cannot be recovered from the text afterwards.
func (m *Model) barrierLines(at int, now time.Time) (lines []string, rowAt map[int]int) {
	rowAt = map[int]int{}
	for _, bar := range m.barriers {
		rows := bar.rows()
		if len(rows) == 0 {
			continue
		}
		lines = append(lines, "", headerStyle.Render(rule(bar.label(), m.width())))
		for _, r := range rows {
			rowAt[at] = len(lines)
			lines = append(lines, m.line(r, at == m.cursor, now))
			at++
		}
	}
	return lines, rowAt
}

// barrierBlocks is barrierLines as one string, for callers that only render.
func (m *Model) barrierBlocks(at int, now time.Time) string {
	lines, _ := m.barrierLines(at, now)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// barrierCost is what every barrier in this run has spent. It is shown on its
// own in the summary as well as inside the run total, because it belongs to no
// issue and a figure folded only into the total is a figure nobody can look at.
func (m *Model) barrierCost() float64 {
	var total float64
	for _, b := range m.barriers {
		total += b.cost()
	}
	return total
}

// --- formatting ---

// mergeState maps a merge's outcome onto what its row shows.
func mergeState(o drain.MergeOutcome) State {
	switch o {
	case drain.MergeClean, drain.MergeResolved:
		return StateMerged
	case drain.MergeParked:
		return StateParked
	case drain.MergeSkipped:
		return StateSkipped
	}
	// A merge with no outcome at all is one the barrier returned an error on,
	// which is not a verdict on the branch and must not read as one.
	return StateInterrupted
}

func gateState(passed bool) State {
	if passed {
		return StatePassed
	}
	return StateFailed
}

// resolvedBy says what got a conflicted branch merged. A conflict in beads'
// own export is settled by rule and costs nothing, so a row that says a model
// resolved it would leave an empty cost cell looking like a bug.
func resolvedBy(mg *drain.Merge) string {
	if len(mg.Conflicts) == 0 {
		return "settled " + conflicts(len(mg.Settled)) + " in beads' exports, no model"
	}
	return "a model resolved " + conflicts(len(mg.Conflicts))
}

// mergeDetail is what a settled branch's activity cell says. Clean and resolved
// both landed and read differently on purpose: a resolved merge is the only one
// that spent anything, and the cost cell beside it is otherwise unexplained.
func mergeDetail(mg *drain.Merge) string {
	switch mg.Outcome {
	case drain.MergeClean:
		return "clean, no conflicts"
	case drain.MergeResolved:
		return resolvedBy(mg)
	}
	if mg.Reason != "" {
		return firstLine(mg.Reason)
	}
	if mg.Outcome == "" {
		return "the barrier reached no verdict on " + mg.Branch
	}
	return string(mg.Outcome)
}

// conflictDetail names what the integrator is working on. The paths are the
// whole of what a watcher can judge a resolution by while it is happening.
func conflictDetail(e drain.Event) string {
	head := "resolving "
	if e.Merge != nil {
		head += conflicts(len(e.Merge.Conflicts))
	} else {
		head += "a conflict"
	}
	if e.Text != "" {
		head += ": " + e.Text
	}
	return head
}

// gateDetail is what the gate row says once the gate has answered.
//
// A gate that is green only because a branch was peeled off leads with that
// branch. The report writes the same fact as a sentence, and puts the name
// sixty characters along — far enough that a narrow terminal clips off the one
// thing a human watching a red gate needs.
func (b *barrier) gateDetail(e drain.Event) string {
	if e.Passed && b.blamed != "" {
		return "green with " + b.blamed + " rolled back"
	}
	verdict := "the gate on the merged result " + passFail(e.Passed)
	if e.Text != "" {
		return verdict + ": " + firstLine(e.Text)
	}
	return verdict
}

// reportGateDetail is the same from the report, for a barrier this view was not
// watching when its gate ran.
func (b *barrier) reportGateDetail(rep *drain.IntegrateReport) string {
	if rep.GatePassed && b.blamed != "" {
		return "green with " + b.blamed + " rolled back"
	}
	if rep.Reason != "" {
		return firstLine(rep.Reason)
	}
	return "the gate on the merged result " + passFail(rep.GatePassed)
}

// bookkeeping is what the barrier put back into bd and what it filed on its
// workers' behalf. Empty is the expected case for both, and an empty string is
// what the block's label wants for it.
func bookkeeping(rep *drain.IntegrateReport) string {
	var out string
	if n := len(rep.Discoveries.Filed); n > 0 {
		out += fmt.Sprintf(" · filed %d discovered", n)
	}
	if !rep.Reconciled.Empty() {
		out += fmt.Sprintf(" · reconciled %d", rep.Reconciled.Total())
	}
	return out
}

// branchOf names the branch a barrier event is about.
func branchOf(e drain.Event) string {
	if e.Merge != nil && e.Merge.Branch != "" {
		return e.Merge.Branch
	}
	if e.Text != "" {
		return e.Text
	}
	return e.Issue
}

func detailOr(e drain.Event, fallback string) string {
	if e.Text != "" {
		return firstLine(e.Text)
	}
	return fallback
}
