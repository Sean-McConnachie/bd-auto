package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"bd-auto/internal/ask"
	"bd-auto/internal/drain"
	"bd-auto/internal/runner"
)

// tick is how often the view redraws on its own, for the elapsed clocks. It is
// slow enough to cost nothing and fast enough that a stuck worker's timer is
// visibly still moving, which is half of what the display is for.
const tick = 500 * time.Millisecond

// State is what one issue is doing, as the table shows it.
type State string

// The row states. They are the display's own vocabulary rather than
// drain.Outcome, because a row spends most of its life in states an outcome has
// no name for: queued behind the concurrency cap, or running.
//
// StateRunning is the least of them, and deliberately so. An issue takes
// several processes — a worker, a gate, a reviewer, whatever else the pipeline
// names — and which one is in flight is what a watcher actually wants; so the
// running cell is written from Row.Doing rather than from this constant
// wherever the row knows. StateRunning is the fallback for the moments in
// between, where nothing has said yet.
const (
	StateWaiting     State = "waiting"
	StateRunning     State = "running"
	StateDone        State = "done"
	StateParked      State = "parked"
	StateFailed      State = "failed"
	StateKilled      State = "killed"
	StateInterrupted State = "stopped"
)

// terminal reports whether a row has finished for good.
func (s State) terminal() bool {
	switch s {
	case StateDone, StateParked, StateFailed, StateKilled, StateInterrupted,
		StateMerged, StateSkipped, StatePassed:
		return true
	}
	return false
}

// Row is one issue's line in the wave table.
type Row struct {
	Issue string
	Title string
	Wave  int
	State State
	// Detail is the last thing that happened: a tool call while running, the
	// reason once it is over.
	Detail  string
	Started time.Time
	Ended   time.Time

	// Role is the role of the process in flight, and Stage the pipeline stage
	// it belongs to. Both are cleared at every boundary rather than left to go
	// stale: a row that keeps naming the reviewer after the review ended is a
	// worse lie than one that admits it only knows the issue is running.
	//
	// They are two fields because a stage need not have a role at all. The gate
	// and a run: stage are bd-auto executing commands, with no model anywhere,
	// and the stage's own name is the only thing they can be called.
	Role  runner.Role
	Stage string

	// stream is the message the model is part-way through writing, rebuilt from
	// the fragments as they arrive.
	stream string

	// logIssue is whose transcript enter opens on this row: the issue itself
	// for a worker's row and for a branch's row at the barrier, since the
	// integrator writes into the transcript of the issue whose branch it is
	// merging. Empty for the gate, which spawns no model and has nothing to
	// read.
	logIssue string
	// barrier marks a row in a barrier block rather than in the wave table.
	// Those rows are not workers: nothing can kill one, and nothing counts one
	// towards the run's tally of issues.
	barrier bool

	// killing is set the moment k is pressed, so the row says so before the
	// worker has finished dying. A kill of a `go test ./...` takes the grace
	// period, and a display that looks frozen for five seconds invites a second
	// keypress.
	killing bool

	// asking is set while this issue has a question waiting for an answer. The
	// row shows it, because a worker blocked on a human is the one state that
	// otherwise looks exactly like a worker that has hung — same spinner, same
	// clock climbing, nothing happening.
	asking bool

	// settled is what this issue's finished processes cost, live what the one
	// in flight has cost so far, and total what the engine finally reported.
	// Three fields because an activity event's usage is a per-process running
	// total: an issue takes several processes, and each starts counting again.
	settled runner.Usage
	live    runner.Usage
	total   runner.Usage
	final   bool
}

// Doing names the process this issue is running now: worker, reviewer,
// integrator, a role the config named for a stage of its own — or, where the
// stage runs no model, the stage. Empty between two of them, and before the
// first has said anything.
func (r *Row) Doing() string {
	if r.Role != "" {
		return string(r.Role)
	}
	return r.Stage
}

// Cost is what this issue has cost so far.
func (r *Row) Cost() float64 {
	if r.final {
		return r.total.CostUSD
	}
	return r.settled.CostUSD + r.live.CostUSD
}

// streamCap is how much of the message being written is kept. It only has to
// outlast the widest terminal anyone renders into; the rest is scrollback the
// table is not trying to be.
const streamCap = 400

// activity folds one live event into the row's activity cell.
//
// A message arrives token by token, so a fragment on its own says nothing — the
// cell has to accumulate them and show the end of what has been written so far.
// That is the whole benefit of --include-partial-messages here: between two tool
// calls the row keeps moving, and a worker that has genuinely stalled is the one
// that stops. Anything that is not a fragment ends the message and replaces it.
func (r *Row) activity(e drain.Event) {
	if e.Fragment() {
		r.stream += e.Text
		if runes := []rune(r.stream); len(runes) > streamCap {
			r.stream = string(runes[len(runes)-streamCap:])
		}
		r.Detail = r.stream
		return
	}
	r.stream = ""
	if e.Text != "" {
		r.Detail = e.Text
	}
}

