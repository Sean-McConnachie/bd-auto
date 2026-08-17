package drain

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"bd-auto/internal/runner"
)

// sample builds one event of every kind, populated enough that a renderer has
// something to say about it.
func sample(kind EventKind) Event {
	e := Event{Kind: kind, Wave: 2, Usage: runner.Usage{CostUSD: 0.25}}
	switch kind {
	case EventRunStart:
		e.Issues = []string{"t-1", "t-2"}
		e.Text = "epic-1"
	case EventScopeParked:
		e.Issue, e.Text = "t-3", "dependency t-9 is out of scope"
	case EventWaveStart:
		e.Issues = []string{"t-1", "t-2"}
	case EventIssueStart:
		e.Issue, e.Text = "t-1", "a title"
	case EventActivity:
		e.Issue, e.Role, e.Tool, e.Text = "t-1", runner.RoleWorker, "Edit", "Edit"
	case EventIssueEnd:
		e.Issue, e.Outcome, e.Text = "t-1", OutcomeDone, ""
		e.Report = &Report{Issue: "t-1", Outcome: OutcomeDone}
	case EventWaveEnd:
		e.Integration = &IntegrateReport{
			Wave: 2, GatePassed: true,
			Merges: []Merge{{Issue: "t-1", Outcome: MergeClean}},
		}
	case EventRunEnd:
		e.Run = &DrainReport{Waves: 2, Done: []string{"t-1"}, Outcome: OutcomeDone}
	}
	return e
}

// The renderers are the contract the TUI is written against: a kind the plain
// renderer drops is a kind a headless run cannot see at all, and a kind the JSON
// renderer drops is one nothing can parse.
func TestBothRenderersCoverEveryEventKind(t *testing.T) {
	for _, kind := range AllEventKinds() {
		e := sample(kind)

		var plain bytes.Buffer
		PlainRenderer(&plain).Observe(e)
		if strings.TrimSpace(plain.String()) == "" {
			t.Fatalf("the plain renderer says nothing about %s", kind)
		}

		var raw bytes.Buffer
		JSONRenderer(&raw).Observe(e)
		var decoded Event
		if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
			t.Fatalf("%s did not round-trip through the JSON renderer: %v", kind, err)
		}
		if decoded.Kind != kind {
			t.Fatalf("%s came back as %s", kind, decoded.Kind)
		}
	}
}

// The plain renderer's job is to name what happened, so the facts a reader
// actually needs have to survive the formatting.
func TestPlainRendererNamesWhatHappened(t *testing.T) {
	cases := map[EventKind][]string{
		EventRunStart:    {"t-1", "t-2", "scope"},
		EventScopeParked: {"t-3", "out of scope"},
		EventWaveStart:   {"wave 2", "t-1"},
		EventIssueEnd:    {"t-1", "done"},
		EventWaveEnd:     {"1 merged", "passed"},
		EventPaused:      {"bd-auto run resume"},
		EventRunEnd:      {"2 wave"},
	}
	for kind, wants := range cases {
		got := plainLine(sample(kind))
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Fatalf("%s rendered as %q, which does not mention %q", kind, got, want)
			}
		}
	}
}

// The bus is written by every worker in a wave at once, and a renderer that had
// to be concurrency-safe itself is a renderer nobody would write correctly
// twice.
func TestBusSerialisesConcurrentEmitters(t *testing.T) {
	// Deliberately an unsynchronised map: under -race, this test fails if the
	// bus ever delivers two events at once.
	seen := map[string]int{}
	bus := NewBus(ObserverFunc(func(e Event) { seen[e.Issue]++ }))
	// A nil observer is dropped rather than panicked on.
	bus.Add(nil)

	var wg sync.WaitGroup
	for _, id := range []string{"t-1", "t-2", "t-3", "t-4", "t-5"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sink := bus.Sink(1, id)
			for i := 0; i < 20; i++ {
				runner.Emit(sink, runner.Event{Kind: runner.EventToolUse, Tool: "Read", Role: runner.RoleWorker})
			}
		}(id)
	}
	wg.Wait()

	if len(seen) != 5 {
		t.Fatalf("saw %d issues, want 5: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 20 {
			t.Fatalf("%s emitted %d events, want 20", id, n)
		}
	}
}

// A nil bus has to be usable, because the engine is not going to check.
func TestNilBusDropsEverything(t *testing.T) {
	var bus *Bus
	bus.Emit(Event{Kind: EventRunStart})
	runner.Emit(bus.Sink(1, "t-1"), runner.Event{Kind: runner.EventDone})
}
