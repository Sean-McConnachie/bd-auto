package drain

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/pipeline"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
	"bd-auto/internal/wave"
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
			"Fix exactly this. Do not start over and do not revert your own earlier work.\n" +
			"Commit the fix to " + t.Branch + ", make sure `bd show " + t.ID + "` reports the issue closed, and stop."
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
	b.WriteString("\nRun `bd show " + t.ID + "` for the description, design, acceptance criteria and notes.\n")
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
	if resume {
		return fmt.Sprintf(
			"The worker has pushed another round of changes to %s for issue %s.\n"+
				"Re-read `git diff %s...HEAD` as it stands now and return a fresh verdict.",
			t.Branch, t.ID, t.Base)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Review the work on branch %s for beads issue %s.\n\n", t.Branch, t.ID)
	if t.Issue != nil && t.Issue.Title != "" {
		fmt.Fprintf(&b, "Title:     %s\n", t.Issue.Title)
	}
	fmt.Fprintf(&b, "Issue:     %s\n", t.ID)
	fmt.Fprintf(&b, "Branch:    %s\n", t.Branch)
	fmt.Fprintf(&b, "Base:      %s\n", t.Base)
	fmt.Fprintf(&b, "Worktree:  %s (your working directory)\n", t.Worktree)
	fmt.Fprintf(&b, "Stage:     %s\n", s.Stage)
	fmt.Fprintf(&b, "\nRead the issue with `bd show %s` and the change with `git diff %s...HEAD`.\n", t.ID, t.Base)
	b.WriteString("The acceptance criteria on the issue are the standard, not your own taste.\n")
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
	b.WriteString("\nConflicted files:\n")
	for _, p := range m.Conflicts {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	fmt.Fprintf(&b, "\nRead both sides with `git diff --diff-filter=U`, and `bd show %s` for what this "+
		"branch was for. `git log` shows what has already merged ahead of it.\n", m.Issue)
	b.WriteString("Resolve every file listed above, `git add` each one, and stop there. " +
		"Do not create the merge commit and do not abort: bd-auto inspects the tree you leave " +
		"and completes or abandons the merge itself.\n")
	return b.String()
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
func reviewNotes(t task, s config.Stage, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", t.ID, s.Stage)
	fmt.Fprintf(&b, "- branch: `%s`\n- base: `%s`\n- attempt %d, round %d\n- %s\n\n",
		t.Branch, t.Base, t.Attempt, t.Round+1, time.Now().UTC().Format(time.RFC3339))
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n")
	return b.String()
}

// notClosedFeedback is what a worker gets for stopping without finishing.
func notClosedFeedback(id, status string) string {
	return fmt.Sprintf(
		"You stopped without finishing: `bd show %s` reports status %q, and bd is what bd-auto reads.\n"+
			"If the work is done, close it: `bd close %s`.\n"+
			"If you are genuinely blocked, say so on the issue: "+
			"`bd update %s --status=blocked --append-notes=\"<what blocked you>\"`.\n"+
			"Do not re-do the work you have already done.",
		id, status, id, id)
}

// selfParkReason is why an issue stops when its own worker set it to blocked.
//
// What the worker said is the whole point of it: the alternative is a human
// reading "bd-auto parked this after 2 attempts" about an issue that was
// answered on the first one, at the first time of asking.
//
// The note is asked for first because it is the field prompts/worker.md sends a
// blocked worker to, so it holds this and nothing else. carriedFailure distrusts
// notes for a different case than this one: what reverts them is the next
// attempt's worktree being created, and this reads the note in the round that
// wrote it. Where bd lost it anyway, the worker's final message says the same
// thing at more length, and is the copy bd-auto holds itself.
func selfParkReason(id, notes, text string) string {
	head := fmt.Sprintf("the worker set %s to blocked rather than closing it, so bd-auto stopped here: "+
		"a worker that says it cannot do the work answers a retry the same way at the same price.", id)
	said := workerNote(notes)
	if said == "" {
		said = strings.TrimSpace(text)
	}
	if said == "" {
		return head + "\nIt gave no reason."
	}
	return head + "\nWhat the worker said:\n" + said
}

// workerNote is what a worker wrote on its own issue when it parked itself.
//
// prompts/worker.md asks a blocked worker to append its reason under the same
// marker bd-auto uses for its own failure notes, so the last note under that
// marker is usually the worker's. Usually, not always: on a retry bd-auto's
// account of the previous attempt is under it too, and returning that as the
// worker's would put words in its mouth. noteFailure writes those in one shape
// — "<marker> N/M failed at stage ..." — so they are skipped rather than
// quoted. Empty where the notes hold nothing a worker wrote, which includes
// every issue read back through a bd that lost the write.
func workerNote(notes string) string {
	parts := strings.Split(notes, wave.NoteMarker)
	for i := len(parts) - 1; i >= 1; i-- {
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(parts[i]), ":"))
		if body == "" || engineNote.MatchString(body) {
			continue
		}
		return body
	}
	return ""
}

// engineNote matches the opening of a note bd-auto wrote about its own attempt.
// See noteFailure for the shape it is reading.
var engineNote = regexp.MustCompile(`^\d+/\d+ failed at stage `)

// selfParkNote is what bd-auto records on an issue its worker parked.
//
// It says only what bd-auto did, not why: the worker's own account is already
// on the issue and in the run's record, and appending a second copy of it would
// leave the next reader working out which one is the original.
func selfParkNote(id, branch string, attempt, allowed int) string {
	return fmt.Sprintf("bd-auto parked %s: its worker set it to blocked on attempt %d of %d rather than "+
		"closing it. The remaining attempt(s) were not spent and %s was not merged. "+
		"Unpark it with `bd-auto run unpark --issue %s` once whatever blocked it is resolved.",
		id, attempt, allowed, branch, id)
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
// is nothing left here to widen, and the refusal came from something outside
// this tool — a PreToolUse hook, a deny rule, an enterprise policy — so sending
// someone to .beads-auto.yaml would send them to the one file that cannot be
// the cause.
func deniedReason(tools []string, perms runner.Permissions) string {
	head := fmt.Sprintf(
		"the worker was refused permission to use %s, and changed nothing.\n"+
			"Headless there is nobody to grant it, and another attempt would be refused identically.\n",
		strings.Join(tools, ", "))
	switch perms {
	case runner.PermBypass:
		return head + "It ran with permissions: bypass, so this refusal did not come from bd-auto: " +
			"look for a PreToolUse hook, a deny rule in settings, or a managed policy."
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
