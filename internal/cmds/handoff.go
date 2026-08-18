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
)

// Handoff implements `bd-auto handoff`: open the pull request for a run that
// already finished.
//
// A drain hands over at the end of its own process, which leaves two ordinary
// situations with no route to a pull request at all: a run that was interrupted,
// and a run whose parked issue a human unparked and fixed by hand. Both have an
// epic branch with the whole result on it. This is the way to hand that branch
// over without re-running the drain.
//
// It runs in the main checkout, on the epic branch, because the gate it runs
// proves the working tree.
func Handoff(args []string) error {
	fs := flag.NewFlagSet("handoff", flag.ContinueOnError)
	force := fs.Bool("force", false,
		"open the pull request over bd-auto's own refusal, when that refusal is a judgement "+
			"about the run — it did not finish, something is parked, the gate is red")
	quiet := fs.Bool("quiet", false, "no live progress on stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}
	if _, err := c.State(); errors.Is(err, runstate.ErrNoRun) {
		return errors.New("no run in this repo, so there is nothing to hand over")
	} else if err != nil {
		return err
	}

	// SIGINT stops the gate, and stops it before anything is published: the push
	// and the pull request are the last two things this does.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng := &drain.Engine{RepoRoot: c.RepoRoot, Cfg: c.Cfg, BD: c.BD}
	if !*quiet {
		eng.Log = func(format string, args ...any) { info(format, args...) }
	}

	h, err := eng.HandoffFromState(ctx, drain.HandoffOptions{Force: *force})
	if err != nil {
		return err
	}
	if err := emitJSON(h); err != nil {
		return err
	}
	if h.URL == "" {
		// The one place --force is worth advertising: a human who has just been
		// refused, and who can see the branch the refusal is about.
		if h.Forceable && !*force && !*quiet {
			info("`bd-auto handoff --force` opens it anyway, over that refusal.")
		}
		return errSilentExit{code: 1}
	}
	return nil
}
