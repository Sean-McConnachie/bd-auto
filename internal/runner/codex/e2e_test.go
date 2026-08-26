package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/ask"
	"bd-auto/internal/runner"
)

// TestFakeCodexEndToEnd drives the process adapter through one executable
// fixture. It covers the protocol features and result classes that a complete
// drain depends on without credentials, network access, or model spending.
func TestFakeCodexEndToEnd(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv")
	mcpResult := filepath.Join(t.TempDir(), "mcp-result")
	t.Setenv("CODEX_E2E_ARGV", argvLog)
	t.Setenv("CODEX_E2E_MCP_RESULT", mcpResult)
	t.Setenv("CODEX_E2E_MCP_BIN", os.Args[0])
	t.Setenv("CODEX_E2E_MCP_HELPER", "1")

	broker := ask.NewBroker(ask.PolicyAsk)
	broker.Hold = 2 * time.Second
	broker.Timeout = 5 * time.Second
	srv, err := ask.Listen(t.TempDir(), broker)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	t.Setenv("CODEX_E2E_MCP_SOCKET", srv.Path())

	const answer = "PERIWINKLE-Q7X"
	asked := make(chan ask.Question, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if pending := broker.Pending(); len(pending) > 0 {
				asked <- pending[0]
				broker.Reply(pending[0].ID, answer)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	bin := fakeCLI(t, `
printf '%s ' "$@" >> "$CODEX_E2E_ARGV"; printf '\n' >> "$CODEX_E2E_ARGV"
prompt=$(cat)
case "$prompt" in
  *"happy worker"*)
    printf '%s\n' \
      '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' \
      '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
      '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_user","arguments":{"header":"Color","question":"Which color?","options":[{"label":"blue"}]}}}' \
      | "$CODEX_E2E_MCP_BIN" -test.run=TestCodexMCPShimHelper > "$CODEX_E2E_MCP_RESULT"
    ;;
esac
case "$prompt" in
  *cancel*) sleep 30 ;;
  *infrastructure*) echo 'network error: connection refused' >&2; exit 1 ;;
  *work-failure*)
    printf '%s\n' '{"type":"thread.started","thread_id":"work-thread"}' '{"type":"turn.started"}' '{"type":"item.completed","item":{"type":"agent_message","text":"attempted the work"}}' '{"type":"turn.failed","error":{"code":"compile_error","message":"tests failed"}}'
    exit 1 ;;
  *resume*)
    printf '%s\n' '{"type":"turn.started"}' '{"type":"item.completed","item":{"type":"agent_message","text":"resumed"}}' '{"type":"turn.completed","usage":{"input_tokens":11,"cached_input_tokens":5,"output_tokens":2}}' ;;
  *)
    printf '%s\n' '{"type":"thread.started","thread_id":"thread-e2e"}' '{"type":"turn.started"}' '{"type":"item.started","item":{"type":"mcp_tool_call","server":"bd_auto","tool":"ask_user","status":"in_progress"}}' '{"type":"item.completed","item":{"type":"mcp_tool_call","server":"bd_auto","tool":"ask_user","status":"completed"}}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":20,"cached_input_tokens":4,"output_tokens":3}}' ;;
esac
`)
	r := &Runner{Bin: bin, KillGrace: 50 * time.Millisecond, Spec: runner.Spec{
		Model: "gpt-test", Sandbox: "workspace-write", ApprovalPolicy: "never", Shell: true,
	}}
	dir := t.TempDir()
	server := runner.ToolServer{
		Name: "bd_auto", Command: os.Args[0], Args: []string{"-test.run=TestCodexMCPShimHelper"},
		Env:   []string{"CODEX_E2E_MCP_HELPER=1", "CODEX_E2E_MCP_SOCKET=" + srv.Path()},
		Tools: []string{"ask_user"}, Required: true,
	}

	var total runner.Usage
	for _, role := range []runner.Role{runner.RoleWorker, runner.RoleReviewer, runner.RoleIntegrator} {
		var events []runner.Event
		res, err := r.Run(context.Background(), runner.Request{
			Role: role, Prompt: "happy " + string(role), Dir: dir, ToolServers: []runner.ToolServer{server},
		}, runner.SinkFunc(func(event runner.Event) { events = append(events, event) }))
		if err != nil || res.Class != runner.ClassOK || res.SessionID != "thread-e2e" {
			t.Fatalf("%s result = %+v, %v", role, res, err)
		}
		if res.Usage.CostUSD != 0 || res.Usage.InputTokens != 16 || res.Usage.CacheReadTokens != 4 || res.Usage.OutputTokens != 3 {
			t.Fatalf("%s usage = %+v", role, res.Usage)
		}
		foundAsk := false
		for _, event := range events {
			if event.Role != role {
				t.Fatalf("%s event attributed to %s", role, event.Role)
			}
			if event.Kind == runner.EventToolUse && event.Tool == "bd_auto/ask_user" {
				foundAsk = true
			}
		}
		if !foundAsk {
			t.Fatalf("%s did not report the ask_user MCP invocation", role)
		}
		total = total.Add(res.Usage)
	}
	if total != (runner.Usage{InputTokens: 48, CacheReadTokens: 12, OutputTokens: 9, Turns: 3}) {
		t.Fatalf("aggregated usage = %+v", total)
	}
	select {
	case question := <-asked:
		if question.Issue != "t-1" || question.Role != "worker" || question.Text != "Which color?" {
			t.Fatalf("MCP question = %+v", question)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the offline Codex fixture did not reach the question broker")
	}
	mcpOutput, err := os.ReadFile(mcpResult)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mcpOutput), answer) {
		t.Fatalf("MCP answer did not return through the shim:\n%s", mcpOutput)
	}

	res, err := r.Run(context.Background(), runner.Request{
		Role: runner.RoleWorker, Prompt: "resume", Dir: dir, Resume: true, SessionID: "thread-e2e",
	}, nil)
	if err != nil || res.Class != runner.ClassOK || res.SessionID != "thread-e2e" || res.Text != "resumed" {
		t.Fatalf("resume result = %+v, %v", res, err)
	}

	for _, test := range []struct {
		prompt string
		want   runner.Class
	}{
		{"work-failure", runner.ClassWorkFailed},
		{"infrastructure", runner.ClassInfraFailed},
	} {
		res, err := r.Run(context.Background(), runner.Request{Prompt: test.prompt, Dir: dir}, nil)
		if err != nil || res.Class != test.want {
			t.Fatalf("%s result = %+v, %v", test.prompt, res, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan runner.Result, 1)
	go func() {
		res, _ := r.Run(ctx, runner.Request{Prompt: "cancel", Dir: dir}, nil)
		done <- res
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		if res.Class != runner.ClassInterrupted {
			t.Fatalf("cancel result = %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled fake Codex process did not stop")
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"mcp_servers=", "ask_user", os.Args[0], "-test.run=TestCodexMCPShimHelper",
		"CODEX_E2E_MCP_SOCKET", srv.Path(), "exec resume thread-e2e", "--json",
		"--strict-config", "--model gpt-test", "features.view_image=false",
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv log does not contain %q:\n%s", want, argv)
		}
	}
}

func TestCodexMCPShimHelper(t *testing.T) {
	if os.Getenv("CODEX_E2E_MCP_HELPER") != "1" {
		t.Skip("helper subprocess")
	}
	shim := ask.Shim{Socket: os.Getenv("CODEX_E2E_MCP_SOCKET"), Issue: "t-1", Role: "worker"}
	if err := shim.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		t.Fatal(err)
	}
}
