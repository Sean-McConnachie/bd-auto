package claude

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

// The argv is this adapter's whole contract with the CLI, and it is the one
// part of a run that can be asserted without spending anything. So it is
// asserted exactly, flag for flag and in order.
func TestArgs(t *testing.T) {
	cases := []struct {
		name string
		req  runner.Request
		want []string
	}{
		{
			name: "worker fresh",
			req: runner.Request{
				Role:         runner.RoleWorker,
				Prompt:       "implement bd-1",
				SystemPrompt: "you are a worker",
				Model:        "opus",
				Permissions:  runner.PermAuto,
				SessionID:    "11111111-1111-1111-1111-111111111111",
			},
			want: []string{
				"-p", "implement bd-1",
				"--output-format", "stream-json",
				"--verbose",
				"--include-partial-messages",
				"--model", "opus",
				"--append-system-prompt", "you are a worker",
				"--permission-mode", "auto",
				"--session-id", "11111111-1111-1111-1111-111111111111",
			},
		},
		{
			name: "worker resumed",
			req: runner.Request{
				Role:         runner.RoleWorker,
				Prompt:       "the gate failed:\n...",
				SystemPrompt: "you are a worker",
				Model:        "opus",
				Permissions:  runner.PermAuto,
				SessionID:    "11111111-1111-1111-1111-111111111111",
				Resume:       true,
			},
			want: []string{
				"-p", "the gate failed:\n...",
				"--output-format", "stream-json",
				"--verbose",
				"--include-partial-messages",
				"--model", "opus",
				"--append-system-prompt", "you are a worker",
				"--permission-mode", "auto",
				"--resume", "11111111-1111-1111-1111-111111111111",
			},
		},
		{
			// A reviewer with a bare Bash entry is a reviewer that can push, so
			// the list is passed through exactly as configured.
			name: "reviewer scoped",
			req: runner.Request{
				Role:         runner.RoleReviewer,
				Prompt:       "review bd-1",
				Model:        "sonnet",
				Permissions:  runner.PermScoped,
				AllowedTools: []string{"Read", "Grep", "Bash(git diff:*)"},
				SessionID:    "22222222-2222-2222-2222-222222222222",
			},
			want: []string{
				"-p", "review bd-1",
				"--output-format", "stream-json",
				"--verbose",
				"--include-partial-messages",
				"--model", "sonnet",
				"--session-id", "22222222-2222-2222-2222-222222222222",
				"--allowed-tools", "Read,Grep,Bash(git diff:*)",
			},
		},
		{
			name: "integrator bypass, no session",
			req: runner.Request{
				Role:        runner.RoleIntegrator,
				Prompt:      "resolve the conflict",
				Permissions: runner.PermBypass,
			},
			want: []string{
				"-p", "resolve the conflict",
				"--output-format", "stream-json",
				"--verbose",
				"--include-partial-messages",
				"--permission-mode", "bypassPermissions",
			},
		},
		{
			name: "unset permissions default to auto, extra args last",
			req: runner.Request{
				Role:      runner.RoleWorker,
				Prompt:    "go",
				ExtraArgs: []string{"--settings", "/tmp/s.json"},
			},
			want: []string{
				"-p", "go",
				"--output-format", "stream-json",
				"--verbose",
				"--include-partial-messages",
				"--permission-mode", "auto",
				"--settings", "/tmp/s.json",
			},
		},
	}

	r := &Runner{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := r.args(c.req)
			if err != nil {
				t.Fatalf("args: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("argv mismatch\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// A request that cannot be run at all is rejected before a process is spawned,
// which is the one case Run is allowed to return an error for.
func TestArgsRejects(t *testing.T) {
	cases := []struct {
		name string
		req  runner.Request
		want string
	}{
		{"no prompt", runner.Request{}, "no prompt"},
		{"blank prompt", runner.Request{Prompt: "  \n"}, "no prompt"},
		{"resume without session", runner.Request{Prompt: "go", Resume: true}, "no session id"},
		{"unknown permissions", runner.Request{Prompt: "go", Permissions: "root"}, "permission level"},
		{"scoped without tools", runner.Request{Prompt: "go", Permissions: runner.PermScoped}, "no allowed tools"},
	}
	r := &Runner{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := r.args(c.req); err == nil {
				t.Fatal("args: want an error, got none")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("args: error %q does not mention %q", err, c.want)
			}
			if _, err := r.Run(context.Background(), c.req, nil); err == nil {
				t.Error("Run: want an error for an unrunnable request, got none")
			}
		})
	}
}

// Class is the field the engine reads first, so the mapping onto it is the most
// consequential thing in this package. A 429 read as work-failed burns rounds
// and parks good issues.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		out  outcome
		want runner.Class
	}{
		{"clean", outcome{exitCode: 0, sawResult: true}, runner.ClassOK},
		{
			"success result but non-zero exit",
			outcome{exitCode: 1, sawResult: true},
			runner.ClassWorkFailed,
		},
		{
			"max turns",
			outcome{exitCode: 1, sawResult: true, resultErr: true, failText: "error_max_turns"},
			runner.ClassWorkFailed,
		},
		{
			"execution error",
			outcome{exitCode: 1, sawResult: true, resultErr: true, failText: "error_during_execution: tool loop"},
			runner.ClassWorkFailed,
		},
		{
			"usage limit",
			outcome{exitCode: 1, stderr: "Claude AI usage limit reached|1799999999"},
			runner.ClassInfraFailed,
		},
		{
			"429",
			outcome{exitCode: 1, stderr: `API Error: 429 {"type":"error","error":{"type":"rate_limit_error"}}`},
			runner.ClassInfraFailed,
		},
		{
			"overloaded reported through the result line",
			outcome{exitCode: 0, sawResult: true, resultErr: true, failText: "error_during_execution: API Error: 529 overloaded_error"},
			runner.ClassInfraFailed,
		},
		{
			"expired credentials",
			outcome{exitCode: 1, stderr: "OAuth token has expired, please run /login"},
			runner.ClassInfraFailed,
		},
		{
			"network",
			outcome{exitCode: 1, stderr: "fetch failed: ECONNRESET"},
			runner.ClassInfraFailed,
		},
		{
			"crash with no result line",
			outcome{exitCode: 1, stderr: "TypeError: undefined is not a function"},
			runner.ClassInfraFailed,
		},
		{
			"exit 0 with no result line",
			outcome{exitCode: 0},
			runner.ClassInfraFailed,
		},
		{
			"cli not runnable",
			outcome{startErr: os.ErrNotExist, exitCode: -1},
			runner.ClassInfraFailed,
		},
		{
			"cancelled",
			outcome{ctxErr: context.Canceled, exitCode: -1},
			runner.ClassInterrupted,
		},
		{
			// Cancellation outranks everything: a killed CLI exits non-zero and
			// its stderr is whatever it was midway through saying.
			"cancelled while rate limited",
			outcome{ctxErr: context.Canceled, exitCode: 1, stderr: "API Error: 429"},
			runner.ClassInterrupted,
		},
		{
			"timed out",
			outcome{timedOut: true, exitCode: -1},
			runner.ClassInterrupted,
		},
		{
			// A model that merely writes "429" in a successful report must not
			// classify its own success as an outage.
			"the word 429 in a successful run",
			outcome{exitCode: 0, sawResult: true, failText: "I fixed the 429 retry handler"},
			runner.ClassOK,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.out); got != c.want {
				t.Errorf("classify = %s, want %s", got, c.want)
			}
		})
	}
}

