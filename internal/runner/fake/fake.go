// Package fake is a scripted runner.Runner: the adapter that makes the whole
// engine testable with zero model calls.
//
// It exists because every interesting property of the engine is a property of
// how it reacts to results — an infra failure must not consume a round, a gate
// failure must resume the same session, a round that changes nothing must fail
// the attempt outright. Those are cheap, exact assertions against a scripted
// sequence of classes, and impossible ones against a live model.
//
// Two things make that work:
//
//   - Every Request is recorded, so a test asserts what was asked for rather
//     than only what came back: the session id, whether Resume was set, the
//     prompt the feedback went into.
//   - A step can Do something — write a file, commit, spawn a grandchild — so
//     the checks the engine runs between rounds see a real worktree.
package fake

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"bd-auto/internal/runner"
)

// Provider is the name this adapter registers under. A repo can put
// `provider: fake` under runners: to drain without spending anything.
const Provider = "fake"

func init() {
	runner.Register(Provider, func(spec runner.Spec) (runner.Runner, error) { return factory(spec) })
}

// Step is one scripted invocation.
//
// A zero Step is a plain success, so a script only has to state what it wants
// to be different.
type Step struct {
	// Class is the result class. Empty means runner.ClassOK.
	Class runner.Class
	// Text is the final assistant message.
	Text string
	// Err is the failure recorded on the Result. A non-OK class with no Err
	// gets a generated one, because the engine logs it.
	Err error
	// ExitCode is the reported exit code. It defaults to 0 for ClassOK and 1
	// otherwise.
	ExitCode int
	// ResetAt is when this call says the outage it reports will lift, as a real
	// backend reports a plan limit. It is what a test scripts to reach the
	// engine's two answers to a limit: wait it out, or stop and say when to
	// come back.
	ResetAt time.Time
	// Denials are the tools this call reports as refused. They are independent
	// of Class on purpose: a real backend reports a refused run as a success,
	// which is exactly the case the engine has to notice.
	Denials []string
	// Usage is what this call reports as costing.
	Usage runner.Usage
	// Events are emitted to the sink in order, before the run finishes. When
	// empty, one text event carrying Text is emitted instead.
	Events []runner.Event
	// Delay makes the call take time, so a test can cancel mid-run. It is
	// interrupted by the context.
	Delay time.Duration
	// Do runs the step's side effects with the worktree the request names. It
	// is what turns a scripted class into a scripted worker: write the file
	// that makes the progress check pass, or spawn the grandchild a
	// cancellation test needs to find dead.
	//
	// An error from Do fails the call with ClassInfraFailed, because a broken
	// fixture is not a verdict on anything.
	Do func(ctx context.Context, req runner.Request) error
	// RunErr makes Run itself return an error, for the "could not produce a
	// Result at all" path.
	RunErr error
}

// Classes builds a script of one step per class, which is the common shape:
// "infra-failed, then ok".
func Classes(classes ...runner.Class) []Step {
	steps := make([]Step, len(classes))
	for i, c := range classes {
		steps[i] = Step{Class: c}
	}
	return steps
}

// Runner is a scripted runner. Its zero value is usable and always succeeds.
//
// It is safe for concurrent use, because the engine runs several of these at
// once and a data race in the test double is indistinguishable from a data
// race in the engine.
type Runner struct {
	// Repeat makes a script that has run out replay its last step instead of
	// failing. It is the default because a loop that runs one round too many
	// should be caught by an assertion on Calls, with a legible failure, rather
	// than by the double falling over.
	Repeat bool
	// PreflightErr is what Preflight reports. It is a field rather than a step
	// because a preflight is not part of the script: it happens once, before
	// the run, and a fake that consumed a step for it would shift every
	// assertion about what the engine asked for.
	PreflightErr error

	mu         sync.Mutex
	steps      []Step
	calls      int
	requests   []runner.Request
	caps       *runner.Capabilities
	preflights []string
}

// New returns a runner that replays steps in order.
func New(steps ...Step) *Runner {
	return &Runner{steps: steps, Repeat: true}
}

// Script replaces the remaining steps and resets the call counter, so one fake
// can serve several phases of a test.
func (r *Runner) Script(steps ...Step) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = steps
	r.calls = 0
}

// Name implements runner.Runner.
func (r *Runner) Name() string { return Provider }

// DefaultCaps is what a fake claims when nothing says otherwise: everything, so
// the engine takes its normal path.
func DefaultCaps() runner.Capabilities {
	return runner.Capabilities{
		Resume:       true,
		Stream:       true,
		ReportsUsage: true,
		Tools:        true,
		Permissions:  runner.AllPermissions(),
	}
}

// SetCaps fixes what this fake claims to support. Use it to reach the degraded
// paths — chiefly a backend without resume, where every feedback round has to
// become a fresh process carrying the feedback in its prompt.
func (r *Runner) SetCaps(c runner.Capabilities) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.caps = &c
}

// Caps implements runner.Runner. Unset, a fake claims everything, so the engine
// takes its normal path.
func (r *Runner) Caps() runner.Capabilities {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.caps != nil {
		return *r.caps
	}
	return DefaultCaps()
}

// Requests returns a copy of every request this runner has been given, in
// order. This is the half of the double that makes exact assertions possible:
// what the engine asked for, not just what it did with the answer.
func (r *Runner) Requests() []runner.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runner.Request(nil), r.requests...)
}

