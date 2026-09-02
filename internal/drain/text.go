package drain

import (
	"fmt"
	"strings"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/pipeline"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
)

// This file is every string the engine says to a model or records about a
// failure. They are here together because they are one voice, and because a
// feedback message that names the fault without naming the fix buys a round and
// spends it on the model working out what was wanted.

// workerPrompt is the task for one worker turn.
//
// A resumed turn gets the feedback alone: the session already holds the issue,
// the exploration and the plan, and restating them invites the worker to start
// over. A fresh turn gets the whole task, with the feedback appended when there
// is any — which is how a backend without resume reaches the same outcome by a
// more expensive route.
//
// The two channels into a fresh turn are different things and read differently.
// t.Carried is why the PREVIOUS attempt was thrown away, and the work it
// describes is gone; feedback is what came back within THIS attempt, and the
// work it describes is still on the branch.
func workerPrompt(t task, resume bool, stage, feedback string) string {
	if resume && feedback != "" {
		return resumeHeader(stage) + "\n\n" + feedback + "\n\n" +
			"Fix exactly this. Do not start over and do not revert your own earlier work. " +
			"Do not commit or change Beads. Finish with the WORKER_STATUS contract."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Implement beads issue %s.\n\n", t.ID)
	if t.Issue != nil && t.Issue.Title != "" {
		fmt.Fprintf(&b, "Title:     %s\n", t.Issue.Title)
	}
	fmt.Fprintf(&b, "Issue:     %s\n", t.ID)
	fmt.Fprintf(&b, "Branch:    %s (already checked out here)\n", t.Branch)
	fmt.Fprintf(&b, "Worktree:  %s\n", t.Worktree)
	fmt.Fprintf(&b, "Base:      %s\n", t.Base)
	if t.Discoveries != "" {
		fmt.Fprintf(&b, "Discoveries: %s\n", t.Discoveries)
	}
	fmt.Fprintf(&b, "Attempt:   %d\n", t.Attempt)
	b.WriteString("\n" + issueText(t.Issue))
	b.WriteString("\nEdit and test only inside the worktree. Do not stage or commit, and do not run Beads commands.\n")
	b.WriteString("Finish with exactly one WORKER_STATUS line as described in your role prompt.\n")
	if t.Carried != "" {
		b.WriteString("\n" + t.Carried + "\n")
	}
	if feedback != "" {
		b.WriteString("\n" + resumeHeader(stage) + "\n\n" + feedback + "\n")
		b.WriteString("\nThat is the only thing that needs fixing. The work already on " + t.Branch +
			" is yours — build on it rather than starting again.\n")
	}
	return b.String()
}

// carriedFailure is what a fresh attempt is told about the attempt before it.
//
// It says where the account came from, and it says not to go looking for it on
// the issue. bd-auto does write the same thing to the issue's notes, but that
// write is reverted by beads' post-checkout hook when this attempt's worktree
// is created — so a worker sent to `bd show` for the history would find the
// section missing and conclude there was no history, which is how the same
// mistake gets made three times at full price.
func carriedFailure(f runstate.Failure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This is a retry. The previous %s\n", f.Summary())
	b.WriteString("\nNothing of that attempt is here: its worktree and its branch were deleted, " +
		"and you are starting again from the base commit. Do not repeat it.\n")
	b.WriteString("The text above is bd-auto's own record and may be the only copy — " +
		"the matching note on the issue is often gone by the time you read it, " +
		"so do not treat a `bd show` with no attempt history as proof there was none.\n")
	return b.String()
}

// resumeHeader names what came back, so the worker knows which of its outputs
// is being judged before it reads the detail.
func resumeHeader(stage string) string {
	switch stage {
	case StageImplement:
		return "This attempt is not finished yet."
	case StageGuard:
		return "Your branch did not pass bd-auto's git checks."
	case "":
		return "There is feedback on your work."
	default:
		return "The " + stage + " stage failed on your work."
	}
}

// reviewPrompt is the task for a judging stage.
func reviewPrompt(t task, s config.Stage, resume bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review the current uncommitted snapshot for beads issue %s.\n\n", t.ID)
	if t.Issue != nil && t.Issue.Title != "" {
		fmt.Fprintf(&b, "Title:     %s\n", t.Issue.Title)
	}
	fmt.Fprintf(&b, "Issue:     %s\n", t.ID)
	fmt.Fprintf(&b, "Branch:    %s\n", t.Branch)
	fmt.Fprintf(&b, "Base:      %s\n", t.Base)
	fmt.Fprintf(&b, "Worktree:  %s (your working directory)\n", t.Worktree)
	fmt.Fprintf(&b, "Stage:     %s\n", s.Stage)
	if resume {
		b.WriteString("Review round: fresh review after worker feedback\n")
	}
	b.WriteString("\n" + issueText(t.Issue))
	b.WriteString("\nCandidate snapshot (binary-capable Git patch; includes non-ignored untracked files):\n\n")
	if strings.TrimSpace(t.Diff) == "" {
		b.WriteString("(empty snapshot)\n")
	} else {
		b.WriteString("```diff\n" + t.Diff + "\n```\n")
	}
	b.WriteString("\nThe supplied issue and snapshot are authoritative. Do not call bd or depend on Git metadata.\n")
	return b.String()
}

// conflictPrompt is the task for the one model integration ever spawns.
//
// It names the conflicted files rather than leaving them to be discovered:
// bd-auto already knows them, and a resolution that wanders outside them is the
// failure mode the prompt is written against.
func conflictPrompt(m Merge, base string, iss *bd.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Resolve the conflicts from merging %s into %s.\n\n", m.Branch, base)
	if iss != nil && iss.Title != "" {
		fmt.Fprintf(&b, "Title:     %s\n", iss.Title)
	}
	fmt.Fprintf(&b, "Issue:     %s\n", m.Issue)
	fmt.Fprintf(&b, "Branch:    %s\n", m.Branch)
	fmt.Fprintf(&b, "Into:      %s, the main checkout, sitting mid-merge\n", base)
	b.WriteString("\n" + issueText(iss))
	b.WriteString("\nConflicted files:\n")
	for _, p := range m.Conflicts {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	if len(m.Settled) > 0 {
		// Said out loud because the tree says it too: these are staged and
		// resolved already, and an integrator that found them in `git status`
		// without being told would be right to wonder who did that.
		fmt.Fprintf(&b, "\nAlready resolved and staged: %s. beads regenerates those from a database both "+
			"branches were writing to, so bd-auto kept the copy %s already had. Leave them as they are.\n",
			strings.Join(m.Settled, ", "), base)
	}
	b.WriteString("\nInspect the conflict markers and surrounding files to understand both sides. " +
		"Do not run Git or Beads commands; the supplied task is complete.\n")
	b.WriteString("Resolve every file listed above by editing it, then stop. Do not stage anything. " +
		"Do not create the merge commit and do not abort: bd-auto inspects the files you leave " +
		"and completes or abandons the merge itself.\n")
	return b.String()
}

// issueText renders every issue field an agent needs, so no role has to reach
// through its sandbox to the canonical Beads database.
func issueText(iss *bd.Issue) string {
	if iss == nil {
		return "Issue data:\n  (unavailable)\n"
	}
	var b strings.Builder
	b.WriteString("Issue data:\n")
	field := func(name, value string) {
		if strings.TrimSpace(value) == "" {
			value = "(none)"
		}
		fmt.Fprintf(&b, "%s:\n%s\n", name, indent(value, "  "))
	}
	field("Title", iss.Title)
	field("Description", iss.Description)
	field("Design", iss.Design)
	field("Acceptance criteria", iss.AcceptanceCriteria)
	field("Notes", iss.Notes)
	b.WriteString("Dependencies:\n")
	if len(iss.Dependencies) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, d := range iss.Dependencies {
			fmt.Fprintf(&b, "  - %s | %s | status=%s | type=%s\n", d.ID, emptyAs(d.Title, "(untitled)"), emptyAs(d.Status, "unknown"), emptyAs(d.Type, "unspecified"))
		}
	}
	return b.String()
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n"+prefix)
}

func emptyAs(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// hookPrompt is the task for an agent: hook.
//
// Three things have to be in it and none of them can be left to the agent
// file. Where the report is, because the file is the whole input and a hook
// that has to go looking for it is a hook spending its turn on that. That it is
// advisory, because a model handed a report of a failed issue will otherwise
// try to fix it, and the fallback prompt for a role with no file of its own is
// the reviewer's — which asks for a verdict nothing here reads. And what it may
// not touch, because the constraints are properties of when a hook runs rather
// than of what it was asked to do, so nobody writing an agent file could know
// them.
func hookPrompt(f hookFiring, h config.Hook, input, repoRoot string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the %s hook %q for this bd-auto run.\n\n", f.Point, h.Name)
	fmt.Fprintf(&b, "Report:  %s\n", input)
	fmt.Fprintf(&b, "         %s.\n", f.What)
	if f.Issue != "" {
		fmt.Fprintf(&b, "Issue:   %s\n", f.Issue)
	}
	fmt.Fprintf(&b, "Repo:    %s (the main checkout, and your working directory)\n", repoRoot)
	b.WriteString("\nRead that file. It is already-published bd-auto report JSON: the same shape " +
		"`bd-auto --json` emits, with stable field names.\n")

	b.WriteString("\nYou are advisory. Everything in that report is already decided and recorded — " +
		"the verdicts, the merges, the parks — and bd-auto reads nothing back out of you: " +
		"no verdict is parsed from your reply, and nothing you say changes what this run did. " +
		"Your final message is put on the run's report and shown to whoever is watching, " +
		"so it is the whole of your output. Say what you found there, briefly.\n")

	b.WriteString("\nWhat you may not do:\n")
	switch {
	case f.Issue != "":
		fmt.Fprintf(&b, "  - Write to any beads issue other than %s. Its worker has exited, so %s is "+
			"yours; other workers are running beside you right now, and beads takes one writer "+
			"per issue.\n", f.Issue, f.Issue)
	default:
		b.WriteString("  - Write to a beads issue this run is still working on. Nothing is in flight " +
			"at this point, but bd-auto has finished its own writes here and will not read yours.\n")
	}
	b.WriteString("  - Run git. This checkout is the run's: the barrier merges into it and the next " +
		"wave's worktrees branch from it. Committing, switching or resetting here corrupts the run.\n")
	fmt.Fprintf(&b, "  - Take longer than %ds. You are stopped at that point and recorded as having "+
		"timed out, because a run is never held up by something that cannot change its outcome.\n",
		hookTimeoutOf(h))
	return b.String()
}

// hookTimeoutOf is the hook's resolved timeout, for the sentence that states it.
func hookTimeoutOf(h config.Hook) int {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return config.DefaultHookTimeout
}

// conflictParkReason explains a resolution bd-auto would not accept. The model's
// own account goes with it: it is the only record of what it was trying to do.
func conflictParkReason(why, text string) string {
	r := "the merge conflict was not resolved: " + why
	if t := strings.TrimSpace(text); t != "" {
		r += "\nThe integrator's account:\n" + t
	}
	return r
}

// reviewNotes is what lands in .beads/auto/review/<id>.md. It outlives the
// round, so a verdict can be read after the fact rather than only in the
// feedback it produced.
//
// The refused tools go in it beside the verdict because a judging stage is
// refused things by design and that must stay non-fatal — but a verdict reached
// with less evidence than the reviewer wanted is not the same verdict, and
// without this the difference is invisible. A repo that reads the same refusal
// under every review has the evidence to widen allowed_tools; one that reads a
// denied bd write has a reviewer that tried to touch the record.
func reviewNotes(t task, s config.Stage, text string, denials []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", t.ID, s.Stage)
	fmt.Fprintf(&b, "- branch: `%s`\n- base: `%s`\n- attempt %d, round %d\n- %s\n",
		t.Branch, t.Base, t.Attempt, t.Round+1, time.Now().UTC().Format(time.RFC3339))
	if len(denials) > 0 {
		fmt.Fprintf(&b, "- refused: %s\n", strings.Join(denials, ", "))
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n")
	return b.String()
}

// deniedVerdictNote is the line a run's log carries when a judging stage was
// refused a tool. Non-fatal, and named as such: the stage still returns a
// verdict, and this is how a reader finds out what it did not get to see.
func deniedVerdictNote(id, stage string, tools []string) string {
	return fmt.Sprintf("%s: the %s stage was refused %s; its verdict was reached without them",
		id, stage, strings.Join(tools, ", "))
}

func workerStatusFeedback(reason string) string {
	return "Your final message did not satisfy the worker status contract: " + reason + "\n" +
		"Return exactly one `WORKER_STATUS: ready` line when the snapshot is ready, or " +
		"`WORKER_STATUS: blocked` plus a non-empty `WORKER_REASON:` line when it cannot advance. " +
		"Do not change Git or Beads state."
}

func reviewerMutationFeedback() string {
	return "The reviewer changed the candidate worktree. Reviewers are read-only, so bd-auto rejected " +
		"the verdict and will not commit a snapshot that differs from the one reviewed. Inspect the current " +
		"worktree, keep only changes required by the issue, and return it ready again."
}

type workerResult struct {
	Status string
	Reason string
	Valid  bool
	Error  string
}

// parseWorkerResult reads the small, explicit handoff contract. Prose is not a
// status: guessing from "done" or "blocked" would recreate the lifecycle
// ambiguity this contract removes.
func parseWorkerResult(text string) workerResult {
	var statuses, reasons []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "WORKER_STATUS:"); ok {
			statuses = append(statuses, strings.TrimSpace(v))
		}
		if v, ok := strings.CutPrefix(line, "WORKER_REASON:"); ok {
			reasons = append(reasons, strings.TrimSpace(v))
		}
	}
	if len(statuses) != 1 {
		return workerResult{Error: fmt.Sprintf("found %d WORKER_STATUS lines; exactly one is required", len(statuses))}
	}
	r := workerResult{Status: statuses[0]}
	switch r.Status {
	case "ready":
		if len(reasons) > 0 {
			return workerResult{Error: "WORKER_REASON is valid only with blocked status"}
		}
		r.Valid = true
	case "blocked":
		if len(reasons) != 1 || reasons[0] == "" {
			return workerResult{Error: "blocked status requires exactly one non-empty WORKER_REASON line"}
		}
		r.Valid, r.Reason = true, reasons[0]
	default:
		r.Error = fmt.Sprintf("status %q is not ready or blocked", r.Status)
	}
	return r
}

// selfParkNote is the orchestrator's durable account of a blocked handoff. The
// worker cannot write the issue, so its required reason must be copied here.
func selfParkNote(id, branch string, attempt, allowed int, reason string) string {
	return fmt.Sprintf("bd-auto parked %s: its worker returned WORKER_STATUS: blocked on attempt %d of %d. "+
		"The remaining attempt(s) were not spent and %s was not merged.\nReason: %s\n"+
		"Unpark it with `bd-auto run unpark --issue %s` once the blocker is resolved.",
		id, attempt, allowed, branch, strings.TrimSpace(reason), id)
}

// missingDeps reads a park reason for the IDs of the issues running beside it.
//
// The reason is prose a model wrote, so this is a search rather than a parse:
// what it is looking for is the worker naming the issue it thinks it is waiting
// for. Every hit is a fact worth reporting on its own, because two siblings
// named is two edges a human might have meant to write.
//
// The issue's own ID is skipped — a park reason names it constantly — and so is
// anything not in the wave, which is the whole point: an issue outside the wave
// really can be a blocker, and bd's ready front already knows about it.
func missingDeps(id, reason string, waveIssues []string) []MissingDep {
	if strings.TrimSpace(reason) == "" {
		return nil
	}
	var out []MissingDep
	seen := map[string]bool{}
	for _, sib := range waveIssues {
		if sib == "" || sib == id || seen[sib] || !namesIssue(reason, sib) {
			continue
		}
		seen[sib] = true
		out = append(out, MissingDep{
			Issue:   id,
			Sibling: sib,
			Command: fmt.Sprintf("bd dep add %s %s", id, sib),
		})
	}
	return out
}

// namesIssue reports whether some text mentions an issue by ID.
//
// A plain strings.Contains is wrong here, because bd IDs nest: "x-j5a" is a
// substring of "x-j5a.4" and "t-1" of "t-10", so a wave holding both would
// report the parent every time a worker named the child. A match therefore has
// to be bounded on both sides by something that cannot continue an ID.
//
// Case is ignored. The ID is bd's spelling; what surrounds it is a sentence a
// model wrote, and one that opened with the ID capitalised means the same
// thing.
func namesIssue(text, id string) bool {
	lower, want := strings.ToLower(text), strings.ToLower(id)
	for i := 0; i+len(want) <= len(lower); {
		j := strings.Index(lower[i:], want)
		if j < 0 {
			return false
		}
		j += i
		before := byte(' ')
		if j > 0 {
			before = lower[j-1]
		}
		after := byte(' ')
		if end := j + len(want); end < len(lower) {
			after = lower[end]
		}
		if !idByte(before) && !idByte(after) {
			return true
		}
		i = j + 1
	}
	return false
}

// idByte reports whether a byte can be part of a bd issue ID. Prefixes are
// alphanumeric with dashes, and a child ID appends a dotted suffix.
func idByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-', b == '.', b == '_':
		return true
	}
	return false
}

