package cmds

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"bd-auto/internal/runstate"
)

// statusRepo is a repo holding one run state, which is all `worker status`
// reads.
func statusRepo(t *testing.T, st *runstate.State) string {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if st != nil {
		if err := os.MkdirAll(filepath.Join(repo, ".beads", "auto"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := runstate.Save(repo, st); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BD_AUTO_REPO", repo)
	t.Chdir(repo)
	return repo
}

func statusOf(t *testing.T) map[string]any {
	t.Helper()
	read := captureStdout(t)
	if err := workerStatus(nil); err != nil {
		t.Fatalf("worker status: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(read()), &got); err != nil {
		t.Fatalf("worker status did not print JSON: %v", err)
	}
	return got
}

// TestWorkerStatusReportsAStoppedRunAsInactive is beads-auto-imp-vzz. A
// run.json on disk is not a run in progress, and reporting one as active is
// what made a drain resume a finished run and exit in a second having done
// nothing.
func TestWorkerStatusReportsAStoppedRunAsInactive(t *testing.T) {
	for _, status := range []string{runstate.StatusDone, runstate.StatusStandalone} {
		t.Run(status, func(t *testing.T) {
			st := runstate.New("epic-1", 1, "auto", 0)
			st.Status = status
			st.UpdatedAt = time.Now().UTC()
			statusRepo(t, st)
			if got := statusOf(t)["active"]; got != false {
				t.Fatalf("a %q run reported active=%v; a run.json on disk is not a run in progress",
					status, got)
			}
		})
	}
}

// TestWorkerStatusStillReportsARunningRun. The fix must not make the command
// blind to the case it exists for.
func TestWorkerStatusStillReportsARunningRun(t *testing.T) {
	st := runstate.New("epic-1", 1, "auto", 0)
	st.Status = runstate.StatusActive
	statusRepo(t, st)
	if got := statusOf(t)["active"]; got != true {
		t.Fatalf("an active run reported active=%v", got)
	}
}

// TestWorkerStatusAgreesWithRunStatus. Two commands disagreeing about whether a
// run is armed is worse than either answer on its own.
func TestWorkerStatusAgreesWithRunStatus(t *testing.T) {
	for _, status := range []string{
		runstate.StatusActive, runstate.StatusPaused,
		runstate.StatusDone, runstate.StatusStandalone,
	} {
		st := runstate.New("epic-1", 1, "auto", 0)
		st.Status = status
		statusRepo(t, st)
		want := st.Active()
		if got := statusOf(t)["active"]; got != want {
			t.Errorf("%q: worker status says active=%v, State.Active() says %v", status, got, want)
		}
	}
}

// TestWorkerStatusWithNoRunAtAll stays what it was.
func TestWorkerStatusWithNoRunAtAll(t *testing.T) {
	statusRepo(t, nil)
	if got := statusOf(t)["active"]; got != false {
		t.Fatalf("no run at all reported active=%v", got)
	}
}
