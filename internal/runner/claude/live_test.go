package claude

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"bd-auto/internal/runner"
)

// LiveEnv opts a run of this file's test in. It spawns a real model and spends
// real money, so it is out of `make check` and out of the gate, in the same way
// internal/ask's live test is.
const LiveEnv = "BD_AUTO_CLAUDE_LIVE"

// The one thing no stub establishes: that the installed CLI still accepts the
// argv this adapter builds.
//
// Every other test here asserts the argv against a fixture, which proves this
// package agrees with itself and nothing about whether it agrees with the CLI.
// This runs the preflight against the real one, which is the whole point of the
// preflight — so it is also the check to run after upgrading the CLI, before a
// drain discovers the upgrade a worktree at a time.
func TestLivePreflight(t *testing.T) {
	if os.Getenv(LiveEnv) == "" {
		t.Skipf("set %s=1 to run this; it spawns a real model and costs real money", LiveEnv)
	}
	bin := DefaultBin
	if env := os.Getenv(BinEnv); env != "" {
		bin = env
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("no claude CLI on PATH: %v", err)
	}

	r := &Runner{Spec: runner.Spec{Model: "haiku", Permissions: runner.PermAuto}}
	desc, err := r.Preflight(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !strings.HasPrefix(desc, "claude ") {
		t.Errorf("description = %q, want it to name the version it found", desc)
	}
	t.Logf("preflight: %s", desc)
}
