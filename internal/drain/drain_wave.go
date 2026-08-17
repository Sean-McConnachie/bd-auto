package drain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
	"bd-auto/internal/scope"
	"bd-auto/internal/wave"
)

// The wave loop is the whole run: plan a wave inside the scope, run it with
// bounded concurrency, integrate it, and go round again until every scoped
// issue is done or parked.
//
// Two things about it are not obvious from the shape.
//
// The scope is a hard allowlist, not a starting point. wave.Plan intersects bd's
// ready front with it, so an issue outside the scope is never dispatched however
// ready bd says it is — which is what keeps discovered work out of a run by
// construction. The other half of that promise is here: an in-scope issue whose
// blocker was never in the scope can never become ready, so it is parked before
// anything is spawned rather than sitting silently unready until the run ends.
//
// A wave stops on an outage. An issue that comes back infra-failed cancels its
// siblings, because five workers meeting one rate limit is one outage, and
// letting the rest keep trying converts it into a pile of parked issues and a
// bill. Neither an interrupt nor an outage is a verdict, so nothing is parked
// for it and every worktree, branch and session survives for the re-run.

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

	// Issues is every issue this run took a verdict on, in the order they
	// finished being planned.
	Issues []Report `json:"issues,omitempty"`
	// Integrations is one entry per wave barrier.
	Integrations []IntegrateReport `json:"integrations,omitempty"`

	Done   []string `json:"done,omitempty"`
	Parked []string `json:"parked,omitempty"`

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

	st, err := e.startRun(opts)
	if err != nil {
		return DrainReport{}, err
	}

	rep := DrainReport{Epic: st.Epic, Scope: st.Scope}
	e.Bus.Emit(Event{Kind: EventRunStart, Issues: st.Scope, Text: st.Epic})

	if err := e.parkOutOfScope(st, &rep); err != nil {
		return e.finish(rep, started), err
	}

	maxWaves := opts.MaxWaves
	if maxWaves <= 0 {
		maxWaves = DefaultMaxWaves
	}

	for rep.Waves < maxWaves {
		if err := ctx.Err(); err != nil {
			rep.Outcome, rep.Reason = OutcomeInterrupted, "the run was interrupted"
			return e.finish(rep, started), nil
		}

		st, err = runstate.Load(e.RepoRoot)
		if err != nil {
			return e.finish(rep, started), err
		}
		if stop := e.awaitResume(ctx, st, &rep); stop {
			return e.finish(rep, started), nil
		}

		issues, err := e.nextWave(st)
		if err != nil {
			return e.finish(rep, started), err
		}
		if len(issues) == 0 {
			break
		}

		rep.Waves++
		e.Bus.Emit(Event{Kind: EventWaveStart, Wave: st.Wave, Issues: issueIDs(issues)})

		reports, werr := e.runWave(ctx, st.Wave, st.Concurrency, issues)
		for _, r := range reports {
			rep.Issues = append(rep.Issues, r)
			rep.Usage = rep.Usage.Add(r.Usage)
		}
		if werr != nil {
			return e.finish(rep, started), werr
		}

		if stop, reason := waveStopped(reports); stop != "" {
			// Nothing here was judged, so nothing is parked and nothing is
			// merged: the branches, worktrees and sessions are all left for the
			// re-run to pick up.
			rep.Outcome, rep.Reason = stop, reason
			return e.finish(rep, started), nil
		}

		if err := e.barrier(ctx, &rep); err != nil {
			return e.finish(rep, started), err
		}
		if rep.Outcome != "" {
			return e.finish(rep, started), nil
		}

		if stop := e.pauseAtBarrier(ctx, &rep); stop {
			return e.finish(rep, started), nil
		}
	}

	if rep.Waves >= maxWaves {
		rep.Outcome = OutcomeFailed
		rep.Reason = fmt.Sprintf("stopped after %d waves without draining the scope", maxWaves)
		return e.finish(rep, started), nil
	}

	// The scope is drained, so anything still unresolved is an issue bd never
	// offered. Saying why is the difference between a finished run and a run
	// that quietly dropped work.
	if err := e.parkStranded(&rep); err != nil {
		return e.finish(rep, started), err
	}
	rep.Outcome = OutcomeDone
	return e.finish(rep, started), nil
}

