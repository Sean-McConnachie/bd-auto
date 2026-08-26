package cmds

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"bd-auto/internal/drain"
	"bd-auto/internal/runner"

	// Registers the shipped runner adapters. Without this import runner.New
	// reports every provider as unknown, which is a confusing way to say the
	// binary was built without a backend.
	_ "bd-auto/internal/runner/providers"
)

// Issue implements `bd-auto issue <run>`: one issue, end to end, in this
// process.
//
// It is the whole engine with no wave around it, which is what makes it the
// thing to reach for when something is wrong: one issue, one worktree, one
// transcript per round on disk, and a report that says which check failed.
func Issue(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bd-auto issue run --issue <id>")
	}
	switch args[0] {
	case "run":
		return issueRun(args[1:])
	default:
		return fmt.Errorf("unknown issue subcommand %q", args[0])
	}
}

func issueRun(args []string) error {
	fs := flag.NewFlagSet("issue run", flag.ContinueOnError)
	issue := fs.String("issue", "", "issue ID (required)")
	base := fs.String("base", "", "ref every attempt branches from (default HEAD)")
	rounds := fs.Int("rounds", 0, "feedback rounds per attempt (default from config)")
	retry := fs.Int("retry", -1, "extra attempts after the rounds run out (default from config)")
	quiet := fs.Bool("quiet", false, "no live progress on stderr")
	allowAPIBilling := fs.Bool("allow-api-billing", false, "authorize Codex API-key charges for this command")
	skipPerms := skipPermissions(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *issue == "" {
		return errors.New("--issue is required")
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}
	applySkipPermissions(c, *skipPerms)

	// SIGINT is the interrupt path, not a crash: the engine returns
	// interrupted, the worktree stays, the session stays recorded, and the
	// attempt counter is untouched.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A bus, not a bare sink. The engine raises its stage boundaries on the bus
	// — EventStageStart and EventStageEnd, added by beads-auto-imp-j5a.6 — and
	// with no bus attached they went nowhere, so this command printed nothing at
	// all for the whole of the gate and any `run:` stage. On a repo whose gate is
	// `go test ./...` that is a silent minute or more from the one command whose
	// own documentation calls it the thing to reach for when something is wrong.
	//
	// PlainRenderer owns stderr alone. It already renders activity events with
	// their role, which is what progressSink did, so attaching both printed every
	// tool call twice. The sink comes off the bus for the same reason it does in
	// a wave: one path for every event, rather than two that render differently.
	eng := &drain.Engine{
		RepoRoot:        c.RepoRoot,
		Cfg:             c.Cfg,
		BD:              c.BD,
		BaseRef:         *base,
		MaxRounds:       *rounds,
		AllowAPIBilling: *allowAPIBilling,
	}
	// Do this before opening the question socket or letting Issue create its
	// worktree. API consent is not an interactive prompt and is never inferred.
	eng.Log = func(format string, args ...any) { info(format, args...) }
	if err := eng.AuthorizeBilling(ctx); err != nil {
		return reportBillingRefusal(err, *quiet)
	}
	if *quiet {
		eng.Sink = runner.Discard
	} else {
		eng.Bus = drain.NewBus(drain.PlainRenderer(os.Stderr))
		eng.Watch(0, *issue)
	}
	if *retry >= 0 {
		eng.Retry = retry
	}
	if !*quiet {
		eng.Log = func(format string, args ...any) { info(format, args...) }
	} else {
		eng.Log = nil
	}

	// One issue in this process has no wave table, so there is no way to put a
	// question to anyone. The channel is opened anyway: the worker still gets
	// the tool, and gets told immediately that nobody is watching, which is a
	// far better answer than a tool that is not there and a decision made
	// silently.
	if asker := openAsk(c, eng, false); asker != nil {
		defer asker.Close()
		drain.WireAsk(asker.Broker(), nil, c.RepoRoot)
	}

	rep, err := eng.Issue(ctx, *issue)
	if err != nil {
		return err
	}
	if err := emitJSON(rep); err != nil {
		return err
	}
	if !rep.Done() {
		// A parked or interrupted issue is a real outcome, already reported in
		// the JSON above, so it exits non-zero without a second error line.
		return errSilentExit{code: 1}
	}
	return nil
}

// progressSink renders live model activity to stderr, keeping stdout clean for
// the JSON report.
//
// Text fragments are deliberately dropped: with partial messages on they arrive
// token by token, and a scrolling wall of them hides the tool calls, which are
// what tells you whether a worker is working or stuck.
func progressSink(quiet bool) runner.EventSink {
	if quiet {
		return runner.Discard
	}
	return runner.SinkFunc(func(e runner.Event) {
		switch e.Kind {
		case runner.EventStart:
			info("  [%s] start", e.Role)
		case runner.EventToolUse:
			info("  [%s] %s", e.Role, e.Tool)
		case runner.EventError:
			info("  [%s] error: %s", e.Role, e.Text)
		case runner.EventDone:
			if e.Usage.CostUSD > 0 {
				info("  [%s] done ($%.4f)", e.Role, e.Usage.CostUSD)
			} else {
				info("  [%s] done", e.Role)
			}
		}
	})
}
