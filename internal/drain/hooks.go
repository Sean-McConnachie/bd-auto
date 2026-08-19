package drain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bd-auto/internal/config"
	"bd-auto/internal/pipeline"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
)

// Hooks are where a repo hangs its own reader of a result.
//
// The pipeline stops at the verdict, and everything past it — the barrier, the
// handoff, the run's own report — is Go with nothing attached to it. A hook is
// the attachment point: an agent: role or a run: command, handed the report
// that was just produced, at the moment it was produced. config/hooks.go has
// the decision this implements; what follows is the three things that decision
// costs to keep true.
//
// **It cannot change anything.** runHooks returns results and the callers put
// them on the report. Nothing reads a verdict out of a hook, nothing branches on
// whether one passed, and a hook that fails is a log line. That is not laziness
// about error handling: a hook is a prompt or a script the repo wrote, and the
// engine deciding differently because of it is the exact promise this feature
// does not make.
//
// **It cannot hang.** A run: hook is bounded by pipeline.Exec's own timeout; an
// agent: hook by a context deadline over the whole invocation, retries and all.
// There is no unlimited, which is the one place hooks differ from a runner's
// timeout: 0.
//
// **It cannot be a second writer.** Hooks fire where nothing else is writing —
// on_issue_end after that issue's worker has exited, on_barrier and on_run_end
// with no worker live at all — and on_issue_end is handed one issue, its own.
// A hook is never dispatched for an outcome that is not a verdict, so an
// interrupt or an outage spawns nothing: there is no result to interpret, and
// the run is on its way out.

// HookResult is what one hook did.
//
// OK rather than Passed, deliberately. A stage's Passed is a verdict that routes
// an issue; this says only that the hook ran to completion — exit 0 for a
// command, a finished turn for a model. Nothing in the engine reads it.
type HookResult struct {
	// Point is the hook point, and Name the hook's name within it.
	Point string `json:"point"`
	Name  string `json:"name"`
	// Kind is "agent" or "run", and Role the agent: role for the first of those.
	Kind string `json:"kind"`
	Role string `json:"role,omitempty"`
	// Issue is set for on_issue_end: the issue whose report this hook read.
	Issue string `json:"issue,omitempty"`
	// Input is the report file the hook was handed. It is left on disk, because
	// it is the evidence for whatever the hook said about it.
	Input string `json:"input,omitempty"`

	OK       bool `json:"ok"`
	TimedOut bool `json:"timed_out,omitempty"`
	ExitCode int  `json:"exit_code,omitempty"`
	// Output is what the hook produced: a command's combined output, or a
	// model's final message. Bounded by output_tail_bytes, like everything else
	// that can reach a report.
	Output string `json:"output,omitempty"`
	// Reason says why a hook did not complete. Empty when it did.
	Reason  string       `json:"reason,omitempty"`
	Usage   runner.Usage `json:"usage,omitempty"`
	Seconds float64      `json:"seconds"`
}

// hookDir is where a hook's input report is written, under the main checkout's
// already-gitignored run directory.
const hookDir = "hooks"

// HookInputPath is the file a hook point writes its report to before running
// anything. key discriminates one firing of a point from the next: the issue for
// on_issue_end, the wave for on_barrier, nothing for a run that ends once.
func HookInputPath(repoRoot string, p config.HookPoint, key string) string {
	name := string(p)
	if key != "" {
		name += "-" + safeName(key)
	}
	return filepath.Join(runstate.Dir(repoRoot), hookDir, name+".json")
}

// hookFiring is one point about to fire: what it is handing over, and what to
// call the thing it is handing over.
type hookFiring struct {
	Point config.HookPoint
	// Issue is the issue a hook may write to, and empty where there is none.
	Issue string
	// Key discriminates this firing's input file from the same point's next.
	Key string
	// What describes the report in one noun phrase, for the agent's task text.
	What string
	// Report is marshalled to the input file.
	Report any
}

// issueHooks fires on_issue_end for a finished issue.
func (e *Engine) issueHooks(ctx context.Context, rep Report) []HookResult {
	return e.runHooks(ctx, hookFiring{
		Point:  config.HookIssueEnd,
		Issue:  rep.Issue,
		Key:    rep.Issue,
		What:   "the issue's Report: its outcome, every attempt it took, and what each cost",
		Report: rep,
	})
}

// barrierHooks fires on_barrier for a finished barrier.
func (e *Engine) barrierHooks(ctx context.Context, rep IntegrateReport) []HookResult {
	return e.runHooks(ctx, hookFiring{
		Point:  config.HookBarrier,
		Key:    fmt.Sprintf("wave-%d", rep.Wave),
		What:   "the barrier's IntegrateReport: what merged, what conflicted, what was parked, and what the gate said about the merged result",
		Report: rep,
	})
}

