package codex

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bd-auto/internal/runner"
)

func TestParseOfficialStyleFullStream(t *testing.T) {
	stream, events, transcript := parseStream(t, "", `{"type":"thread.started","thread_id":"thread-42"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"r1","type":"reasoning","text":"thinking"}}
{"type":"item.completed","item":{"id":"r1","type":"reasoning","text":"thinking"}}
{"type":"item.started","item":{"id":"c1","type":"command_execution","command":"go test ./...","status":"in_progress"}}
{"type":"item.completed","item":{"id":"c1","type":"command_execution","command":"go test ./...","status":"completed","exit_code":0}}
{"type":"item.started","item":{"id":"f1","type":"file_change","status":"in_progress"}}
{"type":"item.completed","item":{"id":"f1","type":"file_change","status":"completed"}}
{"type":"item.started","item":{"id":"m1","type":"mcp_tool_call","server":"beads","tool":"show","status":"in_progress"}}
{"type":"item.completed","item":{"id":"m1","type":"mcp_tool_call","server":"beads","tool":"show","status":"completed"}}
{"type":"item.completed","item":{"id":"a1","type":"agent_message","text":"Implemented it."}}
{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":20,"output_tokens":30,"reasoning_output_tokens":17}}
`)
	if stream.sessionID != "thread-42" || stream.text != "Implemented it." || !stream.terminalComplete {
		t.Fatalf("parsed state = %+v", stream)
	}
	wantUsage := runner.Usage{InputTokens: 100, CacheReadTokens: 20, OutputTokens: 30, Turns: 1}
	if stream.usage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", stream.usage, wantUsage)
	}
	want := []eventSummary{
		{runner.EventToolUse, "shell", ""}, {runner.EventToolResult, "shell", ""},
		{runner.EventToolUse, "apply_patch", ""}, {runner.EventToolResult, "apply_patch", ""},
		{runner.EventToolUse, "beads/show", ""}, {runner.EventToolResult, "beads/show", ""},
		{runner.EventText, "", "Implemented it."}, {runner.EventUsage, "", ""},
	}
	if got := summarize(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	for _, event := range events {
		if event.SessionID != "thread-42" {
			t.Fatalf("event session = %q", event.SessionID)
		}
	}
	if transcript != strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-42"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"r1","type":"reasoning","text":"thinking"}}`,
		`{"type":"item.completed","item":{"id":"r1","type":"reasoning","text":"thinking"}}`,
		`{"type":"item.started","item":{"id":"c1","type":"command_execution","command":"go test ./...","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"c1","type":"command_execution","command":"go test ./...","status":"completed","exit_code":0}}`,
		`{"type":"item.started","item":{"id":"f1","type":"file_change","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"f1","type":"file_change","status":"completed"}}`,
		`{"type":"item.started","item":{"id":"m1","type":"mcp_tool_call","server":"beads","tool":"show","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"m1","type":"mcp_tool_call","server":"beads","tool":"show","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"a1","type":"agent_message","text":"Implemented it."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":20,"output_tokens":30,"reasoning_output_tokens":17}}`,
		"",
	}, "\n") {
		t.Fatal("transcript bytes changed")
	}
}

func TestParseMultipleTurnsUnknownMalformedAndCacheUnderflow(t *testing.T) {
	stream, events, _ := parseStream(t, "resumed-thread", `not json
{"type":"future.event","payload":{"anything":true}}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"first"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":30,"output_tokens":4}}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"second"}}
{"type":"turn.completed","usage":{"input_tokens":50,"cached_input_tokens":15,"output_tokens":6}}
`)
	want := runner.Usage{InputTokens: 35, CacheReadTokens: 45, OutputTokens: 10, Turns: 2}
	if stream.usage != want || stream.text != "second" || stream.badLines != 1 || !stream.terminalComplete {
		t.Fatalf("stream = %+v, want usage %+v and final second", stream, want)
	}
	var usage []runner.Usage
	for _, event := range events {
		if event.Kind == runner.EventUsage {
			usage = append(usage, event.Usage)
		}
	}
	if !reflect.DeepEqual(usage, []runner.Usage{
		{CacheReadTokens: 30, OutputTokens: 4, Turns: 1}, want,
	}) {
		t.Fatalf("usage events = %+v", usage)
	}
}

