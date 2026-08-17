// Package ask is how a headless model puts a question to the human watching a
// run, without ending its session to do it.
//
// A worker that hits a genuine ambiguity used to have two options, and both
// were expensive. Guess, and be wrong in a way nobody sees until review; or
// fail the attempt and let the failure text reach a human on the next round,
// which costs a whole fresh session to say one sentence. The interactive
// orchestrator could simply ask, and that turned out to be one of the more
// valuable things it did.
//
// # Why a tool and not a session exit
//
// Exiting `claude -p` to ask means the next attempt is a new process that must
// be re-sent the whole task, which is exactly the cost bd-auto exists to avoid.
// A tool call blocks inside the live session, so an answer costs one tool
// round-trip instead of a session restart. Only the asking worker is blocked;
// every other worker in the wave keeps running.
//
// # Why a ticket rather than one long call
//
// A human may be away from the keyboard for hours, and no backend will hold a
// tool call open that long. Claude Code kills an idle stdio tool call after 30
// minutes by default, and other backends are stricter. So Ask blocks only for a
// Hold — long enough that a human who is actually watching answers inside the
// first call — and then hands back a ticket. The model collects the answer with
// Wait, which holds again and costs a few dozen tokens a time. The question
// itself lives in the Broker across all of it, so a two-hour wait is a handful
// of cheap round-trips rather than one call that dies.
//
// # The unattended case is the one that matters
//
// --quiet, --plain, --json and any non-TTY run have nobody to answer. A
// question must never hang a drain, so under PolicyUnattended the tool returns
// immediately with an answer telling the model to proceed on its best judgement
// and write the assumption down. An unattended run therefore degrades to what
// it did before this package existed, rather than stalling.
package ask

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ServerName is the MCP server bd-auto offers its own workers, and half of each
// tool's qualified name.
//
// Underscores rather than the hyphen the binary uses: a qualified MCP tool name
// is joined with double underscores, and a hyphen there reads badly in an
// allowlist entry.
const ServerName = "bd_auto"

// The tools this package offers.
const (
	// ToolAsk puts a question to the human.
	ToolAsk = "ask_user"
	// ToolWait collects the answer to a question that is still open.
	ToolWait = "ask_user_wait"
)

// Tools lists the tools the server offers, in the order a model meets them.
func Tools() []string { return []string{ToolAsk, ToolWait} }

// Defaults for a broker's two clocks.
const (
	// DefaultHold is how long one tool call blocks before handing back a
	// ticket. Five minutes is chosen against the backend, not against the
	// human: Claude Code's stdio idle timeout is thirty minutes, so this leaves
	// six times the margin, and a human who is actually watching answers well
	// inside it.
	DefaultHold = 5 * time.Minute
	// DefaultTimeout is how long a question waits for a human before it gives
	// up and tells the model to proceed. It is generous because the cost of
	// waiting is one idle worker and a few polls, and the cost of giving up
	// early is a decision made by nobody.
	DefaultTimeout = time.Hour
)

// Option is one answer a question offers. It is the shape Claude Code's own
// AskUserQuestion tool uses, because models already know it.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is one thing a model wants to know.
type Question struct {
	// ID is the ticket: short, because the model repeats it on every poll and
	// every repetition is context.
	ID string `json:"id"`
	// Issue is who is asking, and Role which of that issue's models.
	Issue string `json:"issue"`
	Role  string `json:"role,omitempty"`
	// Header is a few words naming the decision, for a display with one line to
	// spend on it.
	Header string `json:"header,omitempty"`
	// Text is the question itself.
	Text string `json:"question"`
	// Options are the answers offered. A question may have none, in which case
	// only free text will do.
	Options []Option `json:"options,omitempty"`
	// AskedAt is when it was raised, and Deadline when it gives up.
	AskedAt  time.Time `json:"asked_at"`
	Deadline time.Time `json:"deadline,omitempty"`
}

// Labels lists the option labels, for a renderer with no room for descriptions.
func (q Question) Labels() []string {
	out := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		out = append(out, o.Label)
	}
	return out
}

// Title is the shortest honest name for a question.
func (q Question) Title() string {
	if q.Header != "" {
		return q.Header
	}
	return firstLine(q.Text)
}

