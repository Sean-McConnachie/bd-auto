package drain

import (
	"context"
	"fmt"
	"strings"

	"bd-auto/internal/bd"
	"bd-auto/internal/pipeline"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
)

// The handoff is the last thing a run does and the only thing it publishes.
//
// Everything before it is reversible and local: branches in one checkout, a
// worktree per issue, merges onto a branch nobody else has seen. This is the
// step that crosses that line, so it is the step with a predicate in front of
// it rather than a condition written inline. HandoffReady is a pure function of
// the run's own report, and it is deliberately conservative: a parked issue, a
// red gate or a run that stopped rather than finished all mean the same thing
// here, which is that a human should look at the branch instead of a review
// request appearing in their queue as if the work were done.
//
// Nothing about a refused handoff is destructive. The epic branch is left
// exactly where it is, with everything that landed on it, which is the whole
// reason a run can be handed over later by hand.

// StageHandoff is the stage name recorded for the terminal handoff.
const StageHandoff = "handoff"

// HandoffReport is what the terminal handoff produced.
type HandoffReport struct {
	// Branch is the epic branch the run was staged on, empty for a run that
	// merged straight into its base branch.
	Branch string `json:"branch,omitempty"`
	// Base is the branch a pull request targets, and Head where the epic branch
	// stands.
	Base string `json:"base,omitempty"`
	Head string `json:"head,omitempty"`
	// Issues is what landed on the branch, in the order the barriers merged it.
	Issues []string `json:"issues,omitempty"`

	// Remote is where the branch was pushed, and Pushed whether it got there.
	Remote string `json:"remote,omitempty"`
	Pushed bool   `json:"pushed"`
	// URL is the pull request, and Created reports whether this run opened it
	// rather than finding one already open for the same branch.
	URL     string `json:"url,omitempty"`
	Created bool   `json:"created"`

	// ByHand marks a handoff run after the fact by `bd-auto handoff`, rather
	// than by the drain that produced the branch. The document it writes is
	// narrower for it: run state does not keep everything a live report holds.
	ByHand bool `json:"by_hand,omitempty"`
	// Forced is the refusal a human overrode with --force, empty when the
	// predicate opened this of its own accord. It is kept, and it is printed in
	// the pull request, because a review request bd-auto would not have made is
	// the one thing a reviewer must not have to work out for themselves.
	Forced string `json:"forced,omitempty"`
	// Forceable reports that --force would have opened this one. It is the
	// predicate's own answer rather than a label on the reason — see
	// forcedReady — and it says only that overriding is possible, never that it
	// is a good idea.
	Forceable bool `json:"forceable,omitempty"`

	// Reason says why there is a pull request, or why there is not.
	Reason string `json:"reason"`
}

// HandoffVerdict is the decision to open a pull request and the one condition
// that decided it.
type HandoffVerdict struct {
	Open   bool   `json:"open"`
	Reason string `json:"reason"`
}

// HandoffReady decides whether a finished run may be handed over as a pull
// request. It is a pure function of the run's report, the branch it was staged
// on and whether pull requests are on at all.
//
// Every one of these must hold, and the first that does not is the reason:
//
//   - The run was staged. There is nothing to open a pull request from when the
//     merges went straight into the base branch.
//   - Pull requests are on. Off, the epic branch is the whole deliverable.
//   - The run finished on its own terms. An interrupt or an outage is not a
//     verdict on the work, so it is not a result to ask anyone to review.
//   - Nothing is parked. Parked work is required work that did not get done,
//     and a pull request over it claims the epic is finished when it is not.
//   - The gate passed at the last barrier. Never hand over a red tree.
//   - Something actually landed. An empty branch is not a handoff.
//
// Three of them are facts a human has no argument with — there is no epic
// branch, no barrier ran, nothing landed — and the rest are judgements about the
// run, which --force may override. See forcedReady.
func HandoffReady(rep DrainReport, staged string, prEnabled bool) HandoffVerdict {
	landed := rep.Landed()
	switch {
	case staged == "":
		return HandoffVerdict{Reason: "this run merged into " + nameOr(rep.Base, "its base branch") +
			" directly, so there is no epic branch to open a pull request from"}
	case !prEnabled:
		return HandoffVerdict{Reason: "pull requests are off (handoff.pr); " + staged +
			" is left in place with everything that landed on it"}
	case rep.Outcome != OutcomeDone:
		return HandoffVerdict{Reason: fmt.Sprintf("the run ended %s rather than finishing, so %s is a partial result",
			rep.Outcome, staged)}
	case len(rep.Parked) > 0:
		return HandoffVerdict{Reason: fmt.Sprintf(
			"%d parked issue(s) are required work that did not get done: %s",
			len(rep.Parked), strings.Join(sorted(rep.Parked), ", "))}
	case len(rep.Integrations) == 0:
		return HandoffVerdict{Reason: "no wave reached a barrier, so nothing was merged onto " + staged}
	case !rep.Integrations[len(rep.Integrations)-1].GatePassed:
		return HandoffVerdict{Reason: "the gate is red on " + staged}
	case len(landed) == 0:
		return HandoffVerdict{Reason: "no branch landed on " + staged + "; there is nothing to hand over"}
	}
	return HandoffVerdict{Open: true, Reason: fmt.Sprintf(
		"%d issue(s) landed on %s and the gate passed on the merged result", len(landed), staged)}
}

