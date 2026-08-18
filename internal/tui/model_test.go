package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bd-auto/internal/ask"
	"bd-auto/internal/drain"
	"bd-auto/internal/pipeline"
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

// --- which process is running ---

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// unstyled strips the colour, so a test can measure a column rather than only
// search the whole line for a word that might be in another one.
func unstyled(s string) string { return ansi.ReplaceAllString(s, "") }

// tableLine is one issue's line as it is actually rendered, with the styling
// taken off.
func tableLine(t *testing.T, m *Model, issue string) string {
	t.Helper()
	for _, line := range strings.Split(m.View(), "\n") {
		line = unstyled(line)
		if strings.Contains(line, issue) && !strings.Contains(line, "ISSUE") {
			return line
		}
	}
	t.Fatalf("no line for %s in:\n%s", issue, m.View())
	return ""
}

// stateCell is the STATE column of one issue's line, at the fixed offset the
// header puts it at. Reading it by offset rather than by substring is the
// point: a cell that had overflowed its column would still contain the word.
func stateCell(t *testing.T, m *Model, issue string) string {
	t.Helper()
	line := tableLine(t, m, issue)
	start := 2 + colIssue + 1 + colWave + 1
	if len(line) < start+colState {
		t.Fatalf("the line for %s is too short to have a state column: %q", issue, line)
	}
	return strings.TrimSpace(line[start : start+colState])
}

// An issue is not one process. It is a worker, then a gate, then a reviewer,
// then a worker again — and a row that said "running" through all of it left
// the one fact a watcher wants unanswered.
func TestTheStateCellNamesTheProcessThatIsRunning(t *testing.T) {
	for _, role := range []runner.Role{runner.RoleWorker, runner.RoleReviewer, runner.RoleIntegrator, "security"} {
		m := newTestModel(nil)
		feed(m,
			drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1"}},
			drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"},
			drain.Event{Kind: drain.EventActivity, At: at(1), Wave: 1, Issue: "t-1",
				Role: role, Phase: runner.EventToolUse, Tool: "Edit", Text: "Edit"},
		)
		if got := m.Row("t-1").Doing(); got != string(role) {
			t.Fatalf("the row says %q is running, want %s", got, role)
		}
		if got := stateCell(t, m, "t-1"); got != string(role) {
			t.Fatalf("the state cell says %q, want %s", got, role)
		}
	}
}

// The gate spawns no model and so streams nothing. Before the stage boundaries
// existed the row went on showing the worker's last tool call with the clock
// climbing for the whole of a `go test ./...`, which is indistinguishable from
// a worker that has hung — the one reading this display exists to prevent.
func TestASilentStageIsVisibleAsItselfRatherThanAStalledWorker(t *testing.T) {
	m := newTestModel(nil)
	m.Width = 120
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"},
		drain.Event{Kind: drain.EventActivity, At: at(1), Wave: 1, Issue: "t-1",
			Role: runner.RoleWorker, Phase: runner.EventToolUse, Tool: "Edit", Text: "Edit internal/store/store.go"},
	)
	if got := stateCell(t, m, "t-1"); got != "worker" {
		t.Fatalf("the state cell says %q while the worker runs, want worker", got)
	}

	// Mid-sentence when the stage takes over, which is the ordinary case: the
	// worker's last turn ends with a message, not a tool call.
	feed(m, drain.Event{Kind: drain.EventActivity, At: at(2), Wave: 1, Issue: "t-1",
		Role: runner.RoleWorker, Phase: runner.EventText, Text: "that is the last of it, running the gate"})
	feed(m, drain.Event{Kind: drain.EventStageStart, At: at(2), Wave: 1, Issue: "t-1", Stage: "gate"})
	if got := stateCell(t, m, "t-1"); got != "gate" {
		t.Fatalf("the state cell says %q while the gate runs, want gate", got)
	}
	if got := m.Row("t-1").Detail; strings.Contains(got, "last of it") {
		t.Fatalf("the activity cell still holds the worker's last message (%q); the worker handed over", got)
	}

	// Between two stages nothing in particular is running, and the cell says so
	// rather than keeping the name of the one that just ended.
	feed(m, drain.Event{Kind: drain.EventStageEnd, At: at(3), Wave: 1, Issue: "t-1",
		Stage: "gate", Passed: true})
	if got := stateCell(t, m, "t-1"); got != string(StateRunning) {
		t.Fatalf("the state cell says %q after the gate finished, want running", got)
	}

	// A model stage names its role, which is what a watcher recognises it by —
	// "review" is a stage, "reviewer" is the thing spending the money.
	feed(m, drain.Event{Kind: drain.EventStageStart, At: at(4), Wave: 1, Issue: "t-1",
		Stage: "review", Role: runner.RoleReviewer})
	if got := stateCell(t, m, "t-1"); got != "reviewer" {
		t.Fatalf("the state cell says %q while the reviewer runs, want reviewer", got)
	}

	// A failed stage says the verdict and leaves the reason to the transcript:
	// the feedback on the event is paragraphs written for the worker, and its
	// own opening line already says the stage failed.
	feed(m, drain.Event{Kind: drain.EventStageEnd, At: at(5), Wave: 1, Issue: "t-1",
		Stage: "review", Role: runner.RoleReviewer,
		Text: "The review stage failed. Its findings are your instructions:\n\nthe Store interface is not covered"})
	if got := m.Row("t-1").Detail; got != "the review stage failed" {
		t.Fatalf("the activity cell holds %q, want the verdict alone", got)
	}

	// The worker was part-way through a sentence when it handed over, and the
	// next round's fragments must start a message of their own rather than be
	// appended to the one the stage interrupted.
	feed(m, drain.Event{Kind: drain.EventActivity, At: at(6), Wave: 1, Issue: "t-1",
		Role: runner.RoleWorker, Phase: runner.EventText, Text: "adding the missing case"})
	if got := m.Row("t-1").Detail; got != "adding the missing case" {
		t.Fatalf("the cell holds %q; the stage ended the message before it", got)
	}

	// A terminal row keeps its own word: no process is running to name.
	feed(m, drain.Event{Kind: drain.EventIssueEnd, At: at(9), Wave: 1, Issue: "t-1",
		Outcome: drain.OutcomeDone, Report: &drain.Report{Issue: "t-1"}})
	if got := stateCell(t, m, "t-1"); got != string(StateDone) {
		t.Fatalf("the state cell says %q on a finished row, want done", got)
	}
}

