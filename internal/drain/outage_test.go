package drain

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
)

// A plan limit is the outage that cannot be waited out five seconds at a time.
// Retrying into one spends the whole ladder — 75 seconds across five processes —
// proving that a wall with hours left on it is still there, and then hands back
// an outage anyway. Where the CLI said when it lifts, the first failure is
// enough to know that.
func TestAFarOffLimitStopsTheRunWithoutBurningRetries(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	at := time.Now().Add(2 * time.Hour)
	workers := newByIssue()
	worker := workers.script("t-1", fake.Step{Class: runner.ClassInfraFailed, ResetAt: at})

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeInfra {
		t.Fatalf("run outcome %s (%s), want infra-failed", rep.Outcome, rep.Reason)
	}
	if n := worker.Calls(); n != 1 {
		t.Errorf("the worker ran %d time(s), want 1: the retries were spent against a limit "+
			"that cannot lift before they run out", n)
	}
	if got := outcomeOf(t, rep, "t-1"); got.Attempts[0].InfraRetries != 0 {
		t.Errorf("%d process(es) were burned on the environment, want none",
			got.Attempts[0].InfraRetries)
	}

	when := at.Format(resetTimeFormat)
	if !strings.Contains(rep.Reason, when) {
		t.Errorf("the run stopped saying %q; it has to name the reset (%s), or the only "+
			"answer it leaves is to re-run into the same wall", rep.Reason, when)
	}

	// And in bd-auto's own record of the run, which is where somebody coming
	// back to a stopped run looks first.
	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatalf("load run state: %v", err)
	}
	if !strings.Contains(strings.Join(st.Notes, "\n"), when) {
		t.Errorf("run state notes do not name the reset (%s):\n%s", when, strings.Join(st.Notes, "\n"))
	}
	// Nothing is parked for an outage, and that is not changed by knowing when
	// it ends.
	if len(rep.Parked) != 0 {
		t.Errorf("an outage parked %v", rep.Parked)
	}
}

// The other half: a limit that lifts inside the window is sat out rather than
// retried into. The wait is the limit's, not the ladder's — a backoff of
// seconds against a reset ten minutes out is five wasted processes.
func TestANearLimitIsWaitedOutRatherThanRetriedInto(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	at := time.Now().Add(10 * time.Minute)
	workers := newByIssue()
	workers.script("t-1",
		fake.Step{Class: runner.ClassInfraFailed, ResetAt: at},
		closeAndCommit(iss, "t-1", "one.txt"),
	)

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	var (
		mu    sync.Mutex
		waits []time.Duration
	)
	e.Sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
		return nil
	}

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("run outcome %s (%s), want done: the limit lifted and the round re-ran",
			rep.Outcome, rep.Reason)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) == 0 {
		t.Fatal("the round was re-run without waiting for the limit at all")
	}
	if waits[0] < 9*time.Minute {
		t.Errorf("waited %s for a limit ten minutes out; that is the backoff ladder, "+
			"which this round is going to spend five processes climbing", waits[0])
	}
}

// A reset the engine will not sit through is a stop; one it will is a wait.
// Everything else about an infra failure is unchanged, including a reset that
// has already passed, where the ladder is still the only thing that knows
// whether the limit really lifted.
func TestHoldFor(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 38, 0, 0, time.UTC)
	e := &Engine{InfraRetries: 3, Backoff: func(n int) time.Duration { return time.Duration(n) * time.Second }}

	cases := []struct {
		name   string
		res    runner.Result
		streak int
		want   time.Duration
		hold   bool
	}{
		{
			name:   "no reset reported: the ladder",
			res:    runner.Result{},
			streak: 2,
			want:   2 * time.Second,
			hold:   true,
		},
		{
			name:   "a reset inside the window is waited out",
			res:    runner.Result{ResetAt: now.Add(20 * time.Minute)},
			streak: 1,
			want:   20*time.Minute + resetMargin,
			hold:   true,
		},
		{
			name:   "a reset beyond it stops on the first failure",
			res:    runner.Result{ResetAt: now.Add(3 * time.Hour)},
			streak: 1,
			hold:   false,
		},
		{
			name:   "a reset already passed falls back to the ladder",
			res:    runner.Result{ResetAt: now.Add(-time.Minute)},
			streak: 1,
			want:   time.Second,
			hold:   true,
		},
		{
			name:   "the retry cap still ends it",
			res:    runner.Result{ResetAt: now.Add(time.Minute)},
			streak: 3,
			hold:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, hold := e.holdFor(c.res, c.streak, now)
			if hold != c.hold {
				t.Fatalf("hold = %v, want %v", hold, c.hold)
			}
			if hold && got != c.want {
				t.Errorf("wait = %s, want %s", got, c.want)
			}
		})
	}
}

// The reason has to say which side of the reset the reader is on. "The
// environment failed" alone is answered by re-running immediately, which is the
// one answer a limit refuses.
func TestResetNote(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 38, 0, 0, time.UTC)
	if got := resetNote(time.Time{}, now); got != "" {
		t.Errorf("resetNote of nothing = %q, want empty", got)
	}
	got := resetNote(now.Add(26*time.Minute), now)
	if !strings.Contains(got, "13:04") || !strings.Contains(got, "in 26m") {
		t.Errorf("resetNote = %q, want the clock time and how long that is", got)
	}
	if got := resetNote(now.Add(-time.Minute), now); !strings.Contains(got, "already lifted") {
		t.Errorf("resetNote of a passed reset = %q, want it to say a re-run is worth trying", got)
	}
}
