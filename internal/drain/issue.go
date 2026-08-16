package drain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/gitguard"
	"bd-auto/internal/pipeline"
	"bd-auto/internal/runner"
	"bd-auto/internal/wave"
	"bd-auto/internal/worktree"
)

// task is everything a round needs to describe itself to a model.
type task struct {
	Issue    *bd.Issue
	ID       string
	Branch   string
	Worktree string
	Base     string
	Attempt  int
	Round    int
}

// Issue runs one issue to a terminal outcome: done, parked, interrupted, or
// stopped on the environment.
//
// It returns an error only for a failure that is not about the work — an
// unreachable bd, a worktree that cannot be created, a runner that cannot be
// built. Everything the issue itself can fail at comes back in the Report.
func (e *Engine) Issue(ctx context.Context, id string) (Report, error) {
	started := time.Now()
	switch {
	case e.RepoRoot == "":
		return Report{}, errors.New("drain: RepoRoot is required")
	case e.Cfg == nil:
		return Report{}, errors.New("drain: Cfg is required")
	case e.BD == nil:
		return Report{}, errors.New("drain: BD is required")
	case id == "":
		return Report{}, errors.New("drain: issue is required")
	}

	iss, err := e.BD.Show(id)
	if err != nil {
		return Report{}, fmt.Errorf("drain: %s: %w", id, err)
	}

	branch := e.Cfg.Branch(id)
	rep := Report{Issue: id, Branch: branch, Worktree: worktree.Path(e.RepoRoot, id)}

	// The baseline is recorded before anything runs. Recording it afterwards
	// compares the damage against itself.
	baseline, err := gitguard.Record(e.RepoRoot, id, branch, e.baseRef())
	if err != nil {
		return rep, err
	}
	rep.Base = baseline.Base

	allowed := e.attempts()
	for n := 1; n <= allowed; n++ {
		t := task{Issue: iss, ID: id, Branch: branch, Base: baseline.Base, Attempt: n}
		at, err := e.attempt(ctx, t, baseline)
		rep.Attempts = append(rep.Attempts, at)
		rep.Usage = rep.Usage.Add(at.Usage)
		rep.Stage, rep.Reason = at.Stage, at.Reason
		if err != nil {
			rep.Outcome = OutcomeFailed
			rep.Seconds = time.Since(started).Seconds()
			return rep, err
		}

		switch at.Outcome {
		case OutcomeDone:
			rep.Outcome, rep.Stage, rep.Reason = OutcomeDone, "", ""
			rep.Seconds = time.Since(started).Seconds()
			return rep, e.recordDone(id)
		case OutcomeInterrupted, OutcomeInfra:
			// Neither is a verdict on the work, so the worktree, the branch and
			// the attempt counter are all left exactly as they are.
			rep.Outcome = at.Outcome
			rep.Seconds = time.Since(started).Seconds()
			return rep, nil
		}

		e.noteFailure(id, n, allowed, at)
		if n == allowed {
			break
		}

		// Only here, between attempts. Wiping the worktree between rounds is
		// what would make resuming a session pointless.
		e.logf("%s: attempt %d failed at %s; discarding the worktree and retrying fresh", id, n, at.Stage)
		if err := e.discardAttempt(id, branch); err != nil {
			e.logf("warning: could not discard %s attempt %d: %v", id, n, err)
		}
		if err := e.BD.Reset(id); err != nil {
			e.logf("warning: could not return %s to the ready queue: %v", id, err)
		}
	}

	rep.Outcome = OutcomeParked
	rep.Seconds = time.Since(started).Seconds()
	reason := fmt.Sprintf("bd-auto parked %s after %d attempt(s). Last failure at stage %q: %s",
		id, allowed, stageOr(rep.Stage), rep.Reason)
	if err := e.BD.Park(id, reason); err != nil {
		e.logf("warning: could not park %s: %v", id, err)
	}
	return rep, e.recordParked(id, rep.Reason, stageOr(rep.Stage))
}