// The cell is one column of a table people read down, so what goes in it has to
// stay in it — and two things outrank the name of the process: a row that is
// dying, and a row waiting on a person.
func TestTheStateCellKeepsItsColumnAndYieldsToKillingAndAsking(t *testing.T) {
	control := newPressed("t-1", "t-2", "t-3")
	m := newTestModel(control)
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1", "t-2", "t-3"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-2"},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-3"},
		// A configured stage may be called anything at all.
		drain.Event{Kind: drain.EventStageStart, At: at(1), Wave: 1, Issue: "t-1",
			Stage: "acceptance-criteria-review"},
		drain.Event{Kind: drain.EventActivity, At: at(1), Wave: 1, Issue: "t-2",
			Role: runner.RoleIntegrator, Phase: runner.EventToolUse, Tool: "Edit", Text: "Edit"},
	)

	// Every line has the columns in the same places, whatever is in the cell.
	// The ISSUE column especially: it is what the eye tracks down the table, and
	// the two-cell marker exists to stop it moving.
	want := strings.Index(tableLine(t, m, "t-3"), "t-3")
	for _, id := range []string{"t-1", "t-2"} {
		if got := strings.Index(tableLine(t, m, id), id); got != want {
			t.Fatalf("%s starts at column %d and t-3 at %d: a longer name must not move ISSUE", id, got, want)
		}
	}
	if got := stateCell(t, m, "t-1"); len(got) > colState {
		t.Fatalf("the state cell holds %q (%d cells), which overflows the %d-cell column", got, len(got), colState)
	}
	if got := stateCell(t, m, "t-1"); !strings.HasPrefix(got, "acceptance") {
		t.Fatalf("a clipped stage name reads %q; enough of it must survive to recognise it", got)
	}
	if got := stateCell(t, m, "t-2"); got != "integrator" {
		t.Fatalf("the state cell says %q, want integrator: the longest built-in role must fit whole", got)
	}

	// A worker waiting on a person is the one state that otherwise looks exactly
	// like a worker that has hung, so it outranks which process is running.
	feed(m, drain.Event{Kind: drain.EventQuestion, At: at(2), Wave: 1, Issue: "t-2",
		Question: &ask.Question{ID: "q1", Issue: "t-2", Text: "which shape?"}})
	if got := stateCell(t, m, "t-2"); got != "asking" {
		t.Fatalf("the state cell says %q on a row waiting for an answer, want asking", got)
	}

	// So does a kill: it takes the grace period, and a row that went on naming
	// the reviewer through it would invite a second keypress.
	special(m, tea.KeyEscape) // drop the question, so the table takes keys again
	key(m, "k")
	if got := stateCell(t, m, "t-1"); got != "killing" {
		t.Fatalf("the state cell says %q on a dying row, want killing", got)
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
	if !strings.Contains(lines[len(lines)-2], "q stop the run") {
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

// A branch whose only conflict was beads' own export cost nothing to merge, and
// the row has to say so: a cost cell reading zero beside "a model resolved it"
// looks like a bug in the accounting rather than a rule doing its job.
func TestASettledExportSaysNoModelRan(t *testing.T) {
	m := newTestModel(nil)
	finishedWave(m, "t-1")
	feed(m, drain.Event{Kind: drain.EventWaveEnd, At: at(30), Wave: 1,
		Integration: &drain.IntegrateReport{GatePassed: true, Merges: []drain.Merge{
			{Issue: "t-1", Outcome: drain.MergeResolved, Settled: []string{".beads/issues.jsonl"}},
		}}})
	if got := m.Row("t-1").Detail; got != "merged; settled 1 conflict in beads' exports, no model" {
		t.Fatalf("t-1 says %q, want the row to say no model resolved it", got)
	}
}

// --- the barrier ---

// finishedWave is a run whose workers are all done, standing at the barrier.
func finishedWave(m *Model, ids ...string) {
	feed(m, drain.Event{Kind: drain.EventRunStart, At: at(0), Text: "epic-1", Issues: ids})
	feed(m, drain.Event{Kind: drain.EventWaveStart, At: at(0), Wave: 1, Issues: ids})
	for _, id := range ids {
		feed(m,
			drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: id},
			drain.Event{Kind: drain.EventIssueEnd, At: at(9), Wave: 1, Issue: id,
				Outcome: drain.OutcomeDone, Text: "finished", Report: &drain.Report{Issue: id}})
	}
	feed(m, drain.Event{Kind: drain.EventWaveIntegrating, At: at(10), Wave: 1, Issues: ids})
}

// branchRow is one barrier row, by the issue whose branch it is.
func branchRow(t *testing.T, m *Model, issue string) *Row {
	t.Helper()
	for _, r := range m.barrierRows() {
		if r.Issue == issue {
			return r
		}
	}
	t.Fatalf("the barrier has no row for %s; it has %d row(s)", issue, len(m.barrierRows()))
	return nil
}

// A merge is work. Each branch gets a row in the same columns the wave above it
// uses, carrying what happened to it, how long it took and what it cost — and a
// row for the gate, which is the longest silent thing a barrier does.
func TestTheBarrierIsARowPerBranchAndOneForTheGate(t *testing.T) {
	m := newTestModel(newPressed())
	finishedWave(m, "t-1", "t-2", "t-3")

	feed(m,
		drain.Event{Kind: drain.EventMergeStart, At: at(10), Wave: 1, Issue: "t-1", Text: "bd-auto/t-1"},
		drain.Event{Kind: drain.EventMergeEnd, At: at(13), Wave: 1, Issue: "t-1",
			Merge: &drain.Merge{Issue: "t-1", Branch: "bd-auto/t-1", Outcome: drain.MergeClean, Seconds: 3}},
		drain.Event{Kind: drain.EventMergeStart, At: at(13), Wave: 1, Issue: "t-2", Text: "bd-auto/t-2"},
		drain.Event{Kind: drain.EventMergeEnd, At: at(14), Wave: 1, Issue: "t-2",
			Merge: &drain.Merge{Issue: "t-2", Branch: "bd-auto/t-2", Outcome: drain.MergeParked, Seconds: 1,
				Reason: "git would not merge bd-auto/t-2\nand left no conflicted paths"}},
		drain.Event{Kind: drain.EventMergeStart, At: at(14), Wave: 1, Issue: "t-3", Text: "bd-auto/t-3"},
		drain.Event{Kind: drain.EventMergeEnd, At: at(15), Wave: 1, Issue: "t-3",
			Merge: &drain.Merge{Issue: "t-3", Branch: "bd-auto/t-3", Outcome: drain.MergeSkipped, Seconds: 1,
				Reason: "integration was interrupted"}},
		drain.Event{Kind: drain.EventWaveGateStart, At: at(15), Wave: 1, Text: "go test ./..."},
	)

	// Every outcome a branch can have, visible as what it is.
	for issue, want := range map[string]State{
		"t-1": StateMerged, "t-2": StateParked, "t-3": StateSkipped,
	} {
		if got := branchRow(t, m, issue).State; got != want {
			t.Fatalf("%s's barrier row is %s, want %s", issue, got, want)
		}
	}
	if got := branchRow(t, m, "t-1").Elapsed(at(30)); got != 3*time.Second {
		t.Fatalf("t-1's merge took %s on screen, want the 3s the merge itself reported", got)
	}

	view := m.View()
	for _, want := range []string{
		"wave 1 barrier",
		// The clean merge, and the one git would not have.
		"clean, no conflicts", "git would not merge bd-auto/t-2",
		// The gate, saying what it is running rather than leaving the screen
		// still for the length of a test suite.
		"gate", "go test ./...",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("the barrier block does not show %q:\n%s", want, view)
		}
	}
	// The wave rows above are untouched by the merges that landed, and moved by
	// the one that did not.
	if got := m.Row("t-1").State; got != StateDone {
		t.Fatalf("t-1 is %s in the wave table, want done: its worker finished", got)
	}
	if got := m.Row("t-2").State; got != StateParked {
		t.Fatalf("t-2 is %s in the wave table, want parked: its branch never landed", got)
	}
}