// say replaces the activity cell with something that did not come from a model.
//
// It ends the message being streamed as well, because the worker has handed
// over: leaving the half-written sentence in place would let the next round's
// fragments be appended to it, and the cell would show one message made out of
// two.
func (r *Row) say(detail string) {
	r.stream = ""
	r.Detail = detail
}

// Elapsed is how long this issue has been running, or ran for.
func (r *Row) Elapsed(now time.Time) time.Duration {
	if r.Started.IsZero() {
		return 0
	}
	if !r.Ended.IsZero() {
		return r.Ended.Sub(r.Started)
	}
	return now.Sub(r.Started)
}

// Stopper is the half of drain.Control the view is allowed to use, and the
// whole of what a keystroke can do to a run.
//
// It is an interface rather than the concrete type for two reasons: the view
// genuinely needs nothing else, and a table driven by keystrokes is only worth
// having if a test can press the keys and see what the run was asked to do.
type Stopper interface {
	// Kill ends one worker, reporting whether there was one to end.
	Kill(issue string) bool
	// Stop ends the run, leaving every worktree, branch and session in place.
	Stop()
}

// Model is the wave table. It is a plain bubbletea model over drain events, so
// every property of the display is testable by feeding it events and reading
// View back, with no terminal anywhere.
type Model struct {
	// Control is the run's stop switch. Nil makes the view read-only, which is
	// the honest thing to show when there is nothing to press.
	Control Stopper
	// Ask is where an answer goes back. Nil means questions are shown but
	// cannot be answered from here, which is what a view attached to somebody
	// else's run would get.
	Ask ask.Responder
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// RepoRoot is the MAIN checkout, which is where the run keeps the
	// transcripts the detail view reads. Empty resolves them relative to the
	// working directory, which is where they are for a view started in the
	// repo it is watching.
	RepoRoot string
	// Width and Height are the render size. Zero means the defaults until the
	// terminal says otherwise.
	Width  int
	Height int

	epic string
	wave int
	// lane says this run schedules continuously: one wave for its whole life,
	// and merges arriving beside the workers rather than at a barrier between
	// them. See drain.Event.Lane.
	lane bool

	order  []string
	rows   map[string]*Row
	cursor int
	// tableTop is the first table line on screen. It is state rather than a
	// computation because a reader who has scrolled up expects to stay there
	// while rows change under them; it is re-clamped on every render, since the
	// terminal can be resized and rows can arrive with nobody pressing a key.
	tableTop int

	// detail is the transcript on screen, or nil when the table is. It is a
	// whole screen rather than a pane: a transcript is read rather than
	// watched, and half a screen of one is not worth the half of the table it
	// would cost.
	detail *detail

	// asking is the queue of unanswered questions, oldest first. Only the head
	// is on screen: several workers may ask at once, and a display that showed
	// them all would be a form nobody could answer one field of.
	asking []ask.Question
	// choice is the cursor within the current question's options, typing
	// whether the human is writing their own answer, and typed what they have
	// written so far.
	choice int
	typing bool
	typed  string

	// barriers is a block per wave barrier, under the table and sharing its
	// cursor. A barrier is work — minutes of it, and real money — and it is
	// shown as rows for the same reason a worker is.
	barriers []*barrier
	// report is the run's own total, once there is one.
	report *drain.DrainReport

	status   string
	stopping bool
	finished bool
	quitting bool
}

// DefaultWidth and DefaultHeight are what the view is laid out for before the
// terminal says what size it is.
const (
	DefaultWidth  = 100
	DefaultHeight = 30
)

// NewModel returns an empty wave table.
func NewModel(control Stopper) *Model {
	return &Model{Control: control, rows: map[string]*Row{}, Width: DefaultWidth}
}

func (m *Model) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// --- messages ---

// eventMsg is one drain event delivered to the program.
type eventMsg drain.Event

// finishedMsg says the run is over and there will be no more events.
type finishedMsg struct{}

// tickMsg drives the elapsed clocks.
type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(tick, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd { return tickCmd() }

// --- update ---

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.Width = msg.Width
		}
		if msg.Height > 0 {
			m.Height = msg.Height
		}
	case tickMsg:
		if m.quitting {
			return m, nil
		}
		// The tick is what keeps an open transcript live. Nothing is re-read:
		// each file is followed from a byte offset, so this is a stat and the
		// lines the worker wrote in the last half second.
		m.refreshDetail()
		return m, tickCmd()
	case eventMsg:
		m.apply(drain.Event(msg))
	case finishedMsg:
		m.finished = true
		m.quitting = true
		return m, tea.Quit
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

