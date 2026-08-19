// Package config loads .beads-auto.yaml, the per-repo configuration for a
// bd-auto run: what the gate is, what stages each issue passes through, and how
// aggressively to run.
//
// Every field has a default, so a repo with no config file at all still works.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"bd-auto/internal/runner"
)

// FileName is the config file looked for at the repo root.
const FileName = ".beads-auto.yaml"

// Autonomy controls where a run pauses for the human.
//
// There used to be a third mode, issue, that paused after every issue. It is
// gone: a run's scope is now a set of issues a human selected before anything
// was spawned, so stopping to ask about each one asks a question that has
// already been answered. wave survives because a barrier is where a large scope
// is worth looking at — it is the point where branches have merged and the gate
// has run on the result.
type Autonomy string

const (
	// AutonomyAuto drains the scope without stopping.
	AutonomyAuto Autonomy = "auto"
	// AutonomyWave pauses at each wave barrier and waits for
	// `bd-auto run resume`.
	AutonomyWave Autonomy = "wave"
)

// Autonomies lists the modes, in order of how much they let a run do.
func Autonomies() []Autonomy { return []Autonomy{AutonomyAuto, AutonomyWave} }

// Valid reports whether a is a recognised autonomy mode.
func (a Autonomy) Valid() bool {
	switch a {
	case AutonomyAuto, AutonomyWave:
		return true
	}
	return false
}

// Command is one gate command: a shell command that must exit 0.
type Command struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	// Timeout in seconds. Zero means DefaultCommandTimeout.
	Timeout int `yaml:"timeout"`
}

// Stage is one step in the per-issue pipeline.
//
// One rule covers every entry: the stage name says which step this is, agent:
// says who runs it, run: says it is a shell command instead. Neither field
// decides whether a stage is built in — the name does that, and only two names
// are: implement and gate.
//
// So implement takes an agent: like any other step, and the role it names is
// the role that does the work. gate is the one step that takes none, because it
// is a list of commands rather than a judgement, and Validate says so rather
// than ignoring one written there.
type Stage struct {
	// Stage is the stage name. "implement" and "gate" are built in.
	Stage string `yaml:"stage"`
	// Agent names a runner role: a key under runners:, or one of the built-in
	// roles. The engine spawns a model for it with that role's resolved
	// configuration.
	//
	// This field kept its name across the move to a headless engine but not
	// its meaning — it used to name a Claude Code subagent to dispatch — so
	// Validate rejects a name that is not a defined role rather than letting
	// an old config fail at the moment it would have spawned something.
	Agent string `yaml:"agent"`
	// Run is a shell command executed by this binary.
	Run string `yaml:"run"`
	// MaxRounds caps how many feedback rounds this stage may send back to the
	// same worker before the attempt is considered failed. Unset means the
	// run-level MaxRounds; see Config.MaxRoundsFor.
	MaxRounds int `yaml:"max_rounds"`
	// Timeout in seconds for Run stages. Zero means DefaultCommandTimeout.
	Timeout int `yaml:"timeout"`
	// Optional marks a stage whose failure is reported but does not fail the
	// issue.
	Optional bool `yaml:"optional"`
}

// Built-in stage names.
//
// These two are reserved: a stage called implement is the one that creates the
// worktree, the branch and the session every later stage runs against, and a
// stage called gate is the gate commands. Everything else in a pipeline is
// whatever its agent: or run: makes it.
//
// The worktree belongs to the stage named implement, not to whichever stage
// happens to be first. Those coincide — applyDefaults puts implement at the
// head of every pipeline and Validate refuses it anywhere else — but they are
// not the same invariant, and this is the one drain/issue.go is written
// against: making the role configurable was never meant to let a review stage
// inherit the lifecycle by being moved up a line.
const (
	StageImplement = "implement"
	StageGate      = "gate"
)