// Source says who or what produced an answer. It is on the record because
// "a human chose this" and "nobody was there" are very different things to read
// off a finished run.
type Source string

// The answer sources.
const (
	// SourceHuman is an answer a human gave. It is the only source that
	// suppresses the same question being asked again.
	SourceHuman Source = "human"
	// SourceRecorded is an answer a human gave earlier in this run, replayed
	// from run state so a resume does not ask twice.
	SourceRecorded Source = "recorded"
	// SourceUnattended is a question raised where nobody was watching.
	SourceUnattended Source = "unattended"
	// SourceTimeout is a question a watcher could have answered and did not.
	SourceTimeout Source = "timeout"
	// SourceDeclined is a human reading the question and handing it back.
	SourceDeclined Source = "declined"
	// SourceAbandoned is the asker going away — killed, stopped, or a run that
	// ended underneath it.
	SourceAbandoned Source = "abandoned"
)

// Answered reports whether a source is a real answer rather than the absence of
// one.
func (s Source) Answered() bool { return s == SourceHuman || s == SourceRecorded }

// Answer is what goes back to the model.
type Answer struct {
	Text   string `json:"text"`
	Source Source `json:"source"`
}

// The answers a question gets when no human gives one. Each is an instruction
// rather than an apology: the model has to finish the work either way, and what
// makes an unanswered question recoverable is the assumption being written
// down where the next reader will find it.
const (
	// UnattendedText is what a question gets when nobody is watching the run.
	UnattendedText = "Nobody is watching this run, so there is no answer to give you. " +
		"Proceed on your own best judgement: pick the reading you would defend to a reviewer, " +
		"and record the assumption you made and why — in the issue's notes with `bd`, in a " +
		"comment where the code is, and in your final message. Do not ask this again."
	// TimeoutText is what a question gets when a human could have answered and
	// did not.
	TimeoutText = "No answer came back in time, so nobody is going to decide this for you. " +
		"Proceed on your own best judgement: pick the reading you would defend to a reviewer, " +
		"and record the assumption you made and why — in the issue's notes with `bd`, in a " +
		"comment where the code is, and in your final message. Do not ask this again."
	// DeclinedText is what a question gets when a human read it and handed it
	// back.
	DeclinedText = "The human read your question and decided you should answer it. " +
		"Proceed on your own best judgement, and record the assumption you made and why — " +
		"in the issue's notes with `bd`, in a comment where the code is, and in your final " +
		"message. Do not ask this again."
	// AbandonedText is what an open question resolves to when the run it
	// belongs to ends underneath it.
	AbandonedText = "The run that asked this question has stopped, so the question is gone. " +
		"Stop waiting for an answer."
	// UnknownTicketText is what a poll for a ticket nobody is holding gets.
	// It is the same instruction rather than an error, because the realistic
	// cause is a question that expired or a run that restarted, and a model
	// that receives an error here has no better move than the one this names.
	UnknownTicketText = "That question is no longer open — it expired, or the run restarted. " +
		"Proceed on your own best judgement, and record the assumption you made and why. " +
		"Do not ask it again."
)

// Policy is what happens to a question when it is raised.
type Policy string

// The policies.
const (
	// PolicyAsk puts the question to a human and waits.
	PolicyAsk Policy = "ask"
	// PolicyUnattended answers immediately, because there is nobody to ask.
	// This is what every non-TTY run gets, and it is the default: a broker that
	// was not told a human is present must not assume one.
	PolicyUnattended Policy = "unattended"
)

// Reply is one tool call's outcome: either the question is settled and the
// answer is here, or it is still open and Ticket says what to come back for.
type Reply struct {
	Question Question
	Answer   Answer
	// Settled reports that Answer is the final word. When it is false the
	// caller polls Wait with Question.ID.
	Settled bool
}

// Responder is the half of a Broker a live view uses: it answers questions, it
// cannot raise them.
//
// An interface rather than the concrete type for the same reason drain's
// Stopper is one — a popup driven by keystrokes is only worth having if a test
// can press the keys and read back what the run was told.
type Responder interface {
	// Reply answers a question, reporting whether there was one to answer.
	Reply(id, text string) bool
	// Decline hands the question back to the model.
	Decline(id string) bool
}

