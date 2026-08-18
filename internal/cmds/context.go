// Package cmds implements the bd-auto subcommands.
package cmds

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
)

// Ctx is the resolved environment every subcommand works from.
type Ctx struct {
	// RepoRoot is the MAIN checkout, even when invoked from a worktree, so all
	// workers share one run state.
	RepoRoot string
	// Cwd is where the command was invoked, which for a worker is its worktree.
	Cwd string
	Cfg *config.Config
	BD  *bd.Client
}

// NewCtx resolves the repo root, loads config and prepares a bd client.
func NewCtx() (*Ctx, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root := os.Getenv("BD_AUTO_REPO")
	if root == "" {
		root, err = bd.RepoRoot(cwd)
		if err != nil {
			return nil, err
		}
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	return &Ctx{RepoRoot: root, Cwd: cwd, Cfg: cfg, BD: bd.New(cwd)}, nil
}

// State loads the current run state.
func (c *Ctx) State() (*runstate.State, error) { return runstate.Load(c.RepoRoot) }

// skipPermissionsUsage is the help text for --dangerously-skip-permissions. It
// is shared so the flag reads identically wherever it appears: a flag that
// works on drain but not on issue run, or means something slightly different on
// each, is worse than no flag.
const skipPermissionsUsage = "run every model with permission checks off " +
	"(DANGEROUS: the reviewer loses its read-only tool allowlist too, keeping only its deny rules)"

// skipPermissions is the flag both commands that spawn models register.
func skipPermissions(fs *flag.FlagSet) *bool {
	return fs.Bool("dangerously-skip-permissions", false, skipPermissionsUsage)
}

// applySkipPermissions narrows the flag onto the resolved config, and says so.
//
// The warning is not decoration. The flag turns off the one check that would
// have stopped a model touching something outside its worktree, and a run that
// did that silently would be a run nobody could reconstruct afterwards.
func applySkipPermissions(c *Ctx, on bool) {
	if !on {
		return
	}
	c.Cfg.ForcePermissions = runner.PermBypass
	info("--dangerously-skip-permissions: every model in this run has permission checks off, " +
		"the reviewer included. What still contains them is git: a worktree per issue, a branch " +
		"per issue, and hooks that refuse push, merge and rebase — and the roles' denied_tools, " +
		"which are checked ahead of the permission level, so a reviewer still cannot write issue state.")
}

// emitJSON writes v to stdout as indented JSON.
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// info writes a human-readable line to stderr, keeping stdout clean for the
// JSON a caller parses.
func info(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
