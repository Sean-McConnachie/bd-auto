package drain

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
	"bd-auto/internal/worktree"
)

// --- harness ---

// byIssue routes each request to that issue's own script.
//
// A wave runs several workers at once, so a single scripted runner would replay
// its steps in whatever order the goroutines reached it and the assertions would
// mean nothing. The worktree path names the issue, which is what makes the
// routing exact rather than a guess at the prompt's contents.
type byIssue struct {
	mu       sync.Mutex
	runners  map[string]*fake.Runner
	fallback *fake.Runner
}

func newByIssue() *byIssue {
	return &byIssue{runners: map[string]*fake.Runner{}, fallback: fake.New()}
}

// script fixes what one issue's worker does, in order.
func (b *byIssue) script(issue string, steps ...fake.Step) *fake.Runner {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := fake.New(steps...)
	b.runners[issue] = r
	return r
}

func (b *byIssue) pick(req runner.Request) *fake.Runner {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.runners[filepath.Base(req.Dir)]; ok {
		return r
	}
	return b.fallback
}

func (b *byIssue) Name() string               { return "by-issue" }
func (b *byIssue) Caps() runner.Capabilities  { return fake.DefaultCaps() }
func (b *byIssue) Calls() int                 { return b.fallback.Calls() }
func (b *byIssue) Requests() []runner.Request { return b.fallback.Requests() }
func (b *byIssue) Run(ctx context.Context, req runner.Request, sink runner.EventSink) (runner.Result, error) {
	return b.pick(req).Run(ctx, req, sink)
}

// drainEngine wires an engine for a whole run: a bus nobody reads, no waiting,
// and a per-issue worker script.
func drainEngine(t *testing.T, repo string, cfg *config.Config, iss Issues, worker runner.Runner, reviewer runner.Runner) *Engine {
	t.Helper()
	e := engine(t, repo, cfg, iss, worker, reviewer)
	e.Bus = NewBus(collector())
	return e
}

// collector is an observer that only has to exist: the bus has to be exercised
// under concurrency, and dropping the events is fine.
func collector() Observer { return ObserverFunc(func(Event) {}) }

// closeAndCommit is the whole of a successful worker: a commit through the
// worktree's own hooks, and the issue closed in bd.
func closeAndCommit(iss *fakeIssues, id, file string) fake.Step {
	return fake.Step{Text: "done", Do: steps(commitWork(file), closes(iss, id))}
}