// forcedReady asks the same predicate what it would say if every judgement about
// the run went the other way: the run finished, nothing is parked, the gate is
// green, pull requests are on. What refuses anyway is what --force cannot argue
// with — there is no epic branch, or there is nothing on it — and a refusal that
// survives this is a refusal that stands.
//
// It is done by re-asking rather than by listing the two conditions here, and
// that is the point. HandoffReady stops at the first condition that fails, so a
// hand-kept list of which refusals are overridable would let --force past
// "something is parked" and straight into publishing an empty branch, and a
// condition added to the predicate later would default to overridable without
// anyone deciding that it should.
func forcedReady(rep DrainReport, staged string) HandoffVerdict {
	rep.Outcome = OutcomeDone
	rep.Parked = nil
	// Copied: the report is a value but its integrations are not, and a
	// hypothetical must not colour in the report the caller is about to publish.
	in := append([]IntegrateReport(nil), rep.Integrations...)
	for i := range in {
		in[i].GatePassed = true
	}
	rep.Integrations = in
	return HandoffReady(rep, staged, true)
}

// HandoffOptions are the knobs on a handoff, and both of them exist only for
// the handoff a human runs by hand once the drain is over.
type HandoffOptions struct {
	// Force opens the pull request over a forceable refusal.
	Force bool
	// ByHand records that this is not a drain publishing its own result.
	ByHand bool
}

// Handoff hands a finished run to a human.
//
// It never returns an error, and that is the point rather than an omission. By
// the time it runs the work is already committed to a branch in the checkout,
// so nothing it can fail at makes that work any less finished: a missing gh, a
// remote that refuses the push, a forge that will not open the request. Each of
// those is recorded as the reason there is no pull request, and the branch is
// left where a human can pick it up.
func (e *Engine) Handoff(ctx context.Context, rep DrainReport) HandoffReport {
	return e.HandoffWith(ctx, rep, HandoffOptions{})
}

// HandoffWith is Handoff with the knobs a hand-run handoff needs. A drain never
// sets either of them: a run that publishes its own result over its own
// predicate would leave the predicate meaning nothing.
func (e *Engine) HandoffWith(ctx context.Context, rep DrainReport, opts HandoffOptions) HandoffReport {
	h := HandoffReport{Branch: rep.EpicBranch, Base: rep.Base, Issues: rep.Landed(), ByHand: opts.ByHand}
	if h.Branch != "" {
		h.Head, _ = git(e.RepoRoot, "rev-parse", h.Branch)
	}

	v := HandoffReady(rep, h.Branch, e.Cfg.OpenPR())
	h.Reason = v.Reason
	if !v.Open {
		forced := forcedReady(rep, h.Branch)
		h.Forceable = forced.Open
		switch {
		case !opts.Force:
			if h.Branch != "" {
				e.logf("no pull request: %s", v.Reason)
			}
			return h
		case !forced.Open:
			h.Reason = forced.Reason + ", and --force cannot change that"
			e.logf("no pull request: %s", h.Reason)
			return h
		}
		h.Forced = v.Reason
		h.Reason = "opened by hand over bd-auto's own refusal: " + v.Reason
		e.logf("--force: %s", h.Reason)
	}

	f := e.forge()
	h.Remote = e.Cfg.HandoffRemote()
	if why := f.Available(e.RepoRoot, h.Remote); why != "" {
		h.Reason = fmt.Sprintf("%s is ready to hand over but %s, so it is left in place", h.Branch, why)
		e.logf("no pull request: %s", h.Reason)
		return h
	}
	if err := f.Push(ctx, e.RepoRoot, h.Remote, h.Branch); err != nil {
		h.Reason = fmt.Sprintf("%s could not be pushed to %s, so no pull request was opened: %v",
			h.Branch, h.Remote, err)
		e.logf("no pull request: %s", h.Reason)
		return h
	}
	h.Pushed = true

	url, created, err := f.Open(ctx, e.RepoRoot, PullRequest{
		Base:  h.Base,
		Head:  h.Branch,
		Title: pullRequestTitle(rep),
		Body:  e.pullRequestBody(rep, h),
	})
	if err != nil {
		h.Reason = fmt.Sprintf("%s is pushed to %s but the pull request would not open: %v",
			h.Branch, h.Remote, err)
		e.logf("no pull request: %s", h.Reason)
		return h
	}
	h.URL, h.Created = url, created
	if !created {
		h.Reason = "a pull request was already open for " + h.Branch + "; it now carries this run's work"
	}
	e.logf("handed over at %s: %s", url, h.Reason)
	e.recordPR(url)
	return h
}

