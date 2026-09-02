package codex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/runner"
)

func TestArgsFreshAndResume(t *testing.T) {
	spec := runner.Spec{
		Model: "gpt-5.6-sol", Sandbox: "workspace-write", ApprovalPolicy: "never",
		AddDirs: []string{"/var/run", "/opt/project-cache"},
		Shell:   true, WebSearch: false, ViewImage: true,
	}
	r := &Runner{Spec: spec}

	fresh := runner.Request{
		Prompt: "implement it", SystemPrompt: "quote: \"x\"\nslash: \\",
		SessionID: "caller-id-is-not-an-argument", ExtraArgs: []string{"--color", "never"},
	}
	wantFresh := []string{
		"exec", "--json", "--strict-config", "--model", "gpt-5.6-sol", "--sandbox", "workspace-write",
		"--add-dir", "/var/run", "--add-dir", "/opt/project-cache",
		"-c", `developer_instructions="quote: \"x\"\nslash: \\"`,
		"-c", `approval_policy="never"`,
		"-c", "features.shell_tool=true",
		"-c", "tools.web_search=false",
		"-c", "features.view_image=true",
		"--color", "never", "-",
	}
	got, err := r.args(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantFresh) {
		t.Fatalf("fresh argv\n got %q\nwant %q", got, wantFresh)
	}

	resumed := fresh
	resumed.Resume = true
	resumed.SessionID = "thread-123"
	wantResume := []string{
		"exec", "resume", "thread-123", "--json", "--strict-config", "--model", "gpt-5.6-sol",
		"-c", `sandbox_mode="workspace-write"`,
		"-c", `sandbox_workspace_write.writable_roots=["/var/run","/opt/project-cache"]`,
		"-c", `developer_instructions="quote: \"x\"\nslash: \\"`,
		"-c", `approval_policy="never"`,
		"-c", "features.shell_tool=true",
		"-c", "tools.web_search=false",
		"-c", "features.view_image=true",
		"--color", "never", "-",
	}
	got, err = r.args(resumed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantResume) {
		t.Fatalf("resume argv\n got %q\nwant %q", got, wantResume)
	}
}

func TestArgsRejectInvalidRequests(t *testing.T) {
	r := &Runner{}
	for _, test := range []struct {
		name string
		req  runner.Request
		text string
	}{
		{"empty prompt", runner.Request{}, "no prompt"},
		{"blank prompt", runner.Request{Prompt: " \n"}, "no prompt"},
		{"resume id", runner.Request{Prompt: "go", Resume: true}, "no session id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := r.args(test.req)
			if err == nil || !strings.Contains(err.Error(), test.text) {
				t.Fatalf("error = %v, want text %q", err, test.text)
			}
		})
	}
}

