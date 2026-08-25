package cmds

import (
	"errors"
	"fmt"

	"bd-auto/internal/config"
	"bd-auto/internal/graph"
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
			"graph":    describeGraph(c.Cfg, c.RepoRoot),
			"gate":     gateNames(c.Cfg),
			"pipeline": describePipeline(c.Cfg),
			"hooks":    describeHooks(c.Cfg),
			"runners":  describeRunners(c.Cfg),
			"prompts":  describePrompts(c.Cfg),
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

// describePrompts reports where every dispatched role's system prompt came
// from: an agent file by path, the prompt this binary ships, or the reviewer's,
// for a role that has none of its own.
//
// The last of those is why this is here. It used to happen silently — a stage
// naming a role with no prompt was handed the reviewer on the reasoning that a
// custom stage judges a diff — so `agent: security` was the shipped reviewer
// wearing a different model, and nothing on screen said so. A repo has to be
// able to tell a configured agent from an accidental one.
func describePrompts(c *config.Config) map[string]any {
	out := map[string]any{}
	for _, s := range c.PromptSources() {
		e := map[string]any{"source": s.String(), "origin": string(s.Origin), "judging": s.Judging}
		if s.Path != "" {
			e["path"] = s.Path
		}
		out[s.Role] = e
	}
	return out
}

// describeHooks resolves the hooks: block for `config show`, point by point,
// with the timeout every hook resolved to.
//
// The timeout is the reason this reports resolved values rather than echoing
// the file: it is the promise that a hook cannot hang a run, and a hook that
// never wrote one down still has it. A point with nothing hung on it is left
// out entirely, so a repo with no hooks reads as having none rather than as
// three empty lists.
func describeHooks(c *config.Config) map[string]any {
	out := map[string]any{}
	for _, p := range config.HookPoints() {
		hooks := c.HooksAt(p)
		if len(hooks) == 0 {
			continue
		}
		entries := make([]map[string]any, 0, len(hooks))
		for _, h := range hooks {
			e := map[string]any{"name": h.Name, "kind": h.Kind(), "timeout": c.HookTimeout(h)}
			if h.Agent != "" {
				e["agent"] = h.Agent
			}
			if h.Run != "" {
				e["run"] = h.Run
			}
			entries = append(entries, e)
		}
		out[string(p)] = entries
	}
	return out
}

// describeGraph resolves the graph block, and says whether the index it
// describes is actually there.
//
// built is the field that matters. Everything about this block fails open, so
// enabled: true on a machine without graphify is a run that behaves exactly as
// it did before — and the only way to tell that from a working index is to ask
// whether one is on disk.
func describeGraph(c *config.Config, repoRoot string) map[string]any {
	idx := graph.Read(repoRoot)
	return map[string]any{
		"enabled":       c.Graph.Enabled,
		"exclude_tests": c.Graph.ExcludeTests,
		"refresh":       c.Graph.Refresh,
		"roles":         c.Graph.Roles,
		"timeout":       c.Graph.Timeout,
		"graphify":      graph.Available(),
		"built":         idx.Built,
		"state":         idx.Why,
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
	out := append(make([]string, 0, len(roles)), roles...)
	return map[string]any{
		"enabled": c.AskEnabled(),
		"timeout": timeout,
		"hold":    int(c.AskHold().Seconds()),
		"roles":   out,
	}
}
