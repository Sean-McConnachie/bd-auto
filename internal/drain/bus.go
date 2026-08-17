package drain

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"bd-auto/internal/runner"
)

// The event bus is what lets a run be watched without the watcher being wired
// into the engine.
//
// A drain runs several workers at once and each of them streams model activity,
// so there are two problems to solve at once: attributing an event to the issue
// that produced it, and delivering events to a renderer that is not safe for
// concurrent use. The bus does both — it tags what the runner layer cannot know
// (which issue, which wave) and it serialises delivery — which is why the plain
// renderer, the --json renderer and the TUI can all be the same kind of thing.

// EventKind classifies something that happened during a run.
type EventKind string

// The event kinds. Every one of these is rendered by both shipped renderers,
// which is the contract the TUI is written against: a kind the plain renderer
// ignores is a kind the TUI would be alone in showing.
const (
	// EventRunStart opens a run and carries its scope.
	EventRunStart EventKind = "run-start"
	// EventScopeParked is a scoped issue parked before anything ran, because
	// something it depends on was never in the run.
	EventScopeParked EventKind = "scope-parked"
	// EventWaveStart opens a wave and carries the issues in it.
	EventWaveStart EventKind = "wave-start"
	// EventIssueStart is one worker being dispatched.
	EventIssueStart EventKind = "issue-start"
	// EventActivity is live model activity: a tool call, an error, a finished
	// turn. This is the kind that turns a spinner into a progress display.
	EventActivity EventKind = "activity"
	// EventIssueEnd is one issue reaching a terminal outcome.
	EventIssueEnd EventKind = "issue-end"
	// EventWaveEnd is the barrier: what merged, what did not, and the gate.
	EventWaveEnd EventKind = "wave-end"
	// EventPaused is a run stopping at a barrier under autonomy: wave.
	EventPaused EventKind = "paused"
	// EventResumed is that run being let go again.
	EventResumed EventKind = "resumed"
	// EventRunEnd closes a run and carries the whole report.
	EventRunEnd EventKind = "run-end"
)

// AllEventKinds returns every kind, in the order a run emits them. A renderer
// is complete when it handles all of these.
func AllEventKinds() []EventKind {
	return []EventKind{
		EventRunStart, EventScopeParked, EventWaveStart, EventIssueStart,
		EventActivity, EventIssueEnd, EventWaveEnd, EventPaused, EventResumed,
		EventRunEnd,
	}
}

// Event is one thing that happened, addressed to whoever is watching.
type Event struct {
	Kind EventKind `json:"kind"`
	At   time.Time `json:"at"`
	Wave int       `json:"wave,omitempty"`
	// Issue is the issue this is about, empty for run- and wave-level events.
	Issue string `json:"issue,omitempty"`
	// Role and Tool describe live model activity.
	Role runner.Role `json:"role,omitempty"`
	Tool string      `json:"tool,omitempty"`
	// Phase is the underlying runner event kind on EventActivity, and it is
	// there for one reason: Usage on an activity event is the running total of
	// the process in flight, not of the issue. An issue takes several processes
	// — rounds, stages, absorbed infra failures — and each one starts its count
	// again, so a watcher that wants a per-issue total has to know which event
	// closed a process. That event is runner.EventDone.
	Phase runner.EventKind `json:"phase,omitempty"`
	// Text is the human-readable body: a reason, an error, a note.
	Text string `json:"text,omitempty"`
	// Issues is the wave's issues on EventWaveStart, and the run's scope on
	// EventRunStart.
	Issues []string `json:"issues,omitempty"`
	// Outcome is set on EventIssueEnd.
	Outcome Outcome `json:"outcome,omitempty"`
	// Usage is what the thing being reported cost.
	Usage runner.Usage `json:"usage,omitempty"`
	// Report is the finished issue on EventIssueEnd.
	Report *Report `json:"report,omitempty"`
	// Integration is the barrier's result on EventWaveEnd.
	Integration *IntegrateReport `json:"integration,omitempty"`
	// Run is the whole run on EventRunEnd.
	Run *DrainReport `json:"run,omitempty"`
}

// Observer receives run events. It is called from the bus, one at a time, so an
// implementation does not need its own lock.
type Observer interface {
	Observe(Event)
}

// ObserverFunc adapts a function to Observer. A nil ObserverFunc is a valid
// observer that drops everything.
type ObserverFunc func(Event)

// Observe implements Observer.
func (f ObserverFunc) Observe(e Event) {
	if f != nil {
		f(e)
	}
}

// Bus fans run events out to observers, one event at a time.
//
// Serialising here rather than in each renderer is deliberate: five workers
// stream concurrently, and a renderer that has to be concurrency-safe is a
// renderer nobody will write correctly twice.
type Bus struct {
	mu        sync.Mutex
	observers []Observer
}

// NewBus returns a bus feeding observers. Nil observers are dropped, so a
// caller can pass a renderer that may or may not exist.
func NewBus(observers ...Observer) *Bus {
	b := &Bus{}
	for _, o := range observers {
		b.Add(o)
	}
	return b
}

// Add registers an observer.
func (b *Bus) Add(o Observer) {
	if o == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observers = append(b.observers, o)
}

// Emit delivers an event. A nil bus is valid and drops everything, so the
// engine never has to check.
func (b *Bus) Emit(e Event) {
	if b == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, o := range b.observers {
		o.Observe(e)
	}
}

