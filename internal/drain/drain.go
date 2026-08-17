// Package drain runs one beads issue end to end: implement it, prove it, judge
// it, and keep the worktree alive across the rounds in between.
//
// It is the half of bd-auto that used to be a live orchestrator context. Go owns
// the control flow here and spawns models as subprocesses, so nothing
// accumulates anywhere: the durable record of a run is run.json, the issue in
// bd, and the branch in git.
//
// # One loop, one feedback channel
//
// Every check that can fail feeds the same channel, so there is one place to
// reason about recovery rather than one per stage:
//
//	infra-failed  -> back off and re-run the same round; no round, no attempt
//	interrupted   -> return; the attempt counter is untouched
//	no progress   -> fail the attempt outright, NOT another round
//	not closed    -> feedback, next round
//	guard failed  -> feedback, next round
//	gate failed   -> feedback, next round
//	stage failed  -> feedback, next round
//
// The progress check comes first and is a hard failure, and the ordering is
// load-bearing. Every check below it is satisfiable by stale state: after round
// one the issue is already closed, so Terminal passes on round two even if round
// two did nothing, and gitguard.Verify passes on a branch that never moved. A
// no-op round would sail through to the reviewer, which re-reads an identical
// diff, fails identically, and spends every remaining round in an empty loop at
// full price. A round that changes nothing means resume is not working for this
// issue, and the answer to that is to stop resuming, not to resume again.
//
// # Escalation
//
// Cheapest first: feedback rounds on the same session (max_rounds), then a
// fresh attempt from a discarded worktree (retry), then park. discardAttempt
// fires only between attempts — wiping the worktree is what makes a resumed
// session pointless, so it must never happen between rounds.
//
// # Round budgets
//
// The run-level max_rounds bounds the loop. A stage's own max_rounds bounds how
// many of those rounds that one stage may consume, which is the only thing a
// per-stage budget can mean once every stage shares one loop.
package drain

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
	"bd-auto/prompts"
)

// Outcome is how an issue's run, or one attempt at it, ended.
type Outcome string

const (
	// OutcomeDone is a closed issue on a branch that passed every check.
	OutcomeDone Outcome = "done"
	// OutcomeFailed is an attempt that produced no acceptable result. At the
	// issue level it never appears: a failed final attempt is parked.
	OutcomeFailed Outcome = "failed"
	// OutcomeParked is an issue whose attempts are exhausted. It is set aside
	// for a human and the run carries on without it.
	OutcomeParked Outcome = "parked"
	// OutcomeInterrupted is a cancelled run. Nothing about it is a verdict, the
	// attempt counter is untouched, and the worktree is left where it is.
	OutcomeInterrupted Outcome = "interrupted"
	// OutcomeInfra is repeated infra failure: a usage limit, an outage, a
	// broken CLI. The engine stops rather than converting an outage into a pile
	// of parked issues.
	OutcomeInfra Outcome = "infra-failed"
)

// Stage names the engine reports for failures that are not one of the
// configured pipeline stages.
const (
	StageImplement = config.StageImplement
	StageGuard     = "guard"
)

// Defaults for the knobs that have no home in .beads-auto.yaml.
const (
	// DefaultInfraRetries is how many consecutive infra failures one
	// invocation absorbs before the engine gives up on the environment.
	DefaultInfraRetries = 5
	// DefaultBackoffBase is the first wait after an infra failure; it doubles
	// from there.
	DefaultBackoffBase = 5 * time.Second
	// DefaultBackoffMax caps that doubling.
	DefaultBackoffMax = 2 * time.Minute
)