// attempt is one worktree, one session and up to max_rounds turns inside it.
//
// The check order below is the design: progress first and fatal, then the
// checks that stale state can satisfy.
func (e *Engine) attempt(ctx context.Context, t task, baseline gitguard.Baseline) (Attempt, error) {
	started := time.Now()
	out := Attempt{Attempt: t.Attempt}

	wt, err := worktree.Ensure(e.RepoRoot, t.ID, t.Branch, t.Base)
	if err != nil {
		return out, err
	}
	t.Worktree = wt
	if err := gitguard.Setup(e.RepoRoot, wt, gitguard.Worker{Issue: t.ID, Attempt: t.Attempt}); err != nil {
		return out, err
	}

	rn, err := e.runnerFor(runner.RoleWorker)
	if err != nil {
		return out, err
	}
	sess := &session{}
	stageSessions := map[string]*session{}
	canResume := e.resumes(runner.RoleWorker, rn)

	var feedback, stage string
	stageRounds := map[string]int{}
	budget := e.rounds()

	finish := func(o Outcome, st, reason string) (Attempt, error) {
		out.Outcome, out.Stage, out.Reason = o, st, reason
		out.Session = sess.ID
		out.Seconds = time.Since(started).Seconds()
		return out, nil
	}

	for round := 0; round < budget; {
		// A stage that has used its own budget ends the attempt, even where the
		// loop still has rounds left.
		if stage != "" {
			if n := e.stageBudget(stage); n > 0 && stageRounds[stage] >= n {
				return finish(OutcomeFailed, stage, roundsExhausted(stage, stageRounds[stage], feedback))
			}
		}

		mark := worktree.Snapshot(wt)
		t.Round = round
		fb, st := feedback, stage
		c, err := e.invoke(ctx, invocation{
			Issue:     t.ID,
			Branch:    t.Branch,
			Attempt:   t.Attempt,
			Role:      runner.RoleWorker,
			Runner:    rn,
			Sess:      sess,
			CanResume: canResume,
			Build:     func(resume bool) runner.Request { return e.workerRequest(t, resume, st, fb) },
		})
		out.Usage = out.Usage.Add(c.Usage)
		out.InfraRetries += absorbed(c)
		if err != nil {
			return out, err
		}

		switch c.Result.Class {
		case runner.ClassInterrupted:
			return finish(OutcomeInterrupted, StageImplement, resultReason(c.Result, "the worker was interrupted"))
		case runner.ClassInfraFailed:
			return finish(OutcomeInfra, StageImplement, resultReason(c.Result, "the worker kept failing on the environment"))
		}

		// Only work the model actually did counts.
		round++
		out.Rounds = round
		if err := e.recordRound(t.ID, t.Attempt, round, StageImplement); err != nil {
			return out, err
		}

		// First, and fatal. Every check below is satisfiable by the previous
		// round's state, so a round that changed nothing would pass them all and
		// then spend the rest of the budget proving it again.
		if !worktree.Changed(wt, mark) {
			return finish(OutcomeFailed, StageImplement, noProgressReason(t.Round))
		}

		cur, err := e.BD.Show(t.ID)
		if err != nil {
			return out, fmt.Errorf("drain: %s: %w", t.ID, err)
		}
		if !cur.Terminal() {
			feedback, stage = notClosedFeedback(t.ID, cur.Status), StageImplement
			stageRounds[stage]++
			continue
		}

		if v := gitguard.Verify(e.RepoRoot, baseline); !v.OK {
			feedback, stage = v.Reason(), StageGuard
			stageRounds[stage]++
			continue
		}

		sr, err := e.runStages(ctx, t, stageSessions)
		out.Usage = out.Usage.Add(sr.Usage)
		out.InfraRetries += sr.InfraRetries
		if err != nil {
			return out, err
		}
		switch sr.Control {
		case runner.ClassInterrupted:
			return finish(OutcomeInterrupted, sr.Stage, sr.Feedback)
		case runner.ClassInfraFailed:
			return finish(OutcomeInfra, sr.Stage, sr.Feedback)
		}
		if !sr.Passed {
			feedback, stage = sr.Feedback, sr.Stage
			stageRounds[stage]++
			continue
		}

		return finish(OutcomeDone, "", "")
	}

	return finish(OutcomeFailed, stageOr(stage), roundsExhausted(stageOr(stage), budget, feedback))
}

// --- stages ---

// stageOutcome is what one stage produced.
type stageOutcome struct {
	Passed   bool
	Feedback string
	Usage    runner.Usage
	// InfraRetries is how many processes this stage burned on the environment.
	InfraRetries int
	// Control is non-empty when a model stage never reached a verdict at all:
	// the class that says why, which the caller routes rather than judges.
	Control runner.Class
}