// missingDepNote is how a sibling named in a park reason is recorded on the run.
//
// It says what bd-auto knows (the two issues ran together, so neither blocked
// the other), what it does not (whether the edge is real), and the one command
// that settles it. The last sentence is there so that a human reading the note
// does not go looking for an edge bd-auto might have added on its own: it did
// not, and never does.
func missingDepNote(d MissingDep) string {
	return fmt.Sprintf("%s parked naming %s, which ran beside it in the same wave and so cannot have "+
		"blocked it. If it really is a blocker, the graph is missing that edge: `%s`. "+
		"bd-auto has not added it.", d.Issue, d.Sibling, d.Command)
}

// mergeMissingDeps collects the missing edges across a whole run's issue
// reports, keeping the first mention of each issue-sibling pair.
func mergeMissingDeps(reports []Report) []MissingDep {
	var out []MissingDep
	seen := map[string]bool{}
	for _, r := range reports {
		for _, d := range r.MissingDeps {
			key := d.Issue + "\x00" + d.Sibling
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, d)
		}
	}
	return out
}

// noProgressReason ends an attempt. It is a hard failure rather than another
// round because a turn that changed nothing means resuming is not working for
// this issue, and the answer to that is a fresh worker, not another resume.
//
// It takes the result because an empty worktree has two very different causes
// and only one of them is about the work. A model that ran and produced nothing
// is a finding. A process that failed before it could do anything leaves an
// identical worktree, and describing that as "returned without changing
// anything" hides the failure behind its symptom — which is how a drain came to
// park five issues under a rate limit and report it as five workers that idled.
// Where the process said what went wrong, that is the reason.
func noProgressReason(round int, res runner.Result) string {
	if res.Err != nil {
		return fmt.Sprintf(
			"round %d changed nothing because the process failed: %s. The worktree is empty as a "+
				"consequence of that failure, not as a verdict on the work.", round+1, res.Err)
	}
	return fmt.Sprintf(
		"round %d returned without changing anything: no new commit, no modified file, "+
			"no new untracked file. Every check after this one would pass on the previous round's "+
			"state, so the attempt is failed here rather than spending the rest of its rounds "+
			"re-judging an identical diff.", round+1)
}

