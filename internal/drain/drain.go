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
// A fresh attempt keeps one thing from the one before it: why it failed, in its
// prompt. That account is kept in run state, not on the issue. bd-auto does
// write it to the issue's notes too, but beads' post-checkout hook imports
// .beads/issues.jsonl over its database, so creating the next attempt's
// worktree reverts every bd write since the worker's last commit — the note
// among them. A retry that read its own history back off bd would find nothing
// and repeat the previous attempt exactly, at full price, which is what it did
// before beads-auto-imp-so5. Where there is nothing to carry, the attempt is
// reported blind rather than started quietly.
//
// # Round budgets
//
// The run-level max_rounds bounds the loop. A stage's own max_rounds bounds how
// many of those rounds that one stage may consume, which is the only thing a
// per-stage budget can mean once every stage shares one loop.
package drain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"bd-auto/internal/ask"
	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/gitx"
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
	// OutcomeBlocked is an attempt whose worker set its own issue to blocked
	// rather than closing it, which is what prompts/worker.md tells a worker
	// with nowhere to go to do. It is a verdict on the work — the worker's own
	// — so the issue stops there and is parked with what the worker said. It
	// never appears at the issue level: a self-park is a park.
	OutcomeBlocked Outcome = "blocked"
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
	// All returns every issue in the repo. The barrier needs it to tell a new
	// discovery from one bd already has.
	All() ([]bd.Issue, error)
	// Close closes an issue. Normally only the epic: a child issue belongs to
	// its worker, and two writers on one issue is how beads loses an update.
	// The exception is the barrier's reconcile pass, which re-closes an issue
	// this run already finished and something else reverted — there the worker
	// is long gone and there is no second writer to lose to.
	Close(id, reason string) error
	// Create files a new issue and returns its ID. Only ever called at the
	// barrier, for work a worker discovered beside the issue it was given.
	Create(n bd.NewIssue) (string, error)
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
	// Control is the run's stop switch: k kills one worker, q stops the run.
	// Nil is a valid control nobody can press, which is what a headless run
	// gets.
	Control *Control
	// Ask is the channel a model uses to put a question to the human, offered
	// to every role the config allows it for. Nil offers nothing, which is what
	// every run did before the tool existed and what a backend that cannot
	// carry tools gets anyway.
	Ask *ask.Server

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
	// Forge is where a finished run is handed over: the push and the pull
	// request. Nil means GH, the gh CLI.
	Forge Forge
	// Sleep waits interruptibly. Nil means a context-aware timer; a test sets
	// it to something that returns immediately.
	Sleep func(ctx context.Context, d time.Duration) error
	// Backoff is how long to wait after the nth consecutive infra failure, n
	// counting from 1. Nil means exponential from DefaultBackoffBase.
	Backoff func(n int) time.Duration
	// Log receives human-readable progress. Nil discards it.
	Log func(format string, args ...any)

	// waveNo is the wave this clone is working in, tagged onto the events the
	// engine raises for itself. Zero outside a wave, which is what one issue
	// run on its own gets. See forIssue.
	waveNo int

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
	// MissingDeps is set when this issue parked naming an issue that was
	// running beside it. See MissingDep.
	MissingDeps []MissingDep `json:"missing_deps,omitempty"`
	// Usage is the whole issue's cost, every attempt and every round.
	Usage   runner.Usage `json:"usage"`
	Seconds float64      `json:"seconds"`
}

