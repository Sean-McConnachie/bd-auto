package codex

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/runner"
)

const (
	LiveEnv           = "BD_AUTO_CODEX_LIVE"
	LiveAPIBillingEnv = "BD_AUTO_CODEX_LIVE_API_BILLING"
)

// TestLivePreflight is an explicit paid diagnostic. The default suite always
// skips it. API-backed authentication needs a second, dedicated consent value;
// enabling the general live test alone is not permission to create API charges.
func TestLivePreflight(t *testing.T) {
	r := liveRunner(t)
	dir := liveRepo(t)
	desc, err := r.Preflight(context.Background(), dir)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	t.Logf("preflight: %s", desc)
}

// TestLiveTinyTaskAndResume verifies the real JSONL stream, pinned model,
// transcript, usage totals, and thread resume path with two small turns.
func TestLiveTinyTaskAndResume(t *testing.T) {
	r := liveRunner(t)
	dir := liveRepo(t)
	firstLog := filepath.Join(dir, "first.jsonl")
	first, err := r.Run(context.Background(), runner.Request{
		Role: runner.RoleWorker, Model: r.Spec.Model, Dir: dir, LogPath: firstLog,
		Prompt: "Reply with only the word ready. Do not use tools.",
	}, nil)
	if err != nil || first.Class != runner.ClassOK {
		t.Fatalf("first turn = %+v, %v", first, err)
	}
	if first.SessionID == "" || first.Usage.InputTokens+first.Usage.CacheReadTokens == 0 || first.Usage.OutputTokens == 0 {
		t.Fatalf("first turn did not report a session and token usage: %+v", first)
	}
	if first.Usage.CostUSD != 0 {
		t.Fatalf("Codex CLI unexpectedly reported a dollar cost: %+v", first.Usage)
	}
	secondLog := filepath.Join(dir, "resume.jsonl")
	second, err := r.Run(context.Background(), runner.Request{
		Role: runner.RoleWorker, Model: r.Spec.Model, Dir: dir, LogPath: secondLog,
		Prompt: "Reply with only the word resumed. Do not use tools.",
		Resume: true, SessionID: first.SessionID,
	}, nil)
	if err != nil || second.Class != runner.ClassOK || second.SessionID != first.SessionID {
		t.Fatalf("resumed turn = %+v, %v", second, err)
	}
	for _, path := range []string{firstLog, secondLog} {
		raw, readErr := os.ReadFile(path)
		if readErr != nil || !strings.Contains(string(raw), `"type"`) {
			t.Fatalf("transcript %s was not recorded as JSONL: %v", path, readErr)
		}
	}
	t.Logf("model=%s session=%s usage=%+v", r.Spec.Model, first.SessionID, first.Usage.Add(second.Usage))
}

// TestLiveAddedDirectoryAllowsUnixSocket is an explicit paid proof that an
// added writable root reaches a capability outside the repository. It uses a
// disposable socket, not the Docker daemon.
func TestLiveAddedDirectoryAllowsUnixSocket(t *testing.T) {
	r := liveRunner(t)
	dir := liveRepo(t)
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "proof.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on proof socket: %v", err)
	}
	defer ln.Close()

	r.Spec.Sandbox = "workspace-write"
	r.Spec.AddDirs = []string{socketDir}
	const token = "bd-auto-add-dir-proof"
	received := make(chan string, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			received <- "accept: " + acceptErr.Error()
			return
		}
		defer conn.Close()
		data, readErr := io.ReadAll(conn)
		if readErr != nil {
			received <- "read: " + readErr.Error()
			return
		}
		received <- string(data)
	}()

	res, err := r.Run(context.Background(), runner.Request{
		Role: runner.RoleWorker, Model: r.Spec.Model, Dir: dir,
		Prompt: "Use python3 and the socket module to connect to the Unix socket at " + socketPath +
			". Send exactly " + token + ", close the socket, then reply done.",
	}, nil)
	if err != nil || res.Class != runner.ClassOK {
		t.Fatalf("socket turn = %+v, %v", res, err)
	}
	select {
	case got := <-received:
		if got != token {
			t.Fatalf("socket received %q, want %q", got, token)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Codex did not connect to the socket in the added directory")
	}
}

func liveRunner(t *testing.T) *Runner {
	t.Helper()
	if os.Getenv(LiveEnv) != "1" {
		t.Skipf("set %s=1 to run the live Codex diagnostic", LiveEnv)
	}
	if _, err := exec.LookPath(DefaultBin); err != nil {
		t.Skipf("no Codex CLI on PATH: %v", err)
	}
	spec := runner.Spec{
		Provider: Provider, Model: "gpt-5.6-terra",
		Sandbox: "read-only", ApprovalPolicy: "never", Shell: true,
	}
	r := &Runner{Spec: spec}
	source, err := r.BillingSource(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("billing source: %v", err)
	}
	if source == runner.BillingAPIKey && os.Getenv(LiveAPIBillingEnv) != "1" {
		t.Fatalf("Codex uses API-key billing; set %s=1 as well to authorize this live test", LiveAPIBillingEnv)
	}
	return r
}

func liveRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", "-b", "main", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initialize live-test repository: %v: %s", err, out)
	}
	return dir
}