// Issues is the slice of bd the engine needs. *bd.Client satisfies it.
//
// It is a superset of wave.Source and scope.Source on purpose: the wave loop
// hands this same value to both, and a method set that already covers them
// makes that a plain assignment rather than a type assertion that can fail at
// the worst possible moment.
type Issues interface {
	Show(id string) (*bd.Issue, error)
	// Ready returns bd's blocker-aware ready front under a parent. The wave
	// loop plans from it; readiness is never recomputed here.
	Ready(parent string, limit int) ([]bd.Issue, error)
	AppendNotes(id, note string) error
	Park(id, reason string) error
	Reset(id string) error
	// Children returns every issue under a parent, closed ones included. The
	// integrator needs it to decide whether the epic is finished.
	Children(parent string) ([]bd.Issue, error)
	// Close closes an issue. Only ever called for the epic: a child issue
	// belongs to its worker, and two writers on one issue is how beads loses an
	// update.
	Close(id, reason string) error
}

// Engine runs issues. Every field but the first four has a working default, so
// the zero-ish value is usable.
type Engine struct {
	// RepoRoot is the MAIN checkout. Worktrees, run state and the guard
	// baselines all live relative to it.
	RepoRoot string
	// Cfg is the resolved run configuration.
	Cfg *config.Config
	// BD is the issue tracker.
	BD Issues
	// Sink receives live events from every model this engine spawns. Nil is a
	// valid sink that drops everything.
	Sink runner.EventSink
	// Bus receives run-level events: waves opening, issues finishing, barriers,
	// pauses. It is what the plain, JSON and TUI renderers all attach to. Nil is
	// a valid bus that drops everything.
	//
	// A drain sets each worker's Sink from it, which is the only place a model
	// event can be tagged with the issue that produced it.
	Bus *Bus

	// BaseRef is the ref every attempt branches from, and the ref the guard
	// checks did not move. Empty means HEAD.
	BaseRef string
	// MaxRounds overrides the config's run-level round budget, and every
	// per-stage budget with it. Zero uses the config.
	MaxRounds int
	// Retry overrides the config's extra-attempt budget. Nil uses the config;
	// a pointer to 0 means "one attempt, then park", which is a setting rather
	// than an absence and is why this is a pointer.
	Retry *int
	// InfraRetries overrides DefaultInfraRetries.
	InfraRetries int

	// NewRunner builds the runner for a role. It takes the role as well as the
	// spec because a Spec deliberately does not carry one, and a caller
	// substituting a backend usually wants to substitute it per role.
	// Nil means runner.New.
	NewRunner func(role runner.Role, spec runner.Spec) (runner.Runner, error)
	// Prompt resolves a role's system prompt. Nil means prompts.For.
	Prompt func(role runner.Role) (string, error)
	// Sleep waits interruptibly. Nil means a context-aware timer; a test sets
	// it to something that returns immediately.
	Sleep func(ctx context.Context, d time.Duration) error
	// Backoff is how long to wait after the nth consecutive infra failure, n
	// counting from 1. Nil means exponential from DefaultBackoffBase.
	Backoff func(n int) time.Duration
	// Log receives human-readable progress. Nil discards it.
	Log func(format string, args ...any)

	runners map[runner.Role]runner.Runner
}

// Report is what one issue's run produced.
type Report struct {
	Issue    string    `json:"issue"`
	Branch   string    `json:"branch"`
	Worktree string    `json:"worktree"`
	Base     string    `json:"base"`
	Outcome  Outcome   `json:"outcome"`
	Stage    string    `json:"stage,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Attempts []Attempt `json:"attempts"`
	// Usage is the whole issue's cost, every attempt and every round.
	Usage   runner.Usage `json:"usage"`
	Seconds float64      `json:"seconds"`
}

// Attempt is one pass at an issue: a worktree, a session, and up to max_rounds
// turns inside it.
type Attempt struct {
	Attempt int     `json:"attempt"`
	Rounds  int     `json:"rounds"`
	Outcome Outcome `json:"outcome"`
	Stage   string  `json:"stage,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	// Session is the worker session this attempt ended on. It changes when a
	// resumed turn had to fall back to a fresh dispatch.
	Session string `json:"session,omitempty"`
	// InfraRetries is how many processes this attempt burned on the environment
	// rather than on the work. It is reported separately from Rounds because it
	// costs money without buying anything, and telling the two apart is what
	// makes a resume-versus-fresh cost comparison mean something.
	InfraRetries int          `json:"infra_retries,omitempty"`
	Usage        runner.Usage `json:"usage"`
	Seconds      float64      `json:"seconds"`
}

