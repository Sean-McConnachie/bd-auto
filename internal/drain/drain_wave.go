package drain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/graph"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
	"bd-auto/internal/scope"
	"bd-auto/internal/wave"
)

// Drain is the run, and the run has two schedulers. This file holds the one
// autonomy: wave asks for — plan a wave inside the scope, run it with bounded
// concurrency, integrate the lot at a barrier, and go round again — and the
// prologue both share. The other is drain_cont.go, it is the default, and it
// has no barrier at all.
//
// Four things about the run are not obvious from the shape, and three of them
// belong to both schedulers.
//
// A wave is a cap, not a batch. It is planned at the concurrency cap, but it
// grows: a worker that finishes frees its slot, and the pool asks bd what is
// ready and puts the next in-scope issue into it rather than holding it empty
// until the barrier. Without that, one issue parking in its first minute costs
// the rest of the wave a worker. Continuous scheduling is that idea with the
// barrier taken out from under it as well.
//
// The scope is a hard allowlist, not a starting point. wave.Plan intersects bd's
// ready front with it, so an issue outside the scope is never dispatched however
// ready bd says it is — which is what keeps discovered work out of a run by
// construction. The other half of that promise is here: an in-scope issue whose
// blocker was never in the scope can never become ready, so it is parked before
// anything is spawned rather than sitting silently unready until the run ends.
//
// A run stops on an outage. An issue that comes back infra-failed cancels its
// siblings, because five workers meeting one rate limit is one outage, and
// letting the rest keep trying converts it into a pile of parked issues and a
// bill. Neither an interrupt nor an outage is a verdict, so nothing is parked
// for it and every worktree, branch and session survives for the re-run.
//
// And every way a run can end goes through finish, which is what makes the
// handoff decision happen exactly once and be recorded even when the run got
// nowhere near the happy path.

// DefaultPollInterval is how often a paused run re-reads its state waiting for
// `bd-auto run resume`.
const DefaultPollInterval = 2 * time.Second

// DrainOptions are the resolved knobs for one drain.
type DrainOptions struct {
	// Epic is the epic the run is under. It may be empty for a scope named
	// issue by issue, in which case the epic is never closed.
	Epic string
	// Scope is the set of issues a human approved. Empty means unrestricted,
	// which is only reachable through the older `run start` path — `drain`
	// always names its work.
	Scope []string
	// Concurrency caps how many issues run at once. Zero uses the run state's.
	Concurrency int
	// Autonomy is auto or wave. Empty uses the run state's.
	Autonomy config.Autonomy
	// MaxWaves stops a run after this many waves. Zero means DefaultMaxWaves,
	// which exists only to bound a loop that is not making progress; the scope
	// is what bounds the spend.
	MaxWaves int
}

// DefaultMaxWaves bounds the wave loop. A scope of N issues needs at most N
// waves even when every one of them is serialised behind the last, so this is a
// runaway guard rather than a budget.
const DefaultMaxWaves = 100