// fakeCLI writes an executable standing in for the claude binary and returns
// its path. The script ignores its arguments: what is being tested here is the
// reading of the output and the handling of the process, not the argv, which
// TestArgs covers exactly.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func collect(events *[]runner.Event) runner.EventSink {
	return runner.SinkFunc(func(e runner.Event) { *events = append(*events, e) })
}

func summarise(events []runner.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		switch e.Kind {
		case runner.EventText:
			out = append(out, "text:"+e.Text)
		case runner.EventToolUse, runner.EventToolResult:
			out = append(out, string(e.Kind)+":"+e.Tool)
		default:
			out = append(out, string(e.Kind))
		}
	}
	return out
}

const streamFixture = `{"type":"system","subtype":"init","session_id":"S1","tools":["Read"]}
{"type":"stream_event","session_id":"S1","event":{"type":"message_start","message":{"role":"assistant"}}}
{"type":"stream_event","session_id":"S1","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Look"}}}
{"type":"stream_event","session_id":"S1","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ing"}}}
{"type":"stream_event","session_id":"S1","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"Read"}}}
{"type":"assistant","session_id":"S1","message":{"role":"assistant","content":[{"type":"text","text":"Looking"},{"type":"tool_use","id":"tu_1","name":"Read"}],"usage":{"input_tokens":9,"output_tokens":3,"cache_read_input_tokens":100,"cache_creation_input_tokens":11}}}
{"type":"user","session_id":"S1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"package main"}]}}
this line is not json and must not stop the run
{"type":"result","subtype":"success","is_error":false,"session_id":"S1","result":"done: bd-1 implemented","total_cost_usd":0.0421,"usage":{"input_tokens":12,"output_tokens":34,"cache_read_input_tokens":560,"cache_creation_input_tokens":78}}
`