func outcomeOf(t *testing.T, rep DrainReport, issue string) Report {
	t.Helper()
	for _, r := range rep.Issues {
		if r.Issue == issue {
			return r
		}
	}
	t.Fatalf("no report for %s in %+v", issue, rep.Issues)
	return Report{}
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// --- tests ---

// The whole engine over a four-issue DAG: two waves, a clean pass, a parked
// issue, and a real barrier between them.
//
// t-2 depends on t-1, so it cannot be in the first wave; t-4's worker does
// nothing at all, which is the failure the progress check exists to catch. The
// epic must stay open afterwards, because a parked child is required work that
// did not get done.
// It is driven through `provider: fake` in the config rather than an injected
// runner, so the resolution path a real repo takes — runners: block, registry,
// role specs — is exercised too.
func TestDrainRunsADagAcrossWavesAndParksWhatFailed(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2", "t-3", "t-4").
		under("epic-1", "t-1", "t-2", "t-3", "t-4").
		dependsOn("t-2", "t-1")

	cfg := withGate(testCfg(1, 0), "build", "true")
	cfg.Concurrency = 4
	cfg.Runners = map[string]config.RunnerSpec{
		config.RoleDefault: {Provider: fake.Provider},
	}

	// One step, replayed for every call, branching on the worktree it was given.
	// Order-independent by construction, which is what makes a four-worker wave
	// deterministic through a single shared runner.
	model := fake.New(fake.Step{Text: "done", Do: func(ctx context.Context, req runner.Request) error {
		switch id := filepath.Base(req.Dir); id {
		case "t-1", "t-2", "t-3":
			return steps(commitWork(id+".txt"), closes(iss, id))(ctx, req)
		default:
			// t-4: a round that changes nothing and closes nothing. One attempt,
			// no retries, so it parks.
			return nil
		}
	}})
	defer fake.Install(model)()

	e := &Engine{
		RepoRoot: repo, Cfg: cfg, BD: iss, Bus: NewBus(collector()),
		Prompt:  func(r runner.Role) (string, error) { return "system prompt for " + string(r), nil },
		Sleep:   func(context.Context, time.Duration) error { return nil },
		Backoff: func(int) time.Duration { return 0 },
	}

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic:        "epic-1",
		Scope:       []string{"t-1", "t-2", "t-3", "t-4"},
		Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if rep.Outcome != OutcomeDone {
		t.Fatalf("run outcome %s (%s)", rep.Outcome, rep.Reason)
	}
	if rep.Waves != 2 {
		t.Fatalf("ran %d wave(s); t-2 depends on t-1, so it takes two", rep.Waves)
	}
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		if !has(rep.Done, id) {
			t.Fatalf("%s is not done: done=%v parked=%v", id, rep.Done, rep.Parked)
		}
	}
	if !has(rep.Parked, "t-4") {
		t.Fatalf("t-4 did nothing and must be parked: parked=%v", rep.Parked)
	}
	for _, req := range model.Requests() {
		if req.Role == runner.RoleIntegrator {
			t.Fatal("nothing conflicted, so the barrier must spawn no integrator")
		}
	}

	// Everything that landed is merged into the main checkout, and only that.
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		if !exists(filepath.Join(repo, id+".txt")) {
			t.Fatalf("%s's work did not reach the main checkout", id)
		}
	}
	if exists(filepath.Join(repo, "t-4.txt")) {
		t.Fatal("a parked issue's branch must not be merged")
	}
	if rep.EpicClosed {
		t.Fatal("the epic must stay open while a child is parked")
	}
	if !strings.Contains(rep.EpicReason, "parked") {
		t.Fatalf("epic reason %q does not name the parked work", rep.EpicReason)
	}
}

// bd offers the issue, the run has never touched it, and it is still never
// dispatched. This is the assertion the whole scope design rests on.
func TestDrainNeverTouchesAnIssueOutsideTheScope(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	cfg := testCfg(1, 0)
	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))
	out := workers.script("t-2", closeAndCommit(iss, "t-2", "two.txt"))

	e := drainEngine(t, repo, cfg, iss, workers, fake.New())
	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if out.Calls() != 0 {
		t.Fatalf("t-2 was out of scope and must never be dispatched; its worker ran %d time(s)", out.Calls())
	}
	if exists(worktree.Path(repo, "t-2")) {
		t.Fatal("an out-of-scope issue must not get a worktree")
	}
	if branchExists(repo, cfg.Branch("t-2")) {
		t.Fatal("an out-of-scope issue must not get a branch")
	}
	if has(rep.Done, "t-2") || has(rep.Parked, "t-2") {
		t.Fatalf("t-2 must have no verdict at all: done=%v parked=%v", rep.Done, rep.Parked)
	}
	if !has(rep.Done, "t-1") {
		t.Fatalf("the scoped issue did not finish: %+v", rep)
	}

	// A run whose scope was a subset finishes with children still open, and must
	// leave the epic alone — as a scope fact, not a failure.
	if rep.EpicClosed {
		t.Fatal("a partial scope must not close the epic")
	}
	if !strings.Contains(rep.EpicReason, "scope") {
		t.Fatalf("epic reason %q does not say the scope was partial", rep.EpicReason)
	}
}

// An issue whose blocker was never in the scope can never become ready, so bd
// would keep it out of every wave and the run would end with nothing recorded
// against it. Parking it before dispatch is what turns that silence into a
// reason a human can act on.
func TestAnUnmetOutOfScopeDependencyParksImmediately(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2").dependsOn("t-2", "t-1")

	workers := newByIssue()
	blocked := workers.script("t-2", closeAndCommit(iss, "t-2", "two.txt"))

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-2"}, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if blocked.Calls() != 0 {
		t.Fatalf("nothing may be spawned for an issue that cannot become ready; it ran %d time(s)", blocked.Calls())
	}
	if !has(rep.Parked, "t-2") {
		t.Fatalf("t-2 must be parked: %+v", rep)
	}
	got := outcomeOf(t, rep, "t-2")
	if got.Stage != StageScope || !strings.Contains(got.Reason, "t-1") {
		t.Fatalf("the park must name the dependency and the stage: %+v", got)
	}

	notes, parked, _ := iss.snapshot()
	if len(parked) != 1 || parked[0] != "t-2" {
		t.Fatalf("bd was not told: parked=%v", parked)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "out of scope") {
		t.Fatalf("the issue's notes must carry the reason: %v", notes)
	}
}

