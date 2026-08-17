package cmds

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"bd-auto/internal/config"
	"bd-auto/internal/drain"
	"bd-auto/internal/scope"
	"bd-auto/internal/tui"

	// Registers the shipped runner adapters, so a drain has something to spawn.
	_ "bd-auto/internal/runner/providers"
)

// Drain implements `bd-auto drain`: run a set of issues to completion.
//
// The command's first job is not to run anything. There is no budget, no
// per-request timeout and no circuit breaker anywhere in this engine, so the
// only bound on a run's spend is the set of issues it was given — and that set
// has to be chosen by a human who can see what it implies. So:
//
//   - On a terminal, the candidate set is shown with the waves it decomposes
//     into, the gate it will run and the model each role will spend, and nothing
//     is spawned until an explicit selection is confirmed. It is a preview of
//     the shape of the spend, not a yes/no prompt.
//   - Off a terminal — a skill launcher, CI, --plain — the scope has to be
//     explicit: --issues a,b,c, or --epic X --all. A bare --epic X prints the
//     candidate set and exits non-zero having spawned nothing. The background
//     path is the one with nobody watching, so it is the one that must name its
//     work.
func Drain(args []string) error {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	epic := fs.String("epic", "", "epic whose children are the candidates")
	issues := fs.String("issues", "", "comma-separated issue IDs: the explicit scope")
	all := fs.Bool("all", false, "scope the run to every candidate under --epic")
	concurrency := fs.Int("concurrency", 0, "issues in flight at once (default from config)")
	autonomy := fs.String("autonomy", "", "auto|wave (default from config)")
	rounds := fs.Int("rounds", 0, "feedback rounds per attempt (default from config)")
	retry := fs.Int("retry", -1, "extra attempts after the rounds run out (default from config)")
	base := fs.String("base", "", "ref every attempt branches from (default HEAD)")
	plain := fs.Bool("plain", false, "never prompt; behave as if there were no terminal")
	asJSON := fs.Bool("json", false, "stream events as JSON objects instead of text")
	dryRun := fs.Bool("dry-run", false, "print the candidate set and the plan, then stop")
	quiet := fs.Bool("quiet", false, "no live progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *epic == "" && *issues == "" {
		return errors.New("drain needs --epic <id>, --issues a,b,c, or both")
	}

	c, err := NewCtx()
	if err != nil {
		return err
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

	selected, set, err := resolveScope(c, *epic, *issues, *all, *plain, *dryRun, conc)
	if err != nil {
		return err
	}
	if *dryRun {
		return emitJSON(map[string]any{
			"dry_run":    true,
			"epic":       *epic,
			"candidates": set.IDs(),
			"scope":      selected,
			"waves":      scope.Waves(set, selected, conc),
			"dispatched": false,
		})
	}

	// SIGINT is the interrupt path, not a crash: workers stop, worktrees and
	// branches stay, sessions stay recorded, and re-running resumes them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng := &drain.Engine{
		RepoRoot:  c.RepoRoot,
		Cfg:       c.Cfg,
		BD:        c.BD,
		BaseRef:   *base,
		MaxRounds: *rounds,
		Control:   drain.NewControl(),
	}
	if *retry >= 0 {
		eng.Retry = retry
	}

	opts := drain.DrainOptions{
		Epic:        *epic,
		Scope:       selected,
		Concurrency: conc,
		Autonomy:    auto,
	}

	var rep drain.DrainReport
	if liveView(*quiet, *asJSON, *plain, interactive()) {
		ui := tui.New(tui.Options{Control: eng.Control})
		eng.Bus = drain.NewBus(ui)
		rep, err = watched(ctx, eng, opts, ui)
	} else {
		eng.Bus = drainBus(*asJSON, *quiet)
		if !*quiet && !*asJSON {
			eng.Log = func(format string, args ...any) { info(format, args...) }
		}
		rep, err = eng.Drain(ctx, opts)
	}
	if err != nil {
		return err
	}
	if err := emitJSON(rep); err != nil {
		return err
	}
	// A parked issue is a real outcome, already in the report. The exit code
	// says only whether the run finished on its own terms with nothing parked.
	if !rep.Completed() || len(rep.Parked) > 0 {
		return errSilentExit{code: 1}
	}
	return nil
}