// Handoff decides where a run's merges land and how the finished result reaches
// a human.
//
// Both switches are on by default, and they are two switches rather than one on
// purpose. They answer different questions: branch is "may bd-auto write to the
// branch you work on", pr is "may bd-auto publish". A repo with no remote, or
// one whose review happens somewhere other than a forge, wants the first
// without the second — so turning the pull request off still produces the epic
// branch and leaves it in place. The reverse is not a setting: a pull request
// needs a head branch that is not the base, so pr without branch is a
// contradiction Validate refuses rather than silently ignores.
type Handoff struct {
	// Branch stages the run on a temporary epic branch instead of merging into
	// the branch the run started on. Unset means true.
	Branch *bool `yaml:"branch"`
	// PR opens a pull request from the epic branch once every issue merged
	// clean and the gate passed on the merged result. Unset means true.
	PR *bool `yaml:"pr"`
	// Remote is the git remote the epic branch is pushed to.
	Remote string `yaml:"remote"`
	// Prefix is prepended to the epic's ID to form the epic branch. Must end
	// with /.
	Prefix string `yaml:"prefix"`
}

// Ask configures the ask_user tool: whether a run offers it at all, how long a
// question waits for a human, and which roles may raise one.
//
// The roles list is the interesting one. A worker meets genuine ambiguity and
// an integrator meets it in the sharpest form there is — two branches that
// disagree — so both ask by default. A reviewer does not: it is read-only, it
// is judging somebody else's work against stated criteria, and a reviewer that
// can put a question to the author of the work it is judging is no longer an
// independent check. Where a repo wants one anyway, adding it here is enough.
type Ask struct {
	// Enabled offers the tool. Unset means true.
	Enabled *bool `yaml:"enabled"`
	// Timeout is how long a question waits for a human, in seconds. Unset means
	// DefaultAskTimeout. 0 means never give up, which is a thing a repo can
	// choose and the default deliberately does not.
	Timeout *int `yaml:"timeout"`
	// Hold is how long one tool call blocks before handing the model a ticket
	// to poll with, in seconds. Unset means DefaultAskHold.
	//
	// It exists in the config because it is the one number that has to fit
	// inside a backend's own limit on a single tool call, and that limit is not
	// bd-auto's to know. Lower it for a backend stricter than the shipped one.
	Hold *int `yaml:"hold"`
	// Roles may raise a question. Unset means DefaultAskRoles.
	Roles []string `yaml:"roles"`
}

// Yes and No are the two values an explicit tri-state yaml bool can hold.
//
// They exist because a field whose default is true cannot be a plain bool: an
// absent key and an explicit `false` both unmarshal to the same zero value, so
// the loader could not tell "I did not say" from "no".
func Yes() *bool { v := true; return &v }

// No returns an explicit false. See Yes.
func No() *bool { v := false; return &v }

func enabled(v *bool) bool { return v == nil || *v }

// Kind classifies a stage for the orchestrator: which of the four things this
// step is, not who runs it.
//
// The two reserved names win over agent: and run: because they name a step the
// engine implements itself. That is not the same as ignoring the fields: an
// agent: on implement selects the role that does the work (ImplementRole), and
// an agent: or run: on gate is a contradiction Validate rejects at load.
func (s Stage) Kind() string {
	switch {
	case s.Stage == StageImplement:
		return "builtin-implement"
	case s.Stage == StageGate:
		return "builtin-gate"
	case s.Agent != "":
		return "agent"
	case s.Run != "":
		return "run"
	}
	return "invalid"
}

// Defaults.
//
// DefaultMaxRounds and DefaultRetry are measured rather than chosen, by
// scripts/resume-vs-fresh.sh: the same fixture epic drained twice from one
// commit, once round-heavy and once attempt-heavy, spending the same six model
// processes either way. Recovering inside the session came out 18% cheaper in
// total_cost_usd and 26% faster, winning on every issue. So rounds are the
// primary recovery knob and a fresh attempt is the safety net, not the reverse.
//
// 3 rounds rather than 2 comes from the dogfood run, where a hard review took
// three rounds to pass; rather than more, because nothing past round two has
// been measured — a long attempt eventually loses its cache prefix to the
// five-minute TTL and its transcript to an autocompact, and both of those spend
// the saving the first rounds earned. 1 retry, because a fresh attempt is the
// only escape from a session that has gone wrong in itself, and that is all it
// is for.
//
// Re-run the measurement when the worker prompt or the default model changes:
// both move the re-derivation cost the result turns on.
const (
	DefaultConcurrency     = 5
	DefaultRetry           = 1
	DefaultMaxRounds       = 3
	DefaultCommandTimeout  = 900 // seconds
	DefaultBranchPrefix    = "bd-auto/"
	DefaultOutputTailBytes = 4000
	// DefaultEpicBranchPrefix is prepended to the epic's ID to form the branch
	// a run is staged on. It sits under the worker branch prefix so one
	// `git branch -d bd-auto/...` sweep still finds everything bd-auto made.
	DefaultEpicBranchPrefix = DefaultBranchPrefix + "epic/"
	// DefaultHandoffRemote is where the epic branch is pushed for its pull
	// request.
	DefaultHandoffRemote = "origin"
	// DefaultAskTimeout is how long a question waits for a human, in seconds.
	// An hour, because the cost of waiting is one idle worker and a handful of
	// cheap polls, and the cost of giving up early is a decision nobody made.
	DefaultAskTimeout = 3600
	// DefaultAskHold is how long one ask_user call blocks before handing back a
	// ticket, in seconds. Five minutes: Claude Code kills an idle stdio tool
	// call after thirty, so this leaves six times the margin, and a human who
	// is watching answers inside the first call.
	DefaultAskHold = 300
)

