//go:build unix

package drain

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"bd-auto/internal/runner"
	"bd-auto/internal/runner/claude"
)

// k has to reach the grandchildren, and this is the test that says so end to
// end rather than one layer at a time.
//
// The claude adapter's process-group kill is covered in its own package. What
// is covered here is the chain above it: a key press becomes Control.Kill,
// which cancels one issue's context, which is the context the adapter is
// watching. A break anywhere along it leaves a worker's `go test ./...` running
// and holding the worktree the next attempt wants to delete — and nothing about
// the run's own report would look wrong.
func TestKillTakesTheWorkersChildrenWithIt(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	bin := stubCLI(t, "sleep 300 &\necho $! > "+pidFile+"\nsleep 300\n")

	e := engine(t, repo, testCfg(1, 0), iss, nil, nil)
	e.Bus = NewBus(collector())
	e.Control = NewControl()
	e.NewRunner = func(runner.Role, runner.Spec) (runner.Runner, error) {
		return &claude.Runner{Bin: bin, KillGrace: 300 * time.Millisecond}, nil
	}

	done := make(chan DrainReport, 1)
	go func() {
		rep, err := e.Drain(context.Background(), DrainOptions{
			Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
		})
		if err != nil {
			t.Errorf("Drain: %v", err)
		}
		done <- rep
	}()

	pid := waitForPID(t, pidFile)
	if !alive(pid) {
		t.Fatalf("the grandchild %d was never running", pid)
	}
	if !e.Control.Kill("t-1") {
		t.Fatal("the dispatched worker was not killable")
	}

	select {
	case rep := <-done:
		got := outcomeOf(t, rep, "t-1")
		if got.Outcome != OutcomeFailed || got.Stage != StageKilled {
			t.Fatalf("the killed issue came back %s at %q, want failed at %q",
				got.Outcome, got.Stage, StageKilled)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not end after the worker was killed")
	}

	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the grandchild %d survived the kill: it is still holding the worktree", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stubCLI writes a shell script that stands in for the claude CLI and returns
// its path.
func stubCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			if text := strings.TrimSpace(string(raw)); text != "" {
				pid, err := strconv.Atoi(text)
				if err != nil {
					t.Fatalf("the pid file holds %q", text)
				}
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stub never recorded a grandchild pid at %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// alive treats a reaped-but-uncollected process as gone: signal 0 succeeds
// against a zombie, and a zombie holds nothing.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat))
	return len(fields) < 3 || fields[2] != "Z"
}
