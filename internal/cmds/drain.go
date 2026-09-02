package cmds

import (
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
	"time"

	"bd-auto/internal/config"
	"bd-auto/internal/drain"
	"bd-auto/internal/gitx"
	"bd-auto/internal/runstate"
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
	noPR := fs.Bool("no-pr", false, "stage the run on an epic branch but open no pull request")
	noStage := fs.Bool("no-epic-branch", false, "merge straight into the base branch, as if there were no handoff")
	skipPerms := skipPermissions(fs)
	noPreflight := fs.Bool("no-preflight", false, "start without checking that the backends can be spawned")
	allowAPIBilling := fs.Bool("allow-api-billing", false, "authorize Codex API-key charges for this command")
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

	// The handoff switches are narrowed on the resolved config rather than
	// carried into the engine as another pair of overrides: the config is
	// already this process's snapshot of the run's settings, and one place that
	// decides where a run lands is worth more than a flag path and a config path
	// that have to agree.
	if *noPR {
		c.Cfg.Handoff.PR = config.No()
	}
	if *noStage {
		c.Cfg.Handoff.Branch, c.Cfg.Handoff.PR = config.No(), config.No()
	}
	// Before the preview, so the level the run will actually use is the one
	// preview prints in its Models block, rather than the config's.
	applySkipPermissions(c, *skipPerms)

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

	// Billing authorization comes before the run lock and before preflight. It
	// is intentionally absent from dry-run, which spawns no model.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	eng := &drain.Engine{
		RepoRoot:        c.RepoRoot,
		Cfg:             c.Cfg,
		BD:              c.BD,
		BaseRef:         *base,
		MaxRounds:       *rounds,
		Control:         drain.NewControl(),
		SkipPreflight:   *noPreflight,
		AllowAPIBilling: *allowAPIBilling,
		Log:             func(format string, args ...any) { info(format, args...) },
	}
	if *retry >= 0 {
		eng.Retry = retry
	}
	if err := eng.AuthorizeBilling(ctx); err != nil {
		return reportBillingRefusal(err, *quiet || *asJSON)
	}
	eng.Log = nil

	// Held for the rest of this process, so a second drain in the same repo
	// refuses instead of finding an active run and resuming it -- which would
	// put two model processes in one worktree, both writing the same issue.
	// After the dry run, which decides nothing and may be read at any time.
	release, err := runstate.Hold(c.RepoRoot)
	if err != nil {
		if errors.Is(err, runstate.ErrRunLive) {
			return fmt.Errorf("a drain is already in progress in this repo; " +
				"watch it with `bd-auto status`, or stop it before starting another")
		}
		return err
	}
	defer release()

	opts := drain.DrainOptions{
		Epic:        *epic,
		Scope:       selected,
		Concurrency: conc,
		Autonomy:    auto,
	}

	// The wave table is the only thing in a run that can answer a worker's
	// question, so whether it is up is also what decides the ask policy: with
	// it, a question waits for a human; without it, the worker is told on the
	// spot that nobody is watching and to decide for itself.
	live := liveView(*quiet, *asJSON, *plain, interactive())

	// Here rather than left to the engine's own start, though the engine does
	// it too: the check takes a few seconds and can end the run, and both of
	// those belong on a terminal that is still plain text rather than under a
	// table that has just opened on an empty run. Drain does not repeat it.
	if err := preflight(ctx, eng, *quiet || *asJSON); err != nil {
		return err
	}
	asker := openAsk(c, eng, live)
	if asker != nil {
		defer asker.Close()
	}

	var rep drain.DrainReport
	if live {
		ui := tui.New(tui.Options{Control: eng.Control, Ask: responder(asker), RepoRoot: c.RepoRoot})
		eng.Bus = drain.NewBus(ui)
		drain.WireAsk(broker(asker), eng.Bus, c.RepoRoot)
		rep, err = watched(ctx, eng, opts, ui)
	} else {
		eng.Bus = drainBus(*asJSON, *quiet)
		drain.WireAsk(broker(asker), eng.Bus, c.RepoRoot)
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

// preflight checks the backends before the live view takes the terminal.
//
// The engine reports what it found through Engine.Log, which a drain otherwise
// only sets on the path with no live view, so it is lent one for the length of
// the check. Silent under --quiet and --json for the usual reason: one is
// asking for nothing, and the other is a stream something parses.
func preflight(ctx context.Context, eng *drain.Engine, quiet bool) error {
	if !quiet {
		prev := eng.Log
		eng.Log = func(format string, args ...any) { info(format, args...) }
		defer func() { eng.Log = prev }()
	}
	return eng.Preflight(ctx)
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
		// bd offers nothing, but bd is not the only thing with an opinion here.
		// A run killed after integration closed its issues but before its
		// barrier finished leaves exactly this: nothing open under the epic, a
		// run.json still marked active, and branches nobody merged -- possibly
		// with the checkout sitting mid-merge. Deriving the scope from bd again
		// refuses the one command that would finish it, and the tree stays
		// broken with no way in. So the unfinished run's own scope is the
		// answer, which is what run.json is for.
		if resumed, ok := unfinishedScope(c, epic); ok {
			return resumed.IDs(), resumed, nil
		}
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
	sel, err = selectInteractively(c, os.Stdin, set, conc)
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

// unfinishedScope is the scope of a run that was started and never finished,
// for the caller to resume. It is only consulted when bd offers nothing:
// while there is open work the candidate set is the truth, and a run that
// recorded no scope is an unrestricted one with no list to resume from.
func unfinishedScope(c *Ctx, epic string) (scope.Set, bool) {
	st, err := runstate.Load(c.RepoRoot)
	if err != nil || !st.Active() || len(st.Scope) == 0 {
		return scope.Set{}, false
	}
	// Never another run's. Naming an epic that is not the one in flight has to
	// keep saying there is nothing to drain.
	if epic != "" && st.Epic != epic {
		return scope.Set{}, false
	}
	// The recorded list is the whole answer. bd is asked only to put titles and
	// priorities on it, and an issue it will not talk about is still in scope:
	// the run already decided that, and closing an issue is not leaving the run.
	set := scope.Set{Skipped: map[string]string{}}
	for _, id := range st.Scope {
		e := scope.Issue{ID: id}
		if c.BD != nil {
			if iss, err := c.BD.Show(id); err == nil {
				e.Title, e.Type, e.Priority, e.Status = iss.Title, iss.IssueType, iss.Priority, iss.Status
			}
		}
		set.Issues = append(set.Issues, e)
	}
	info("%s was left unfinished; resuming its %d issue(s) rather than asking bd for open work",
		nameOr(st.Epic, "a run"), len(set.Issues))
	return set, true
}

// candidateSet computes what could be run. With an epic it is the epic's open,
// unparked, undeferred children; with a bare --issues it is those issues,
// checked to exist and to be workable.
func candidateSet(c *Ctx, epic, issues string) (scope.Set, error) {
	now := time.Now()
	if epic != "" {
		return scope.Candidates(c.BD, epic, now)
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
		// Named explicitly rather than picked off an epic, but bd will still
		// keep a deferred issue out of every ready front, so a run scoped to
		// one would spawn nothing and park it at the end. Better said here.
		if iss.Deferred(now) {
			return set, fmt.Errorf("%s is deferred until %s, so bd will never offer it to a wave; "+
				"undefer it with `bd update %s --defer=` first",
				id, iss.DeferUntil.UTC().Format("2006-01-02"), id)
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
//
// The reader is a parameter and not os.Stdin because the terminal is handed
// straight to the live view after this returns, so what this function leaves
// unread is part of what it does. See readLine.
func selectInteractively(c *Ctx, in io.Reader, set scope.Set, conc int) ([]string, error) {
	fmt.Fprint(os.Stderr, preview(c, set, set.IDs(), conc))

	for {
		fmt.Fprintf(os.Stderr, "\nSelect issues to run [all | none | 1,3,5-7]: ")
		line, err := readLine(in)
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
		confirm, err := readLine(in)
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

// readLine reads one line and not one byte past it.
//
// A bufio.Reader cannot be used here, however natural it looks. It reads ahead
// in whole chunks, and every byte still sitting in it when the prompt returns is
// a byte nobody ever sees again: the live view takes the terminal over
// immediately afterwards, and it reads os.Stdin, not this buffer. A run can take
// seconds to spawn its first worker, so keys typed into that gap — k, q, an
// arrow — are exactly the ones at risk. Reading a byte at a time leaves
// everything after the answer where it belongs, in the terminal's own queue,
// which is where the view will look for it.
//
// The cost is a syscall per byte on an answer a human types by hand. That is
// nothing, and it is paid twice per run.
//
// Like bufio.ReadString it returns what it read alongside io.EOF when the input
// ends mid-line: an unterminated answer is still an answer, and the callers
// decide what an unterminated one means.
func readLine(in io.Reader) (string, error) {
	var b strings.Builder
	var buf [1]byte
	for {
		n, err := in.Read(buf[:])
		if n > 0 {
			if buf[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return b.String(), err
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

	// The shape of the graph at this cap, not a schedule. Under autonomy: auto
	// the run does not wait for a layer to finish — an issue starts as soon as
	// what it depends on has merged — so what this says is how serialised the
	// selection is, which is the thing worth seeing before agreeing to it.
	waves := scope.Waves(set, selected, conc)
	fmt.Fprintf(&b, "\nDependency layers at concurrency %d: %d\n", conc, len(waves))
	for i, w := range waves {
		fmt.Fprintf(&b, "  layer %d: %s\n", i+1, strings.Join(w, ", "))
	}

	if blocked := outOfScope(set, selected); len(blocked) > 0 {
		b.WriteString("\nParked before dispatch (a blocker is outside the selection):\n")
		for _, l := range blocked {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}

	// Where the work ends up belongs in the preview for the same reason the
	// spend does: it is the other thing being agreed to, and "this merges into
	// the branch you are standing on" is not something to discover afterwards.
	base := gitx.CurrentBranch(c.RepoRoot)
	b.WriteString("\nWhere it lands:\n")
	switch {
	case c.Cfg.OpenPR():
		fmt.Fprintf(&b, "  a temporary branch under %s, then a pull request against %s.\n",
			c.Cfg.EpicBranchPrefix(), base)
		fmt.Fprintf(&b, "  %s is not written to, and nothing is pushed unless every issue lands green.\n", base)
	case c.Cfg.StageOnBranch():
		fmt.Fprintf(&b, "  a temporary branch under %s, left in place for you. %s is not written to.\n",
			c.Cfg.EpicBranchPrefix(), base)
	default:
		fmt.Fprintf(&b, "  merged straight into %s as each wave finishes.\n", base)
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