// DefaultAskRoles are the roles that may put a question to the human. See Ask.
func DefaultAskRoles() []string {
	return []string{string(runner.RoleWorker), string(runner.RoleIntegrator)}
}

// Config is the resolved configuration for a run.
type Config struct {
	// Gate commands must all exit 0 for an issue to pass the gate stage.
	Gate []Command `yaml:"gate"`
	// Pipeline is the ordered per-issue pipeline.
	Pipeline []Stage `yaml:"pipeline"`
	// Concurrency caps how many workers run in one wave.
	Concurrency int `yaml:"concurrency"`
	// Autonomy controls where the run pauses.
	Autonomy Autonomy `yaml:"autonomy"`
	// Retry is how many extra attempts a failed issue gets. 1 means
	// "retry once fresh, then park".
	Retry int `yaml:"retry"`
	// MaxRounds is the run-level feedback-round budget: how many times a
	// failing check may send work back to the same worker within one attempt.
	// A stage that sets its own max_rounds wins over this.
	MaxRounds int `yaml:"max_rounds"`
	// Runners configures the model backend per role. Each entry resolves over
	// the "default" entry; see Config.Runner.
	Runners map[string]RunnerSpec `yaml:"runners"`
	// DiscoveredWork is "triage", "defer" or "immediate", and decides what a
	// wave barrier does with what the run's workers found.
	//
	// "triage" is the default. The barrier files nothing: findings are staged
	// in .beads/auto/triage.json and `bd-auto triage` is what turns one into an
	// issue, folds it into an issue that already exists, or discards it. It is
	// the default because filing is the irreversible half. Measured over this
	// repository's own history, discovered work peaked at 2.27 issues created
	// per issue closed, and the shape of it — nine parent issues, each with
	// exactly two children — is a model answering a question it is expected to
	// have an answer to rather than a run that learned eighteen things.
	//
	// "defer" files each finding as an issue but hides it from bd ready, so it
	// waits for a human rather than being offered to a later run. That protects
	// the next run and does nothing for the backlog a human reads.
	//
	// "immediate" files it and offers it. A run is scoped to issues a human
	// approved and its own allowlist refuses anything else, so this does not
	// feed work back into the run that found it. What it changes is the run
	// after this one, in a repo drained continuously with nobody triaging
	// between runs.
	DiscoveredWork string `yaml:"discovered_work"`
	// BranchPrefix is prepended to the issue ID to form a worker branch.
	BranchPrefix string `yaml:"branch_prefix"`
	// OutputTailBytes caps captured command output fed back into a retry.
	OutputTailBytes int `yaml:"output_tail_bytes"`
	// Handoff decides where the merges land and how the run reaches a human.
	Handoff Handoff `yaml:"handoff"`
	// Ask configures the ask_user tool a worker uses to put a question to the
	// human watching the run.
	Ask Ask `yaml:"ask"`
	// Hooks are the repo's own post-result interpreters: an agent or a command
	// hung on the moment an issue, a barrier or a run produced a result. They
	// are advisory — nothing a hook says changes what the run decided. See
	// hooks.go.
	Hooks Hooks `yaml:"hooks"`
	// Graph configures the code index the roles can query.
	Graph Graph `yaml:"graph"`

	// ForcePermissions replaces every role's resolved permissions when it is
	// set. It is what --dangerously-skip-permissions writes, and it is not a
	// yaml key: a config file that wants a permission level says so per role.
	//
	// It is narrowed onto the resolved config rather than carried through the
	// engine as another override, for the same reason the handoff switches are:
	// the config is already this process's snapshot of the run's settings, and
	// one place that decides what a role may do is worth more than a flag path
	// and a config path that have to agree.
	ForcePermissions runner.Permissions `yaml:"-"`

	// path is where this config was loaded from, empty if defaults.
	path string
	// agents are the repo's own agent files, keyed by the role each defines,
	// read once at Load in the main checkout. See agents.go.
	agents map[string]*Agent
}

