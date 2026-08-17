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
