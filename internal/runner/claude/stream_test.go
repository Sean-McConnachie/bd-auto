package claude

import (
	"reflect"
	"strings"
	"testing"

	"bd-auto/internal/runner"
)

func feed(t *testing.T, sessionID, lines string) (*parser, []runner.Event) {
	t.Helper()
	var events []runner.Event
	p := newParser(runner.RoleWorker, sessionID, collect(&events), nil)
	if err := p.consume(strings.NewReader(lines)); err != nil {
		t.Fatalf("consume: %v", err)
	}
	return p, events
}

// Without --include-partial-messages the completed assistant messages are the
// only text there is, so they must be emitted. This is the degraded display
// path, and it has to stay working: the flag is the difference between a
// text-granular activity line and a tool-call-granular one, not between output
// and none.
func TestParseWithoutPartials(t *testing.T) {
	p, events := feed(t, "S1", `{"type":"assistant","session_id":"S1","message":{"role":"assistant","content":[{"type":"text","text":"reading the issue"},{"type":"tool_use","id":"tu_1","name":"Bash"}]}}
{"type":"user","session_id":"S1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}
{"type":"result","subtype":"success","session_id":"S1","result":"finished","total_cost_usd":0.01}
`)
	got := summarise(events)
	want := []string{"text:reading the issue", "tool-use:Bash", "tool-result:Bash", "usage"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events\n got %q\nwant %q", got, want)
	}
	if p.text() != "finished" {
		t.Errorf("text = %q, want %q", p.text(), "finished")
	}
}

// A run that ends without a result line still has a final message, and it is
// the last complete assistant message. Losing it would leave the engine with a
// class and nothing to log or feed back.
func TestParseFallsBackToLastMessage(t *testing.T) {
	p, _ := feed(t, "S1", `{"type":"assistant","session_id":"S1","message":{"role":"assistant","content":[{"type":"text","text":"first"}]}}
{"type":"assistant","session_id":"S1","message":{"role":"assistant","content":[{"type":"text","text":"second and last"}]}}
`)
	if p.sawResult {
		t.Error("sawResult is true with no result line")
	}
	if p.text() != "second and last" {
		t.Errorf("text = %q, want the last assistant message", p.text())
	}
}

// The CLI reports the session it actually ran, which is what gets echoed back
// on the Result for the next round to resume.
func TestParseAdoptsSessionID(t *testing.T) {
	p, events := feed(t, "", `{"type":"system","subtype":"init","session_id":"generated-by-the-cli"}
{"type":"result","subtype":"success","session_id":"generated-by-the-cli","result":"done"}
`)
	if p.sessionID != "generated-by-the-cli" {
		t.Errorf("sessionID = %q", p.sessionID)
	}
	for _, e := range events {
		if e.SessionID != "generated-by-the-cli" {
			t.Errorf("event %s carries session %q", e.Kind, e.SessionID)
		}
	}
}

// An error result carries its subtype and message into one string, because
// that is what both the classifier and the human reading the log need.
func TestParseFailText(t *testing.T) {
	p, _ := feed(t, "S1", `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"S1","error":"API Error: 429","result":""}
`)
	if !p.resultErr {
		t.Error("resultErr = false for an is_error result")
	}
	if got := p.failText(); !strings.Contains(got, "error_during_execution") || !strings.Contains(got, "429") {
		t.Errorf("failText = %q, want both the subtype and the message", got)
	}
	if !infraSignal(p.failText()) {
		t.Error("a 429 in the result line must read as an infrastructure failure")
	}
}

// A refused run reports itself as a success, which is the whole reason denials
// have to be read off the result line rather than inferred from the class.
//
// The line below is the shape claude 2.1.233 emitted for a worker that was
// refused a Write under --permission-mode auto in one of this repo's own worker
// worktrees: exit 0, subtype success, is_error false, and the refusal visible
// nowhere but permission_denials.
func TestParseRecordsPermissionDenials(t *testing.T) {
	p, _ := feed(t, "S1", `{"type":"result","subtype":"success","is_error":false,"session_id":"S1","result":"I've requested permission to write hello.txt. Please approve the write when ready.","total_cost_usd":0.01,"permission_denials":[{"tool_name":"Write","tool_use_id":"tu_1","tool_input":{"file_path":"/w/hello.txt"}},{"tool_name":"Write","tool_use_id":"tu_2","tool_input":{"file_path":"/w/hello.txt"}},{"tool_name":"Bash","tool_use_id":"tu_3","tool_input":{"command":"touch hello.txt"}}]}
`)
	if p.resultErr {
		t.Error("resultErr = true; a refused run still reports success, and that is the point")
	}
	if got, want := p.denied, []string{"Write", "Bash"}; !reflect.DeepEqual(got, want) {
		t.Errorf("denied = %v, want %v deduplicated and in refusal order", got, want)
	}
	if classify(outcome{exitCode: 0, sawResult: true}) != runner.ClassOK {
		t.Error("the class must stay ok: being refused one tool is not by itself a failed run")
	}
}

// A CLI that reports no denials, or one too old to report them at all, must
// leave the field empty rather than something the engine reads as a refusal.
func TestParseWithoutDenials(t *testing.T) {
	p, _ := feed(t, "S1", `{"type":"result","subtype":"success","session_id":"S1","result":"done","permission_denials":[]}
{"type":"result","subtype":"success","session_id":"S1","result":"done"}
`)
	if len(p.denied) != 0 {
		t.Errorf("denied = %v, want none", p.denied)
	}
}

// A truncated stream is what a killed process leaves behind. It must not panic
// or hang, and whatever arrived before the kill must survive.
func TestParseTruncatedStream(t *testing.T) {
	p, events := feed(t, "S1", `{"type":"stream_event","session_id":"S1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"half a th`)
	if p.sawResult {
		t.Error("sawResult is true for a truncated stream")
	}
	if len(events) != 0 {
		t.Errorf("events = %q, want none from an unterminated line", summarise(events))
	}
	if p.bad != 1 {
		t.Errorf("bad = %d, want the unparseable line counted", p.bad)
	}
}

// num_turns is on the result line beside the cost, and it is the only figure
// there that says how much work the process did rather than what it billed.
func TestTheResultLineCarriesItsTurnCount(t *testing.T) {
	p, _ := feed(t, "S1", `{"type":"result","subtype":"success","session_id":"S1","result":"done","total_cost_usd":0.25,"num_turns":11,"usage":{"input_tokens":30,"output_tokens":900}}`)
	if p.usage.Turns != 11 {
		t.Fatalf("turns = %d, want 11", p.usage.Turns)
	}
	if p.usage.CostUSD != 0.25 || p.usage.OutputTokens != 900 {
		t.Fatalf("the turn count displaced the rest of the usage: %+v", p.usage)
	}
	if (runner.Usage{}).Add(p.usage).Add(p.usage).Turns != 22 {
		t.Fatal("turns must sum across processes like everything else in Usage")
	}
}
