package drain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
	"bd-auto/internal/wave"
)

// --- harness ---

// laneWatch is what a continuous run looks like from the bus: how many workers
// were in flight at each moment the integrator was doing something, and the
// order the whole thing happened in.
type laneWatch struct {
	mu sync.Mutex
	// live is the workers dispatched and not yet ended.
	live int
	// duringGate is the live count at each barrier gate. That gate is the
	// longest thing an integration does, and the count during it is the whole
	// of "the merge does not cost the run a worker".
	duringGate []int
	// order is every event kind that matters here, in the order it arrived,
	// tagged with the issue where there is one.
	order []string
	// gateStarted is closed the first time the integrator gates, so a worker
	// can be held live until then.
	gateStarted chan struct{}
	gateOnce    sync.Once
	// lanes counts the integrations that reported themselves as a lane rather
	// than as a barrier between waves.
	lanes int
}

func newLaneWatch() *laneWatch { return &laneWatch{gateStarted: make(chan struct{})} }

func (l *laneWatch) Observe(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch e.Kind {
	case EventIssueStart:
		l.live++
		l.order = append(l.order, "start "+e.Issue)
	case EventIssueEnd:
		l.live--
		l.order = append(l.order, "end "+e.Issue)
	case EventMergeEnd:
		l.order = append(l.order, "merged "+e.Issue)
	case EventWaveGateStart:
		l.duringGate = append(l.duringGate, l.live)
		l.gateOnce.Do(func() { close(l.gateStarted) })
	case EventWaveEnd:
		if e.Lane {
			l.lanes++
		}
	}
}

func (l *laneWatch) peakDuringGate() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	max := 0
	for _, n := range l.duringGate {
		if n > max {
			max = n
		}
	}
	return max
}

func (l *laneWatch) at(entry string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, s := range l.order {
		if s == entry {
			return i
		}
	}
	return -1
}

func (l *laneWatch) laneCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lanes
}

// slowBarrierGate makes the gate on the merged result take a moment while the
// same gate stays instant inside a worker's worktree.
//
// The two share one configured gate, and this is the one thing that tells them
// apart from inside a shell: run.json is in the main checkout's .beads/auto and
// a worktree is several directories below it, so the path only resolves where
// the integrator runs.
func slowBarrierGate(cfg *config.Config) *config.Config {
	return withGate(cfg, "barrier", "sh -c 'test -f .beads/auto/run.json && sleep 0.3; true'")
}

// --- tests ---

// The whole point of the change, in one run: t-2 depends on t-1, and it starts
// as soon as t-1's branch is merged rather than waiting for everything else in
// flight to finish first.
//
// t-3 is a long chain behind t-2 and t-4 is independent, so the run has to keep
// finding work in three different states at once. Under waves this graph is
// three barriers; here it is one continuous run, and t-2's worker proves it was
// built on the merged result rather than beside it.
func TestAContinuousRunStartsADependentAsSoonAsItsBlockerIsMerged(t *testing.T) {
	repo := testRepo(t)
	ids := []string{"t-1", "t-2", "t-3", "t-4"}
	iss := newIssues(ids...).under("epic-1", ids...).
		dependsOn("t-2", "t-1").
		dependsOn("t-3", "t-2")

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "t-1.txt"))
	workers.script("t-4", closeAndCommit(iss, "t-4", "t-4.txt"))
	// Each dependent asserts, from inside its own worktree, that everything it
	// depends on is already there. A worker branches from the main checkout's
	// HEAD, so this is only true if the dependency was merged before this
	// worker was dispatched.
	for _, tc := range []struct{ id, needs string }{{"t-2", "t-1.txt"}, {"t-3", "t-2.txt"}} {
		tc := tc
		workers.script(tc.id, fake.Step{Text: "done", Do: steps(
			func(_ context.Context, req runner.Request) error {
				if !exists(filepath.Join(req.Dir, tc.needs)) {
					return fmt.Errorf("%s started before %s was merged into the checkout it branched from",
						tc.id, tc.needs)
				}
				return nil
			},
			commitWork(tc.id+".txt"), closes(iss, tc.id))})
	}

	watch := newLaneWatch()
	e := engine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	e.Bus = NewBus(watch)

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: ids, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("run outcome %s (%s)", rep.Outcome, rep.Reason)
	}
	if !rep.Continuous {
		t.Fatal("autonomy: auto must report a continuous run")
	}
	for _, id := range ids {
		if !has(rep.Done, id) {
			t.Fatalf("%s is not done: done=%v parked=%v", id, rep.Done, rep.Parked)
		}
		if !exists(filepath.Join(repo, id+".txt")) {
			t.Fatalf("%s's work did not reach the main checkout", id)
		}
	}

	// Each dependent started after its blocker's branch was merged, and there
	// was no barrier in between: the merge is what released it.
	for _, tc := range []struct{ dep, on string }{{"t-2", "t-1"}, {"t-3", "t-2"}} {
		merged, start := watch.at("merged "+tc.on), watch.at("start "+tc.dep)
		if merged < 0 || start < 0 {
			t.Fatalf("no merge for %s (%d) or no start for %s (%d)", tc.on, merged, tc.dep, start)
		}
		if start < merged {
			t.Fatalf("%s started before %s was merged", tc.dep, tc.on)
		}
	}
	// One integration per issue that landed, plus the settling barrier at the
	// end. Four separate merges, not one barrier that waited for all of them.
	if got := len(rep.Integrations); got != len(ids)+1 {
		t.Fatalf("%d integration(s) for %d issues; a continuous run integrates each one as it "+
			"lands and settles once at the end", got, len(ids))
	}
	if got := watch.laneCount(); got != len(ids) {
		t.Fatalf("%d integration(s) said they were a lane, want %d; a watcher told these were "+
			"barriers would draw a stop between workers that never stopped", got, len(ids))
	}
}