// deniedReason ends the run on the environment rather than on the work.
//
// It is the counterpart to noProgressReason, and the distinction is the whole
// point: an empty worktree after a refused Write is not a model that did
// nothing, it is a model that was not allowed to do anything. Retrying is
// futile — the next attempt runs under the same permission level and is refused
// identically — so the reason has to be the fix rather than a description.
//
// Which fix depends on the level it ran at, so the level is a parameter. Under
// auto or scoped the answer is bd-auto's own configuration. Under bypass there
// is no level left to widen, and the refusal came from something checked ahead
// of the level: the role's own denied_tools, a PreToolUse hook, a deny rule in
// settings, an enterprise policy.
func deniedReason(tools []string, perms runner.Permissions) string {
	head := fmt.Sprintf(
		"the worker was refused permission to use %s, and changed nothing.\n"+
			"Headless there is nobody to grant it, and another attempt would be refused identically.\n",
		strings.Join(tools, ", "))
	switch perms {
	case runner.PermBypass:
		return head + fmt.Sprintf(
			"It ran with permissions: bypass, so no permission level was in its way: look at the "+
				"role's denied_tools in %s, which are checked ahead of the level, then at a "+
				"PreToolUse hook, a deny rule in settings, or a managed policy.", config.FileName)
	case runner.PermScoped:
		return head + fmt.Sprintf(
			"It ran with permissions: scoped, so only its allowed_tools list can run. "+
				"Widen that list in %s, or give the role permissions: bypass.", config.FileName)
	default:
		return head + fmt.Sprintf(
			"Set the role's permissions to bypass in %s, or re-run with --dangerously-skip-permissions.",
			config.FileName)
	}
}