// The one model a barrier ever spawns belongs on the row of the branch it is
// resolving. That row is what tells a resolving barrier from a hung one, and
// the issue's own row in the wave table finished minutes ago.
func TestAResolvingBarrierShowsTheIntegratorsToolCalls(t *testing.T) {
	m := newTestModel(newPressed())
	finishedWave(m, "t-1", "t-2")

	feed(m,
		drain.Event{Kind: drain.EventMergeStart, At: at(10), Wave: 1, Issue: "t-2", Text: "bd-auto/t-2"},
		drain.Event{Kind: drain.EventMergeConflict, At: at(11), Wave: 1, Issue: "t-2",
			Role: runner.RoleIntegrator, Text: "internal/wave/plan.go",
			Merge: &drain.Merge{Issue: "t-2", Branch: "bd-auto/t-2",
				Conflicts: []string{"internal/wave/plan.go"}}},
	)
	row := branchRow(t, m, "t-2")
	if row.State != StateResolving {
		t.Fatalf("t-2's barrier row is %s, want resolving: a model is on it", row.State)
	}
	if !strings.Contains(row.Detail, "internal/wave/plan.go") {
		t.Fatalf("the row says %q, want the paths the integrator is resolving", row.Detail)
	}

	feed(m, drain.Event{Kind: drain.EventActivity, At: at(20), Wave: 1, Issue: "t-2",
		Role: runner.RoleIntegrator, Phase: runner.EventToolUse, Tool: "Edit",
		Text: "Edit(internal/wave/plan.go)", Usage: runner.Usage{CostUSD: 0.021}})

	if got := branchRow(t, m, "t-2").Detail; got != "Edit(internal/wave/plan.go)" {
		t.Fatalf("the barrier row says %q, want the integrator's live tool call", got)
	}
	if got := branchRow(t, m, "t-2").Cost(); got != 0.021 {
		t.Fatalf("the barrier row has cost %v, want what the integrator has spent so far", got)
	}
	// And not on the worker's row, which is finished and did not run it.
	if got := m.Row("t-2").Detail; got != "finished" {
		t.Fatalf("the wave row says %q, want what its worker last said", got)
	}
	if got := branchRow(t, m, "t-2").Elapsed(at(50)); got != 40*time.Second {
		t.Fatalf("the merge shows %s, want a clock that is still running", got)
	}
}

// A red gate is the interesting case. The gate runs once on everything, and
// when it fails nothing but peeling the merges back can say which branch did
// it — so the block has to show that happening, and name the branch it landed
// on rather than only reporting a verdict.
func TestARedGateShowsTheRollbackAndNamesTheBranchItBlamed(t *testing.T) {
	m := newTestModel(newPressed())
	finishedWave(m, "t-1", "t-2")

	feed(m,
		drain.Event{Kind: drain.EventMergeStart, At: at(10), Wave: 1, Issue: "t-1", Text: "bd-auto/t-1"},
		drain.Event{Kind: drain.EventMergeEnd, At: at(11), Wave: 1, Issue: "t-1",
			Merge: &drain.Merge{Issue: "t-1", Branch: "bd-auto/t-1", Outcome: drain.MergeClean, Seconds: 1}},
		drain.Event{Kind: drain.EventMergeStart, At: at(11), Wave: 1, Issue: "t-2", Text: "bd-auto/t-2"},
		drain.Event{Kind: drain.EventMergeEnd, At: at(12), Wave: 1, Issue: "t-2",
			Merge: &drain.Merge{Issue: "t-2", Branch: "bd-auto/t-2", Outcome: drain.MergeClean, Seconds: 1}},
		drain.Event{Kind: drain.EventWaveGateStart, At: at(12), Wave: 1, Text: "go test ./..."},
		drain.Event{Kind: drain.EventWaveGateEnd, At: at(40), Wave: 1, Passed: false,
			Text: "test failed (exit 1)"},
	)
	if got := branchRow(t, m, gateRowName).State; got != StateFailed {
		t.Fatalf("the gate row is %s, want failed", got)
	}

	// The peel: t-2 comes back off, and the gate is asked again.
	feed(m,
		drain.Event{Kind: drain.EventWaveRollback, At: at(41), Wave: 1, Issue: "t-2",
			Text:  "rolled back to find out what the gate is red on",
			Merge: &drain.Merge{Issue: "t-2", Branch: "bd-auto/t-2"}},
		drain.Event{Kind: drain.EventWaveGateStart, At: at(41), Wave: 1, Text: "go test ./..."},
	)
	if got := branchRow(t, m, "t-2").State; got != StateRolledBack {
		t.Fatalf("t-2's barrier row is %s, want it showing the rollback", got)
	}
	if got := branchRow(t, m, gateRowName).State; got != StateRunning {
		t.Fatalf("the gate row is %s, want it running again on the peeled tree", got)
	}

	feed(m,
		drain.Event{Kind: drain.EventWaveGateEnd, At: at(60), Wave: 1, Passed: true, Text: "test"},
		drain.Event{Kind: drain.EventMergeEnd, At: at(60), Wave: 1, Issue: "t-2",
			Merge: &drain.Merge{Issue: "t-2", Branch: "bd-auto/t-2", Outcome: drain.MergeParked, Seconds: 1,
				Reason: "the wave gate failed on the merged result and went green once " +
					"bd-auto/t-2 was rolled back"}},
		drain.Event{Kind: drain.EventWaveEnd, At: at(61), Wave: 1,
			Integration: &drain.IntegrateReport{Wave: 1, GatePassed: true,
				Reason: "the gate was red on the merged result and is green with bd-auto/t-2 rolled back",
				Gate:   []pipeline.Result{{Name: "test", Passed: true}},
				Merges: []drain.Merge{
					{Issue: "t-1", Branch: "bd-auto/t-1", Outcome: drain.MergeClean, Seconds: 1},
					{Issue: "t-2", Branch: "bd-auto/t-2", Outcome: drain.MergeParked, Seconds: 1,
						Reason: "the wave gate failed on the merged result and went green once " +
							"bd-auto/t-2 was rolled back"},
				}}},
	)

	if got := branchRow(t, m, "t-2").State; got != StateParked {
		t.Fatalf("t-2's barrier row is %s, want parked: the gate blamed it", got)
	}
	if got := m.Row("t-2").State; got != StateParked {
		t.Fatalf("t-2 is %s in the wave table, want parked: its work did not land", got)
	}
	view := m.View()
	// The gate row names the branch that fixed it, on a hundred-column
	// terminal, which is where the report's own sentence about it is clipped.
	if !strings.Contains(view, "green with bd-auto/t-2 rolled back") {
		t.Fatalf("the gate row does not name the branch it blamed:\n%s", view)
	}
	if !strings.Contains(view, "1 merged, 1 parked, gate") {
		t.Fatalf("the block does not say how the barrier went:\n%s", view)
	}
}