// drainBus wires the renderers. Events go to stderr so stdout stays clean for
// the final report, which is what a caller parses.
func drainBus(asJSON, quiet bool) *drain.Bus {
	switch {
	case quiet:
		return drain.NewBus()
	case asJSON:
		return drain.NewBus(drain.JSONRenderer(os.Stderr))
	default:
		return drain.NewBus(drain.PlainRenderer(os.Stderr))
	}
}

// liveView decides whether this run gets the wave table.
//
// It is a rule over flags and one terminal check, and it is a separate function
// so that the rule can be exercised without attaching a pseudo-terminal to a
// test. Deciding once, here, is what keeps a redirected run from reaching the
// TUI at all: a renderer negotiating with a terminal that is not there is how a
// CI log fills with escape sequences. Every other form falls back to the plain
// or JSON renderer, which carry the same facts.
func liveView(quiet, asJSON, plain, tty bool) bool {
	return tty && !quiet && !asJSON && !plain
}

// watched runs the drain underneath the wave table.
//
// The engine runs on its own goroutine because bubbletea owns this one: it has
// the terminal, and the keys it reads are the only way to stop the run. Both
// ends are joined before returning, so the report is never printed over a table
// that is still drawing.
func watched(ctx context.Context, eng *drain.Engine, opts drain.DrainOptions, ui *tui.UI) (drain.DrainReport, error) {
	var (
		rep  drain.DrainReport
		err  error
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		defer ui.Finish()
		rep, err = eng.Drain(ctx, opts)
	}()

	if uiErr := ui.Run(ctx); uiErr != nil {
		// The view is gone but the run is not: stopping it is the only way to
		// avoid leaving models spawning against a terminal nobody is watching.
		eng.Control.Stop()
		info("the live view failed (%v); stopping the run", uiErr)
	}
	<-done
	if ui.Stopped() {
		info("stopped from the live view. Nothing was parked for it; re-run drain to resume.")
	}
	return rep, err
}

// --- scope selection ---

// resolveScope produces the issue list a run is allowed to touch, or an error
// that explains what to name.
func resolveScope(c *Ctx, epic, issues string, all, plain, dryRun bool, conc int) ([]string, scope.Set, error) {
	set, err := candidateSet(c, epic, issues)
	if err != nil {
		return nil, set, err
	}
	if len(set.Issues) == 0 {
		return nil, set, fmt.Errorf("nothing to drain: %s has no open, unparked children", nameOr(epic, "the selection"))
	}

	headless := plain || !interactive()
	sel, prompt, err := chooseScope(set, issues, all, dryRun, headless)
	if err != nil {
		if headless && issues == "" && !all {
			// The candidate set is the useful half of this refusal: it is the
			// list the caller has to choose from.
			fmt.Fprint(os.Stderr, preview(c, set, set.IDs(), conc))
		}
		return nil, set, err
	}
	if !prompt {
		return sel, set, nil
	}
	sel, err = selectInteractively(c, set, conc)
	return sel, set, err
}

// chooseScope decides the scope from the flags alone, and reports whether the
// answer has to come from a human instead.
//
// It is separated from the terminal handling because it is the rule this whole
// command exists for, and a rule that can only be exercised by attaching a
// pseudo-terminal is a rule nobody checks: off a terminal, with nothing named,
// this returns an error and the caller spawns nothing.
func chooseScope(set scope.Set, issues string, all, dryRun, headless bool) (sel []string, prompt bool, err error) {
	// An explicit list wins everywhere, terminal or not. It is the form that
	// names its work.
	if issues != "" {
		sel, err = scope.Resolve(set, strings.Split(issues, ","))
		return sel, false, err
	}
	if all {
		return set.IDs(), false, nil
	}
	if dryRun {
		// Nothing is spawned, so there is nothing to approve; the whole
		// candidate set stands in as the hypothetical scope.
		return set.IDs(), false, nil
	}
	if headless {
		return nil, false, fmt.Errorf(
			"nothing was dispatched: a run with no terminal must name its scope explicitly.\n"+
				"Re-run with --issues %s, or --epic %s --all to take all %d.",
			strings.Join(firstN(set.IDs(), 3), ","), set.Epic, len(set.Issues))
	}
	return nil, true, nil
}