// gateFeedback turns a failed gate into instructions.
func gateFeedback(results []pipeline.Result) string {
	var b strings.Builder
	b.WriteString("The gate failed. bd-auto runs it again after every round, so this has to pass " +
		"before the work can be accepted.\n\n")
	b.WriteString(pipeline.Summary(results))
	if f := pipeline.FirstFailure(results); f != nil {
		fmt.Fprintf(&b, "\n%s (`%s`) output:\n\n%s\n", f.Name, f.Command, f.Output)
	}
	return b.String()
}

// stageFeedback turns a failed run: stage into instructions.
func stageFeedback(r pipeline.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The %s stage failed.\n\n", r.Name)
	b.WriteString(pipeline.Summary([]pipeline.Result{r}))
	if r.Output != "" {
		fmt.Fprintf(&b, "\n`%s` output:\n\n%s\n", r.Command, r.Output)
	}
	return b.String()
}

// reviewFeedback turns a verdict into instructions.
func reviewFeedback(stage, notes string, v Verdict) string {
	var b strings.Builder
	if v.Found {
		fmt.Fprintf(&b, "The %s stage failed. Its findings are your instructions for this round:\n\n", stage)
	} else {
		// Silence fails closed. The message is still usually actionable, so it
		// goes back rather than being thrown away.
		fmt.Fprintf(&b, "The %s stage returned no explicit %s pass line, which bd-auto reads as a "+
			"failure. What it did say:\n\n", stage, verdictPrefix)
	}
	body := strings.TrimSpace(v.Body)
	if body == "" {
		body = "(the stage returned nothing)"
	}
	b.WriteString(body)
	b.WriteString("\n\nAddress each point. If one is wrong, fix the rest and say why in your final message.\n")
	if notes != "" {
		fmt.Fprintf(&b, "The full text is at %s.\n", notes)
	}
	return b.String()
}

