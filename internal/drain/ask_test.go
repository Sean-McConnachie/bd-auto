package drain

import (
	"context"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/ask"
	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
)

// askServer opens a question channel for a test, with a hold short enough that
// nothing waits on it.
func askServer(t *testing.T, repo string, p ask.Policy) *ask.Server {
	t.Helper()
	b := ask.NewBroker(p)
	b.Hold = 20 * time.Millisecond
	b.Timeout = -1
	srv, err := ask.Listen(runstate.Dir(repo), b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// The rule the reviewer turns on: a role the config did not name never gets the
// tool, so a reviewer cannot put a question to the author of the work it is
// judging.
func TestOnlyNamedRolesAreOfferedTheTool(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	cfg := withReview(config.Default())
	worker := fake.New(fake.Step{Text: "done", Do: steps(commitWork("a.txt"), closes(iss, "t-1"))})
	reviewer := pass()
	e := engine(t, repo, cfg, iss, worker, reviewer)
	e.Ask = askServer(t, repo, ask.PolicyAsk)

	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatal(err)
	}

	if !offersAsk(worker.Requests()) {
		t.Fatal("the worker was not offered the tool")
	}
	if offersAsk(reviewer.Requests()) {
		t.Fatal("the reviewer was offered the tool")
	}

	// And what the worker was offered has to be usable: the right issue, and
	// both tools on the allowlist the adapter will build from it.
	spec := askSpec(worker.Requests())
	if !strings.Contains(strings.Join(spec.Args, " "), "--issue t-1") {
		t.Fatalf("the tool server is not fixed to the issue: %q", spec.Args)
	}
	if len(spec.Tools) != 2 {
		t.Fatalf("the tool server offers %v", spec.Tools)
	}
}

// A backend that cannot carry tools has to degrade rather than break: the
// worker simply decides for itself, which is what every run did before the tool
// existed.
func TestABackendWithoutToolsIsOfferedNothing(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Text: "done", Do: steps(commitWork("a.txt"), closes(iss, "t-1"))})
	caps := fake.DefaultCaps()
	caps.Tools = false
	worker.SetCaps(caps)

	e := engine(t, repo, config.Default(), iss, worker, fake.New())
	e.Ask = askServer(t, repo, ask.PolicyAsk)

	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("the issue ended %s: %s", rep.Outcome, rep.Reason)
	}
	if offersAsk(worker.Requests()) {
		t.Fatal("a backend that cannot carry tools was handed one anyway")
	}
}

// A run with no question channel is unchanged, which is what makes this whole
// feature safe to have: nothing above the seam behaves differently.
func TestNoBrokerOffersNothing(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Text: "done", Do: steps(commitWork("a.txt"), closes(iss, "t-1"))})
	e := engine(t, repo, config.Default(), iss, worker, fake.New())

	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatal(err)
	}
	if offersAsk(worker.Requests()) {
		t.Fatal("a run with no broker offered a tool server")
	}
}

// A question raised during a run has to reach whatever is watching, and its
// answer has to reach the record. Both ends are what WireAsk exists for.
func TestWireAskPutsQuestionsOnTheBusAndAnswersInRunState(t *testing.T) {
	repo := testRepo(t)
	var seen []Event
	bus := NewBus(ObserverFunc(func(e Event) { seen = append(seen, e) }))

	b := ask.NewBroker(ask.PolicyAsk)
	b.Hold = 20 * time.Millisecond
	b.Timeout = -1
	WireAsk(b, bus, repo)

	reply, err := b.Ask(context.Background(), ask.Question{
		Issue: "t-1", Role: "worker", Header: "Config key",
		Text:    "Which key should the timeout live under?",
		Options: []ask.Option{{Label: "ask.timeout"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Reply(reply.Question.ID, "ask.timeout") {
		t.Fatal("the question could not be answered")
	}

	q := findEvent(seen, EventQuestion)
	if q == nil || q.Issue != "t-1" || q.Question == nil {
		t.Fatalf("the question did not reach the bus: %+v", seen)
	}
	if q.Question.Header != "Config key" {
		t.Fatalf("the question arrived as %+v", q.Question)
	}
	a := findEvent(seen, EventAnswer)
	if a == nil || a.Answer == nil || a.Answer.Text != "ask.timeout" {
		t.Fatalf("the answer did not reach the bus: %+v", seen)
	}

	// And the record, so a re-run does not ask again.
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st.AnswerFor("t-1", "Which key should the timeout live under?")
	if !ok || got.Answer != "ask.timeout" {
		t.Fatalf("run state recorded %+v", st.Questions)
	}

	// Asked again — which is what a resumed worker does — it is answered from
	// the record without troubling anybody.
	seen = nil
	again, err := b.Ask(context.Background(), ask.Question{
		Issue: "t-1", Text: "which key should the TIMEOUT live under?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Settled || again.Answer.Text != "ask.timeout" {
		t.Fatalf("the recorded answer was not replayed: %+v", again)
	}
	if findEvent(seen, EventQuestion) != nil {
		t.Fatal("a question answered from the record was put to the human again")
	}
}

// A worker that stops takes its questions with it. Left alone they would sit on
// screen asking a human to decide something for a process that no longer
// exists.
func TestAFinishedIssueDropsItsQuestions(t *testing.T) {
	repo := testRepo(t)
	srv := askServer(t, repo, ask.PolicyAsk)

	iss := newIssues("t-1")
	e := engine(t, repo, config.Default(), iss,
		fake.New(fake.Step{Text: "done", Do: steps(commitWork("a.txt"), closes(iss, "t-1"))}), fake.New())
	e.Ask = srv

	if _, err := srv.Broker().Ask(context.Background(), ask.Question{Issue: "t-1", Text: "which one?"}); err != nil {
		t.Fatal(err)
	}
	if len(srv.Broker().Pending()) != 1 {
		t.Fatalf("the question was not queued: %+v", srv.Broker().Pending())
	}

	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatal(err)
	}
	if pending := srv.Broker().Pending(); len(pending) != 0 {
		t.Fatalf("a finished issue left %d question(s) queued: %+v", len(pending), pending)
	}
}

func offersAsk(reqs []runner.Request) bool {
	for _, r := range reqs {
		if len(r.ToolServers) > 0 {
			return true
		}
	}
	return false
}

func askSpec(reqs []runner.Request) runner.ToolServer {
	for _, r := range reqs {
		if len(r.ToolServers) > 0 {
			return r.ToolServers[0]
		}
	}
	return runner.ToolServer{}
}

func findEvent(events []Event, kind EventKind) *Event {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}