// The cap is the cap, including while an issue is being merged and gated.
//
// A merge takes minutes when a model has to resolve it, and a wave run spent
// every one of them with nothing running at all. Here the worker that landed
// gives its slot back first and integrates afterwards, so the slot it freed is
// already filled by the time the gate runs — which is what this measures.
func TestAContinuousRunKeepsTheCapFullWhileAnIssueIsMerging(t *testing.T) {
	repo := testRepo(t)
	ids := []string{"t-1", "t-2", "t-3"}
	iss := newIssues(ids...).under("epic-1", ids...)

	watch := newLaneWatch()
	workers := newByIssue()
	// t-1 lands at once, which is what starts an integration.
	workers.script("t-1", closeAndCommit(iss, "t-1", "t-1.txt"))
	// The other two are held live until the integrator has started gating, so
	// what the count during that gate measures is the scheduler rather than a
	// race between two goroutines.
	for _, id := range ids[1:] {
		id := id
		workers.script(id, fake.Step{Text: "done", Do: steps(
			func(ctx context.Context, _ runner.Request) error {
				select {
				case <-watch.gateStarted:
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
					return fmt.Errorf("%s waited for an integration that never started", id)
				}
				return nil
			},
			commitWork(id+".txt"), closes(iss, id))})
	}

	e := engine(t, repo, slowBarrierGate(testCfg(1, 0)), iss, workers, fake.New())
	e.Bus = NewBus(watch)

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: ids, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone || len(rep.Done) != len(ids) {
		t.Fatalf("the run did not finish: outcome=%s done=%v parked=%v", rep.Outcome, rep.Done, rep.Parked)
	}
	if got := watch.peakDuringGate(); got != 2 {
		t.Fatalf("%d worker(s) were in flight while the integrator gated; the cap is 2 and a "+
			"merge must not cost the run one of them", got)
	}
}

// An outage in the middle of a continuous run stops it dispatching, and does it
// without taking back what already landed.
//
// The circuit breaker used to be a property of a wave: one infra-failed report
// cancelled its siblings so five workers did not walk into one rate limit. A run
// with no waves needs the same thing at the run level, and it has one more thing
// to get right than a wave did — issues have already merged by then, and an
// outage is not a reason to undo work that landed green.
func TestAnOutageMidWayThroughAContinuousRunStopsItAndKeepsWhatLanded(t *testing.T) {
	repo := testRepo(t)
	ids := []string{"t-1", "t-2", "t-3"}
	iss := newIssues(ids...).under("epic-1", ids...)

	// One at a time, so the order is the run's rather than the scheduler's:
	// t-1 lands and merges, t-2 then meets the wall, and t-3 is what the run
	// must not start afterwards.
	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "t-1.txt"))
	workers.script("t-2", fake.Step{Class: runner.ClassInfraFailed})
	third := workers.script("t-3", closeAndCommit(iss, "t-3", "t-3.txt"))

	e := engine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	e.Bus = NewBus(collector())
	e.InfraRetries = 1 // give up on the environment immediately

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: ids, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeInfra {
		t.Fatalf("run outcome %s (%s), want infra-failed", rep.Outcome, rep.Reason)
	}
	if len(rep.Parked) != 0 {
		t.Fatalf("an outage is not a verdict, so nothing may be parked: %v", rep.Parked)
	}
	if third.Calls() != 0 {
		t.Fatal("t-3 was dispatched after the outage; the breaker must stop the run dispatching")
	}
	// What landed before the wall stays landed. It was merged and gated on its
	// own, and an unrelated outage is not a reason to throw it away.
	if !exists(filepath.Join(repo, "t-1.txt")) {
		t.Fatal("the issue that landed and merged before the outage was taken back out")
	}
	if !has(rep.Landed(), "t-1") {
		t.Fatalf("the report does not say t-1 landed: %v", rep.Landed())
	}

	// Still armed: the run is not over, it is waiting to be re-run.
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != runstate.StatusActive {
		t.Fatalf("run status %q; a run stopped on the environment stays active so a re-run picks it up", st.Status)
	}
}

