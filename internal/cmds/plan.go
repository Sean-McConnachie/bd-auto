package cmds

import (
	"errors"
	"flag"
	"fmt"

	"bd-auto/internal/config"
	"bd-auto/internal/runstate"
	"bd-auto/internal/wave"
)

// Plan implements `bd-auto plan`: compute the next wave.
//
// The planning itself lives in internal/wave; this handles flags, run state and
// JSON.
func Plan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	dispatch := fs.Bool("dispatch", false, "record the wave as in-flight and return dispatch payload")
	limit := fs.Int("limit", 0, "override the concurrency cap for this wave")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}
	st, err := c.State()
	if errors.Is(err, runstate.ErrNoRun) {
		return errors.New("no active run: start one with `bd-auto run start --epic <id>`")
	}
	if err != nil {
		return err
	}

	cap := st.Concurrency
	if *limit > 0 {
		cap = *limit
	}

	res, err := wave.Plan(c.BD, st, wave.Options{Concurrency: cap, Branch: c.Cfg.Branch})
	if err != nil {
		return err
	}

	payload := map[string]any{
		"epic":        st.Epic,
		"wave":        st.Wave,
		"issues":      res.Issues,
		"in_flight":   st.Remaining(),
		"drained":     res.Drained,
		"autonomy":    st.Autonomy,
		"concurrency": cap,
		"pipeline":    describePipeline(c.Cfg),
		"gate":        gateNames(c.Cfg),
	}

	if !*dispatch {
		payload["dispatched"] = false
		return emitJSON(payload)
	}

	if len(res.Issues) == 0 {
		payload["dispatched"] = false
		payload["note"] = "nothing to dispatch"
		return emitJSON(payload)
	}

	updated, err := wave.Record(c.RepoRoot, res.Issues)
	if err != nil {
		return err
	}

	payload["dispatched"] = true
	payload["wave"] = updated.Wave
	return emitJSON(payload)
}

func describePipeline(cfg *config.Config) []map[string]any {
	var out []map[string]any
	for _, s := range cfg.Pipeline {
		e := map[string]any{"stage": s.Stage, "kind": s.Kind()}
		if s.Agent != "" {
			e["agent"] = s.Agent
			e["max_rounds"] = s.MaxRounds
		}
		if s.Run != "" {
			e["run"] = s.Run
		}
		if s.Optional {
			e["optional"] = true
		}
		out = append(out, e)
	}
	return out
}

// describeRunners reports the runner configuration each role actually resolves
// to, rather than what the file says, since the whole point of the runners:
// block is that most of it is inherited.
func describeRunners(cfg *config.Config) map[string]any {
	out := map[string]any{}
	for _, role := range append([]string{config.RoleDefault}, cfg.Roles()...) {
		s := cfg.Runner(role)
		e := map[string]any{
			"provider":        s.Provider,
			"model":           s.Model,
			"timeout_seconds": int(s.Timeout.Seconds()),
			"extra_args":      s.ExtraArgs,
			"resume":          s.Resume,
		}
		if s.Provider == config.CodexProvider {
			e["sandbox"] = s.Sandbox
			e["approval_policy"] = s.ApprovalPolicy
			e["tools"] = map[string]bool{
				"shell": s.Shell, "web_search": s.WebSearch, "view_image": s.ViewImage,
			}
		} else {
			e["permissions"] = string(s.Permissions)
			e["allowed_tools"] = s.AllowedTools
			e["denied_tools"] = s.DeniedTools
		}
		out[role] = e
	}
	return out
}

func gateNames(cfg *config.Config) []string {
	out := []string{}
	for _, g := range cfg.Gate {
		out = append(out, fmt.Sprintf("%s: %s", g.Name, g.Run))
	}
	return out
}