// runEndHooks fires on_run_end for a finished run.
func (e *Engine) runEndHooks(ctx context.Context, rep DrainReport) []HookResult {
	return e.runHooks(ctx, hookFiring{
		Point:  config.HookRunEnd,
		What:   "the run's DrainReport: every issue, every barrier, what was done and parked, and where the run was handed over",
		Report: rep,
	})
}

// runHooks runs one point's hooks in order and returns what they did.
//
// A point with nothing hung on it does no work at all — no file is written and
// no directory created — which is what keeps hooks from being something every
// repo pays for and nobody asked for.
func (e *Engine) runHooks(ctx context.Context, f hookFiring) []HookResult {
	if e.Cfg == nil {
		return nil
	}
	hooks := e.Cfg.HooksAt(f.Point)
	if len(hooks) == 0 {
		return nil
	}
	// A cancelled run is not a result to interpret. The callers already refuse
	// an outcome that is not a verdict; this is the same refusal for the
	// interrupt that lands between the verdict and here.
	if ctx.Err() != nil {
		return nil
	}

	input, err := e.writeHookInput(f)
	if err != nil {
		// Every hook at this point wanted that file, so none of them can run.
		// Reported per hook rather than logged once, so the reason reaches the
		// report a reader is already holding.
		e.logf("warning: could not write the %s hook input: %v", f.Point, err)
		out := make([]HookResult, 0, len(hooks))
		for _, h := range hooks {
			r := newHookResult(f, h)
			r.Reason = "bd-auto could not write the report this hook reads: " + err.Error()
			out = append(out, r)
		}
		return out
	}

	out := make([]HookResult, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, e.runHook(ctx, f, h, input))
	}
	return out
}