// key handles one keystroke.
//
// q is two-stage on purpose. The first press stops the run and keeps the view
// up, because stopping is not instant — a worker in the middle of a tool call
// has to be signalled, given its grace period and reaped — and a display that
// vanishes the moment you ask leaves you unable to tell a clean stop from a
// hung one. The second press leaves anyway.
func (m *Model) key(msg tea.KeyMsg) tea.Cmd {
	// A question on screen takes the keys first, and takes nearly all of them:
	// a human answering a prompt must not find that the digit they typed was a
	// shortcut, or that k killed the worker waiting on their answer. Ctrl-C is
	// the exception, because a way out has to work from everywhere.
	if m.Question() != nil && msg.String() != "ctrl+c" {
		if m.askKey(msg) {
			return nil
		}
	}
	// An open transcript takes the rest. Ctrl-C is the exception again, and it
	// closes the transcript on its way to the run: a view that quit with the
	// table hidden would leave the last frame showing somebody's scrollback.
	if m.detail != nil && msg.String() != "ctrl+c" {
		m.detailKey(msg)
		return nil
	}
	switch msg.String() {
	case "up", "shift+tab":
		m.move(-1)
	case "down", "tab":
		m.move(1)
	case "enter":
		m.open()
	case "k":
		m.kill()
	case "q", "ctrl+c", "esc":
		m.detail = nil
		return m.stop()
	}
	return nil
}

// stop is the q path: ask the run to stop, and leave on the second press.
func (m *Model) stop() tea.Cmd {
	if m.finished || m.stopping || m.Control == nil {
		m.quitting = true
		return tea.Quit
	}
	m.stopping = true
	m.Control.Stop()
	m.status = "stopping: the workers are being signalled. Nothing is parked and " +
		"every worktree, branch and session is kept — re-run drain to resume. q again to leave."
	return nil
}

func (m *Model) move(by int) {
	n := len(m.nav())
	if n == 0 {
		return
	}
	m.cursor += by
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= n {
		m.cursor = n - 1
	}
}

// kill ends the selected worker.
func (m *Model) kill() {
	row := m.Selected()
	if row == nil {
		return
	}
	if row.barrier {
		// The barrier is the run itself, not a worker inside it. q stops the
		// run, which is the only thing that can end one.
		m.status = "the barrier is not a worker: there is nothing here to kill"
		return
	}
	if m.Control == nil {
		m.status = "this view has no control channel: nothing to kill"
		return
	}
	if row.State.terminal() {
		m.status = fmt.Sprintf("%s is already %s", row.Issue, row.State)
		return
	}
	if !m.Control.Kill(row.Issue) {
		m.status = fmt.Sprintf("%s has no worker to kill", row.Issue)
		return
	}
	row.killing = true
	m.status = fmt.Sprintf("killing %s: the worker and everything it started. "+
		"The issue will be parked and reported failed.", row.Issue)
}

// --- questions ---

// Question is the question on screen, or nil when there is none.
func (m *Model) Question() *ask.Question {
	if len(m.asking) == 0 {
		return nil
	}
	return &m.asking[0]
}

// Waiting is how many questions are queued behind the one on screen.
func (m *Model) Waiting() int { return maxInt(len(m.asking)-1, 0) }

// Typed is what the human has written so far, for a test that presses keys.
func (m *Model) Typed() (string, bool) { return m.typed, m.typing }

// askKey handles a keystroke while a question is up, reporting whether it took
// it.
//
// Two modes, because a question has two kinds of answer. Choosing from the
// options is a cursor and a digit; writing your own is a text field, and while
// it is open every printable key belongs to it — including the digits, which is
// the whole reason the modes are separate rather than one screen where some
// keys mean two things.
func (m *Model) askKey(msg tea.KeyMsg) bool {
	q := m.Question()
	if m.typing {
		return m.typeKey(msg, q)
	}

	switch msg.String() {
	case "up", "shift+tab":
		m.moveChoice(-1)
		return true
	case "down", "tab":
		m.moveChoice(1)
		return true
	case "enter":
		if len(q.Options) == 0 {
			m.startTyping()
			return true
		}
		m.answer(q.Options[m.choice].Label)
		return true
	case "t":
		m.startTyping()
		return true
	case "s":
		m.decline(q)
		return true
	case "esc":
		// Dismissing is declining. Leaving the question queued but hidden would
		// hold the worker until the timeout for no reason a reader could see.
		m.decline(q)
		return true
	}

	// A digit picks an option by the number printed beside it, so the list can
	// be answered without moving a cursor through it first.
	if n := digit(msg.String()); n >= 1 && n <= len(q.Options) {
		m.answer(q.Options[n-1].Label)
		return true
	}

	// Everything else is swallowed rather than handed to the table underneath.
	// The table's keys destroy things — k ends a worker, q ends the run — and
	// the worker they would land on is the one waiting for this answer.
	m.status = "answer the question first: " + m.askKeys(len(q.Options))
	return true
}

// typeKey handles a keystroke while the human is writing their own answer.
func (m *Model) typeKey(msg tea.KeyMsg, q *ask.Question) bool {
	switch msg.Type {
	case tea.KeyRunes, tea.KeySpace:
		m.typed += string(msg.Runes)
		if msg.Type == tea.KeySpace && len(msg.Runes) == 0 {
			m.typed += " "
		}
		return true
	case tea.KeyBackspace:
		if r := []rune(m.typed); len(r) > 0 {
			m.typed = string(r[:len(r)-1])
		}
		return true
	case tea.KeyEnter:
		if strings.TrimSpace(m.typed) == "" {
			// An empty answer is not an answer. Fall back to the options rather
			// than sending the worker a blank line.
			m.typing = false
			m.status = "nothing typed: pick an option, t to type again, or s to let it decide"
			return true
		}
		m.answer(m.typed)
		return true
	case tea.KeyEsc:
		m.typing, m.typed = false, ""
		return true
	}
	return true
}

