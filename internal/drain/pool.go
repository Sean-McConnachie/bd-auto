package drain

import (
	"context"
	"sync"

	"bd-auto/internal/config"
	"bd-auto/internal/wave"
)

// pool is the workers a run has in flight, and the one place that decides what
// goes into a slot one of them frees.
//
// It is shared by both schedulers because the part that is hard is the same for
// both: the cap has to hold while workers finish concurrently, the refill
// decision has to be taken one at a time, and an issue must never get two
// workers. What differs is only what may go into a freed slot and what happens
// to an issue that has just landed, and those are the two callbacks.
//
// The bound is a semaphore rather than a worker pool because the pool can be
// handed more issues than the cap — a resumed run hands back everything that
// was in flight, and a refill can land while a second worker is still finishing
// — and the semaphore is what holds the line in both cases.
type pool struct {
	e *Engine
	// no is the wave every event this pool raises is tagged with. A continuous
	// run keeps one number for its whole life; a wave has one per wave.
	no     int
	conc   int
	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}
	wg     sync.WaitGroup

	// refill is asked what to put in the slots that are free, and is called by
	// a worker that has just freed one. Nil never grows the pool. It is called
	// under top, so two workers finishing together cannot plan against the same
	// free slots and dispatch the same issue twice.
	refill func(free int) []wave.Issue
	// landed is called with each finished issue after its slot has been given
	// back. A continuous run integrates the issue there; a wave leaves it nil
	// and integrates every branch at the barrier.
	//
	// It runs after the slot is freed and refilled rather than before, which is
	// what keeps a merge from costing the run a worker: the goroutine doing the
	// integrating holds no slot, so the cap is still full while it works.
	landed func(wave.Issue, Report)

	// top serialises the refill decision.
	top sync.Mutex

	mu      sync.Mutex
	reports []Report
	errs    []error
	// live counts workers dispatched and not yet finished, which is what says
	// how many slots are free. It is not the semaphore's fill level: a worker
	// still queued on the semaphore is already this pool's, and its slot is
	// spoken for.
	live int
	// dispatched is every issue this pool has started a worker for. Run state
	// already keeps a refill from re-offering one, but a second worker on one
	// issue means two worktrees fighting over one branch, so it is not left to
	// a file another process can rewrite.
	dispatched map[string]bool
}

// newPool builds a pool of at most conc workers under a context of its own, so
// an outage in one issue can stop the others without touching the caller's.
func newPool(e *Engine, ctx context.Context, no, conc int) *pool {
	if conc <= 0 {
		conc = config.DefaultConcurrency
	}
	pctx, cancel := context.WithCancel(ctx)
	return &pool{
		e: e, no: no, conc: conc,
		ctx: pctx, cancel: cancel,
		sem:        make(chan struct{}, conc),
		dispatched: map[string]bool{},
	}
}

// dispatch starts a worker for one issue and takes the slot it will occupy.
func (p *pool) dispatch(iss wave.Issue) {
	p.mu.Lock()
	if p.dispatched[iss.ID] {
		p.mu.Unlock()
		return
	}
	p.dispatched[iss.ID] = true
	i := len(p.reports)
	p.reports = append(p.reports, Report{})
	p.errs = append(p.errs, nil)
	p.live++
	p.mu.Unlock()

	// Added before the caller's own Done, which is what makes it safe for a
	// finishing worker to grow the pool it is leaving.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.work(i, iss)
		// The freed slot first, and the integration after it. A worker that
		// integrated before giving its slot back would hold the run one worker
		// short for every merge it does.
		p.topUp()
		p.land(i, iss)
	}()
}

// work runs one issue to its verdict.
func (p *pool) work(i int, iss wave.Issue) {
	e := p.e

	// One cancel per issue, registered before the semaphore rather than after
	// it: an issue queued behind the concurrency cap is on screen and is
	// therefore something a human can decide against.
	ictx, kill := context.WithCancel(p.ctx)
	defer kill()
	e.Control.register(iss.ID, kill)
	defer e.Control.unregister(iss.ID)

	select {
	case p.sem <- struct{}{}:
	case <-ictx.Done():
		p.finish(i, iss, e.stoppedBeforeStart(iss), nil)
		return
	}
	defer func() { <-p.sem }()

	e.Bus.Emit(Event{Kind: EventIssueStart, Wave: p.no, Issue: iss.ID, Text: iss.Title})
	rep, err := e.forIssue(p.no, iss.ID).Issue(ictx, iss.ID)
	rep = e.settleKill(rep)
	if rep.Outcome == OutcomeInfra {
		// One outage is one outage. Stop the siblings rather than let them burn
		// their budgets against the same wall.
		p.cancel()
	}
	p.finish(i, iss, rep, err)
}

// finish records one issue's result and frees its slot.
//
// Whatever happened above, the issue ends on the stream: an issue that stops
// without an end event leaves a watcher showing it as still queued, which is the
// one state it is certainly not in.
func (p *pool) finish(i int, iss wave.Issue, rep Report, err error) {
	p.mu.Lock()
	p.reports[i], p.errs[i] = rep, err
	p.live--
	p.mu.Unlock()

	p.e.Bus.Emit(Event{
		Kind: EventIssueEnd, Wave: p.no, Issue: iss.ID, Outcome: rep.Outcome,
		Text: rep.Reason, Usage: rep.Usage, Report: &rep,
		// The attempt it ended on, and no round: nothing is running to count,
		// but which attempt an issue died on is part of its verdict. Zero for
		// an issue the wave never dispatched, which had no attempt at all.
		Attempt: rep.LastAttempt(),
	})
}

// land hands one finished issue to the scheduler, and then asks again what may
// start: an integration is the one thing that can make a dependent of it
// dispatchable, and nothing else is going to ask on its behalf.
func (p *pool) land(i int, iss wave.Issue) {
	if p.landed == nil {
		return
	}
	p.mu.Lock()
	rep := p.reports[i]
	p.mu.Unlock()
	p.landed(iss, rep)
	p.topUp()
}

// topUp refills the slots this pool has free.
//
// A wave used to be planned at the concurrency cap and never grow, so an issue
// that parked in its first minute left a worker's worth of capacity idle until
// the barrier — with the rest of the scope on screen saying "waiting". What may
// go into the slot is the same question the planner already answers, so refill
// asks it again rather than answering it a second way.
//
// It is called only by a worker that has just freed a slot or landed an issue,
// which is what keeps it from becoming a poll. No slot frees, no question asked.
func (p *pool) topUp() {
	p.top.Lock()
	defer p.top.Unlock()

	if p.refill == nil || p.stopped() {
		return
	}
	free := p.free()
	if free <= 0 {
		return
	}
	for _, iss := range p.refill(free) {
		p.dispatch(iss)
	}
}

// stopped reports whether the pool is over rather than merely between workers.
//
// Nothing is topped up after an outage, a stop or an interrupt. An OutcomeInfra
// report cancels the pool precisely so five workers do not walk into the same
// wall, and refilling the slot it just freed would undo that. The reports are
// checked as well as the context because the worker that cancels and the worker
// that frees a slot can be two different goroutines finishing at once.
func (p *pool) stopped() bool {
	if p.ctx.Err() != nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stop, _ := waveStopped(p.reports)
	return stop != ""
}

// free is how many more workers this pool may have in flight.
func (p *pool) free() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conc - p.live
}

// results is every report this pool has, and the first error any worker
// returned. Safe to call while workers are still running.
func (p *pool) results() ([]Report, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]Report(nil), p.reports...)
	for _, err := range p.errs {
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