// DrainReport is what a whole run produced.
type DrainReport struct {
	Epic  string   `json:"epic,omitempty"`
	Scope []string `json:"scope,omitempty"`
	Waves int      `json:"waves"`
	// Continuous reports that this run held its concurrency cap full and
	// integrated each issue as it landed, rather than gathering a wave and
	// stopping the world at a barrier. It is what autonomy: auto does, and it
	// is on the report because Waves cannot say it: a continuous run is one
	// wave for its whole life, which reads the same as a wave run that finished
	// in one.
	Continuous bool `json:"continuous,omitempty"`

	// Issues is every issue this run took a verdict on, in the order they
	// finished being planned.
	Issues []Report `json:"issues,omitempty"`
	// Integrations is one entry per barrier: per wave for a wave run, and per
	// issue that landed for a continuous one, with the settling barrier last.
	Integrations []IntegrateReport `json:"integrations,omitempty"`

	Done   []string `json:"done,omitempty"`
	Parked []string `json:"parked,omitempty"`
	// MissingDeps is every park in this run whose reason named an issue that
	// was running beside it. It is the run's answer to a worker that stopped
	// waiting for a sibling: nothing in a wave blocks anything else in it, so
	// either the worker was wrong or the graph is short an edge. See MissingDep.
	MissingDeps []MissingDep `json:"missing_deps,omitempty"`

	// Base is the branch this run was for, and EpicBranch the temporary branch
	// it was staged on. EpicBranch is empty for a run that merged straight into
	// its base branch.
	Base       string `json:"base,omitempty"`
	EpicBranch string `json:"epic_branch,omitempty"`
	// Handoff is the terminal step: the pull request this run was handed over
	// as, or the reason there is none.
	Handoff *HandoffReport `json:"handoff,omitempty"`

	// Hooks is what the repo's on_run_end hooks said about the finished run.
	// They run after the handoff and are advisory, so nothing in here opened,
	// refused or altered a pull request. See hooks.go.
	Hooks []HookResult `json:"hooks,omitempty"`

	// Outcome is the run's own ending, not a verdict on any issue: done when
	// the loop finished, interrupted or infra-failed when something stopped it.
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`

	// EpicClosed reports whether a barrier closed the epic, and EpicReason why
	// the last barrier did or did not.
	EpicClosed bool   `json:"epic_closed"`
	EpicReason string `json:"epic_reason,omitempty"`

	Usage   runner.Usage `json:"usage"`
	Seconds float64      `json:"seconds"`
}

// Completed reports whether the run ended on its own terms. Parked issues are
// still a completed run: they are a result, not an interruption.
func (r DrainReport) Completed() bool { return r.Outcome == OutcomeDone }

// Landed lists every issue whose branch is in the merged result, in the order
// the barriers merged them.
func (r DrainReport) Landed() []string {
	seen := map[string]bool{}
	var out []string
	for _, in := range r.Integrations {
		for _, id := range in.Merged() {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// Drain runs a scoped set of issues to completion.
//
// It returns an error only for a failure that is not about the work — an
// unreachable bd, an unwritable run state, a run already active for a different
// epic. Everything an issue or a barrier can fail at comes back in the report.
func (e *Engine) Drain(ctx context.Context, opts DrainOptions) (DrainReport, error) {
	started := time.Now()
	switch {
	case e.RepoRoot == "":
		return DrainReport{}, errors.New("drain: RepoRoot is required")
	case e.Cfg == nil:
		return DrainReport{}, errors.New("drain: Cfg is required")
	case e.BD == nil:
		return DrainReport{}, errors.New("drain: BD is required")
	case opts.Epic == "" && len(opts.Scope) == 0:
		return DrainReport{}, errors.New("drain: an epic or an explicit scope is required")
	}

	// Everything below runs under a context the control channel can cancel, so
	// q reaches the workers by the same path a SIGINT does.
	ctx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	e.Control.bind(stopRun)
	defer e.Control.unbind()

	e.logPromptSources()

	// Before the run state, and long before a worktree: a backend that cannot
	// be spawned should cost one error rather than a wave of them. See
	// Preflight.
	if err := e.Preflight(ctx); err != nil {
		return DrainReport{}, err
	}

	st, err := e.startRun(opts)
	if err != nil {
		return DrainReport{}, err
	}

	rep := DrainReport{Epic: st.Epic, Scope: st.Scope}
	e.Bus.Emit(Event{Kind: EventRunStart, Issues: st.Scope, Text: st.Epic})

	// Where the integrator writes down what it did to the main checkout. It is
	// opened here, before anything is dispatched, because a worker's guard reads
	// it and there is no moment in a continuous run when the two are not both
	// live. See headLog.
	e.merged = &headLog{}

	// The code index, if this repo asked for one. It is built once, before any
	// worker is dispatched, because every worker would otherwise derive the same
	// map from scratch. Nothing here can fail the run: graph.Build reports why
	// there is no index and the drain carries on without one.
	e.buildIndex(ctx)

	if err := e.parkOutOfScope(st, &rep); err != nil {
		return e.finish(ctx, rep, started, err)
	}

	// The two schedulers. autonomy: wave gathers a wave, drains it, integrates
	// it at a barrier and offers a human the gap; autonomy: auto holds the cap
	// full continuously and integrates each issue as it lands. Both end through
	// finish, which is what makes the handoff decision happen exactly once
	// however the run got there.
	if st.Autonomy == string(config.AutonomyWave) {
		return e.drainInWaves(ctx, opts, rep, started)
	}
	return e.drainContinuously(ctx, opts, rep, started)
}

// drainInWaves is the scheduler autonomy: wave asks for: plan a wave inside the
// scope, run it to the last worker, integrate the lot at a barrier, and offer a
// human the gap before the next one.
func (e *Engine) drainInWaves(ctx context.Context, opts DrainOptions, rep DrainReport, started time.Time) (DrainReport, error) {
	maxWaves := opts.MaxWaves
	if maxWaves <= 0 {
		maxWaves = DefaultMaxWaves
	}

	for rep.Waves < maxWaves {
		if err := ctx.Err(); err != nil {
			rep.Outcome, rep.Reason = OutcomeInterrupted, "the run was interrupted"
			return e.finish(ctx, rep, started, nil)
		}

		st, err := runstate.Load(e.RepoRoot)
		if err != nil {
			return e.finish(ctx, rep, started, err)
		}
		if stop := e.awaitResume(ctx, st, &rep); stop {
			return e.finish(ctx, rep, started, nil)
		}

		issues, err := e.nextWave(st)
		if err != nil {
			return e.finish(ctx, rep, started, err)
		}
		if len(issues) == 0 {
			break
		}

		rep.Waves++
		e.Bus.Emit(Event{Kind: EventWaveStart, Wave: st.Wave, Issues: wave.IDs(issues)})

		reports, werr := e.runWave(ctx, st.Wave, st.Concurrency, issues)
		for _, r := range reports {
			rep.Issues = append(rep.Issues, r)
			rep.Usage = rep.Usage.Add(r.Usage)
		}
		if werr != nil {
			return e.finish(ctx, rep, started, werr)
		}

		if stop, reason := waveStopped(reports); stop != "" {
			// Nothing here was judged, so nothing is parked and nothing is
			// merged: the branches, worktrees and sessions are all left for the
			// re-run to pick up.
			rep.Outcome, rep.Reason = stop, reason
			return e.finish(ctx, rep, started, nil)
		}

		if err := e.barrier(ctx, &rep); err != nil {
			return e.finish(ctx, rep, started, err)
		}
		if rep.Outcome != "" {
			return e.finish(ctx, rep, started, nil)
		}

		if stop := e.pauseAtBarrier(ctx, &rep); stop {
			return e.finish(ctx, rep, started, nil)
		}
	}

	if rep.Waves >= maxWaves {
		rep.Outcome = OutcomeFailed
		rep.Reason = fmt.Sprintf("stopped after %d waves without draining the scope", maxWaves)
		return e.finish(ctx, rep, started, nil)
	}

	// The scope is drained, so anything still unresolved is an issue bd never
	// offered. Saying why is the difference between a finished run and a run
	// that quietly dropped work.
	if err := e.parkStranded(&rep); err != nil {
		return e.finish(ctx, rep, started, err)
	}
	rep.Outcome = OutcomeDone
	return e.finish(ctx, rep, started, nil)
}

// finish stamps the run's totals from run state, hands the result over and
// closes the run out.
//
// Done and Parked are read back from run state rather than accumulated from the
// reports, because a barrier can park an issue whose own report said done: a
// branch that failed the wave gate did not land, whatever the worker that
// produced it reported.
//
// The handoff belongs here rather than at the end of the happy path because
// finish is the run's single exit. Every way a drain can end passes through it,
// so putting the handoff here is what makes it evaluated exactly once and
// refused explicitly — an interrupted run gets a recorded reason for having no
// pull request, rather than silence from a branch of the code it never reached.
//
// It takes the error the caller is about to return, and returns it back, for
// the same reason. An error exit is a run that stopped part-way, and a report
// that defaulted such a run to done would let the handoff publish a pull request
// claiming an epic is finished when the drain abandoned it several waves early —
// exactly what the human gate exists to prevent. Threading the error through the
// single exit is what makes that impossible to get wrong later: a new error exit
// cannot be added without saying so here, because the signature will not compile
// otherwise.
func (e *Engine) finish(ctx context.Context, rep DrainReport, started time.Time, err error) (DrainReport, error) {
	if err != nil && rep.Outcome == "" {
		rep.Outcome = OutcomeFailed
		rep.Reason = "the run stopped on an error: " + err.Error()
	}
	if rep.Outcome == "" {
		rep.Outcome = OutcomeDone
	}
	// Derived from the issue reports rather than run state, because this is the
	// one park detail run state keeps only as prose in Notes, and Notes is
	// capped: a long run drops its oldest breadcrumbs, and a missing edge found
	// in wave one should still be on the report at the end of wave nine.
	rep.MissingDeps = mergeMissingDeps(rep.Issues)

	if st, err := runstate.Load(e.RepoRoot); err == nil {
		rep.Done = append([]string(nil), st.Done...)
		rep.Parked = parkedIDs(st)
		rep.Base, rep.EpicBranch = st.Base, st.EpicBranch
		if rep.Epic == "" {
			rep.Epic = st.Epic
		}
	}
	if n := len(rep.Integrations); n > 0 {
		last := rep.Integrations[n-1]
		rep.EpicReason = last.EpicReason
	}
	for _, in := range rep.Integrations {
		if in.EpicClosed {
			rep.EpicClosed = true
		}
	}
	rep.Seconds = time.Since(started).Seconds()

	// Last, and with the totals already in the report: what a pull request says
	// about a run is read off the finished report, and the predicate in front of
	// it reads the parked list this function just filled in.
	h := e.Handoff(ctx, rep)
	rep.Handoff = &h

	// After the handoff rather than before it, and advisory either way. A hook
	// placed in front of the handoff would be a hook that looks like it could
	// stop one; placed behind it, the report it reads is the whole run
	// including where it was handed over, which is the more useful input and
	// the honest position.
	if verdictOutcome(rep.Outcome) {
		rep.Hooks = e.runEndHooks(ctx, rep)
		rep.Usage = rep.Usage.Add(hookUsage(rep.Hooks))
	}

	status := runstate.StatusDone
	if rep.Outcome == OutcomeInterrupted || rep.Outcome == OutcomeInfra {
		// Still armed. The run is not over; it is waiting to be re-run.
		status = runstate.StatusActive
	}
	_, _ = runstate.Update(e.RepoRoot, false, func(s *runstate.State) error {
		s.Status = status
		s.Note("drain ended %s %s", rep.Outcome, runShape(rep))
		return nil
	})

	e.Bus.Emit(Event{Kind: EventRunEnd, Run: &rep, Usage: rep.Usage, Text: rep.Reason})
	return rep, err
}

// startRun creates or adopts the run state for this drain.
//
// Adopting rather than replacing is what makes re-running after an interrupt the
// obvious thing to do: the scope, the attempt counts and the recorded sessions
// are all still there, and the run picks up where it stopped.
func (e *Engine) startRun(o DrainOptions) (*runstate.State, error) {
	return runstate.Update(e.RepoRoot, true, func(s *runstate.State) error {
		if s.Epic != "" && o.Epic != "" && s.Epic != o.Epic {
			return fmt.Errorf("drain: a run for %s is already active; stop it before draining %s",
				s.Epic, o.Epic)
		}
		if s.Version == 0 {
			s.Version = runstate.Version
		}
		if s.StartedAt.IsZero() {
			s.StartedAt = time.Now().UTC()
		}
		if o.Epic != "" {
			s.Epic = o.Epic
		}
		s.Status = runstate.StatusActive
		if o.Concurrency > 0 {
			s.Concurrency = o.Concurrency
		}
		if s.Concurrency <= 0 {
			s.Concurrency = config.DefaultConcurrency
		}
		if o.Autonomy != "" {
			s.Autonomy = string(o.Autonomy)
		}
		if s.Autonomy == "" {
			s.Autonomy = string(config.AutonomyAuto)
		}
		s.Retry = e.attempts() - 1
		if len(o.Scope) > 0 {
			s.Scope = append([]string(nil), o.Scope...)
		}
		s.Note("drain started for %s (%d in scope, concurrency %d, autonomy %s)",
			nameOr(s.Epic, "no epic"), len(s.Scope), s.Concurrency, s.Autonomy)
		return nil
	})
}

// nextWave returns the issues to run next: whatever an interrupted run left in
// flight, otherwise a freshly planned wave.
//
// The in-flight case comes first and does not re-plan, because an in-flight
// issue is excluded from planning by design — it is already dispatched. Picking
// it up here is what turns a killed run into a resumed one rather than a run
// that silently skips whatever it was working on.
func (e *Engine) nextWave(st *runstate.State) ([]wave.Issue, error) {
	if resume := wave.Resume(st, e.planOptions(st)); len(resume) > 0 {
		return resume, nil
	}
	res, err := wave.Plan(e.BD, st, e.planOptions(st))
	if err != nil {
		return nil, err
	}
	if len(res.Issues) == 0 {
		return nil, nil
	}
	// Record is what advances the wave counter, and it advances it on disk. The
	// caller's copy has to move with it: every event this wave emits takes its
	// number from st, and a stale st numbers the whole wave as the one before
	// it — which for the first wave is zero, rendered as no wave at all, while
	// the barrier reloads state and reports the real number underneath it.
	updated, err := wave.Record(e.RepoRoot, res.Issues)
	if err != nil {
		return nil, err
	}
	*st = *updated
	return res.Issues, nil
}

func (e *Engine) planOptions(st *runstate.State) wave.Options {
	conc := st.Concurrency
	if conc <= 0 {
		conc = config.DefaultConcurrency
	}
	return wave.Options{Concurrency: conc, Branch: e.Cfg.Branch}
}

// runWave runs one wave with at most conc issues in flight, and keeps it that
// way: a worker that finishes frees a slot, and the wave asks bd for something
// to put in it rather than holding it empty until the barrier.
//
// conc comes from run state rather than config, because a run keeps the
// concurrency it was started with even if .beads-auto.yaml changes underneath
// it. Everything else about holding the cap is the pool's; a wave differs from a
// continuous run only in what it puts into a freed slot and in having a barrier
// to integrate at, so it hands the pool a refill and no landing hook.
func (e *Engine) runWave(ctx context.Context, waveNo, conc int, issues []wave.Issue) ([]Report, error) {
	w := newPool(e, ctx, waveNo, conc)
	defer w.cancel()
	w.refill = func(free int) []wave.Issue { return e.waveTopUp(waveNo, free) }

	for _, iss := range issues {
		w.dispatch(iss)
	}
	w.wg.Wait()
	return w.results()
}

// waveTopUp is what may go into the slots a running wave has free.
//
// bd's ready front, minus what this run has already handled, intersected with
// the scope — the same question the planner answers, asked again rather than
// answered a second way — and then narrowed to what can actually be built on
// the checkout as it stands. See wave.Joinable.
func (e *Engine) waveTopUp(waveNo, free int) []wave.Issue {
	st, err := runstate.Load(e.RepoRoot)
	if err != nil || st.Status != runstate.StatusActive {
		// Unreadable, stopped, or paused by a human. None of those is a run to
		// hand more work to.
		return nil
	}
	opt := e.planOptions(st)
	opt.Concurrency = free
	res, err := wave.Plan(e.BD, st, opt)
	if err != nil {
		e.logf("warning: could not ask bd what to put in wave %d's free slot(s): %v", waveNo, err)
		return nil
	}
	joining := wave.Joinable(e.BD, st, res.Issues)
	if len(joining) == 0 {
		return nil
	}
	if _, err := wave.Join(e.RepoRoot, joining); err != nil {
		e.logf("warning: could not record %s joining wave %d: %v",
			strings.Join(wave.IDs(joining), ", "), waveNo, err)
		return nil
	}
	e.logf("wave %d: %s joined it in a freed slot", waveNo, strings.Join(wave.IDs(joining), ", "))
	return joining
}

// forIssue clones the engine for one worker.
//
// The clone exists for the sink: every runner event has to be tagged with the
// issue that produced it before it reaches the bus, and the runner layer cannot
// know that. The runner cache is dropped rather than shared, because a map
// written by five goroutines is a data race whatever it holds.
//
// The wave is carried for the same reason one step up: the stage boundaries the
// engine raises itself go onto the bus beside those runner events, and a watcher
// that groups by wave needs them tagged the same way.
func (e *Engine) forIssue(waveNo int, issue string) *Engine {
	c := *e
	c.runners = nil
	c.waveNo = waveNo
	c.Watch(waveNo, issue)
	return &c
}

// Watch points this engine's activity sink at its bus, for one issue.
//
// It is a method rather than a bare assignment because the sink and the engine
// have to share one set of marks: the sink is made once and lives for the whole
// issue, while the attempt and the round move underneath it. A caller running a
// single issue in its own process needs the same pairing a wave gets, so both
// go through here.
//
// No bus is no sink, which is what a quiet run gets.
func (e *Engine) Watch(waveNo int, issue string) {
	if e.Bus == nil {
		return
	}
	e.marks = &Marks{}
	e.Sink = e.Bus.Sink(waveNo, issue, e.marks)
}

// mark records where the issue this clone is running has got to, so that the
// model activity streaming out of it is tagged with the same two numbers the
// engine puts on the stage boundaries it raises itself.
func (e *Engine) mark(attempt, round int) { e.marks.Set(attempt, round) }

// buildIndex builds the code index for this run, or says why it did not.
func (e *Engine) buildIndex(ctx context.Context) {
	idx := graph.Build(ctx, e.RepoRoot, e.graphOptions(), e.logf)
	if !idx.Built && e.Cfg.Graph.Enabled {
		e.logf("%s", idx.Why)
	}
}

// refreshIndex updates the index after a barrier has merged, so the next wave
// reads the code as it now is rather than as it was when the run started.
func (e *Engine) refreshIndex(ctx context.Context) {
	if !e.Cfg.Graph.Enabled || !e.Cfg.Graph.Refresh {
		return
	}
	graph.Refresh(ctx, e.RepoRoot, e.graphOptions(), e.logf)
}

func (e *Engine) graphOptions() graph.Options {
	return graph.Options{
		Enabled:      e.Cfg.Graph.Enabled,
		ExcludeTests: e.Cfg.Graph.ExcludeTests,
		Timeout:      time.Duration(e.Cfg.Graph.Timeout) * time.Second,
	}
}

// stoppedBeforeStart is the report for an issue the wave never dispatched.
func (e *Engine) stoppedBeforeStart(iss wave.Issue) Report {
	rep := Report{
		Issue: iss.ID, Branch: iss.Branch, Outcome: OutcomeInterrupted,
		Reason: "the wave stopped before this issue started",
	}
	return e.settleKill(rep)
}

// settleKill turns a worker a human ended into a recorded failure.
//
// A kill and an interrupt both arrive as a cancelled context, and the engine
// cannot tell them apart from the result alone — which matters, because reading
// a kill as an interrupt would stop the whole wave over one issue somebody
// changed their mind about, and would leave that issue in flight to be resumed
// by the next run.
//
// So a killed issue is parked, which is what keeps the planner from offering it
// again and the barrier from merging a branch nothing judged, and it is reported
// failed, because that is what happened to it. Its siblings are untouched.
func (e *Engine) settleKill(rep Report) Report {
	reason, killed := e.Control.Killed(rep.Issue)
	if !killed || rep.Outcome != OutcomeInterrupted {
		return rep
	}
	rep.Outcome, rep.Stage, rep.Reason = OutcomeFailed, StageKilled, reason
	if err := e.BD.Park(rep.Issue, "bd-auto parked "+rep.Issue+": "+reason); err != nil {
		e.logf("warning: could not park %s after it was killed: %v", rep.Issue, err)
	}
	deps, err := e.recordParked(rep.Issue, reason, StageKilled)
	if err != nil {
		e.logf("warning: could not record %s as parked after it was killed: %v", rep.Issue, err)
	}
	rep.MissingDeps = deps
	return rep
}

// waveStopped reports whether the wave ended on something that is not a verdict.
func waveStopped(reports []Report) (Outcome, string) {
	for _, r := range reports {
		if r.Outcome == OutcomeInfra {
			return OutcomeInfra, fmt.Sprintf("%s stopped on the environment: %s", r.Issue, r.Reason)
		}
	}
	for _, r := range reports {
		if r.Outcome == OutcomeInterrupted {
			return OutcomeInterrupted, fmt.Sprintf("%s was interrupted: %s", r.Issue, r.Reason)
		}
	}
	return "", ""
}

// barrier integrates the finished wave and records what it produced.
//
// All is set because a barrier settles the run, not just the wave: a branch left
// unmerged by an earlier wave that stopped would otherwise never be looked at
// again. Anything already merged has no branch left to merge, so widening the
// candidates costs nothing.
func (e *Engine) barrier(ctx context.Context, rep *DrainReport) error {
	in, err := e.Integrate(ctx, IntegrateOptions{All: true})
	rep.Integrations = append(rep.Integrations, in)
	rep.Usage = rep.Usage.Add(in.Usage)
	if err != nil {
		return err
	}
	e.Bus.Emit(Event{Kind: EventWaveEnd, Wave: in.Wave, Integration: &rep.Integrations[len(rep.Integrations)-1], Usage: in.Usage})
	if in.Stopped != "" {
		rep.Outcome, rep.Reason = in.Stopped, in.Reason
	}
	return nil
}

// pauseAtBarrier holds the run at the barrier under autonomy: wave. It reports
// whether the run should stop rather than carry on.
func (e *Engine) pauseAtBarrier(ctx context.Context, rep *DrainReport) bool {
	st, err := runstate.Load(e.RepoRoot)
	if err != nil || st.Autonomy != string(config.AutonomyWave) {
		return false
	}
	// Nothing left to pause in front of.
	if next, err := e.nextWavePeek(st); err == nil && len(next) == 0 {
		return false
	}
	updated, err := runstate.Update(e.RepoRoot, false, func(s *runstate.State) error {
		s.Status = runstate.StatusPaused
		s.Note("paused at the wave %d barrier", s.Wave)
		return nil
	})
	if err != nil {
		return false
	}
	return e.awaitResume(ctx, updated, rep)
}

// pauseBoundary says where a paused run has stopped.
//
// A wave run stops at its barrier, which is a boundary the scheduler makes for
// itself. A continuous run has none, so it stops at the only defined point it
// has: dispatch refuses a paused run, and the loop waits once everything
// already in flight has landed and been integrated.
func pauseBoundary(st *runstate.State) string {
	if st.Autonomy == string(config.AutonomyWave) {
		return fmt.Sprintf("paused at the wave %d barrier", st.Wave)
	}
	return "paused; what was in flight has landed and integrated, and nothing more will be dispatched"
}

// nextWavePeek reports what the next wave would be without recording it.
func (e *Engine) nextWavePeek(st *runstate.State) ([]wave.Issue, error) {
	if resume := wave.Resume(st, e.planOptions(st)); len(resume) > 0 {
		return resume, nil
	}
	res, err := wave.Plan(e.BD, st, e.planOptions(st))
	return res.Issues, err
}

// awaitResume blocks while the run is paused, and reports whether the run must
// stop instead of continuing.
//
// Polling run state is the mechanism on purpose: `bd-auto run resume` writes the
// file, and a paused drain that survives its own terminal being closed is worth
// more than a cheaper signal.
func (e *Engine) awaitResume(ctx context.Context, st *runstate.State, rep *DrainReport) bool {
	if st.Status != runstate.StatusPaused {
		return false
	}
	// Said here rather than by whatever set the state, because both callers
	// reach this and only one of them did the setting: a wave run pauses itself
	// at its barrier, and a continuous run is paused by `bd-auto run pause`
	// from another process entirely and finds out about it here.
	where := pauseBoundary(st)
	e.logf("%s; `bd-auto run resume` continues", where)
	e.Bus.Emit(Event{Kind: EventPaused, Wave: st.Wave, Text: where})
	for {
		if err := e.sleep(ctx, DefaultPollInterval); err != nil {
			rep.Outcome, rep.Reason = OutcomeInterrupted, "the run was interrupted while paused"
			return true
		}
		cur, err := runstate.Load(e.RepoRoot)
		if errors.Is(err, runstate.ErrNoRun) {
			rep.Outcome, rep.Reason = OutcomeInterrupted, "the run was stopped while paused"
			return true
		}
		if err != nil {
			rep.Outcome, rep.Reason = OutcomeInterrupted, "the run state became unreadable while paused: "+err.Error()
			return true
		}
		switch cur.Status {
		case runstate.StatusPaused:
			continue
		case runstate.StatusDone:
			rep.Outcome, rep.Reason = OutcomeInterrupted, "the run was stopped while paused"
			return true
		default:
			e.Bus.Emit(Event{Kind: EventResumed, Wave: cur.Wave})
			return false
		}
	}
}

// --- scope enforcement ---

// parkOutOfScope parks every scoped issue whose blocker the run can never
// satisfy: one outside the scope, or one inside it that bd has deferred.
//
// It runs before the first wave because that is the only moment the answer is
// cheap and complete. Left alone, such an issue never appears in a ready front,
// never gets dispatched, and ends the run with nothing recorded against it at
// all — the one outcome a human cannot act on.
func (e *Engine) parkOutOfScope(st *runstate.State, rep *DrainReport) error {
	if len(st.Scope) == 0 {
		return nil
	}
	var pending []string
	for _, id := range st.Scope {
		if !st.Excluded(id) {
			pending = append(pending, id)
		}
	}
	blockers, err := scope.Blocked(e.BD, pending, time.Now())
	if err != nil {
		return err
	}
	for _, b := range blockers {
		reason := fmt.Sprintf("bd-auto parked %s before dispatch: %s. %s", b.Issue, b.Reason, b.Fix)
		e.park(b.Issue, reason)
		rep.Issues = append(rep.Issues, Report{
			Issue: b.Issue, Branch: e.Cfg.Branch(b.Issue),
			Outcome: OutcomeParked, Stage: StageScope, Reason: b.Reason,
		})
		e.Bus.Emit(Event{Kind: EventScopeParked, Issue: b.Issue, Text: b.Reason})
	}
	return nil
}

// parkStranded parks scoped issues the run never got a verdict on.
//
// Reaching here means the planner found nothing left to dispatch while these
// were still open, so bd never offered them: something they depend on did not
// complete. That is a result, and recording it is what stops a run from ending
// with work silently missing.
func (e *Engine) parkStranded(rep *DrainReport) error {
	st, err := runstate.Load(e.RepoRoot)
	if err != nil || len(st.Scope) == 0 {
		return nil
	}
	for _, id := range st.Scope {
		if st.Excluded(id) {
			continue
		}
		iss, err := e.BD.Show(id)
		if err != nil || iss == nil || iss.Terminal() {
			continue
		}
		reason := strandedReason(iss, st, time.Now())
		e.park(id, "bd-auto parked "+id+": "+reason)
		rep.Issues = append(rep.Issues, Report{
			Issue: id, Branch: e.Cfg.Branch(id),
			Outcome: OutcomeParked, Stage: StageScope, Reason: reason,
		})
		e.Bus.Emit(Event{Kind: EventScopeParked, Issue: id, Text: reason})
	}
	return nil
}

// strandedReason says what held an issue back, naming the parked dependency
// where there is one because that is the issue a human has to fix first.
//
// The last branch used to read "never became ready, and the run drained without
// bd ever offering it", which is a description of the engine's ignorance
// dressed as a diagnosis: it fits a deferred issue, a planner disagreement and a
// bd outage equally, so a human reading it on five issues learns nothing and
// starts guessing. Every branch here now names the evidence it is asserting on
// and the command that would show the same thing, so a wrong one is falsifiable
// in a few seconds rather than an afternoon.
func strandedReason(iss *bd.Issue, st *runstate.State, now time.Time) string {
	// Ahead of the dependencies, because it is a fact about this issue rather
	// than a guess about its graph: bd hides a deferred issue from every ready
	// front, so it can be the whole explanation even with nothing unmet.
	if iss.Deferred(now) {
		return fmt.Sprintf(
			"never became ready: bd has it deferred until %s, so it was in the scope but could "+
				"not appear in any ready front. `bd update %s --defer=` undefers it.",
			iss.DeferUntil.UTC().Format("2006-01-02"), iss.ID)
	}

	var unmet []string
	for _, d := range iss.Dependencies {
		if d.ID == "" || d.Status == "closed" {
			continue
		}
		if st.IsParked(d.ID) {
			return fmt.Sprintf("never became ready: it depends on %s, which this run parked", d.ID)
		}
		if d.Deferred(now) {
			return fmt.Sprintf(
				"never became ready: it depends on %s, which bd has deferred until %s and will "+
					"never offer to a wave. `bd update %s --defer=` undefers it.",
				d.ID, d.DeferUntil.UTC().Format("2006-01-02"), d.ID)
		}
		unmet = append(unmet, d.ID)
	}
	if len(unmet) > 0 {
		return fmt.Sprintf("never became ready: still waiting on %s", strings.Join(unmet, ", "))
	}
	return fmt.Sprintf(
		"never became ready, and nothing here explains it: %s is open, is not deferred, and has "+
			"no unmet dependency, yet bd did not offer it in any wave of this run. Compare "+
			"`bd ready` with `bd show %s` — this is bd and the run disagreeing, not a blocker.",
		iss.ID, iss.ID)
}

// StageScope is the stage recorded against an issue the scope itself stopped.
const StageScope = "scope"

func nameOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
