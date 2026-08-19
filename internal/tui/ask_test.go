package tui

import (
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"bd-auto/internal/ask"
	"bd-auto/internal/drain"
)

// answered records what the view sent back to the run. As with the control
// channel, the effect on the engine is the half that matters: a popup that
// renders beautifully and replies to nobody is not a question channel.
type answered struct {
	mu       sync.Mutex
	replies  map[string]string
	declined []string
	open     map[string]bool
}

func newAnswered(open ...string) *answered {
	a := &answered{replies: map[string]string{}, open: map[string]bool{}}
	for _, id := range open {
		a.open[id] = true
	}
	return a
}

func (a *answered) Reply(id, text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open[id] {
		return false
	}
	delete(a.open, id)
	a.replies[id] = text
	return true
}

func (a *answered) Decline(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open[id] {
		return false
	}
	delete(a.open, id)
	a.declined = append(a.declined, id)
	return true
}

func (a *answered) reply(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.replies[id]
}

func askEvent(issue, id, header, text string, options ...string) drain.Event {
	q := ask.Question{ID: id, Issue: issue, Role: "worker", Header: header, Text: text}
	for _, o := range options {
		q.Options = append(q.Options, ask.Option{Label: o})
	}
	return drain.Event{Kind: drain.EventQuestion, At: at(10), Wave: 1, Issue: issue, Text: text, Question: &q}
}

func askModel(t *testing.T, a *answered, running ...string) *Model {
	t.Helper()
	m := newTestModel(newPressed(running...))
	m.Ask = a
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Text: "epic-1", Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventWaveStart, At: at(0), Wave: 1, Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(1), Wave: 1, Issue: "t-1", Text: "the first issue"},
		drain.Event{Kind: drain.EventIssueStart, At: at(1), Wave: 1, Issue: "t-2", Text: "the second issue"},
	)
	return m
}

// The question has to be answerable from what is on screen: which issue is
// asking, what it wants to know, and what the choices are.
func TestAQuestionIsRenderedWithItsIssueAndOptions(t *testing.T) {
	m := askModel(t, newAnswered("q1"), "t-1", "t-2")
	feed(m, askEvent("t-1", "q1", "Config key",
		"Which key should the timeout live under?", "ask.timeout", "runners.timeout"))

	view := m.View()
	for _, want := range []string{"t-1", "asks", "Config key", "Which key should the timeout live under?",
		"1. ask.timeout", "2. runners.timeout"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the question box does not show %q:\n%s", want, view)
		}
	}
	// The row has to say so too. A worker waiting on a person looks exactly
	// like one that has hung: same clock climbing, nothing happening.
	if !strings.Contains(view, "asking") {
		t.Fatalf("the asking issue's row does not say it is waiting:\n%s", view)
	}
	// And the rest of the run stays visible: covering the table to ask about
	// one issue hides the answer to "is anything else stuck too".
	if !strings.Contains(view, "t-2") {
		t.Fatalf("the table was hidden by the question:\n%s", view)
	}
}

// Answering by number is the fast path, and the answer has to be the option's
// label rather than the digit.
func TestADigitAnswersTheQuestion(t *testing.T) {
	a := newAnswered("q1")
	m := askModel(t, a, "t-1")
	feed(m, askEvent("t-1", "q1", "Config key", "Which key?", "ask.timeout", "runners.timeout"))

	key(m, "2")
	if got := a.reply("q1"); got != "runners.timeout" {
		t.Fatalf("the run was told %q", got)
	}
	// And the form goes away, or it gets answered twice.
	if m.Question() != nil {
		t.Fatal("the question is still on screen after being answered")
	}
	if m.Row("t-1").asking {
		t.Fatal("the row still says the worker is waiting")
	}
}