// Path reports the file this config was loaded from, or "" for built-in
// defaults.
func (c *Config) Path() string { return c.path }

// Default returns the configuration used when no config file exists.
func Default() *Config {
	return &Config{
		Gate: nil,
		Pipeline: []Stage{
			{Stage: StageImplement, Agent: string(runner.RoleWorker)},
			{Stage: StageGate},
		},
		Concurrency:    DefaultConcurrency,
		Autonomy:       AutonomyAuto,
		Retry:          DefaultRetry,
		MaxRounds:      DefaultMaxRounds,
		DiscoveredWork: "triage",
		Graph: Graph{
			// Off; see the Graph doc comment. The rest are the values that
			// apply once somebody turns it on.
			ExcludeTests: true,
			Refresh:      true,
			Roles:        []string{"worker", "reviewer", "integrator"},
		},
		BranchPrefix:    DefaultBranchPrefix,
		OutputTailBytes: DefaultOutputTailBytes,
		Handoff: Handoff{
			Branch: Yes(),
			PR:     Yes(),
			Remote: DefaultHandoffRemote,
			Prefix: DefaultEpicBranchPrefix,
		},
		Ask: Ask{
			Enabled: Yes(),
			Timeout: intPtr(DefaultAskTimeout),
			Hold:    intPtr(DefaultAskHold),
			Roles:   DefaultAskRoles(),
		},
	}
}