// A killed run resumes: the issue it landed is not run again, and the issue it
// left in flight is picked up rather than dispatched a second time.
//
// The state the first run leaves behind is the interesting part. It has an issue
// in Done whose branch was merged, and an issue in InFlight whose branch was
// not — and a continuous run reads WaveIssues to decide what a dependent may be
// built on, so a resumed run that mishandled either would either re-run finished
// work or hold every dependent back for good.
func TestAContinuousRunResumesWithoutRerunningWhatLanded(t *testing.T) {
	repo := testRepo(t)
	ids := []string{"t-1", "t-2"}
	iss := newIssues(ids...).under("epic-1", ids...)

	// First run: t-1 lands, t-2 is interrupted the moment t-1's merge is done.
	first := newByIssue()
	first.script("t-1", closeAndCommit(iss, "t-1", "t-1.txt"))

	e1 := engine(t, repo, testCfg(1, 0), iss, first, fake.New())
	e1.Control = NewControl()
	// t-2 is still working when the run is stopped. The delay is never waited
	// out: the stop cancels it, which is what makes the outcome an interrupt
	// rather than a verdict, and an interrupt is what leaves it in flight.
	first.script("t-2", fake.Step{Delay: time.Minute})
	var stop sync.Once
	e1.Bus = NewBus(ObserverFunc(func(ev Event) {
		if ev.Kind == EventMergeEnd && ev.Issue == "t-1" {
			stop.Do(e1.Control.Stop)
		}
	}))

	rep1, err := e1.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: ids, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if rep1.Outcome == OutcomeDone {
		t.Fatalf("the first run was supposed to be stopped part-way: %+v", rep1)
	}
	if !has(rep1.Landed(), "t-1") {
		t.Fatalf("the first run did not land t-1, so there is nothing to resume around: %v", rep1.Landed())
	}

	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.InFlight["t-2"]; !ok {
		t.Fatal("t-2 must be left in flight; that is what the re-run resumes")
	}
	if wave.HasIssue(st.WaveIssues, "t-1") {
		t.Fatalf("t-1 merged, so it must be off the list of work waiting to land: %v", st.WaveIssues)
	}

	// Second run, same repo and same state.
	second := newByIssue()
	again := second.script("t-1", closeAndCommit(iss, "t-1", "t-1-again.txt"))
	resumed := second.script("t-2", closeAndCommit(iss, "t-2", "t-2.txt"))

	e2 := engine(t, repo, testCfg(1, 0), iss, second, fake.New())
	e2.Bus = NewBus(collector())

	rep2, err := e2.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: ids, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if rep2.Outcome != OutcomeDone {
		t.Fatalf("the resumed run did not finish: %s (%s)", rep2.Outcome, rep2.Reason)
	}
	if again.Calls() != 0 {
		t.Fatal("the resumed run re-ran an issue that had already landed")
	}
	if resumed.Calls() != 1 {
		t.Fatalf("the interrupted issue got %d worker(s), want exactly 1", resumed.Calls())
	}
	for _, id := range ids {
		if !has(rep2.Done, id) {
			t.Fatalf("%s is not done after the resume: done=%v parked=%v", id, rep2.Done, rep2.Parked)
		}
	}
	if exists(filepath.Join(repo, "t-1-again.txt")) {
		t.Fatal("the resumed run merged a second attempt at an issue that had already landed")
	}
}