// stagesResult is what the post-implement half of the pipeline produced.
type stagesResult struct {
	stageOutcome
	Stage string
}

// runStages runs every pipeline stage after implement, in order, stopping at
// the first one that fails.
//
// The gate and the review are not special cases here: they are what the default
// pipeline contains, and a repo that adds a stage gets it fed through the same
// feedback channel as the ones that shipped.
func (e *Engine) runStages(ctx context.Context, t task, sessions map[string]*session) (stagesResult, error) {
	out := stagesResult{stageOutcome: stageOutcome{Passed: true}}
	for _, s := range e.Cfg.Pipeline {
		var (
			so  stageOutcome
			err error
		)
		switch s.Kind() {
		case "builtin-implement":
			continue
		case "builtin-gate":
			so = e.gate(t)
		case "run":
			so = e.runCommandStage(t, s)
		case "agent":
			so, err = e.agentStage(ctx, t, s, sessions)
		default:
			// Validate rejects this at load time; reaching it means the config
			// changed underneath the run.
			return out, fmt.Errorf("drain: stage %q is neither a command nor a role", s.Stage)
		}
		out.Usage = out.Usage.Add(so.Usage)
		out.InfraRetries += so.InfraRetries
		if err != nil {
			return out, err
		}
		if so.Control != "" {
			out.Stage, out.Passed, out.Feedback, out.Control = s.Stage, false, so.Feedback, so.Control
			return out, nil
		}
		if so.Passed {
			continue
		}
		if s.Optional {
			e.logf("%s: optional stage %s failed; continuing", t.ID, s.Stage)
			continue
		}
		out.Stage, out.Passed, out.Feedback = s.Stage, false, so.Feedback
		return out, nil
	}
	return out, nil
}

// gate runs the configured gate commands inside the worktree. A repo with no
// gate passes trivially, which is what makes bd-auto usable in a repo with no
// test suite.
func (e *Engine) gate(t task) stageOutcome {
	if !e.Cfg.HasGate() {
		return stageOutcome{Passed: true}
	}
	results := pipeline.Gate(e.Cfg, e.env(t))
	if pipeline.Passed(results) {
		return stageOutcome{Passed: true}
	}
	return stageOutcome{Feedback: gateFeedback(results)}
}

// runCommandStage executes one run: stage. The diff is materialised for it and
// removed afterwards, which is why this is a function rather than a case.
func (e *Engine) runCommandStage(t task, s config.Stage) stageOutcome {
	env := e.env(t)
	if p, err := pipeline.WriteDiff(t.Worktree, t.Base); err == nil {
		env.DiffFile = p
		defer os.Remove(p)
	}
	r := pipeline.Exec(s.Stage, s.Run, s.Timeout, e.Cfg.OutputTailBytes, env)
	if r.Passed {
		return stageOutcome{Passed: true}
	}
	return stageOutcome{Feedback: stageFeedback(r)}
}

// agentStage spawns a model to judge the work.
//
// It returns the verdict, what it cost, and — when the model never reached a
// verdict at all — the class that says why, so the caller can route an outage
// rather than reading it as a failed review.
func (e *Engine) agentStage(ctx context.Context, t task, s config.Stage, sessions map[string]*session) (stageOutcome, error) {
	role := runner.Role(e.Cfg.Role(s.Agent))
	rn, err := e.runnerFor(role)
	if err != nil {
		return stageOutcome{}, err
	}
	sess := sessions[s.Stage]
	if sess == nil {
		sess = &session{}
		sessions[s.Stage] = sess
	}

	c, err := e.invoke(ctx, invocation{
		Issue:     t.ID,
		Branch:    t.Branch,
		Attempt:   t.Attempt,
		Role:      role,
		Runner:    rn,
		Sess:      sess,
		CanResume: e.resumes(role, rn),
		Build:     func(resume bool) runner.Request { return e.reviewRequest(t, s, role, resume) },
	})
	out := stageOutcome{Usage: c.Usage, InfraRetries: absorbed(c)}
	if err != nil {
		return out, err
	}

	switch c.Result.Class {
	case runner.ClassInterrupted:
		out.Control = runner.ClassInterrupted
		out.Feedback = resultReason(c.Result, "the "+s.Stage+" stage was interrupted")
		return out, nil
	case runner.ClassInfraFailed:
		out.Control = runner.ClassInfraFailed
		out.Feedback = resultReason(c.Result, "the "+s.Stage+" stage kept failing on the environment")
		return out, nil
	}

	notes := ReviewNotesPath(e.RepoRoot, t.ID)
	if werr := writeFile(notes, reviewNotes(t, s, c.Result.Text)); werr != nil {
		e.logf("warning: could not write review notes for %s: %v", t.ID, werr)
		notes = ""
	}

	if v := ParseVerdict(c.Result.Text); v.Pass {
		out.Passed = true
	} else {
		out.Feedback = reviewFeedback(s.Stage, notes, v)
	}
	return out, nil
}

