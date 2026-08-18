package cmds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/runstate"
)

// bigRun builds a run state of n issues, spread across every bucket the poll
// view reports on.
func bigRun(n int) (*runstate.State, []string) {
	st := runstate.New("epic-1", 5, "auto", 1)
	st.Status = runstate.StatusActive
	st.Wave = 4
	var ready []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("beads-auto-imp-wz9.%d", i)
		st.Scope = append(st.Scope, id)
		switch i % 4 {
		case 0:
			st.Done = append(st.Done, id)
		case 1:
			st.InFlight[id] = runstate.Attempt{Branch: "bd-auto/" + id, Attempt: 1}
		case 2:
			st.Parked = append(st.Parked, runstate.Parked{ID: id, Reason: "gate failed"})
		case 3:
			ready = append(ready, id)
		}
	}
	return st, ready
}

// epicStats is the worst case the poll view has to fit in: an epic of n
// children with every bucket the view reports on non-empty, the deferred one
// included. bd counts deferred issues as open, so the view has to have room to
// say otherwise and still hold its budget.
func epicStats(n int) bd.Stats {
	return bd.Stats{Total: n, Closed: n / 4, Deferred: n / 4, Open: n / 2}
}

// TestRenderContextIsBoundedByEpicSize is the mechanical half of this project's
// acceptance criterion: a session that launches a drain and polls it must not
// grow with the size of the epic it launched.
//
// A poll's cost is paid once every couple of minutes for the whole run, so a
// view that names every finished issue would put the orchestrator's context
// cost straight back into the launching session — the exact thing the headless
// engine removed. So the invariant checked here is structural: no line may name
// more than maxNamed issues, at any epic size.
func TestRenderContextIsBoundedByEpicSize(t *testing.T) {
	for _, n := range []int{8, 400, 4000} {
		st, ready := bigRun(n)
		render(t, n, renderContext(st, epicStats(n), ready))

		// The same bound at the other end of a run, where the handoff line is.
		// A finished run is the only shape that can print it, so it is the only
		// shape that proves it did not add a line.
		done, _ := bigRun(n)
		done.Status = runstate.StatusDone
		done.InFlight = map[string]runstate.Attempt{}
		done.Base = "main"
		done.EpicBranch = "bd-auto/epic/beads-auto-imp-wz9-20260817-141230"
		done.PR = "https://github.com/an-organisation/a-repository/pull/1234"
		render(t, n, renderContext(done, epicStats(n), nil))

		staged := *done
		staged.PR = ""
		render(t, n, renderContext(&staged, epicStats(n), nil))
	}
}

// render asserts the poll view's bound on one rendering of it.
func render(t *testing.T, n int, got string) {
	t.Helper()
	if lines := strings.Count(got, "\n"); lines > 4 {
		t.Fatalf("%d-issue epic: poll view is %d lines, want at most 4:\n%s", n, lines, got)
	}
	// 400 bytes is the figure scripts/launch-cost.sh budgets a poll at, so it
	// is the number that has to hold. This is the worst case the view can
	// produce — both lists saturated, long IDs; a realistic run at concurrency
	// 5 with nothing parked prints half of it.
	if len(got) > 400 {
		t.Fatalf("%d-issue epic: poll view is %d bytes, want under 400:\n%s", n, len(got), got)
	}
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "running:") && !strings.HasPrefix(line, "parked") {
			continue
		}
		if named := strings.Count(line, ",") + 1; named > maxNamed {
			t.Fatalf("%d-issue epic: %q names %d issues, want at most %d",
				n, line, named, maxNamed)
		}
	}
}

// TestRenderContextElidesRatherThanTruncates: a capped list that just stops is
// a lie about how much is running. The count of what it left out is the part
// that keeps it honest.
func TestRenderContextElidesRatherThanTruncates(t *testing.T) {
	st, ready := bigRun(400)
	got := renderContext(st, epicStats(400), ready)
	if !strings.Contains(got, fmt.Sprintf("and %d more", 100-maxNamed)) {
		t.Fatalf("100 running issues, %d named, so 94 must be accounted for:\n%s", maxNamed, got)
	}
}

// TestRenderContextReportsAnIdleRun covers the poll that ends the loop: a run
// with nothing running and nothing queued has to say so, or a watcher polls a
// finished run forever.
func TestRenderContextReportsAnIdleRun(t *testing.T) {
	st := runstate.New("epic-1", 5, "auto", 1)
	st.Status = runstate.StatusDone
	st.Done = []string{"a", "b"}

	got := renderContext(st, bd.Stats{Total: 2, Closed: 2}, nil)
	if !strings.Contains(got, "nothing left to dispatch") {
		t.Fatalf("a drained run must say so:\n%s", got)
	}
	if !strings.Contains(got, runstate.StatusDone) {
		t.Fatalf("the status is what ends the poll loop, so it must appear:\n%s", got)
	}
}