// Calls is how many times Run has been entered.
func (r *Runner) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// Preflight implements runner.Preflighter, so a drain over a fake backend
// takes the same path a real one does — including the one where the backend is
// unusable and nothing should be dispatched at all.
func (r *Runner) Preflight(_ context.Context, dir string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.preflights = append(r.preflights, dir)
	if r.PreflightErr != nil {
		return "", r.PreflightErr
	}
	return "fake", nil
}

// Preflights returns the directory each preflight was asked to check, in
// order. Its length is how many were run, which is what a test asserting that
// one backend is checked once rather than once per role reads.
func (r *Runner) Preflights() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.preflights...)
}

// Reset forgets every recorded request and rewinds the script.
func (r *Runner) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = nil
	r.calls = 0
	r.preflights = nil
}

// next records the request and returns the step for this call.
func (r *Runner) next(req runner.Request) (Step, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	i := r.calls
	r.calls++
	switch {
	case i < len(r.steps):
		return r.steps[i], nil
	case len(r.steps) > 0 && r.Repeat:
		return r.steps[len(r.steps)-1], nil
	case len(r.steps) == 0:
		return Step{}, nil
	}
	return Step{}, fmt.Errorf("fake: call %d has no scripted step (%d scripted)", i+1, len(r.steps))
}

// Run implements runner.Runner.
func (r *Runner) Run(ctx context.Context, req runner.Request, sink runner.EventSink) (runner.Result, error) {
	step, err := r.next(req)
	if err != nil {
		return runner.Result{}, err
	}
	if step.RunErr != nil {
		return runner.Result{}, step.RunErr
	}

	started := time.Now()
	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	emit := func(e runner.Event) {
		e.Role = req.Role
		e.SessionID = req.SessionID
		if e.At.IsZero() {
			e.At = time.Now()
		}
		runner.Emit(sink, e)
	}
	emit(runner.Event{Kind: runner.EventStart})

	res := runner.Result{
		Class:     step.Class,
		Text:      step.Text,
		SessionID: req.SessionID,
		ExitCode:  step.ExitCode,
		Err:       step.Err,
		Denials:   step.Denials,
		Usage:     step.Usage,
		ResetAt:   step.ResetAt,
	}
	if res.Class == "" {
		res.Class = runner.ClassOK
	}

	if step.Do != nil {
		if err := step.Do(runCtx, req); err != nil {
			res.Class = runner.ClassInfraFailed
			res.Err = fmt.Errorf("fake: step: %w", err)
		}
	}
	// The fake provider models a conforming headless worker. Tests concerned
	// with another property need not repeat the lifecycle footer in every step;
	// an explicit WORKER_STATUS line is preserved for malformed/blocked tests.
	if res.Class == runner.ClassOK && strings.Contains(req.Prompt, "WORKER_STATUS") && !strings.Contains(res.Text, "WORKER_STATUS:") {
		if strings.TrimSpace(res.Text) != "" {
			res.Text += "\n"
		}
		res.Text += "WORKER_STATUS: ready"
	}
	if step.Delay > 0 && res.Class != runner.ClassInfraFailed {
		select {
		case <-time.After(step.Delay):
		case <-runCtx.Done():
		}
	}

	// Cancellation outranks the script, exactly as it does for a real backend:
	// a stop or a timeout is not a verdict on the work.
	if runCtx.Err() != nil {
		res.Class = runner.ClassInterrupted
		res.TimedOut = req.Timeout > 0 && ctx.Err() == nil
		if res.Err == nil {
			res.Err = runCtx.Err()
		}
	}

	if len(step.Events) > 0 {
		for _, e := range step.Events {
			emit(e)
		}
	} else if step.Text != "" {
		emit(runner.Event{Kind: runner.EventText, Text: step.Text})
	}

	if res.Class != runner.ClassOK {
		if res.ExitCode == 0 {
			res.ExitCode = 1
		}
		if res.Err == nil {
			res.Err = fmt.Errorf("fake: %s", res.Class)
		}
		emit(runner.Event{Kind: runner.EventError, Text: res.Err.Error()})
	}
	res.Duration = time.Since(started)
	emit(runner.Event{Kind: runner.EventDone, Usage: res.Usage})
	return res, nil
}

var (
	sharedMu sync.Mutex
	shared   *Runner
)

// Install makes r the runner that `provider: fake` resolves to, and returns a
// function that removes it again. It is how a config-driven test — a whole
// drain over a synthetic epic — gets its script in without threading a runner
// through the engine.
//
// Because the registry is process-wide, a test that installs a shared runner
// cannot run in parallel with another that does.
func Install(r *Runner) func() {
	sharedMu.Lock()
	prev := shared
	shared = r
	sharedMu.Unlock()
	return func() {
		sharedMu.Lock()
		shared = prev
		sharedMu.Unlock()
	}
}

// Shared returns the installed runner, or nil.
func Shared() *Runner {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	return shared
}

// factory is what the registry calls. With a runner installed every role shares
// it, so one script covers a whole run and the recorded requests show the order
// the engine asked in.
func factory(spec runner.Spec) (runner.Runner, error) {
	if spec.Permissions != "" && !spec.Permissions.Valid() {
		return nil, fmt.Errorf("fake: %q is not a permission level", spec.Permissions)
	}
	if r := Shared(); r != nil {
		return r, nil
	}
	// ExtraArgs turns the fake into something that can actually do the work.
	// See exec.go: without it a whole drain under `provider: fake` parks every
	// issue, because nothing changes any file.
	if len(spec.ExtraArgs) > 0 {
		return &execRunner{argv: spec.ExtraArgs, timeout: spec.Timeout}, nil
	}
	return New(), nil
}
