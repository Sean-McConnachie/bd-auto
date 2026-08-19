package drain

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	// EventMergeStart is one branch beginning its trip through the barrier.
	//
	// The four kinds from here to EventWaveRollback are the inside of the
	// barrier, and they exist for the same reason EventWaveIntegrating does,
	// one level down: that event and EventWaveEnd are the two ends of
	// something that takes minutes, and between them a watcher had nothing at
	// all. A barrier is work — it merges, it spawns a model, it gates, it
	// rolls back — and every one of those steps is now something a display can
	// put on a row.
	EventMergeStart EventKind = "merge-start"
	// EventMergeConflict is git stopping on a conflict, and the one model
	// invocation integration ever makes being spawned to resolve it. It is the
	// moment a barrier stops being bookkeeping, and the moment a watcher needs
	// most: from here the branch's row carries the integrator's live tool
	// calls, which is the whole difference between resolving and hung.
	EventMergeConflict EventKind = "merge-conflict"
	// EventMergeEnd is what became of one branch, carrying the merge itself:
	// its outcome, what it conflicted on, what it cost and how long it took.
	//
	// A branch can end twice. A red gate is blamed by peeling merges back off
	// the merged result, so a branch that landed can be parked minutes after it
	// landed — and the second event is the true one.
	EventMergeEnd EventKind = "merge-end"
	// EventWaveGateStart is the gate beginning on the merged result, and
	// EventWaveGateEnd its verdict. They are the barrier's gate rather than an
	// issue's — a wave gates once, on everything together — which is why they
	// are not the EventStageStart a worker's gate raises, and why their kinds
	// say so.
	EventWaveGateStart EventKind = "wave-gate-start"
	// EventWaveGateEnd is that gate's verdict.
	EventWaveGateEnd EventKind = "wave-gate-end"
	// EventWaveRollback is one merge taken back off the merged result, because
	// the gate went red and nothing but peeling can say which branch did it.
	// Each one is followed by another gate, until the tree goes green or every
	// merge is out.
	EventWaveRollback EventKind = "wave-rollback"
	// EventWaveEnd is the barrier: what merged, what did not, and the gate.
	EventWaveEnd EventKind = "wave-end"
	// EventPaused is a run stopping at a barrier under autonomy: wave.
	EventPaused EventKind = "paused"
	// EventResumed is that run being let go again.
	EventResumed EventKind = "resumed"
	// EventHookStart is one of the repo's own hooks beginning, and EventHookEnd
	// what it produced. They exist because a hook is the one thing in a run
	// that nothing else announces: it runs after the result a watcher was
	// waiting for has already been reported, so without these an agent hook
	// spending two minutes at a barrier is a run that has apparently finished
	// and then sat there.
	EventHookStart EventKind = "hook-start"
	// EventHookEnd is that hook's result, carrying the whole HookResult: what
	// it said, what it cost, and why it did not complete where it did not.
	EventHookEnd EventKind = "hook-end"
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
		EventWaveIntegrating, EventMergeStart, EventMergeConflict, EventMergeEnd,
		EventWaveGateStart, EventWaveGateEnd, EventWaveRollback,
		EventWaveEnd, EventPaused, EventResumed,
		EventHookStart, EventHookEnd, EventRunEnd,
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
	// Attempt is which attempt at the issue this belongs to, and Round which
	// turn inside that attempt — counted per stage and zero-based, so a
	// worker's first go is round 0 and a review that has already sent work back
	// once is round 1. They count different things and both are worth having:
	// a round is another turn in the same worktree and the same session, an
	// attempt is that worktree and session thrown away and started again.
	//
	// Attempt is zero on everything that is not one issue's own work — the
	// run- and wave-level events, and the barrier's — and zero there means
	// nothing has said yet rather than attempt zero. Round means nothing
	// without it, so read the pair.
	Attempt int `json:"attempt,omitempty"`
	Round   int `json:"round,omitempty"`
	// Text is the human-readable body: a reason, an error, a note.
	Text string `json:"text,omitempty"`
	// Lane says an event belongs to a continuous run: its integration lane
	// rather than to a barrier between waves. Under autonomy: auto the run
	// merges one issue at a time while every other worker keeps running, so
	// there is no boundary between the workers to draw a block at — the merges
	// are a lane beside them, and a view that showed a barrier would be drawing
	// a stop that is not happening. It is on EventWaveStart for the same
	// reason one level up: a continuous run opens one wave and never opens
	// another, so a view that numbered it would be offering a comparison there
	// is nothing to make. Set on EventWaveStart, EventWaveIntegrating and
	// EventWaveEnd.
	Lane bool `json:"lane,omitempty"`
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
	// Merge is one branch's trip through the barrier, on EventMergeConflict,
	// EventMergeEnd and EventWaveRollback. It is the whole of it — outcome,
	// conflicted paths, usage, seconds — so that a watcher needs nothing from
	// the report at the end to render the barrier as it happens.
	Merge *Merge `json:"merge,omitempty"`
	// Integration is the barrier's result on EventWaveEnd.
	Integration *IntegrateReport `json:"integration,omitempty"`
	// Run is the whole run on EventRunEnd.
	Run *DrainReport `json:"run,omitempty"`
	// Hook is one hook on EventHookStart and EventHookEnd. On the first it is
	// what is about to run; on the second it is the whole result, so a watcher
	// needs nothing from the report at the end to render what a hook said.
	Hook *HookResult `json:"hook,omitempty"`
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

