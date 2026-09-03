package tui

// Reading a run's transcripts off disk.
//
// Every model bd-auto spawns writes its whole stream to
// .beads/auto/logs/<issue>-a<attempt>-r<round>-<role>.jsonl, and until now
// nothing read them back. They are the only complete account of what a worker
// did: the live event stream carries a tool's name but never its arguments, it
// starts when the view starts, and it is gone the moment the run ends. The
// files have the arguments, the earlier rounds, the earlier attempts, the
// reviewer and the integrator, and they outlive the run.
//
// What is read here is the provider-native transport each shipped adapter
// writes. Filenames do not encode a provider, so the decoder identifies Claude
// and Codex records from their envelope and keeps provider state per file.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"bd-auto/internal/drain"
)

// The bounds. Five workers streaming for an hour is more text than a view has
// any business holding, so nothing here grows without a limit: the window keeps
// the newest entries, a tool result keeps its head, and a line keeps its front.
const (
	// entryCap is how many entries one transcript keeps. Older ones are dropped
	// and counted, because what a watcher opens a transcript for is what is
	// happening now.
	entryCap = 400
	// resultLines is how much of a tool result is kept. A Read returns a whole
	// file and a test run returns a whole suite; the head of it plus an honest
	// count of the rest is what fits on a screen anyway.
	resultLines = 8
	// lineCap bounds one line, and textCap one assistant message.
	lineCap = 400
	textCap = 2000
	// argCap bounds a tool call's argument summary.
	argCap = 160
)

// entryKind is what one piece of a transcript is.
type entryKind int

const (
	// entryHead is a process boundary: which role ran, in which round of which
	// attempt. It is what keeps a worker's third round from reading as a
	// continuation of its second.
	entryHead entryKind = iota
	// entryText is assistant prose.
	entryText
	// entryTool is a tool call, and entryResult what it returned.
	entryTool
	entryResult
	// entryEnd is the process's own last word: its verdict, its turns and what
	// it cost.
	entryEnd
	// entryDiagnostic makes an existing but undecodable transcript visible.
	// It is removed if a later append produces a displayable provider record.
	entryDiagnostic
)

// entry is one thing that happened, ready to be rendered.
type entry struct {
	kind entryKind
	// tool and args are the tool's name and a one-line summary of its input,
	// on entryTool. Everything else carries its body in text.
	tool string
	args string
	text string
	// cut is how many lines of a tool result were dropped off the end.
	cut int
	// fail is a tool result that came back an error, or a process that ended
	// as one. It is what the failure colour is for.
	fail bool
	// source identifies a temporary diagnostic with its transcript file.
	source string
}

// transcript is one issue's model output, read off disk and kept to a bound.
type transcript struct {
	issue   string
	readers []*reader
	entries []entry
	// dropped is how many entries fell off the front of the window.
	dropped int
	// found is whether any transcript exists at all, which is the difference
	// between a worker that has written nothing yet and one that was never
	// dispatched.
	found bool
}

func newTranscript(issue string) *transcript { return &transcript{issue: issue} }

// refresh reads whatever has been appended since the last call, and picks up
// processes that have started since.
//
// It is called on every tick while the view is open. That is affordable because
// nothing is re-read: each file is followed from a byte offset, so a worker an
// hour into a session costs a stat and the handful of lines it wrote in the
// last half second.
func (t *transcript) refresh(repoRoot string) {
	known := make(map[string]*reader, len(t.readers))
	for _, r := range t.readers {
		known[r.file.Path] = r
	}
	for _, f := range drain.LogFiles(repoRoot, t.issue) {
		r, ok := known[f.Path]
		if !ok {
			r = &reader{file: f}
			t.readers = append(t.readers, r)
			t.add(entry{kind: entryHead, text: processHead(f)})
		}
		r.read(func(e entry) {
			if r.diagnostic {
				t.removeDiagnostic(r.file.Path)
				r.diagnostic = false
			}
			r.visible++
			t.add(e)
		})
		if r.off > 0 && r.visible == 0 && !r.diagnostic {
			message := "transcript exists, but its provider format is unsupported or no records could be decoded"
			if r.decoder.recognized {
				message = "transcript format recognized; no displayable activity has arrived yet"
			}
			t.add(entry{kind: entryDiagnostic, text: message, fail: !r.decoder.recognized, source: r.file.Path})
			r.diagnostic = true
		}
	}
	t.found = len(t.readers) > 0
}

func (t *transcript) removeDiagnostic(source string) {
	out := t.entries[:0]
	for _, e := range t.entries {
		if e.kind == entryDiagnostic && e.source == source {
			continue
		}
		out = append(out, e)
	}
	t.entries = out
}

