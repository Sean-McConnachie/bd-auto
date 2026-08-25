package cmds

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bd-auto/internal/config"
)

// Init implements `bd-auto init`: write a starter .beads-auto.yaml, and the
// built-in agents beside it.
//
// It writes into the working directory rather than the repo root, because the
// user asking for a config file has already chosen where they are standing. A
// run still reads the file from the repo root, so `init` in a subdirectory is
// reported back with its full path rather than silently doing nothing useful.
//
// The agents are materialised — worker.md, reviewer.md and integrator.md
// written into .beads-auto/agents/ as though a human had written them — so what
// a run did is readable in this repo's own history, and an upgrade that rewrites
// a shipped prompt cannot change how this repo behaves without a commit saying
// so. What that costs, and what to do about it, is in each file's frontmatter.
func Init(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing config file and agent files")
	dir := fs.String("dir", "", "directory to write into (default: working directory)")
	provider := fs.String("provider", config.DefaultProvider, "runner provider for generated defaults (claude or codex)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !config.ValidInitProvider(*provider) {
		return fmt.Errorf("init: --provider %q is not one of claude, codex", *provider)
	}

	target := *dir
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		target = cwd
	}

	path, err := config.WriteForProvider(target, *provider, *force)
	switch {
	case errors.Is(err, config.ErrConfigExists):
		info("bd-auto: %s already exists; use --force to replace it", path)
		// The agents are still written: a repo initialised before they existed
		// has none, and an existing config file says nothing about that.
		agents, aerr := writeAgents(target, *force)
		if aerr != nil {
			return aerr
		}
		return emitJSON(map[string]any{
			"path":    path,
			"created": false,
			"reason":  "already exists",
			"agents":  agents,
		})
	case err != nil:
		return err
	}

	agents, err := writeAgents(target, *force)
	if err != nil {
		return err
	}

	// Parsing what was just written turns a broken template into a build-time
	// failure rather than a confusing error on the user's first run.
	cfg, err := config.Load(target)
	if err != nil {
		return fmt.Errorf("generated config does not load: %w", err)
	}

	info("bd-auto: wrote %s", path)
	return emitJSON(map[string]any{
		"path":        path,
		"created":     true,
		"provider":    *provider,
		"concurrency": cfg.Concurrency,
		"autonomy":    string(cfg.Autonomy),
		"retry":       cfg.Retry,
		"pipeline":    describePipeline(cfg),
		"agents":      agents,
		"next":        "edit the gate section, then `bd-auto run start --epic <id>`",
	})
}

// writeAgents materialises the built-in agents and says what it did, one line
// per role, so `init` in a repo that already has some of them reads as the
// partial write it is rather than as a silent skip.
func writeAgents(target string, force bool) ([]config.AgentWrite, error) {
	out, err := config.WriteAgents(target, Version, force)
	if err != nil {
		return out, err
	}
	var written []string
	skipped := 0
	for _, w := range out {
		if w.Written {
			written = append(written, w.Role)
			continue
		}
		skipped++
	}
	if len(written) > 0 {
		info("bd-auto: wrote %s for %s", filepath.Join(target, config.AgentsDir()),
			strings.Join(written, ", "))
	}
	if skipped > 0 {
		info("bd-auto: left %d agent file(s) in %s alone; use --force to replace them",
			skipped, config.AgentsDir())
	}
	return out, nil
}