func (m *Model) startTyping() {
	m.typing, m.typed = true, ""
	m.status = "type an answer · enter sends it · esc goes back to the options"
}

func (m *Model) moveChoice(by int) {
	q := m.Question()
	if q == nil || len(q.Options) == 0 {
		return
	}
	m.choice += by
	if m.choice < 0 {
		m.choice = 0
	}
	if m.choice >= len(q.Options) {
		m.choice = len(q.Options) - 1
	}
}

// answer sends a reply and moves on to whatever is queued behind it.
//
// The question is dropped here rather than waiting for the run's own answer
// event to come back, because the human has to be able to type the next one
// immediately: a form that stays on screen after you have answered it is a form
// you answer twice.
func (m *Model) answer(text string) {
	q := m.Question()
	if q == nil {
		return
	}
	if m.Ask == nil {
		m.status = "this view has no channel back to the run: the question cannot be answered here"
		return
	}
	if !m.Ask.Reply(q.ID, text) {
		m.status = fmt.Sprintf("%s is no longer waiting for an answer", q.Issue)
	} else {
		m.status = fmt.Sprintf("answered %s: %s", q.Issue, firstLine(text))
	}
	m.dropQuestion(q.ID)
}

// decline hands the question back to the model, which is a real answer: it is
// told to decide for itself and to write down what it assumed.
func (m *Model) decline(q *ask.Question) {
	if m.Ask != nil {
		m.Ask.Decline(q.ID)
		m.status = fmt.Sprintf("%s was told to decide for itself and record the assumption", q.Issue)
	} else {
		m.status = "this view has no channel back to the run: the question cannot be answered here"
	}
	m.dropQuestion(q.ID)
}

// dropQuestion takes a question off the queue and resets the input for the next.
func (m *Model) dropQuestion(id string) {
	for i, q := range m.asking {
		if q.ID != id {
			continue
		}
		m.asking = append(m.asking[:i], m.asking[i+1:]...)
		if r := m.rows[q.Issue]; r != nil {
			r.asking = m.hasQuestion(q.Issue)
		}
		break
	}
	m.choice, m.typing, m.typed = 0, false, ""
}

// hasQuestion reports whether an issue still has one queued.
func (m *Model) hasQuestion(issue string) bool {
	for _, q := range m.asking {
		if q.Issue == issue {
			return true
		}
	}
	return false
}

func digit(s string) int {
	if len(s) != 1 || s[0] < '1' || s[0] > '9' {
		return 0
	}
	return int(s[0] - '0')
}

// Selected is the row the cursor is on, or nil when the table is empty.
func (m *Model) Selected() *Row {
	rows := m.nav()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return nil
	}
	return rows[m.cursor]
}

// nav is every row the cursor can be on: the wave table, then each barrier's
// block. One cursor space rather than two, because the barrier is part of the
// same table — and because a barrier row is worth selecting, since enter on one
// opens the integrator's transcript.
func (m *Model) nav() []*Row {
	out := make([]*Row, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.rows[id])
	}
	return append(out, m.barrierRows()...)
}

// Row returns one issue's row, for tests and for the summary line.
func (m *Model) Row(issue string) *Row { return m.rows[issue] }

// Rows returns the table in display order.
func (m *Model) Rows() []*Row {
	out := make([]*Row, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.rows[id])
	}
	return out
}

// row finds or creates an issue's row.
func (m *Model) row(issue string) *Row {
	if r, ok := m.rows[issue]; ok {
		return r
	}
	r := &Row{Issue: issue, State: StateWaiting, Wave: m.wave, logIssue: issue}
	m.rows[issue] = r
	m.order = append(m.order, issue)
	return r
}