// add appends an entry, dropping the oldest once the window is full.
func (t *transcript) add(e entry) {
	t.entries = append(t.entries, e)
	if over := len(t.entries) - entryCap; over > 0 {
		t.entries = append(t.entries[:0], t.entries[over:]...)
		t.dropped += over
	}
}

// processHead names one transcript file for the separator that opens it.
func processHead(f drain.LogFile) string {
	role := string(f.Role)
	if role == "" {
		role = "process"
	}
	if f.Dup > 1 {
		role += fmt.Sprintf(" (#%d)", f.Dup)
	}
	return fmt.Sprintf("%s · attempt %d · round %d", role, f.Attempt, f.Round)
}

// reader follows one transcript file from a byte offset.
//
// The offset rather than the file, and the offset rather than the whole
// contents, is the whole memory story here: a transcript is megabytes and grows
// for an hour, and what a refresh wants is the part nobody has read yet.
type reader struct {
	file       drain.LogFile
	off        int64
	decoder    transcriptDecoder
	visible    int
	diagnostic bool
}

// read decodes everything appended since the last call, handing each entry
// straight to add rather than returning a slice — a first read of a finished
// worker is thousands of entries, and the window is what bounds them.
//
// A line without its newline yet is left where it is. The file is being written
// by a live process, so the last line of it is regularly half a line.
func (r *reader) read(add func(entry)) {
	fi, err := os.Stat(r.file.Path)
	if err != nil {
		return
	}
	switch size := fi.Size(); {
	case size == r.off:
		return
	case size < r.off:
		// Shorter than it was: something replaced it. Start again rather than
		// decode from the middle of a line.
		r.off = 0
	}
	f, err := os.Open(r.file.Path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(r.off, io.SeekStart); err != nil {
		return
	}

	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if n := len(line); n > 0 && line[n-1] == '\n' {
			r.off += int64(n)
			r.decoder.decode(line, add)
		}
		if err != nil {
			return
		}
	}
}

// --- the transport ---

