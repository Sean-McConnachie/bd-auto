package cmds

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"bd-auto/internal/runstate"
)

// Exit codes hooks use. 2 is the blocking code: it feeds stderr back to the
// model and refuses the stop.
const (
	exitOK    = 0
	exitBlock = 2
)

// maxContinuations is the runaway guard. If the Stop hook refuses this many
// times in a row without the wave advancing, the run stops itself rather than
// spinning forever.
const maxContinuations = 25

// hookInput is the JSON every hook receives on stdin. Fields absent for a given
// event stay zero.
type hookInput struct {
	SessionID            string          `json:"session_id"`
	HookEventName        string          `json:"hook_event_name"`
	Cwd                  string          `json:"cwd"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	CompactionReason     string          `json:"compaction_reason"`
}

type bashToolInput struct {
	Command string `json:"command"`
}

// Hook implements `bd-auto hook <event>`.
//
// Every hook fails open: an internal error exits 0 and stays quiet rather than
// wedging the user's session. The only deliberate non-zero exit is the Stop
// hook refusing to stop, which is the whole point of the design.
func Hook(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bd-auto hook <stop|session-start|post-compact|subagent-stop|pre-tool-use>")
	}
	event := args[0]

	raw, _ := io.ReadAll(os.Stdin)
	var in hookInput
	_ = json.Unmarshal(raw, &in)

	c, err := NewCtx()
	if err != nil {
		return nil // not a repo we understand; stay out of the way
	}

	switch event {
	case "session-start", "post-compact":
		return hookRehydrate(c)
	case "stop":
		return hookStop(c, in)
	case "subagent-stop":
		return hookSubagentStop(c, in)
	case "pre-tool-use":
		return hookPreToolUse(c, in)
	default:
		return fmt.Errorf("unknown hook event %q", event)
	}
}

// hookRehydrate prints the run state so a session that just compacted, resumed
// or started knows it is mid-run. Silent when no run is active.
func hookRehydrate(c *Ctx) error {
	st, err := c.State()
	if err != nil {
		return nil
	}
	if st.Status == runstate.StatusDone {
		return nil
	}
	stats, _ := c.BD.EpicStats(st.Epic)
	ready, _ := c.BD.Ready(st.Epic, 0)
	var readyIDs []string
	for _, r := range ready {
		if !st.Excluded(r.ID) {
			readyIDs = append(readyIDs, r.ID)
		}
	}
	fmt.Print(renderContext(st, stats.Total, stats.Closed, readyIDs))
	return nil
}

// hookStop is the autonomy engine. While a run has work left it refuses to let
// the session stop, and says exactly what to do next.
func hookStop(c *Ctx, in hookInput) error {
	// Never block a subagent's own stop through this hook.
	if in.AgentID != "" {
		return nil
	}
	st, err := c.State()
	if err != nil {
		return nil // no run: ordinary sessions are unaffected
	}
	if st.Status != runstate.StatusActive {
		return nil
	}

	// Runaway guard.
	if st.Continuations >= maxContinuations {
		_, _ = runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
			s.Status = runstate.StatusPaused
			s.Note("paused: %d continuations without progress", s.Continuations)
			return nil
		})
		fmt.Fprintf(os.Stderr,
			"bd-auto: paused after %d stop-hook continuations without the wave advancing. "+
				"Inspect `bd-auto run status`, then `bd-auto run resume` or `bd-auto run stop`.\n",
			st.Continuations)
		return nil
	}

	inFlight := st.Remaining()
	ready, _ := c.BD.Ready(st.Epic, 0)
	var readyIDs []string
	for _, r := range ready {
		if !st.Excluded(r.ID) {
			readyIDs = append(readyIDs, r.ID)
		}
	}
	unmerged := unmergedBranches(c, st)

	reason, block := stopDecision(st, inFlight, readyIDs, unmerged)
	if !block {
		// Nothing left: close the run out so the next Stop is silent.
		if len(inFlight) == 0 && len(readyIDs) == 0 && len(unmerged) == 0 {
			_, _ = runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
				s.Status = runstate.StatusDone
				s.Note("run complete: %d done, %d parked", len(s.Done), len(s.Parked))
				return nil
			})
			fmt.Fprintf(os.Stderr, "bd-auto: run complete for %s (%d done, %d parked). "+
				"Run `bd-auto run stop` to clear the run state.\n", st.Epic, len(st.Done), len(st.Parked))
		}
		return nil
	}

	_, _ = runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
		s.Continuations++
		return nil
	})
	fmt.Fprint(os.Stderr, reason)
	return errSilentExit{code: exitBlock}
}

// stopDecision decides whether the session may stop, and what to say if not.
// Split out from hookStop so it is testable without a repo.
func stopDecision(st *runstate.State, inFlight, ready, unmerged []string) (string, bool) {
	autonomy := st.Autonomy

	var b strings.Builder
	b.WriteString("bd-auto: the run is not finished. Do not stop.\n")
	fmt.Fprintf(&b, "Epic %s, wave %d, autonomy %s.\n", st.Epic, st.Wave, autonomy)

	switch {
	case len(inFlight) > 0:
		fmt.Fprintf(&b, "Still in flight: %s\n", strings.Join(inFlight, ", "))
		b.WriteString("Wait for those workers to report, then run their review stage.\n")
		b.WriteString("If a worker has already reported, record it with `bd-auto worker done` or `bd-auto worker fail`.\n")
		return b.String(), true

	case len(unmerged) > 0:
		// A wave finished but its branches are not integrated. Never start a
		// new wave here: the next wave's worktrees branch from HEAD and would
		// not see this work.
		fmt.Fprintf(&b, "Wave %d is complete but these branches are unmerged: %s\n",
			st.Wave, strings.Join(unmerged, ", "))
		b.WriteString("Dispatch the bd-integrator agent now: `bd-auto merge-order` lists them in dependency order.\n")
		return b.String(), true

	case len(ready) > 0:
		if autonomy == "wave" || autonomy == "issue" {
			// Barrier reached, and the human asked to be consulted here.
			return "", false
		}
		fmt.Fprintf(&b, "Ready to dispatch: %s\n", strings.Join(ready, ", "))
		b.WriteString("Run `bd-auto plan --dispatch` and spawn one bd-worker per issue IN A SINGLE MESSAGE.\n")
		return b.String(), true
	}

	return "", false
}

// unmergedBranches reports wave branches that have commits the base does not.
func unmergedBranches(c *Ctx, st *runstate.State) []string {
	var out []string
	for _, id := range st.WaveIssues {
		br := c.Cfg.Branch(id)
		if branchExists(c.RepoRoot, br) && commitsAhead(c.RepoRoot, br) > 0 {
			out = append(out, br)
		}
	}
	return out
}

// footerRe matches the report footer every worker must end with. It is how a
// finishing worker is tied back to its issue when no claim binding was seen.
var footerRe = regexp.MustCompile(`BD-AUTO:\s*issue=(\S+)\s+branch=(\S+)\s+status=(\S+)`)

// hookSubagentStop verifies a worker actually finished, instead of trusting its
// report. A worker that says "done" without closing its issue is sent back.
func hookSubagentStop(c *Ctx, in hookInput) error {
	st, err := c.State()
	if err != nil || st.Status != runstate.StatusActive {
		return nil
	}
	// Only police our own workers.
	if in.AgentType != "" && !strings.Contains(in.AgentType, "bd-worker") {
		return nil
	}

	issueID, _ := st.IssueForAgent(in.AgentID)
	var footerStatus string
	if m := footerRe.FindStringSubmatch(in.LastAssistantMessage); m != nil {
		if issueID == "" {
			issueID = m[1]
		}
		footerStatus = m[3]
	}
	if issueID == "" {
		// Nothing ties this agent to an issue. Ask for the footer rather than
		// guessing, but only when a run is active and this looks like a worker.
		if in.AgentType != "" && strings.Contains(in.AgentType, "bd-worker") {
			fmt.Fprint(os.Stderr,
				"bd-auto: your final message must end with a footer line:\n"+
					"BD-AUTO: issue=<id> branch=<branch> status=done|blocked\n"+
					"Add it and finish again.\n")
			return errSilentExit{code: exitBlock}
		}
		return nil
	}

	iss, err := c.BD.Show(issueID)
	if err != nil {
		return nil // cannot verify; fail open
	}
	if iss.Terminal() {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "bd-auto: %s is still %q, so this work is not finished.\n", issueID, iss.Status)
	if footerStatus == "done" {
		b.WriteString("You reported status=done but the issue was never closed.\n")
	}
	b.WriteString("Do one of these, then finish again:\n")
	b.WriteString("  - Passing: commit your work, then `bd close " + issueID + "`.\n")
	b.WriteString("  - Genuinely blocked: `bd update " + issueID +
		" --status=blocked --append-notes=\"bd-auto attempt: <what failed>\"`.\n")
	fmt.Fprint(os.Stderr, b.String())
	return errSilentExit{code: exitBlock}
}