// waiter is one open question and the goroutines parked on it.
type waiter struct {
	q Question
	// done closes when the question is settled; answer is written before it
	// closes, so a reader that sees the close sees the answer.
	done   chan struct{}
	answer Answer
	once   sync.Once
}

func (w *waiter) settle(a Answer) bool {
	settled := false
	w.once.Do(func() {
		w.answer = a
		settled = true
		close(w.done)
	})
	return settled
}

// Broker is the run's question channel: one per run, shared by every worker.
//
// A nil Broker is not valid; the engine checks for one before offering the tool
// at all, which is what makes a run with no broker behave exactly as it did
// before this package existed.
type Broker struct {
	// Policy decides whether a question waits for a human or is answered on the
	// spot. Empty means PolicyUnattended, because assuming a human is watching
	// is the assumption that hangs a drain.
	Policy Policy
	// Hold is how long one Ask or Wait blocks before handing back a ticket.
	// Zero means DefaultHold.
	Hold time.Duration
	// Timeout is how long a question waits for a human in total. Zero means
	// DefaultTimeout; negative means never give up, which is a choice a config
	// can make and the default deliberately does not.
	Timeout time.Duration

	// Raise is called when a question is put to a human and will actually wait
	// for one. It is not called for a question answered on the spot — from run
	// state or by an unattended policy — because a watcher that is shown a
	// question and has it withdrawn in the same frame learns nothing.
	Raise func(Question)
	// Settle is called with every question's final answer, including the ones
	// Raise never saw. It is the complete record of what was asked.
	Settle func(Question, Answer)

	// Recall returns what a human already answered for this question, so a
	// resumed run does not ask it again. Nil means nothing is remembered.
	Recall func(Question) (Answer, bool)
	// Record persists a settled question. Nil means nothing is written down.
	Record func(Question, Answer)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	seq     int
	order   []string
	pending map[string]*waiter
	// settled holds answers nobody has collected yet, oldest first in
	// collected.
	//
	// It exists because there is a gap between a question being answered and
	// its asker coming back for the answer: the model is mid-turn deciding to
	// poll, and the human types in exactly that moment. Dropping the question
	// from pending and nothing else would lose the answer and tell the model
	// its question had expired — with the human watching the run, having just
	// answered it.
	settled   map[string]Reply
	collected []string
	closed    bool

	reaper sync.Once
	stop   chan struct{}
}

// NewBroker returns a broker under a policy. Everything else has a default.
func NewBroker(p Policy) *Broker {
	if p == "" {
		p = PolicyUnattended
	}
	return &Broker{Policy: p}
}

func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Broker) hold() time.Duration {
	if b.Hold > 0 {
		return b.Hold
	}
	return DefaultHold
}

// deadline is when a question raised now gives up. A zero deadline means never.
func (b *Broker) deadline(at time.Time) time.Time {
	switch {
	case b.Timeout < 0:
		return time.Time{}
	case b.Timeout == 0:
		return at.Add(DefaultTimeout)
	}
	return at.Add(b.Timeout)
}

func (b *Broker) policy() Policy {
	if b.Policy == "" {
		return PolicyUnattended
	}
	return b.Policy
}

// Attended reports whether questions raised on this broker reach a human.
func (b *Broker) Attended() bool { return b.policy() == PolicyAsk }

// Ask raises a question and waits up to Hold for the answer.
//
// It returns a settled Reply where the answer is already known — remembered
// from an earlier round, or refused because nobody is watching — and otherwise
// an open one carrying the ticket to poll with.
//
// ctx bounds this call only. A cancelled ctx does not settle the question: the
// asker may simply have hit its backend's tool timeout, and throwing the
// question away for that would lose an answer a human is part-way through
// typing.
func (b *Broker) Ask(ctx context.Context, q Question) (Reply, error) {
	q.Text = strings.TrimSpace(q.Text)
	if q.Text == "" {
		return Reply{}, errors.New("ask: a question is required")
	}
	if q.Issue == "" {
		return Reply{}, errors.New("ask: a question must name the issue asking it")
	}
	q.AskedAt = b.now()

	if b.Recall != nil {
		if prev, ok := b.Recall(q); ok && prev.Text != "" {
			prev.Source = SourceRecorded
			return b.instant(q, prev), nil
		}
	}
	if !b.Attended() {
		return b.instant(q, Answer{Text: UnattendedText, Source: SourceUnattended}), nil
	}

	w, err := b.enqueue(q)
	if err != nil {
		return Reply{}, err
	}
	if b.Raise != nil {
		b.Raise(w.q)
	}
	return b.hang(ctx, w), nil
}

