package drain

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"bd-auto/internal/ask"
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
	// EventQuestion is a worker putting a question to the human and waiting for
	// the answer. It is raised only where somebody could actually answer: a
	// question settled on the spot — from the record, or because nobody is
	// watching — arrives as EventAnswer alone.
	EventQuestion EventKind = "question"
	// EventAnswer is a question being settled, however it was settled. Every
	// question produces exactly one of these.
	EventAnswer EventKind = "answer"
	// EventStageStart is one pipeline stage after implement beginning: the
	// gate, a review, a command the repo added. It exists because most of
	// those stages are silent — the gate and any run: stage execute without a
	// runner, so they emit no activity at all — and a watcher with nothing to
	// show keeps showing the worker's last tool call with the clock still
	// climbing. That reads as a worker that has stalled, which is precisely
	// the reading a live view exists to prevent.
	EventStageStart EventKind = "stage-start"
	// EventStageEnd is that stage's verdict, and the feedback going back to
	// the worker when it failed.
	EventStageEnd EventKind = "stage-end"
	// EventIssueEnd is one issue reaching a terminal outcome.
	EventIssueEnd EventKind = "issue-end"
	// EventWaveIntegrating opens the barrier and carries the branches it is
	// about to merge. It exists because a barrier is not instant: a conflict
	// spawns a model, and a run whose workers have all finished can spend
	// minutes here. Without it a watcher cannot tell integrating from hung.
	EventWaveIntegrating EventKind = "wave-integrating"
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
		EventActivity, EventQuestion, EventAnswer,
		EventStageStart, EventStageEnd, EventIssueEnd,
		EventWaveIntegrating, EventWaveEnd, EventPaused, EventResumed, EventRunEnd,
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
	// Stage names the pipeline stage on EventStageStart and EventStageEnd, and
	// Passed is that stage's verdict on EventStageEnd. Role is set with them
	// when the stage is a model; a gate or a run: stage has no role at all,
	// and Stage is the only name a watcher can call it by.
	//
	// Text on a failed EventStageEnd is the whole feedback going back to the
	// worker, carried for a reader that can hold it — the JSON stream, and a
	// view with room for a transcript. The line renderers say only that the
	// stage failed, because every one of those texts opens by saying so in
	// prose and a log that printed both would say it twice.
	Stage  string `json:"stage,omitempty"`
	Passed bool   `json:"passed,omitempty"`
	// Text is the human-readable body: a reason, an error, a note.
	Text string `json:"text,omitempty"`
	// Issues is the wave's issues on EventWaveStart, the run's scope on
	// EventRunStart, and the branches about to be merged on
	// EventWaveIntegrating.
	Issues []string `json:"issues,omitempty"`
	// Outcome is set on EventIssueEnd.
	Outcome Outcome `json:"outcome,omitempty"`
	// Question is set on EventQuestion and EventAnswer, and Answer on
	// EventAnswer alone.
	Question *ask.Question `json:"question,omitempty"`
	Answer   *ask.Answer   `json:"answer,omitempty"`
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
	case EventQuestion:
		return fmt.Sprintf("  %s [%s] asks: %s%s", e.Issue, roleOr(e.Role),
			questionText(e), optionList(e))
	case EventAnswer:
		return fmt.Sprintf("  %s [%s] %s: %s", e.Issue, roleOr(e.Role),
			answerSource(e), firstLine(answerText(e)))
	case EventStageStart:
		return fmt.Sprintf("  %s [%s] stage started%s", e.Issue, e.Stage, byRole(e.Role))
	case EventStageEnd:
		return fmt.Sprintf("  %s [%s] stage %s%s", e.Issue, e.Stage, passFail(e.Passed), byRole(e.Role))
	case EventIssueEnd:
		out := fmt.Sprintf("wave %d: %s %s", e.Wave, e.Issue, e.Outcome)
		if e.Text != "" {
			out += " (" + firstLine(e.Text) + ")"
		}
		if e.Usage.CostUSD > 0 {
			out += fmt.Sprintf(" $%.4f", e.Usage.CostUSD)
		}
		return out
	case EventWaveIntegrating:
		return fmt.Sprintf("wave %d: integrating %d branch(es): %s", e.Wave, len(e.Issues), join(e.Issues))
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
		return fmt.Sprintf("run finished after %d wave(s): %d done, %d parked%s, cost $%.4f%s%s",
			r.Waves, len(r.Done), len(r.Parked), missingDepsLine(r.MissingDeps),
			r.Usage.CostUSD, suffix(r.Reason), handoffLine(r.Handoff))
	}
	return ""
}

// missingDepsLine says how many parks named an issue running beside them.
//
// It is a count on the run's last line rather than a list, because the list is
// in the report and in the run's notes with the command to go with it. What the
// count buys is a headless run not hiding it: a park that named a sibling is
// the one park shape a human can act on immediately.
func missingDepsLine(deps []MissingDep) string {
	if len(deps) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d naming a wave sibling; see missing_deps)", len(deps))
}

// handoffLine is where the run went. It closes the run's last line because it
// is the one thing a human needs off the end of a drain: the pull request to
// review, or the branch to go and look at and why there is no pull request.
func handoffLine(h *HandoffReport) string {
	switch {
	case h == nil:
		return ""
	case h.URL != "":
		return "; " + h.URL
	case h.Branch != "":
		return "; " + h.Branch + " (" + firstLine(h.Reason) + ")"
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

// questionText is the question an event carries, falling back to the event's
// own text so a renderer never prints an empty question.
func questionText(e Event) string {
	if e.Question != nil && e.Question.Text != "" {
		return collapse(e.Question.Text)
	}
	return collapse(e.Text)
}

// optionList names the answers on offer. A headless reader gets them because
// they are most of what the question means — "which of these" with no list is
// not a question anyone can act on afterwards.
func optionList(e Event) string {
	if e.Question == nil || len(e.Question.Options) == 0 {
		return ""
	}
	return " [" + strings.Join(e.Question.Labels(), " | ") + "]"
}

func answerText(e Event) string {
	if e.Answer != nil && e.Answer.Text != "" {
		return e.Answer.Text
	}
	return e.Text
}

// answerSource says who decided, because "a human chose this" and "nobody was
// there" are the two facts a reader of a finished run needs to tell apart.
func answerSource(e Event) string {
	src := ask.SourceUnattended
	if e.Answer != nil && e.Answer.Source != "" {
		src = e.Answer.Source
	}
	switch src {
	case ask.SourceHuman:
		return "answered"
	case ask.SourceRecorded:
		return "answered earlier in this run"
	case ask.SourceDeclined:
		return "asked, and the human handed it back"
	case ask.SourceTimeout:
		return "asked, and nobody answered in time"
	case ask.SourceAbandoned:
		return "asked, and the question was dropped"
	}
	return "asked, with nobody watching"
}

// byRole names the model behind a stage, and says nothing at all where there is
// none: the gate and a run: stage are this binary executing a command, and
// attributing them to a role would invent one.
func byRole(r runner.Role) string {
	if r == "" {
		return ""
	}
	return " (" + string(r) + ")"
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