// candidateSet computes what could be run. With an epic it is the epic's open,
// unparked children; with a bare --issues it is those issues, checked to exist.
func candidateSet(c *Ctx, epic, issues string) (scope.Set, error) {
	if epic != "" {
		return scope.Candidates(c.BD, epic)
	}
	set := scope.Set{Skipped: map[string]string{}}
	for _, raw := range strings.Split(issues, ",") {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		iss, err := c.BD.Show(id)
		if err != nil {
			return set, fmt.Errorf("%s: %w", id, err)
		}
		if iss.Terminal() {
			return set, fmt.Errorf("%s is %s; there is nothing to run", id, iss.Status)
		}
		set.Issues = append(set.Issues, scope.Issue{
			ID: id, Title: iss.Title, Type: iss.IssueType,
			Priority: iss.Priority, Status: iss.Status,
		})
	}
	return set, nil
}

// selectInteractively shows the preview and takes a selection.
//
// Two answers, not one: what to run, and then a confirmation of the resolved
// list. The second exists because the first accepts ranges, and a mistyped range
// is exactly the mistake this whole gate is here to catch.
func selectInteractively(c *Ctx, set scope.Set, conc int) ([]string, error) {
	in := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, preview(c, set, set.IDs(), conc))

	for {
		fmt.Fprintf(os.Stderr, "\nSelect issues to run [all | none | 1,3,5-7]: ")
		line, err := in.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil, errors.New("nothing was dispatched: no selection was made")
			}
			return nil, err
		}
		answer := strings.TrimSpace(line)
		if answer == "" || strings.EqualFold(answer, "none") || strings.EqualFold(answer, "q") {
			return nil, errors.New("nothing was dispatched: no issues selected")
		}

		picked, err := pick(set, answer)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			continue
		}

		fmt.Fprint(os.Stderr, "\n"+preview(c, set, picked, conc))
		fmt.Fprintf(os.Stderr, "\nRun these %d issue(s)? [y/N]: ", len(picked))
		confirm, err := in.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(confirm), "y") {
			return picked, nil
		}
		if err == io.EOF {
			return nil, errors.New("nothing was dispatched: the selection was not confirmed")
		}
	}
}

// pick turns an answer into issue IDs. Numbers index the preview, so a human
// reading the list can answer with what they see; IDs are accepted too, because
// a copied ID should not have to be looked up in a list.
func pick(set scope.Set, answer string) ([]string, error) {
	if strings.EqualFold(answer, "all") {
		return set.IDs(), nil
	}
	ids := set.IDs()
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}

	for _, field := range strings.Split(answer, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if lo, hi, ok := parseRange(field); ok {
			if lo < 1 || hi > len(ids) || lo > hi {
				return nil, fmt.Errorf("%q is outside 1-%d", field, len(ids))
			}
			for i := lo; i <= hi; i++ {
				add(ids[i-1])
			}
			continue
		}
		if n, err := strconv.Atoi(field); err == nil {
			if n < 1 || n > len(ids) {
				return nil, fmt.Errorf("%d is outside 1-%d", n, len(ids))
			}
			add(ids[n-1])
			continue
		}
		if _, ok := set.Get(field); !ok {
			return nil, fmt.Errorf("%q is neither a number in 1-%d nor a candidate issue", field, len(ids))
		}
		add(field)
	}
	if len(out) == 0 {
		return nil, errors.New("nothing selected")
	}
	// Back into candidate order, so the confirmation reads like the list above.
	sort.Slice(out, func(i, j int) bool { return positionOf(ids, out[i]) < positionOf(ids, out[j]) })
	return out, nil
}

