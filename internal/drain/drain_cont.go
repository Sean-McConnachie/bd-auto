package drain

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
	"bd-auto/internal/wave"
)

// Continuous scheduling is what autonomy: auto does.
//
// A wave already grows into the slots its own workers free, so half of "keep N
// workers in flight" was there before this. What was not is the barrier. A wave
// run joins every worker before it merges anything, so the last slow issue in a
// wave leaves every other slot empty until it lands and then nothing runs at all
// while the integrator works — and a dependent could not join the wave its
// dependency was in, because that dependency's branch is not in the checkout
// until the barrier puts it there.
//
// So there is no barrier here. Each issue is merged and gated on its own as it
// lands, serialised behind one lock, and the workers carry on around it. That is
// what makes the two problems one problem: a merge that finishes is a dependency
// that is now in HEAD, so wave.Joinable stops holding its dependents back and
// the merging worker's own refill starts them. Readiness is asked again on every
// completion and again on every merge, which are the only two moments it can
// change.
//
// What the barrier used to be responsible for besides merging still has to
// happen exactly once: the epic decision, the discovered issues, the
// reconciliation. Those stay at the end, in one settling barrier that merges
// nothing new. See drainContinuously.
//
// Three things this had to keep working, none of them obvious from the shape:
//
// The checkout is no longer quiet while workers run. gitguard's base-moved check
// used to mean "nothing may write to the branch a worker was told not to touch",
// and the integrator now does, legitimately, several times per run. It is told
// which commits were its own; see gitguard.Baseline.Integrated.
//
// A run with no wave boundary still has to have somewhere to stop. `bd-auto run
// pause` marks the state paused, dispatch refuses a paused run, and the loop
// below waits once everything in flight has landed and been integrated. That is
// the boundary: not a wave, but a quiet point the run reaches on its own.
//
// And a run with no wave count still needs a runaway guard. The bound is on
// workers dispatched rather than on waves, for the same reason DefaultMaxWaves
// exists: to stop a loop that is not making progress, not to bound the spend.

