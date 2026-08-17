package drain

import (
	"context"
	"sort"
	"sync"
)

// The control channel is how a run in flight is abandoned.
//
// Everything else in bd-auto bounds a run before it starts: the scope is chosen
// up front and the engine treats it as a hard allowlist. That is the first line
// and it is deliberately the only automatic one. This is the second: a human
// watching a worker dig itself into a hole, or a run that was a mistake five
// minutes ago, needs a way out that does not involve killing the terminal and
// leaving worktrees, branches and half-recorded sessions behind.
//
// Two verbs, and the difference between them is the whole design:
//
//	Kill(issue) ends one worker. The rest of the wave carries on, and the issue
//	            is parked and reported failed, because a human decided against
//	            it — it is a verdict, just not the model's.
//	Stop()      ends the run. Nothing is parked, nothing is judged, and every
//	            worktree, branch and session survives, because re-running the
//	            drain is meant to pick the interrupted issues back up.
//
// Both work by cancelling a context, which is what makes them reach the
// grandchildren: the claude adapter puts each process in its own group and
// signals the group, so a worker forty seconds into `go test ./...` takes the
// test with it rather than leaving it running and holding the worktree.

// KillReason is what a killed issue is recorded as. It is a fixed string
// because a human pressing a key supplies no explanation, and the honest record
// is that the run did not decide this.
const KillReason = "a human killed the worker from the live view; " +
	"the process and everything it had started were terminated"

// StageKilled is the stage recorded against an issue a human ended.
const StageKilled = "killed"

// Control is a run's stop switch, shared between the engine and whatever is
// watching it. A nil *Control is a valid one that no one can press, which is
// what a headless run gets.
type Control struct {
	mu      sync.Mutex
	stop    context.CancelFunc
	stopped bool
	workers map[string]context.CancelFunc
	killed  map[string]string
}

// NewControl returns a control channel with nothing yet attached to it.
func NewControl() *Control {
	return &Control{workers: map[string]context.CancelFunc{}, killed: map[string]string{}}
}

// bind attaches the run's own cancel. A Stop that arrived before the run
// started is honoured immediately rather than lost, because the gap between
// "the command decided to run" and "the engine is running" is exactly where an
// impatient second thought lands.
func (c *Control) bind(stop context.CancelFunc) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stop = stop
	already := c.stopped
	c.mu.Unlock()
	if already {
		stop()
	}
}

// unbind detaches the run's cancel once the run is over.
func (c *Control) unbind() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stop = nil
}

// Stop ends the run. It is not a verdict on anything: workers stop, worktrees,
// branches and sessions stay, and re-running the drain resumes them.
func (c *Control) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stopped = true
	stop := c.stop
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// Stopping reports whether Stop has been pressed.
func (c *Control) Stopping() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// register makes one issue killable for as long as it is dispatched.
func (c *Control) register(issue string, cancel context.CancelFunc) {
	if c == nil || issue == "" {
		return
	}
	c.mu.Lock()
	killed := c.stopped
	c.workers[issue] = cancel
	if _, was := c.killed[issue]; was {
		killed = true
	}
	c.mu.Unlock()
	if killed {
		// Racing the dispatch is normal: a key pressed while the wave was being
		// planned must not land on a worker that started a millisecond later.
		cancel()
	}
}

// unregister takes an issue back out of reach, once it has stopped.
func (c *Control) unregister(issue string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.workers, issue)
}

// Kill ends one worker and everything it started. It reports whether there was
// a worker to end.
func (c *Control) Kill(issue string) bool {
	if c == nil || issue == "" {
		return false
	}
	c.mu.Lock()
	cancel, running := c.workers[issue]
	if running {
		c.killed[issue] = KillReason
	}
	c.mu.Unlock()
	if !running {
		return false
	}
	cancel()
	return true
}

// Killed reports whether an issue was killed, and with what reason. The engine
// asks after the worker returns: a cancelled worker comes back interrupted
// whoever cancelled it, and this is what tells the two apart.
func (c *Control) Killed(issue string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	reason, ok := c.killed[issue]
	return reason, ok
}

// Running lists the issues that can currently be killed, sorted.
func (c *Control) Running() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.workers))
	for id := range c.workers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
