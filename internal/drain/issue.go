package drain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/gitguard"
	"bd-auto/internal/gitx"
	"bd-auto/internal/pipeline"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
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
	// Discoveries is where this worker writes what it found beside its issue.
	// It is an absolute path outside the worktree, so nothing the worker leaves
	// there can enter the snapshot that the orchestrator stages. See discover.go.
	Discoveries string
	Attempt     int
	Round       int
	// Carried is what this attempt is told about the attempt before it, already
	// rendered for a prompt. Empty on a first attempt — and empty on a retry
	// means the retry is starting blind.
	Carried string
	// Diff is the complete uncommitted snapshot supplied to a judging stage.
	Diff string
}

// Issue runs one issue to an integration-ready, parked, interrupted, or
// environment-stopped outcome, and then lets the repo read the result.
//
// The hooks are here rather than in the wave loop because this is the one place
// every caller passes through — the loop, and `bd-auto issue` on its own — and
// because it is the first moment no model is writing files for the issue. A
// successful issue remains in progress until integration gates and closes it.
//
// It returns an error only for a failure that is not about the work — an
// unreachable bd, a worktree that cannot be created, a runner that cannot be
// built. Everything the issue itself can fail at comes back in the Report.
func (e *Engine) Issue(ctx context.Context, id string) (Report, error) {
	rep, err := e.issue(ctx, id)
	// Not for an interrupt or an outage. Neither is a verdict, so there is
	// nothing to interpret and the run is on its way out; spawning a hook into
	// a cancelled context would only record it as interrupted too.
	//
	// Nor for an error. An error here is never about the work — an unreachable
	// bd, an unwritable run state — so the report beside it describes an issue
	// that has not finished being decided, and OutcomeFailed at the issue level
	// means nothing else.
	if err == nil && verdictOutcome(rep.Outcome) {
		rep.Hooks = e.issueHooks(ctx, rep)
		rep.Usage = rep.Usage.Add(hookUsage(rep.Hooks))
	}
	return rep, err
}