// The barrier spends real money and belongs to no issue, so it is shown as a
// figure of its own — and it still has to agree with the run total, which is
// the number the report carries and the one a human reconciles against.
func TestTheBarriersCostIsItsOwnFigureAndStillInTheTotal(t *testing.T) {
	m := newTestModel(newPressed())
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Text: "epic-1", Issues: []string{"t-1"}},
		drain.Event{Kind: drain.EventWaveStart, At: at(0), Wave: 1, Issues: []string{"t-1"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 1, Issue: "t-1"},
		drain.Event{Kind: drain.EventIssueEnd, At: at(9), Wave: 1, Issue: "t-1", Text: "finished",
			Outcome: drain.OutcomeDone, Usage: runner.Usage{CostUSD: 0.50},
			Report: &drain.Report{Issue: "t-1"}},
		drain.Event{Kind: drain.EventWaveIntegrating, At: at(10), Wave: 1, Issues: []string{"t-1"}},
		drain.Event{Kind: drain.EventMergeStart, At: at(10), Wave: 1, Issue: "t-1", Text: "bd-auto/t-1"},
		drain.Event{Kind: drain.EventMergeConflict, At: at(11), Wave: 1, Issue: "t-1",
			Role: runner.RoleIntegrator, Merge: &drain.Merge{Issue: "t-1", Branch: "bd-auto/t-1",
				Conflicts: []string{"a.go"}}},
		drain.Event{Kind: drain.EventActivity, At: at(20), Wave: 1, Issue: "t-1",
			Role: runner.RoleIntegrator, Phase: runner.EventToolUse, Text: "Edit",
			Usage: runner.Usage{CostUSD: 0.02}},
	)

	// Live, from the row of the branch being resolved.
	if got := m.barrierCost(); got != 0.02 {
		t.Fatalf("the barrier has spent %v on screen, want the integrator's running total", got)
	}
	if got := m.Cost(); got != 0.52 {
		t.Fatalf("the run total is %v, want the workers plus the barrier", got)
	}
	if !strings.Contains(m.View(), "barrier $0.0200") {
		t.Fatalf("the summary hides what the barrier cost:\n%s", m.View())
	}

	// Settled, from the report — which is what the run's own total was built
	// from, so the two must not be added together.
	feed(m, drain.Event{Kind: drain.EventWaveEnd, At: at(30), Wave: 1,
		Usage: runner.Usage{CostUSD: 0.03},
		Integration: &drain.IntegrateReport{Wave: 1, GatePassed: true, Merges: []drain.Merge{
			{Issue: "t-1", Branch: "bd-auto/t-1", Outcome: drain.MergeResolved,
				Conflicts: []string{"a.go"}, Usage: runner.Usage{CostUSD: 0.03}},
		}}})
	if got := m.barrierCost(); got != 0.03 {
		t.Fatalf("the barrier has spent %v, want the figure the report carried", got)
	}
	if got := m.Cost(); got != 0.53 {
		t.Fatalf("the run total is %v, want the workers plus the barrier and nothing counted twice", got)
	}
}

// A view can meet a barrier that is already over: a run resumed into, an
// integrate on its own, or a barrier asked to settle every branch the run ever
// touched — which can name issues from waves this view never watched. The block
// is built from the report then, and it says what the barrier did to branches
// the wave table above has no row for at all.
func TestABarrierIsBuiltFromItsReportWhenItsEventsWereMissed(t *testing.T) {
	m := newTestModel(newPressed())
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Text: "epic-1", Issues: []string{"t-1"}},
		drain.Event{Kind: drain.EventWaveStart, At: at(0), Wave: 2, Issues: []string{"t-1"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(0), Wave: 2, Issue: "t-1"},
		drain.Event{Kind: drain.EventIssueEnd, At: at(9), Wave: 2, Issue: "t-1",
			Outcome: drain.OutcomeDone, Text: "finished", Report: &drain.Report{Issue: "t-1"}},
		drain.Event{Kind: drain.EventWaveEnd, At: at(30), Wave: 2,
			Usage: runner.Usage{CostUSD: 0.04},
			Integration: &drain.IntegrateReport{Wave: 2, GatePassed: false,
				Reason: "the gate is red on main with every branch of this wave rolled back",
				Gate:   []pipeline.Result{{Name: "test"}},
				Merges: []drain.Merge{
					{Issue: "t-1", Branch: "bd-auto/t-1", Outcome: drain.MergeClean, Seconds: 2},
					// An earlier wave's branch, which this view never watched a
					// worker for.
					{Issue: "t-9", Branch: "bd-auto/t-9", Outcome: drain.MergeSkipped, Seconds: 1,
						Reason: "integration was interrupted"},
				},
				Discoveries: drain.DiscoveryFiling{Filed: map[string]string{"t-20": "t-1"}, Skipped: 2},
				Reconciled:  drain.Reconciliation{Closed: []string{"t-1"}}}})

	if got := branchRow(t, m, "t-9").State; got != StateSkipped {
		t.Fatalf("t-9's barrier row is %s, want skipped", got)
	}
	if m.Row("t-9") != nil {
		t.Fatal("the barrier invented a wave row for an issue this view never watched")
	}
	if got := branchRow(t, m, "t-1").Elapsed(at(60)); got != 2*time.Second {
		t.Fatalf("t-1's merge shows %s, want the seconds the report carried", got)
	}
	view := m.View()
	for _, want := range []string{
		"t-9", "integration was interrupted",
		// The gate, and the barrier's other two jobs, which are visible only
		// when they had anything to do.
		"gate", "the gate is red on main",
		"filed 1 discovered", "reconciled 1",
		"barrier $0.0400",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("the barrier block does not show %q:\n%s", want, view)
		}
	}
}