// Sink returns a runner.EventSink that republishes one issue's model activity
// onto the bus.
//
// This is the join the runner layer cannot make for itself: a runner.Event knows
// its role and its session, and nothing about which issue or wave it belongs to.
func (b *Bus) Sink(wave int, issue string) runner.EventSink {
	return runner.SinkFunc(func(re runner.Event) {
		e := Event{
			Kind:  EventActivity,
			At:    re.At,
			Wave:  wave,
			Issue: issue,
			Role:  re.Role,
			Tool:  re.Tool,
			Phase: re.Kind,
			Usage: re.Usage,
			Text:  activityText(re),
		}
		b.Emit(e)
	})
}

// Fragment reports whether an event is a piece of a message the model is still
// writing. With --include-partial-messages these arrive token by token, which
// is what makes a live view text-granular rather than tool-call-granular — and
// what makes them useless to anything that renders a line per event.
func (e Event) Fragment() bool { return e.Kind == EventActivity && e.Phase == runner.EventText }

// activityText is the one line an activity event is worth.
func activityText(re runner.Event) string {
	switch re.Kind {
	case runner.EventStart:
		return "started"
	case runner.EventText:
		return collapse(re.Text)
	case runner.EventToolUse:
		return re.Tool
	case runner.EventToolResult:
		return re.Tool + " returned"
	case runner.EventError:
		return "error: " + re.Text
	case runner.EventDone:
		if re.Usage.CostUSD > 0 {
			return fmt.Sprintf("finished ($%.4f)", re.Usage.CostUSD)
		}
		return "finished"
	}
	return ""
}

// --- renderers ---

// PlainRenderer writes a run to w as one line per event.
//
// It is what runs with no terminal: a skill launcher, CI, a redirected log. The
// TUI shows the same facts arranged differently, so anything this drops is
// something a headless run cannot see at all.
func PlainRenderer(w io.Writer) Observer {
	var mu sync.Mutex // w may be shared with the engine's own logging
	return ObserverFunc(func(e Event) {
		line := plainLine(e)
		if line == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%s %s\n", e.At.Format("15:04:05"), line)
	})
}

// plainLine renders one event. Split out from PlainRenderer so a test can
// assert every kind produces something without capturing a writer.
func plainLine(e Event) string {
	switch e.Kind {
	case EventRunStart:
		return fmt.Sprintf("run start: %d issue(s) in scope: %s", len(e.Issues), join(e.Issues))
	case EventScopeParked:
		return fmt.Sprintf("parked %s before dispatch: %s", e.Issue, e.Text)
	case EventWaveStart:
		return fmt.Sprintf("wave %d: dispatching %d issue(s): %s", e.Wave, len(e.Issues), join(e.Issues))
	case EventIssueStart:
		return fmt.Sprintf("wave %d: %s started", e.Wave, e.Issue)
	case EventActivity:
		// Fragments are dropped here rather than at the bus, because dropping
		// them is a property of rendering one line per event: a wall of tokens
		// buries the tool calls, which are what say whether a worker is working
		// or stuck. A view that overwrites a single cell wants them.
		if e.Text == "" || e.Fragment() {
			return ""
		}
		return fmt.Sprintf("  %s [%s] %s", e.Issue, roleOr(e.Role), e.Text)
	case EventIssueEnd:
		out := fmt.Sprintf("wave %d: %s %s", e.Wave, e.Issue, e.Outcome)
		if e.Text != "" {
			out += " (" + firstLine(e.Text) + ")"
		}
		if e.Usage.CostUSD > 0 {
			out += fmt.Sprintf(" $%.4f", e.Usage.CostUSD)
		}
		return out
	case EventWaveEnd:
		if e.Integration == nil {
			return fmt.Sprintf("wave %d: barrier reached", e.Wave)
		}
		in := e.Integration
		return fmt.Sprintf("wave %d integrated: %d merged, %d parked, gate %s%s",
			e.Wave, len(in.Merged()), len(in.Parked()), passFail(in.GatePassed), suffix(in.Reason))
	case EventPaused:
		return fmt.Sprintf("wave %d: paused at the barrier; `bd-auto run resume` continues", e.Wave)
	case EventResumed:
		return fmt.Sprintf("wave %d: resumed", e.Wave)
	case EventRunEnd:
		if e.Run == nil {
			return "run finished"
		}
		r := e.Run
		return fmt.Sprintf("run finished after %d wave(s): %d done, %d parked, cost $%.4f%s",
			r.Waves, len(r.Done), len(r.Parked), r.Usage.CostUSD, suffix(r.Reason))
	}
	return ""
}

// JSONRenderer writes one JSON object per event to w.
//
// It is the machine half of the same stream the plain renderer prints, so a
// caller that wants to parse a run does not have to parse prose — and it drops
// message fragments for the same reason the plain renderer does: they are
// hundreds of tokens per turn, and everything they say arrives again, whole, in
// the events around them.
func JSONRenderer(w io.Writer) Observer {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	return ObserverFunc(func(e Event) {
		if e.Fragment() {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(e)
	})
}

func roleOr(r runner.Role) string {
	if r == "" {
		return "model"
	}
	return string(r)
}

func join(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	return strings.Join(ids, ", ")
}

// collapse folds a message fragment onto one line, so a watcher can put it in a
// cell without the newlines in it taking the table apart.
//
// Only the line breaks go. Fragments are concatenated by whoever displays them,
// and trimming their spacing would run the words together.
func collapse(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		return r
	}, s)
}

func suffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + firstLine(reason)
}