// Done reports whether the issue finished.
func (r Report) Done() bool { return r.Outcome == OutcomeDone }

// session is a model conversation the engine can continue.
type session struct {
	// ID is the caller-generated session identifier, so a later round resumes
	// without parsing any output to discover what the session was called.
	ID string
	// Started reports that a turn actually ran on this ID. Only then is there
	// anything to resume; an infra failure before the first turn leaves the
	// session unborn.
	Started bool
}

// --- defaults ---

func (e *Engine) baseRef() string {
	if e.BaseRef != "" {
		return e.BaseRef
	}
	return "HEAD"
}

func (e *Engine) rounds() int {
	if e.MaxRounds > 0 {
		return e.MaxRounds
	}
	if e.Cfg != nil && e.Cfg.MaxRounds > 0 {
		return e.Cfg.MaxRounds
	}
	return config.DefaultMaxRounds
}

func (e *Engine) attempts() int {
	retry := config.DefaultRetry
	if e.Cfg != nil && e.Cfg.Retry >= 0 {
		retry = e.Cfg.Retry
	}
	if e.Retry != nil && *e.Retry >= 0 {
		retry = *e.Retry
	}
	return retry + 1 // the first attempt plus its retries
}

// stageBudget is how many of the loop's rounds one stage may consume. An
// explicit MaxRounds override wins over every configured budget; otherwise a
// stage's own max_rounds narrows the run-level one, which is the only thing a
// per-stage budget can mean once every stage shares a single loop.
func (e *Engine) stageBudget(name string) int {
	if e.MaxRounds > 0 {
		return e.MaxRounds
	}
	if e.Cfg != nil {
		for _, s := range e.Cfg.Pipeline {
			if s.Stage == name {
				return e.Cfg.MaxRoundsFor(s)
			}
		}
	}
	return e.rounds()
}

func (e *Engine) infraRetries() int {
	if e.InfraRetries > 0 {
		return e.InfraRetries
	}
	return DefaultInfraRetries
}

func (e *Engine) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log(format, args...)
	}
}

// runnerFor resolves and caches the runner for a role. One runner per role per
// engine: adapters are stateless, and rebuilding one per round would throw away
// whatever a future adapter caches.
func (e *Engine) runnerFor(role runner.Role) (runner.Runner, error) {
	if r, ok := e.runners[role]; ok {
		return r, nil
	}
	spec := e.Cfg.Runner(string(role))
	build := e.NewRunner
	if build == nil {
		build = func(_ runner.Role, s runner.Spec) (runner.Runner, error) { return runner.New(s) }
	}
	r, err := build(role, spec)
	if err != nil {
		return nil, fmt.Errorf("drain: runner for role %s: %w", role, err)
	}
	if e.runners == nil {
		e.runners = map[runner.Role]runner.Runner{}
	}
	e.runners[role] = r
	return r, nil
}

// promptFor resolves a role's system prompt. An agent stage naming a role with
// no prompt of its own gets the reviewer's, because a custom stage is a judging
// stage: it reads a diff and returns a verdict.
func (e *Engine) promptFor(role runner.Role) string {
	lookup := e.Prompt
	if lookup == nil {
		lookup = func(r runner.Role) (string, error) { return prompts.For(string(r)) }
	}
	if p, err := lookup(role); err == nil {
		return p
	}
	p, _ := lookup(runner.RoleReviewer)
	return p
}

// resumes reports whether a role's feedback rounds continue the same session.
// It is a preference and a capability: config asks, the backend decides.
func (e *Engine) resumes(role runner.Role, rn runner.Runner) bool {
	return e.Cfg.Runner(string(role)).Resume && rn.Caps().Resume
}

