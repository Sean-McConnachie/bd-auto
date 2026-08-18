package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"bd-auto/internal/runner"
)

// This file turns `--output-format stream-json --verbose
// --include-partial-messages` into runner.Events.
//
// The CLI emits one JSON object per line. Three shapes matter:
//
//	{"type":"system","subtype":"init","session_id":...}
//	{"type":"stream_event","event":{...}}        // only with --include-partial-messages
//	{"type":"assistant"|"user","message":{...}}  // whole messages
//	{"type":"result",...}                        // exactly one, last
//
// The partial events are what make the TUI's activity line text-granular rather
// than tool-call-granular, and they are also why this parser tracks whether it
// has seen any: text and tool_use arrive twice when partials are on, once as
// deltas and again in the completed message. Emitting both would double every
// line of a worker's output on screen.

// apiUsage is the usage block, which appears on assistant messages and again on
// the result.
type apiUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u *apiUsage) tokens() runner.Usage {
	if u == nil {
		return runner.Usage{}
	}
	return runner.Usage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheReadTokens:     u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
	}
}

// contentBlock is one block of a message, or the block a partial event opens.
type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}

type apiMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
	Usage   *apiUsage      `json:"usage"`
}

type streamDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type streamEvent struct {
	Type         string        `json:"type"`
	Delta        *streamDelta  `json:"delta"`
	ContentBlock *contentBlock `json:"content_block"`
	Message      *apiMessage   `json:"message"`
	Usage        *apiUsage     `json:"usage"`
}

// streamLine is the union of every line shape, decoded leniently: an unknown
// type is skipped rather than failing the run, because a CLI upgrade that adds
// an event kind must not break a drain.
type streamLine struct {
	Type      string       `json:"type"`
	Subtype   string       `json:"subtype"`
	SessionID string       `json:"session_id"`
	Message   *apiMessage  `json:"message"`
	Event     *streamEvent `json:"event"`

	// result only
	IsError      bool      `json:"is_error"`
	Result       string    `json:"result"`
	Error        string    `json:"error"`
	TotalCostUSD float64   `json:"total_cost_usd"`
	NumTurns     int       `json:"num_turns"`
	Usage        *apiUsage `json:"usage"`
	// PermissionDenials is the CLI's own list of tool calls it refused. It is
	// read rather than inferred from tool_result text because it is structured,
	// and because a refused run still reports subtype "success" with is_error
	// false — this array is the only place the refusal is visible at all. A CLI
	// that does not emit it leaves the field nil, which reads as no denials.
	PermissionDenials []permissionDenial `json:"permission_denials"`
	// TerminalReason and APIErrorStatus are the CLI's own account of why the
	// run ended, and they are the only reliable one. Everything else on the
	// result line describes an outage badly: a session limit arrives as
	// subtype "success" with the prose in `result`, so the subtype says the run
	// went fine, and the prose is marketing copy that changes with the product
	// ("You've hit your session limit · resets 3:20pm"). Matching that prose is
	// guesswork; these two fields are the CLI stating that the API call failed
	// and with what status, which is a fact. Absent on a CLI that does not emit
	// them, which reads as no API error. See classify.
	TerminalReason string `json:"terminal_reason"`
	APIErrorStatus int    `json:"api_error_status"`
}

// permissionDenial is one entry of the result line's permission_denials array.
// Only the tool name is kept: the inputs are the worker's own arguments, which
// belong in the transcript rather than in a reason string.
type permissionDenial struct {
	ToolName string `json:"tool_name"`
}

// parser accumulates the state the Result needs while emitting live events.
type parser struct {
	role      runner.Role
	sink      runner.EventSink
	sessionID string
	log       io.Writer

	// sawPartial records that --include-partial-messages is in effect, so
	// completed messages are used for state only and not re-emitted.
	sawPartial bool
	// toolNames maps a tool_use id to its name, so a tool_result can say which
	// tool it came back from.
	toolNames map[string]string

	message   strings.Builder
	lastText  string
	finalText string

	usage     runner.Usage
	sawResult bool
	resultErr bool
	subtype   string
	errText   string
	// terminalReason and apiErrorStatus are the result line's structured
	// account of an outage. See streamLine.
	terminalReason string
	apiErrorStatus int
	// denied is the tool names the CLI refused, deduplicated and in the order
	// they were first refused.
	denied []string
	// bad counts lines that were not JSON at all.
	bad int
}

func newParser(role runner.Role, sessionID string, sink runner.EventSink, log io.Writer) *parser {
	return &parser{role: role, sessionID: sessionID, sink: sink, log: log, toolNames: map[string]string{}}
}