func TestArgsRejectInvalidAddedDirectories(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec runner.Spec
		want string
	}{
		{"empty", runner.Spec{Sandbox: "workspace-write", AddDirs: []string{""}}, "empty"},
		{"relative", runner.Spec{Sandbox: "workspace-write", AddDirs: []string{"var/run"}}, "not absolute"},
		{"nul", runner.Spec{Sandbox: "workspace-write", AddDirs: []string{"/var/ru\x00n"}}, "NUL"},
		{"wrong sandbox", runner.Spec{Sandbox: "read-only", AddDirs: []string{"/var/run"}}, "workspace-write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Runner{Spec: tc.spec}).args(runner.Request{Prompt: "go"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestArgsDefaultToNoAddedDirectories(t *testing.T) {
	args, err := (&Runner{Spec: runner.Spec{Sandbox: "workspace-write"}}).args(runner.Request{Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "add-dir") || configValueOptional(args, "sandbox_workspace_write.writable_roots") != "" {
		t.Fatalf("default invocation widened the sandbox: %q", args)
	}
}

func configValueOptional(args []string, key string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && strings.HasPrefix(args[i+1], key+"=") {
			return strings.TrimPrefix(args[i+1], key+"=")
		}
	}
	return ""
}

func TestMCPServersAreSafeDeterministicTOML(t *testing.T) {
	servers := []runner.ToolServer{
		{
			Name: `z."server`, Command: "tool\ncommand", Args: []string{"a\\b", `"; injected=true`},
			Env: []string{"Z=last", "A=one=two"}, Tools: []string{"ask_user", `odd"tool`},
			Required: true, Timeout: 1501 * time.Millisecond,
		},
		{Name: "alpha", Command: "alpha-tool"},
	}
	got, err := mcpServersTOML(servers)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"alpha"={command="alpha-tool",required=false,default_tools_approval_mode="approve"},` +
		`"z.\"server"={command="tool\ncommand",args=["a\\b","\"; injected=true"],` +
		`env={"A"="one=two","Z"="last"},enabled_tools=["ask_user","odd\"tool"],` +
		`required=true,tool_timeout_sec=2,default_tools_approval_mode="approve"}}`
	if got != want {
		t.Fatalf("MCP TOML\n got %s\nwant %s", got, want)
	}

	args, err := (&Runner{}).args(runner.Request{Prompt: "go", ToolServers: servers})
	if err != nil {
		t.Fatal(err)
	}
	if value := configValue(t, args, "mcp_servers"); value != got {
		t.Fatalf("config value = %q, want %q", value, got)
	}
	if args[len(args)-1] != "-" {
		t.Fatalf("prompt sentinel is not last: %q", args)
	}
}

func TestTOMLStringEscapesEveryControlCharacter(t *testing.T) {
	value := "quote=\" slash=\\ newline=\n tab=\t del=\x7f unicode=🦫"
	got := tomlString(value)
	if strings.ContainsRune(got, '\x7f') || !strings.Contains(got, `\u007f`) {
		t.Fatalf("DEL is not safely escaped in %q", got)
	}
	for _, want := range []string{`\"`, `\\`, `\n`, `\t`, "🦫"} {
		if !strings.Contains(got, want) {
			t.Fatalf("TOML string %q is missing %q", got, want)
		}
	}
}

func TestMCPRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		servers []runner.ToolServer
		text    string
	}{
		{"empty name", []runner.ToolServer{{Command: "x"}}, "name and a command"},
		{"empty command", []runner.ToolServer{{Name: "x"}}, "name and a command"},
		{"duplicate", []runner.ToolServer{{Name: "x", Command: "a"}, {Name: "x", Command: "b"}}, "duplicate"},
		{"bad env", []runner.ToolServer{{Name: "x", Command: "a", Env: []string{"NOPE"}}}, "environment"},
		{"duplicate env", []runner.ToolServer{{Name: "x", Command: "a", Env: []string{"A=1", "A=2"}}}, "repeats"},
		{"negative timeout", []runner.ToolServer{{Name: "x", Command: "a", Timeout: -1}}, "negative"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := mcpServersTOML(test.servers)
			if err == nil || !strings.Contains(err.Error(), test.text) {
				t.Fatalf("error = %v, want text %q", err, test.text)
			}
		})
	}
}

func TestCapabilitiesDoNotClaimClaudePermissions(t *testing.T) {
	caps := (&Runner{}).Caps()
	if !caps.Resume || !caps.Stream || !caps.ReportsUsage || !caps.Tools {
		t.Fatalf("capabilities = %+v", caps)
	}
	if len(caps.Permissions) != 0 {
		t.Fatalf("Codex claims Claude permissions: %v", caps.Permissions)
	}
}

func TestRegisteredProvider(t *testing.T) {
	built, err := runner.New(runner.Spec{Provider: Provider})
	if err != nil {
		t.Fatal(err)
	}
	if built.Name() != Provider {
		t.Fatalf("provider = %q", built.Name())
	}
}