// --- invocation ---

// invocation is one logical model call: possibly several processes, because an
// infra failure is retried in place.
type invocation struct {
	Issue  string
	Branch string
	// Attempt is the issue attempt this call belongs to, for run state.
	Attempt int
	Role    runner.Role
	Runner  runner.Runner
	// Sess is mutated: invoke mints a session id for a fresh dispatch and
	// records that a turn has run on it.
	Sess *session
	// CanResume is the resolved preference-and-capability for this role.
	CanResume bool
	// Ephemeral suppresses the run-state write. It is set for a call that is not
	// an attempt at an issue — the integrator resolving one merge conflict —
	// where recording an in-flight entry would put a finished issue back in
	// flight and hold its epic open forever.
	Ephemeral bool
	// Build returns the request for one process. It is called once per process,
	// with whether this one resumes, because a resumed turn's prompt is the
	// feedback alone and a fresh one's is the whole task.
	Build func(resume bool) runner.Request
}

// call is one logical model call: its final result, plus what every process it
// took cost. The two differ because an infra failure is absorbed here, and a
// backend that charged for the attempt still charged for it.
type call struct {
	Result runner.Result
	Usage  runner.Usage
	// Procs is how many processes this call took. One is the normal case;
	// more means infra failures were absorbed.
	Procs int
}

// invoke runs one logical call to completion.
//
// An infra failure is the whole reason this is not a single Run: it consumes
// neither a round nor an attempt, so it is absorbed here rather than routed by
// the loop. If they keep coming the last result is handed back with its class
// intact and the caller stops the run.
//
// A resumed turn that comes back infra-failed is retried fresh. A process killed
// mid-turn can leave a transcript ending in a tool_use with no matching
// tool_result, and resuming that errors immediately and identically every time —
// which is indistinguishable from a rate limit until you stop resuming. Falling
// back is what makes the least-tested path in the system self-healing.
func (e *Engine) invoke(ctx context.Context, in invocation) (call, error) {
	var out call
	for streak := 0; ; {
		if err := ctx.Err(); err != nil {
			out.Result = runner.Result{Class: runner.ClassInterrupted, Err: err}
			return out, nil
		}

		resume := in.CanResume && in.Sess.Started
		if !resume {
			in.Sess.ID = newSessionID()
			in.Sess.Started = false
		}

		req := in.Build(resume)
		req.Role = in.Role
		req.SessionID = in.Sess.ID
		req.Resume = resume

		// Written before the process starts, not after it returns: a session
		// recorded afterwards is lost by exactly the interrupt that needs it.
		if !in.Ephemeral {
			if err := e.recordSession(in, req.SessionID); err != nil {
				return out, err
			}
		}

		res, err := in.Runner.Run(ctx, req, e.Sink)
		out.Procs++
		if err != nil {
			return out, fmt.Errorf("drain: %s: %w", in.Role, err)
		}
		out.Result = res
		out.Usage = out.Usage.Add(res.Usage)

		if res.Class == runner.ClassInfraFailed {
			streak++
			if streak >= e.infraRetries() {
				return out, nil
			}
			if resume {
				in.Sess.Started = false // next process starts a fresh session
			}
			wait := e.backoff(streak)
			e.logf("%s: %s failed on the environment (%v); retrying the same round in %s",
				in.Issue, in.Role, res.Err, wait)
			if err := e.sleep(ctx, wait); err != nil {
				out.Result = runner.Result{Class: runner.ClassInterrupted, Err: err}
				return out, nil
			}
			continue
		}

		if res.Class.Counts() {
			in.Sess.Started = true
		}
		return out, nil
	}
}

func (e *Engine) backoff(n int) time.Duration {
	if e.Backoff != nil {
		return e.Backoff(n)
	}
	d := DefaultBackoffBase
	for i := 1; i < n && d < DefaultBackoffMax; i++ {
		d *= 2
	}
	if d > DefaultBackoffMax {
		d = DefaultBackoffMax
	}
	return d
}

