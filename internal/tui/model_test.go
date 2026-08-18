package tui

import (
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bd-auto/internal/drain"
	"bd-auto/internal/runner"
)

// --- harness ---

// pressed records what the keys asked the run to do. The whole point of the
// control channel is the effect it has on the engine, so a test that only
// asserted on the rendered text would be asserting on the half that does not
// matter.
//
// It is locked because the real thing is: keys are handled on bubbletea's event
// loop, and a control channel that were not safe to press from there would be a
// data race in the shipped code, not in the double.
type pressed struct {
	mu      sync.Mutex
	killed  []string
	running map[string]bool
	stops   int
}

func newPressed(running ...string) *pressed {
	p := &pressed{running: map[string]bool{}}
	for _, id := range running {
		p.running[id] = true
	}
	return p
}

func (p *pressed) Kill(issue string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running[issue] {
		return false
	}
	p.killed = append(p.killed, issue)
	delete(p.running, issue)
	return true
}

func (p *pressed) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops++
}

// kills is every issue that was killed, in order.
func (p *pressed) kills() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.killed...)
}

// stopped is how many times the run was asked to stop.
func (p *pressed) stopped() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stops
}

var clock = time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)

// at returns a fixed time n seconds into the run, so elapsed columns are exact.
func at(n int) time.Time { return clock.Add(time.Duration(n) * time.Second) }

// feed applies events to a model in order.
func feed(m *Model, events ...drain.Event) {
	for _, e := range events {
		m.Update(eventMsg(e))
	}
}

func key(m *Model, k string) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	return cmd
}

func special(m *Model, t tea.KeyType) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: t})
	return cmd
}

func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func newTestModel(control Stopper) *Model {
	m := NewModel(control)
	m.Now = func() time.Time { return at(30) }
	return m
}

// --- the table ---

// A run's whole shape has to be legible from the table alone: which issues are
// in scope, which wave each is in, what each is doing now, and what it has cost.
func TestTheTableTracksEveryIssueThroughItsStates(t *testing.T) {
	m := newTestModel(newPressed("t-1"))
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Text: "epic-1", Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventWaveStart, At: at(0), Wave: 1, Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(1), Wave: 1, Issue: "t-1", Text: "the first issue"},
		drain.Event{Kind: drain.EventActivity, At: at(2), Wave: 1, Issue: "t-1",
			Phase: runner.EventToolUse, Tool: "Edit", Text: "Edit"},
	)

	if got := m.Row("t-1").State; got != StateRunning {
		t.Fatalf("t-1 is %s, want running", got)
	}
	if got := m.Row("t-2").State; got != StateWaiting {
		t.Fatalf("t-2 is %s: an issue queued behind the concurrency cap is waiting, not running", got)
	}

	view := m.View()
	for _, want := range []string{"epic-1", "wave 1", "t-1", "t-2", "running", "waiting", "Edit"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the table does not show %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "29s") {
		t.Fatalf("the table must show how long t-1 has been running:\n%s", view)
	}

	feed(m,
		drain.Event{Kind: drain.EventIssueEnd, At: at(10), Wave: 1, Issue: "t-1",
			Outcome: drain.OutcomeDone, Report: &drain.Report{Issue: "t-1"}},
		drain.Event{Kind: drain.EventIssueEnd, At: at(12), Wave: 1, Issue: "t-2",
			Outcome: drain.OutcomeParked, Text: "no progress", Report: &drain.Report{Issue: "t-2"}},
	)
	if got := m.Row("t-1").State; got != StateDone {
		t.Fatalf("t-1 is %s, want done", got)
	}
	if got := m.Row("t-2").State; got != StateParked {
		t.Fatalf("t-2 is %s, want parked", got)
	}
	// A finished row stops its clock; a table whose done rows keep counting is
	// unreadable the moment a wave finishes.
	if got := m.Row("t-1").Elapsed(at(600)); got != 9*time.Second {
		t.Fatalf("t-1 ran for %s, want 9s measured between its own events", got)
	}
}

