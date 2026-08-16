package cmds

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"bd-auto/internal/drain"
	"bd-auto/internal/runstate"

	// Registers the shipped runner adapters, so a merge conflict has something
	// to spawn.
	_ "bd-auto/internal/runner/providers"
)

// Integrate implements `bd-auto integrate`: merge the completed wave into the
// main checkout, gate the merged result, clean up and settle the epic.
//
// It runs in the main checkout by definition — merging is the thing workers are
// forbidden to do — so it works from the repo root rather than the working
// directory, whichever worktree it was invoked from.
func Integrate(args []string) error {
	fs := flag.NewFlagSet("integrate", flag.ContinueOnError)
	all := fs.Bool("all", false, "consider every branch from the run, not just the current wave")
	quiet := fs.Bool("quiet", false, "no live progress on stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}
	if _, err := c.State(); errors.Is(err, runstate.ErrNoRun) {
		return errors.New("no active run")
	} else if err != nil {
		return err
	}

	// SIGINT stops between branches rather than mid-merge: the engine aborts the
	// merge it was in, leaves every branch it has not reached alone, and reports.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng := &drain.Engine{
		RepoRoot: c.RepoRoot,
		Cfg:      c.Cfg,
		BD:       c.BD,
		Sink:     progressSink(*quiet),
	}
	if !*quiet {
		eng.Log = func(format string, args ...any) { info(format, args...) }
	}

	rep, err := eng.Integrate(ctx, drain.IntegrateOptions{All: *all})
	if err != nil {
		return err
	}
	if err := emitJSON(rep); err != nil {
		return err
	}
	// A parked branch is a real outcome already in the JSON, not a second error
	// line. The exit code says only whether the wave landed on a green tree.
	if !rep.GatePassed || rep.Stopped != "" || len(rep.Parked()) > 0 {
		return errSilentExit{code: 1}
	}
	return nil
}
