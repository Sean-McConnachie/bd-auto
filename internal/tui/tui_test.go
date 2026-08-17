package tui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"bd-auto/internal/drain"
)

// The bus delivers events while holding its own lock, so a renderer that blocks
// on delivery blocks every worker in the run — and one that drops events shows a
// table that disagrees with the report printed under it. The queue is the only
// thing standing between those two failures.
func TestTheQueueIsNeitherBlockingNorLossy(t *testing.T) {
	q := newQueue()

	const n = 500
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Nothing is reading yet: every one of these has to be absorbed anyway,
		// which is the case that matters — the first events of a run are emitted
		// before the program is up.
		for i := 0; i < n; i++ {
			q.push(drain.Event{Kind: drain.EventActivity, Issue: fmt.Sprintf("t-%d", i)})
		}
		q.close()
	}()
	wg.Wait()

	var got []string
	for {
		items, open := q.take()
		for _, e := range items {
			got = append(got, e.Issue)
		}
		if !open {
			break
		}
	}
	if len(got) != n {
		t.Fatalf("the queue delivered %d of %d events", len(got), n)
	}
	for i, id := range got {
		if want := fmt.Sprintf("t-%d", i); id != want {
			t.Fatalf("event %d is %s, want %s: order is what makes the table match the run", i, id, want)
		}
	}

	// Everything queued before the close still comes out; a run whose last event
	// lands in the same instant it ends must still show it.
	q2 := newQueue()
	q2.push(drain.Event{Kind: drain.EventRunEnd})
	q2.close()
	items, open := q2.take()
	if open || len(items) != 1 {
		t.Fatalf("take() = %d event(s), open=%v; want the queued event and a closed queue", len(items), open)
	}
}

// The whole thing, driven the way the command drives it: events in from another
// goroutine, a table on the writer, and Run returning once the run is over.
func TestTheViewRendersARunAndReturnsWhenItFinishes(t *testing.T) {
	var out bytes.Buffer
	control := drain.NewControl()
	ui := New(Options{Control: control, Output: &out, Input: strings.NewReader("")})

	// Emitted before Run: the first events of a real drain arrive in exactly
	// this window, and a renderer that deadlocked here would hang every run.
	ui.Observe(drain.Event{Kind: drain.EventRunStart, At: time.Now(), Text: "epic-1", Issues: []string{"t-1"}})
	ui.Observe(drain.Event{Kind: drain.EventIssueStart, At: time.Now(), Wave: 1, Issue: "t-1"})

	done := make(chan error, 1)
	go func() { done <- ui.Run(context.Background()) }()

	ui.Observe(drain.Event{Kind: drain.EventIssueEnd, At: time.Now(), Wave: 1, Issue: "t-1",
		Outcome: drain.OutcomeDone, Report: &drain.Report{Issue: "t-1"}})
	ui.Finish()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the run finished")
	}

	view := out.String()
	for _, want := range []string{"epic-1", "t-1", "done"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the rendered table does not show %q:\n%s", want, view)
		}
	}
	if ui.Stopped() {
		t.Fatal("nobody pressed q: the run must not be reported as stopped")
	}
}

// A cancelled context is a signal that has already reached the engine. The view
// follows the run out rather than outliving it and holding the terminal.
func TestACancelledContextClosesTheView(t *testing.T) {
	var out bytes.Buffer
	ui := New(Options{Output: &out, Input: strings.NewReader("")})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ui.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}