func parseRange(field string) (int, int, bool) {
	lo, hi, found := strings.Cut(field, "-")
	if !found {
		return 0, 0, false
	}
	l, err1 := strconv.Atoi(strings.TrimSpace(lo))
	h, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return l, h, true
}

// preview is what a human approves: the issues, the waves they decompose into,
// the gate that will run and the model each role will spend.
//
// The point of showing all four together is that none of them alone says what a
// run costs. The issue count says how much work; the wave count says how many
// times the gate and the barrier run; the roles say what each of those spends.
func preview(c *Ctx, set scope.Set, selected []string, conc int) string {
	sel := map[string]bool{}
	for _, id := range selected {
		sel[id] = true
	}

	var b strings.Builder
	if set.Epic != "" {
		fmt.Fprintf(&b, "Epic %s: %d candidate issue(s), %d selected.\n\n", set.Epic, len(set.Issues), len(selected))
	} else {
		fmt.Fprintf(&b, "%d issue(s) named, %d selected.\n\n", len(set.Issues), len(selected))
	}
	for i, iss := range set.Issues {
		mark := " "
		if sel[iss.ID] {
			mark = "x"
		}
		fmt.Fprintf(&b, " [%s] %2d. %-24s P%d %s\n", mark, i+1, iss.ID, iss.Priority, truncate(iss.Title, 52))
		if len(iss.DependsOn) > 0 {
			fmt.Fprintf(&b, "          waits on %s\n", strings.Join(iss.DependsOn, ", "))
		}
	}

	waves := scope.Waves(set, selected, conc)
	fmt.Fprintf(&b, "\nWaves at concurrency %d: %d\n", conc, len(waves))
	for i, w := range waves {
		fmt.Fprintf(&b, "  wave %d: %s\n", i+1, strings.Join(w, ", "))
	}

	if blocked := outOfScope(set, selected); len(blocked) > 0 {
		b.WriteString("\nParked before dispatch (a blocker is outside the selection):\n")
		for _, l := range blocked {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}

	b.WriteString("\nGate (per branch, and again on the merged result):\n")
	if !c.Cfg.HasGate() {
		b.WriteString("  (none configured; every branch passes the gate trivially)\n")
	}
	for _, g := range c.Cfg.Gate {
		fmt.Fprintf(&b, "  %s: %s\n", g.Name, g.Run)
	}

	b.WriteString("\nModels:\n")
	for _, role := range c.Cfg.Roles() {
		s := c.Cfg.Runner(role)
		fmt.Fprintf(&b, "  %-11s %s/%s (%s)\n", role, s.Provider, s.Model, s.Permissions)
	}
	return b.String()
}

// outOfScope reports the selected issues whose blocker was not selected, in the
// words the run will use when it parks them.
func outOfScope(set scope.Set, selected []string) []string {
	sel := map[string]bool{}
	for _, id := range selected {
		sel[id] = true
	}
	var out []string
	for _, iss := range set.Issues {
		if !sel[iss.ID] {
			continue
		}
		for _, d := range iss.DependsOn {
			if !sel[d] {
				out = append(out, fmt.Sprintf("%s: dependency %s is out of scope and is not closed", iss.ID, d))
				break
			}
		}
	}
	return out
}

// interactive reports whether there is a human at both ends of this process. It
// has to be both: a run whose output is redirected has nobody reading the
// preview, and a run whose input is a pipe has nobody to answer it.
func interactive() bool {
	return charDevice(os.Stdin) && charDevice(os.Stderr)
}

func charDevice(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func firstN(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

func positionOf(in []string, want string) int {
	for i, v := range in {
		if v == want {
			return i
		}
	}
	return len(in)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func nameOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
