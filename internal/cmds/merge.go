package cmds

import (
	"errors"
	"flag"

	"bd-auto/internal/drain"
	"bd-auto/internal/runstate"
)

// MergeOrder implements `bd-auto merge-order`: list the current wave's branches
// in dependency order for the integrator.
//
// It asks the integrator itself rather than working the answer out again. This
// command used to gather its own candidates, and the two had already parted
// company over parked issues — the barrier leaves them out, and this listed
// them as work waiting to land.
func MergeOrder(args []string) error {
	fs := flag.NewFlagSet("merge-order", flag.ContinueOnError)
	all := fs.Bool("all", false, "consider every branch from the run, not just the current wave")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}
	st, err := c.State()
	if errors.Is(err, runstate.ErrNoRun) {
		return errors.New("no active run")
	}
	if err != nil {
		return err
	}

	eng := &drain.Engine{RepoRoot: c.RepoRoot, Cfg: c.Cfg, BD: c.BD}
	return emitJSON(eng.MergeOrder(st, *all))
}