// A run that stops making progress ends, and says why.
//
// DefaultMaxWaves bounded the wave loop for exactly this: not to bound the
// spend, which the scope does, but to stop a loop that is going round without
// getting anywhere. A run with no waves needs the same backstop counted on
// something else, and workers dispatched is the thing that moves.
func TestAContinuousRunThatKeepsDispatchingStopsAndSaysSo(t *testing.T) {
	repo := testRepo(t)
	ids := []string{"t-1", "t-2", "t-3", "t-4"}
	iss := newIssues(ids...).under("epic-1", ids...)

	// Every worker does nothing, so every issue parks — and bd keeps offering
	// the next one. MaxWaves 1 at concurrency 2 allows two workers and no more.
	workers := newByIssue()
	for _, id := range ids {
		workers.script(id, fake.Step{Text: "nothing"})
	}

	watch := newLaneWatch()
	e := engine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	e.Bus = NewBus(watch)

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: ids, Concurrency: 2, MaxWaves: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeFailed {
		t.Fatalf("run outcome %s, want failed once the guard bites", rep.Outcome)
	}
	if rep.Reason == "" {
		t.Fatal("a run stopped by the runaway guard must say so")
	}
	started := 0
	for _, id := range ids {
		if watch.at("start "+id) >= 0 {
			started++
		}
	}
	if started != 2 {
		t.Fatalf("%d worker(s) were dispatched; the guard allows 2", started)
	}
}

// autonomy: wave still gathers, integrates and pauses. Continuous scheduling is
// the auto path only, and the mode that exists to put a human between waves has
// to still have a between.
func TestWaveAutonomyStillIntegratesAtABarrier(t *testing.T) {
	repo := testRepo(t)
	ids := []string{"t-1", "t-2", "t-3"}
	iss := newIssues(ids...).under("epic-1", ids...).dependsOn("t-3", "t-1")

	workers := newByIssue()
	for _, id := range ids {
		workers.script(id, closeAndCommit(iss, id, id+".txt"))
	}

	watch := newLaneWatch()
	e := engine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	e.Bus = NewBus(watch)
	resumesAtEveryBarrier(t, e, repo)

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: ids, Concurrency: 2, Autonomy: config.AutonomyWave,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone || len(rep.Done) != len(ids) {
		t.Fatalf("the run did not finish: outcome=%s done=%v parked=%v", rep.Outcome, rep.Done, rep.Parked)
	}
	if rep.Continuous {
		t.Fatal("autonomy: wave must not schedule continuously")
	}
	if rep.Waves != 2 {
		t.Fatalf("ran %d wave(s); t-3 depends on t-1, so under waves it takes two", rep.Waves)
	}
	if got := watch.laneCount(); got != 0 {
		t.Fatalf("%d integration(s) reported themselves as a lane; a wave run integrates at a "+
			"barrier and a watcher must be told so", got)
	}
	// One barrier per wave, each merging what that wave produced.
	if got := len(rep.Integrations); got != 2 {
		t.Fatalf("%d integration(s), want one per wave", got)
	}
}

// `bd-auto run pause` still has somewhere to stop.
//
// It used to hold the run at the next wave boundary, and a continuous run has
// none. The boundary it stops at instead is the quiet point the run reaches on
// its own: dispatch refuses a paused run, so whatever is in flight lands and is
// integrated, and then nothing more starts until `resume`.
func TestPausingAContinuousRunStopsItDispatchingAndResumes(t *testing.T) {
	repo := testRepo(t)
	ids := []string{"t-1", "t-2", "t-3"}
	iss := newIssues(ids...).under("epic-1", ids...).
		dependsOn("t-2", "t-1").
		dependsOn("t-3", "t-1")

	workers := newByIssue()
	for _, id := range ids {
		workers.script(id, closeAndCommit(iss, id, id+".txt"))
	}

	e := engine(t, repo, testCfg(1, 0), iss, workers, fake.New())

	// The human, on both sides. One pauses the run the moment its first issue
	// has been integrated; the other releases it the first time the run polls,
	// and records that it really was paused when it did.
	var once sync.Once
	e.Bus = NewBus(ObserverFunc(func(ev Event) {
		if ev.Kind != EventWaveEnd {
			return
		}
		once.Do(func() {
			_, err := runstate.Update(repo, false, func(s *runstate.State) error {
				s.Status = runstate.StatusPaused
				return nil
			})
			if err != nil {
				t.Errorf("could not pause the run: %v", err)
			}
		})
	}))

	var pausedWhenPolled bool
	var dispatchedWhilePaused []string
	var mu sync.Mutex
	e.Sleep = func(context.Context, time.Duration) error {
		st, err := runstate.Load(repo)
		if err != nil {
			return err
		}
		mu.Lock()
		if st.Status == runstate.StatusPaused {
			pausedWhenPolled = true
			// Nothing may have been started while it was held: t-2 and t-3 are
			// ready by now, and the pause is the only thing stopping them.
			for id := range st.InFlight {
				dispatchedWhilePaused = append(dispatchedWhilePaused, id)
			}
		}
		mu.Unlock()
		_, err = runstate.Update(repo, false, func(s *runstate.State) error {
			s.Status = runstate.StatusActive
			return nil
		})
		return err
	}

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: ids, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !pausedWhenPolled {
		t.Fatal("a paused continuous run must wait, so that `bd-auto run resume` has something to release")
	}
	mu.Lock()
	held := append([]string(nil), dispatchedWhilePaused...)
	mu.Unlock()
	if len(held) != 0 {
		t.Fatalf("%v were dispatched while the run was paused; a pause stops dispatch", held)
	}
	if rep.Outcome != OutcomeDone || len(rep.Done) != len(ids) {
		t.Fatalf("the resumed run did not finish: outcome=%s done=%v parked=%v",
			rep.Outcome, rep.Done, rep.Parked)
	}
}