// The cursor and enter are the same answer by another route, and the box has to
// show which option is selected.
func TestTheCursorAndEnterAnswerToo(t *testing.T) {
	a := newAnswered("q1")
	m := askModel(t, a, "t-1")
	feed(m, askEvent("t-1", "q1", "", "Which key?", "first", "second", "third"))

	special(m, tea.KeyDown)
	special(m, tea.KeyDown)
	if !strings.Contains(m.View(), "> 3. third") {
		t.Fatalf("the cursor is not on the third option:\n%s", m.View())
	}
	special(m, tea.KeyEnter)
	if got := a.reply("q1"); got != "third" {
		t.Fatalf("the run was told %q", got)
	}
}

// An option list is a guess at what the human will say, so writing something
// else has to be possible — and while it is being written, every printable key
// belongs to the text field, digits included.
func TestTypingYourOwnAnswer(t *testing.T) {
	a := newAnswered("q1")
	m := askModel(t, a, "t-1")
	feed(m, askEvent("t-1", "q1", "", "Which key?", "first", "second"))

	key(m, "t")
	key(m, "u")
	key(m, "s")
	key(m, "e")
	special(m, tea.KeySpace)
	key(m, "2")
	special(m, tea.KeyBackspace)
	key(m, "3")

	typed, typing := m.Typed()
	if !typing || typed != "use 3" {
		t.Fatalf("typed %q (typing=%v); a digit must not have been read as an option", typed, typing)
	}
	if !strings.Contains(m.View(), "use 3") {
		t.Fatalf("what is being typed is not on screen:\n%s", m.View())
	}

	special(m, tea.KeyEnter)
	if got := a.reply("q1"); got != "use 3" {
		t.Fatalf("the run was told %q", got)
	}
}

// Handing the question back is a real answer: the model is told to decide and
// to write down what it assumed.
func TestDecliningHandsTheQuestionBack(t *testing.T) {
	a := newAnswered("q1")
	m := askModel(t, a, "t-1")
	feed(m, askEvent("t-1", "q1", "", "Which key?", "first"))

	key(m, "s")
	if len(a.declined) != 1 || a.declined[0] != "q1" {
		t.Fatalf("the run was not told to decide: %+v", a.declined)
	}
	if m.Question() != nil {
		t.Fatal("the question is still on screen")
	}
}

// Several workers may ask at once. The queue is answered one at a time, oldest
// first, and the human is told how many are behind.
func TestConcurrentQuestionsQueueOneAtATime(t *testing.T) {
	a := newAnswered("q1", "q2")
	m := askModel(t, a, "t-1", "t-2")
	feed(m,
		askEvent("t-1", "q1", "", "First question?", "a", "b"),
		askEvent("t-2", "q2", "", "Second question?", "c", "d"),
	)

	if got := m.Question(); got == nil || got.ID != "q1" {
		t.Fatalf("the queue is showing %+v, want the oldest", got)
	}
	if m.Waiting() != 1 {
		t.Fatalf("%d question(s) waiting, want 1", m.Waiting())
	}
	if !strings.Contains(m.View(), "1 more waiting") {
		t.Fatalf("the human is not told another is queued:\n%s", m.View())
	}

	key(m, "1")
	if got := a.reply("q1"); got != "a" {
		t.Fatalf("the first question was answered %q", got)
	}
	// The second is now up, with the input reset rather than carrying the
	// previous question's cursor.
	q := m.Question()
	if q == nil || q.ID != "q2" {
		t.Fatalf("the next question is %+v", q)
	}
	if m.choice != 0 {
		t.Fatalf("the cursor carried over at %d", m.choice)
	}
	key(m, "2")
	if got := a.reply("q2"); got != "d" {
		t.Fatalf("the second question was answered %q", got)
	}
	if m.Question() != nil {
		t.Fatal("the queue is not empty")
	}
}

