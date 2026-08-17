package drain

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
	"bd-auto/internal/wave"
	"bd-auto/internal/worktree"
)

// blocks returns a step that reports when it has started and then waits to be
// cancelled. It is the shape of a worker a human decides against: alive, doing
// something, and not about to stop on its own.
func blocks(started chan<- struct{}) fake.Step {
	var once sync.Once
	return fake.Step{
		Delay: 30 * time.Second,
		Do: func(context.Context, runner.Request) error {
			once.Do(func() { close(started) })
			return nil
		},
	}
}

// --- the control channel itself ---

func TestKillOnlyReachesADispatchedWorker(t *testing.T) {
	c := NewControl()
	if c.Kill("t-1") {
		t.Fatal("killing an issue with no worker must report that there was nothing to kill")
	}
	if _, killed := c.Killed("t-1"); killed {
		t.Fatal("a kill that found no worker must not be recorded against the issue")
	}

	var cancelled bool
	c.register("t-1", func() { cancelled = true })
	if !c.Kill("t-1") {
		t.Fatal("a dispatched worker must be killable")
	}
	if !cancelled {
		t.Fatal("Kill must cancel the worker's context; that is what reaches its children")
	}
	if reason, killed := c.Killed("t-1"); !killed || reason == "" {
		t.Fatalf("the kill must be recorded with a reason: %q %v", reason, killed)
	}
	if got := c.Running(); len(got) != 1 || got[0] != "t-1" {
		t.Fatalf("Running() = %v", got)
	}
	c.unregister("t-1")
	if got := c.Running(); len(got) != 0 {
		t.Fatalf("a finished worker must leave the kill list: %v", got)
	}
	// The verdict outlives the worker: the engine asks after it has returned.
	if _, killed := c.Killed("t-1"); !killed {
		t.Fatal("the kill must still be readable once the worker has stopped")
	}
}

// The gap between deciding to run and the engine running is exactly where an
// impatient second thought lands, and a stop that fell into it would be lost.
func TestStopBeforeTheRunStartsIsHonoured(t *testing.T) {
	c := NewControl()
	c.Stop()
	if !c.Stopping() {
		t.Fatal("Stopping() must report a stop that has already been pressed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.bind(cancel)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("binding a run to a control that was already stopped must stop it immediately")
	}

	// Same race one level down: a worker dispatched after the stop.
	var cancelled bool
	c.register("t-1", func() { cancelled = true })
	if !cancelled {
		t.Fatal("a worker dispatched into a stopped run must be cancelled as it registers")
	}
}

// --- k, through the whole engine ---

// The wave has to survive the kill. A killed worker is one issue a human
// decided against, not an outage and not an interrupt: its siblings keep their
// budgets, the barrier still runs, and the issue itself comes back failed rather
// than in flight for the next run to pick up.
func TestKillFailsOneIssueAndLeavesTheWaveRunning(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	started := make(chan struct{})
	workers := newByIssue()
	workers.script("t-1", blocks(started))
	workers.script("t-2", fake.Step{Delay: 50 * time.Millisecond, Text: "done",
		Do: steps(commitWork("two.txt"), closes(iss, "t-2"))})

	e := drainEngine(t, repo, testCfg(3, 1), iss, workers, pass())
	e.Control = NewControl()
	go func() {
		<-started
		e.Control.Kill("t-1")
	}()

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"}, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if rep.Outcome != OutcomeDone {
		t.Fatalf("run outcome %s (%s): a kill is a verdict on one issue, not on the run",
			rep.Outcome, rep.Reason)
	}
	killed := outcomeOf(t, rep, "t-1")
	if killed.Outcome != OutcomeFailed {
		t.Fatalf("the killed issue is %s, want failed", killed.Outcome)
	}
	if killed.Stage != StageKilled {
		t.Fatalf("the killed issue's stage is %q, want %q", killed.Stage, StageKilled)
	}
	if !strings.Contains(killed.Reason, "killed") {
		t.Fatalf("the reason must say what happened: %q", killed.Reason)
	}
	if !has(rep.Done, "t-2") {
		t.Fatalf("the sibling must finish: done=%v parked=%v", rep.Done, rep.Parked)
	}
	if !has(rep.Parked, "t-1") {
		t.Fatalf("a killed issue must be set aside for a human: parked=%v", rep.Parked)
	}

	// Parked, not in flight: the next run must offer neither the issue nor its
	// abandoned session.
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, inflight := st.InFlight["t-1"]; inflight {
		t.Fatal("a killed issue must not stay in flight; that is what a resumed run picks up")
	}
	if !st.IsParked("t-1") {
		t.Fatal("run state does not record the kill")
	}
	_, parked, _ := iss.snapshot()
	if len(parked) != 1 || parked[0] != "t-1" {
		t.Fatalf("bd was not told about the kill: parked=%v", parked)
	}
}