// apply folds one event into the table.
func (m *Model) apply(e drain.Event) {
	switch e.Kind {
	case drain.EventRunStart:
		m.epic = e.Text
		for _, id := range e.Issues {
			m.row(id)
		}
	case drain.EventScopeParked:
		r := m.row(e.Issue)
		r.State, r.Detail, r.final = StateParked, e.Text, true

	case drain.EventWaveStart:
		m.wave, m.lane = e.Wave, m.lane || e.Lane
		for _, id := range e.Issues {
			r := m.row(id)
			r.Wave = e.Wave
			if !r.State.terminal() {
				r.State, r.Detail = StateWaiting, "queued"
			}
		}
	case drain.EventIssueStart:
		r := m.row(e.Issue)
		r.Wave, r.State, r.Title = e.Wave, StateRunning, e.Text
		r.Started, r.Detail = e.At, "started"
		r.Role, r.Stage = "", ""

	case drain.EventActivity:
		// The barrier takes it first where it has a row for this branch in
		// flight: a run's workers are all finished by then, so activity tagged
		// with an issue is the integrator's, and the branch's row is the only
		// place it means anything.
		if r := m.resolving(e.Issue); r != nil {
			if e.Role != "" {
				r.Role = e.Role
			}
			r.activity(e)
			m.accrue(r, e)
			return
		}
		r := m.row(e.Issue)
		if r.State == StateWaiting {
			r.State = StateRunning
			if r.Started.IsZero() {
				r.Started = e.At
			}
		}
		if e.Role != "" {
			r.Role = e.Role
		}
		r.activity(e)
		m.accrue(r, e)

	case drain.EventStageStart:
		// This is the only thing a silent stage ever says, so it has to settle
		// both cells: the state, and an activity cell still holding whatever
		// tool the worker called last before it handed over.
		r := m.row(e.Issue)
		r.Role, r.Stage = e.Role, e.Stage
		if !r.State.terminal() {
			r.State = StateRunning
			r.say("the " + e.Stage + " stage is running")
		}
	case drain.EventStageEnd:
		r := m.row(e.Issue)
		r.Role, r.Stage = "", ""
		if !r.State.terminal() {
			r.say(stageDetail(e))
		}

	case drain.EventQuestion:
		if e.Question != nil {
			m.asking = append(m.asking, *e.Question)
			m.row(e.Issue).asking = true
		}
	case drain.EventAnswer:
		// Every question ends here, including the ones this view answered and
		// has already dropped, and the ones nobody ever saw because they were
		// settled on the spot. All three are the same operation.
		if e.Question != nil {
			m.dropQuestion(e.Question.ID)
		}

	case drain.EventIssueEnd:
		r := m.row(e.Issue)
		r.asking = false
		r.Role, r.Stage = "", ""
		r.State = rowState(e.Outcome, r.killing || stageOf(e) == drain.StageKilled)
		r.Ended, r.total, r.final = e.At, e.Usage, true
		r.settled, r.live = runner.Usage{}, runner.Usage{}
		if e.Text != "" {
			r.Detail = firstLine(e.Text)
		} else {
			r.Detail = string(e.Outcome)
		}

	case drain.EventWaveIntegrating:
		m.integrating(e)
	case drain.EventMergeStart:
		m.mergeStart(e)
	case drain.EventMergeConflict:
		m.mergeConflict(e)
	case drain.EventMergeEnd:
		m.mergeEnd(e)
	case drain.EventWaveGateStart:
		m.gateStart(e)
	case drain.EventWaveGateEnd:
		m.gateEnd(e)
	case drain.EventWaveRollback:
		m.rolledBack(e)
	case drain.EventWaveEnd:
		m.waveEnd(e)
	case drain.EventPaused:
		where := e.Text
		if where == "" {
			where = fmt.Sprintf("paused at the wave %d barrier", e.Wave)
		}
		m.status = where + "; `bd-auto run resume` continues"
	case drain.EventResumed:
		m.status = fmt.Sprintf("wave %d resumed", e.Wave)
	case drain.EventRunEnd:
		m.report = e.Run
		if e.Run != nil {
			shape := fmt.Sprintf("after %d wave(s)", e.Run.Waves)
			if e.Run.Continuous {
				shape = "continuously"
			}
			m.status = fmt.Sprintf("run %s %s: %d done, %d parked",
				e.Run.Outcome, shape, len(e.Run.Done), len(e.Run.Parked))
		}
	}
}

// stageDetail is what the activity cell says once a stage has answered.
//
// The verdict and nothing else. A failed stage carries its whole feedback on
// the event, and it is several paragraphs written for the worker that is about
// to read it — one line of which is "The gate failed", so a cell built out of
// it would say the same thing twice and clip the rest. The cell is replaced by
// the next round's activity within a second anyway; what the reason is for is
// the transcript.
func stageDetail(e drain.Event) string {
	return "the " + e.Stage + " stage " + passFail(e.Passed)
}

// branches counts branches for a status line, plurally.
func branches(n int) string {
	if n == 1 {
		return "1 branch"
	}
	return fmt.Sprintf("%d branches", n)
}

// conflicts counts conflicted paths for a row's activity cell, plurally.
func conflicts(n int) string {
	if n == 1 {
		return "1 conflict"
	}
	return fmt.Sprintf("%d conflicts", n)
}

// accrue folds an activity event's usage into a row.
//
// The usage on an activity event is the running total of the process in flight,
// so it replaces rather than adds — and the process's final event is the one
// that banks it, because the next process starts counting from zero again.
func (m *Model) accrue(r *Row, e drain.Event) {
	if e.Phase == runner.EventDone {
		banked := e.Usage
		if banked.IsZero() {
			banked = r.live
		}
		r.settled = r.settled.Add(banked)
		r.live = runner.Usage{}
		return
	}
	if !e.Usage.IsZero() {
		r.live = e.Usage
	}
}