// A question the run settles by itself — it timed out, or the worker was killed
// — has to come off the screen. Left there, it asks a human to decide something
// for a process that no longer exists.
func TestARunSettledQuestionLeavesTheScreen(t *testing.T) {
	m := askModel(t, newAnswered("q1"), "t-1")
	feed(m, askEvent("t-1", "q1", "", "Which key?", "a"))
	if m.Question() == nil {
		t.Fatal("the question never appeared")
	}

	q := ask.Question{ID: "q1", Issue: "t-1", Text: "Which key?"}
	feed(m, drain.Event{
		Kind: drain.EventAnswer, At: at(20), Issue: "t-1",
		Question: &q, Answer: &ask.Answer{Text: ask.TimeoutText, Source: ask.SourceTimeout},
	})
	if m.Question() != nil {
		t.Fatal("the question stayed up after the run settled it")
	}
	if m.Row("t-1").asking {
		t.Fatal("the row still says the worker is waiting")
	}
}

// While a question is up the keys belong to it. A digit that killed a worker,
// or a k that killed the one waiting for the answer, would be the worst
// possible surprise on this screen.
func TestTheQuestionTakesTheKeysFromTheTable(t *testing.T) {
	a := newAnswered("q1")
	p := newPressed("t-1", "t-2")
	m := newTestModel(p)
	m.Ask = a
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1", "t-2"}},
		drain.Event{Kind: drain.EventIssueStart, At: at(1), Wave: 1, Issue: "t-1"},
		drain.Event{Kind: drain.EventIssueStart, At: at(1), Wave: 1, Issue: "t-2"},
		askEvent("t-1", "q1", "", "Which key?", "a", "b"),
	)

	key(m, "k")
	if kills := p.kills(); len(kills) != 0 {
		t.Fatalf("k killed %v while a question was on screen", kills)
	}
	if cmd := key(m, "q"); quits(cmd) {
		t.Fatal("q left the view while a question was on screen")
	}
	if p.stopped() != 0 {
		t.Fatal("q stopped the run while a question was on screen")
	}

	// Ctrl-C is the exception: a way out has to work from everywhere.
	special(m, tea.KeyCtrlC)
	if p.stopped() != 1 {
		t.Fatalf("ctrl+c did not stop the run: %d", p.stopped())
	}
}

// A view with no channel back to the run says so rather than pretending to
// answer.
func TestAViewWithNoChannelSaysSo(t *testing.T) {
	m := newTestModel(newPressed("t-1"))
	feed(m,
		drain.Event{Kind: drain.EventRunStart, At: at(0), Issues: []string{"t-1"}},
		askEvent("t-1", "q1", "", "Which key?", "a"),
	)
	key(m, "1")
	if !strings.Contains(m.View(), "cannot answer") && !strings.Contains(m.View(), "no channel") {
		t.Fatalf("the view does not say it cannot answer:\n%s", m.View())
	}
}

// A question with no options is answerable only in words, and the box has to
// lead with that rather than with a list that is not there.
func TestAQuestionWithNoOptionsGoesStraightToTyping(t *testing.T) {
	a := newAnswered("q1")
	m := askModel(t, a, "t-1")
	feed(m, askEvent("t-1", "q1", "", "What should the flag be called?"))

	special(m, tea.KeyEnter)
	if _, typing := m.Typed(); !typing {
		t.Fatal("enter did not open the text field")
	}
	for _, r := range "verbose" {
		key(m, string(r))
	}
	special(m, tea.KeyEnter)
	if got := a.reply("q1"); got != "verbose" {
		t.Fatalf("the run was told %q", got)
	}
}

// An empty answer is not an answer, and sending one would tell the worker a
// blank line was the human's decision.
func TestAnEmptyTypedAnswerIsRefused(t *testing.T) {
	a := newAnswered("q1")
	m := askModel(t, a, "t-1")
	feed(m, askEvent("t-1", "q1", "", "Which key?", "a"))

	key(m, "t")
	special(m, tea.KeyEnter)
	if got := a.reply("q1"); got != "" {
		t.Fatalf("an empty answer was sent as %q", got)
	}
	if m.Question() == nil {
		t.Fatal("the question was dropped without being answered")
	}
}

