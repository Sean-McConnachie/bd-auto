package drain

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"bd-auto/internal/ask"
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
	case EventQuestion:
		e.Issue, e.Role = "t-1", runner.RoleWorker
		e.Question = &ask.Question{
			ID: "q1", Issue: "t-1", Role: "worker", Header: "Config key",
			Text:    "Which key should the timeout live under?",
			Options: []ask.Option{{Label: "ask.timeout"}, {Label: "runners.timeout"}},
		}
		e.Text = e.Question.Text
	case EventAnswer:
		e.Issue, e.Role = "t-1", runner.RoleWorker
		e.Question = &ask.Question{ID: "q1", Issue: "t-1", Text: "Which key should the timeout live under?"}
		e.Answer = &ask.Answer{Text: "ask.timeout", Source: ask.SourceHuman}
		e.Text = e.Answer.Text
	case EventIssueEnd:
		e.Issue, e.Outcome, e.Text = "t-1", OutcomeDone, ""
		e.Report = &Report{Issue: "t-1", Outcome: OutcomeDone}
	case EventWaveIntegrating:
		e.Wave, e.Issues = 2, []string{"t-1"}
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

// Fragments are on the bus and off the line-oriented renderers, and both halves
// of that matter: the live view is text-granular only because they are carried,
// and a log stays readable only because they are dropped again.
func TestMessageFragmentsReachTheBusAndNotTheLines(t *testing.T) {
	var got []Event
	bus := NewBus(ObserverFunc(func(e Event) { got = append(got, e) }))
	sink := bus.Sink(1, "t-1")
	runner.Emit(sink, runner.Event{Kind: runner.EventText, Text: "writing\nthe answer"})
	runner.Emit(sink, runner.Event{Kind: runner.EventToolUse, Tool: "Edit"})

	if len(got) != 2 {
		t.Fatalf("the bus carried %d event(s), want both", len(got))
	}
	frag := got[0]
	if !frag.Fragment() {
		t.Fatalf("a text event must be recognisable as a fragment: %+v", frag)
	}
	if frag.Text != "writing the answer" {
		t.Fatalf("the fragment is %q; its newlines must be folded before anything puts it in a cell", frag.Text)
	}
	if got[1].Fragment() {
		t.Fatal("a tool call is not a fragment")
	}

	if line := plainLine(frag); line != "" {
		t.Fatalf("the plain renderer printed a fragment (%q); a wall of tokens buries the tool calls", line)
	}
	if line := plainLine(got[1]); !strings.Contains(line, "Edit") {
		t.Fatalf("the plain renderer dropped the tool call too: %q", line)
	}

	var raw bytes.Buffer
	JSONRenderer(&raw).Observe(frag)
	if raw.Len() != 0 {
		t.Fatalf("the JSON renderer emitted a fragment: %s", raw.String())
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