var (
	claimRe = regexp.MustCompile(`\bbd\b[^|;&]*\bupdate\s+(\S+)[^|;&]*--claim`)
	// A worker must not integrate. Only the integrator merges, and only at the
	// wave barrier. Denying this deterministically beats asking politely.
	//
	// Deliberately an anywhere-match: a command can reach git through sudo, env,
	// nohup, a loop body or an if condition, and anchoring to command position
	// misses every one of those. False positives here cost a worker one
	// rephrasing; a false negative lets a worker integrate.
	forbiddenRe = regexp.MustCompile(`\bgit\s+(merge|rebase|push|cherry-pick)\b`)
)

// forbidsIntegration reports whether a worker's Bash command would integrate.
//
// Quoted spans are stripped before matching, because quoted text is data rather
// than something about to run. Found live: the anywhere-match denied a worker's
// `bd update --append-notes="...'git merge main'..."`, so the guard blocked a
// note *describing* the guard. Stripping quotes fixes that without narrowing
// what counts as a command.
func forbidsIntegration(cmd string) bool {
	return forbiddenRe.MatchString(stripQuoted(cmd))
}

// stripQuoted blanks out single- and double-quoted spans, leaving everything
// that could still execute. Quotes become spaces so a stripped span cannot glue
// two words into one.
func stripQuoted(cmd string) string {
	var b strings.Builder
	b.Grow(len(cmd))
	var quote byte // 0 outside quotes, else '\'' or '"'
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
			b.WriteByte(' ')
		case quote != 0 && c == quote:
			quote = 0
			b.WriteByte(' ')
		case quote != 0:
			// Inside double quotes a backslash escapes the next byte, so skip it
			// or an escaped quote would look like the end of the span.
			if quote == '"' && c == '\\' && i+1 < len(cmd) {
				i++
			}
		case c == '\\' && i+1 < len(cmd):
			// An escaped character outside quotes cannot open a quoted span.
			i++
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// hookPreToolUse binds a worker to the issue it claims, and blocks workers from
// integrating.
func hookPreToolUse(c *Ctx, in hookInput) error {
	if in.ToolName != "Bash" || in.AgentID == "" {
		return nil
	}
	st, err := c.State()
	if err != nil || st.Status == runstate.StatusDone {
		return nil
	}

	var ti bashToolInput
	_ = json.Unmarshal(in.ToolInput, &ti)
	if ti.Command == "" {
		return nil
	}

	// Record the binding: agent_id -> issue.
	if m := claimRe.FindStringSubmatch(ti.Command); m != nil {
		issue := strings.TrimSpace(m[1])
		_, _ = runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
			s.Bindings[in.AgentID] = issue
			if a, ok := s.InFlight[issue]; ok {
				a.AgentID = in.AgentID
				s.InFlight[issue] = a
			}
			return nil
		})
	}

	// Block integration attempts by workers. The integrator runs in the main
	// checkout with no agent worktree, and is exempt by agent type.
	if strings.Contains(in.AgentType, "bd-integrator") {
		return nil
	}
	if forbidsIntegration(ti.Command) {
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":      "PreToolUse",
				"permissionDecision": "deny",
				"permissionDecisionReason": "bd-auto: workers must not merge, rebase, push or cherry-pick. " +
					"Commit on your own branch and report; the bd-integrator merges the whole wave at the barrier.",
			},
		}
		return emitJSON(out)
	}
	return nil
}