// Wait collects the answer to an open question, blocking up to Hold.
//
// A ticket nobody is holding is answered rather than refused: the realistic
// cause is a question that expired or a run that restarted, and there is
// nothing better for the model to do than what UnknownTicketText tells it.
func (b *Broker) Wait(ctx context.Context, ticket string) (Reply, error) {
	b.mu.Lock()
	w, open := b.pending[ticket]
	done, answered := b.settled[ticket]
	b.mu.Unlock()

	switch {
	case open:
		return b.hang(ctx, w), nil
	case answered:
		return done, nil
	}
	return Reply{
		Question: Question{ID: ticket},
		Answer:   Answer{Text: UnknownTicketText, Source: SourceAbandoned},
		Settled:  true,
	}, nil
}

// hang blocks on a waiter for one hold, and reports what it found.
func (b *Broker) hang(ctx context.Context, w *waiter) Reply {
	t := time.NewTimer(b.hold())
	defer t.Stop()
	select {
	case <-w.done:
		return Reply{Question: w.q, Answer: w.answer, Settled: true}
	case <-t.C:
	case <-ctx.Done():
	}
	// Non-blocking re-check: an answer that landed in the same instant as the
	// timer must not become a pointless extra poll.
	select {
	case <-w.done:
		return Reply{Question: w.q, Answer: w.answer, Settled: true}
	default:
	}
	return Reply{Question: w.q}
}

// instant settles a question that never had to wait.
func (b *Broker) instant(q Question, a Answer) Reply {
	if q.ID == "" {
		b.mu.Lock()
		b.seq++
		q.ID = fmt.Sprintf("q%d", b.seq)
		b.mu.Unlock()
	}
	b.finish(q, a)
	return Reply{Question: q, Answer: a, Settled: true}
}

// enqueue registers an open question and starts the reaper that will expire it.
func (b *Broker) enqueue(q Question) (*waiter, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("ask: the run has ended")
	}
	b.seq++
	q.ID = fmt.Sprintf("q%d", b.seq)
	q.Deadline = b.deadline(q.AskedAt)
	w := &waiter{q: q, done: make(chan struct{})}
	if b.pending == nil {
		b.pending = map[string]*waiter{}
	}
	b.pending[q.ID] = w
	b.order = append(b.order, q.ID)
	if b.stop == nil {
		b.stop = make(chan struct{})
	}
	stop := b.stop
	b.reaper.Do(func() { go b.reap(stop) })
	return w, nil
}

// Pending lists the open questions, oldest first. It is what a view renders and
// what a queue of several is read off.
func (b *Broker) Pending() []Question {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Question, 0, len(b.order))
	for _, id := range b.order {
		if w, ok := b.pending[id]; ok {
			out = append(out, w.q)
		}
	}
	return out
}

// Reply implements Responder: it answers a question as the human.
func (b *Broker) Reply(id, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	return b.close(id, Answer{Text: text, Source: SourceHuman})
}

// Decline implements Responder: the human read the question and handed it back.
func (b *Broker) Decline(id string) bool {
	return b.close(id, Answer{Text: DeclinedText, Source: SourceDeclined})
}