// A wave can hand back more issues than the concurrency cap — that is what a
// resumed wave does — and the ones waiting their turn are on screen, so they are
// things a human can decide against before they ever spawn anything.
//
// The other half of the same test: every issue has to end on the event stream
// whether or not it started, because a watcher that never hears about one shows
// it as still queued, which is the one state it is certainly not in.
func TestAQueuedIssueCanBeKilledBeforeItStarts(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2")

	first := make(chan struct{})
	second := make(chan struct{})
	workers := newByIssue()
	workers.script("t-1", blocks(first))
	workers.script("t-2", blocks(second))

	var mu sync.Mutex
	ended := map[string]Outcome{}
	e := engine(t, repo, testCfg(1, 0), iss, workers, pass())
	e.Bus = NewBus(ObserverFunc(func(ev Event) {
		if ev.Kind != EventIssueEnd {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		ended[ev.Issue] = ev.Outcome
	}))
	e.Control = NewControl()

	// One in flight, one waiting. Which is which is the scheduler's business,
	// so the test asks rather than assumes.
	go func() {
		var running, queued string
		select {
		case <-first:
			running, queued = "t-1", "t-2"
		case <-second:
			running, queued = "t-2", "t-1"
		}
		e.Control.Kill(queued)
		e.Control.Kill(running)
	}()

	reports, err := e.runWave(context.Background(), 1, 1, []wave.Issue{
		{ID: "t-1", Branch: e.Cfg.Branch("t-1")},
		{ID: "t-2", Branch: e.Cfg.Branch("t-2")},
	})
	if err != nil {
		t.Fatalf("runWave: %v", err)
	}

	if len(reports) != 2 {
		t.Fatalf("got %d report(s), want one per issue", len(reports))
	}
	for _, got := range reports {
		if got.Outcome != OutcomeFailed || got.Stage != StageKilled {
			t.Fatalf("%s came back %s at %q, want failed at %q", got.Issue, got.Outcome, got.Stage, StageKilled)
		}
		mu.Lock()
		outcome, sawEnd := ended[got.Issue]
		mu.Unlock()
		if !sawEnd {
			t.Fatalf("%s never ended on the event stream; a watcher would still be showing it as queued", got.Issue)
		}
		if outcome != OutcomeFailed {
			t.Fatalf("%s ended on the stream as %s, want failed", got.Issue, outcome)
		}
	}
	if stop, reason := waveStopped(reports); stop != "" {
		t.Fatalf("a wave of killed issues reads as %s (%s); a kill is a verdict, not an interrupt", stop, reason)
	}
}

// --- q, through the whole engine ---

// Stopping is the other half of the control channel, and the thing that makes
// it usable is that it costs nothing: no verdict, no parked issue, and a re-run
// that continues the same session rather than starting the work again.
func TestStopEndsTheRunWithStateIntactAndResumable(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	started := make(chan struct{})
	workers := newByIssue()
	stopped := workers.script("t-1", blocks(started))

	e := drainEngine(t, repo, testCfg(3, 1), iss, workers, pass())
	e.Control = NewControl()
	go func() {
		<-started
		e.Control.Stop()
	}()

	opts := DrainOptions{Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1}
	first, err := e.Drain(context.Background(), opts)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if first.Outcome != OutcomeInterrupted {
		t.Fatalf("run outcome %s (%s), want interrupted", first.Outcome, first.Reason)
	}
	if len(first.Parked) != 0 {
		t.Fatalf("a stop is nobody's verdict, so nothing may be parked: %v", first.Parked)
	}
	if !exists(worktree.Path(repo, "t-1")) {
		t.Fatal("the worktree must survive a stop; there is nothing to resume without it")
	}
	if len(stopped.Requests()) != 1 {
		t.Fatalf("the stopped run made %d call(s), want 1", len(stopped.Requests()))
	}
	session := stopped.Requests()[0].SessionID

	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != runstate.StatusActive {
		t.Fatalf("run status %q: a stopped run stays armed so a re-run picks it up", st.Status)
	}
	if _, inflight := st.InFlight["t-1"]; !inflight {
		t.Fatal("the stopped issue must stay in flight; that is what the re-run resumes")
	}

	// The re-run, with a fresh control nobody presses.
	stopped.Reset()
	stopped.Script(closeAndCommit(iss, "t-1", "one.txt"))
	e.Control = NewControl()

	second, err := e.Drain(context.Background(), opts)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	reqs := stopped.Requests()
	if len(reqs) == 0 {
		t.Fatal("the re-run dispatched nothing")
	}
	if !reqs[0].Resume || reqs[0].SessionID != session {
		t.Fatalf("the re-run must resume %s rather than restart the issue; got resume=%v session=%s",
			session, reqs[0].Resume, reqs[0].SessionID)
	}
	if second.Outcome != OutcomeDone || !has(second.Done, "t-1") {
		t.Fatalf("the resumed run did not finish the issue: %+v", second)
	}
	got := outcomeOf(t, second, "t-1")
	if len(got.Attempts) != 1 || got.Attempts[0].Attempt != 1 {
		t.Fatalf("a stop must consume no attempt: %+v", got.Attempts)
	}
}