func (e *Engine) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- run state ---

// recordSession writes the session a process is about to run under.
//
// create is true because `bd-auto issue run` is meant to work standalone, with
// no drain around it. The state it creates has no status, so it arms nothing.
func (e *Engine) recordSession(in invocation, sid string) error {
	_, err := runstate.Update(e.RepoRoot, true, func(s *runstate.State) error {
		a := s.InFlight[in.Issue]
		a.Branch = in.Branch
		a.Attempt = in.Attempt
		if a.StartedAt.IsZero() {
			a.StartedAt = time.Now().UTC()
		}
		switch in.Role {
		case runner.RoleWorker:
			a.WorkerSession = sid
			a.Stage = StageImplement
		default:
			a.ReviewSession = sid
			a.Stage = string(in.Role)
		}
		s.InFlight[in.Issue] = a
		return nil
	})
	if err != nil {
		return fmt.Errorf("drain: record session for %s: %w", in.Issue, err)
	}
	return nil
}

// recordRound keeps run state honest about how far an attempt got, so a run
// interrupted mid-attempt can be reported on without re-deriving anything.
func (e *Engine) recordRound(issue string, attempt, round int, stage string) error {
	_, err := runstate.Update(e.RepoRoot, true, func(s *runstate.State) error {
		a := s.InFlight[issue]
		a.Attempt = attempt
		a.Rounds = round
		if stage != "" {
			a.Stage = stage
		}
		s.InFlight[issue] = a
		s.Attempts[issue] = attempt
		return nil
	})
	if err != nil {
		return fmt.Errorf("drain: record round for %s: %w", issue, err)
	}
	return nil
}

func (e *Engine) recordDone(issue string) error {
	_, err := runstate.Update(e.RepoRoot, true, func(s *runstate.State) error {
		s.MarkDone(issue)
		s.Note("%s done", issue)
		return nil
	})
	return err
}

func (e *Engine) recordParked(issue, reason, stage string) error {
	_, err := runstate.Update(e.RepoRoot, true, func(s *runstate.State) error {
		s.Park(issue, reason, stage)
		s.Note("%s parked at %s", issue, stage)
		return nil
	})
	return err
}

// --- paths ---

// LogPath is where one process's full transcript lands. The engine keeps only
// the final message in memory; this is the rest of it, and it is better
// observability than the plugin ever had, where worker transcripts were
// invisible.
func LogPath(repoRoot, issue string, attempt, round int, role runner.Role) string {
	name := fmt.Sprintf("%s-a%d-r%d-%s.jsonl", safeName(issue), attempt, round, role)
	return filepath.Join(runstate.Dir(repoRoot), "logs", name)
}

// ReviewNotesPath is where a stage's verdict is kept. It outlives the round, so
// a failed review can be read after the fact rather than only in the feedback
// that was handed to the worker.
func ReviewNotesPath(repoRoot, issue string) string {
	return filepath.Join(runstate.Dir(repoRoot), "review", safeName(issue)+".md")
}

// safeName keeps an issue ID from escaping the directory it names a file in. bd
// IDs are already path-safe, so this is a guard rather than a transformation.
func safeName(issue string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, issue)
	s = strings.TrimLeft(s, ".")
	if s == "" {
		return "issue"
	}
	return s
}

// newSessionID returns a random UUIDv4. Generating it here rather than reading
// it back out of the backend is what lets a later round resume with no output
// parsing at all.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it ever does,
		// a session id that collides is a far smaller problem than a panic
		// halfway through a run.
		return fmt.Sprintf("bd-auto-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// --- git ---

// git runs a git command in dir and returns trimmed stdout, carrying stderr on
// the error so a failure says what git actually complained about.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = out
		}
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}

func branchExists(repoRoot, branch string) bool {
	_, err := git(repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// writeFile writes a file, creating its directory.
func writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