// rowState maps an issue's outcome onto what the table shows.
func rowState(o drain.Outcome, killed bool) State {
	if killed {
		return StateKilled
	}
	switch o {
	case drain.OutcomeDone:
		return StateDone
	case drain.OutcomeParked:
		return StateParked
	case drain.OutcomeFailed:
		return StateFailed
	case drain.OutcomeInterrupted, drain.OutcomeInfra:
		return StateInterrupted
	}
	return StateRunning
}

func stageOf(e drain.Event) string {
	if e.Report == nil {
		return ""
	}
	return e.Report.Stage
}

// Cost is the run's total so far: every issue, plus what the barriers spent.
// Once the engine has reported its own total, that wins — it is the number the
// report carries and the two must not disagree.
func (m *Model) Cost() float64 {
	if m.report != nil {
		return m.report.Usage.CostUSD
	}
	total := m.barrierCost()
	for _, r := range m.rows {
		total += r.Cost()
	}
	return total
}

// counts is how many rows are in each state, for the summary line.
func (m *Model) counts() map[State]int {
	out := map[State]int{}
	for _, r := range m.rows {
		out[r.State]++
	}
	return out
}

// --- view ---

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	// A question is the one thing on this screen that needs a human right now,
	// so it gets the only border and the only accent colour.
	askBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.AdaptiveColor{Light: "31", Dark: "39"}).
			Padding(0, 1)
	askHeadStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "31", Dark: "39"})
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "247", Dark: "241"})
	selectedStyle = lipgloss.NewStyle().Bold(true)
	stateStyles   = map[State]lipgloss.Style{
		StateWaiting:     lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "247", Dark: "241"}),
		StateRunning:     lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "31", Dark: "39"}),
		StateDone:        lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"}),
		StateParked:      lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"}),
		StateFailed:      lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"}),
		StateKilled:      lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"}),
		StateInterrupted: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"}),
		// The barrier's states, painted in the same vocabulary: what is
		// happening is blue, what landed is green, and what did not is the
		// amber a parked row already wears.
		StateMerging:    lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "31", Dark: "39"}),
		StateResolving:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "31", Dark: "39"}),
		StateMerged:     lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"}),
		StatePassed:     lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"}),
		StateSkipped:    lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"}),
		StateRolledBack: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"}),
	}
)

// The fixed columns. Activity takes whatever is left.
//
// colState is 11 rather than the 8 a State needs, because the cell names a role
// as often as a state and "integrator" is 10. Widening it rather than adding a
// column of its own keeps the marker, ISSUE and WAVE exactly where they were —
// they are what the eye tracks down the table — and costs three cells of
// ACTIVITY, which is the column built to give way. A role or stage name longer
// than the cell is clipped: a configured stage may be called anything, and a
// column that stretched to fit one would move every column after it.
const (
	colIssue = 22
	colWave  = 4
	colState = 11
	colTime  = 6
	colCost  = 8
)

// View implements tea.Model.
func (m *Model) View() string {
	if m.detail != nil {
		return m.detailView()
	}
	now := m.now()
	var b strings.Builder

	// The chrome is built first so the table can be given exactly the room that
	// is left. It is fixed and must survive at any height: the summary and the
	// status line are what say whether the run is still moving, and a table that
	// pushed them off the bottom would answer the one question a watcher has by
	// hiding it.
	head := titleStyle.Render(m.heading()) + "\n\n" + headerStyle.Render(m.header()) + "\n"
	var foot strings.Builder
	foot.WriteString("\n" + m.summary() + "\n")
	if box := m.questionBox(); box != "" {
		foot.WriteString(box + "\n")
	}
	if m.status != "" {
		foot.WriteString(dimStyle.Render(clip(m.status, m.width())) + "\n")
	}
	foot.WriteString(dimStyle.Render(m.keys()))

	body, cursorLine := m.tableBody(now)
	b.WriteString(head)
	for _, line := range m.windowTable(body, cursorLine, lipgloss.Height(head)+lipgloss.Height(foot.String())) {
		b.WriteString(line + "\n")
	}
	b.WriteString(foot.String())
	// The trailing newline is load-bearing. bubbletea renders the final frame on
	// its way out and then erases the line the cursor is left on, which is the
	// last line of that frame — so a view whose last line carries anything ends
	// every run by throwing it away. Ending on an empty line gives the renderer
	// something to erase that nobody needed.
	b.WriteString("\n")
	return b.String()
}

// tableBody is every line of the table, and which of them the cursor is on.
//
// Built as lines rather than written straight out because the table has to be
// windowed, and a barrier's rows share the cursor's index space with the issue
// rows — barrierBlocks continues counting from len(m.order) — so the cursor's
// line can only be found by rendering them together.
func (m *Model) tableBody(now time.Time) (lines []string, cursorLine int) {
	cursorLine = -1
	for i, id := range m.order {
		if i == m.cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, m.line(m.rows[id], i == m.cursor, now))
	}
	barrier, rowAt := m.barrierLines(len(m.order), now)
	if len(barrier) > 0 {
		base := len(lines)
		lines = append(lines, barrier...)
		if at, ok := rowAt[m.cursor]; ok {
			cursorLine = base + at
		}
	}
	if cursorLine < 0 {
		cursorLine = 0
	}
	return lines, cursorLine
}