// Cost is the number a human watches to decide whether to press k, so it has to
// be the same number the report ends up carrying. An issue takes several
// processes and each one's usage is a running total that starts again from
// zero, which is the trap this covers.
func TestCostAccumulatesAcrossProcessesAndAcrossTheRun(t *testing.T) {
	m := newTestModel(nil)
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"},

		// First process: a running total, then the event that banks it.
		drain.Event{Kind: drain.EventActivity, At: at(1), Issue: "t-1",
			Phase: runner.EventUsage, Usage: runner.Usage{CostUSD: 0.10}},
		drain.Event{Kind: drain.EventActivity, At: at(2), Issue: "t-1",
			Phase: runner.EventUsage, Usage: runner.Usage{CostUSD: 0.25}},
		drain.Event{Kind: drain.EventActivity, At: at(3), Issue: "t-1",
			Phase: runner.EventDone, Usage: runner.Usage{CostUSD: 0.25}},

		// Second process: counting starts again, and must add rather than replace.
		drain.Event{Kind: drain.EventActivity, At: at(4), Issue: "t-1",
			Phase: runner.EventUsage, Usage: runner.Usage{CostUSD: 0.05}},
	)

	if got := m.Row("t-1").Cost(); got != 0.30 {
		t.Fatalf("t-1 has cost %.4f, want 0.30: the second process must add to the first", got)
	}

	feed(m, drain.Event{Kind: drain.EventWaveEnd, At: at(5), Wave: 1,
		Usage: runner.Usage{CostUSD: 0.02}, Integration: &drain.IntegrateReport{GatePassed: true}})
	if got := m.Cost(); got != 0.32 {
		t.Fatalf("the run total is %.4f, want 0.32: the barrier's own spend belongs to the run", got)
	}

	// The engine's own totals win once they exist: the table is printed above
	// the report and the two must not disagree.
	feed(m,
		drain.Event{Kind: drain.EventIssueEnd, At: at(6), Issue: "t-1", Outcome: drain.OutcomeDone,
			Usage: runner.Usage{CostUSD: 0.31}, Report: &drain.Report{Issue: "t-1"}},
		drain.Event{Kind: drain.EventRunEnd, At: at(7),
			Run: &drain.DrainReport{Outcome: drain.OutcomeDone, Waves: 1, Usage: runner.Usage{CostUSD: 0.33}}},
	)
	if got := m.Row("t-1").Cost(); got != 0.31 {
		t.Fatalf("t-1's settled cost is %.4f, want the engine's 0.31", got)
	}
	if got := m.Cost(); got != 0.33 {
		t.Fatalf("the run total is %.4f, want the report's 0.33", got)
	}
	if !strings.Contains(m.View(), "$0.3300") {
		t.Fatalf("the run total is not on screen:\n%s", m.View())
	}
}

// What --include-partial-messages buys is a row that keeps moving between tool
// calls, which is the difference between a worker that is thinking and one that
// has stalled. A fragment on its own says nothing, so the cell has to rebuild
// the message and show its end.
func TestTheActivityCellFollowsTheMessageBeingWritten(t *testing.T) {
	m := newTestModel(nil)
	m.Width = 120
	feed(m, drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"})

	for _, fragment := range []string{"I will ", "read the ", "config first"} {
		feed(m, drain.Event{Kind: drain.EventActivity, At: at(1), Issue: "t-1",
			Phase: runner.EventText, Text: fragment})
	}
	if got := m.Row("t-1").Detail; got != "I will read the config first" {
		t.Fatalf("the cell holds %q; the fragments must be joined as they arrive", got)
	}

	// A long message shows its end: the newest words are the ones that say what
	// the worker is doing now.
	feed(m, drain.Event{Kind: drain.EventActivity, At: at(2), Issue: "t-1",
		Phase: runner.EventText, Text: strings.Repeat("x", 200) + " the newest words"})
	view := m.View()
	if !strings.Contains(view, "the newest words") {
		t.Fatalf("the table shows the start of the message rather than its end:\n%s", view)
	}
	if got := len([]rune(m.Row("t-1").stream)); got > streamCap {
		t.Fatalf("the rebuilt message is %d runes; it must stay capped at %d", got, streamCap)
	}

	// A tool call ends the message and replaces it, rather than being appended
	// to a sentence that is over.
	feed(m, drain.Event{Kind: drain.EventActivity, At: at(3), Issue: "t-1",
		Phase: runner.EventToolUse, Tool: "Read", Text: "Read"})
	if got := m.Row("t-1").Detail; got != "Read" {
		t.Fatalf("the cell holds %q, want the tool call alone", got)
	}
}

// --- the controls ---

func TestKKillsTheSelectedWorker(t *testing.T) {
	control := newPressed("t-1", "t-2")
	m := newTestModel(control)
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-2"},
	)

	special(m, tea.KeyDown) // t-2
	if cmd := key(m, "k"); quits(cmd) {
		t.Fatal("k kills a worker; it must not leave the view")
	}
	if len(control.kills()) != 1 || control.kills()[0] != "t-2" {
		t.Fatalf("killed %v, want the selected worker t-2", control.kills())
	}
	if control.stopped() != 0 {
		t.Fatal("k must not stop the run")
	}
	if !m.Row("t-2").killing {
		t.Fatal("the row must say it is dying: a kill takes the grace period, and a frozen row invites a second press")
	}
	if !strings.Contains(m.View(), "killing") {
		t.Fatalf("the table does not show the kill in progress:\n%s", m.View())
	}

	// The engine's answer arrives afterwards, and it is what fixes the row.
	feed(m, drain.Event{Kind: drain.EventIssueEnd, At: at(5), Issue: "t-2",
		Outcome: drain.OutcomeFailed, Text: drain.KillReason,
		Report: &drain.Report{Issue: "t-2", Stage: drain.StageKilled}})
	if got := m.Row("t-2").State; got != StateKilled {
		t.Fatalf("t-2 is %s, want killed", got)
	}

	// A second press has nothing to kill, and must say so rather than pretend.
	key(m, "k")
	if len(control.kills()) != 1 {
		t.Fatalf("killed %v: a finished row must not be killed twice", control.kills())
	}
	if !strings.Contains(m.View(), "already") {
		t.Fatalf("the view does not explain the refusal:\n%s", m.View())
	}
}