// finish stamps the run's totals from run state and closes it out.
//
// Done and Parked are read back from run state rather than accumulated from the
// reports, because a barrier can park an issue whose own report said done: a
// branch that failed the wave gate did not land, whatever the worker that
// produced it reported.
func (e *Engine) finish(rep DrainReport, started time.Time) DrainReport {
	if rep.Outcome == "" {
		rep.Outcome = OutcomeDone
	}
	if st, err := runstate.Load(e.RepoRoot); err == nil {
		rep.Done = append([]string(nil), st.Done...)
		rep.Parked = parkedIDs(st)
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

	status := runstate.StatusDone
	if rep.Outcome == OutcomeInterrupted || rep.Outcome == OutcomeInfra {
		// Still armed. The run is not over; it is waiting to be re-run.
		status = runstate.StatusActive
	}
	_, _ = runstate.Update(e.RepoRoot, false, func(s *runstate.State) error {
		s.Status = status
		s.Note("drain ended %s after %d wave(s)", rep.Outcome, rep.Waves)
		return nil
	})

	e.Bus.Emit(Event{Kind: EventRunEnd, Run: &rep, Usage: rep.Usage, Text: rep.Reason})
	return rep
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
		s.Continuations = 0
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
	if _, err := wave.Record(e.RepoRoot, res.Issues); err != nil {
		return nil, err
	}
	return res.Issues, nil
}

func (e *Engine) planOptions(st *runstate.State) wave.Options {
	conc := st.Concurrency
	if conc <= 0 {
		conc = config.DefaultConcurrency
	}
	return wave.Options{Concurrency: conc, Branch: e.Cfg.Branch}
}

// runWave runs one wave with at most conc issues in flight.
//
// conc comes from run state rather than config, because a run keeps the
// concurrency it was started with even if .beads-auto.yaml changes underneath
// it. The bound is a semaphore rather than a worker pool because the wave is
// already capped by the planner; the semaphore is what holds the line when a
// resumed wave hands back more issues than the cap.
func (e *Engine) runWave(ctx context.Context, waveNo, conc int, issues []wave.Issue) ([]Report, error) {
	if conc <= 0 {
		conc = config.DefaultConcurrency
	}
	if conc > len(issues) {
		conc = len(issues)
	}

	// A cancellable child so an outage in one issue stops the others. The parent
	// context still wins, so a real interrupt is unaffected.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reports := make([]Report, len(issues))
	errs := make([]error, len(issues))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for i, iss := range issues {
		wg.Add(1)
		go func(i int, iss wave.Issue) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-wctx.Done():
				reports[i] = Report{
					Issue: iss.ID, Branch: iss.Branch, Outcome: OutcomeInterrupted,
					Reason: "the wave stopped before this issue started",
				}
				return
			}
			defer func() { <-sem }()

			e.Bus.Emit(Event{Kind: EventIssueStart, Wave: waveNo, Issue: iss.ID, Text: iss.Title})
			rep, err := e.forIssue(waveNo, iss.ID).Issue(wctx, iss.ID)
			reports[i], errs[i] = rep, err
			if rep.Outcome == OutcomeInfra {
				// One outage is one outage. Stop the siblings rather than let
				// them burn their budgets against the same wall.
				cancel()
			}
			e.Bus.Emit(Event{
				Kind: EventIssueEnd, Wave: waveNo, Issue: iss.ID,
				Outcome: rep.Outcome, Text: rep.Reason, Usage: rep.Usage, Report: &reports[i],
			})
		}(i, iss)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return reports, err
		}
	}
	return reports, nil
}

// forIssue clones the engine for one worker.
//
// The clone exists for the sink: every runner event has to be tagged with the
// issue that produced it before it reaches the bus, and the runner layer cannot
// know that. The runner cache is dropped rather than shared, because a map
// written by five goroutines is a data race whatever it holds.
func (e *Engine) forIssue(waveNo int, issue string) *Engine {
	c := *e
	c.runners = nil
	if e.Bus != nil {
		c.Sink = e.Bus.Sink(waveNo, issue)
	}
	return &c
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
	e.Bus.Emit(Event{Kind: EventPaused, Wave: updated.Wave})
	return e.awaitResume(ctx, updated, rep)
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
	e.logf("paused at the wave %d barrier; `bd-auto run resume` continues", st.Wave)
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

// parkOutOfScope parks every scoped issue whose blocker the run may never touch.
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
	blockers, err := scope.Blocked(e.BD, pending)
	if err != nil {
		return err
	}
	for _, b := range blockers {
		reason := fmt.Sprintf("bd-auto parked %s before dispatch: %s. Widen the scope to include %s, "+
			"or close it first, then unpark this issue.", b.Issue, b.Reason, b.Dep)
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
		reason := strandedReason(id, iss.Dependencies, st)
		e.park(id, "bd-auto parked "+id+": "+reason)
		rep.Issues = append(rep.Issues, Report{
			Issue: id, Branch: e.Cfg.Branch(id),
			Outcome: OutcomeParked, Stage: StageScope, Reason: reason,
		})
		e.Bus.Emit(Event{Kind: EventScopeParked, Issue: id, Text: reason})
	}
	return nil
}

// strandedReason says which dependency held an issue back, naming the parked
// one where there is one because that is the issue a human has to fix first.
func strandedReason(id string, deps []bd.Ref, st *runstate.State) string {
	var unmet []string
	for _, d := range deps {
		if d.ID == "" || d.Status == "closed" {
			continue
		}
		if st.IsParked(d.ID) {
			return fmt.Sprintf("never became ready: it depends on %s, which this run parked", d.ID)
		}
		unmet = append(unmet, d.ID)
	}
	if len(unmet) > 0 {
		return fmt.Sprintf("never became ready: still waiting on %s", strings.Join(unmet, ", "))
	}
	return "never became ready, and the run drained without bd ever offering it"
}

// StageScope is the stage recorded against an issue the scope itself stopped.
const StageScope = "scope"

func issueIDs(in []wave.Issue) []string {
	out := make([]string, 0, len(in))
	for _, i := range in {
		out = append(out, i.ID)
	}
	return out
}

func nameOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