func (e *Engine) issue(ctx context.Context, id string) (Report, error) {
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
	if err := e.AuthorizeBilling(ctx); err != nil {
		return Report{}, err
	}
	if err := e.preflightIssue(ctx); err != nil {
		return Report{}, err
	}

	// Whatever ends this issue — done, parked, killed, interrupted — its models
	// are gone, so anything they left unanswered has to come off the queue with
	// them.
	defer e.cancelAsk(id)

	// The index, if this repo has asked for one. A drain builds it once for the
	// whole run; a single issue has to build its own, and without this
	// `bd-auto issue run` is the one entry point where graph.enabled is true and
	// no worker ever hears about it.
	e.buildIndex(ctx)

	iss, err := e.BD.Show(id)
	if err != nil {
		return Report{}, fmt.Errorf("drain: %s: %w", id, err)
	}
	branch := e.Cfg.Branch(id)
	rep := Report{Issue: id, Branch: branch, Worktree: worktree.Path(e.RepoRoot, id)}
	if st, loadErr := runstate.Load(e.RepoRoot); loadErr == nil && st.IsDone(id) && gitx.BranchExists(e.RepoRoot, branch) {
		rep.Outcome = OutcomeDone
		rep.Base, _ = git(e.RepoRoot, "merge-base", branch, e.baseRef())
		return rep, nil
	}
	if iss.Closed() {
		return rep, fmt.Errorf("drain: %s is already closed", id)
	}
	if iss.Status == "open" {
		if err := e.BD.Claim(id); err != nil {
			return Report{}, fmt.Errorf("drain: claim %s: %w", id, err)
		}
		iss.Status = "in_progress"
	}

	// The baseline is recorded before anything runs. Recording it afterwards
	// compares the damage against itself.
	baseline, err := gitguard.Record(e.RepoRoot, id, branch, e.baseRef())
	if err != nil {
		return rep, err
	}
	rep.Base = baseline.Base

	allowed := e.attempts()
	start := e.startAttempt(id, allowed)

	// What the next attempt is told about the last one. It is seeded from run
	// state rather than carried in memory alone, because a run killed between
	// two attempts starts the next process here with nothing in memory at all.
	carried := e.carryOver(id, start)

	for n := start; n <= allowed; n++ {
		t := task{Issue: iss, ID: id, Branch: branch, Base: baseline.Base, Attempt: n, Carried: carried}
		at, err := e.attempt(ctx, t, baseline)

		// Whatever the attempt came to. A worker that failed still did the
		// exploring, and one that stopped on the environment did nothing wrong
		// at all — so the findings are collected before the outcome is judged,
		// not only on the path where it succeeded. See discover.go.
		e.harvest(id)

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
		case OutcomeBlocked:
			// The worker reported that it is blocked. That is an answer, not a failure
			// to produce one, so the attempts it has left are not spent: a fresh
			// worker reading the same issue reaches the same conclusion and
			// charges full price for it. The issue is parked here with the
			// worker's own reason, and because run state now has it parked its
			// branch is skipped at the barrier rather than merged.
			rep.Outcome = OutcomeParked
			rep.Seconds = time.Since(started).Seconds()
			// The orchestrator writes the status and the human label.
			if err := e.BD.Park(id, selfParkNote(id, branch, n, allowed, rep.Reason)); err != nil {
				e.logf("warning: could not park %s: %v", id, err)
			}
			deps, err := e.recordParked(id, rep.Reason, stageOr(rep.Stage))
			rep.MissingDeps = deps
			return rep, err
		case OutcomeInterrupted, OutcomeInfra:
			// Neither is a verdict on the work, so the worktree, the branch and
			// the attempt counter are all left exactly as they are.
			rep.Outcome = at.Outcome
			rep.Seconds = time.Since(started).Seconds()
			return rep, nil
		}

		carried = carriedFailure(e.noteFailure(id, n, allowed, at))
		if n == allowed {
			break
		}

		// Only here, between attempts. Wiping the worktree between rounds is
		// what would make resuming a session pointless.
		e.logf("%s: attempt %d failed at %s; discarding the worktree and retrying fresh", id, n, at.Stage)
		if err := e.discardAttempt(id, branch); err != nil {
			e.logf("warning: could not discard %s attempt %d: %v", id, n, err)
		}
		// The orchestrator keeps ownership across fresh attempts. Returning the
		// child to open here would allow another drain to claim it while this run
		// is about to create its replacement worktree.
	}

	rep.Outcome = OutcomeParked
	rep.Seconds = time.Since(started).Seconds()
	reason := fmt.Sprintf("bd-auto parked %s after %d attempt(s). Last failure at stage %q: %s",
		id, allowed, stageOr(rep.Stage), rep.Reason)
	if err := e.BD.Park(id, reason); err != nil {
		e.logf("warning: could not park %s: %v", id, err)
	}
	deps, err := e.recordParked(id, rep.Reason, stageOr(rep.Stage))
	rep.MissingDeps = deps
	return rep, err
}