// A barrier row is where enter pays off twice: the integrator writes into the
// transcript of the issue whose branch it is merging, so the row of a conflict
// being resolved opens the model that is resolving it. The gate is the one row
// on this screen that has nothing to open.
func TestEnterOnABarrierRowOpensTheIntegratorsTranscript(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "t-2-a1-r0-integrator.jsonl", 1,
		assistantLine(t, textBlock("Both branches added a case to the same switch.")))

	m := newTestModel(newPressed())
	m.RepoRoot = root
	finishedWave(m, "t-1", "t-2")
	feed(m,
		drain.Event{Kind: drain.EventMergeStart, At: at(10), Wave: 1, Issue: "t-2", Text: "bd-auto/t-2"},
		drain.Event{Kind: drain.EventMergeConflict, At: at(11), Wave: 1, Issue: "t-2",
			Role: runner.RoleIntegrator, Merge: &drain.Merge{Issue: "t-2", Branch: "bd-auto/t-2",
				Conflicts: []string{"internal/cli/cli.go"}}},
		drain.Event{Kind: drain.EventWaveGateStart, At: at(12), Wave: 1, Text: "go test ./..."},
	)

	// Down past the two wave rows and the one barrier row before it: the
	// barrier shares the table's cursor rather than having one of its own.
	for i := 0; i < 3; i++ {
		key(m, "down")
	}
	if got := m.Selected(); got == nil || got.Issue != "t-2" || !got.barrier {
		t.Fatalf("the cursor is on %+v, want t-2's barrier row", got)
	}
	special(m, tea.KeyEnter)
	if m.detail == nil {
		t.Fatal("enter on a barrier row opened nothing")
	}
	view := m.View()
	if !strings.Contains(view, "Both branches added a case") {
		t.Fatalf("the transcript is not the integrator's:\n%s", view)
	}
	// The sub-heading is the barrier row's state, not the finished worker's.
	if !strings.Contains(view, "resolving") {
		t.Fatalf("the transcript says nothing about what the barrier is doing:\n%s", view)
	}

	special(m, tea.KeyEsc)
	key(m, "down")
	if got := m.Selected(); got == nil || got.Issue != gateRowName {
		t.Fatalf("the cursor is on %+v, want the gate row", got)
	}
	special(m, tea.KeyEnter)
	if m.detail != nil {
		t.Fatal("enter opened a transcript for the gate, which spawns no model")
	}
	if !strings.Contains(m.status, "no model") {
		t.Fatalf("the view says %q, want it saying why there is nothing to read", m.status)
	}
	// And k on it does not go looking for a worker to kill.
	key(m, "k")
	if !strings.Contains(m.status, "not a worker") {
		t.Fatalf("k on a barrier row said %q", m.status)
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

// --- the transcript view ---

// The transcript fixtures. They are the shapes the shipped adapter writes, one
// JSON object per line, built rather than pasted so that a test asserting on
// what a tool call renders as is asserting on the input it really gets.

func assistantLine(t *testing.T, blocks ...map[string]any) string {
	t.Helper()
	return jsonLine(t, map[string]any{"type": "assistant", "message": map[string]any{"content": blocks}})
}

func userLine(t *testing.T, blocks ...map[string]any) string {
	t.Helper()
	return jsonLine(t, map[string]any{"type": "user", "message": map[string]any{"content": blocks}})
}

func textBlock(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

func toolBlock(name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": "tu-" + name, "name": name, "input": input}
}

func resultBlock(text string, isErr bool) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": "tu", "is_error": isErr, "content": text}
}

func endLine(t *testing.T, subtype string, turns int, cost float64) string {
	t.Helper()
	return jsonLine(t, map[string]any{"type": "result", "subtype": subtype,
		"is_error": subtype != "success", "num_turns": turns, "total_cost_usd": cost,
		"duration_ms": 92000})
}

func jsonLine(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return string(raw)
}

// writeTranscript puts one process's transcript where drain.LogPath would.
//
// nth fixes the modification time, which is how LogFiles orders two processes
// from the same round: an issue's processes are sequential, so the file that
// stopped growing first ran first.
func writeTranscript(t *testing.T, root, name string, nth int, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, ".beads", "auto", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 17, 9, nth, 0, 0, time.UTC)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// openable is a two-issue run with a transcript on disk for the second, so a
// test can press down and then enter.
func openable(t *testing.T, root string) *Model {
	t.Helper()
	m := newTestModel(newPressed("t-1", "t-2"))
	m.RepoRoot = root
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Text: "epic-1", Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventWaveStart, At: at(0), Wave: 1, Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(1), Wave: 1, Issue: "t-1", Text: "the first issue"},
		drain.Event{Kind: drain.EventIssueStart, At: at(1), Wave: 1, Issue: "t-2",
			Text: "internal/cli: the command dispatch table"},
	)
	return m
}