// Marks is where an issue has got to: the attempt in flight, and the round
// inside it.
//
// It is a handle rather than two ints because a sink outlives both. Bus.Sink is
// made once for an issue and republishes every model event that issue ever
// produces, while the two numbers move underneath it — so the sink reads them
// through this instead of closing over whatever they were when the issue
// started. The loop that writes them and the runner goroutine that reads them
// are different goroutines, which is what the atomics are for.
type Marks struct {
	attempt atomic.Int64
	round   atomic.Int64
}

// Set records where the issue has got to. A nil Marks is valid and drops it, so
// an engine with no bus never has to check.
func (m *Marks) Set(attempt, round int) {
	if m == nil {
		return
	}
	m.attempt.Store(int64(attempt))
	m.round.Store(int64(round))
}

// Get is what to tag an event with. A nil Marks knows nothing, which is the
// same answer as an issue that has not started.
func (m *Marks) Get() (attempt, round int) {
	if m == nil {
		return 0, 0
	}
	return int(m.attempt.Load()), int(m.round.Load())
}

// Sink returns a runner.EventSink that republishes one issue's model activity
// onto the bus.
//
// This is the join the runner layer cannot make for itself: a runner.Event knows
// its role and its session, and nothing about which issue or wave it belongs to
// — nor which attempt and round, which marks carries and which nothing else on
// an activity event could be recovered from.
func (b *Bus) Sink(wave int, issue string, marks *Marks) runner.EventSink {
	return runner.SinkFunc(func(re runner.Event) {
		attempt, round := marks.Get()
		e := Event{
			Kind:    EventActivity,
			At:      re.At,
			Wave:    wave,
			Issue:   issue,
			Role:    re.Role,
			Tool:    re.Tool,
			Phase:   re.Kind,
			Usage:   re.Usage,
			Text:    activityText(re),
			Attempt: attempt,
			Round:   round,
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
//
// The one exception to one-line-per-event is a question written straight to a
// terminal, which gets its options on lines of their own — see plainLines.
func PlainRenderer(w io.Writer) Observer {
	var mu sync.Mutex // w may be shared with the engine's own logging
	watched := isTerminal(w)
	return ObserverFunc(func(e Event) {
		lines := plainLines(e, watched)
		if len(lines) == 0 {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		stamp := e.At.Format("15:04:05")
		for _, line := range lines {
			fmt.Fprintf(w, "%s %s\n", stamp, line)
		}
	})
}

// plainLines is what one event writes.
//
// Everything is one line, and a question on a pipe or in a file is too: that
// form is what a grep, a tail and every reader of an existing log expect. On a
// terminal a question is the exception. Its options are the half of it a reader
// has to act on, and behind the text on one line they are a soft-wrapped run
// with no structure, at the one moment in a run where somebody has to read
// carefully before they answer.
func plainLines(e Event, watched bool) []string {
	if watched && e.Kind == EventQuestion {
		return plainQuestion(e)
	}
	line := plainLine(e)
	if line == "" {
		return nil
	}
	return []string{line}
}

// plainQuestion writes a question as a block: who is asking and what they want,
// then one option per line, numbered as the answering view numbers them.
func plainQuestion(e Event) []string {
	lines := []string{fmt.Sprintf("  %s [%s] asks: %s", e.Issue, roleOr(e.Role), questionText(e))}
	if e.Question == nil {
		return lines
	}
	for i, opt := range e.Question.Options {
		line := fmt.Sprintf("    %d. %s", i+1, collapse(opt.Label))
		if opt.Description != "" {
			line += " — " + collapse(opt.Description)
		}
		lines = append(lines, line)
	}
	return lines
}

// isTerminal says whether w is something a person is watching now, as opposed
// to a file or a pipe something will read later. Being a character device is
// the whole test, and it is the right one here: a redirected log, a pipe into
// another process and a buffer in a test all want the one-line form, and all
// three fail it.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// plainLine renders one event. Split out from PlainRenderer so a test can
// assert every kind produces something without capturing a writer.
func plainLine(e Event) string {
	switch e.Kind {
	case EventRunStart:
		return fmt.Sprintf("run start: %d issue(s) in scope: %s", len(e.Issues), join(e.Issues))
	case EventScopeParked:
		// Not "before dispatch": the same kind is emitted by the end-of-run
		// sweep, and a line reading "parked X before dispatch: the run drained
		// without ever offering it" says two contradictory things about when it
		// happened. The reason says when; this says what.
		return fmt.Sprintf("scope parked %s: %s", e.Issue, e.Text)
	case EventWaveStart:
		if e.Lane {
			return fmt.Sprintf("dispatching %d issue(s): %s", len(e.Issues), join(e.Issues))
		}
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
		return fmt.Sprintf("  %s [%s] stage started%s%s", e.Issue, e.Stage, byRole(e.Role), whereAt(e))
	case EventStageEnd:
		return fmt.Sprintf("  %s [%s] stage %s%s%s", e.Issue, e.Stage, passFail(e.Passed), byRole(e.Role), whereAt(e))
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
		if e.Lane {
			if len(e.Issues) == 0 {
				return ""
			}
			return fmt.Sprintf("integrating %s while the other workers run", join(e.Issues))
		}
		return fmt.Sprintf("wave %d: integrating %d branch(es): %s", e.Wave, len(e.Issues), join(e.Issues))
	case EventMergeStart:
		return fmt.Sprintf("  %s: merging %s", e.Issue, mergeBranchOf(e))
	case EventMergeConflict:
		return fmt.Sprintf("  %s: %s conflicts in %s; a model is resolving them",
			e.Issue, mergeBranchOf(e), join(conflictsOf(e)))
	case EventMergeEnd:
		return "  " + e.Issue + ": " + mergeLine(e)
	case EventWaveGateStart:
		return fmt.Sprintf("wave %d: gating the merged result%s", e.Wave, suffix(e.Text))
	case EventWaveGateEnd:
		return fmt.Sprintf("wave %d: the gate on the merged result %s%s",
			e.Wave, passFail(e.Passed), suffix(firstLine(e.Text)))
	case EventWaveRollback:
		return fmt.Sprintf("  %s: rolled %s back off the merged result to find out what the gate is red on",
			e.Issue, mergeBranchOf(e))
	case EventWaveEnd:
		if e.Integration == nil {
			return fmt.Sprintf("wave %d: barrier reached", e.Wave)
		}
		in := e.Integration
		if e.Lane {
			if len(in.Merges) == 0 {
				return ""
			}
			return fmt.Sprintf("integrated %s: %d merged, %d parked, gate %s%s",
				join(candidates(in)), len(in.Merged()), len(in.Parked()),
				passFail(in.GatePassed), suffix(in.Reason))
		}
		return fmt.Sprintf("wave %d integrated: %d merged, %d parked, gate %s%s",
			e.Wave, len(in.Merged()), len(in.Parked()), passFail(in.GatePassed), suffix(in.Reason))
	case EventPaused:
		if e.Text != "" {
			return e.Text + "; `bd-auto run resume` continues"
		}
		return fmt.Sprintf("wave %d: paused at the barrier; `bd-auto run resume` continues", e.Wave)
	case EventResumed:
		return fmt.Sprintf("wave %d: resumed", e.Wave)
	case EventHookStart:
		return "hook " + hookLabel(e) + " started" + byRole(e.Role)
	case EventHookEnd:
		if e.Passed {
			return "hook " + hookLabel(e) + " finished" + suffix(firstLine(hookOutputOf(e)))
		}
		return "hook " + hookLabel(e) + " did not complete" + suffix(firstLine(e.Text))
	case EventRunEnd:
		if e.Run == nil {
			return "run finished"
		}
		r := e.Run
		return fmt.Sprintf("run finished %s: %d done, %d parked%s, cost $%.4f%s%s",
			runShape(*r), len(r.Done), len(r.Parked), missingDepsLine(r.MissingDeps),
			r.Usage.CostUSD, suffix(r.Reason), handoffLine(r.Handoff))
	}
	return ""
}

// hookLabel names a hook the way its configuration does: the point it hangs
// off and its own name. It falls back rather than rendering nothing, because an
// event a renderer drops is a hook a headless run cannot see ran at all.
func hookLabel(e Event) string {
	if e.Hook == nil {
		return "(unnamed)"
	}
	switch {
	case e.Hook.Point != "" && e.Hook.Name != "":
		return e.Hook.Point + "/" + e.Hook.Name
	case e.Hook.Name != "":
		return e.Hook.Name
	case e.Hook.Point != "":
		return e.Hook.Point
	}
	return "(unnamed)"
}

// hookOutputOf is what a finished hook said, for the one line a renderer has.
func hookOutputOf(e Event) string {
	if e.Hook == nil {
		return ""
	}
	return e.Hook.Output
}

// runShape says how a run scheduled itself, in the words its own report uses.
// A continuous run is one wave for its whole life, so counting waves at it
// would say the same thing as a wave run that finished in one.
func runShape(r DrainReport) string {
	if r.Continuous {
		return "continuously"
	}
	return fmt.Sprintf("after %d wave(s)", r.Waves)
}

// candidates names the branches one integration was about.
func candidates(in *IntegrateReport) []string {
	out := make([]string, 0, len(in.Merges))
	for _, m := range in.Merges {
		out = append(out, m.Issue)
	}
	return out
}

// whereAt says which attempt and which turn of it a stage line belongs to.
//
// The wave table puts the same two numbers in the ATT column and in the state
// cell. A headless log has no columns, so it says them in words — and it says
// them here rather than on every activity line because the stage boundaries
// already bracket those, and a log that repeated the pair on every tool call
// would bury the tool call.
//
// Round is the number the table shows: zero-based, so the first turn of a stage
// is round 0.
func whereAt(e Event) string {
	if e.Attempt == 0 {
		return ""
	}
	return fmt.Sprintf(" (attempt %d, round %d)", e.Attempt, e.Round)
}

// mergeBranchOf names the branch a barrier event is about, falling back to the
// issue so a renderer never prints an empty branch.
func mergeBranchOf(e Event) string {
	if e.Merge != nil && e.Merge.Branch != "" {
		return e.Merge.Branch
	}
	if e.Text != "" {
		return e.Text
	}
	return e.Issue
}

func conflictsOf(e Event) []string {
	if e.Merge == nil {
		return nil
	}
	return e.Merge.Conflicts
}

// mergeLine is what became of one branch, in the words the outcome deserves.
//
// Clean and resolved both landed and read differently on purpose: what a
// reader of a finished run wants to know is where the money went, and a
// resolved merge is the only merge that spent any.
func mergeLine(e Event) string {
	m := e.Merge
	if m == nil {
		return "the barrier reached no verdict on " + mergeBranchOf(e)
	}
	out := ""
	switch m.Outcome {
	case MergeClean:
		out = "merged " + m.Branch + " cleanly"
	case MergeResolved:
		out = "merged " + m.Branch + " after " + m.resolution()
	case MergeParked:
		out = "parked " + m.Branch + suffix(m.Reason)
	case MergeSkipped:
		out = "left " + m.Branch + " for the next barrier" + suffix(m.Reason)
	default:
		out = "the barrier reached no verdict on " + m.Branch + suffix(m.Reason)
	}
	if m.Usage.CostUSD > 0 {
		out += fmt.Sprintf(" $%.4f", m.Usage.CostUSD)
	}
	return out
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