// recordPR keeps the pull request in run state, so `bd-auto run status` can say
// where the run went after the process that ran it is gone.
func (e *Engine) recordPR(url string) {
	if url == "" {
		return
	}
	_, err := runstate.Update(e.RepoRoot, false, func(s *runstate.State) error {
		s.PR = url
		s.Note("handed over at %s", url)
		return nil
	})
	if err != nil {
		e.logf("warning: could not record the pull request: %v", err)
	}
}

// --- the handoff document ---

func pullRequestTitle(rep DrainReport) string {
	n := len(rep.Landed())
	if rep.Epic != "" {
		return fmt.Sprintf("%s: %d issue(s) drained by bd-auto", rep.Epic, n)
	}
	return fmt.Sprintf("bd-auto: %d issue(s) drained", n)
}

// pullRequestBody is the handoff document.
//
// Whoever opens it did not watch the run, so it answers, in this order, the
// questions they are about to ask: what landed, whether it is green, which
// parts a model had to make a judgement call on, and what the run found that it
// deliberately did not do. The resolved merges are called out separately from
// the clean ones because they are the only places in the diff where a model
// chose between two correct changes, and that is where a reviewer's attention
// is worth the most.
func (e *Engine) pullRequestBody(rep DrainReport, h HandoffReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "`bd-auto` drained %d issue(s) onto `%s` in %d wave(s) and is asking for a human to land it.\n",
		len(h.Issues), h.Branch, rep.Waves)
	fmt.Fprintf(&b, "Nothing has been merged into `%s`: this branch is the whole result.\n", h.Base)

	if h.ByHand {
		b.WriteString("\nThis was opened after the fact with `bd-auto handoff`, not by the run itself. " +
			"The branch is the same one the run built; what a live run also reports — which merges a model " +
			"resolved, what the whole thing cost — is not in run state, so it is missing here rather than guessed at.\n")
	}
	if h.Forced != "" {
		fmt.Fprintf(&b, "\n> [!WARNING]\n> **Forced.** bd-auto would not have opened this: %s.\n"+
			"> A human passed `--force` after looking at the branch.\n", h.Forced)
	}

	b.WriteString("\n## Issues\n\n")
	titles := e.titles(h.Issues)
	for _, id := range h.Issues {
		if t := titles[id]; t != "" {
			fmt.Fprintf(&b, "- `%s` — %s\n", id, t)
		} else {
			fmt.Fprintf(&b, "- `%s`\n", id)
		}
	}

	if resolved := resolvedMerges(rep); len(resolved) > 0 {
		b.WriteString("\n## Conflicts a model resolved\n\n")
		b.WriteString("Each of these merged two changes that were correct alone. " +
			"They are the parts of this diff worth reading closely.\n\n")
		for _, m := range resolved {
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", m.Issue, m.Branch, strings.Join(m.Conflicts, ", "))
		}
	}

	b.WriteString("\n## Gate\n\n")
	b.WriteString(gateSection(rep))

	if found := e.discovered(h.Issues); len(found) > 0 {
		b.WriteString("\n## Discovered, not done\n\n")
		b.WriteString("Filed by the workers and deliberately kept out of this run:\n\n")
		for _, d := range found {
			fmt.Fprintf(&b, "- `%s` — %s\n", d.ID, d.Title)
		}
	}

	b.WriteString("\n## Run\n\n")
	fmt.Fprintf(&b, "- %d wave(s), %d issue(s) landed, %s\n", rep.Waves, len(h.Issues), parkedLine(rep.Parked))
	if adrift := notLanded(rep.Done, h.Issues); len(adrift) > 0 {
		fmt.Fprintf(&b, "- %d issue(s) the run finished are NOT on this branch: %s\n",
			len(adrift), strings.Join(adrift, ", "))
	}
	fmt.Fprintf(&b, "- %s\n", usageLine(rep.Usage, rep.Seconds))
	if rep.EpicClosed && rep.Epic != "" {
		fmt.Fprintf(&b, "- `%s` is closed in beads: %s\n", rep.Epic, rep.EpicReason)
	}
	return b.String()
}

