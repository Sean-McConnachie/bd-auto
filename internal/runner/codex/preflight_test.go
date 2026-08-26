package codex

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

func preflightCLI(t *testing.T, version, argvPath string) string {
	t.Helper()
	return fakeCLI(t, `
if [ "$1" = "--version" ]; then printf '%s\n' '`+version+`'; exit 0; fi
if [ "$1" = "login" ] && [ "$2" = "status" ]; then printf '%s\n' 'Logged in using ChatGPT'; exit 0; fi
printf '%s\n' "$@" > '`+argvPath+`'
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"preflight-thread"}' '{"type":"turn.started"}' '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}' '{"type":"turn.completed","usage":{"input_tokens":7,"cached_input_tokens":2,"output_tokens":1}}'
`)
}

func TestPreflightUsesResolvedCodexInvocation(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	argvPath := filepath.Join(t.TempDir(), "argv")
	r := &Runner{
		Spec: runner.Spec{
			Model: "gpt-test", Sandbox: "workspace-write", ApprovalPolicy: "never",
			Shell: true, ExtraArgs: []string{"--ephemeral"},
		},
		Bin: preflightCLI(t, "codex-cli 9.8.7", argvPath),
	}
	desc, err := r.Preflight(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	for _, want := range []string{"codex-cli 9.8.7", "model gpt-test", "billing chatgpt-plan"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description %q does not contain %q", desc, want)
		}
	}
	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, want := range []string{"exec", "--json", "--strict-config", "--model", "gpt-test", "--sandbox", "workspace-write", "--ephemeral", "-"} {
		if !slices.Contains(argv, want) {
			t.Errorf("probe argv %q is missing %q", argv, want)
		}
	}
}

func TestPreflightChecksAuthenticationBeforeProbe(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	probeMarker := filepath.Join(t.TempDir(), "probe")
	r := &Runner{Bin: fakeCLI(t, `
if [ "$1" = "--version" ]; then echo 'codex-cli 1.0'; exit 0; fi
if [ "$1" = "login" ]; then echo 'Not logged in'; exit 1; fi
: > '`+probeMarker+`'
`)}
	_, err := r.Preflight(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("Preflight error = %v", err)
	}
	if _, statErr := os.Stat(probeMarker); !os.IsNotExist(statErr) {
		t.Fatal("preflight spent a probe after authentication could not be established")
	}
}

func TestAuthorizedBillingSourceIsReusedByPreflight(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	callLog := filepath.Join(t.TempDir(), "calls")
	r := &Runner{Bin: fakeCLI(t, `
printf '%s\n' "$*" >> '`+callLog+`'
if [ "$1" = "--version" ]; then echo 'codex-cli 1.0'; exit 0; fi
if [ "$1" = "login" ]; then echo 'Logged in using ChatGPT'; exit 0; fi
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"preflight-thread"}' '{"type":"turn.completed","usage":{}}'
`)}
	dir := t.TempDir()
	if _, err := r.BillingSource(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Preflight(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "login status\n"); got != 1 {
		t.Fatalf("login status calls = %d, want 1; calls:\n%s", got, raw)
	}
}

func TestPreflightMissingAndInvalidCLI(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		r := &Runner{Bin: filepath.Join(t.TempDir(), "missing")}
		_, err := r.Preflight(context.Background(), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), BinEnv) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("empty version", func(t *testing.T) {
		r := &Runner{Bin: fakeCLI(t, "exit 0\n")}
		_, err := r.Preflight(context.Background(), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "not the Codex CLI") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestPreflightProbeFailureAndTimeout(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	t.Run("rejected invocation", func(t *testing.T) {
		r := &Runner{Bin: fakeCLI(t, `
if [ "$1" = "--version" ]; then echo 'codex-cli 2.0'; exit 0; fi
if [ "$1" = "login" ]; then echo 'Logged in using ChatGPT'; exit 0; fi
echo 'unknown option --json' >&2; exit 2
`)}
		_, err := r.Preflight(context.Background(), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "infra-failed") || !strings.Contains(err.Error(), "2.0") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("bounded", func(t *testing.T) {
		r := &Runner{Spec: runner.Spec{Timeout: 100 * time.Millisecond}, KillGrace: 50 * time.Millisecond, Bin: fakeCLI(t, `
if [ "$1" = "--version" ]; then echo 'codex-cli 2.0'; exit 0; fi
if [ "$1" = "login" ]; then echo 'Logged in using ChatGPT'; exit 0; fi
sleep 30
`)}
		if got := r.preflightTimeout(); got != 100*time.Millisecond {
			t.Fatalf("timeout = %s", got)
		}
		_, err := r.Preflight(context.Background(), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("error = %v", err)
		}
	})
}
