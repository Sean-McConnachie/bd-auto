package cmds

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

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
	autonomy := fs.String("autonomy", "", "auto|wave (default from config)")
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

	// A repo with no config file runs on built-in defaults, which works but
	// leaves the user nothing to edit. Write the starter file now that the run
	// is known to be going ahead, and load it so the run uses what is on disk.
	//
	// Only run start does this. Commands that fire from inside a worktree would
	// scatter config files around if they generated one.
	configCreated := ""
	if c.Cfg.Path() == "" {
		path, werr := config.Write(c.RepoRoot, false)
		if werr != nil && !errors.Is(werr, config.ErrConfigExists) {
			return werr
		}
		if werr == nil {
			configCreated = path
			info("bd-auto: no %s found, wrote one at %s", config.FileName, path)
		}
		if cfg, lerr := config.Load(c.RepoRoot); lerr == nil {
			c.Cfg = cfg
		} else {
			return fmt.Errorf("generated config does not load: %w", lerr)
		}
	}

	conc := c.Cfg.Concurrency
	if *concurrency > 0 {
		conc = *concurrency
	}
	auto := c.Cfg.Autonomy
	if *autonomy != "" {
		auto = config.Autonomy(*autonomy)
		if !auto.Valid() {
			return fmt.Errorf("--autonomy: %q is not one of auto, wave", *autonomy)
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
	if configCreated != "" {
		out["config_created"] = configCreated
	}
	return emitJSON(out)
}

// pollInterval is how often --wait re-reads run.json. It is a file read against
// a run that takes minutes per issue, so it can afford to be frequent; what it
// must not be is a busy loop.
const pollInterval = 3 * time.Second

// waitForRun loads the run state, optionally blocking until the run is no
// longer active.
//
// This is what makes watching a drain cost a fixed amount rather than an amount
// proportional to how long it runs. A watcher without it polls on a timer, and
// every one of those polls is output it has to read; with it, one call covers a
// whole run and prints once. The bound is still there — the wait expires and
// reports whatever is true then — because a caller blocked forever on a wedged
// run is worse than one that comes back and says it is still going.
func waitForRun(c *Ctx, wait time.Duration) (*runstate.State, error) {
	st, err := c.State()
	if wait <= 0 || err != nil {
		return st, err
	}
	deadline := time.Now().Add(wait)
	for st.Status == runstate.StatusActive {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		time.Sleep(min(pollInterval, remaining))
		// A run that is cleared mid-wait finished and tidied up after itself.
		// The last state we saw is the truthful answer, not an error.
		next, nerr := c.State()
		if nerr != nil {
			break
		}
		st = next
	}
	return st, nil
}

func configPathOrDefault(cfg *config.Config) string {
	if cfg.Path() == "" {
		return "(built-in defaults; no .beads-auto.yaml)"
	}
	return cfg.Path()
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("run status", flag.ContinueOnError)
	asContext := fs.Bool("context", false, "render as a few lines of text instead of JSON")
	wait := fs.Duration("wait", 0, "block until the run leaves active, or this long")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}
	st, err := waitForRun(c, *wait)
	if errors.Is(err, runstate.ErrNoRun) {
		if *asContext {
			fmt.Println("No bd-auto run is recorded.")
			return nil
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
		"scope":       st.Scope,
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

// maxNamed caps how many issue IDs any one line of the poll view names.
//
// It is the whole reason the poll view is bounded. A session that launches a
// background drain reads this output once an hour for as long as the run lasts,
// so a line that names every finished issue makes the reader's cost grow with
// the epic — the exact cost this engine exists to remove. Counts answer "how is
// it going"; names are only needed for the few issues something can be done
// about.
//
// 4 rather than more because it has to hold at the worst case, not the typical
// one: two saturated lists of long issue IDs is what scripts/launch-cost.sh
// budgets for, and at 6 the whole view no longer fits.
const maxNamed = 4

// renderContext is the poll view: the state of a run in four lines or fewer, at
// any epic size and any point in the run.
//
// It exists because the JSON form is the wrong shape to read repeatedly — its
// notes and in-flight records say far more than "is it still going". Anything
// added here is paid for on every poll, so add counts, not lists.
func renderContext(st *runstate.State, total, closed int, ready []string) string {
	inFlight := make([]string, 0, len(st.InFlight))
	for id := range st.InFlight {
		inFlight = append(inFlight, id)
	}
	sort.Strings(inFlight)

	parked := make([]string, 0, len(st.Parked))
	for _, p := range st.Parked {
		parked = append(parked, p.ID)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "bd-auto run: %s | epic %s | wave %d | %d/%d children closed\n",
		st.Status, nameOr(st.Epic, "(no epic)"), st.Wave, closed, total)
	fmt.Fprintf(&b, "scope %d | running %d | done %d | parked %d | queued %d\n",
		len(st.Scope), len(inFlight), len(st.Done), len(parked), len(ready))

	// Two lists survive the cull, because each names issues a watcher might act
	// on: what is being worked right now, and what has given up and wants a
	// human. Done and queued are counts above and nothing else.
	if len(inFlight) > 0 {
		fmt.Fprintf(&b, "running: %s\n", nameSome(inFlight))
	}
	if len(parked) > 0 {
		fmt.Fprintf(&b, "parked (needs a human): %s\n", nameSome(parked))
	}
	if len(inFlight) == 0 && len(ready) == 0 {
		b.WriteString("nothing left to dispatch\n")
	}
	return b.String()
}

// nameSome joins IDs, naming at most maxNamed of them and counting the rest.
func nameSome(ids []string) string {
	if len(ids) <= maxNamed {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:maxNamed], ", "), len(ids)-maxNamed)
}

func runStop(args []string) error {
	fs := flag.NewFlagSet("run stop", flag.ContinueOnError)
	keep := fs.Bool("keep-state", false, "leave run.json in place as a record of what landed")
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
