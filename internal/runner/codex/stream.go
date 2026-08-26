package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"bd-auto/internal/runner"
)

// streamLine is the forward-compatible envelope emitted by `codex exec
// --json`. Unknown event and item types are deliberately ignored.
type streamLine struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     json.RawMessage `json:"item"`
	Usage    *codexUsage     `json:"usage"`
	Message  string          `json:"message"`
	Error    json.RawMessage `json:"error"`
}

type codexUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

func (u *codexUsage) tokens() runner.Usage {
	if u == nil {
		return runner.Usage{Turns: 1}
	}
	input := u.InputTokens - u.CachedInputTokens
	if input < 0 {
		input = 0
	}
	return runner.Usage{
		InputTokens:     input,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CachedInputTokens,
		Turns:           1,
	}
}

type streamItem struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Command string          `json:"command"`
	Server  string          `json:"server"`
	Tool    string          `json:"tool"`
	Status  string          `json:"status"`
	Error   json.RawMessage `json:"error"`
}

// invocationStream accumulates the semantic result while preserving every
// byte received from stdout in the transcript.
type invocationStream struct {
	role runner.Role
	sink runner.EventSink
	log  io.Writer

	sessionID        string
	text             string
	usage            runner.Usage
	terminalComplete bool
	terminalFailed   bool
	worked           bool
	completedAt      time.Time
	badLines         int
	lastBadLine      string
	errors           []string
	failures         []errorInfo
	denials          []string
	denialSet        map[string]bool
	emittedText      map[string]bool
}

func newInvocationStream(role runner.Role, sessionID string, sink runner.EventSink, log io.Writer) *invocationStream {
	return &invocationStream{
		role: role, sink: sink, log: log, sessionID: sessionID,
		emittedText: map[string]bool{}, denialSet: map[string]bool{},
	}
}

func (s *invocationStream) consume(input io.Reader) error {
	reader := bufio.NewReaderSize(input, 64<<10)
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			s.line(raw)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *invocationStream) line(raw []byte) {
	if s.log != nil {
		_, _ = s.log.Write(raw)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return
	}
	var line streamLine
	if err := json.Unmarshal(trimmed, &line); err != nil {
		s.badLines++
		s.lastBadLine = string(trimmed)
		return
	}

	switch line.Type {
	case "thread.started":
		if line.ThreadID != "" {
			s.sessionID = line.ThreadID
		}
	case "turn.started":
		s.terminalComplete = false
		s.terminalFailed = false
	case "item.started":
		s.item(line.Item, false)
	case "item.completed":
		s.item(line.Item, true)
	case "turn.completed":
		s.terminalComplete = true
		s.terminalFailed = false
		s.completedAt = time.Now()
		s.usage = s.usage.Add(line.Usage.tokens())
		s.emit(runner.Event{Kind: runner.EventUsage, Usage: s.usage})
	case "turn.failed":
		s.terminalComplete = false
		s.terminalFailed = true
		s.recordFailure(line.Message, line.Error)
	case "error":
		s.recordFailure(line.Message, line.Error)
	}
}

func (s *invocationStream) item(raw json.RawMessage, completed bool) {
	if len(raw) == 0 {
		return
	}
	var item streamItem
	if json.Unmarshal(raw, &item) != nil {
		return
	}
	tool := itemTool(item)
	if item.Type == "reasoning" || item.Type == "agent_message" || tool != "" {
		s.worked = true
	}
	if tool != "" {
		kind := runner.EventToolUse
		if completed {
			kind = runner.EventToolResult
		}
		s.emit(runner.Event{Kind: kind, Tool: tool})
	}
	if completed && item.Type == "agent_message" {
		s.text = item.Text
		if item.Text != "" && !s.emittedText[item.Text] {
			s.emittedText[item.Text] = true
			s.emit(runner.Event{Kind: runner.EventText, Text: item.Text})
		}
	}
	if completed && strings.EqualFold(item.Status, "failed") {
		s.recordError(errorText("", item.Error))
		if tool != "" && denialError(item.Error) {
			s.recordDenial(tool)
		}
	}
}

func (s *invocationStream) recordFailure(message string, raw json.RawMessage) {
	info := parseErrorInfo(message, raw)
	s.failures = append(s.failures, info)
	s.recordError(info.Text)
}

func (s *invocationStream) recordDenial(tool string) {
	if tool = strings.TrimSpace(tool); tool == "" || s.denialSet[tool] {
		return
	}
	s.denialSet[tool] = true
	s.denials = append(s.denials, tool)
}

func itemTool(item streamItem) string {
	switch item.Type {
	case "command_execution":
		return "shell"
	case "file_change":
		return "apply_patch"
	case "mcp_tool_call":
		server, tool := strings.TrimSpace(item.Server), strings.TrimSpace(item.Tool)
		switch {
		case server != "" && tool != "":
			return server + "/" + tool
		case server != "":
			return server
		default:
			return tool
		}
	default:
		return ""
	}
}

func errorText(message string, raw json.RawMessage) string {
	if text := strings.TrimSpace(message); text != "" {
		return text
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var detail struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &detail) == nil && strings.TrimSpace(detail.Message) != "" {
		return strings.TrimSpace(detail.Message)
	}
	return string(bytes.TrimSpace(raw))
}

func (s *invocationStream) recordError(text string) {
	if text = strings.TrimSpace(text); text == "" {
		return
	}
	s.errors = append(s.errors, text)
	s.emit(runner.Event{Kind: runner.EventError, Text: text})
}

func (s *invocationStream) diagnostic() string {
	parts := append([]string(nil), s.errors...)
	if s.badLines > 0 {
		parts = append(parts, "malformed JSONL lines: "+strconv.Itoa(s.badLines)+" (last: "+s.lastBadLine+")")
	}
	return strings.Join(parts, ": ")
}

func (s *invocationStream) emit(event runner.Event) {
	event.Role = s.role
	event.SessionID = s.sessionID
	if event.At.IsZero() {
		event.At = time.Now()
	}
	runner.Emit(s.sink, event)
}
