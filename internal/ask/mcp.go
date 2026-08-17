package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// This file is the whole MCP surface bd-auto speaks: JSON-RPC 2.0 over stdio,
// five methods, two tools.
//
// It is hand-written rather than taken from an SDK because what it has to do is
// small and fixed — a backend spawns it, lists two tools and calls them — and
// because the alternative is a dependency in the one process that must start
// instantly and print nothing to stdout but protocol. Everything here is the
// spec's shape, not a convenience layer over it.

// ProtocolVersion is what the server speaks when the client asks for something
// it does not recognise. A client that names a version this server knows gets
// its own back, which is what the spec asks for.
const ProtocolVersion = "2025-06-18"

// knownProtocols are the versions this server will echo back to a client that
// asks for them.
var knownProtocols = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// Call is one tool invocation, already decoded.
type Call struct {
	// Tool is the unqualified tool name: ToolAsk or ToolWait.
	Tool string
	// Header, Question and Options are ToolAsk's arguments.
	Header   string
	Question string
	Options  []Option
	// Ticket is ToolWait's argument.
	Ticket string
}

// Handler answers one tool call. A returned error becomes a tool result marked
// as an error rather than a protocol error, because a tool that failed is
// something the model should read and carry on from, not something that should
// break the session.
type Handler func(ctx context.Context, c Call) (string, error)

// ServeMCP speaks MCP over in and out until in closes.
//
// Tool calls are dispatched on their own goroutines. That is not for
// throughput: a tool call here blocks for minutes by design, and a server that
// handled it on the read loop would stop answering the client's pings for the
// whole hold, which reads as a dead server.
func ServeMCP(ctx context.Context, in io.Reader, out io.Writer, h Handler) error {
	s := &mcpServer{h: h, enc: json.NewEncoder(out)}
	dec := json.NewDecoder(in)

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return fmt.Errorf("ask: read: %w", err)
		}
		if req.Method == "tools/call" {
			wg.Add(1)
			go func(req rpcRequest) {
				defer wg.Done()
				s.dispatch(ctx, req)
			}(req)
			continue
		}
		s.dispatch(ctx, req)
	}
}

// --- JSON-RPC ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// notification reports whether a request wants no answer. A request with no id
// is a notification, and answering one is a protocol error.
func (r rpcRequest) notification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// The JSON-RPC error codes this server can produce.
const (
	codeInvalidParams = -32602
	codeMethodMissing = -32601
)

type mcpServer struct {
	h Handler

	mu  sync.Mutex
	enc *json.Encoder
}

// send writes one message. The lock is what makes a tool call answered on its
// own goroutine safe to interleave with the read loop's replies.
func (s *mcpServer) send(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(v)
}

func (s *mcpServer) reply(req rpcRequest, result any) {
	if req.notification() {
		return
	}
	s.send(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *mcpServer) fail(req rpcRequest, code int, msg string) {
	if req.notification() {
		return
	}
	s.send(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}})
}

func (s *mcpServer) dispatch(ctx context.Context, req rpcRequest) {
	switch req.Method {
	case "initialize":
		s.reply(req, initializeResult(req.Params))
	case "notifications/initialized", "notifications/cancelled":
		// Nothing to do, and nothing to answer.
	case "ping":
		s.reply(req, map[string]any{})
	case "tools/list":
		s.reply(req, map[string]any{"tools": toolDescriptors()})
	case "resources/list":
		s.reply(req, map[string]any{"resources": []any{}})
	case "prompts/list":
		s.reply(req, map[string]any{"prompts": []any{}})
	case "tools/call":
		s.callTool(ctx, req)
	default:
		s.fail(req, codeMethodMissing, "unknown method "+req.Method)
	}
}

func initializeResult(params json.RawMessage) map[string]any {
	version := ProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &p); err == nil && knownProtocols[p.ProtocolVersion] {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": ServerName, "version": "1"},
	}
}

// callParams is a tools/call request. The arguments are decoded into one struct
// covering both tools, because there are two of them and a type switch on a
// name would be more machinery than the thing it dispatches.
type callParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Header   string   `json:"header"`
		Question string   `json:"question"`
		Options  []Option `json:"options"`
		Ticket   string   `json:"ticket"`
	} `json:"arguments"`
}

func (s *mcpServer) callTool(ctx context.Context, req rpcRequest) {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.fail(req, codeInvalidParams, "tools/call: "+err.Error())
		return
	}

	name := strings.TrimPrefix(p.Name, ServerName+"__")
	call := Call{
		Tool:     name,
		Header:   p.Arguments.Header,
		Question: p.Arguments.Question,
		Options:  p.Arguments.Options,
		Ticket:   strings.TrimSpace(p.Arguments.Ticket),
	}

	switch name {
	case ToolAsk, ToolWait:
	default:
		s.reply(req, toolResult("unknown tool "+p.Name, true))
		return
	}

	text, err := s.h(ctx, call)
	if err != nil {
		s.reply(req, toolResult(err.Error(), true))
		return
	}
	s.reply(req, toolResult(text, false))
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}

// toolDescriptors is what tools/list returns.
//
// The wording of the descriptions is the whole interface: a model decides
// whether to ask from these two paragraphs and nothing else, so they say when
// to reach for the tool and — just as importantly — when not to.
func toolDescriptors() []any {
	return []any{
		map[string]any{
			"name": ToolAsk,
			"description": "Ask the human running this drain a question and wait for their answer, " +
				"without ending your session.\n\n" +
				"Use it for a genuine ambiguity you cannot settle from the issue, the code or the " +
				"repo's conventions — a decision that is the human's to make and that you would " +
				"otherwise have to guess at. Do not use it for anything you can find out by reading, " +
				"for permission to do the work you were given, or to report progress.\n\n" +
				"Offer 2-4 concrete options wherever you can: an answerable question gets answered, " +
				"an open one usually does not. The human may also reply in their own words, or hand " +
				"the decision back to you.\n\n" +
				"If nobody answers within a few minutes the call returns PENDING with a ticket. " +
				"That is not a failure: collect the answer with " + ToolWait + ". If nobody is " +
				"watching the run at all, you are told so immediately and should proceed on your " +
				"own judgement.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{
						"type":        "string",
						"description": "The question, in one or two sentences. State what you are deciding between and why it matters.",
					},
					"header": map[string]any{
						"type":        "string",
						"description": "Two or three words naming the decision, for the watcher's display. For example \"Config key\" or \"Error handling\".",
					},
					"options": map[string]any{
						"type":        "array",
						"description": "The answers you can see, best first. 2-4 of them.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"label":       map[string]any{"type": "string", "description": "The option, in a few words."},
								"description": map[string]any{"type": "string", "description": "What choosing it means, in one line."},
							},
							"required": []any{"label"},
						},
					},
				},
				"required": []any{"question"},
			},
		},
		map[string]any{
			"name": ToolWait,
			"description": "Collect the answer to a question " + ToolAsk + " returned a ticket for.\n\n" +
				"It waits a few minutes and returns either the answer or PENDING again. Keep calling " +
				"it until you get an answer or are told to proceed without one. Do not ask the " +
				"question again, and do not decide it yourself while it is still pending.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{
						"type":        "string",
						"description": "The ticket " + ToolAsk + " returned, exactly as given.",
					},
				},
				"required": []any{"ticket"},
			},
		},
	}
}