func TestCompletedMessagesAreNotDuplicatedAndPartialTextIsIgnored(t *testing.T) {
	stream, events, _ := parseStream(t, "S1", `{"type":"turn.started"}
{"type":"item.started","item":{"id":"a1","type":"agent_message","text":"par"}}
{"type":"item.completed","item":{"id":"a1","type":"agent_message","text":"whole"}}
{"type":"item.completed","item":{"id":"a1","type":"agent_message","text":"whole"}}
{"type":"item.completed","item":{"id":"a2","type":"agent_message","text":"whole"}}
{"type":"turn.completed","usage":{}}
`)
	if stream.text != "whole" {
		t.Fatalf("text = %q", stream.text)
	}
	var texts []string
	for _, event := range events {
		if event.Kind == runner.EventText {
			texts = append(texts, event.Text)
		}
	}
	if !reflect.DeepEqual(texts, []string{"whole"}) {
		t.Fatalf("text events = %q", texts)
	}
}

func TestToolItemsWithoutOptionalFieldsAndStructuredErrors(t *testing.T) {
	stream, events, _ := parseStream(t, "S1", `{"type":"turn.started"}
{"type":"item.completed","item":{"type":"command_execution"}}
{"type":"item.completed","item":{"type":"file_change"}}
{"type":"item.completed","item":{"type":"mcp_tool_call","server":"srv"}}
{"type":"item.completed","item":{"type":"mcp_tool_call","tool":"tool"}}
{"type":"error","message":"retry failed"}
{"type":"turn.failed","error":{"message":"model stopped"}}
`)
	if stream.terminalComplete || !strings.Contains(stream.diagnostic(), "retry failed") || !strings.Contains(stream.diagnostic(), "model stopped") {
		t.Fatalf("stream diagnostics = %q, complete = %v", stream.diagnostic(), stream.terminalComplete)
	}
	want := []eventSummary{
		{runner.EventToolResult, "shell", ""}, {runner.EventToolResult, "apply_patch", ""},
		{runner.EventToolResult, "srv", ""}, {runner.EventToolResult, "tool", ""},
		{runner.EventError, "", "retry failed"}, {runner.EventError, "", "model stopped"},
	}
	if got := summarize(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestFailedToolDenialsAreNativeAndDeduplicated(t *testing.T) {
	stream, _, _ := parseStream(t, "S1", `{"type":"turn.started"}
{"type":"item.completed","item":{"id":"c1","type":"command_execution","status":"failed","error":{"code":"sandbox_denied","message":"permission denied by sandbox"}}}
{"type":"item.completed","item":{"id":"c2","type":"command_execution","status":"failed","error":{"code":"approval_denied"}}}
{"type":"item.completed","item":{"id":"f1","type":"file_change","status":"failed","error":{"type":"permission_denied"}}}
{"type":"item.completed","item":{"id":"m1","type":"mcp_tool_call","server":"beads","tool":"show","status":"failed","error":{"message":"approval rejected"}}}
{"type":"item.completed","item":{"id":"a1","type":"agent_message","text":"recovered after the denials"}}
{"type":"turn.completed","usage":{}}
`)
	want := []string{"shell", "apply_patch", "beads/show"}
	if !reflect.DeepEqual(stream.denials, want) {
		t.Fatalf("denials = %q, want %q", stream.denials, want)
	}
	if !stream.terminalComplete {
		t.Fatal("a recovered denial prevented successful completion")
	}
}

func TestRunRequiresTerminalCompletedTurn(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Bin: fakeCLI(t, `cat >/dev/null; printf '%s\n' '{"type":"thread.started","thread_id":"thread"}' '{"type":"turn.started"}'`)}
	res, err := r.Run(context.Background(), runner.Request{Prompt: "go", Dir: dir, LogPath: filepath.Join(dir, "raw.jsonl")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != runner.ClassInfraFailed || res.Err == nil || !strings.Contains(res.Err.Error(), "without a completed turn") {
		t.Fatalf("result = %+v", res)
	}
}

func TestMalformedLineBeforeCompletionDoesNotFailRun(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Bin: fakeCLI(t, `cat >/dev/null; printf '%s\n' 'broken {' '{"type":"thread.started","thread_id":"thread"}' '{"type":"turn.completed","usage":{}}'`)}
	res, err := r.Run(context.Background(), runner.Request{Prompt: "go", Dir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != runner.ClassOK || res.Err != nil || res.Usage.Turns != 1 {
		t.Fatalf("result = %+v", res)
	}
}

type eventSummary struct {
	kind runner.EventKind
	tool string
	text string
}

func summarize(events []runner.Event) []eventSummary {
	out := make([]eventSummary, 0, len(events))
	for _, event := range events {
		out = append(out, eventSummary{event.Kind, event.Tool, event.Text})
	}
	return out
}

func parseStream(t *testing.T, sessionID, input string) (*invocationStream, []runner.Event, string) {
	t.Helper()
	var events []runner.Event
	var transcript bytes.Buffer
	stream := newInvocationStream(runner.RoleWorker, sessionID, runner.SinkFunc(func(event runner.Event) {
		events = append(events, event)
	}), &transcript)
	if err := stream.consume(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	return stream, events, transcript.String()
}