func TestRunParsesStream(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(fixture, []byte(streamFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "logs", "bd-1-0.jsonl")

	r := &Runner{Bin: fakeCLI(t, "cat "+fixture+"\n")}
	var events []runner.Event
	res, err := r.Run(context.Background(), runner.Request{
		Role:      runner.RoleWorker,
		Prompt:    "implement bd-1",
		SessionID: "S1",
		LogPath:   logPath,
	}, collect(&events))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Class != runner.ClassOK {
		t.Errorf("Class = %s (%v), want ok", res.Class, res.Err)
	}
	if res.Text != "done: bd-1 implemented" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.SessionID != "S1" {
		t.Errorf("SessionID = %q, want S1", res.SessionID)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}

	// Cost is the primary field: summing tokens across resumed rounds bills the
	// same prefix repeatedly, so the run total comes from total_cost_usd and
	// the cache halves are kept apart from the fresh ones.
	want := runner.Usage{CostUSD: 0.0421, InputTokens: 12, OutputTokens: 34, CacheReadTokens: 560, CacheCreationTokens: 78}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v", res.Usage, want)
	}

	// The partial deltas are the text; the completed assistant message repeats
	// them and must not be emitted again.
	got := summarise(events)
	wantEvents := []string{"start", "text:Look", "text:ing", "tool-use:Read", "usage", "tool-result:Read", "usage", "done"}
	if !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("events\n got %q\nwant %q", got, wantEvents)
	}

	if res.LogPath != logPath {
		t.Errorf("LogPath = %q, want %q", res.LogPath, logPath)
	}
	transcript, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if string(transcript) != streamFixture {
		t.Errorf("transcript does not match the raw output (%d bytes)", len(transcript))
	}
}

// A usage limit and a 429 are the failures that must not cost an issue a round,
// end to end through a real process rather than only through classify.
func TestRunInfraFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"usage limit", "echo 'Claude AI usage limit reached|1799999999' >&2\nexit 1\n"},
		{"rate limited", "echo 'API Error: 429 {\"type\":\"rate_limit_error\"}' >&2\nexit 1\n"},
		{"crash", "echo 'Error: Cannot find module (node:internal/modules)' >&2\nexit 1\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Runner{Bin: fakeCLI(t, c.body)}
			res, err := r.Run(context.Background(), runner.Request{Role: runner.RoleWorker, Prompt: "go"}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Class != runner.ClassInfraFailed {
				t.Errorf("Class = %s, want infra-failed", res.Class)
			}
			if res.Class.Counts() {
				t.Error("an infra failure must consume neither a round nor an attempt")
			}
			if res.Err == nil {
				t.Error("Err is nil; the log and the TUI have nothing to show")
			}
			if res.ExitCode != 1 {
				t.Errorf("ExitCode = %d, want 1", res.ExitCode)
			}
		})
	}
}