// A long question has to be readable: clipping it to one line loses the
// question, which is the only thing on screen the human has to act on.
func TestALongQuestionWraps(t *testing.T) {
	m := askModel(t, newAnswered("q1"), "t-1")
	long := strings.Repeat("this is a long question that has to stay readable ", 6)
	feed(m, askEvent("t-1", "q1", "", long, "a"))

	view := m.View()
	if !strings.Contains(view, "this is a long question") {
		t.Fatalf("the question is not on screen:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if len([]rune(line)) > m.width()+8 { // the border and its styling
			t.Fatalf("a line of %d cells overflows a %d-cell view:\n%s", len([]rune(line)), m.width(), line)
		}
	}
}

// askQuestion feeds a question with descriptions on its options, which the
// shorter helper above cannot express and which are where the cost of choosing
// one usually lives.
func askQuestion(issue, id, header, text string, options ...ask.Option) drain.Event {
	q := ask.Question{ID: id, Issue: issue, Role: "worker", Header: header, Text: text, Options: options}
	return drain.Event{Kind: drain.EventQuestion, At: at(10), Wave: 1, Issue: issue, Text: text, Question: &q}
}

// boxLines is what the question box put on screen, with the border and its
// padding taken back off.
func boxLines(view string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(view, "\n") {
		switch {
		case strings.HasPrefix(line, "╭"):
			in = true
		case strings.HasPrefix(line, "╰"):
			in = false
		case in && strings.HasPrefix(line, "│"):
			out = append(out, strings.TrimRight(strings.TrimPrefix(line, "│ "), " │"))
		}
	}
	return out
}

// flatten reduces text to its words, so a test can ask whether something
// survived without caring which line the wrapping put it on.
func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

// fits checks the box against the terminal it is drawn on.
//
// The box only, not the whole view: the table above it is laid out in fixed
// columns that add up to more than a narrow terminal has, which is a separate
// fault and not one a question can do anything about.
func fits(t *testing.T, m *Model, view string) {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		if !strings.HasPrefix(line, "│") && !strings.HasPrefix(line, "╭") && !strings.HasPrefix(line, "╰") {
			continue
		}
		if n := len([]rune(line)); n > m.width() {
			t.Fatalf("a box line of %d cells overflows a %d-cell terminal:\n%s", n, m.width(), line)
		}
	}
}

// The options are the half of a question the reader acts on, and they were the
// half that was clipped: each was one clip()ped line, so a label with a
// description behind it lost its tail and the reader chose a number having read
// the front of what it meant.
func TestALongOptionWrapsUnderItsNumberRatherThanBeingClipped(t *testing.T) {
	m := askModel(t, newAnswered("q1"), "t-1")
	m.Width, m.Height = 56, 40
	const label = "runners.timeout"
	const desc = "each runner names its own deadline, which is more configuration to write but no surprises for whoever reads one runner"
	feed(m, askQuestion("t-1", "q1", "Where the timeout key lives", "Where should the timeout live?",
		ask.Option{Label: "ask.timeout", Description: "the ask package owns it and every caller inherits it"},
		ask.Option{Label: label, Description: desc},
	))

	view := m.View()
	fits(t, m, view)
	box := flatten(strings.Join(boxLines(view), " "))
	if want := flatten("2. " + label + " — " + desc); !strings.Contains(box, want) {
		t.Fatalf("the second option is not on screen in full:\n%s", view)
	}
	// The header goes with it: the box says which decision this is, and how many
	// other workers are waiting behind it, on the same line it used to clip.
	if !strings.Contains(box, "Where the timeout key lives") {
		t.Fatalf("the header was clipped:\n%s", view)
	}

	// And a wrapped option still reads as one option: its continuation lines sit
	// under the label rather than in the column the numbers are in.
	lines, _ := m.optionLines(m.Question(), m.width()-6)
	var second []string
	for i, line := range lines {
		if strings.Contains(line, "2. "+label) {
			second = lines[i:]
			break
		}
	}
	if len(second) < 2 {
		t.Fatalf("the second option did not wrap at all:\n%s", strings.Join(lines, "\n"))
	}
	for _, line := range second[1:] {
		if !strings.HasPrefix(line, "     ") {
			t.Fatalf("a continuation line is not indented under the number: %q", line)
		}
	}
}