// MissingDep is a park whose reason named another issue running in the same
// wave.
//
// By construction that issue cannot be a blocker. A wave is bd's own ready
// front intersected with the run's scope (see wave.Plan), and bd's ready front
// is blocker-aware, so no member of a wave holds a blocks edge over another
// member. Either the worker was wrong about needing it, or the edge is real and
// nobody ever wrote it down — and bd-auto cannot tell which from a sentence.
//
// So it reports it with the command a human would run and stops there. Adding
// the edge automatically would let one model's prose rewrite the dependency
// graph, which is worse than a missing edge: a wrong edge is believed by every
// later run.
type MissingDep struct {
	// Issue is the issue that parked, Sibling the wave member its reason named.
	Issue   string `json:"issue"`
	Sibling string `json:"sibling"`
	// Command is the bd invocation that would record the edge, if it is real.
	Command string `json:"command"`
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
	InfraRetries int `json:"infra_retries,omitempty"`
	// Blind reports a retry that started with no account of the attempt before
	// it, and is therefore free to repeat its mistake at full price.
	//
	// It is a field on the report rather than only a log line because the log
	// is discarded whenever Log is nil, which is exactly what --quiet does. The
	// final report goes to stdout either way, so this is the one channel a
	// silent run cannot swallow.
	Blind   bool         `json:"blind,omitempty"`
	Usage   runner.Usage `json:"usage"`
	Seconds float64      `json:"seconds"`
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
		e.attachAsk(&req, in)

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

// recordParked writes a park into run state, and is the single funnel every
// park goes through — the attempt loop, the barrier and a killed worker alike.
//
// That is why the sibling check lives here rather than beside any one of them:
// a park reason that names an issue running in the same wave is the same fault
// wherever it was written, and the wave's membership is in the state this
// function already holds open. The hits go into the run's notes as well as back
// to the caller, because the caller's report is in memory and the notes are
// what a human reads off a run that has already ended. Nothing is added to the
// graph. See MissingDep.
func (e *Engine) recordParked(issue, reason, stage string) ([]MissingDep, error) {
	var found []MissingDep
	_, err := runstate.Update(e.RepoRoot, true, func(s *runstate.State) error {
		s.Park(issue, reason, stage)
		s.Note("%s parked at %s", issue, stage)
		found = missingDeps(issue, reason, s.WaveIssues)
		for _, d := range found {
			s.Note("%s", missingDepNote(d))
		}
		return nil
	})
	return found, err
}

// --- paths ---

// LogPath is where one process's full transcript lands. The engine keeps only
// the final message in memory; this is the rest of it, and it is better
// observability than the plugin ever had, where worker transcripts were
// invisible.
//
// It is the name asked for rather than a promise. Issue, attempt, round and
// role stop identifying a process uniquely as soon as something resets the
// attempt counter, so an adapter that finds the name taken writes beside it and
// reports back where it actually wrote. See claude.transcript.
func LogPath(repoRoot, issue string, attempt, round int, role runner.Role) string {
	name := fmt.Sprintf("%s-a%d-r%d-%s.jsonl", safeName(issue), attempt, round, role)
	return filepath.Join(runstate.Dir(repoRoot), "logs", name)
}

// LogFile is one process's transcript on disk: where it is, and what LogPath
// encoded into its name.
type LogFile struct {
	Path string
	// Attempt and Round are the ones the name carries, and Role is the process
	// that ran: worker, integrator, or the stage that spawned a model.
	Attempt int
	Round   int
	Role    runner.Role
	// Dup is the disambiguator an adapter added because the name LogPath asked
	// for was already taken — 0 for the process that claimed the name, 2 for
	// the next. It is not noise: `run unpark` resets the attempt counter on
	// purpose, so a retried worker asks for the same name its own corpse is
	// written to, and both transcripts are worth reading.
	Dup     int
	ModTime time.Time
	Size    int64
}

// LogFiles lists every transcript one issue has, oldest process first.
//
// It is a listing rather than a computation over LogPath because LogPath
// returns the name asked for and not always the name that was taken. What is on
// disk is the only complete account of which processes an issue has had — every
// round, every attempt, every stage — and the only one that survives a run that
// has already finished, which is the whole reason anything reads these files.
//
// The order is attempt, then round, then when the file was last written: an
// issue's processes are sequential, so the file that stopped growing first is
// the process that ran first.
func LogFiles(repoRoot, issue string) []LogFile {
	stem := safeName(issue)
	matches, err := filepath.Glob(filepath.Join(runstate.Dir(repoRoot), "logs", stem+"-a*.jsonl"))
	if err != nil {
		return nil
	}
	out := make([]LogFile, 0, len(matches))
	for _, path := range matches {
		f, ok := parseLogName(filepath.Base(path), stem)
		if !ok {
			continue
		}
		f.Path = path
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		f.ModTime, f.Size = fi.ModTime(), fi.Size()
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Attempt != b.Attempt:
			return a.Attempt < b.Attempt
		case a.Round != b.Round:
			return a.Round < b.Round
		case !a.ModTime.Equal(b.ModTime):
			return a.ModTime.Before(b.ModTime)
		}
		return a.Path < b.Path
	})
	return out
}

// parseLogName reads back what LogPath wrote: <stem>-a<attempt>-r<round>-<role>
// with an optional -<n> the adapter added to avoid overwriting.
//
// A role that itself ends in -<digits> would be misread as a duplicate. Roles
// are configuration keys and none of the shipped ones look like that, and the
// cost of being wrong is a header line that says "(#2)" — so this is left
// simple rather than made ambiguous in the other direction.
func parseLogName(base, stem string) (LogFile, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSuffix(base, ".jsonl"), stem+"-")
	if !ok {
		return LogFile{}, false
	}
	attempt, rest, ok := cutNumber(rest, "a")
	if !ok {
		return LogFile{}, false
	}
	round, role, ok := cutNumber(rest, "r")
	if !ok || role == "" {
		return LogFile{}, false
	}
	f := LogFile{Attempt: attempt, Round: round}
	if cut := strings.LastIndex(role, "-"); cut > 0 {
		if n, err := strconv.Atoi(role[cut+1:]); err == nil && n > 1 {
			f.Dup, role = n, role[:cut]
		}
	}
	f.Role = runner.Role(role)
	return f, true
}

// cutNumber takes a <prefix><digits>- field off the front of s.
func cutNumber(s, prefix string) (int, string, bool) {
	field, rest, ok := strings.Cut(s, "-")
	if !ok {
		return 0, "", false
	}
	digits, ok := strings.CutPrefix(field, prefix)
	if !ok {
		return 0, "", false
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, "", false
	}
	return n, rest, true
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
//
// It fires no hooks. The barrier merges a branch per issue and switches the
// checkout onto the staging branch, and beads' post-merge and post-checkout
// hooks import .beads/issues.jsonl over its database — so integrating a wave
// used to revert the closes the wave had just earned. See internal/gitx.
func git(dir string, args ...string) (string, error) {
	return gitx.Run(dir, args...)
}

// writeFile writes a file, creating its directory.
func writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
