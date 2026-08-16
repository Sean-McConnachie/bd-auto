package cmds

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/runstate"
)

// WaveIssue is one issue the orchestrator should dispatch a worker for.
type WaveIssue struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Type     string `json:"type"`
	Priority int    `json:"priority"`
	Branch   string `json:"branch"`
	Attempt  int    `json:"attempt"`
	// RetryContext carries why the previous attempt failed. Empty on a first
	// attempt. This is what makes a retry informed rather than a repeat.
	RetryContext string `json:"retry_context,omitempty"`
}

// Plan implements `bd-auto plan`: compute the next wave.
//
// Readiness is not recomputed here. bd ready is already blocker-aware, so this
// asks bd and then subtracts what this run has already handled.
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

	ready, err := c.BD.Ready(st.Epic, 0)
	if err != nil {
		return err
	}

	cap := st.Concurrency
	if *limit > 0 {
		cap = *limit
	}

	var wave []WaveIssue
	for _, iss := range ready {
		if len(wave) >= cap {
			break
		}
		if st.Excluded(iss.ID) {
			continue
		}
		attempt := st.Attempts[iss.ID] + 1
		wave = append(wave, WaveIssue{
			ID:           iss.ID,
			Title:        iss.Title,
			Type:         iss.IssueType,
			Priority:     iss.Priority,
			Branch:       c.Cfg.Branch(iss.ID),
			Attempt:      attempt,
			RetryContext: retryContext(c.BD, iss.ID, attempt),
		})
	}

	drained := len(wave) == 0 && len(st.InFlight) == 0
	payload := map[string]any{
		"epic":        st.Epic,
		"wave":        st.Wave,
		"issues":      wave,
		"in_flight":   st.Remaining(),
		"drained":     drained,
		"autonomy":    st.Autonomy,
		"concurrency": cap,
		"pipeline":    describePipeline(c.Cfg),
		"gate":        gateNames(c.Cfg),
	}

	if !*dispatch {
		payload["dispatched"] = false
		return emitJSON(payload)
	}

	if len(wave) == 0 {
		payload["dispatched"] = false
		payload["note"] = "nothing to dispatch"
		return emitJSON(payload)
	}

	// Record the wave. Done under lock because worker hooks write concurrently.
	updated, err := runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
		s.Wave++
		s.LastWaveChange = s.Wave
		s.Continuations = 0
		s.WaveIssues = nil
		for _, w := range wave {
			s.WaveIssues = append(s.WaveIssues, w.ID)
			s.Attempts[w.ID] = w.Attempt
			s.InFlight[w.ID] = runstate.Attempt{
				Branch:  w.Branch,
				Attempt: w.Attempt,
				Stage:   config.StageImplement,
			}
		}
		s.Note("wave %d dispatched: %s", s.Wave, strings.Join(s.WaveIssues, ", "))
		return nil
	})
	if err != nil {
		return err
	}

	payload["dispatched"] = true
	payload["wave"] = updated.Wave
	return emitJSON(payload)
}

// retryContext pulls the failure notes recorded on a previous attempt so the
// fresh worker starts informed.
func retryContext(c *bd.Client, id string, attempt int) string {
	if attempt <= 1 {
		return ""
	}
	iss, err := c.Show(id)
	if err != nil || iss.Notes == "" {
		return ""
	}
	const marker = "bd-auto attempt"
	idx := strings.LastIndex(iss.Notes, marker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(iss.Notes[idx:])
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
		out[role] = map[string]any{
			"provider":        s.Provider,
			"model":           s.Model,
			"permissions":     string(s.Permissions),
			"timeout_seconds": int(s.Timeout.Seconds()),
			"allowed_tools":   s.AllowedTools,
			"extra_args":      s.ExtraArgs,
			"resume":          s.Resume,
		}
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
