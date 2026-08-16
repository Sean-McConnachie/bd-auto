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