// consume reads the CLI's stdout to EOF. A line that will not parse is counted
// and skipped; only a read failure comes back as an error.
func (p *parser) consume(r io.Reader) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			p.line(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (p *parser) line(raw []byte) {
	if p.log != nil {
		_, _ = p.log.Write(raw)
		if raw[len(raw)-1] != '\n' {
			_, _ = p.log.Write([]byte{'\n'})
		}
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return
	}
	var l streamLine
	if err := json.Unmarshal([]byte(trimmed), &l); err != nil {
		p.bad++
		return
	}
	if l.SessionID != "" {
		p.sessionID = l.SessionID
	}
	switch l.Type {
	case "stream_event":
		p.partial(l.Event)
	case "assistant":
		p.assistant(l.Message)
	case "user":
		p.user(l.Message)
	case "result":
		p.result(l)
	}
}

// partial handles one --include-partial-messages event.
func (p *parser) partial(e *streamEvent) {
	if e == nil {
		return
	}
	p.sawPartial = true
	switch e.Type {
	case "message_start":
		p.message.Reset()
	case "content_block_start":
		if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			p.toolUse(e.ContentBlock)
		}
	case "content_block_delta":
		if e.Delta != nil && e.Delta.Type == "text_delta" && e.Delta.Text != "" {
			p.message.WriteString(e.Delta.Text)
			p.emit(runner.Event{Kind: runner.EventText, Text: e.Delta.Text})
		}
	case "message_stop":
		p.endMessage()
	}
}

func (p *parser) assistant(m *apiMessage) {
	if m == nil {
		return
	}
	if m.Usage != nil {
		p.usage = p.usage.Add(m.Usage.tokens())
		p.emit(runner.Event{Kind: runner.EventUsage, Usage: p.usage})
	}
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			// With partials on, this text has already been emitted delta by
			// delta and accumulated; only a non-streaming run needs it here.
			if !p.sawPartial {
				p.message.WriteString(b.Text)
				p.emit(runner.Event{Kind: runner.EventText, Text: b.Text})
			}
		case "tool_use":
			if b.ID != "" && b.Name != "" {
				p.toolNames[b.ID] = b.Name
			}
			if !p.sawPartial {
				p.emit(runner.Event{Kind: runner.EventToolUse, Tool: b.Name})
			}
		}
	}
	p.endMessage()
}

// user carries tool results back into the transcript. Their content is often
// enormous, so only the tool name is reported: the transcript on disk has the
// rest.
func (p *parser) user(m *apiMessage) {
	if m == nil {
		return
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" {
			continue
		}
		p.emit(runner.Event{Kind: runner.EventToolResult, Tool: p.toolNames[b.ToolUseID]})
	}
}

func (p *parser) toolUse(b *contentBlock) {
	if b.ID != "" && b.Name != "" {
		p.toolNames[b.ID] = b.Name
	}
	p.emit(runner.Event{Kind: runner.EventToolUse, Tool: b.Name})
}

// endMessage keeps the most recent assistant text, which is the fallback final
// message for a run that ends without a result line.
func (p *parser) endMessage() {
	if p.message.Len() > 0 {
		p.lastText = p.message.String()
		p.message.Reset()
	}
}

func (p *parser) result(l streamLine) {
	p.sawResult = true
	p.subtype = l.Subtype
	p.resultErr = l.IsError || (l.Subtype != "" && l.Subtype != "success")
	p.finalText = l.Result
	p.errText = l.Error
	p.terminalReason = l.TerminalReason
	p.apiErrorStatus = l.APIErrorStatus
	p.denials(l.PermissionDenials)
	// The result's usage is the run total and is the only place cost appears,
	// so it replaces whatever was summed from individual messages rather than
	// adding to it.
	if l.Usage != nil {
		p.usage = l.Usage.tokens()
	}
	p.usage.CostUSD = l.TotalCostUSD
	p.usage.Turns = l.NumTurns
	p.emit(runner.Event{Kind: runner.EventUsage, Usage: p.usage})
}

// denials records the tools the CLI refused. A tool refused ten times is one
// entry: what the engine acts on is which tools were unavailable, not how many
// times the model asked.
func (p *parser) denials(in []permissionDenial) {
	seen := map[string]bool{}
	for _, d := range p.denied {
		seen[d] = true
	}
	for _, d := range in {
		name := strings.TrimSpace(d.ToolName)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		p.denied = append(p.denied, name)
	}
}

// text is the final assistant message: the result line's own text where there
// is one, and the last complete assistant message otherwise.
func (p *parser) text() string {
	if strings.TrimSpace(p.finalText) != "" {
		return p.finalText
	}
	p.endMessage()
	return p.lastText
}

// failText is everything the CLI said about a failure, for classification and
// for the error on the Result.
//
// A subtype of "success" is dropped rather than led with. It is the subtype an
// outage arrives under — a session limit reports subtype "success" with the
// refusal in `result` — so keeping it produces "exit 1: success: You've hit your
// session limit", which is the log line that sent a human looking for a
// deferred issue for an afternoon. Every other subtype names the failure and is
// kept.
func (p *parser) failText() string {
	parts := make([]string, 0, 3)
	for _, s := range []string{p.subtype, p.errText, p.finalText} {
		if strings.TrimSpace(s) == "" {
			continue
		}
		if s == p.subtype && strings.EqualFold(strings.TrimSpace(s), "success") {
			continue
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ": ")
}

func (p *parser) emit(e runner.Event) {
	e.Role = p.role
	e.SessionID = p.sessionID
	if e.At.IsZero() {
		e.At = time.Now()
	}
	runner.Emit(p.sink, e)
}