func TestRunUsesStdinAdoptsThreadIDAndPreservesTranscript(t *testing.T) {
	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.Mkdir(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	argvPath := filepath.Join(dir, "argv")
	stdinPath := filepath.Join(dir, "stdin")
	cwdPath := filepath.Join(dir, "cwd")
	t.Setenv("CODEX_TEST_ARGV", argvPath)
	t.Setenv("CODEX_TEST_STDIN", stdinPath)
	t.Setenv("CODEX_TEST_CWD", cwdPath)
	t.Setenv(BinEnv, fakeCLI(t, `
: > "$CODEX_TEST_ARGV"
for arg do printf '%s\n' "$arg" >> "$CODEX_TEST_ARGV"; done
cat > "$CODEX_TEST_STDIN"
pwd > "$CODEX_TEST_CWD"
printf '%s\n' '{"type":"thread.started","thread_id":"authoritative-thread"}' '{"type":"turn.started"}' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":2,"cached_input_tokens":1,"output_tokens":3}}'
`))
	logPath := filepath.Join(dir, "logs", "run.jsonl")
	prompt := "quotes \" newlines\nbackslash \\ unicode 🦫"
	res, err := (&Runner{Spec: runner.Spec{Model: "gpt-test", Sandbox: "read-only"}}).Run(
		context.Background(), runner.Request{Prompt: prompt, Dir: dir, SessionID: "caller-thread", LogPath: logPath}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != runner.ClassOK || res.SessionID != "authoritative-thread" || res.LogPath != logPath {
		t.Fatalf("result = %+v", res)
	}
	if res.Text != "done" || res.Usage != (runner.Usage{InputTokens: 1, CacheReadTokens: 1, OutputTokens: 3, Turns: 1}) {
		t.Fatalf("parsed result = %+v", res)
	}
	if got := readFile(t, stdinPath); got != prompt {
		t.Fatalf("stdin = %q, want %q", got, prompt)
	}
	if got := strings.TrimSpace(readFile(t, cwdPath)); got != dir {
		t.Fatalf("working directory = %q, want %q", got, dir)
	}
	argv := strings.Split(strings.TrimSpace(readFile(t, argvPath)), "\n")
	if contains(argv, prompt) || contains(argv, "caller-thread") {
		t.Fatalf("prompt or caller session leaked into argv: %q", argv)
	}
	if argv[len(argv)-1] != "-" {
		t.Fatalf("argv does not end in stdin sentinel: %q", argv)
	}
	wantLog := "{\"type\":\"thread.started\",\"thread_id\":\"authoritative-thread\"}\n{\"type\":\"turn.started\"}\n{\"type\":\"item.completed\",\"item\":{\"id\":\"answer\",\"type\":\"agent_message\",\"text\":\"done\"}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":2,\"cached_input_tokens\":1,\"output_tokens\":3}}\n"
	if got := readFile(t, logPath); got != wantLog {
		t.Fatalf("transcript = %q, want %q", got, wantLog)
	}
	entries, err := os.ReadDir(codexHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("adapter wrote persistent Codex state: %v", entries)
	}
}

func TestRunRejectsBadWorkingDirectories(t *testing.T) {
	r := &Runner{}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, dir, text string }{
		{"missing", "", "no working directory"},
		{"not found", filepath.Join(t.TempDir(), "absent"), "working directory"},
		{"file", file, "not a directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := r.Run(context.Background(), runner.Request{Prompt: "go", Dir: test.dir}, nil)
			if err == nil || !strings.Contains(err.Error(), test.text) {
				t.Fatalf("error = %v, want %q", err, test.text)
			}
		})
	}
}

func TestRunResumeKeepsRequestedSessionWithoutThreadEvent(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Bin: fakeCLI(t, `cat >/dev/null; printf '%s\n' '{"type":"turn.started"}' '{"type":"turn.completed","usage":{}}'`)}
	res, err := r.Run(context.Background(), runner.Request{Prompt: "again", Dir: dir, Resume: true, SessionID: "thread-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != runner.ClassOK || res.SessionID != "thread-1" {
		t.Fatalf("result = %+v", res)
	}
}

func TestTranscriptNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.jsonl")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: fakeCLI(t, `cat >/dev/null; printf '%s\n' '{"type":"thread.started","thread_id":"new"}'`)}
	res, err := r.Run(context.Background(), runner.Request{Prompt: "go", Dir: dir, LogPath: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "old" {
		t.Fatalf("existing transcript overwritten: %q", got)
	}
	wantPath := filepath.Join(dir, "run-2.jsonl")
	if res.LogPath != wantPath {
		t.Fatalf("LogPath = %q, want %q", res.LogPath, wantPath)
	}
}

func TestTimeoutForcesAnIgnoringProcessToExit(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Bin: fakeCLI(t, `trap '' TERM; cat >/dev/null; while :; do sleep 1; done`), KillGrace: 20 * time.Millisecond}
	started := time.Now()
	res, err := r.Run(context.Background(), runner.Request{Prompt: "go", Dir: dir, Resume: true, SessionID: "thread", Timeout: 30 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != runner.ClassInterrupted || !res.TimedOut || res.Err == nil {
		t.Fatalf("result = %+v", res)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("process that ignored SIGTERM was not killed after the grace period")
	}
}

func TestCancellationTerminatesTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "pid")
	t.Setenv("CODEX_TEST_PID", pidPath)
	r := &Runner{Bin: fakeCLI(t, `
trap '' TERM
(trap '' TERM; while :; do sleep 1; done) &
child=$!
printf '%s' "$child" > "$CODEX_TEST_PID"
cat >/dev/null
while :; do sleep 1; done
`), KillGrace: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan runner.Result, 1)
	go func() {
		res, _ := r.Run(ctx, runner.Request{Prompt: "go", Dir: dir, Resume: true, SessionID: "thread"}, nil)
		done <- res
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(pidPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake CLI did not start its child")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case res := <-done:
		if res.Class != runner.ClassInterrupted || res.TimedOut {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled process group did not exit")
	}
	pidText := strings.TrimSpace(readFile(t, pidPath))
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("child pid = %q: %v", pidText, err)
	}
	deadline = time.Now().Add(time.Second)
	for processAlive(pid) && !zombie(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived cancellation", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCompletedTurnWinsCancellationRace(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Bin: fakeCLI(t, `
cat >/dev/null
trap 'exit 143' TERM
printf '%s\n' '{"type":"turn.completed","usage":{}}'
while :; do sleep 1; done
`), KillGrace: 100 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	res, err := r.Run(ctx, runner.Request{Prompt: "go", Dir: dir, Resume: true, SessionID: "thread"}, runner.SinkFunc(func(event runner.Event) {
		if event.Kind == runner.EventUsage {
			cancel()
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != runner.ClassOK || res.TimedOut {
		t.Fatalf("result = %+v; completed turn observed before cancellation must win", res)
	}
}

func TestCancellationBeforeShutdownCompletionIsInterrupted(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	t.Setenv("CODEX_TEST_READY", ready)
	r := &Runner{Bin: fakeCLI(t, `
cat >/dev/null
trap 'printf "%s\n" "{\"type\":\"turn.completed\",\"usage\":{}}"; exit 0' TERM
: > "$CODEX_TEST_READY"
while :; do sleep 1; done
`), KillGrace: 100 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan runner.Result, 1)
	go func() {
		res, _ := r.Run(ctx, runner.Request{Prompt: "go", Dir: dir, Resume: true, SessionID: "thread"}, nil)
		done <- res
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake CLI did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case res := <-done:
		if res.Class != runner.ClassInterrupted {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("graceful cancellation did not finish")
	}
}

func TestRunClassifiesFailuresAndResetOnlyForInfrastructure(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, event, stderr string
		want                runner.Class
		reset               bool
	}{
		{"plan limit", `{"type":"turn.failed","error":{"type":"usage_limit","message":"limit resets in 20 minutes"}}`, "", runner.ClassInfraFailed, true},
		{"work failure with misleading stderr", `{"type":"turn.failed","error":{"type":"model_error","message":"tests failed"}}`, "HTTP 429", runner.ClassWorkFailed, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `cat >/dev/null; printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"worked"}}' '` + test.event + `'`
			if test.stderr != "" {
				body += `; printf '%s\n' '` + test.stderr + `' >&2`
			}
			body += `; exit 1`
			res, err := (&Runner{Bin: fakeCLI(t, body)}).Run(context.Background(), runner.Request{Prompt: "go", Dir: dir, Resume: true, SessionID: "thread"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Class != test.want || res.ResetAt.IsZero() != !test.reset {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

func TestStderrBufferKeepsOnlyTheTail(t *testing.T) {
	buffer := &capBuffer{limit: 8}
	if _, err := buffer.Write([]byte("prefix-tail")); err != nil {
		t.Fatal(err)
	}
	if got := buffer.String(); got != "fix-tail" {
		t.Fatalf("stderr tail = %q", got)
	}
}

func zombie(pid int) bool {
	value, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(value))
	return len(fields) >= 3 && fields[2] == "Z"
}

func configValue(t *testing.T, args []string, key string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && strings.HasPrefix(args[i+1], key+"=") {
			return strings.TrimPrefix(args[i+1], key+"=")
		}
	}
	t.Fatalf("config key %q absent from %q", key, args)
	return ""
}

func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
