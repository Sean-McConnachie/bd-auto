package cmds

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"bd-auto/internal/config"
	"bd-auto/internal/runstate"
)

// Run implements `bd-auto run <start|status|stop|pause|resume|unpark>`.
func Run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bd-auto run <start|status|stop|pause|resume|unpark>")
	}
	switch args[0] {
	case "start":
		return runStart(args[1:])
	case "status":
		return runStatus(args[1:])
	case "stop":
		return runStop(args[1:])
	case "unpark":
		return runUnpark(args[1:])
	case "pause":
		return runSetStatus(runstate.StatusPaused)
	case "resume":
		return runSetStatus(runstate.StatusActive)
	default:
		return fmt.Errorf("unknown run subcommand %q", args[0])
	}
}

func runStart(args []string) error {
	fs := flag.NewFlagSet("run start", flag.ContinueOnError)
	epic := fs.String("epic", "", "epic ID to drain (required)")
	concurrency := fs.Int("concurrency", 0, "max workers per wave (default from config)")
	autonomy := fs.String("autonomy", "", "auto|wave|issue (default from config)")
	retry := fs.Int("retry", -1, "extra attempts per issue (default from config)")
	force := fs.Bool("force", false, "replace an existing run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *epic == "" {
		return errors.New("--epic is required")
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}

	if existing, err := c.State(); err == nil && !*force {
		return fmt.Errorf("a run for %s is already active (wave %d); use --force to replace it",
			existing.Epic, existing.Wave)
	}

	// Fail early on an epic that does not exist, rather than at first dispatch.
	if _, err := c.BD.Show(*epic); err != nil {
		return fmt.Errorf("epic %s: %w", *epic, err)
	}

	conc := c.Cfg.Concurrency
	if *concurrency > 0 {
		conc = *concurrency
	}
	auto := c.Cfg.Autonomy
	if *autonomy != "" {
		auto = config.Autonomy(*autonomy)
		if !auto.Valid() {
			return fmt.Errorf("--autonomy: %q is not one of auto, wave, issue", *autonomy)
		}
	}
	rty := c.Cfg.Retry
	if *retry >= 0 {
		rty = *retry
	}

	st := runstate.New(*epic, conc, string(auto), rty)
	st.Note("run started for %s (concurrency %d, autonomy %s)", *epic, conc, auto)
	if err := runstate.Save(c.RepoRoot, st); err != nil {
		return err
	}

	// bd's own DAG check. Warnings here mean the run would mis-order work, so
	// surface them now while it is still cheap to fix.
	report, verr := c.BD.SwarmValidate(*epic)
	if verr != nil {
		info("warning: bd swarm validate failed: %v", verr)
	}

	out := map[string]any{
		"status":          "started",
		"epic":            *epic,
		"concurrency":     conc,
		"autonomy":        string(auto),
		"retry":           rty,
		"config":          configPathOrDefault(c.Cfg),
		"gate_configured": c.Cfg.HasGate(),
		"run_state":       runstate.Path(c.RepoRoot),
		"swarm_validate":  strings.TrimSpace(report),
	}
	return emitJSON(out)
}

func configPathOrDefault(cfg *config.Config) string {
	if cfg.Path() == "" {
		return "(built-in defaults; no .beads-auto.yaml)"
	}
	return cfg.Path()
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("run status", flag.ContinueOnError)
	asContext := fs.Bool("context", false, "render as compact text for context rehydration")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}
	st, err := c.State()
	if errors.Is(err, runstate.ErrNoRun) {
		if *asContext {
			return nil // nothing to say; hooks stay silent
		}
		return emitJSON(map[string]any{"active": false})
	}
	if err != nil {
		return err
	}

	stats, _ := c.BD.EpicStats(st.Epic)
	ready, _ := c.BD.Ready(st.Epic, 0)
	var readyIDs []string
	for _, r := range ready {
		if !st.Excluded(r.ID) {
			readyIDs = append(readyIDs, r.ID)
		}
	}

	if *asContext {
		fmt.Print(renderContext(st, stats.Total, stats.Closed, readyIDs))
		return nil
	}

	return emitJSON(map[string]any{
		"active":      true,
		"epic":        st.Epic,
		"status":      st.Status,
		"wave":        st.Wave,
		"wave_issues": st.WaveIssues,
		"in_flight":   st.InFlight,
		"done":        st.Done,
		"parked":      st.Parked,
		"ready_next":  readyIDs,
		"concurrency": st.Concurrency,
		"autonomy":    st.Autonomy,
		"retry":       st.Retry,
		"epic_total":  stats.Total,
		"epic_closed": stats.Closed,
		"notes":       st.Notes,
	})
}