// logLine is the union of the line shapes this view reads. Everything else —
// the partial-message deltas, the status and hook lines, the rate-limit notices
// — is skipped: the deltas are the same text the completed assistant message
// carries, and rendering both would show every sentence twice.
type logLine struct {
	Type    string      `json:"type"`
	Subtype string      `json:"subtype"`
	Message *logMessage `json:"message"`

	// result only.
	IsError      bool    `json:"is_error"`
	Error        string  `json:"error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
}

// logMessage keeps its content raw. A user message's content is a list of
// blocks in every line this cares about and a bare string in some it does not,
// and a struct that insisted on the list would fail to decode the whole line.
type logMessage struct {
	Content json.RawMessage `json:"content"`
}

func (m *logMessage) blocks() []logBlock {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	var out []logBlock
	if json.Unmarshal(m.Content, &out) != nil {
		return nil
	}
	return out
}

type logBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	IsError bool            `json:"is_error"`
	Content json.RawMessage `json:"content"`
}

type transcriptDecoder struct {
	recognized bool
	started    map[string]bool
	completed  map[string]bool
}

// decodeLine is the stateless test seam for one complete provider record.
// Readers use transcriptDecoder directly so Codex started/completed pairs can
// share state across appended lines.
func decodeLine(raw []byte, add func(entry)) {
	var d transcriptDecoder
	d.decode(raw, add)
}

func (d *transcriptDecoder) decode(raw []byte, add func(entry)) {
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	switch envelope.Type {
	case "assistant", "user", "result":
		d.recognized = true
		decodeClaudeLine(raw, add)
	case "thread.started", "turn.started", "item.started", "item.completed", "turn.completed", "turn.failed", "error":
		d.recognized = true
		d.decodeCodexLine(raw, add)
	}
}

func decodeClaudeLine(raw []byte, add func(entry)) {
	var l logLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	switch l.Type {
	case "assistant":
		for _, b := range l.Message.blocks() {
			switch b.Type {
			case "text":
				// Thinking blocks land here as their own type and are left
				// out: they are the model talking to itself, and this view is
				// for the record of what it did.
				if s := oneParagraph(b.Text); s != "" {
					add(entry{kind: entryText, text: capRunes(s, textCap)})
				}
			case "tool_use":
				add(entry{kind: entryTool, tool: b.Name, args: toolArgs(b.Input)})
			}
		}
	case "user":
		for _, b := range l.Message.blocks() {
			if b.Type != "tool_result" {
				continue
			}
			text, cut := trimLines(resultText(b.Content), resultLines)
			add(entry{kind: entryResult, text: text, cut: cut, fail: b.IsError})
		}
	case "result":
		add(entry{kind: entryEnd, text: endText(l), fail: failed(l)})
	}
}

type codexTranscriptLine struct {
	Type    string          `json:"type"`
	Item    json.RawMessage `json:"item"`
	Usage   *codexUsage     `json:"usage"`
	Message string          `json:"message"`
	Error   json.RawMessage `json:"error"`
}

type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

type codexTranscriptItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Status           string          `json:"status"`
	Error            json.RawMessage `json:"error"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Query            string          `json:"query"`
	Changes          []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

func (d *transcriptDecoder) decodeCodexLine(raw []byte, add func(entry)) {
	var l codexTranscriptLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	switch l.Type {
	case "item.started", "item.completed":
		var item codexTranscriptItem
		if json.Unmarshal(l.Item, &item) != nil {
			return
		}
		d.codexItem(item, l.Type == "item.completed", add)
	case "turn.completed":
		add(entry{kind: entryEnd, text: codexEndText(l.Usage)})
	case "turn.failed", "error":
		text := codexError(l.Message, l.Error)
		if text == "" {
			text = strings.TrimSuffix(l.Type, ".failed") + " failed"
		}
		add(entry{kind: entryEnd, text: capRunes(firstLine(text), lineCap), fail: true})
	}
}

func (d *transcriptDecoder) codexItem(item codexTranscriptItem, done bool, add func(entry)) {
	if d.started == nil {
		d.started = map[string]bool{}
		d.completed = map[string]bool{}
	}
	if item.ID != "" && d.completed[item.ID] {
		return
	}
	already := item.ID != "" && d.started[item.ID]
	trackStart := item.Type == "command_execution" || item.Type == "file_change" || item.Type == "mcp_tool_call" || item.Type == "web_search"
	if !done && trackStart && item.ID != "" {
		if already {
			return
		}
		d.started[item.ID] = true
	}

	switch item.Type {
	case "agent_message":
		if done {
			if text := oneParagraph(item.Text); text != "" {
				add(entry{kind: entryText, text: capRunes(text, textCap)})
			}
		}
	case "command_execution", "file_change", "mcp_tool_call", "web_search":
		if !already {
			tool, args := codexTool(item)
			if tool != "" {
				add(entry{kind: entryTool, tool: tool, args: capRunes(args, argCap)})
			}
		}
		if done {
			if result, show := codexItemResult(item); show {
				text, cut := trimLines(result, resultLines)
				add(entry{kind: entryResult, text: text, cut: cut, fail: codexItemFailed(item)})
			}
		}
	}
	if done && item.ID != "" {
		d.completed[item.ID] = true
	}
}

func codexTool(item codexTranscriptItem) (string, string) {
	switch item.Type {
	case "command_execution":
		return "shell", oneLine(item.Command)
	case "file_change":
		parts := make([]string, 0, len(item.Changes))
		for _, change := range item.Changes {
			part := strings.TrimSpace(change.Kind)
			if part != "" {
				part += " "
			}
			parts = append(parts, part+shortPath(change.Path))
		}
		return "apply_patch", strings.Join(parts, ", ")
	case "mcp_tool_call":
		name := strings.Trim(strings.TrimSpace(item.Server)+"/"+strings.TrimSpace(item.Tool), "/")
		return name, toolArgs(item.Arguments)
	case "web_search":
		return "web_search", oneLine(item.Query)
	}
	return "", ""
}

func codexItemResult(item codexTranscriptItem) (string, bool) {
	failed := codexItemFailed(item)
	switch item.Type {
	case "command_execution":
		text := item.AggregatedOutput
		if errText := codexError("", item.Error); errText != "" {
			if strings.TrimSpace(text) != "" {
				text += "\n"
			}
			text += errText
		}
		return text, true
	case "mcp_tool_call", "web_search":
		text := codexResultText(item.Result)
		if errText := codexError("", item.Error); errText != "" {
			text = errText
		}
		return text, text != "" || failed
	case "file_change":
		if failed {
			return codexError("", item.Error), true
		}
	}
	return "", false
}

func codexItemFailed(item codexTranscriptItem) bool {
	if strings.EqualFold(item.Status, "failed") {
		return true
	}
	if item.ExitCode != nil && *item.ExitCode != 0 {
		return true
	}
	return len(item.Error) > 0 && string(item.Error) != "null"
}

func codexResultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if text := resultText(raw); text != "" {
		return text
	}
	var result struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &result) == nil {
		if text := resultText(result.Content); text != "" {
			return text
		}
	}
	return oneLine(string(raw))
}