// A result line reporting failure is work to feed back, not an outage to retry.
func TestRunWorkFailed(t *testing.T) {
	r := &Runner{Bin: fakeCLI(t, `echo '{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"S9","total_cost_usd":0.5}'`+"\nexit 1\n")}
	res, err := r.Run(context.Background(), runner.Request{Role: runner.RoleWorker, Prompt: "go", SessionID: "S9"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Class != runner.ClassWorkFailed {
		t.Errorf("Class = %s, want work-failed", res.Class)
	}
	if !res.Class.Counts() {
		t.Error("work-failed must consume a round")
	}
	if res.Usage.CostUSD != 0.5 {
		t.Errorf("CostUSD = %v, want 0.5; a failed round still cost money", res.Usage.CostUSD)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "error_max_turns") {
		t.Errorf("Err = %v, want it to name the subtype", res.Err)
	}
}

func TestRunMissingBinary(t *testing.T) {
	r := &Runner{Bin: filepath.Join(t.TempDir(), "not-installed")}
	var events []runner.Event
	res, err := r.Run(context.Background(), runner.Request{Role: runner.RoleWorker, Prompt: "go"}, collect(&events))
	if err != nil {
		t.Fatalf("Run: a backend that will not start is a Result, not an error: %v", err)
	}
	if res.Class != runner.ClassInfraFailed {
		t.Errorf("Class = %s, want infra-failed", res.Class)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	if got, want := summarise(events), []string{"start", "error", "done"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

func TestRunTimeout(t *testing.T) {
	r := &Runner{Bin: fakeCLI(t, "sleep 30\n"), KillGrace: 200 * time.Millisecond}
	res, err := r.Run(context.Background(), runner.Request{
		Role:    runner.RoleWorker,
		Prompt:  "go",
		Timeout: 150 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Class != runner.ClassInterrupted {
		t.Errorf("Class = %s, want interrupted", res.Class)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if res.Class.Counts() {
		t.Error("an interrupt must consume neither a round nor an attempt")
	}
}

func TestRunCancelled(t *testing.T) {
	r := &Runner{Bin: fakeCLI(t, "sleep 30\n"), KillGrace: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	res, err := r.Run(ctx, runner.Request{Role: runner.RoleWorker, Prompt: "go"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Class != runner.ClassInterrupted {
		t.Errorf("Class = %s, want interrupted", res.Class)
	}
	if res.TimedOut {
		t.Error("TimedOut = true for a cancellation, want false")
	}
}

// A round that reached a clean result has already been paid for, so a stop
// landing in the moment after it finished must not throw it away and re-run it.
// Class is read before anything else, and interrupted here would mean losing a
// worker's whole turn to a keystroke that arrived too late to prevent it.
func TestCancelAfterCleanResultKeepsIt(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(fixture, []byte(streamFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// The stub ignores SIGTERM, so it delivers its result and exits 0 well
	// after the cancellation lands.
	r := &Runner{Bin: fakeCLI(t, "trap '' TERM\ncat "+fixture+"\nsleep 0.3\nexit 0\n"), KillGrace: 10 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	res, err := r.Run(ctx, runner.Request{Role: runner.RoleWorker, Prompt: "go", SessionID: "S1"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Class != runner.ClassOK {
		t.Errorf("Class = %s (%v), want ok: the run finished before the stop landed", res.Class, res.Err)
	}
	if res.Text != "done: bd-1 implemented" {
		t.Errorf("Text = %q, want the result that was already produced", res.Text)
	}
}

// The reason for Setpgid, asserted rather than assumed.
//
// exec.CommandContext kills the direct child only. A worker forty seconds into
// `go test ./...` is exactly this shape — the CLI is the child and the test
// binary is the grandchild — and killing only the child leaves the test running
// and holding the worktree that the next attempt wants to delete.
func TestCancelKillsGrandchild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	r := &Runner{
		Bin:       fakeCLI(t, "sleep 300 &\necho $! > "+pidFile+"\nsleep 300\n"),
		KillGrace: 500 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan runner.Result, 1)
	go func() {
		res, err := r.Run(ctx, runner.Request{Role: runner.RoleWorker, Prompt: "go"}, nil)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()

	pid := waitForPID(t, pidFile)
	if !stillRunning(pid) {
		t.Fatalf("grandchild %d was never running", pid)
	}
	cancel()

	select {
	case res := <-done:
		if res.Class != runner.ClassInterrupted {
			t.Errorf("Class = %s, want interrupted", res.Class)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	deadline := time.Now().Add(5 * time.Second)
	for stillRunning(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived cancellation: the process group was not signalled", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			if text := strings.TrimSpace(string(b)); text != "" {
				pid, err := strconv.Atoi(text)
				if err != nil {
					t.Fatalf("pid file holds %q", text)
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

// stillRunning treats a reaped-but-not-yet-collected process as gone: signal 0
// succeeds against a zombie, and a zombie is not holding a worktree.
func stillRunning(pid int) bool {
	if !processAlive(pid) {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat))
	if len(fields) < 3 {
		return true
	}
	return fields[2] != "Z"
}