// windowTable is the screenful of the table that fits, with the cursor kept in
// it and a count of what is off each end.
//
// Without this the view wrote one line per issue whatever the terminal was, so
// a thirty-issue scope on a twenty-row terminal rendered thirty-six lines and
// the terminal kept the last twenty: the heading and the first rows were gone,
// with no key to bring them back. A barrier makes it easier to hit, since it
// adds a row per branch plus a gate row and a rule.
func (m *Model) windowTable(body []string, cursorLine, chrome int) []string {
	room := m.height() - chrome
	if room < minTableRows {
		room = minTableRows
	}
	if len(body) <= room {
		m.tableTop = 0
		return body
	}

	// One line at each end is spent on saying what is hidden, but only at the
	// end that is actually hiding something.
	top := m.tableTop
	if top > len(body)-room {
		top = len(body) - room
	}
	if top < 0 {
		top = 0
	}
	// The cursor is what the window follows: a row selected off screen is a
	// selection nobody can see.
	if cursorLine < top+1 {
		top = maxInt(cursorLine-1, 0)
	}
	if cursorLine >= top+room-1 {
		top = cursorLine - room + 2
	}
	if top > len(body)-room {
		top = len(body) - room
	}
	if top < 0 {
		top = 0
	}
	m.tableTop = top

	out := make([]string, 0, room)
	end := top + room
	if end > len(body) {
		end = len(body)
	}
	out = append(out, body[top:end]...)
	if top > 0 {
		out[0] = dimStyle.Render(fmt.Sprintf("  ↑ %d more above", top))
	}
	if end < len(body) {
		out[len(out)-1] = dimStyle.Render(fmt.Sprintf("  ↓ %d more below", len(body)-end+1))
	}
	return out
}

// minTableRows is the fewest rows the table is given however small the terminal
// is. Below this the view is useless anyway, and clamping here keeps the
// windowing arithmetic from going negative.
const minTableRows = 3