func codexError(message string, raw json.RawMessage) string {
	if text := strings.TrimSpace(message); text != "" {
		return text
	}
	if len(raw) == 0 || string(raw) == "null" {
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
	return oneLine(string(raw))
}

func codexEndText(usage *codexUsage) string {
	parts := []string{"finished"}
	if usage != nil {
		parts = append(parts, fmt.Sprintf("input %d", usage.InputTokens))
		if usage.CachedInputTokens > 0 {
			parts = append(parts, fmt.Sprintf("cached %d", usage.CachedInputTokens))
		}
		parts = append(parts, fmt.Sprintf("output %d", usage.OutputTokens))
	}
	return strings.Join(parts, " · ")
}

func failed(l logLine) bool {
	return l.IsError || (l.Subtype != "" && l.Subtype != "success")
}

// endText is the one line a process's result is worth: how it ended, how many
// turns it took and what it cost. The result's own text is not repeated — it is
// the last assistant message, which is already the entry above this one.
func endText(l logLine) string {
	verdict := "finished"
	switch {
	case l.Subtype != "" && l.Subtype != "success":
		verdict = l.Subtype
	case l.IsError:
		verdict = "failed"
	}
	parts := []string{verdict}
	if l.NumTurns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", l.NumTurns))
	}
	if l.TotalCostUSD > 0 {
		parts = append(parts, money(l.TotalCostUSD))
	}
	if l.DurationMS > 0 {
		parts = append(parts, duration(msecs(l.DurationMS)))
	}
	if s := firstLine(l.Error); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

// resultText flattens a tool result, which the CLI writes either as a string or
// as a list of blocks.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []logBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(blk.Text)
	}
	return b.String()
}

// --- argument summaries ---

// argKeys are the tool inputs worth putting in a header line, in the order they
// win. It is one list rather than a table of tool names because the tools are
// not a fixed set — a repo's MCP servers add their own — and every tool worth
// summarising names its subject with one of these.
//
// command beats description because Bash carries both and the command is what
// somebody watching a four-minute tool call came to see; description beats
// prompt for the same reason in reverse, since a prompt is an essay.
var argKeys = []string{
	"command", "file_path", "notebook_path", "pattern", "path", "url",
	"query", "description", "skill", "prompt", "content",
}

// pathKeys are the ones whose value is a path, and which are shortened from the
// front: a worktree makes every path absolute and thirty cells of
// .beads/auto/wt/<issue> is thirty cells that say nothing.
var pathKeys = map[string]bool{"file_path": true, "notebook_path": true, "path": true}

// toolArgs summarises a tool call's input in one line.
func toolArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range argKeys {
		s, ok := in[k].(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		if pathKeys[k] {
			return capRunes(shortPath(s), argCap)
		}
		out := oneLine(s)
		// A pattern or a query on its own does not say where it looked.
		if p, ok := in["path"].(string); ok && strings.TrimSpace(p) != "" {
			out += " in " + shortPath(p)
		}
		return capRunes(out, argCap)
	}
	return capRunes(otherArgs(in), argCap)
}

// otherArgs is the fallback for a tool none of argKeys describes: its fields,
// in a stable order, as compactly as they can be written.
func otherArgs(in map[string]any) string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		raw, err := json.Marshal(in[k])
		if err != nil {
			continue
		}
		parts = append(parts, k+"="+oneLine(string(raw)))
	}
	return strings.Join(parts, " ")
}

// shortPath keeps the last few segments of a path. The front of an absolute
// path inside a worktree is the same for every file in the run.
func shortPath(p string) string {
	p = oneLine(p)
	parts := strings.Split(p, "/")
	if len(parts) <= 3 {
		return p
	}
	return "…/" + strings.Join(parts[len(parts)-3:], "/")
}

// --- text ---

// trimLines keeps the first n lines of s and reports how many it dropped.
func trimLines(s string, n int) (string, int) {
	s = strings.Trim(sanitise(s), "\n")
	if s == "" {
		return "", 0
	}
	lines := strings.Split(s, "\n")
	cut := 0
	if len(lines) > n {
		cut, lines = len(lines)-n, lines[:n]
	}
	for i, line := range lines {
		lines[i] = capRunes(strings.TrimRight(line, " "), lineCap)
	}
	return strings.Join(lines, "\n"), cut
}

// oneParagraph is an assistant message with its control characters gone and its
// blank lines closed up, so it wraps as prose.
func oneParagraph(s string) string {
	var out []string
	for _, line := range strings.Split(sanitise(s), "\n") {
		if line = strings.TrimRight(line, " "); line != "" {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// oneLine collapses whitespace so a value can go in a header line.
func oneLine(s string) string { return strings.Join(strings.Fields(sanitise(s)), " ") }

// sanitise removes what a terminal would obey rather than print. A transcript
// is a record of somebody else's output — a test run, a file, a command's
// stderr — and escape sequences in it would rewrite the screen around it.
func sanitise(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r < 0x20, r == 0x7f:
			return -1
		}
		return r
	}, s)
}

// capRunes truncates to n runes, marking the cut.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