// q is two-stage because stopping is not instant: a worker in a tool call has
// to be signalled, given its grace and reaped, and a view that vanished on the
// first press would leave you unable to tell a clean stop from a hung one.
func TestQStopsTheRunAndOnlyThenLeaves(t *testing.T) {
	control := newPressed("t-1")
	m := newTestModel(control)
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"},
	)

	if cmd := key(m, "q"); quits(cmd) {
		t.Fatal("the first q must stop the run and keep the view up")
	}
	if control.stopped() != 1 {
		t.Fatalf("the run was stopped %d time(s), want 1", control.stopped())
	}
	if len(control.kills()) != 0 {
		t.Fatal("a stop is not a kill: nothing may be recorded against an issue for it")
	}
	if !strings.Contains(m.View(), "stopping") {
		t.Fatalf("the view does not say the run is stopping:\n%s", m.View())
	}
	if !m.stopping {
		t.Fatal("the model does not remember that a human stopped the run")
	}

	if cmd := key(m, "q"); !quits(cmd) {
		t.Fatal("the second q must leave")
	}
}

func TestCtrlCStopsTheRunLikeQ(t *testing.T) {
	control := newPressed("t-1")
	m := newTestModel(control)
	feed(m, drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"})

	if cmd := special(m, tea.KeyCtrlC); quits(cmd) {
		t.Fatal("ctrl-c must stop the run rather than abandon the view over it")
	}
	if control.stopped() != 1 {
		t.Fatalf("the run was stopped %d time(s), want 1", control.stopped())
	}
}

// A view with nothing to press must say so, and must not pretend a keystroke
// did something.
func TestAViewWithNoControlChannelIsReadOnly(t *testing.T) {
	m := newTestModel(nil)
	feed(m, drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"})

	key(m, "k")
	if m.Row("t-1").killing {
		t.Fatal("a read-only view must not mark a row as dying")
	}
	if !strings.Contains(m.View(), "cannot stop the run") {
		t.Fatalf("the view does not admit it is read-only:\n%s", m.View())
	}
	if cmd := key(m, "q"); !quits(cmd) {
		t.Fatal("with nothing to stop, q just leaves")
	}
}

// The run ending is what closes the view: the last frame is the finished table,
// which is the thing worth leaving in the scrollback.
func TestTheViewClosesWhenTheRunEnds(t *testing.T) {
	m := newTestModel(newPressed())
	_, cmd := m.Update(finishedMsg{})
	if !quits(cmd) {
		t.Fatal("the view must leave once the run is over")
	}
	if !strings.Contains(m.View(), "the run is over") {
		t.Fatalf("the last frame does not say the run finished:\n%s", m.View())
	}
}

// A scoped issue parked before dispatch never gets a worker, and must still
// appear: it is a result, and a table that hid it would be the one place a human
// could not see that work was dropped.
func TestAnIssueParkedBeforeDispatchIsStillOnTheTable(t *testing.T) {
	m := newTestModel(newPressed())
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventScopeParked, At: at(0), Issue: "t-2",
			Text: "dependency t-9 is out of scope"},
	)
	if got := m.Row("t-2").State; got != StateParked {
		t.Fatalf("t-2 is %s, want parked", got)
	}
	if !strings.Contains(m.View(), "out of scope") {
		t.Fatalf("the table does not say why t-2 was parked:\n%s", m.View())
	}
}