// CancelIssue drops every question an issue has open, for a worker that has
// stopped.
//
// It is what keeps a killed worker's question off the screen. Between two polls
// there is nobody parked on the question, so its asker dying is not something
// the broker can notice for itself — the engine has to say so.
func (b *Broker) CancelIssue(issue string) int {
	var ids []string
	b.mu.Lock()
	for _, id := range b.order {
		if w, ok := b.pending[id]; ok && w.q.Issue == issue {
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	n := 0
	for _, id := range ids {
		if b.close(id, Answer{Text: AbandonedText, Source: SourceAbandoned}) {
			n++
		}
	}
	return n
}

// Close settles every open question and stops the reaper. It is safe to call
// twice, and a closed broker refuses new questions rather than parking a worker
// against a run that has ended.
func (b *Broker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	ids := append([]string(nil), b.order...)
	if b.stop != nil {
		close(b.stop)
		b.stop = nil
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.close(id, Answer{Text: AbandonedText, Source: SourceAbandoned})
	}
}

// keptAnswers is how many collected answers are held for a poll that has not
// arrived yet. Small, because the window it covers is one model turn.
const keptAnswers = 64

// close settles one open question, reporting whether it was still open.
func (b *Broker) close(id string, a Answer) bool {
	b.mu.Lock()
	w, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
		b.order = remove(b.order, id)
		if b.settled == nil {
			b.settled = map[string]Reply{}
		}
		b.settled[id] = Reply{Question: w.q, Answer: a, Settled: true}
		b.collected = append(b.collected, id)
		for len(b.collected) > keptAnswers {
			delete(b.settled, b.collected[0])
			b.collected = b.collected[1:]
		}
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	if !w.settle(a) {
		return false
	}
	b.finish(w.q, a)
	return true
}

// finish records and announces a settled question.
//
// Only an answer a human just gave is written down. An unanswered question is
// not answered, and recording "nobody was there" would suppress it on a later
// run where somebody is; a recalled answer is already in the record, and
// writing it again would grow the file once per round.
func (b *Broker) finish(q Question, a Answer) {
	if b.Record != nil && a.Source == SourceHuman {
		b.Record(q, a)
	}
	if b.Settle != nil {
		b.Settle(q, a)
	}
}

// reap expires questions nobody answered.
//
// A goroutine rather than a check inside Wait, because between two polls
// there is no call to check inside — and a view that keeps showing a question
// the run has already given up on is worse than no view.
func (b *Broker) reap(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		now := b.now()
		var due []string
		b.mu.Lock()
		for _, id := range b.order {
			w, ok := b.pending[id]
			if ok && !w.q.Deadline.IsZero() && now.After(w.q.Deadline) {
				due = append(due, id)
			}
		}
		b.mu.Unlock()
		for _, id := range due {
			b.close(id, Answer{Text: TimeoutText, Source: SourceTimeout})
		}
	}
}

// PendingText is the tool result for a question that is still open. It is
// deliberately terse and repeats the ticket, because the model will read this
// once for every poll and every word of it is context it pays for again.
func PendingText(ticket string, hold time.Duration) string {
	return fmt.Sprintf("PENDING %s\n\n"+
		"The question is queued for the human. Nothing else in the run is blocked by it.\n"+
		"Call %s with ticket %q to collect the answer: it waits up to %s and comes back with "+
		"either the answer or PENDING again.\n"+
		"Keep calling it until you get an answer or are told to proceed without one. "+
		"Do not decide this yourself while it is pending, and do not ask it again.",
		ticket, ToolWait, ticket, short(hold))
}

// short renders a duration the way the instruction should read it.
//
// It never rounds down to nothing: "waits up to 0s" would tell a model the tool
// does not wait at all, which is the opposite of what it has to understand.
func short(d time.Duration) string {
	if d <= 0 {
		d = DefaultHold
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	if d < time.Second {
		return "1 second"
	}
	return d.Round(time.Second).String()
}

// Result renders a reply as the text the model receives.
func (r Reply) Result(hold time.Duration) string {
	if r.Settled {
		return r.Answer.Text
	}
	return PendingText(r.Question.ID, hold)
}

func remove(ids []string, want string) []string {
	for i, id := range ids {
		if id == want {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// Normalise reduces a question to the key an earlier answer is looked up by:
// the same words asked again, whatever the spacing or case. It is here rather
// than in whatever stores the answers so that both ends agree on what "the same
// question" means.
func Normalise(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), " ")
}

// SortQuestions orders questions oldest first, which is the order they must be
// answered in: a queue that reorders itself under a human's cursor is a queue
// that gets the wrong answer typed into it.
func SortQuestions(in []Question) {
	sort.SliceStable(in, func(i, j int) bool { return in[i].AskedAt.Before(in[j].AskedAt) })
}