// The cursor is at the end of what is being typed, so the end is what has to
// stay on screen: an input clipped at the right edge is one written blind.
func TestTheEndOfALongAnswerStaysVisible(t *testing.T) {
	a := newAnswered("q1")
	m := askModel(t, a, "t-1")
	m.Width, m.Height = 56, 40
	feed(m, askEvent("t-1", "q1", "", "What should the flag be called?"))

	const answer = "call it --review-rounds, and say in the help that zero means the reviewer never runs"
	key(m, "t")
	for _, r := range answer {
		key(m, string(r))
	}

	view := m.View()
	fits(t, m, view)
	var typed string
	for _, line := range boxLines(view) {
		if strings.HasPrefix(line, "your answer:") {
			typed = line
		}
	}
	if typed == "" {
		t.Fatalf("the answer being typed is not on screen:\n%s", view)
	}
	if !strings.HasSuffix(typed, "the reviewer never runs▌") {
		t.Fatalf("the end of the answer and the cursor are off the right edge: %q", typed)
	}
	// The whole of it is still sent: only the display gave way.
	special(m, tea.KeyEnter)
	if got := a.reply("q1"); got != answer {
		t.Fatalf("the run was told %q", got)
	}
}

// A short window is the vertical form of the same bug. The question gives way
// first, because a reader who can see the options and the keys can answer and
// can grow the window to read the rest; one whose options went off the bottom
// cannot answer at all.
func TestAShortWindowKeepsTheOptionsAndTheKeys(t *testing.T) {
	m := askModel(t, newAnswered("q1"), "t-1")
	m.Width, m.Height = 56, 18
	var options []ask.Option
	for _, name := range []string{"first", "second", "third", "fourth", "fifth"} {
		options = append(options, ask.Option{Label: name, Description: "what choosing it commits the run to"})
	}
	feed(m, askQuestion("t-1", "q1", "", strings.Repeat("a question long enough to fill the box on its own. ", 6), options...))

	view := m.View()
	fits(t, m, view)
	box := strings.Join(boxLines(view), "\n")
	if !strings.Contains(box, "1. first") {
		t.Fatalf("the options were pushed out of a short box:\n%s", view)
	}
	if !strings.Contains(box, "esc dismiss") {
		t.Fatalf("the answer keys were pushed out of a short box:\n%s", view)
	}
	// What was dropped is counted rather than silently gone.
	if !strings.Contains(box, "…") {
		t.Fatalf("the box was trimmed without saying so:\n%s", view)
	}

	// And the window follows the cursor: an option selected off the bottom is a
	// selection nobody can read before they press enter.
	for i := 0; i < 4; i++ {
		special(m, tea.KeyDown)
	}
	if box := strings.Join(boxLines(m.View()), "\n"); !strings.Contains(box, "5. fifth") {
		t.Fatalf("the selected option is not in the window:\n%s", m.View())
	}
}

// The box tracks the terminal rather than assuming a hundred columns, and
// nothing in it is clipped on the way down.
func TestTheBoxFitsANarrowTerminal(t *testing.T) {
	for _, width := range []int{40, 56, 80, 120} {
		m := askModel(t, newAnswered("q1"), "t-1")
		m.Width, m.Height = width, 40
		feed(m, askQuestion("t-1", "q1", "Config key", "Which key should the timeout live under?",
			ask.Option{Label: "ask.timeout", Description: "the ask package owns the deadline"},
			ask.Option{Label: "runners.timeout", Description: "each runner names its own"},
		))
		view := m.View()
		fits(t, m, view)
		box := flatten(strings.Join(boxLines(view), " "))
		for _, want := range []string{"Which key should the timeout live under?",
			"1. ask.timeout — the ask package owns the deadline",
			"2. runners.timeout — each runner names its own"} {
			if !strings.Contains(box, flatten(want)) {
				t.Fatalf("at %d columns the box lost %q:\n%s", width, want, view)
			}
		}
	}
}