// Interrupt recovery, end to end and in one test because the two halves are one
// mechanism: the killed run's session is resumed, and when that resumed first
// turn comes back infra-failed — which is exactly what a transcript ending in an
// unanswered tool_use does — it falls back to a fresh dispatch without spending
// an attempt.
func TestAnInterruptedRunResumesItsSessionAndFallsBackFresh(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")
	cfg := testCfg(3, 1) // three rounds, one retry: an attempt to lose if this is wrong

	workers := newByIssue()
	ctx, cancel := context.WithCancel(context.Background())
	killed := workers.script("t-1", fake.Step{
		// Killed mid-turn: the session is already recorded, the worktree exists,
		// and nothing has been judged.
		Do: func(context.Context, runner.Request) error { cancel(); return nil },
	})

	e := drainEngine(t, repo, cfg, iss, workers, pass())
	opts := DrainOptions{Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1}

	first, err := e.Drain(ctx, opts)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if first.Outcome != OutcomeInterrupted {
		t.Fatalf("first run outcome %s (%s), want interrupted", first.Outcome, first.Reason)
	}
	if len(killed.Requests()) != 1 {
		t.Fatalf("the killed run made %d call(s), want 1", len(killed.Requests()))
	}
	session := killed.Requests()[0].SessionID
	if session == "" {
		t.Fatal("no session was recorded for the interrupted turn")
	}
	if !exists(worktree.Path(repo, "t-1")) {
		t.Fatal("an interrupt must leave the worktree in place; there is nothing to resume without it")
	}

	// The re-run. Its first turn resumes and comes back on the environment; its
	// second must be a fresh session, and the whole thing must still be attempt
	// one.
	killed.Reset()
	killed.Script(
		fake.Step{Class: runner.ClassInfraFailed},
		closeAndCommit(iss, "t-1", "one.txt"),
	)

	second, err := e.Drain(context.Background(), opts)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	reqs := killed.Requests()
	if len(reqs) != 2 {
		t.Fatalf("the re-run made %d call(s), want a resumed one and a fresh one", len(reqs))
	}
	if !reqs[0].Resume || reqs[0].SessionID != session {
		t.Fatalf("the first turn of the re-run must resume %s; got resume=%v session=%s",
			session, reqs[0].Resume, reqs[0].SessionID)
	}
	if reqs[1].Resume || reqs[1].SessionID == session {
		t.Fatalf("after an infra failure the next turn must be fresh: resume=%v session=%s",
			reqs[1].Resume, reqs[1].SessionID)
	}
	if second.Outcome != OutcomeDone || !has(second.Done, "t-1") {
		t.Fatalf("the re-run did not finish the issue: %+v", second)
	}
	got := outcomeOf(t, second, "t-1")
	if len(got.Attempts) != 1 || got.Attempts[0].Attempt != 1 {
		t.Fatalf("neither the interrupt nor the infra failure may consume an attempt: %+v", got.Attempts)
	}
	if got.Attempts[0].InfraRetries != 1 {
		t.Fatalf("the absorbed process must be reported as an infra retry: %+v", got.Attempts[0])
	}
}

// One outage is one outage. A rate limit that stops one worker must stop the
// wave, not be answered by four siblings burning their budgets against the same
// wall and parking perfectly good issues.
func TestAnOutageStopsTheWaveWithoutParkingAnything(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	workers := newByIssue()
	workers.script("t-1", fake.Step{Class: runner.ClassInfraFailed})
	workers.script("t-2", fake.Step{Delay: 2 * time.Second, Do: commitWork("two.txt")})

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	e.InfraRetries = 1 // give up on the environment immediately

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"}, Concurrency: 2,
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
	if len(rep.Integrations) != 0 {
		t.Fatal("a wave that stopped on the environment has nothing to integrate")
	}

	// Still armed: the run is not over, it is waiting to be re-run.
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != runstate.StatusActive {
		t.Fatalf("run status %q; an interrupted run stays active so a re-run picks it up", st.Status)
	}
	if _, ok := st.InFlight["t-1"]; !ok {
		t.Fatal("the interrupted issue must stay in flight; that is what the re-run resumes")
	}
}