// attempt is one worktree, one session and up to max_rounds turns inside it.
//
// The check order below is the design: progress first and fatal, then the
// checks that stale state can satisfy.
func (e *Engine) attempt(ctx context.Context, t task, baseline gitguard.Baseline) (Attempt, error) {
	started := time.Now()
	out := Attempt{Attempt: t.Attempt, Blind: t.Attempt > 1 && t.Carried == ""}
	if out.Blind {
		e.recordBlindRetry(t.ID, t.Attempt)
	}

	// Whether the worktree was already there decides whether an interrupted
	// session can be resumed, so it has to be asked before Ensure creates one.
	survived := worktreeExists(e.RepoRoot, t.ID)

	wt, err := worktree.Ensure(e.RepoRoot, t.ID, t.Branch, t.Base)
	if err != nil {
		return out, err
	}
	t.Worktree = wt
	if err := gitguard.Setup(e.RepoRoot, wt, gitguard.Worker{Issue: t.ID, Attempt: t.Attempt}); err != nil {
		return out, err
	}

	// The directory, not the file: an empty discoveries file and a missing one
	// mean the same thing, and creating one would only invite a worker to treat
	// it as something it must fill in.
	t.Discoveries = DiscoveriesPath(e.RepoRoot, t.ID)
	if err := os.MkdirAll(filepath.Dir(t.Discoveries), 0o755); err != nil {
		return out, fmt.Errorf("drain: %s: discoveries directory: %w", t.ID, err)
	}

	// The role the implement stage runs under. Which role that is comes from
	// the config; that the stage named implement is the one holding the
	// worktree, the branch and the resumable session does not.
	role := e.implementRole()
	rn, err := e.runnerFor(role)
	if err != nil {
		return out, err
	}
	sess := e.adoptSession(t, survived)
	canResume := e.resumes(role, rn)

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

		snap := worktree.Snapshot(wt)
		t.Round = round
		// A worker's own turn count is the loop counter: it runs exactly once
		// per round, so its zero-based per-stage number and the loop's are the
		// same one. Set before the invoke, because the sink tags every event
		// the worker streams from here on with it.
		e.mark(t.Attempt, round)
		fb, st := feedback, stage
		c, err := e.invoke(ctx, invocation{
			Issue:     t.ID,
			Branch:    t.Branch,
			Attempt:   t.Attempt,
			Role:      role,
			Runner:    rn,
			Sess:      sess,
			CanResume: canResume,
			Implement: true,
			Build:     func(resume bool) runner.Request { return e.workerRequest(t, role, resume, st, fb) },
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

		// A backend-declared work failure may not have reached its final-message
		// contract. Preserve the concrete process failure on an empty tree instead
		// of spending rounds asking a failed process for a footer.
		if c.Result.Class == runner.ClassWorkFailed && !worktree.Changed(wt, snap) {
			return finish(OutcomeFailed, StageImplement, noProgressReason(t.Round, c.Result))
		}

		status := parseWorkerResult(c.Result.Text)
		if !status.Valid {
			feedback, stage = workerStatusFeedback(status.Error), StageImplement
			stageRounds[stage]++
			continue
		}
		if status.Status == "blocked" {
			return finish(OutcomeBlocked, StageImplement, status.Reason)
		}

		// First of the rest, and fatal. Every check below is satisfiable by the
		// previous round's state, so a round that changed nothing would pass
		// them all and then spend the rest of the budget proving it again.
		//
		// Unless the worker was refused the tools it needed: then it did not fail
		// the work, it was not allowed to do it, and no number of fresh attempts
		// under the same permission level will end differently. That is the
		// environment, so the run stops on it and costs the issue neither a round
		// nor an attempt.
		if !worktree.Changed(wt, snap) {
			if len(c.Result.Denials) > 0 {
				perms := e.Cfg.Runner(string(role)).Permissions
				return finish(OutcomeInfra, StageImplement, deniedReason(c.Result.Denials, perms))
			}
			return finish(OutcomeFailed, StageImplement, noProgressReason(t.Round, c.Result))
		}

		if v := e.verifyGuard(baseline); !v.OK {
			// A worker-created commit or moved ref cannot be repaired by the
			// worker under this contract. Fail the attempt so a fresh worktree can
			// be created without accepting any part of that history.
			return finish(OutcomeFailed, StageGuard, v.Reason())
		}

		sr, err := e.runStages(ctx, t, stageRounds)
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
		approved := worktree.Snapshot(t.Worktree)
		if sr.Approved != nil {
			approved = *sr.Approved
			if worktree.Snapshot(t.Worktree) != approved {
				feedback, stage = reviewerMutationFeedback(), sr.Stage
				stageRounds[stage]++
				continue
			}
		}
		if err := e.commitApproved(t, approved); err != nil {
			return out, err
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
	// Approved is the immutable worktree snapshot a fresh judging agent passed.
	Approved *worktree.Mark
	// InfraRetries is how many processes this stage burned on the environment.
	InfraRetries int
	// Control is non-empty when a model stage never reached a verdict at all:
	// the class that says why, which the caller routes rather than judges.
	Control runner.Class
}

// stagesResult is what the post-implement half of the pipeline produced.
type stagesResult struct {
	stageOutcome
	Stage    string
	Approved *worktree.Mark
}

// runStages runs every pipeline stage after implement, in order, stopping at
// the first one that fails.
//
// The gate and the review are not special cases here: they are what the default
// pipeline contains, and a repo that adds a stage gets it fed through the same
// feedback channel as the ones that shipped.
func (e *Engine) runStages(ctx context.Context, t task, rounds map[string]int) (stagesResult, error) {
	out := stagesResult{stageOutcome: stageOutcome{Passed: true}}
	for _, s := range e.Cfg.Pipeline {
		kind := s.Kind()
		if kind == "builtin-implement" {
			continue
		}
		if kind != "builtin-gate" && kind != "run" && kind != "agent" {
			// Validate rejects this at load time; reaching it means the config
			// changed underneath the run.
			return out, fmt.Errorf("drain: stage %q is neither a command nor a role", s.Stage)
		}

		// The boundary is announced rather than inferred, because only a model
		// stage says anything for itself: the gate and a run: stage execute
		// without a runner, and between the worker's last tool call and the
		// stage's verdict a watcher would otherwise be shown nothing changing
		// for the length of a `go test ./...`.
		role := e.stageRole(s)
		// Which turn of this stage this is, zero-based: rounds counts the times
		// it has already sent work back. It is per stage rather than per
		// attempt because that is the number a stage's own budget bounds, and
		// because a reviewer running for the first time in an attempt the gate
		// has already failed twice is on its own round 0, not round 2.
		round := rounds[s.Stage]
		e.mark(t.Attempt, round)
		e.Bus.Emit(Event{
			Kind: EventStageStart, Wave: e.waveNo, Issue: t.ID, Stage: s.Stage, Role: role,
			Attempt: t.Attempt, Round: round,
		})

		var (
			so  stageOutcome
			err error
		)
		switch kind {
		case "builtin-gate":
			so = e.gate(t)
		case "run":
			so = e.runCommandStage(t, s)
		case "agent":
			so, err = e.agentStage(ctx, t, s)
		}
		e.Bus.Emit(Event{
			Kind: EventStageEnd, Wave: e.waveNo, Issue: t.ID, Stage: s.Stage, Role: role,
			Passed: so.Passed, Text: so.Feedback, Usage: so.Usage,
			Attempt: t.Attempt, Round: round,
		})

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
			if so.Approved != nil {
				mark := *so.Approved
				out.Approved = &mark
				out.Stage = s.Stage
			}
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

// stageRole is the role a stage runs under: whatever its agent: names, which is
// the same question for the implement stage as for any other. It is empty for
// the stages that run under none — the gate and a run: command are this binary
// executing a command list, and there is no model to name.
func (e *Engine) stageRole(s config.Stage) runner.Role {
	if s.Run != "" {
		return ""
	}
	return runner.Role(s.Agent)
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
func (e *Engine) agentStage(ctx context.Context, t task, s config.Stage) (stageOutcome, error) {
	role := e.stageRole(s)
	rn, err := e.runnerFor(role)
	if err != nil {
		return stageOutcome{}, err
	}
	diff, err := pipeline.WorktreeDiff(t.Worktree, t.Base)
	if err != nil {
		return stageOutcome{}, fmt.Errorf("drain: build candidate snapshot for %s: %w", t.ID, err)
	}
	t.Diff = string(diff)
	sess := &session{}
	before := worktree.Snapshot(t.Worktree)

	c, err := e.invoke(ctx, invocation{
		Issue:     t.ID,
		Branch:    t.Branch,
		Attempt:   t.Attempt,
		Role:      role,
		Runner:    rn,
		Sess:      sess,
		CanResume: false,
		Ephemeral: true,
		Build:     func(resume bool) runner.Request { return e.reviewRequest(t, s, role, resume) },
	})
	out := stageOutcome{Usage: c.Usage, InfraRetries: absorbed(c)}
	if err != nil {
		return out, err
	}
	if after := worktree.Snapshot(t.Worktree); after != before {
		out.Feedback = reviewerMutationFeedback()
		return out, nil
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

	// A scoped stage is refused things by design, so this is reporting and not
	// a failure path: the verdict below stands either way.
	if len(c.Result.Denials) > 0 {
		e.logf("%s", deniedVerdictNote(t.ID, s.Stage, c.Result.Denials))
	}

	notes := ReviewNotesPath(e.RepoRoot, t.ID)
	if werr := writeFile(notes, reviewNotes(t, s, c.Result.Text, c.Result.Denials)); werr != nil {
		e.logf("warning: could not write review notes for %s: %v", t.ID, werr)
		notes = ""
	}

	if v := ParseVerdict(c.Result.Text); v.Pass {
		out.Passed = true
		mark := before
		out.Approved = &mark
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

var commitExclusions = []string{
	":(exclude).beads/auto/**",
	":(exclude).beads/issues.jsonl",
	":(exclude).beads/interactions.jsonl",
}

// commitApproved turns the reviewer-approved dirty tree into the branch's one
// issue commit. Git hooks are suppressed through gitx because this is an engine
// operation; the worker hook exists only to refuse this operation from agents.
func (e *Engine) commitApproved(t task, approved worktree.Mark) error {
	if now := worktree.Snapshot(t.Worktree); now != approved {
		return fmt.Errorf("drain: %s: approved snapshot changed before commit", t.ID)
	}
	if _, err := gitx.Run(t.Worktree, "reset", "--quiet", "HEAD", "--", "."); err != nil {
		return fmt.Errorf("drain: %s: reset the private worktree index: %w", t.ID, err)
	}
	args := []string{"add", "-A", "--", "."}
	args = append(args, commitExclusions...)
	if _, err := gitx.Run(t.Worktree, args...); err != nil {
		return fmt.Errorf("drain: %s: stage the approved snapshot: %w", t.ID, err)
	}
	staged, err := gitx.Run(t.Worktree, "diff", "--cached", "--name-only", "--")
	if err != nil {
		return fmt.Errorf("drain: %s: inspect the approved snapshot: %w", t.ID, err)
	}
	if strings.TrimSpace(staged) == "" {
		return fmt.Errorf("drain: %s: the approved snapshot contains no committable changes", t.ID)
	}
	title := "completed issue"
	if t.Issue != nil && strings.TrimSpace(t.Issue.Title) != "" {
		title, _, _ = strings.Cut(strings.TrimSpace(t.Issue.Title), "\n")
	}
	message := fmt.Sprintf("%s: %s\n\n%s", t.ID, title, (gitguard.Worker{Issue: t.ID, Attempt: t.Attempt}).Trailer())
	if _, err := gitx.Run(t.Worktree, "commit", "--quiet", "-m", message); err != nil {
		return fmt.Errorf("drain: %s: commit the approved snapshot: %w", t.ID, err)
	}
	return nil
}

// --- requests ---

func (e *Engine) workerRequest(t task, role runner.Role, resume bool, stage, feedback string) runner.Request {
	req := e.Cfg.Runner(string(role)).Request(role)
	req.Dir = t.Worktree
	req.SystemPrompt = e.implementPrompt(role)
	req.Prompt = workerPrompt(t, resume, stage, feedback)
	req.LogPath = LogPath(e.RepoRoot, t.ID, t.Attempt, t.Round, role)
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

// --- interrupt recovery ---

// startAttempt is the attempt number this run picks up on.
//
// A killed process leaves its attempt recorded as in flight, and starting again
// at one would hand an issue that has already burned its budget a fresh one. An
// interrupt is not a verdict, so it consumes no attempt — but it does not refund
// the attempts already spent either.
func (e *Engine) startAttempt(id string, allowed int) int {
	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		return 1
	}
	n := st.InFlight[id].Attempt
	switch {
	case n < 1:
		return 1
	case n > allowed:
		return allowed
	}
	return n
}

// adoptSession resumes the model session an interrupted attempt was in, when
// there is one to resume.
//
// Both conditions are load-bearing. The session id has to belong to this attempt
// — after discardAttempt the previous attempt's session describes a worktree
// that no longer exists — and the worktree has to have survived, because a
// backend resolves a resumable session against the project derived from its
// working directory, so a session whose worktree is gone cannot be continued.
//
// Where it does resume, the first turn carries the risk this whole path is built
// around: a process killed mid-turn can leave a transcript ending in a tool_use
// with no matching tool_result, and resuming that errors immediately. invoke
// reads that as infra-failed and falls back to a fresh dispatch, consuming
// neither a round nor an attempt, which is what makes the least-tested path in
// the system self-healing rather than a coin flip.
func (e *Engine) adoptSession(t task, survived bool) *session {
	if !survived {
		return &session{}
	}
	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		return &session{}
	}
	a, ok := st.InFlight[t.ID]
	if !ok || a.WorkerSession == "" || a.Attempt != t.Attempt {
		return &session{}
	}
	e.logf("%s: resuming the interrupted worker session in %s", t.ID, worktree.Path(e.RepoRoot, t.ID))
	return &session{ID: a.WorkerSession, Started: true}
}

// worktreeExists reports whether an issue's worktree is already on disk.
func worktreeExists(repoRoot, issue string) bool {
	fi, err := os.Stat(worktree.Path(repoRoot, issue))
	return err == nil && fi.IsDir()
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
	if !gitx.BranchExists(e.RepoRoot, branch) {
		return nil
	}
	_, err := git(e.RepoRoot, "branch", "-D", branch)
	return err
}

// noteFailure records a failed attempt in both places it belongs, and returns
// what it recorded so the caller can hand it to the next attempt.
//
// Run state first, and it is the copy that matters. The note on the issue is
// written for the humans and the tools that read bd, but it is not a channel
// bd-auto can rely on: beads' post-checkout hook imports .beads/issues.jsonl
// over its database, so creating the next attempt's worktree reverts this write
// before the worker that needs it ever runs. Run state is bd-auto's own file,
// gitignored, and nothing imports over it.
//
// Writing the note is still safe here: the attempt has finished, which keeps it
// inside the one-writer-per-issue rule bd's unlocked notes field imposes.
func (e *Engine) noteFailure(id string, n, allowed int, at Attempt) runstate.Failure {
	f := runstate.Failure{
		Attempt: n, Of: allowed, Stage: stageOr(at.Stage),
		Reason: at.Reason, At: time.Now().UTC(),
	}
	if _, err := runstate.Update(e.RepoRoot, true, func(s *runstate.State) error {
		s.RecordFailure(id, f)
		return nil
	}); err != nil {
		e.logf("warning: could not record %s attempt %d in run state: %v", id, n, err)
	}

	note := fmt.Sprintf("%s %d/%d failed at stage %q on %s:\n%s",
		wave.NoteMarker, n, allowed, f.Stage, f.At.Format(time.RFC3339), at.Reason)
	if err := e.BD.AppendNotes(id, note); err != nil {
		e.logf("warning: could not append notes to %s: %v", id, err)
	}
	return f
}

// carryOver reads back what this run recorded about the last failed attempt at
// an issue, rendered for the next attempt's prompt.
//
// Empty for a first attempt, and empty where nothing was recorded — a run whose
// state was cleared between attempts, or an attempt that failed before this
// version of bd-auto wrote anything down. Both cases are reported rather than
// papered over; see recordBlindRetry.
func (e *Engine) carryOver(id string, attempt int) string {
	if attempt <= 1 {
		return ""
	}
	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		return ""
	}
	f, ok := st.LastFailure(id)
	if !ok || f.Attempt >= attempt {
		return ""
	}
	return carriedFailure(f)
}

// recordBlindRetry records a retry that has nothing to carry.
//
// It goes to run state as well as the log because the log is discarded whenever
// Log is nil, which is what --quiet does — and a retry with no account of the
// attempt before it is the one thing a run must not be able to lose quietly. It
// will repeat that attempt's mistake, and it will charge for it. Report.Attempts
// carries the same fact out on stdout.
func (e *Engine) recordBlindRetry(id string, attempt int) {
	e.logf("warning: %s attempt %d starts with no account of attempt %d; "+
		"the retry cannot be told what already failed", id, attempt, attempt-1)
	if _, err := runstate.Update(e.RepoRoot, true, func(s *runstate.State) error {
		s.Note("%s attempt %d started blind: nothing recorded about attempt %d", id, attempt, attempt-1)
		return nil
	}); err != nil {
		e.logf("warning: could not record %s attempt %d as blind: %v", id, attempt, err)
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
