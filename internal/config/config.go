// Package config loads .beads-auto.yaml, the per-repo configuration for a
// bd-auto run: what the gate is, what stages each issue passes through, and how
// aggressively to run.
//
// Every field has a default, so a repo with no config file at all still works.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the config file looked for at the repo root.
const FileName = ".beads-auto.yaml"

// Autonomy controls where a run pauses for the human.
type Autonomy string

const (
	// AutonomyAuto drains the epic without stopping.
	AutonomyAuto Autonomy = "auto"
	// AutonomyWave pauses at each wave barrier.
	AutonomyWave Autonomy = "wave"
	// AutonomyIssue pauses after every issue.
	AutonomyIssue Autonomy = "issue"
)

// Valid reports whether a is a recognised autonomy mode.
func (a Autonomy) Valid() bool {
	switch a {
	case AutonomyAuto, AutonomyWave, AutonomyIssue:
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
	// Agent names a subagent to invoke. The binary does not run these; it
	// reports them so the orchestrator can dispatch them via the Agent tool.
	Agent string `yaml:"agent"`
	// Run is a shell command executed by this binary.
	Run string `yaml:"run"`
	// MaxRounds caps how many times a failing agent stage may send work back
	// to the same worker before the attempt is considered failed.
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
const (
	DefaultConcurrency     = 5
	DefaultRetry           = 1
	DefaultMaxRounds       = 3
	DefaultCommandTimeout  = 900 // seconds
	DefaultBranchPrefix    = "bd-auto/"
	DefaultReportMaxLines  = 25
	DefaultOutputTailBytes = 4000
)

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
	// DiscoveredWork is "defer" or "immediate". Workers file discovered work
	// either way; "defer" keeps it out of the current run.
	DiscoveredWork string `yaml:"discovered_work"`
	// BranchPrefix is prepended to the issue ID to form a worker branch.
	BranchPrefix string `yaml:"branch_prefix"`
	// ReportMaxLines caps a worker's report, protecting orchestrator context.
	ReportMaxLines int `yaml:"report_max_lines"`
	// OutputTailBytes caps captured command output fed back into a retry.
	OutputTailBytes int `yaml:"output_tail_bytes"`

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
		DiscoveredWork:  "defer",
		BranchPrefix:    DefaultBranchPrefix,
		ReportMaxLines:  DefaultReportMaxLines,
		OutputTailBytes: DefaultOutputTailBytes,
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
	if c.DiscoveredWork == "" {
		c.DiscoveredWork = d.DiscoveredWork
	}
	if c.BranchPrefix == "" {
		c.BranchPrefix = d.BranchPrefix
	}
	if c.ReportMaxLines <= 0 {
		c.ReportMaxLines = d.ReportMaxLines
	}
	if c.OutputTailBytes <= 0 {
		c.OutputTailBytes = d.OutputTailBytes
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
		if c.Pipeline[i].Agent != "" && c.Pipeline[i].MaxRounds <= 0 {
			c.Pipeline[i].MaxRounds = DefaultMaxRounds
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
		return fmt.Errorf("autonomy: %q is not one of auto, wave, issue", c.Autonomy)
	}
	switch c.DiscoveredWork {
	case "defer", "immediate":
	default:
		return fmt.Errorf("discovered_work: %q is not one of defer, immediate", c.DiscoveredWork)
	}
	if !strings.HasSuffix(c.BranchPrefix, "/") {
		return fmt.Errorf("branch_prefix: %q must end with /", c.BranchPrefix)
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
	}
	for i, g := range c.Gate {
		if g.Run == "" {
			return fmt.Errorf("gate[%d] (%s): run is required", i, g.Name)
		}
	}
	return nil
}

// Branch returns the worker branch name for an issue.
func (c *Config) Branch(issueID string) string {
	return c.BranchPrefix + issueID
}

// HasGate reports whether any gate command is configured. With no gate, the
// gate stage passes trivially, which is what lets the tool be used immediately
// in a repo with no test suite.
func (c *Config) HasGate() bool { return len(c.Gate) > 0 }