// The whole point of the view: a row is one line, and enter is how a watcher
// asks what the four minutes behind that line were actually spent on.
func TestEnterOpensTheSelectedIssuesTranscript(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "t-2-a1-r0-worker.jsonl", 1,
		assistantLine(t, textBlock("Registering the three commands from an init.")),
		assistantLine(t, toolBlock("Bash", map[string]any{
			"command": "go test ./...", "description": "run the tests"})),
		userLine(t, resultBlock("ok  \tbd-auto/internal/cli\t0.4s", false)),
		assistantLine(t, toolBlock("Edit", map[string]any{
			"file_path": "/tmp/wt/t-2/internal/cli/cli.go", "old_string": "a", "new_string": "b"})),
		userLine(t, resultBlock("String to replace not found", true)),
		endLine(t, "success", 12, 0.4210),
	)
	writeTranscript(t, root, "t-2-a1-r0-review.jsonl", 2,
		assistantLine(t, textBlock("The diff does what the issue asked.")))

	m := openable(t, root)
	key(m, "down")
	special(m, tea.KeyEnter)

	view := m.View()
	for _, want := range []string{
		// Whose transcript it is, and what that issue was given to do.
		"t-2", "internal/cli: the command dispatch table",
		// The prose, as prose.
		"Registering the three commands",
		// The tool call, with what it was called with — which is the thing the
		// live event stream cannot say, because it carries only the name.
		"⏺ Bash(go test ./...)",
		// Its result, indented under it, and a path shortened from the front.
		"⎿", "bd-auto/internal/cli", "⏺ Edit(…/internal/cli/cli.go)",
		"String to replace not found",
		// What the process cost, and the boundary to the one after it.
		"finished · 12 turns · $0.4210",
		"worker · attempt 1 · round 0", "review · attempt 1 · round 0",
		"The diff does what the issue asked.",
		"esc back to the table",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("the transcript does not show %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "ACTIVITY") {
		t.Fatalf("the table is still on screen under the transcript:\n%s", view)
	}

	// esc goes back, and the cursor is where it was left.
	special(m, tea.KeyEsc)
	if !strings.Contains(m.View(), "ACTIVITY") {
		t.Fatalf("esc did not put the table back:\n%s", m.View())
	}
	if got := m.Selected().Issue; got != "t-2" {
		t.Fatalf("the cursor is on %s, want t-2: opening a transcript must not move it", got)
	}
	// And esc closed the transcript rather than stopping the run, which is what
	// it means with the table up.
	if m.stopping {
		t.Fatal("esc out of a transcript stopped the run")
	}
}

// A worker two hours in has written more than a screen. The view has to be able
// to reach both ends of it and to stop at both, or a reader who over-scrolls
// once is looking at a blank pane with no way to tell why.
func TestTheTranscriptScrollsAndClampsAtBothEnds(t *testing.T) {
	root := t.TempDir()
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, assistantLine(t, toolBlock("Read",
			map[string]any{"file_path": fmt.Sprintf("/repo/internal/tui/file%02d.go", i)})))
	}
	writeTranscript(t, root, "t-2-a1-r0-worker.jsonl", 1, lines...)

	m := openable(t, root)
	m.Height = 20
	key(m, "down")
	special(m, tea.KeyEnter)

	// It opens at the end: a transcript is opened to find out what is happening
	// now, and now is the bottom.
	if !strings.Contains(m.View(), "file39.go") {
		t.Fatalf("the transcript did not open at its end:\n%s", m.View())
	}

	key(m, "g")
	view := m.View()
	if !strings.Contains(view, "file00.go") {
		t.Fatalf("g did not reach the top:\n%s", view)
	}
	if !strings.Contains(view, "lines 1-") {
		t.Fatalf("the view does not say where in the transcript it is:\n%s", view)
	}
	// Up, at the top, is a no-op rather than a scroll into nothing.
	special(m, tea.KeyUp)
	if got := m.detail.top; got != 0 {
		t.Fatalf("up at the top scrolled to %d, want 0", got)
	}

	key(m, "G")
	end := m.detail.top
	if end == 0 {
		t.Fatal("G did not move to the end of the transcript")
	}
	special(m, tea.KeyDown)
	if got := m.detail.top; got != end {
		t.Fatalf("down at the end scrolled to %d, want it clamped at %d", got, end)
	}

	// A page moves by nearly a screen, and clamps like everything else.
	special(m, tea.KeyPgUp)
	if m.detail.top >= end {
		t.Fatalf("pgup did not move: top is %d and the end is %d", m.detail.top, end)
	}
	special(m, tea.KeyPgDown)
	if got := m.detail.top; got != end {
		t.Fatalf("pgdn left the window at %d, want the end at %d", got, end)
	}
}

// A tool result is often a whole file. The view keeps its head and says what it
// cut, because a result that stops without saying so reads as a command that
// produced exactly that much output.
func TestALongToolResultIsCutWithAMarker(t *testing.T) {
	root := t.TempDir()
	body := ""
	for i := 0; i < resultLines+5; i++ {
		body += fmt.Sprintf("line %d\n", i)
	}
	writeTranscript(t, root, "t-2-a1-r0-worker.jsonl", 1,
		assistantLine(t, toolBlock("Read", map[string]any{"file_path": "/repo/go.mod"})),
		userLine(t, resultBlock(body, false)))

	m := openable(t, root)
	key(m, "down")
	special(m, tea.KeyEnter)

	view := m.View()
	if !strings.Contains(view, "+5 more lines") {
		t.Fatalf("the cut result does not say what is missing:\n%s", view)
	}
	if strings.Contains(view, fmt.Sprintf("line %d", resultLines+4)) {
		t.Fatalf("the whole result was kept; the view is meant to be bounded:\n%s", view)
	}
}