// The runaway guard bites on a run that is not making progress, and only on
// one. A run that spends its budget exactly and then drains is a finished run,
// and reporting it as stopped would turn every full-budget run into a failure.
func TestAContinuousRunThatSpendsItsGuardExactlyStillFinishes(t *testing.T) {
	repo := testRepo(t)
	ids := []string{"t-1", "t-2"}
	iss := newIssues(ids...).under("epic-1", ids...)

	workers := newByIssue()
	for _, id := range ids {
		workers.script(id, closeAndCommit(iss, id, id+".txt"))
	}

	e := engine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	e.Bus = NewBus(collector())

	// Exactly two workers allowed, and exactly two issues to run.
	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: ids, Concurrency: 2, MaxWaves: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("run outcome %s (%s); the scope drained inside the guard", rep.Outcome, rep.Reason)
	}
	if len(rep.Done) != len(ids) {
		t.Fatalf("done=%v parked=%v, want both issues done", rep.Done, rep.Parked)
	}
}

// A worker's commit takes the MAIN checkout's index lock, because beads' hook
// re-exports its database there, and a continuous run merges in that same
// checkout while workers are committing into it.
//
// Under waves the two could never meet: the barrier ran when no worker was
// live. Here they can, git does not retry a lock it could not take, and an
// unretried merge would park finished, gated, reviewed work with "git would not
// merge" — the worst possible reading of a hundred milliseconds of contention.
func TestAMergeWaitsOutAWorkerHoldingTheCheckoutsIndexLock(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "t-1.txt"))

	// A worker's hook, in the state it holds the lock in: taken just as the
	// merge starts, and given back a moment later.
	held := make(chan struct{})
	lock := filepath.Join(repo, ".git", "index.lock")
	e := engine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	e.Bus = NewBus(ObserverFunc(func(ev Event) {
		if ev.Kind != EventMergeStart {
			return
		}
		if err := os.WriteFile(lock, nil, 0o644); err != nil {
			t.Errorf("could not stand in for a worker's hook: %v", err)
			return
		}
		go func() {
			time.Sleep(150 * time.Millisecond)
			_ = os.Remove(lock)
			close(held)
		}()
	}))

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the stand-in hook never released the lock")
	}
	if !has(rep.Done, "t-1") {
		t.Fatalf("finished work was not merged because a worker held the index for a moment: "+
			"done=%v parked=%v", rep.Done, rep.Parked)
	}
	if !exists(filepath.Join(repo, "t-1.txt")) {
		t.Fatal("the branch did not reach the main checkout")
	}
}

// Only lock contention is waited out. A merge git refused for a real reason is
// a real answer, and retrying it for a second would turn every genuine refusal
// into a delay.
func TestOnlyALockedIndexIsWaitedOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"an index another git process holds", errors.New(
			"git: fatal: Unable to create '/repo/.git/index.lock': File exists."), true},
		{"a ref another git process holds", errors.New(
			"git: fatal: cannot lock ref 'refs/heads/x': Unable to create '/repo/.git/refs/heads/x.lock': File exists"), true},
		{"a branch that is not there", errors.New(
			"git: merge: bd-auto/t-1 - not something we can merge"), false},
		{"a tree git will not overwrite", errors.New(
			"git: error: Your local changes to the following files would be overwritten by merge"), false},
		{"nothing at all", nil, false},
	} {
		if got := gitLocked(tc.err); got != tc.want {
			t.Errorf("%s: gitLocked = %v, want %v", tc.name, got, tc.want)
		}
	}
}
