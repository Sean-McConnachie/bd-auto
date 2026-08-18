package tui

// The transcript view: enter, on a selected row.
//
// The table gives one issue one line, and that line is the last thing that
// happened. This is the rest of it, arranged the way a Claude Code session
// reads: prose wrapped, each tool call a header carrying its name and what it
// was called with, its result indented underneath and cut off honestly, and a
// separator wherever one process handed over to the next.
//
// It renders instead of the table rather than over it. A transcript is the one
// thing on this screen that is read rather than watched, and half a screen of
// it is not worth having; esc puts the table back with the cursor where it was.

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// detail is one issue's transcript, open.
type detail struct {
	// issue is the row this was opened on. The row itself is looked up on every
	// frame rather than held, because the table keeps folding events in
	// underneath and the heading should say what the issue is doing now.
	issue string
	log   *transcript
	// top is the first body line on screen, and follow is whether it should
	// stay pinned to the end as more arrives. A transcript is opened to see
	// what a worker is doing now, so it opens at the bottom and stays there
	// until somebody scrolls up.
	top    int
	follow bool
}

func newDetail(issue string) *detail {
	return &detail{issue: issue, log: newTranscript(issue), follow: true}
}

// open shows the selected row's transcript.
//
// Nothing here checks for a question on screen: askKey has already taken the
// enter that would have opened this, which is the same rule every other table
// key follows. A human answering a prompt must not find the screen replaced
// underneath them.
func (m *Model) open() {
	row := m.Selected()
	if row == nil {
		return
	}
	// The status line is cleared first: it is a message about the table, it
	// costs the transcript a line of height, and the offset below is measured
	// against that height.
	m.status = ""
	m.detail = newDetail(row.Issue)
	m.detail.log.refresh(m.RepoRoot)
	m.detail.top = m.detailTop(m.detail.lines(m.width()))
}

// close puts the table back. The cursor never moved, because opening this never
// touched it.
func (m *Model) close() {
	m.detail = nil
	m.status = ""
}

// detailKey handles a keystroke while a transcript is open.
//
// Everything the table's keys do is destructive or navigational, and neither
// belongs to a screen the table is not on: k would kill a worker whose
// transcript you are reading, and q would end the run. So the keys this does
// not use are swallowed with a reminder rather than handed back, which is what
// the question box does with the same problem. Ctrl-C is the exception
// everywhere, and it is handled before this is reached.
func (m *Model) detailKey(msg tea.KeyMsg) {
	page := maxInt(m.bodyHeight()-1, 1)
	body := m.detail.lines(m.width())
	switch msg.String() {
	case "up", "shift+tab":
		m.scroll(-1, body)
	case "down", "tab":
		m.scroll(1, body)
	case "pgup":
		m.scroll(-page, body)
	case "pgdown", " ":
		m.scroll(page, body)
	case "home", "g":
		m.scroll(-len(body), body)
	case "end", "G":
		m.scroll(len(body), body)
	case "esc", "q":
		m.close()
	default:
		m.status = "reading " + m.detail.issue + " · " + detailKeys
	}
}

// scroll moves the window and clamps it at both ends.
func (m *Model) scroll(by int, body []string) {
	d := m.detail
	max := m.detailTop(body)
	d.top += by
	if d.top < 0 {
		d.top = 0
	}
	if d.top > max {
		d.top = max
	}
	// Reaching the end is what asks to be followed again, and leaving it is
	// what stops it: a reader half way up a transcript must not be dragged to
	// the bottom by the next tool call the worker makes.
	d.follow = d.top >= max
}

// detailTop is the furthest the window can be scrolled: the last screenful.
func (m *Model) detailTop(body []string) int {
	return maxInt(len(body)-m.bodyHeight(), 0)
}

// refreshDetail reads whatever the worker has written since the last tick.
func (m *Model) refreshDetail() {
	if m.detail == nil {
		return
	}
	m.detail.log.refresh(m.RepoRoot)
	if m.detail.follow {
		m.detail.top = m.detailTop(m.detail.lines(m.width()))
	}
}

// --- view ---

// detailChrome is how many lines the view spends on everything that is not
// transcript: the heading, the sub-heading, the blank line under them, the
// blank line and key line at the foot, and the trailing line bubbletea erases.
const detailChrome = 6

const detailKeys = "↑/↓ scroll · pgup/pgdn page · g/G ends · esc back to the table"

// bodyHeight is how many lines of transcript fit.
func (m *Model) bodyHeight() int {
	room := m.height() - detailChrome
	if m.status != "" {
		room--
	}
	if box := m.questionBox(); box != "" {
		room -= lipgloss.Height(box)
	}
	return maxInt(room, 3)
}