// renderContext is what SessionStart and PostCompact inject. It is the whole
// answer to "the model forgot the instructions after autocompact": everything
// needed to resume is reconstructed from beads and run.json, not from history.
func renderContext(st *runstate.State, total, closed int, ready []string) string {
	var b strings.Builder
	b.WriteString("<bd-auto-run>\n")
	fmt.Fprintf(&b, "A bd-auto run is ACTIVE. You are the orchestrator. Do not do issue work yourself.\n")
	fmt.Fprintf(&b, "Epic: %s | status: %s | wave: %d | autonomy: %s | concurrency: %d\n",
		st.Epic, st.Status, st.Wave, st.Autonomy, st.Concurrency)
	fmt.Fprintf(&b, "Epic progress: %d/%d children closed.\n", closed, total)

	if len(st.InFlight) > 0 {
		b.WriteString("In flight right now:\n")
		for id, a := range st.InFlight {
			fmt.Fprintf(&b, "  - %s (attempt %d, branch %s)\n", id, a.Attempt, a.Branch)
		}
	}
	if len(ready) > 0 {
		fmt.Fprintf(&b, "Ready to dispatch next: %s\n", strings.Join(ready, ", "))
	} else if len(st.InFlight) == 0 {
		b.WriteString("No ready work left. If nothing is in flight, integrate the wave and finish the run.\n")
	}
	if len(st.Parked) > 0 {
		var ids []string
		for _, p := range st.Parked {
			ids = append(ids, p.ID)
		}
		fmt.Fprintf(&b, "Parked (needs a human, do not retry): %s\n", strings.Join(ids, ", "))
	}
	b.WriteString("Next step: run `bd-auto plan` and dispatch that wave, or `bd-auto merge-order` if the wave is complete.\n")
	b.WriteString("Full protocol: the bd-auto skill. Run state: .beads/auto/run.json\n")
	b.WriteString("</bd-auto-run>\n")
	return b.String()
}

func runStop(args []string) error {
	fs := flag.NewFlagSet("run stop", flag.ContinueOnError)
	keep := fs.Bool("keep-state", false, "leave run.json in place (disarms nothing)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := NewCtx()
	if err != nil {
		return err
	}
	st, err := c.State()
	if errors.Is(err, runstate.ErrNoRun) {
		return emitJSON(map[string]any{"status": "no-run"})
	}
	if err != nil {
		return err
	}
	summary := map[string]any{
		"status": "stopped",
		"epic":   st.Epic,
		"waves":  st.Wave,
		"done":   st.Done,
		"parked": st.Parked,
	}
	if *keep {
		_, err = runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
			s.Status = runstate.StatusDone
			s.Note("run stopped")
			return nil
		})
	} else {
		err = runstate.Clear(c.RepoRoot)
	}
	if err != nil {
		return err
	}
	return emitJSON(summary)
}

// runUnpark puts a parked issue back into the run. Parking is deliberately
// sticky — the plan excludes parked issues from run state, not just from bd —
// so reopening the issue in bd alone would not bring it back. This is the one
// supported way to retry a parked issue without discarding the whole run.
func runUnpark(args []string) error {
	fs := flag.NewFlagSet("run unpark", flag.ContinueOnError)
	issue := fs.String("issue", "", "parked issue ID (required)")
	reason := fs.String("reason", "", "what was fixed, appended to the issue's notes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *issue == "" {
		return errors.New("--issue is required")
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
	if !st.IsParked(*issue) {
		return fmt.Errorf("%s is not parked in this run", *issue)
	}

	// Reopen in bd first: if that fails, the issue stays out of the run rather
	// than being offered to a worker that cannot claim it.
	note := "bd-auto unparked: returned to the run for another attempt."
	if *reason != "" {
		note += " " + *reason
	}
	if err := c.BD.Unpark(*issue, note); err != nil {
		return fmt.Errorf("reopen %s: %w", *issue, err)
	}

	st, err = runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
		if !s.Unpark(*issue) {
			return fmt.Errorf("%s is not parked in this run", *issue)
		}
		s.Continuations = 0
		s.Note("%s unparked, attempts reset", *issue)
		return nil
	})
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{
		"issue": *issue, "recorded": "unparked",
		"parked_remaining": len(st.Parked),
		"note":             "attempts reset; the next `bd-auto plan` will offer it again",
	})
}

func runSetStatus(status string) error {
	c, err := NewCtx()
	if err != nil {
		return err
	}
	st, err := runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
		s.Status = status
		s.Continuations = 0
		s.Note("status set to %s", status)
		return nil
	})
	if errors.Is(err, runstate.ErrNoRun) {
		fmt.Fprintln(os.Stderr, "no active run")
		return nil
	}
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{"status": st.Status, "epic": st.Epic})
}
