package ask

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fast is a broker that does not make a test wait. The hold is the only thing
// these tests would otherwise spend real seconds on.
func fast(p Policy) *Broker {
	b := NewBroker(p)
	b.Hold = 20 * time.Millisecond
	b.Timeout = -1 // never expire unless a test asks for it
	return b
}

func question(issue, text string, options ...string) Question {
	q := Question{Issue: issue, Text: text}
	for _, o := range options {
		q.Options = append(q.Options, Option{Label: o})
	}
	return q
}

// The whole point of the unattended policy: a drain with nobody watching must
// never park a worker against a question, however long the hold is.
func TestUnattendedAnswersImmediately(t *testing.T) {
	b := NewBroker(PolicyUnattended)
	b.Hold = time.Hour // if this is consulted at all, the test hangs

	start := time.Now()
	reply, err := b.Ask(context.Background(), question("t-1", "which one?", "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Settled {
		t.Fatal("an unattended question came back open")
	}
	if reply.Answer.Source != SourceUnattended {
		t.Fatalf("source is %q", reply.Answer.Source)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("it waited %s before answering", took)
	}
	// The answer has to tell the model what to do instead, or it has learned
	// nothing it did not already know.
	if !strings.Contains(reply.Answer.Text, "best judgement") {
		t.Fatalf("the unattended answer does not say to proceed: %q", reply.Answer.Text)
	}
}

// A human who is watching answers inside the first call, and that is the case
// worth keeping cheap: one tool call, no ticket, nothing to poll.
func TestAnsweredWithinTheHoldNeedsNoTicket(t *testing.T) {
	b := fast(PolicyAsk)
	b.Hold = 2 * time.Second

	go func() {
		waitForPending(b, 1)
		b.Reply(b.Pending()[0].ID, "the second one")
	}()

	reply, err := b.Ask(context.Background(), question("t-1", "which one?", "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Settled {
		t.Fatal("the answer arrived inside the hold but came back as a ticket")
	}
	if reply.Answer.Text != "the second one" || reply.Answer.Source != SourceHuman {
		t.Fatalf("got %+v", reply.Answer)
	}
}

// The case this design exists for: nobody is at the keyboard yet, so the call
// hands back a ticket and the answer is collected later. No single call is ever
// long enough for a backend to kill it.
func TestSlowAnswerArrivesThroughTheTicket(t *testing.T) {
	b := fast(PolicyAsk)

	reply, err := b.Ask(context.Background(), question("t-1", "which one?", "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Settled {
		t.Fatal("the question settled with nobody answering it")
	}
	ticket := reply.Question.ID
	if ticket == "" {
		t.Fatal("an open question came back with no ticket")
	}
	// What the model is told has to name the tool and the ticket, or it cannot
	// come back for the answer.
	text := reply.Result(b.hold())
	if !strings.Contains(text, ticket) || !strings.Contains(text, ToolWait) {
		t.Fatalf("the pending instruction is unusable: %q", text)
	}

	// A poll before the answer lands returns the same ticket, not an error.
	again, err := b.Wait(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if again.Settled || again.Question.ID != ticket {
		t.Fatalf("a poll for an unanswered question came back %+v", again)
	}

	go func() {
		waitForPending(b, 1)
		b.Reply(ticket, "b, because it is reversible")
	}()
	got, err := b.Wait(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Settled || got.Answer.Text != "b, because it is reversible" {
		t.Fatalf("got %+v", got)
	}
	if len(b.Pending()) != 0 {
		t.Fatalf("the answered question is still queued: %+v", b.Pending())
	}
}

// The gap between two polls is where a human most plausibly answers: the model
// is mid-turn deciding to come back, and somebody types in exactly that moment.
// An answer given then has to be waiting when the poll arrives.
func TestAnAnswerGivenBetweenPollsIsStillCollected(t *testing.T) {
	b := fast(PolicyAsk)

	reply, err := b.Ask(context.Background(), question("t-1", "which one?"))
	if err != nil {
		t.Fatal(err)
	}
	ticket := reply.Question.ID

	// Nobody is polling at this instant. This is the whole test.
	if !b.Reply(ticket, "the first one") {
		t.Fatal("the question could not be answered")
	}

	got, err := b.Wait(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Settled || got.Answer.Text != "the first one" {
		t.Fatalf("the answer was lost between polls: %+v", got)
	}
	// A model that polls twice must get the same answer, not a contradiction.
	if again, _ := b.Wait(context.Background(), ticket); again.Answer.Text != "the first one" {
		t.Fatalf("a second poll returned %+v", again)
	}
}

// A ticket nobody is holding is answered rather than refused. The realistic
// cause is a run that restarted, and an error there leaves the model with no
// move at all.
func TestUnknownTicketIsAnsweredNotRefused(t *testing.T) {
	b := fast(PolicyAsk)
	reply, err := b.Wait(context.Background(), "q99")
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Settled || reply.Answer.Source != SourceAbandoned {
		t.Fatalf("got %+v", reply)
	}
	if !strings.Contains(reply.Answer.Text, "best judgement") {
		t.Fatalf("it does not say what to do instead: %q", reply.Answer.Text)
	}
}

// A human may walk away after the view is up, so the timeout has to fire even
// where somebody could in principle have answered.
func TestQuestionTimesOutWithAWatcherPresent(t *testing.T) {
	b := fast(PolicyAsk)
	b.Timeout = 10 * time.Millisecond

	if _, err := b.Ask(context.Background(), question("t-1", "which one?", "a")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(b.Pending()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the question never expired")
		}
		time.Sleep(5 * time.Millisecond)
	}
	reply, err := b.Wait(context.Background(), "q1")
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Settled {
		t.Fatal("the expired question is still open")
	}
	if reply.Answer.Source != SourceAbandoned && reply.Answer.Source != SourceTimeout {
		t.Fatalf("source is %q", reply.Answer.Source)
	}
}

// Several workers may ask at once. One blocked question must not delay another,
// and both must be answerable independently.
func TestConcurrentQuestionsQueueIndependently(t *testing.T) {
	b := fast(PolicyAsk)
	b.Hold = 2 * time.Second

	type result struct {
		issue string
		reply Reply
	}
	out := make(chan result, 2)
	for _, id := range []string{"t-1", "t-2"} {
		go func(id string) {
			r, err := b.Ask(context.Background(), question(id, "which one for "+id+"?", "a", "b"))
			if err != nil {
				t.Error(err)
			}
			out <- result{id, r}
		}(id)
	}

	waitForPending(b, 2)
	pending := b.Pending()
	// Oldest first, because that is the order a queue has to be answered in.
	if pending[0].AskedAt.After(pending[1].AskedAt) {
		t.Fatal("the queue is not in the order the questions arrived")
	}
	for _, q := range pending {
		b.Reply(q.ID, "answer for "+q.Issue)
	}

	for i := 0; i < 2; i++ {
		got := <-out
		if !got.reply.Settled {
			t.Fatalf("%s did not get its answer: %+v", got.issue, got.reply)
		}
		if want := "answer for " + got.issue; got.reply.Answer.Text != want {
			t.Fatalf("%s got %q, which belongs to another issue", got.issue, got.reply.Answer.Text)
		}
	}
}

// A resumed run must not ask what a human has already answered: the worker is a
// fresh process with no memory of having asked.
func TestARecordedAnswerIsNotAskedAgain(t *testing.T) {
	b := fast(PolicyAsk)
	var recorded []Question
	b.Record = func(q Question, _ Answer) { recorded = append(recorded, q) }
	b.Recall = func(q Question) (Answer, bool) {
		if strings.Contains(q.Text, "already") {
			return Answer{Text: "yes, do that", Source: SourceHuman}, true
		}
		return Answer{}, false
	}
	var raised int
	b.Raise = func(Question) { raised++ }

	reply, err := b.Ask(context.Background(), question("t-1", "the one we have already settled?"))
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Settled || reply.Answer.Text != "yes, do that" {
		t.Fatalf("got %+v", reply)
	}
	if reply.Answer.Source != SourceRecorded {
		t.Fatalf("a replayed answer is reported as %q", reply.Answer.Source)
	}
	// Nobody should be shown a question that was answered before it was asked.
	if raised != 0 {
		t.Fatalf("a recalled question was put to the human %d time(s)", raised)
	}
	// And it must not be written down again, or the record grows once a round.
	if len(recorded) != 0 {
		t.Fatalf("a replayed answer was recorded again: %+v", recorded)
	}
}

// Only an answer a human just gave is written down. Recording "nobody was
// there" would suppress the question on a later run where somebody is.
func TestOnlyHumanAnswersAreRecorded(t *testing.T) {
	b := fast(PolicyAsk)
	var recorded []Answer
	b.Record = func(_ Question, a Answer) { recorded = append(recorded, a) }

	if _, err := b.Ask(context.Background(), question("t-1", "answered?")); err != nil {
		t.Fatal(err)
	}
	b.Reply("q1", "this one")

	if _, err := b.Ask(context.Background(), question("t-1", "declined?")); err != nil {
		t.Fatal(err)
	}
	b.Decline("q2")

	if len(recorded) != 1 || recorded[0].Source != SourceHuman {
		t.Fatalf("recorded %+v; only the human answer belongs there", recorded)
	}
}

// A killed worker must not leave a question on screen asking a human to decide
// something for a process that no longer exists.
func TestCancelIssueDropsThatIssuesQuestionsOnly(t *testing.T) {
	b := fast(PolicyAsk)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := b.Ask(context.Background(), question(id, "which one for "+id+"?")); err != nil {
			t.Fatal(err)
		}
	}
	waitForPending(b, 2)

	if n := b.CancelIssue("t-1"); n != 1 {
		t.Fatalf("cancelled %d question(s), want 1", n)
	}
	pending := b.Pending()
	if len(pending) != 1 || pending[0].Issue != "t-2" {
		t.Fatalf("the wrong questions survived: %+v", pending)
	}
}

// A run that ends with questions open has to release them, or the shim polls a
// broker that will never answer.
func TestCloseSettlesEveryOpenQuestion(t *testing.T) {
	b := fast(PolicyAsk)
	if _, err := b.Ask(context.Background(), question("t-1", "which one?")); err != nil {
		t.Fatal(err)
	}
	waitForPending(b, 1)

	b.Close()
	if len(b.Pending()) != 0 {
		t.Fatalf("questions survived the close: %+v", b.Pending())
	}
	if _, err := b.Ask(context.Background(), question("t-2", "and now?")); err == nil {
		t.Fatal("a closed broker accepted a new question")
	}
}

// A malformed call is the model's mistake and has to come back as one, rather
// than parking a worker on an empty question nobody can answer.
func TestAMalformedQuestionIsRefused(t *testing.T) {
	b := fast(PolicyAsk)
	if _, err := b.Ask(context.Background(), Question{Issue: "t-1", Text: "  "}); err == nil {
		t.Fatal("an empty question was accepted")
	}
	if _, err := b.Ask(context.Background(), Question{Text: "who am I?"}); err == nil {
		t.Fatal("a question with no issue was accepted")
	}
}

// A cancelled call must not settle the question: the asker may just have hit
// its own tool timeout, and a human part-way through typing an answer would
// lose it.
func TestACancelledCallLeavesTheQuestionOpen(t *testing.T) {
	b := fast(PolicyAsk)
	b.Hold = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitForPending(b, 1)
		cancel()
	}()
	reply, err := b.Ask(ctx, question("t-1", "which one?"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Settled {
		t.Fatal("a cancelled call settled the question")
	}
	if len(b.Pending()) != 1 {
		t.Fatalf("the question was dropped: %+v", b.Pending())
	}
}

// waitForPending blocks until n questions are queued, so a test never races the
// goroutine that asked them.
func waitForPending(b *Broker, n int) {
	deadline := time.Now().Add(5 * time.Second)
	for len(b.Pending()) < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

// Two goroutines answering the same question must not both win, and neither may
// panic on the closed channel.
func TestOneAnswerWins(t *testing.T) {
	b := fast(PolicyAsk)
	if _, err := b.Ask(context.Background(), question("t-1", "which one?")); err != nil {
		t.Fatal(err)
	}
	waitForPending(b, 1)

	var wg sync.WaitGroup
	won := make(chan bool, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won <- b.Reply("q1", "mine")
		}()
	}
	wg.Wait()
	close(won)

	wins := 0
	for ok := range won {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d answers were accepted for one question", wins)
	}
}

// TestPendingIsOrderedByAskedAtEvenWhenStampsRaceTheLock pins beads-auto-imp-7tu
// deterministically, where the original flake reproduced about one run in eight.
//
// The clock is what makes it deterministic. Ask stamps AskedAt before it takes
// any lock, so a goroutine delayed between stamping and enqueueing lands in the
// queue behind a question stamped after it. This clock forces exactly that: the
// first caller to ask for a time is held up while the second one takes its
// stamp and gets into the queue first.
//
// Before the fix, Pending() returned b.order — enqueue order — while claiming
// "oldest first", so the two disagreed and the assertion below failed every
// time rather than one time in eight.
func TestPendingIsOrderedByAskedAtEvenWhenStampsRaceTheLock(t *testing.T) {
	b := fast(PolicyAsk)
	b.Hold = 2 * time.Second
	// An explicit timeout, so the deadline below is a real time rather than the
	// zero value fast() leaves for "waits forever".
	b.Timeout = time.Hour

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	var delay sync.Once
	b.Now = func() time.Time {
		n := calls.Add(1)
		// Only the very first stamp is held back, and only once: after the
		// re-stamp under the lock there is nothing left to race.
		if n == 1 {
			delay.Do(func() { time.Sleep(80 * time.Millisecond) })
		}
		return base.Add(time.Duration(n) * time.Millisecond)
	}

	for _, id := range []string{"t-1", "t-2"} {
		go func(id string) {
			if _, err := b.Ask(context.Background(), question(id, "which one for "+id+"?", "a", "b")); err != nil {
				t.Error(err)
			}
		}(id)
	}

	waitForPending(b, 2)
	pending := b.Pending()
	if pending[0].AskedAt.After(pending[1].AskedAt) {
		t.Fatalf("Pending says oldest first and returned %s before %s: the queue order and "+
			"the timestamps disagree", pending[0].AskedAt, pending[1].AskedAt)
	}
	// The deadline follows the stamp, so it must have moved with it.
	for _, q := range pending {
		if !q.Deadline.After(q.AskedAt) {
			t.Fatalf("%s has a deadline %s that does not follow its stamp %s", q.ID, q.Deadline, q.AskedAt)
		}
	}
	for _, q := range pending {
		b.Reply(q.ID, "answer")
	}
}
