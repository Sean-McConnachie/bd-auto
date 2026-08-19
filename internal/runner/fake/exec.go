package fake

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"bd-auto/internal/runner"
)

// The fake that actually does something.
//
// `provider: fake` on its own returns a runner that reports success and changes
// nothing, which is what an in-process test wants: the test scripts the work
// itself through Step.Do. Out of process there is no way to script anything, so
// a whole drain under `provider: fake` dispatched five workers that did nothing,
// found no progress, and parked every issue. That exercises the machinery only
// as far as its failure path.
//
// ExtraArgs — the seam's documented per-backend escape hatch — is read here as
// the command to run instead of a model. It runs in the worktree, so a smoke
// test can write a file, commit it and close the issue exactly as a worker
// would, and the drain it is driving cannot tell the difference. That is the
// point: the thing under test is the engine, and a fake that can only fail
// tests half of it.
//
// It is deliberately not a shell. The command is argv, so a config cannot grow
// a quoting bug that only appears on somebody else's machine; a caller that
// wants a shell asks for one, as `["sh", "-c", "..."]`.

// execRunner runs a command in place of a model.
type execRunner struct {
	argv    []string
	timeout time.Duration
}

// Caps says what this fake can do. No resume: a command is not a session, and
// claiming otherwise would have the engine send feedback into a process that
// has no memory of the round it is answering.
func (r *execRunner) Caps() runner.Capabilities { return runner.Capabilities{} }

func (r *execRunner) Run(ctx context.Context, req runner.Request, sink runner.EventSink) (runner.Result, error) {
	emit := func(e runner.Event) {
		e.Role = req.Role
		e.SessionID = req.SessionID
		if e.At.IsZero() {
			e.At = time.Now()
		}
		runner.Emit(sink, e)
	}
	emit(runner.Event{Kind: runner.EventStart})

	timeout := r.timeout
	if req.Timeout > 0 {
		timeout = req.Timeout
	}
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, r.argv[0], r.argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = append(os.Environ(),
		"BD_WORKTREE="+req.Dir,
		"BD_ROLE="+string(req.Role),
		"BD_SESSION="+req.SessionID,
		// The prompt, so a command can assert it was given one rather than
		// silently succeeding against an engine that stopped filling it in.
		"BD_PROMPT="+req.Prompt,
	)
	emit(runner.Event{Kind: runner.EventToolUse, Tool: "Bash(" + r.argv[0] + ")"})

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	res := runner.Result{
		Class:     runner.ClassOK,
		Text:      text,
		SessionID: req.SessionID,
	}
	if err != nil {
		// A command that fails is work that failed, not infrastructure that
		// broke: the engine must treat it as a round to feed back, the same way
		// it treats a model that got it wrong. Classing it as infra would cost
		// the run its retry budget for a test's own assertion failing.
		res.Class = runner.ClassWorkFailed
		res.ExitCode = cmd.ProcessState.ExitCode()
		res.Err = fmt.Errorf("fake: %s: %w", strings.Join(r.argv, " "), err)
		if text != "" {
			res.Text = text
		}
		emit(runner.Event{Kind: runner.EventError, Text: res.Err.Error()})
	}
	emit(runner.Event{Kind: runner.EventDone})
	return res, nil
}

// Name is the provider's, not the command's: the engine reports it in run state
// and in the transcript, and "fake" is the honest answer to what drove this.
func (r *execRunner) Name() string { return Provider }
