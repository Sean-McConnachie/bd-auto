package cmds

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"bd-auto/internal/config"
)

// Init implements `bd-auto init`: write a starter .beads-auto.yaml.
//
// It writes into the working directory rather than the repo root, because the
// user asking for a config file has already chosen where they are standing. A
// run still reads the file from the repo root, so `init` in a subdirectory is
// reported back with its full path rather than silently doing nothing useful.
func Init(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing config file")
	dir := fs.String("dir", "", "directory to write into (default: working directory)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := *dir
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		target = cwd
	}

	path, err := config.Write(target, *force)
	switch {
	case errors.Is(err, config.ErrConfigExists):
		info("bd-auto: %s already exists; use --force to replace it", path)
		return emitJSON(map[string]any{
			"path":    path,
			"created": false,
			"reason":  "already exists",
		})
	case err != nil:
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
		"concurrency": cfg.Concurrency,
		"autonomy":    string(cfg.Autonomy),
		"retry":       cfg.Retry,
		"pipeline":    describePipeline(cfg),
		"next":        "edit the gate section, then `bd-auto run start --epic <id>`",
	})
}
