// Package cmds implements the bd-auto subcommands.
package cmds

import (
	"encoding/json"
	"fmt"
	"os"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
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
