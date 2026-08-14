package cmds

import (
	"errors"
	"fmt"
)

// Config implements `bd-auto config <show|validate>`.
func Config(args []string) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "show":
		c, err := NewCtx()
		if err != nil {
			return err
		}
		return emitJSON(map[string]any{
			"path":             configPathOrDefault(c.Cfg),
			"repo_root":        c.RepoRoot,
			"concurrency":      c.Cfg.Concurrency,
			"autonomy":         string(c.Cfg.Autonomy),
			"retry":            c.Cfg.Retry,
			"discovered_work":  c.Cfg.DiscoveredWork,
			"branch_prefix":    c.Cfg.BranchPrefix,
			"report_max_lines": c.Cfg.ReportMaxLines,
			"gate":             gateNames(c.Cfg),
			"pipeline":         describePipeline(c.Cfg),
		})
	case "validate":
		c, err := NewCtx()
		if err != nil {
			return err
		}
		if err := c.Cfg.Validate(); err != nil {
			return err
		}
		fmt.Println("ok: " + configPathOrDefault(c.Cfg))
		return nil
	default:
		return errors.New("usage: bd-auto config <show|validate>")
	}
}