// absorbed is how many of a call's processes went on the environment rather
// than on the work: every one but the last.
func absorbed(c call) int {
	if c.Procs < 1 {
		return 0
	}
	return c.Procs - 1
}

func (e *Engine) env(t task) pipeline.Env {
	return pipeline.Env{Issue: t.ID, Branch: t.Branch, Dir: t.Worktree, RepoRoot: e.RepoRoot}
}

// --- requests ---

func (e *Engine) workerRequest(t task, resume bool, stage, feedback string) runner.Request {
	req := e.Cfg.Runner(string(runner.RoleWorker)).Request(runner.RoleWorker)
	req.Dir = t.Worktree
	req.SystemPrompt = e.promptFor(runner.RoleWorker)
	req.Prompt = workerPrompt(t, resume, stage, feedback)
	req.LogPath = LogPath(e.RepoRoot, t.ID, t.Attempt, t.Round, runner.RoleWorker)
	return req
}

func (e *Engine) reviewRequest(t task, s config.Stage, role runner.Role, resume bool) runner.Request {
	req := e.Cfg.Runner(string(role)).Request(role)
	req.Dir = t.Worktree
	req.SystemPrompt = e.promptFor(role)
	req.Prompt = reviewPrompt(t, s, resume)
	req.LogPath = LogPath(e.RepoRoot, t.ID, t.Attempt, t.Round, runner.Role(s.Stage))
	return req
}

// --- bookkeeping ---

// discardAttempt removes a failed attempt's worktree and branch so the retry
// starts from the base rather than inheriting half-done work.
//
// It is called between attempts and nowhere else.
func (e *Engine) discardAttempt(issue, branch string) error {
	if err := worktree.Remove(e.RepoRoot, issue); err != nil {
		return err
	}
	if !branchExists(e.RepoRoot, branch) {
		return nil
	}
	_, err := git(e.RepoRoot, "branch", "-D", branch)
	return err
}

// noteFailure records an attempt on the issue itself, so the next worker starts
// informed and the evidence outlives any process.
//
// Safe to write here: the attempt has finished, which keeps this inside the
// one-writer-per-issue rule bd's unlocked notes field imposes.
func (e *Engine) noteFailure(id string, n, allowed int, at Attempt) {
	note := fmt.Sprintf("%s %d/%d failed at stage %q on %s:\n%s",
		wave.NoteMarker, n, allowed, stageOr(at.Stage), time.Now().UTC().Format(time.RFC3339), at.Reason)
	if err := e.BD.AppendNotes(id, note); err != nil {
		e.logf("warning: could not append notes to %s: %v", id, err)
	}
}

func stageOr(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// --- verdicts ---

// Verdict is a judging stage's answer.
type Verdict struct {
	// Pass is the verdict itself.
	Pass bool
	// Found reports that an explicit VERDICT: line was there to read. A stage
	// that returned no verdict is not a pass: silence has to fail closed, or a
	// reviewer that crashed reads as approval.
	Found bool
	// Body is what to hand the worker: the findings under a failed verdict, or
	// the whole message when there was no verdict line at all.
	Body string
}

// verdictPrefix is the line a judging stage's message must begin with. It is
// read literally, and the role prompt says so.
const verdictPrefix = "VERDICT:"

// ParseVerdict reads a judging stage's final message.
func ParseVerdict(text string) Verdict {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		rest, ok := cutPrefixFold(strings.TrimSpace(line), verdictPrefix)
		if !ok {
			continue
		}
		body := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		switch strings.ToLower(strings.TrimSpace(rest)) {
		case "pass":
			return Verdict{Pass: true, Found: true, Body: body}
		default:
			return Verdict{Found: true, Body: body}
		}
	}
	return Verdict{Body: strings.TrimSpace(text)}
}

// cutPrefixFold is strings.CutPrefix, case-insensitively.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}