// detailView renders the transcript instead of the table.
func (m *Model) detailView() string {
	width := m.width()
	body := m.detail.lines(width)
	// The window is clamped here as well as on every keystroke, because the
	// terminal can be resized and the transcript can grow under a reader who is
	// pressing nothing at all.
	if top := m.detailTop(body); m.detail.top > top {
		m.detail.top = top
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render(clip(m.detailHead(), width)) + "\n")
	b.WriteString(m.detailSub(len(body)) + "\n\n")
	for _, line := range m.window(body) {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	if box := m.questionBox(); box != "" {
		b.WriteString(box + "\n")
	}
	if m.status != "" {
		b.WriteString(dimStyle.Render(clip(m.status, width)) + "\n")
	}
	if m.Question() != nil {
		b.WriteString(dimStyle.Render("answer the question above · ctrl+c stop the run"))
	} else {
		b.WriteString(dimStyle.Render(clip(detailKeys, width)))
	}
	// The same load-bearing trailing newline the table ends on.
	b.WriteString("\n")
	return b.String()
}

// window is the screenful at the current offset, padded so the foot of the
// view does not jump about as a short transcript grows.
func (m *Model) window(body []string) []string {
	h := m.bodyHeight()
	out := make([]string, 0, h)
	for i := m.detail.top; i < len(body) && len(out) < h; i++ {
		out = append(out, body[i])
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out
}

// detailHead names the issue and what it was given to do.
func (m *Model) detailHead() string {
	head := m.detail.issue
	if r := m.rows[m.detail.issue]; r != nil && r.Title != "" {
		head += " · " + r.Title
	}
	return head
}

// detailSub is the row's own state, and where in the transcript this is.
func (m *Model) detailSub(total int) string {
	var parts []string
	style := dimStyle
	if r := m.rows[m.detail.issue]; r != nil {
		state := string(r.State)
		if r.State == StateRunning && r.Doing() != "" {
			state = r.Doing()
		}
		parts = append(parts, state)
		if d := r.Elapsed(m.now()); d > 0 {
			parts = append(parts, duration(d))
		}
		if c := r.Cost(); c > 0 {
			parts = append(parts, money(c))
		}
		if s, ok := stateStyles[r.State]; ok {
			style = s
		}
	}
	if where := m.detailWhere(total); where != "" {
		parts = append(parts, where)
	}
	return style.Render(clip(strings.Join(parts, " · "), m.width()))
}

// detailWhere says which part of the transcript is on screen. A view that can
// scroll has to say whether there is more, or a reader at the top of a long
// worker's session has no way to know they are.
func (m *Model) detailWhere(total int) string {
	// Nothing to be anywhere in. The pane says so in its own words already, and
	// "the whole transcript" over an empty one reads as a claim.
	if len(m.detail.log.entries) == 0 {
		return ""
	}
	h := m.bodyHeight()
	if total <= h {
		return "the whole transcript"
	}
	last := m.detail.top + h
	if last > total {
		last = total
	}
	return fmt.Sprintf("lines %d-%d of %d", m.detail.top+1, last, total)
}

// lines renders the whole transcript. It is rebuilt every frame rather than
// cached: it is a few hundred entries against a redraw twice a second, and a
// cache would have to be invalidated by the width, the window and every line
// the worker writes.
func (d *detail) lines(width int) []string {
	width = maxInt(width, 20)
	var out []string
	if d.log.dropped > 0 {
		out = append(out, dimStyle.Render(clip(fmt.Sprintf(
			"… %s dropped off the front: this view keeps the last %d",
			droppedEntries(d.log.dropped), entryCap), width)))
	}
	for _, e := range d.log.entries {
		out = append(out, e.lines(width)...)
	}
	if len(out) > 0 {
		return out
	}
	return []string{dimStyle.Render(clip(d.empty(), width))}
}

// empty is what an issue with nothing to read says. A blank pane would be
// indistinguishable from a broken one, and the two cases it stands for are
// different enough to be worth naming: a worker that was never dispatched, and
// one that has started and not yet said anything.
func (d *detail) empty() string {
	if !d.log.found {
		return "nothing to read yet: no model has been spawned for " + d.issue +
			", so no transcript has been written"
	}
	return "the process has started and nothing has reached its transcript yet"
}

// lines renders one entry.
func (e entry) lines(width int) []string {
	switch e.kind {
	case entryHead:
		return []string{"", headerStyle.Render(rule(e.text, width))}
	case entryText:
		return append([]string{""}, wrap(e.text, width)...)
	case entryTool:
		head := "⏺ " + e.tool
		if e.args != "" {
			head += "(" + e.args + ")"
		}
		return []string{"", selectedStyle.Render(clip(head, width))}
	case entryResult:
		return e.resultLines(width)
	case entryEnd:
		return []string{"", e.style().Render(clip(e.text, width))}
	}
	return nil
}

// resultLines is what a tool returned, indented under the call and cut off with
// a count of what is missing.
func (e entry) resultLines(width int) []string {
	body := strings.Split(e.text, "\n")
	if e.text == "" {
		body = []string{"(no output)"}
	}
	style := e.style()
	out := make([]string, 0, len(body)+1)
	for i, line := range body {
		marker := "     "
		if i == 0 {
			marker = "  ⎿  "
		}
		out = append(out, style.Render(clip(marker+line, width)))
	}
	if e.cut > 0 {
		out = append(out, dimStyle.Render(clip("     … "+moreLines(e.cut), width)))
	}
	return out
}

// style is dim for the ordinary case and the table's failure colour for an
// error, which is the same vocabulary the rows are painted in.
func (e entry) style() lipgloss.Style {
	if e.fail {
		return stateStyles[StateFailed]
	}
	return dimStyle
}

// rule is a separator with its name at the front.
func rule(name string, width int) string {
	head := "── " + name + " "
	if pad := width - lipgloss.Width(head); pad > 0 {
		return head + strings.Repeat("─", pad)
	}
	return clip(head, width)
}

// moreLines counts what a truncated tool result is missing, plurally.
func moreLines(n int) string {
	if n == 1 {
		return "+1 more line"
	}
	return fmt.Sprintf("+%d more lines", n)
}

// droppedEntries counts what fell out of the window, plurally.
func droppedEntries(n int) string {
	if n == 1 {
		return "1 earlier entry"
	}
	return fmt.Sprintf("%d earlier entries", n)
}

// duration formats an elapsed time for a column six cells wide.
func duration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func msecs(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }
