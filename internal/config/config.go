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

// Stage is one step in the per-issue pipeline. Exactly one of Agent or Run must
// be set, except for the built-in stages which need neither.
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

// Kind classifies a stage for the orchestrator.
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
	// DiscoveredWork is "defer" or "immediate". Either way bd-auto files what
	// its workers found, at the wave barrier; "defer" hides the result from bd
	// ready so it waits for a human rather than being offered to a later run.
	//
	// "defer" is the default and is almost always what is wanted. A run is
	// scoped to issues a human approved, and its own allowlist already refuses
	// anything else — so "immediate" does not feed work back into the run that
	// found it. What it changes is the run after this one, in a repo where the
	// backlog is drained continuously and nobody triages between runs.
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
}

// Path reports the file this config was loaded from, or "" for built-in
// defaults.
func (c *Config) Path() string { return c.path }

// Default returns the configuration used when no config file exists.
func Default() *Config {
	return &Config{
		Gate: nil,
		Pipeline: []Stage{
			{Stage: StageImplement},
			{Stage: StageGate},
		},
		Concurrency:     DefaultConcurrency,
		Autonomy:        AutonomyAuto,
		Retry:           DefaultRetry,
		MaxRounds:       DefaultMaxRounds,
		DiscoveredWork:  "defer",
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
func Load(repoRoot string) (*Config, error) {
	p := filepath.Join(repoRoot, FileName)
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	cfg.path = p
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
	if len(c.Pipeline) > 0 && c.Pipeline[0].Stage != StageImplement {
		c.Pipeline = append([]Stage{{Stage: StageImplement}}, c.Pipeline...)
	}
	for i := range c.Pipeline {
		// Per-stage max_rounds wins where it is set; where it is not, the
		// stage inherits the run-level budget.
		if c.Pipeline[i].Agent != "" && c.Pipeline[i].MaxRounds <= 0 {
			c.Pipeline[i].MaxRounds = c.MaxRounds
		}
		if c.Pipeline[i].Timeout <= 0 {
			c.Pipeline[i].Timeout = DefaultCommandTimeout
		}
	}
	for i := range c.Gate {
		if c.Gate[i].Timeout <= 0 {
			c.Gate[i].Timeout = DefaultCommandTimeout
		}
		if c.Gate[i].Name == "" {
			c.Gate[i].Name = fmt.Sprintf("gate-%d", i+1)
		}
	}
}

// Validate checks the config for contradictions that would only surface
// mid-run, when they are far more expensive.
func (c *Config) Validate() error {
	if !c.Autonomy.Valid() {
		return fmt.Errorf("autonomy: %q is not one of auto, wave", c.Autonomy)
	}
	switch c.DiscoveredWork {
	case "defer", "immediate":
	default:
		return fmt.Errorf("discovered_work: %q is not one of defer, immediate", c.DiscoveredWork)
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
		if seen[s.Stage] {
			return fmt.Errorf("pipeline[%d]: duplicate stage %q", i, s.Stage)
		}
		seen[s.Stage] = true
		if s.Agent != "" && s.Run != "" {
			return fmt.Errorf("pipeline[%d] (%s): set agent or run, not both", i, s.Stage)
		}
		if s.Kind() == "invalid" {
			return fmt.Errorf("pipeline[%d] (%s): needs either agent or run", i, s.Stage)
		}
		// agent: now names a runner role rather than a subagent to dispatch.
		// Catching a stale name here costs a line of output; catching it at
		// dispatch costs a wave.
		if s.Agent != "" && !c.RoleDefined(s.Agent) {
			return fmt.Errorf("pipeline[%d] (%s): agent: %q is not a defined runner role; valid roles are %s",
				i, s.Stage, s.Agent, strings.Join(c.Roles(), ", "))
		}
	}
	for i, g := range c.Gate {
		if g.Run == "" {
			return fmt.Errorf("gate[%d] (%s): run is required", i, g.Name)
		}
	}
	return nil
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

// DeferDiscovered reports whether work a worker discovered is filed hidden from
// bd ready, so it waits for a human.
//
// Anything other than an explicit "immediate" defers, including a Config built
// in code that never set the field. Load fills the default in and Validate
// refuses a third value, but this is also reached from a CLI flag or a test
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