// autonomy: wave stops at the barrier and waits. The pause has to be visible in
// run state, because `bd-auto run resume` is a different process.
func TestWaveAutonomyPausesAtTheBarrier(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2").dependsOn("t-2", "t-1")

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))
	workers.script("t-2", closeAndCommit(iss, "t-2", "two.txt"))

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())

	// Stand in for the human: the first time the run waits, resume it, and
	// record that it really was paused when we did.
	var pausedWhenPolled bool
	e.Sleep = func(context.Context, time.Duration) error {
		st, err := runstate.Load(repo)
		if err != nil {
			return err
		}
		pausedWhenPolled = st.Status == runstate.StatusPaused
		_, err = runstate.Update(repo, false, func(s *runstate.State) error {
			s.Status = runstate.StatusActive
			return nil
		})
		return err
	}

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"},
		Concurrency: 2, Autonomy: config.AutonomyWave,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !pausedWhenPolled {
		t.Fatal("autonomy: wave must record the pause in run state, so another process can end it")
	}
	if rep.Outcome != OutcomeDone || len(rep.Done) != 2 {
		t.Fatalf("the resumed run did not finish: %+v", rep)
	}
	if rep.Waves != 2 {
		t.Fatalf("ran %d wave(s), want 2", rep.Waves)
	}
}

// The wave a row shows and the wave the barrier reports have to be the same
// wave. wave.Record advances the counter on disk and hands back the updated
// state; a caller that drops it numbers everything it emits one wave behind,
// and the first wave — the one that matters most, because it is the one you are
// watching when you decide whether to trust the run — comes out as zero.
func TestEveryEventCarriesTheWaveItActuallyBelongsTo(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").
		under("epic-1", "t-1", "t-2").
		dependsOn("t-2", "t-1")

	cfg := withGate(testCfg(1, 0), "build", "true")
	cfg.Runners = map[string]config.RunnerSpec{config.RoleDefault: {Provider: fake.Provider}}

	model := fake.New(fake.Step{Text: "done", Do: func(ctx context.Context, req runner.Request) error {
		id := filepath.Base(req.Dir)
		return steps(commitWork(id+".txt"), closes(iss, id))(ctx, req)
	}})
	defer fake.Install(model)()

	waves := map[string][]int{}
	var barriers []int
	e := &Engine{
		RepoRoot: repo, Cfg: cfg, BD: iss,
		Bus: NewBus(ObserverFunc(func(ev Event) {
			switch ev.Kind {
			case EventWaveStart, EventIssueStart, EventIssueEnd:
				key := string(ev.Kind)
				if ev.Issue != "" {
					key += " " + ev.Issue
				}
				waves[key] = append(waves[key], ev.Wave)
			case EventWaveEnd:
				barriers = append(barriers, ev.Wave)
			}
		})),
		Prompt:  func(r runner.Role) (string, error) { return "system prompt for " + string(r), nil },
		Sleep:   func(context.Context, time.Duration) error { return nil },
		Backoff: func(int) time.Duration { return 0 },
	}

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"}, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Waves != 2 {
		t.Fatalf("ran %d wave(s), want 2", rep.Waves)
	}

	want := map[string][]int{
		"wave-start":      {1, 2},
		"issue-start t-1": {1},
		"issue-end t-1":   {1},
		"issue-start t-2": {2},
		"issue-end t-2":   {2},
	}
	for key, w := range want {
		if got := waves[key]; !equalInts(got, w) {
			t.Errorf("%s carried wave %v, want %v", key, got, w)
		}
	}
	if !equalInts(barriers, []int{1, 2}) {
		t.Errorf("the barriers reported waves %v, want [1 2]", barriers)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
