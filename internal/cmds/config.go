package cmds

import (
	"errors"
	"fmt"

	"bd-auto/internal/config"
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
			"path":            configPathOrDefault(c.Cfg),
			"repo_root":       c.RepoRoot,
			"concurrency":     c.Cfg.Concurrency,
			"autonomy":        string(c.Cfg.Autonomy),
			"retry":           c.Cfg.Retry,
			"discovered_work": c.Cfg.DiscoveredWork,
			"branch_prefix":   c.Cfg.BranchPrefix,
			"max_rounds":      c.Cfg.MaxRounds,
			"handoff": map[string]any{
				"branch": c.Cfg.StageOnBranch(),
				"pr":     c.Cfg.OpenPR(),
				"remote": c.Cfg.HandoffRemote(),
				"prefix": c.Cfg.EpicBranchPrefix(),
			},
			"ask":      describeAsk(c.Cfg),
			"gate":     gateNames(c.Cfg),
			"pipeline": describePipeline(c.Cfg),
			"runners":  describeRunners(c.Cfg),
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

// describeAsk resolves the ask block for `config show`.
//
// It is here rather than left out because ask is the block whose defaults
// surprise people: the tool is on unless it is turned off, the reviewer is
// deliberately not among the roles, and hold and timeout are what decide
// whether a question outlives a backend's idle limit. A reader who cannot see
// the resolved values cannot tell a config that was read from one that was not.
func describeAsk(c *config.Config) map[string]any {
	// AskTimeout spells "wait forever" as a negative duration, which is the
	// broker's vocabulary rather than the file's. Report it the way it is
	// written: 0.
	timeout := int(c.AskTimeout().Seconds())
	if timeout < 0 {
		timeout = 0
	}
	roles := c.Ask.Roles
	if roles == nil {
		roles = config.DefaultAskRoles()
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, c.Role(r))
	}
	return map[string]any{
		"enabled": c.AskEnabled(),
		"timeout": timeout,
		"hold":    int(c.AskHold().Seconds()),
		"roles":   out,
	}
}