// writeHookInput materialises the report a point is handing over.
//
// It is already-defined report JSON — the same Report, IntegrateReport and
// DrainReport a --json run emits — rather than a shape invented for hooks. That
// is the input contract: a hook reads what bd-auto already publishes, so a hook
// written against a report cannot drift from what the run says it did.
//
// The file is left behind. It is small, it is in the gitignored run directory
// beside the transcripts and the review notes, and it is the evidence for
// whatever the hook said about it.
func (e *Engine) writeHookInput(f hookFiring) (string, error) {
	p := HookInputPath(e.RepoRoot, f.Point, f.Key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(f.Report, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, append(raw, '\n'), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

func newHookResult(f hookFiring, h config.Hook) HookResult {
	return HookResult{
		Point: string(f.Point), Name: h.Name, Kind: h.Kind(),
		Role: h.Agent, Issue: f.Issue,
	}
}

// runHook runs one hook and reports what it did, whatever that was.
func (e *Engine) runHook(ctx context.Context, f hookFiring, h config.Hook, input string) HookResult {
	started := time.Now()
	r := newHookResult(f, h)
	r.Input = input

	e.Bus.Emit(Event{
		Kind: EventHookStart, Wave: e.waveNo, Issue: f.Issue,
		Role: runner.Role(h.Agent), Hook: &r,
	})

	switch h.Kind() {
	case "run":
		r = e.runCommandHook(f, h, r)
	case "agent":
		r = e.runAgentHook(ctx, f, h, r)
	default:
		// Validate rejects this at load; reaching it means the config changed
		// underneath the run.
		r.Reason = fmt.Sprintf("hook %q is neither a command nor a role", h.Name)
	}
	r.Seconds = time.Since(started).Seconds()

	if !r.OK {
		e.logf("warning: the %s hook %s did not complete: %s", f.Point, h.Name, firstLine(r.Reason))
	}
	done := r
	e.Bus.Emit(Event{
		Kind: EventHookEnd, Wave: e.waveNo, Issue: f.Issue,
		Role: runner.Role(h.Agent), Passed: r.OK, Text: r.Reason,
		Usage: r.Usage, Hook: &done,
	})
	return r
}

// runCommandHook executes a run: hook in the main checkout.
func (e *Engine) runCommandHook(f hookFiring, h config.Hook, r HookResult) HookResult {
	res := pipeline.Exec(h.Name, h.Run, e.Cfg.HookTimeout(h), e.Cfg.OutputTailBytes, pipeline.Env{
		Issue:      f.Issue,
		Dir:        e.RepoRoot,
		RepoRoot:   e.RepoRoot,
		ReportFile: r.Input,
		Hook:       h.Name,
		HookPoint:  string(f.Point),
	})
	r.OK, r.ExitCode, r.TimedOut, r.Output = res.Passed, res.ExitCode, res.TimedOut, res.Output
	if res.TimedOut {
		r.Reason = fmt.Sprintf("it ran past its %ds timeout and was stopped", e.Cfg.HookTimeout(h))
	} else if !res.Passed {
		r.Reason = fmt.Sprintf("it exited %d", res.ExitCode)
	}
	return r
}

// runAgentHook spawns the model a hook names.
//
// The deadline is the whole point of the shape here: it wraps invoke rather
// than one process, so a hook that meets a rate limit is bounded by its timeout
// including every retry, and a hook cannot turn an outage into an unbounded
// wait at a barrier.
func (e *Engine) runAgentHook(ctx context.Context, f hookFiring, h config.Hook, r HookResult) HookResult {
	role := runner.Role(h.Agent)
	he := e.forHook()
	rn, err := he.runnerFor(role)
	if err != nil {
		r.Reason = err.Error()
		return r
	}

	timeout := time.Duration(e.Cfg.HookTimeout(h)) * time.Second
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, err := he.invoke(hctx, invocation{
		Issue:  f.Issue,
		Role:   role,
		Runner: rn,
		Sess:   &session{},
		// One report, one call. There is no feedback round to resume into: a
		// hook is read once and answers once.
		CanResume: false,
		// Nothing a hook does is an attempt at an issue, and recording one
		// would put a finished issue back in flight and hold its epic open.
		Ephemeral: true,
		Build: func(bool) runner.Request {
			req := e.Cfg.Runner(h.Agent).Request(role)
			req.Dir = e.RepoRoot
			req.SystemPrompt = he.promptFor(role)
			req.Prompt = hookPrompt(f, h, r.Input, e.RepoRoot)
			req.LogPath = LogPath(e.RepoRoot, hookLogKey(f), 0, 0, runner.Role("hook-"+h.Name))
			return req
		},
	})
	r.Usage = c.Usage
	if err != nil {
		r.Reason = err.Error()
		return r
	}

	switch c.Result.Class {
	case runner.ClassInterrupted:
		if hctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			r.TimedOut = true
			r.Reason = fmt.Sprintf("it ran past its %ds timeout and was stopped", e.Cfg.HookTimeout(h))
			return r
		}
		r.Reason = resultReason(c.Result, "it was interrupted")
		return r
	case runner.ClassInfraFailed:
		r.Reason = resultReason(c.Result, "it kept failing on the environment")
		return r
	}

	// No verdict is parsed. A hook is advisory, so its final message is a
	// finding rather than a decision, and reading a VERDICT: out of it would be
	// the first step towards the authority this deliberately does not have.
	r.OK = true
	r.Output = pipeline.Tail([]byte(strings.TrimSpace(c.Result.Text)), e.Cfg.OutputTailBytes)
	return r
}

// forHook clones the engine for a hook's model.
//
// Two things are dropped, for two reasons.
//
// The sink, because a hook's live tool calls have nowhere honest to go:
// on_issue_end runs after its row reached a terminal state, so streaming into it
// would overwrite the outcome a reader is looking at, and the other two points
// have no row at all. What a watcher gets instead is the hook starting and the
// hook finishing, which is what it needs to tell a slow hook from a hung run;
// the whole transcript is on disk at LogPath.
//
// The ask channel, because a hook cannot act on an answer. Putting a question to
// a human from here asks them to decide something nothing will then do — and at
// on_run_end it asks it of a view that is about to close. A hook that meets a
// genuine ambiguity says so in its output, which is the whole of what a hook
// produces anyway.
func (e *Engine) forHook() *Engine {
	c := *e
	c.runners = nil
	c.Sink = nil
	c.Ask = nil
	return &c
}

// hookLogKey names a hook's transcript. The issue where there is one, so a
// hook's log sits beside the worker's it read about; the point otherwise.
func hookLogKey(f hookFiring) string {
	if f.Issue != "" {
		return f.Issue
	}
	if f.Key != "" {
		return string(f.Point) + "-" + f.Key
	}
	return string(f.Point)
}

// hookUsage totals what a point's hooks cost.
func hookUsage(rs []HookResult) runner.Usage {
	var u runner.Usage
	for _, r := range rs {
		u = u.Add(r.Usage)
	}
	return u
}

// verdictOutcome reports whether an outcome is a result to interpret.
//
// An interrupt and an outage are neither. Nothing was judged, the run is on its
// way out or waiting to be re-run, and a hook fired on one would be reading a
// report about work that has not finished happening.
func verdictOutcome(o Outcome) bool {
	return o != OutcomeInterrupted && o != OutcomeInfra
}
