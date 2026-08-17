package ask

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/runner"
	"bd-auto/internal/runner/claude"
)

// LiveEnv opts a run of this file's test in. It spawns a real model and spends
// real money, so it is out of `make check` and out of the gate, in the same way
// scripts/resume-vs-fresh.sh is.
const LiveEnv = "BD_AUTO_ASK_LIVE"

// The one thing no amount of unit testing establishes: that a real backend's
// MCP client is satisfied by this server.
//
// Everything else here is asserted against a client written in the same package
// as the server, which proves the two agree with each other and nothing about
// whether either agrees with the CLI. This runs the whole path instead — the
// adapter's argv, the CLI's own client, the shim it spawns, the socket, the
// broker, and the answer coming back into the model's turn — against a live
// `claude -p`.
//
// It is also the only check on the parts of that path a version bump can move
// underneath us: the flag names, the config shape, the tool-name qualification,
// and the handshake. Run it after upgrading the CLI.
func TestLiveClaudeCallsTheTool(t *testing.T) {
	if os.Getenv(LiveEnv) == "" {
		t.Skipf("set %s=1 to run this; it spawns a real model and costs real money", LiveEnv)
	}
	if _, err := exec.LookPath(binName(t)); err != nil {
		t.Skipf("no claude CLI on PATH: %v", err)
	}

	srv := serve(t, PolicyAsk)
	srv.Broker().Hold = 90 * time.Second
	srv.Broker().Timeout = 2 * time.Minute
	// Under `go test` the executable is this test binary, so the shim has to be
	// pointed at a real bd-auto.
	srv.bin = buildBinary(t)

	// A token the model cannot produce by guessing, so the assertion cannot pass
	// on a plausible answer the model made up.
	const token = "PERIWINKLE-Q7X"
	// Stands in for the human. The wait has to be in minutes rather than the
	// seconds a unit test can assume: a real CLI has to start, load its config,
	// spawn the shim, handshake, and take a model turn before the question
	// arrives.
	asked := make(chan Question, 1)
	go func() {
		q, ok := awaitQuestion(srv.Broker(), 3*time.Minute)
		if !ok {
			return // the assertions below say so; there is nothing to answer
		}
		asked <- q
		srv.Broker().Reply(q.ID, token)
	}()

	rn, err := claude.New(runner.Spec{Model: "haiku", Permissions: runner.PermBypass})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	res, err := rn.Run(ctx, runner.Request{
		Role: runner.RoleWorker,
		Dir:  t.TempDir(),
		Prompt: "Call the " + ToolAsk + " tool exactly once, with the question " +
			"\"Which colour should the banner be?\" and the options \"red\" and \"blue\". " +
			"If it returns PENDING, collect the answer with " + ToolWait + ". " +
			"Then reply with the answer text you were given and nothing else. " +
			"Do not answer the question yourself and do not edit any file.",
		Permissions: runner.PermBypass,
		ToolServers: []runner.ToolServer{srv.Spec("t-1", "worker")},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != runner.ClassOK {
		t.Fatalf("the run came back %s: %v", res.Class, res.Err)
	}
	// Which half failed matters. No question means the CLI never reached the
	// tool — the handshake, the argv or the allowlist — and is the failure a
	// version bump causes. A question with no token means the tool was called
	// and the answer did not get back into the turn.
	select {
	case q := <-asked:
		if q.Issue != "t-1" || q.Role != "worker" {
			t.Errorf("the question arrived as %s/%s, not from the argv the spec fixed", q.Issue, q.Role)
		}
	default:
		t.Fatalf("the model never called %s.\nfinal message: %q\ndenials: %v", ToolAsk, res.Text, res.Denials)
	}
	if !strings.Contains(res.Text, token) {
		t.Fatalf("the answer did not reach the model.\nfinal message: %q\ndenials: %v", res.Text, res.Denials)
	}
}

// awaitQuestion blocks until a question is queued, or within elapses.
func awaitQuestion(b *Broker, within time.Duration) (Question, bool) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if pending := b.Pending(); len(pending) > 0 {
			return pending[0], true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return Question{}, false
}

// buildBinary compiles bd-auto for the shim to be, and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bd-auto")
	cmd := exec.Command("go", "build", "-o", bin, "bd-auto/cmd/bd-auto")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build bd-auto: %v\n%s", err, out)
	}
	return bin
}

func binName(t *testing.T) string {
	t.Helper()
	if env := os.Getenv(claude.BinEnv); env != "" {
		return env
	}
	return claude.DefaultBin
}