// questionBox is the popup a waiting question is answered in.
//
// It sits under the table rather than over it on purpose: the table is what
// says whether the rest of the run is still moving, and covering it to ask
// about one issue would hide the answer to "is anything else stuck too". The
// border and the issue name are what make it clear which worker is waiting,
// since several may be.
func (m *Model) questionBox() string {
	q := m.Question()
	if q == nil {
		return ""
	}

	width := maxInt(m.width()-2, 40)
	inner := width - 4
	var b strings.Builder

	head := q.Issue + " asks"
	if q.Header != "" {
		head += " · " + q.Header
	}
	if n := m.Waiting(); n > 0 {
		head += fmt.Sprintf("  (%d more waiting)", n)
	}
	b.WriteString(askHeadStyle.Render(clip(head, inner)) + "\n")
	for _, line := range wrap(q.Text, inner) {
		b.WriteString(line + "\n")
	}

	if len(q.Options) > 0 {
		b.WriteString("\n")
		for i, opt := range q.Options {
			marker := "  "
			if i == m.choice && !m.typing {
				marker = "> "
			}
			line := fmt.Sprintf("%s%d. %s", marker, i+1, opt.Label)
			if opt.Description != "" {
				line += " — " + opt.Description
			}
			line = clip(line, inner)
			if i == m.choice && !m.typing {
				line = selectedStyle.Render(line)
			}
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n")
	if m.typing {
		b.WriteString(clip("your answer: "+m.typed+"▌", inner) + "\n")
		b.WriteString(dimStyle.Render(clip("enter sends · esc goes back", inner)))
	} else {
		b.WriteString(dimStyle.Render(clip(m.askKeys(len(q.Options)), inner)))
	}
	return askBoxStyle.Width(width).Render(b.String())
}

func (m *Model) askKeys(options int) string {
	if m.Ask == nil {
		return "this view cannot answer: the run has no channel back"
	}
	if options == 0 {
		return "enter or t to type an answer · s let it decide · esc dismiss"
	}
	return fmt.Sprintf("1-%d or ↑/↓ and enter to answer · t type your own · s let it decide · esc dismiss", options)
}

// wrap breaks text onto lines of at most n cells, on word boundaries where it
// can. A question is prose and gets read once, so losing the end of it to a
// clip is losing the question.
func wrap(s string, n int) []string {
	if n <= 0 {
		return nil
	}
	var out []string
	for _, para := range strings.Split(strings.TrimSpace(s), "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case len([]rune(line))+1+len([]rune(word)) <= n:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
			// A single word longer than the whole line is cut rather than
			// allowed to take the border apart.
			if len([]rune(line)) > n {
				out = append(out, clip(line, n))
				line = ""
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func (m *Model) width() int {
	if m.Width > 0 {
		return m.Width
	}
	return DefaultWidth
}

func (m *Model) height() int {
	if m.Height > 0 {
		return m.Height
	}
	return DefaultHeight
}

func (m *Model) heading() string {
	head := "bd-auto drain"
	if m.epic != "" {
		head += " · " + m.epic
	}
	// Not for a continuous run. It opens one wave and never opens another, so
	// the number is a comparison with nothing to compare it to, and it would sit
	// in the heading for the whole run saying the run had not moved.
	if m.wave > 0 && !m.lane {
		head += fmt.Sprintf(" · wave %d", m.wave)
	}
	return clip(fmt.Sprintf("%s · %d issue(s) in scope", head, len(m.order)), m.width())
}

func (m *Model) header() string {
	return fmt.Sprintf("  %-*s %-*s %-*s %*s %*s  %s",
		colIssue, "ISSUE", colWave, "WAVE", colState, "STATE",
		colTime, "TIME", colCost, "COST", "ACTIVITY")
}

// line renders one row. The marker column is two characters so that selecting a
// row does not shift the table sideways.
func (m *Model) line(r *Row, selected bool, now time.Time) string {
	if r == nil {
		return ""
	}
	marker := "  "
	if selected {
		marker = "> "
	}

	state := string(r.State)
	// A terminal row keeps its own word: done, parked, failed, killed and
	// stopped are outcomes, and no process is running to name.
	if r.State == StateRunning && r.Doing() != "" {
		state = r.Doing()
	}
	switch {
	case r.killing && !r.State.terminal():
		state = "killing"
	case r.asking && !r.State.terminal():
		// Not a State: the worker is still running, it is running a tool call
		// that happens to be waiting on a person. What the column has to say is
		// that the stopped clock is somebody's fault and not the model's, which
		// outranks which process it is.
		state = "asking"
	}

	fixed := fmt.Sprintf("%s%-*s %-*s %-*s %*s %*s  ",
		marker,
		colIssue, clip(r.Issue, colIssue),
		colWave, waveOf(r),
		colState, clip(state, colState),
		colTime, elapsed(r, now),
		colCost, money(r.Cost()))

	// A message still being written is shown from its end: the newest words are
	// the ones that say what the worker is doing now. Everything else reads from
	// the front.
	room := maxInt(m.width()-lipgloss.Width(fixed), 8)
	detail := clip(r.Detail, room)
	if r.stream != "" {
		detail = tail(r.Detail, room)
	}
	line := fixed + detail
	if style, ok := stateStyles[r.State]; ok {
		line = style.Render(line)
	}
	if selected {
		line = selectedStyle.Render(line)
	}
	return line
}

// summary is the run in one line: how the issues stand, and what it has cost.
//
// The barrier's own figure is beside the total rather than only inside it. It
// belongs to no issue, so every other number on this line excludes it, and a
// run that spent a third of its money resolving conflicts should be able to say
// so rather than leaving it as the difference between the total and a sum
// nobody computes.
func (m *Model) summary() string {
	c := m.counts()
	out := fmt.Sprintf("%d running · %d done · %d parked · %d killed",
		c[StateRunning], c[StateDone], c[StateParked]+c[StateFailed], c[StateKilled])
	if cost := m.barrierCost(); cost > 0 {
		out += " · " + m.integratorName() + " " + money(cost)
	}
	return out + " · run total " + money(m.Cost())
}

func (m *Model) keys() string {
	switch {
	case m.Question() != nil:
		// The box carries its own key line; repeating it here would leave two
		// sets of instructions on screen disagreeing about what enter does.
		return "answer the question above · ctrl+c stop the run"
	case m.finished:
		// No keystroke is offered, because none can be taken: the message that
		// sets this quits the program in the same update. This is the last thing
		// the table says, not an invitation.
		return "the run is over"
	case m.stopping:
		return "stopping · q again to leave the view"
	case m.Control == nil:
		return "↑/↓ select · enter transcript · q close (this view cannot stop the run)"
	}
	// Terse on purpose: enter and k both act on the selected row, and both say
	// what they did in the status line the moment they are pressed. The line
	// they replaced spelled k out in full and no longer fit a narrow terminal
	// once enter joined it.
	return "↑/↓ select · enter transcript · k kill · q stop the run"
}

// --- formatting ---

func waveOf(r *Row) string {
	if r.Wave <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", r.Wave)
}

func elapsed(r *Row, now time.Time) string {
	d := r.Elapsed(now)
	if d <= 0 {
		return "-"
	}
	return duration(d)
}

// money is deliberately shown to four places. Cost is displayed and never
// enforced: nothing in bd-auto stops a run for spending, so the number is there
// to be watched, and a number rounded to cents hides the difference between a
// worker that is thinking and one that has stalled.
func money(usd float64) string {
	if usd <= 0 {
		return "-"
	}
	return fmt.Sprintf("$%.4f", usd)
}

func passFail(ok bool) string {
	if ok {
		return "passed"
	}
	return "failed"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// clip truncates to n display cells, counting runes rather than bytes so an
// issue title with an ellipsis in it does not lose half a character.
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// tail truncates to the last n cells, marking the front as cut.
func tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return "…" + string(r[len(r)-(n-1):])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