// gateSection renders the gate as it stood at the last barrier: the one run
// that asked whether every branch works together rather than alone.
func gateSection(rep DrainReport) string {
	if len(rep.Integrations) == 0 {
		return "No barrier ran.\n"
	}
	last := rep.Integrations[len(rep.Integrations)-1]
	if len(last.Gate) == 0 {
		return "No gate is configured for this repo, so nothing was proved beyond the merges themselves.\n"
	}
	// Only a forced handoff ever gets here red. It is the one line of this
	// document a reviewer would act on without reading further, so it says which
	// it is rather than assuming the predicate already refused.
	head := "Green on the merged result:"
	if !pipeline.Passed(last.Gate) {
		head = "**RED on the merged result.** This branch does not pass its own gate:"
	}
	return head + "\n\n```\n" + pipeline.Summary(last.Gate) + "```\n"
}

// parkedLine is the parked half of the run line. It is derived rather than
// written down because a forced handoff is a handoff over parked work, and the
// one thing that document must not do is say there is none.
func parkedLine(parked []string) string {
	if len(parked) == 0 {
		return "nothing parked"
	}
	return fmt.Sprintf("%d PARKED: %s", len(parked), strings.Join(sorted(parked), ", "))
}

// notLanded lists what a run finished that is not in the merged result. It is
// empty for a run that reached its last barrier, and not for one that stopped
// between a worker closing an issue and the barrier that would have merged it.
func notLanded(done, landed []string) []string {
	in := map[string]bool{}
	for _, id := range landed {
		in[id] = true
	}
	var out []string
	for _, id := range done {
		if !in[id] {
			out = append(out, id)
		}
	}
	return sorted(out)
}

// resolvedMerges lists every merge a model had to resolve, across every wave.
func resolvedMerges(rep DrainReport) []Merge {
	var out []Merge
	for _, in := range rep.Integrations {
		for _, m := range in.Merges {
			if m.Outcome == MergeResolved {
				out = append(out, m)
			}
		}
	}
	return out
}

// titles looks up issue titles for the body. A lookup that fails costs the
// title and nothing else: the ID is already the useful half.
func (e *Engine) titles(ids []string) map[string]string {
	out := map[string]string{}
	if e.BD == nil {
		return out
	}
	for _, id := range ids {
		if iss, err := e.BD.Show(id); err == nil && iss != nil {
			out[id] = iss.Title
		}
	}
	return out
}

// discovered lists the work the run's own issues turned up.
//
// A worker files discovered work with a discovered-from dependency and defers
// it, so it never enters the run — which is correct, and also means it would
// otherwise leave no trace in the handoff at all. It is read off the landed
// issues' dependents rather than queried separately, because that is where bd
// records the edge and the issues are already being read for their titles.
func (e *Engine) discovered(ids []string) []bd.Ref {
	if e.BD == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	var out []bd.Ref
	for _, id := range ids {
		iss, err := e.BD.Show(id)
		if err != nil || iss == nil {
			continue
		}
		for _, d := range iss.Dependents {
			if d.ID == "" || seen[d.ID] || !strings.Contains(d.Type, "discovered") {
				continue
			}
			seen[d.ID] = true
			out = append(out, d)
		}
	}
	return out
}

// usageLine renders what the run cost, or says nothing was reported rather than
// printing a confident zero.
func usageLine(u runner.Usage, seconds float64) string {
	if u.IsZero() {
		return fmt.Sprintf("%.0fs; the backend reported no usage", seconds)
	}
	line := fmt.Sprintf("%.0fs, $%.2f, %d in / %d out tokens",
		seconds, u.CostUSD, u.InputTokens, u.OutputTokens)
	if u.Turns > 0 {
		line += fmt.Sprintf(", %d turns", u.Turns)
	}
	return line
}