// Load reads the config from repoRoot, applying defaults for absent fields. A
// missing file is not an error: it yields Default().
//
// It also reads the repo's agent files, so a repo with no .beads-auto.yaml at
// all still gets the agents it defined — dropping in reviewer.md is meant to be
// the whole of the change. They are read here, once, in the main checkout,
// because the text is carried in each request rather than read again from a
// worktree; see agents.go.
func Load(repoRoot string) (*Config, error) {
	p := filepath.Join(repoRoot, FileName)
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := Default()
			if err := cfg.loadAgents(repoRoot); err != nil {
				return nil, err
			}
			if err := cfg.Validate(); err != nil {
				return nil, err
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	cfg.path = p
	// Before applyDefaults and Validate, because an agent file defines a role:
	// `agent: security` with security.md and no runners: entry has to load, and
	// a pipeline naming a role that exists nowhere has to fail here.
	if err := cfg.loadAgents(repoRoot); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Concurrency <= 0 {
		c.Concurrency = d.Concurrency
	}
	if c.Autonomy == "" {
		c.Autonomy = d.Autonomy
	}
	if c.Retry < 0 {
		c.Retry = d.Retry
	}
	if c.MaxRounds <= 0 {
		c.MaxRounds = d.MaxRounds
	}
	if c.DiscoveredWork == "" {
		c.DiscoveredWork = d.DiscoveredWork
	}
	if c.BranchPrefix == "" {
		c.BranchPrefix = d.BranchPrefix
	}
	if c.OutputTailBytes <= 0 {
		c.OutputTailBytes = d.OutputTailBytes
	}
	if c.Handoff.Remote == "" {
		c.Handoff.Remote = d.Handoff.Remote
	}
	if c.Handoff.Prefix == "" {
		c.Handoff.Prefix = d.Handoff.Prefix
	}
	if c.Ask.Timeout == nil {
		c.Ask.Timeout = d.Ask.Timeout
	}
	if c.Ask.Hold == nil || *c.Ask.Hold <= 0 {
		c.Ask.Hold = d.Ask.Hold
	}
	if c.Ask.Roles == nil {
		c.Ask.Roles = d.Ask.Roles
	}
	if len(c.Pipeline) == 0 {
		c.Pipeline = d.Pipeline
	}
	// The implement stage is implicit and always first: a pipeline that omits
	// it still has to be implemented by someone.
	//
	// Only where it is missing altogether, though. A pipeline that names it
	// somewhere other than the head is a mistake Validate reports against the
	// line the reader wrote, and prepending a second one first would make that
	// report point at an index nobody typed.
	if len(c.Pipeline) > 0 && !hasStage(c.Pipeline, StageImplement) {
		c.Pipeline = append([]Stage{{Stage: StageImplement}}, c.Pipeline...)
	}
	for i := range c.Pipeline {
		// An implement stage that names nobody is run by the worker, which is
		// what it has always meant. Writing it into the resolved config rather
		// than deciding it downstream is the point: `bd-auto config show` then
		// answers "who implements this" the same way it answers it for every
		// other stage.
		if c.Pipeline[i].Stage == StageImplement && c.Pipeline[i].Agent == "" {
			c.Pipeline[i].Agent = string(runner.RoleWorker)
		}
		// Per-stage max_rounds wins where it is set; where it is not, the
		// stage inherits the run-level budget.
		if c.Pipeline[i].Agent != "" && c.Pipeline[i].MaxRounds <= 0 {
			c.Pipeline[i].MaxRounds = c.MaxRounds
		}
		if c.Pipeline[i].Timeout <= 0 {
			c.Pipeline[i].Timeout = DefaultCommandTimeout
		}
	}
	c.applyHookDefaults()
	for i := range c.Gate {
		if c.Gate[i].Timeout <= 0 {
			c.Gate[i].Timeout = DefaultCommandTimeout
		}
		if c.Gate[i].Name == "" {
			c.Gate[i].Name = fmt.Sprintf("gate-%d", i+1)
		}
	}
}

func hasStage(p []Stage, name string) bool {
	for _, s := range p {
		if s.Stage == name {
			return true
		}
	}
	return false
}

// Validate checks the config for contradictions that would only surface
// mid-run, when they are far more expensive.
func (c *Config) Validate() error {
	if !c.Autonomy.Valid() {
		return fmt.Errorf("autonomy: %q is not one of auto, wave", c.Autonomy)
	}
	switch c.DiscoveredWork {
	case "triage", "defer", "immediate":
	default:
		return fmt.Errorf("discovered_work: %q is not one of triage, defer, immediate", c.DiscoveredWork)
	}
	if !strings.HasSuffix(c.BranchPrefix, "/") {
		return fmt.Errorf("branch_prefix: %q must end with /", c.BranchPrefix)
	}
	if !strings.HasSuffix(c.Handoff.Prefix, "/") {
		return fmt.Errorf("handoff.prefix: %q must end with /", c.Handoff.Prefix)
	}
	// A pull request needs a head branch that is not the base. Asking for one
	// while merging straight into the base branch is a contradiction, and
	// resolving it quietly either way would give someone the opposite of what
	// they wrote down.
	if c.Handoff.PR != nil && *c.Handoff.PR && !enabled(c.Handoff.Branch) {
		return errors.New("handoff: pr: true needs branch: true; " +
			"a pull request has nothing to open from when the run merges straight into its base branch")
	}
	if err := c.validateRunners(); err != nil {
		return err
	}
	if err := c.validateAgents(); err != nil {
		return err
	}
	if c.Ask.Timeout != nil && *c.Ask.Timeout < 0 {
		return fmt.Errorf("ask: timeout: %d is negative; use 0 to wait forever", *c.Ask.Timeout)
	}
	if c.Ask.Hold != nil && *c.Ask.Hold < 0 {
		return fmt.Errorf("ask: hold: %d is negative", *c.Ask.Hold)
	}
	for i, role := range c.Ask.Roles {
		if !c.RoleDefined(role) {
			return fmt.Errorf("ask: roles[%d]: %q is not a defined runner role; valid roles are %s",
				i, role, strings.Join(c.Roles(), ", "))
		}
	}
	seen := map[string]bool{}
	for i, s := range c.Pipeline {
		if s.Stage == "" {
			return fmt.Errorf("pipeline[%d]: stage name is required", i)
		}
		// Said before the duplicate check, which would otherwise report a
		// misplaced implement stage as a duplicate of the one applyDefaults
		// prepended — true, and no help at all in finding the line to move.
		if s.Stage == StageImplement && i != 0 {
			return fmt.Errorf("pipeline[%d]: the implement stage must come first; "+
				"it creates the worktree and branch every later stage runs against", i)
		}
		if seen[s.Stage] {
			return fmt.Errorf("pipeline[%d]: duplicate stage %q", i, s.Stage)
		}
		seen[s.Stage] = true
		// Ahead of the agent-or-run check, which applyDefaults would otherwise
		// make unreadable here: it fills the implement stage's agent in, so a
		// stage that only said run: gets told it said both.
		if s.Stage == StageImplement && s.Run != "" {
			return fmt.Errorf("pipeline[%d] (implement): the implement stage takes no run:; "+
				"it is a model doing the work, and agent: names which role does it", i)
		}
		if s.Agent != "" && s.Run != "" {
			return fmt.Errorf("pipeline[%d] (%s): set agent or run, not both", i, s.Stage)
		}
		// The gate is the one step that runs under nobody: it is the gate
		// commands, and a role written on it would be spawned by nothing. Every
		// other stage answers "who runs this", so saying so here is cheaper
		// than a config whose answer is quietly discarded.
		if s.Stage == StageGate && (s.Agent != "" || s.Run != "") {
			return fmt.Errorf("pipeline[%d] (gate): the gate stage takes no agent: or run:; "+
				"it runs the gate: commands, which are commands rather than a judgement", i)
		}
		if s.Kind() == "invalid" {
			return fmt.Errorf("pipeline[%d] (%s): needs either agent or run", i, s.Stage)
		}
		// agent: now names a runner role rather than a subagent to dispatch.
		// Catching a stale name here costs a line of output; catching it at
		// dispatch costs a wave.
		if s.Agent != "" && !c.RoleDefined(s.Agent) {
			return fmt.Errorf("pipeline[%d] (%s): agent: %q is not a defined runner role; "+
				"valid roles are %s. Define it with a key under runners:, or with an agent file at %s",
				i, s.Stage, s.Agent, strings.Join(c.Roles(), ", "), c.agentPathHint(s.Agent))
		}
	}
	for i, g := range c.Gate {
		if g.Run == "" {
			return fmt.Errorf("gate[%d] (%s): run is required", i, g.Name)
		}
	}
	return c.validateHooks()
}

// ImplementRole is the role that runs the implement stage: whatever that
// stage's agent: names, and the worker where a config named nobody.
//
// It is the only supported way to ask "who does the work", so a repo that puts
// its own role on implement gets that role spawned, prompted and logged under
// its own name. What it does not move is the lifecycle: the worktree, the
// branch and the resumable session still belong to the stage named implement.
// See the StageImplement comment.
func (c *Config) ImplementRole() runner.Role {
	for _, s := range c.Pipeline {
		if s.Stage == StageImplement && s.Agent != "" {
			return runner.Role(s.Agent)
		}
	}
	return runner.RoleWorker
}

// MaxRoundsFor returns the feedback-round budget for a stage: the stage's own
// max_rounds where it is set, otherwise the run-level max_rounds. Per-stage
// wins, and this function is the only place that precedence is decided.
func (c *Config) MaxRoundsFor(s Stage) int {
	if s.MaxRounds > 0 {
		return s.MaxRounds
	}
	if c.MaxRounds > 0 {
		return c.MaxRounds
	}
	return DefaultMaxRounds
}

// Branch returns the worker branch name for an issue.
func (c *Config) Branch(issueID string) string {
	return c.BranchPrefix + issueID
}

// StageOnBranch reports whether a run's merges land on a temporary epic branch
// rather than on the branch the run started from.
func (c *Config) StageOnBranch() bool { return enabled(c.Handoff.Branch) }

// TriageDiscovered reports whether a barrier stages what its workers found for
// a human instead of filing it.
//
// Explicit, unlike DeferDiscovered's default-to-safe: a Config built in code
// that never set the field files as it always did, so nothing that predates
// triage starts silently staging into a file its caller will never read.
func (c *Config) TriageDiscovered() bool { return c.DiscoveredWork == "triage" }

// Graph is the code index built with graphify at the start of a run.
//
// Off by default, and deliberately. plans/graph-index.md measured the premise
// before anything was designed around it: building the index is free — pure AST
// extraction, no API key, 1.9s for 2199 nodes on this repo — but a broad query
// returns a truncated list of symbol locations rather than an explanation, so
// the saving is on searching and not on reading. This repo sets knobs from
// measurements, and the A/B that would justify turning this on has not been
// run.
type Graph struct {
	// Enabled builds the index at the start of a run.
	Enabled bool `yaml:"enabled"`
	// ExcludeTests keeps test files out of it. Measured: with them in, this
	// repo's most-connected nodes are testRepo, newIssues, testCfg and engine —
	// the test harness rather than the architecture, which is precisely what the
	// index is asked for. It trades away "where is this tested?" as an index
	// question, which is the one thing a human might reasonably want back.
	ExcludeTests bool `yaml:"exclude_tests"`
	// Refresh rebuilds the index at each wave barrier, after the integrator has
	// merged. Only meaningful with Enabled.
	Refresh bool `yaml:"refresh"`
	// Roles are told the index exists. A role not named here is never told, so
	// it cannot spend tokens on a tool it was not meant to have.
	Roles []string `yaml:"roles"`
	// Timeout bounds one build; zero means graph.DefaultTimeout.
	Timeout int `yaml:"timeout"`
}

// IndexFor reports whether a role is told about the code index.
func (c *Config) IndexFor(role string) bool {
	if !c.Graph.Enabled {
		return false
	}
	for _, r := range c.Graph.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// DeferDiscovered reports whether work a worker discovered is filed hidden from
// bd ready, so it waits for a human.
//
// Anything other than an explicit "immediate" defers, including a Config built
// in code that never set the field. Load fills the default in and Validate
// refuses a fourth value, but this is also reached from a CLI flag or a test
// where neither ran — and of the two ways to be wrong, quietly deferring work
// somebody wanted offered is much cheaper than quietly offering work somebody
// wanted held.
func (c *Config) DeferDiscovered() bool { return c.DiscoveredWork != "immediate" }

// OpenPR reports whether a finished run opens a pull request.
//
// It is false whenever the run is not staged, whatever the pr key says. Load
// already refuses that combination, but this method is also reached from a
// Config built in code — a CLI flag, a test — where nothing validated it.
func (c *Config) OpenPR() bool { return c.StageOnBranch() && enabled(c.Handoff.PR) }

// HandoffRemote is the remote an epic branch is pushed to.
func (c *Config) HandoffRemote() string {
	if c.Handoff.Remote == "" {
		return DefaultHandoffRemote
	}
	return c.Handoff.Remote
}

// EpicBranchPrefix is what an epic branch name starts with.
func (c *Config) EpicBranchPrefix() string {
	if c.Handoff.Prefix == "" {
		return DefaultEpicBranchPrefix
	}
	return c.Handoff.Prefix
}

// HasGate reports whether any gate command is configured. With no gate, the
// gate stage passes trivially, which is what lets the tool be used immediately
// in a repo with no test suite.
func (c *Config) HasGate() bool { return len(c.Gate) > 0 }

// AskEnabled reports whether a run offers the ask_user tool at all.
func (c *Config) AskEnabled() bool { return enabled(c.Ask.Enabled) }

// AskTimeout is how long a question waits for a human. A configured 0 means
// never give up, and is returned as a negative duration, which is how the
// broker spells the same thing.
func (c *Config) AskTimeout() time.Duration {
	if c.Ask.Timeout == nil {
		return DefaultAskTimeout * time.Second
	}
	if *c.Ask.Timeout == 0 {
		return -1
	}
	return time.Duration(*c.Ask.Timeout) * time.Second
}

// AskHold is how long one ask_user call blocks before handing back a ticket.
func (c *Config) AskHold() time.Duration {
	if c.Ask.Hold == nil || *c.Ask.Hold <= 0 {
		return DefaultAskHold * time.Second
	}
	return time.Duration(*c.Ask.Hold) * time.Second
}

// AskRole reports whether a role may put a question to the human.
func (c *Config) AskRole(role string) bool {
	if !c.AskEnabled() {
		return false
	}
	roles := c.Ask.Roles
	if roles == nil {
		roles = DefaultAskRoles()
	}
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}