// drainContinuously holds the concurrency cap full for as long as there is
// anything in scope to work on.
func (e *Engine) drainContinuously(ctx context.Context, opts DrainOptions, rep DrainReport, started time.Time) (DrainReport, error) {
	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		return e.finish(ctx, rep, started, err)
	}
	rep.Continuous = true
	rep.Waves = 1

	c := newContRun(e, ctx, st, opts)
	defer c.pool.cancel()
	// Whatever the scope check parked before anything was dispatched. harvest
	// replaces the issue list rather than appending to it, so it has to be told
	// what was already on it.
	c.pre = rep.Issues

	// The barrier an interrupted run never reached. Anything it left dispatched
	// and unmerged is on the list this run's dispatch filter reads, so leaving
	// it there would hold every dependent of it back for the whole run — and
	// nothing else is ever going to merge it, because the worker that would
	// have landed it is gone.
	if err := c.settleLeftovers(&rep); err != nil {
		return e.finish(ctx, rep, started, err)
	}

	for {
		if err := ctx.Err(); err != nil {
			rep.Outcome, rep.Reason = OutcomeInterrupted, "the run was interrupted"
			return e.finish(ctx, rep, started, nil)
		}
		cur, err := runstate.Load(e.RepoRoot)
		if err != nil {
			return e.finish(ctx, rep, started, err)
		}
		if stop := e.awaitResume(ctx, cur, &rep); stop {
			return e.finish(ctx, rep, started, nil)
		}

		before := c.started()
		c.pool.topUp()
		// Returns when every worker has finished AND every issue one of them
		// landed has been through the integrator: the goroutine that does the
		// merging is the worker's own, and it is still counted until the merge
		// is done. There is nothing in flight when this returns.
		c.pool.wg.Wait()

		werr := c.harvest(&rep)
		if werr != nil {
			return e.finish(ctx, rep, started, werr)
		}
		if stop, reason := waveStopped(rep.Issues); stop != "" {
			// Nothing that stopped here was judged, so nothing is parked for
			// it: the branches, worktrees and sessions are all left where they
			// are for the re-run. What already merged stays merged — it landed
			// and was gated before anything went wrong, and taking it back out
			// would throw away finished work over an unrelated outage.
			rep.Outcome, rep.Reason = stop, reason
			return e.finish(ctx, rep, started, nil)
		}
		if c.overBudget() {
			rep.Outcome = OutcomeFailed
			rep.Reason = fmt.Sprintf(
				"stopped after dispatching %d worker(s) without draining the scope", c.budget)
			return e.finish(ctx, rep, started, nil)
		}

		// Nothing is in flight. Either a human paused the run while it was
		// working — in which case this is the boundary pause promised, and the
		// top of the loop waits at it — or there was nothing left to start and
		// the scope is drained.
		if cur, err := runstate.Load(e.RepoRoot); err == nil && cur.Status == runstate.StatusPaused {
			continue
		}
		if c.started() == before {
			// Nothing started, so this is the end of the run — unless the last
			// thing the scheduler heard from bd was a failure, in which case
			// the run did not drain the scope, it lost the ability to ask.
			if err := c.lastPlanErr(); err != nil {
				return e.finish(ctx, rep, started, err)
			}
			break
		}
	}

	// The settling barrier. It merges nothing new — every issue that landed was
	// merged as it landed — and it is here for the three things a barrier does
	// besides merging, each of which must happen exactly once at the end of a
	// run: reconcile what bd reverted underneath it, file what the workers
	// discovered, and decide the epic. It also gates the final tree, which is
	// what the handoff's "green on the fully merged epic branch" rests on.
	if err := e.barrier(ctx, &rep); err != nil {
		return e.finish(ctx, rep, started, err)
	}
	if rep.Outcome != "" {
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

// contRun is one continuous run: the workers, the single integrator behind
// them, and the two counters that bound the whole thing.
type contRun struct {
	e    *Engine
	pool *pool
	// integ serialises integration. Two merges must never overlap: they land on
	// one branch in one checkout, and the gate that decides whether a merge
	// stays runs on that checkout too, so a second merge arriving in the middle
	// would be gating somebody else's tree.
	integ sync.Mutex
	// budget bounds the workers this run may dispatch. It is the runaway guard
	// DefaultMaxWaves is in a wave run — a scope of N issues needs at most N
	// workers, so anything past a wave's worth per issue is a loop that is not
	// making progress rather than a run that needs more room.
	budget int

	// pre is the reports that existed before the first worker: the issues the
	// scope check parked. It never changes once set.
	pre []Report

	mu           sync.Mutex
	integrations []IntegrateReport
	// dispatched counts every worker this run has started, which is what the
	// budget is spent on and what says whether a round made progress.
	dispatched int
	spent      bool
	// err is the first failure the integrator hit that is not about the work.
	// It is kept rather than returned because the integrator runs inside a
	// worker's goroutine, and the run reports it from the loop above.
	err error
	// planErr is the last failure asking bd what to start next, and it is kept
	// for one reason: a run that cannot ask cannot claim the scope is drained.
	//
	// It is not fatal on its own. A blip while workers are still running is
	// answered by the next refill, and the successful one clears it. What it
	// must not do is fall through to the end of the run, where a settling
	// barrier would gate a green tree, nothing would be parked, and the handoff
	// would open a pull request over an epic the run never finished asking
	// about. So it is checked exactly where the loop would otherwise stop.
	planErr error
	// stale says a merge has landed since the code index was last built.
	//
	// The index is only ever read when a worker is dispatched, so it is
	// refreshed there rather than after each merge: a run that rebuilt it once
	// per merge would pay for it once per issue whether or not anything was
	// left to start, which is the cost beads-auto-imp-xhw put it at the barrier
	// to avoid. Deferring it to the dispatch that needs it keeps that property
	// with no barrier to hang it on.
	stale bool
	// resumed records that whatever an interrupted run left in flight has been
	// handed back. It happens once, on the first dispatch.
	resumed bool
	// opened records that the run's one wave has been announced on the stream.
	opened bool
}

func newContRun(e *Engine, ctx context.Context, st *runstate.State, opts DrainOptions) *contRun {
	conc := st.Concurrency
	if conc <= 0 {
		conc = config.DefaultConcurrency
	}
	maxWaves := opts.MaxWaves
	if maxWaves <= 0 {
		maxWaves = DefaultMaxWaves
	}
	// The wave number every event carries. A continuous run has one for its
	// whole life: the workers and the integration lane are one thing happening,
	// and numbering them apart would invite a watcher to draw a boundary that
	// is not there.
	no := st.Wave
	if no <= 0 {
		no = 1
	}

	c := &contRun{e: e, budget: maxWaves * conc}
	c.pool = newPool(e, ctx, no, conc)
	c.pool.refill = c.plan
	c.pool.landed = c.integrate
	return c
}

// plan is what may go into the slots the run has free.
//
// bd's ready front, minus what this run has already handled, intersected with
// the scope — the planner's own question, asked again rather than answered a
// second way — and then narrowed to what can actually be built on the checkout
// as it stands. That last filter is the one that moves here: it rejects an issue
// whose dependency this run has dispatched and not yet merged, and a continuous
// run merges one issue at a time, so it relaxes one issue at a time too.
func (c *contRun) plan(free int) []wave.Issue {
	e := c.e
	st, err := runstate.Load(e.RepoRoot)
	if err != nil || st.Status != runstate.StatusActive {
		// Unreadable, stopped, or paused by a human. None of those is a run to
		// hand more work to.
		return nil
	}

	var out []wave.Issue
	// Whatever an interrupted run left in flight, once, and not planned for. An
	// in-flight issue is already dispatched and already recorded, which is
	// exactly why Plan excludes it; asking bd again would either offer it as new
	// work, resetting the attempt it was on, or — more likely, since a claimed
	// issue is not in a ready front — not at all.
	if !c.takeResume() {
		out = wave.Resume(st, e.planOptions(st))
		c.count(len(out))
		free -= len(out)
	}

	if free > 0 {
		out = append(out, c.planFresh(st, free)...)
	}
	if len(out) == 0 {
		return nil
	}
	// Before the worktrees that will read it, and only when something is about
	// to. See contRun.stale.
	c.refreshIndex()
	c.announce(out)
	return out
}

// announce opens the run's one wave on the stream, once.
//
// A watcher needs a wave to hang its rows off: the number every event carries,
// and the moment the issues stop being a scope and start being work. A
// continuous run has exactly one of each, so this says so once and tags it as a
// lane, which is what stops a view drawing "wave 1" over a run that has no
// second one to compare it to.
func (c *contRun) announce(out []wave.Issue) {
	c.mu.Lock()
	first := !c.opened
	c.opened = true
	c.mu.Unlock()
	if !first {
		return
	}
	c.e.Bus.Emit(Event{Kind: EventWaveStart, Wave: c.pool.no, Lane: true, Issues: wave.IDs(out)})
}

// planFresh asks bd for work that has not been dispatched yet and records it as
// started, so a killed run resumes it rather than starting it twice.
func (c *contRun) planFresh(st *runstate.State, free int) []wave.Issue {
	e := c.e
	opt := e.planOptions(st)
	opt.Concurrency = free
	res, err := wave.Plan(e.BD, st, opt)
	c.notePlan(err)
	if err != nil {
		e.logf("warning: could not ask bd what to start next: %v", err)
		return nil
	}
	joining := c.afford(wave.Joinable(e.BD, st, res.Issues))
	if len(joining) == 0 {
		return nil
	}
	// Record opens the run's one wave; every dispatch after it joins that wave
	// rather than starting another. A resumed run already has one open, and
	// re-recording would clear the issues still waiting to land out of the very
	// list the dispatch filter reads.
	record := wave.Join
	if st.Wave == 0 {
		record = wave.Record
	}
	if _, err := record(e.RepoRoot, joining); err != nil {
		c.notePlan(err)
		e.logf("warning: could not record %s as started: %v",
			strings.Join(wave.IDs(joining), ", "), err)
		return nil
	}
	e.logf("started %s in a free slot", strings.Join(wave.IDs(joining), ", "))
	return joining
}

// integrate merges one landed issue and gates the result, one at a time.
//
// It runs in the goroutine of the worker that produced the branch, after that
// worker has already given its slot back — so the cap is still full while this
// works, which is the whole point of doing it here rather than at a barrier.
func (c *contRun) integrate(iss wave.Issue, rep Report) {
	if rep.Outcome != OutcomeDone {
		// Nothing to merge and nothing to decide. A parked issue's branch is
		// not a candidate — keeping half-done work out of the tree is what
		// parking is for — and neither an interrupt nor an outage is a verdict
		// on anybody's work.
		return
	}
	c.integ.Lock()
	defer c.integ.Unlock()
	if c.pool.stopped() {
		// An outage or a stop arrived while this was queued behind another
		// merge. Merging now would be starting new work on a run that has
		// stopped, and the branch keeps until the re-run.
		return
	}

	in, err := c.e.Integrate(c.pool.ctx, IntegrateOptions{Only: []string{iss.ID}, Lane: true})

	c.mu.Lock()
	c.integrations = append(c.integrations, in)
	if in.Head != in.BaseHead {
		c.stale = true
	}
	if err != nil && c.err == nil {
		c.err = err
	}
	c.mu.Unlock()

	c.e.Bus.Emit(Event{Kind: EventWaveEnd, Wave: c.pool.no, Lane: true,
		Integration: &in, Usage: in.Usage})

	if err != nil || in.Stopped != "" {
		// An integrator that cannot run, or one an outage or an interrupt
		// stopped, is not a reason to send the next worker at the same wall.
		c.pool.cancel()
	}
}

// settleLeftovers merges what an interrupted run dispatched and never landed.
//
// It is the only place a continuous run integrates something it did not itself
// produce, and it happens before the first worker so that a dependent of one of
// those branches is dispatchable from the start rather than after a barrier the
// run is never going to have.
func (c *contRun) settleLeftovers(rep *DrainReport) error {
	st, err := runstate.Load(c.e.RepoRoot)
	if err != nil {
		return err
	}
	var left []string
	for _, id := range st.WaveIssues {
		if _, inflight := st.InFlight[id]; !inflight {
			left = append(left, id)
		}
	}
	if len(left) == 0 {
		return nil
	}
	c.e.logf("settling %s, left unmerged by an earlier run", strings.Join(left, ", "))
	in, err := c.e.Integrate(c.pool.ctx, IntegrateOptions{All: true, Only: left, Lane: true})

	c.mu.Lock()
	c.integrations = append(c.integrations, in)
	if in.Head != in.BaseHead {
		c.stale = true
	}
	c.mu.Unlock()

	c.e.Bus.Emit(Event{Kind: EventWaveEnd, Wave: c.pool.no, Lane: true,
		Integration: &in, Usage: in.Usage})
	c.harvest(rep)
	return err
}

// harvest folds everything the workers and the integrator have produced into
// the run's report, and returns the first failure that is not about the work.
//
// Both lists are replaced rather than appended to, and the totals recomputed
// from them, because both grow from underneath: the pool hands back every report
// it holds rather than only the new ones, and harvest is called again after
// every round. Adding would count the same worker's cost once per round it
// survived.
func (c *contRun) harvest(rep *DrainReport) error {
	reports, werr := c.pool.results()
	rep.Issues = append(append([]Report(nil), c.pre...), reports...)

	c.mu.Lock()
	rep.Integrations = append([]IntegrateReport(nil), c.integrations...)
	cerr := c.err
	c.mu.Unlock()

	var usage runner.Usage
	for _, r := range rep.Issues {
		usage = usage.Add(r.Usage)
	}
	for _, in := range rep.Integrations {
		usage = usage.Add(in.Usage)
	}
	rep.Usage = usage

	if werr != nil {
		return werr
	}
	return cerr
}

// refreshIndex rebuilds the code index if a merge has landed since it was last
// built, and does nothing otherwise.
func (c *contRun) refreshIndex() {
	c.mu.Lock()
	stale := c.stale
	c.stale = false
	c.mu.Unlock()
	if stale {
		c.e.refreshIndex(c.pool.ctx)
	}
}

// afford trims a plan to what the runaway guard still allows, and records that
// the guard has bitten when there is nothing left.
func (c *contRun) afford(in []wave.Issue) []wave.Issue {
	if len(in) == 0 {
		// Nothing to dispatch is not the guard biting. A run that has spent its
		// budget exactly and then drains asks this question one last time, and
		// answering "the guard stopped it" there would report a finished run as
		// a failed one.
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	left := c.budget - c.dispatched
	if left <= 0 {
		c.spent = true
		return nil
	}
	if len(in) > left {
		in = in[:left]
	}
	c.dispatched += len(in)
	return in
}

func (c *contRun) count(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dispatched += n
}

func (c *contRun) started() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dispatched
}

func (c *contRun) overBudget() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spent
}

// notePlan keeps the last answer bd gave the scheduler, failure or not.
func (c *contRun) notePlan(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.planErr = err
}

func (c *contRun) lastPlanErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.planErr
}

// takeResume reports whether the in-flight issues have already been handed
// back, and marks them as handed back if not.
func (c *contRun) takeResume() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.resumed
	c.resumed = true
	return was
}
