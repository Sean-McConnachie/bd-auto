package ask

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// client drives ServeMCP over a pipe, the way a backend does.
type client struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	done chan error
	seq  int
}

func newClient(t *testing.T, h Handler) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() {
		err := ServeMCP(context.Background(), inR, outW, h)
		outW.Close()
		c.done <- err
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			t.Error("the server did not stop when its input closed")
		}
	})
	return c
}

// send writes a request and returns its id, or writes a notification.
func (c *client) send(method string, params any, notify bool) json.RawMessage {
	c.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	var id json.RawMessage
	if !notify {
		c.seq++
		id = json.RawMessage(itoa(c.seq))
		msg["id"] = c.seq
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.in.Write(append(raw, '\n')); err != nil {
		c.t.Fatal(err)
	}
	return id
}

// read takes the next response off the wire.
func (c *client) read() map[string]any {
	c.t.Helper()
	line, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(line, &out); err != nil {
		c.t.Fatalf("the server wrote something that is not a message: %q", line)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func okHandler(text string) Handler {
	return func(context.Context, Call) (string, error) { return text, nil }
}

// The handshake is the whole of what a backend needs before it will call
// anything, so a mistake here is a tool that silently never exists.
func TestInitializeAndListTools(t *testing.T) {
	c := newClient(t, okHandler("fine"))

	c.send("initialize", map[string]any{"protocolVersion": "2025-06-18"}, false)
	res := c.read()
	result, _ := res["result"].(map[string]any)
	if result == nil {
		t.Fatalf("initialize returned %v", res)
	}
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("the server did not echo the client's protocol: %v", result["protocolVersion"])
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("the server does not advertise tools: %v", caps)
	}

	c.send("notifications/initialized", nil, true)

	c.send("tools/list", nil, false)
	listed, _ := c.read()["result"].(map[string]any)
	tools, _ := listed["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("listed %d tool(s), want 2", len(tools))
	}
	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
		// A tool with no description is a tool a model will not reach for, and
		// a schema is what makes the call well-formed.
		if desc, _ := tool["description"].(string); len(desc) < 40 {
			t.Fatalf("%s has no useful description: %q", name, desc)
		}
		if _, ok := tool["inputSchema"].(map[string]any); !ok {
			t.Fatalf("%s has no input schema", name)
		}
	}
	if !names[ToolAsk] || !names[ToolWait] {
		t.Fatalf("the wrong tools are listed: %v", names)
	}
}

// A client that asks for a protocol this server has never heard of gets the
// server's own, rather than an echo that promises something it cannot do.
func TestUnknownProtocolFallsBack(t *testing.T) {
	c := newClient(t, okHandler("fine"))
	c.send("initialize", map[string]any{"protocolVersion": "1999-01-01"}, false)
	result, _ := c.read()["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Fatalf("got %v, want %s", result["protocolVersion"], ProtocolVersion)
	}
}

// The call the whole package exists for: arguments in, answer out.
func TestToolCallCarriesTheArgumentsAndTheAnswer(t *testing.T) {
	var got Call
	c := newClient(t, func(_ context.Context, call Call) (string, error) {
		got = call
		return "the second one", nil
	})

	c.send("tools/call", map[string]any{
		"name": ToolAsk,
		"arguments": map[string]any{
			"question": "which one?",
			"header":   "Config key",
			"options": []any{
				map[string]any{"label": "a", "description": "the first"},
				map[string]any{"label": "b"},
			},
		},
	}, false)

	result, _ := c.read()["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("the call came back as an error: %v", result)
	}
	if text := resultText(t, result); text != "the second one" {
		t.Fatalf("the answer did not reach the model: %q", text)
	}
	if got.Tool != ToolAsk || got.Question != "which one?" || got.Header != "Config key" {
		t.Fatalf("the handler was given %+v", got)
	}
	if len(got.Options) != 2 || got.Options[0].Label != "a" || got.Options[0].Description != "the first" {
		t.Fatalf("the options did not survive decoding: %+v", got.Options)
	}
}

// A failing tool must not break the session: the model has to be able to read
// what went wrong and carry on.
func TestAFailingToolIsAToolResultNotAProtocolError(t *testing.T) {
	c := newClient(t, func(context.Context, Call) (string, error) {
		return "", errAsk("the drain is gone")
	})
	c.send("tools/call", map[string]any{"name": ToolAsk, "arguments": map[string]any{"question": "?"}}, false)

	res := c.read()
	if res["error"] != nil {
		t.Fatalf("a tool failure became a protocol error: %v", res["error"])
	}
	result, _ := res["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("the failure is not marked as one: %v", result)
	}
	if text := resultText(t, result); !strings.Contains(text, "the drain is gone") {
		t.Fatalf("the reason did not reach the model: %q", text)
	}
}

// A tool call here blocks for minutes by design. If it were handled on the read
// loop the server would stop answering everything else for the whole hold,
// which a client reads as a dead server.
func TestALongToolCallDoesNotStallTheServer(t *testing.T) {
	release := make(chan struct{})
	c := newClient(t, func(ctx context.Context, _ Call) (string, error) {
		<-release
		return "eventually", nil
	})

	c.send("tools/call", map[string]any{"name": ToolAsk, "arguments": map[string]any{"question": "?"}}, false)
	c.send("ping", nil, false)

	// The ping must come back while the tool call is still in flight.
	pong := c.read()
	if pong["error"] != nil {
		t.Fatalf("ping failed: %v", pong["error"])
	}
	if id, _ := pong["id"].(float64); int(id) != 2 {
		t.Fatalf("the first reply was to request %v, so the tool call blocked the loop", pong["id"])
	}

	close(release)
	answered := c.read()
	result, _ := answered["result"].(map[string]any)
	if text := resultText(t, result); text != "eventually" {
		t.Fatalf("got %q", text)
	}
}

// A notification has no id, and answering one is a protocol error. The only way
// to see that it was not answered is to send something after it and check what
// comes back first.
func TestNotificationsAreNotAnswered(t *testing.T) {
	c := newClient(t, okHandler("fine"))
	c.send("notifications/initialized", nil, true)
	c.send("ping", nil, false)

	res := c.read()
	if res["error"] != nil {
		t.Fatalf("ping failed: %v", res["error"])
	}
	if _, ok := res["id"]; !ok {
		t.Fatalf("the first message back had no id, so the notification was answered: %v", res)
	}
}

func TestUnknownMethodIsRefused(t *testing.T) {
	c := newClient(t, okHandler("fine"))
	c.send("resources/subscribe", nil, false)
	res := c.read()
	rpcErr, _ := res["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("an unknown method was accepted: %v", res)
	}
	if code, _ := rpcErr["code"].(float64); int(code) != codeMethodMissing {
		t.Fatalf("code is %v", rpcErr["code"])
	}
}

func resultText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in %v", result)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

type errAsk string

func (e errAsk) Error() string { return string(e) }