// The table has to fit the terminal it is in: a wrapped row makes a five-wide
// wave unreadable.
func TestRowsAreClippedToTheTerminalWidth(t *testing.T) {
	m := newTestModel(newPressed())
	m.Update(tea.WindowSizeMsg{Width: 70})
	feed(m,
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "an-issue-with-a-very-long-identifier"},
		drain.Event{Kind: drain.EventActivity, At: at(1), Issue: "an-issue-with-a-very-long-identifier",
			Phase: runner.EventToolUse, Text: strings.Repeat("long activity ", 20)},
	)
	for _, line := range strings.Split(m.View(), "\n") {
		if len([]rune(line)) > 70 {
			t.Fatalf("a %d-cell line in a 70-cell terminal:\n%s", len([]rune(line)), line)
		}
	}
}

// bubbletea writes the final frame on its way out and then erases the line the
// cursor is left on. That line is the view's last, so a view that ends on
// content ends every run by discarding it — and the content here is the key
// line, which is the half of the display that says what can be pressed.
func TestTheViewEndsOnALineThatIsSafeToErase(t *testing.T) {
	m := newTestModel(newPressed("t-1"))
	feed(m, drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"})

	view := m.View()
	if !strings.HasSuffix(view, "\n") {
		t.Fatalf("the view does not end on a newline, so the renderer's exit will eat the key line:\n%q",
			lastLine(view))
	}
	lines := strings.Split(view, "\n")
	if got := lines[len(lines)-1]; got != "" {
		t.Fatalf("the last line is %q, want it empty and expendable", got)
	}
	if !strings.Contains(lines[len(lines)-2], "kill the selected worker") {
		t.Fatalf("the key line is not the last thing rendered:\n%s", view)
	}
}

// The finished view is the last thing the table ever says, so it must not offer
// a key: finishing quits the program in the same update that sets the state, so
// there is nobody left to press one.
func TestTheFinishedViewOffersNoKeystroke(t *testing.T) {
	m := newTestModel(newPressed())
	m.Update(finishedMsg{})
	if got := m.keys(); got != "the run is over" {
		t.Fatalf("keys() = %q, want a statement rather than an invitation", got)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// A worker finishing is not the same as its work landing. The barrier can park
// an issue whose worker was done, gated and reviewed, and the table has to say
// so — otherwise the run ends with a summary line contradicting the verdict
// printed directly beneath it.
func TestTheBarrierMovesTheRowsItParked(t *testing.T) {
	m := newTestModel(nil)
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventWaveStart, At: at(0), Wave: 1, Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-2"},
		drain.Event{Kind: drain.EventIssueEnd, At: at(9), Issue: "t-1", Outcome: drain.OutcomeDone,
			Report: &drain.Report{Issue: "t-1"}},
		drain.Event{Kind: drain.EventIssueEnd, At: at(9), Issue: "t-2", Outcome: drain.OutcomeDone,
			Report: &drain.Report{Issue: "t-2"}},
	)

	feed(m, drain.Event{Kind: drain.EventWaveEnd, At: at(10), Wave: 1,
		Integration: &drain.IntegrateReport{GatePassed: true, Merges: []drain.Merge{
			{Issue: "t-1", Branch: "bd-auto/t-1", Outcome: drain.MergeClean},
			{Issue: "t-2", Branch: "bd-auto/t-2", Outcome: drain.MergeParked,
				Reason: "git would not merge bd-auto/t-2\nand left no conflicted paths"},
			// A branch from a wave this view never watched, which a barrier
			// asked to settle everything can name.
			{Issue: "t-9", Branch: "bd-auto/t-9", Outcome: drain.MergeParked, Reason: "not this run's"},
		}}})

	if got := m.Row("t-1").State; got != StateDone {
		t.Fatalf("t-1 is %s, want done: it merged", got)
	}
	if got := m.Row("t-2").State; got != StateParked {
		t.Fatalf("t-2 is %s, want parked: the barrier would not merge it", got)
	}
	if got := m.Row("t-2").Detail; got != "git would not merge bd-auto/t-2" {
		t.Fatalf("t-2 says %q, want the merge's reason, first line only", got)
	}
	if m.Row("t-9") != nil {
		t.Fatal("the barrier invented a row for an issue this view never watched")
	}

	view := m.View()
	if !strings.Contains(view, "1 done") || !strings.Contains(view, "1 parked") {
		t.Fatalf("the summary disagrees with the barrier:\n%s", view)
	}
}

// A barrier is not instant. It merges in order, and a conflict spawns a model
// that can run for minutes — during which every worker is finished and the
// table, left to itself, is indistinguishable from a run that has hung.
func TestTheBarrierSaysItIsWorking(t *testing.T) {
	m := newTestModel(nil)
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventWaveStart, At: at(0), Wave: 1, Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-2"},
		drain.Event{Kind: drain.EventIssueEnd, At: at(9), Issue: "t-1", Outcome: drain.OutcomeDone,
			Report: &drain.Report{Issue: "t-1"}},
		drain.Event{Kind: drain.EventIssueEnd, At: at(9), Issue: "t-2", Outcome: drain.OutcomeDone,
			Report: &drain.Report{Issue: "t-2"}},
		drain.Event{Kind: drain.EventWaveIntegrating, At: at(10), Wave: 1, Issues: []string{"t-1", "t-2"}},
	)
	if got := m.View(); !strings.Contains(got, "wave 1 integrating: merging 2 branches") {
		t.Fatalf("the barrier is invisible:\n%s", got)
	}

	// The integrator's activity belongs to the row whose branch it is merging.
	// That row is finished, and its state must stay finished: what is running
	// is the barrier, not the worker.
	feed(m, drain.Event{Kind: drain.EventActivity, At: at(11), Wave: 1, Issue: "t-2",
		Role: runner.RoleIntegrator, Phase: runner.EventToolUse, Text: "Edit internal/cli/cli.go"})
	if got := m.Row("t-2").Detail; got != "Edit internal/cli/cli.go" {
		t.Fatalf("t-2 says %q, want the integrator's tool call", got)
	}
	if got := m.Row("t-2").State; got != StateDone {
		t.Fatalf("t-2 is %s, want done: the barrier is what is running, not the worker", got)
	}

	feed(m, drain.Event{Kind: drain.EventWaveEnd, At: at(30), Wave: 1,
		Integration: &drain.IntegrateReport{GatePassed: true, Merges: []drain.Merge{
			{Issue: "t-1", Outcome: drain.MergeClean},
			{Issue: "t-2", Outcome: drain.MergeResolved,
				Conflicts: []string{"internal/cli/cli.go", "internal/cli/cli_test.go"}},
		}}})
	if got := m.Row("t-2").Detail; got != "merged; a model resolved 2 conflicts" {
		t.Fatalf("t-2 says %q, want what the barrier did rather than the last tool call it ran", got)
	}
}

// A wave grows: a worker that finishes frees a slot, and the run puts an issue
// bd has just offered into it. The table has to show that row as part of the
// wave it joined, not as a worker appearing from nowhere in no wave at all.
func TestAToppedUpIssueShowsAsPartOfTheWaveItJoined(t *testing.T) {
	m := newTestModel(newPressed())
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Text: "epic-1",
			Issues: []string{"t-1", "t-2", "t-3"}},
		drain.Event{Kind: drain.EventWaveStart, At: at(0), Wave: 1, Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(1), Wave: 1, Issue: "t-1"},
		drain.Event{Kind: drain.EventIssueStart, At: at(1), Wave: 1, Issue: "t-2"},
		drain.Event{Kind: drain.EventIssueEnd, At: at(5), Wave: 1, Issue: "t-1",
			Outcome: drain.OutcomeParked, Report: &drain.Report{Issue: "t-1"}},
		// t-3 was never in the wave-start list; it joined the freed slot.
		drain.Event{Kind: drain.EventIssueStart, At: at(6), Wave: 1, Issue: "t-3",
			Text: "the third issue"},
	)

	r := m.Row("t-3")
	if r.Wave != 1 {
		t.Fatalf("t-3 shows wave %d, want the wave it joined", r.Wave)
	}
	if r.State != StateRunning {
		t.Fatalf("t-3 is %s, want running", r.State)
	}
	if r.Title != "the third issue" {
		t.Fatalf("t-3's title is %q; a joined row carries the same detail as any other", r.Title)
	}
	if !strings.Contains(m.View(), "t-3") {
		t.Fatalf("the table does not show the topped-up issue:\n%s", m.View())
	}
}