// roundsExhausted is the reason recorded when an attempt runs out of rounds.
func roundsExhausted(stage string, rounds int, feedback string) string {
	r := fmt.Sprintf("%d round(s) of feedback did not clear the %s stage", rounds, stage)
	if feedback != "" {
		r += ".\nThe last feedback was:\n" + strings.TrimSpace(feedback)
	}
	return r
}

// resultReason renders a non-verdict result for the log and the run state.
//
// A reported reset time is appended where there is one, because it is the only
// thing in an outage a human can act on: without it the reason says the
// environment failed and the obvious answer is to re-run immediately, which is
// the one answer a plan limit refuses for the next half hour.
func resultReason(res runner.Result, prefix string) string {
	r := prefix
	switch {
	case res.Err != nil:
		r = prefix + ": " + res.Err.Error()
	case res.TimedOut:
		r = prefix + ": the invocation timed out"
	}
	if n := resetNote(res.ResetAt, time.Now()); n != "" {
		r += "\n" + n
	}
	return r
}

// resetTimeFormat is how a reset time is written for a human. The day is there
// because a weekly limit resets on one, and "15:20" alone would read as this
// afternoon.
const resetTimeFormat = "Mon 15:04 MST"

// resetNote states a reset time the backend reported.
//
// Both halves are worth saying. A limit that has not lifted yet says when to
// come back and that coming back sooner is wasted; one that has already lifted
// says the opposite, and a stop reason that leaves a human guessing which of the
// two they are looking at is a stop reason that gets answered by waiting a day.
//
// Empty where nothing was reported, which is every failure but a plan limit.
func resetNote(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	if d := at.Sub(now); d > 0 {
		return fmt.Sprintf("The limit it reported lifts at %s, in %s: nothing runs before then, "+
			"so a re-run started earlier meets the same wall.", at.Format(resetTimeFormat), shortDur(d))
	}
	return fmt.Sprintf("The limit it reported has already lifted (%s), so a re-run is worth trying now.",
		at.Format(resetTimeFormat))
}

// holdNote is the log line for a round about to be re-run after an infra
// failure.
//
// It says which of the two waits this is. "retrying the same round in 26m0s"
// beside a rate limit reads as a backoff ladder that has run away with itself,
// rather than as the engine waiting out a wall whose height it was told.
func holdNote(at time.Time, wait time.Duration, now time.Time) string {
	if !at.IsZero() && at.After(now) {
		return fmt.Sprintf("the limit lifts at %s, so the round is held for %s rather than retried into it",
			at.Format(resetTimeFormat), shortDur(wait))
	}
	return fmt.Sprintf("retrying the same round in %s", shortDur(wait))
}

// shortDur renders a wait the way somebody would say it: 45s, 26m, 2h5m. The
// Duration's own spelling turns half an hour into "30m0s", which is the sort of
// detail that makes a log line look machine-generated and read as noise.
func shortDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	d = d.Round(time.Minute)
	h, m := d/time.Hour, d%time.Hour/time.Minute
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