// bd counts a deferred issue as open and ready, so a watcher polling an epic
// whose backlog is mostly discovered work sees a run waiting on issues nobody
// can pick up. The count is the whole fix: the view says how many, and says
// nothing at all when there are none.
func TestRenderContextSeparatesDeferredFromOpenChildren(t *testing.T) {
	st := runstate.New("epic-1", 5, "auto", 1)
	st.Status = runstate.StatusActive

	got := renderContext(st, bd.Stats{Total: 19, Closed: 3, Deferred: 16}, nil)
	if !strings.Contains(got, "16 deferred") {
		t.Fatalf("deferred children must be reported apart from open ones:\n%s", got)
	}
	if quiet := renderContext(st, bd.Stats{Total: 3, Closed: 3}, nil); strings.Contains(quiet, "deferred") {
		t.Fatalf("an epic with nothing deferred must not pay for the word:\n%s", quiet)
	}
}

// waitRepo is a repo root holding one saved run state.
func waitRepo(t *testing.T, status string) *Ctx {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := runstate.New("epic-1", 5, "auto", 1)
	st.Status = status
	if err := runstate.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	return &Ctx{RepoRoot: dir}
}

// TestWaitForRunReturnsWhenTheRunEnds is the reason --wait exists: one call has
// to cover a whole run, so it must come back the moment the run stops rather
// than sitting out its full deadline.
func TestWaitForRunReturnsWhenTheRunEnds(t *testing.T) {
	c := waitRepo(t, runstate.StatusActive)

	go func() {
		time.Sleep(2 * pollInterval)
		_, _ = runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
			s.Status = runstate.StatusDone
			return nil
		})
	}()

	start := time.Now()
	st, err := waitForRun(c, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != runstate.StatusDone {
		t.Fatalf("waited but returned status %q, want %q", st.Status, runstate.StatusDone)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("took %s to notice the run ended; the wait is not polling", elapsed)
	}
}

// TestWaitForRunGivesUpAtTheDeadline: a wedged run must not block the caller
// forever. Coming back and saying "still active" is the useful answer.
func TestWaitForRunGivesUpAtTheDeadline(t *testing.T) {
	c := waitRepo(t, runstate.StatusActive)

	start := time.Now()
	st, err := waitForRun(c, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != runstate.StatusActive {
		t.Fatalf("status %q, want the run reported as still active", st.Status)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("overran its deadline by %s", elapsed)
	}
}

// TestWaitForRunDoesNotWaitForNothing: with no run recorded there is nothing
// coming, so --wait must not hold the caller for its full deadline.
func TestWaitForRunDoesNotWaitForNothing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := waitForRun(&Ctx{RepoRoot: dir}, time.Minute)
	if !errors.Is(err, runstate.ErrNoRun) {
		t.Fatalf("err = %v, want ErrNoRun", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s for a run that does not exist", elapsed)
	}
}

// TestWaitForRunSkipsAFinishedRun: polling a run that already ended is the
// normal last call of a watch loop, and it has to be instant.
func TestWaitForRunSkipsAFinishedRun(t *testing.T) {
	c := waitRepo(t, runstate.StatusDone)

	start := time.Now()
	st, err := waitForRun(c, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != runstate.StatusDone {
		t.Fatalf("status %q, want %q", st.Status, runstate.StatusDone)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s on an already-finished run", elapsed)
	}
}

// TestStatusJSONReportsOnlyArmedRunsAsActive is the field every caller branches
// on: a launcher deciding whether to keep polling, and scripts/smoke.sh
// deciding whether it may delete .beads/auto. It has to mean "a run is going",
// not "run.json exists" — a stopped run and a standalone `bd-auto issue run`
// both leave the file behind, and neither is a run anybody is waiting on.
func TestStatusJSONReportsOnlyArmedRunsAsActive(t *testing.T) {
	for _, tc := range []struct {
		status string
		active bool
	}{
		{runstate.StatusActive, true},
		{runstate.StatusPaused, true},
		{runstate.StatusDone, false},
		{runstate.StatusStandalone, false},
	} {
		st := runstate.New("epic-1", 5, "auto", 1)
		st.Status = tc.status
		got := statusJSON(st, bd.Stats{}, nil)
		if got["active"] != tc.active {
			t.Fatalf("status %q reported active=%v, want %v", tc.status, got["active"], tc.active)
		}
		// The rest is still reported either way: an unarmed run is the one a
		// human is most likely to be reading this output to understand.
		if got["status"] != tc.status {
			t.Fatalf("status %q was not reported back: %v", tc.status, got["status"])
		}
	}
}

// TestRenderContextDropsTheEpicClauseWithNoEpic covers the poll view of a
// standalone `bd-auto issue run`. There is no epic, so there are no children to
// count, and printing "0/0 children closed" about one describes a drained epic
// rather than a run that never had one.
func TestRenderContextDropsTheEpicClauseWithNoEpic(t *testing.T) {
	st := runstate.New("", 1, "auto", 0)
	st.Status = runstate.StatusStandalone
	st.InFlight["beads-auto-imp-gvg"] = runstate.Attempt{Branch: "bd-auto/beads-auto-imp-gvg", Attempt: 1}

	got := renderContext(st, bd.Stats{}, nil)
	if !strings.Contains(got, runstate.StatusStandalone) {
		t.Fatalf("the run must say what kind of run it is:\n%s", got)
	}
	if strings.Contains(got, "children closed") || strings.Contains(got, "epic ") {
		t.Fatalf("no epic means no epic progress to report:\n%s", got)
	}
	if !strings.Contains(got, "running: beads-auto-imp-gvg") {
		t.Fatalf("the issue being worked is the whole content of this view:\n%s", got)
	}
}
