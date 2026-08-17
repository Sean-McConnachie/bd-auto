package ask

import (
	"context"
	"strings"
	"testing"
	"time"
)

// serve opens a socket in front of a broker that does not make a test wait.
func serve(t *testing.T, p Policy) *Server {
	t.Helper()
	b := fast(p)
	srv, err := Listen(t.TempDir(), b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// The whole path a real worker takes: a shim on one side, the run's broker on
// the other, and a socket in between.
func TestAQuestionCrossesTheSocketAndTheAnswerComesBack(t *testing.T) {
	srv := serve(t, PolicyAsk)
	srv.Broker().Hold = 2 * time.Second
	shim := Shim{Socket: srv.Path(), Issue: "t-1", Role: "worker"}

	go func() {
		waitForPending(srv.Broker(), 1)
		q := srv.Broker().Pending()[0]
		// The identity comes off the argv, not off the model: a shim fixed to
		// one issue is what stops a worker asking as somebody else.
		if q.Issue != "t-1" || q.Role != "worker" {
			t.Errorf("the question arrived as %s/%s", q.Issue, q.Role)
		}
		srv.Broker().Reply(q.ID, "take the second one")
	}()

	got, err := shim.Handle(context.Background(), Call{
		Tool:     ToolAsk,
		Question: "which one?",
		Options:  []Option{{Label: "a"}, {Label: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "take the second one" {
		t.Fatalf("got %q", got)
	}
}

// The slow path: the first call hands back a ticket, and the poll collects the
// answer. This is what keeps any single call short enough for a backend to hold
// open.
func TestASlowAnswerIsCollectedWithTheTicket(t *testing.T) {
	srv := serve(t, PolicyAsk)
	shim := Shim{Socket: srv.Path(), Issue: "t-1", Role: "worker"}

	pending, err := shim.Handle(context.Background(), Call{Tool: ToolAsk, Question: "which one?"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pending, "PENDING ") {
		t.Fatalf("the first call did not hand back a ticket: %q", pending)
	}
	ticket := strings.Fields(strings.TrimPrefix(pending, "PENDING "))[0]
	// The instruction has to say how long the next call will block, or the
	// model cannot tell a slow answer from a broken tool.
	if !strings.Contains(pending, "1 second") && !strings.Contains(pending, "minutes") {
		t.Fatalf("the instruction does not say how long a poll waits: %q", pending)
	}

	go func() {
		waitForPending(srv.Broker(), 1)
		srv.Broker().Reply(ticket, "the first one")
	}()
	got, err := shim.Handle(context.Background(), Call{Tool: ToolWait, Ticket: ticket})
	if err != nil {
		t.Fatal(err)
	}
	if got != "the first one" {
		t.Fatalf("got %q", got)
	}
}

// Off a terminal there is nobody to ask, and the tool has to say so at once
// rather than parking a worker against a run nobody is watching.
func TestUnattendedCrossesTheSocketWithoutWaiting(t *testing.T) {
	srv := serve(t, PolicyUnattended)
	srv.Broker().Hold = time.Hour // consulted at all and this test hangs
	shim := Shim{Socket: srv.Path(), Issue: "t-1", Role: "worker"}

	done := make(chan string, 1)
	go func() {
		got, err := shim.Handle(context.Background(), Call{Tool: ToolAsk, Question: "which one?"})
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, "best judgement") {
			t.Fatalf("got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an unattended question blocked")
	}
}

// A drain that has exited leaves nothing listening. The worker must be told to
// get on with it rather than left holding a tool error it cannot act on.
func TestAVanishedRunTellsTheModelToProceed(t *testing.T) {
	shim := Shim{Socket: t.TempDir() + "/nothing.sock", Issue: "t-1", Role: "worker"}
	got, err := shim.Handle(context.Background(), Call{Tool: ToolAsk, Question: "which one?"})
	if err != nil {
		t.Fatalf("a dead socket became an error the model cannot use: %v", err)
	}
	if !strings.Contains(got, "best judgement") {
		t.Fatalf("got %q", got)
	}
}

// A malformed call is the model's mistake and comes back as one.
func TestTheShimRefusesAMalformedCall(t *testing.T) {
	srv := serve(t, PolicyAsk)
	shim := Shim{Socket: srv.Path(), Issue: "t-1"}

	if _, err := shim.Handle(context.Background(), Call{Tool: ToolAsk, Question: "  "}); err == nil {
		t.Fatal("an empty question was accepted")
	}
	if _, err := shim.Handle(context.Background(), Call{Tool: ToolWait}); err == nil {
		t.Fatal("a poll with no ticket was accepted")
	}
}

// The spec is what a backend is handed. Everything it needs has to be in it,
// and the issue has to be fixed there rather than left to the model.
func TestSpecNamesTheBinaryTheSocketAndTheIssue(t *testing.T) {
	srv := serve(t, PolicyAsk)
	spec := srv.Spec("t-1", "worker")

	if spec.Name != ServerName {
		t.Fatalf("server name is %q", spec.Name)
	}
	if spec.Command == "" {
		t.Fatal("the spec names no command")
	}
	argv := strings.Join(spec.Args, " ")
	for _, want := range []string{"ask", "--socket " + srv.Path(), "--issue t-1", "--role worker"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv %q is missing %q", argv, want)
		}
	}
	if len(spec.Tools) != 2 {
		t.Fatalf("the spec offers %v", spec.Tools)
	}
	// One call must be allowed to outlast one hold, or the backend kills it
	// before the ticket is ever handed back.
	if spec.Timeout <= srv.Broker().hold() {
		t.Fatalf("a call is allowed %s, which is not longer than the %s hold", spec.Timeout, srv.Broker().hold())
	}
}

// Closing takes the socket with it, so a stale one cannot be dialled by the
// next run.
func TestCloseRemovesTheSocket(t *testing.T) {
	srv := serve(t, PolicyAsk)
	path := srv.Path()
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	shim := Shim{Socket: path, Issue: "t-1"}
	got, err := shim.Handle(context.Background(), Call{Tool: ToolAsk, Question: "which one?"})
	if err != nil {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(got, "best judgement") {
		t.Fatalf("got %q", got)
	}
	// Twice is safe: a deferred close and an explicit one both happen.
	_ = srv.Close()
}
