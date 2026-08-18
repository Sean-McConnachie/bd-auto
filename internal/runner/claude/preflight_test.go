package claude

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/runner"
)

// preflightCLI writes a stub that answers --version with version and every
// other invocation with a clean result line, recording the argv it was given.
// The recorded argv is the point: what a preflight is for is proving the CLI
// was handed the flags a worker will hand it.
func preflightCLI(t *testing.T, version, argvFile string) string {
	t.Helper()
	return fakeCLI(t, `if [ "$1" = "--version" ]; then echo '`+version+`'; exit 0; fi
for a in "$@"; do printf '%s\n' "$a"; done > `+argvFile+`
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"S1","result":"ok","total_cost_usd":0.001}'
`)
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the probe never spawned the CLI: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// The whole value of the check is that the probe goes through the same argv
// builder a worker does. A probe that skipped a flag would pass on the install
// that drops it, which is the one case it exists for.
func TestPreflightProbesWithTheWorkersArgv(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	r := &Runner{
		Spec: runner.Spec{Model: "opus", Permissions: runner.PermAuto},
		Bin:  preflightCLI(t, "2.1.233 (Claude Code)", argv),
	}

	desc, err := r.Preflight(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !strings.Contains(desc, "2.1.233") || !strings.Contains(desc, "opus") {
		t.Errorf("description = %q, want it to name the version and the model", desc)
	}

	got := readArgv(t, argv)
	for _, flag := range []string{
		"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--model", "opus", "--append-system-prompt", "--permission-mode", "auto", "--session-id",
	} {
		if !slices.Contains(got, flag) {
			t.Errorf("argv %q is missing %q", got, flag)
		}
	}
	if i := slices.Index(got, "--session-id"); i >= 0 && i+1 < len(got) {
		if id := got[i+1]; len(id) != 36 || strings.Count(id, "-") != 4 {
			t.Errorf("--session-id %q is not a uuid, which is the only shape the CLI takes", id)
		}
	}
}

// A scoped role carries its allowlist into the probe, because under scoped
// permissions the allowlist is the argv rather than a setting beside it.
func TestPreflightProbesTheRolesOwnConfiguration(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	r := &Runner{
		Spec: runner.Spec{
			Model:        "sonnet",
			Permissions:  runner.PermScoped,
			AllowedTools: []string{"Read", "Grep"},
			ExtraArgs:    []string{"--settings", "/dev/null"},
		},
		Bin: preflightCLI(t, "2.1.233", argv),
	}
	if _, err := r.Preflight(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	got := readArgv(t, argv)
	if i := slices.Index(got, "--allowed-tools"); i < 0 || got[i+1] != "Read,Grep" {
		t.Errorf("argv %q does not carry the role's allowlist", got)
	}
	if i := slices.Index(got, "--permission-mode"); i < 0 || got[i+1] != "manual" {
		t.Errorf("argv %q does not probe under the role's own permission level", got)
	}
	if !slices.Contains(got, "--settings") {
		t.Errorf("argv %q does not carry the role's extra args", got)
	}
}

// The binary is checked before anything is spent, and the error says how to fix
// it: neither PATH nor the override is guessable from "no such file".
func TestPreflightMissingBinary(t *testing.T) {
	r := &Runner{Bin: filepath.Join(t.TempDir(), "not-installed")}
	_, err := r.Preflight(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Preflight succeeded against a binary that does not exist")
	}
	if !strings.Contains(err.Error(), BinEnv) {
		t.Errorf("error = %v, want it to name %s", err, BinEnv)
	}
}

// Something on PATH under the right name that is not the CLI fails as the CLI
// being wrong, not as a model failing.
func TestPreflightVersionSaysNothing(t *testing.T) {
	r := &Runner{Bin: fakeCLI(t, "exit 0\n")}
	_, err := r.Preflight(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Preflight succeeded against a binary that printed no version")
	}
	if !strings.Contains(err.Error(), "not the claude CLI") {
		t.Errorf("error = %v, want it to say what is wrong with the binary", err)
	}
}

func TestPreflightVersionExitsNonZero(t *testing.T) {
	r := &Runner{Bin: fakeCLI(t, "echo 'bad option: --version' >&2\nexit 2\n")}
	_, err := r.Preflight(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Preflight succeeded against a CLI whose --version failed")
	}
	if !strings.Contains(err.Error(), "bad option") {
		t.Errorf("error = %v, want it to carry what the CLI said", err)
	}
}

// The failure this whole check exists for: the binary is there and answers
// --version, and rejects the argv the run will spawn.
func TestPreflightRejectedArgv(t *testing.T) {
	r := &Runner{Bin: fakeCLI(t, `if [ "$1" = "--version" ]; then echo '3.0.0'; exit 0; fi
echo "error: unknown option '--include-partial-messages'" >&2
exit 1
`)}
	_, err := r.Preflight(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Preflight succeeded against a CLI that rejected the argv")
	}
	if !strings.Contains(err.Error(), "unknown option") {
		t.Errorf("error = %v, want it to carry what the CLI said", err)
	}
	if !strings.Contains(err.Error(), "3.0.0") {
		t.Errorf("error = %v, want it to name the version that rejected it", err)
	}
}

// An outage at the start of a run is worth stopping for too: five workers about
// to meet the same usage limit is a wave of retries against a wall.
func TestPreflightUsageLimit(t *testing.T) {
	r := &Runner{Bin: fakeCLI(t, `if [ "$1" = "--version" ]; then echo '2.1.233'; exit 0; fi
echo 'Claude AI usage limit reached|1799999999' >&2
exit 1
`)}
	_, err := r.Preflight(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Preflight succeeded against a CLI that reported a usage limit")
	}
	if !strings.Contains(err.Error(), string(runner.ClassInfraFailed)) {
		t.Errorf("error = %v, want it to name the class", err)
	}
}

// A role whose configuration cannot produce an invocation at all is a config
// error, and the preflight is where it should be found: it would have failed
// identically on every spawn of the run.
func TestPreflightUnusableRoleConfiguration(t *testing.T) {
	r := &Runner{
		Spec: runner.Spec{Model: "sonnet", Permissions: runner.PermScoped},
		Bin:  preflightCLI(t, "2.1.233", filepath.Join(t.TempDir(), "argv")),
	}
	_, err := r.Preflight(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Preflight succeeded for a scoped role with no allowed tools")
	}
	if !strings.Contains(err.Error(), "scoped permissions with no allowed tools") {
		t.Errorf("error = %v, want the reason the request could not be built", err)
	}
}

// A probe that hangs is the failure it was added to catch, arriving in its
// worst form. It is bounded, and by the role's own timeout when that is
// shorter.
func TestPreflightTimeout(t *testing.T) {
	r := &Runner{
		Spec:      runner.Spec{Timeout: 150 * time.Millisecond},
		Bin:       fakeCLI(t, "if [ \"$1\" = \"--version\" ]; then echo '2.1.233'; exit 0; fi\nsleep 30\n"),
		KillGrace: 100 * time.Millisecond,
	}
	if got := r.preflightTimeout(); got != 150*time.Millisecond {
		t.Errorf("preflightTimeout = %v, want the role's shorter timeout", got)
	}

	done := make(chan error, 1)
	go func() {
		_, err := r.Preflight(context.Background(), t.TempDir())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Preflight succeeded against a CLI that never answered")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Preflight did not return; the probe is unbounded")
	}
}

// A cancelled run is not a broken backend, but it is not a passed preflight
// either: nothing should be dispatched after it.
func TestPreflightCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Runner{Bin: preflightCLI(t, "2.1.233", filepath.Join(t.TempDir(), "argv"))}
	if _, err := r.Preflight(ctx, t.TempDir()); err == nil {
		t.Fatal("Preflight succeeded under a cancelled context")
	}
}