// An issue queued behind the concurrency cap has no transcript, because nothing
// has been spawned for it. A blank pane there is indistinguishable from a
// broken one.
func TestATranscriptThatDoesNotExistYetSaysSo(t *testing.T) {
	m := openable(t, t.TempDir())
	special(m, tea.KeyEnter)

	view := m.View()
	if !strings.Contains(view, "no model has been spawned for t-1") {
		t.Fatalf("an issue with nothing to read says nothing:\n%s", view)
	}
}

// The table is not paused while a transcript is open — the run is not paused —
// so what arrived meanwhile has to be there on the way back, and the transcript
// itself has to pick up what the worker wrote while it was being read.
func TestTheRunKeepsMovingUnderAnOpenTranscript(t *testing.T) {
	root := t.TempDir()
	path := writeTranscript(t, root, "t-2-a1-r0-worker.jsonl", 1,
		assistantLine(t, toolBlock("Bash", map[string]any{"command": "go build ./..."})))

	m := openable(t, root)
	key(m, "down")
	special(m, tea.KeyEnter)

	// The run carries on underneath.
	feed(m, drain.Event{Kind: drain.EventIssueEnd, At: at(9), Wave: 1, Issue: "t-1",
		Outcome: drain.OutcomeDone, Text: "the first issue landed",
		Report: &drain.Report{Issue: "t-1"}})

	// So does the worker being read. A tick is what follows it: only the bytes
	// appended since the last one are read.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(assistantLine(t, toolBlock("Grep",
		map[string]any{"pattern": "func Dispatch", "path": "/repo/internal/cli"})) + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	m.Update(tickMsg(at(10)))
	if view := m.View(); !strings.Contains(view, "⏺ Grep(func Dispatch in …/repo/internal/cli)") {
		t.Fatalf("the open transcript did not follow the worker:\n%s", view)
	}

	special(m, tea.KeyEsc)
	view := m.View()
	if !strings.Contains(view, "the first issue landed") {
		t.Fatalf("the table lost what arrived while the transcript was open:\n%s", view)
	}
	if got := m.Row("t-1").State; got != StateDone {
		t.Fatalf("t-1 is %s, want done: the table folds events in whatever is on screen", got)
	}
}

// The question box takes enter before anything else does. A human answering a
// prompt must not find the screen replaced by somebody else's transcript.
func TestEnterAnswersAQuestionRatherThanOpeningATranscript(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "t-1-a1-r0-worker.jsonl", 1,
		assistantLine(t, textBlock("this must not be on screen")))

	answers := newAnswered("q1")
	m := openable(t, root)
	m.Ask = answers
	feed(m, drain.Event{Kind: drain.EventQuestion, At: at(2), Wave: 1, Issue: "t-1",
		Question: &ask.Question{ID: "q1", Issue: "t-1", Text: "which shape?",
			Options: []ask.Option{{Label: "a flat object"}, {Label: "an array"}}}})

	special(m, tea.KeyEnter)
	if m.detail != nil {
		t.Fatal("enter opened a transcript over a question waiting for an answer")
	}
	if got := answers.reply("q1"); got != "a flat object" {
		t.Fatalf("enter answered %q, want the option under the cursor", got)
	}
}

// Five workers streaming partial messages for an hour is more text than a view
// has any business holding, so the window keeps the newest entries — and says
// how many it let go, because a transcript that silently starts in the middle
// reads as a worker that started in the middle.
func TestTheTranscriptWindowIsBoundedAndSaysWhatItDropped(t *testing.T) {
	root := t.TempDir()
	var lines []string
	for i := 0; i < entryCap+20; i++ {
		lines = append(lines, assistantLine(t, toolBlock("Read",
			map[string]any{"file_path": fmt.Sprintf("/repo/file%04d.go", i)})))
	}
	writeTranscript(t, root, "t-2-a1-r0-worker.jsonl", 1, lines...)

	m := openable(t, root)
	key(m, "down")
	special(m, tea.KeyEnter)

	// The head entry that names the process costs one of the slots, so 21 of
	// the calls are gone rather than 20.
	if got := m.detail.log.dropped; got != 21 {
		t.Fatalf("the window dropped %d entries, want 21 with a cap of %d", got, entryCap)
	}
	if got := len(m.detail.log.entries); got != entryCap {
		t.Fatalf("the window holds %d entries, want it bounded at %d", got, entryCap)
	}
	key(m, "g")
	view := m.View()
	if !strings.Contains(view, "21 earlier entries dropped off the front") {
		t.Fatalf("the window does not say what it dropped:\n%s", view)
	}
	// The end is what it kept, and the start is what it let go.
	key(m, "G")
	if view := m.View(); !strings.Contains(view, fmt.Sprintf("file%04d.go", entryCap+19)) {
		t.Fatalf("the window kept the wrong end of the transcript:\n%s", view)
	}
}
