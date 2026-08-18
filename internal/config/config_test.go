package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMissingConfigYieldsDefaults(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("a repo with no config must still work, got %v", err)
	}
	if cfg.Concurrency != DefaultConcurrency || cfg.Autonomy != AutonomyAuto || cfg.Retry != DefaultRetry {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.HasGate() {
		t.Fatal("no gate should be configured by default")
	}
	if len(cfg.Pipeline) != 2 || cfg.Pipeline[0].Stage != StageImplement || cfg.Pipeline[1].Stage != StageGate {
		t.Fatalf("default pipeline should be implement then gate, got %+v", cfg.Pipeline)
	}
	if cfg.Path() != "" {
		t.Fatalf("defaults should report no path, got %q", cfg.Path())
	}
}

func TestLoadFullConfig(t *testing.T) {
	dir := write(t, `
gate:
  - name: build
    run: go build ./...
  - name: test
    run: go test ./...
pipeline:
  - stage: implement
  - stage: gate
  - stage: review
    agent: reviewer
  - stage: security
    run: ./scripts/sec.sh
    optional: true
concurrency: 3
autonomy: wave
retry: 2
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Gate) != 2 || cfg.Gate[0].Name != "build" {
		t.Fatalf("gate not parsed: %+v", cfg.Gate)
	}
	if cfg.Concurrency != 3 || cfg.Autonomy != AutonomyWave || cfg.Retry != 2 {
		t.Fatalf("scalars not parsed: %+v", cfg)
	}
	if len(cfg.Pipeline) != 4 {
		t.Fatalf("want 4 stages, got %d", len(cfg.Pipeline))
	}
	if k := cfg.Pipeline[2].Kind(); k != "agent" {
		t.Fatalf("review should be an agent stage, got %q", k)
	}
	if k := cfg.Pipeline[3].Kind(); k != "run" {
		t.Fatalf("security should be a run stage, got %q", k)
	}
	if cfg.Pipeline[2].MaxRounds != DefaultMaxRounds {
		t.Fatalf("agent stages should default to %d rounds, got %d", DefaultMaxRounds, cfg.Pipeline[2].MaxRounds)
	}
	if cfg.Gate[0].Timeout != DefaultCommandTimeout {
		t.Fatal("gate commands should get a default timeout")
	}
}

// A pipeline that forgets the implement stage still has to be implemented by
// someone, so it is prepended rather than silently skipped.
func TestImplementStageIsPrepended(t *testing.T) {
	dir := write(t, `
pipeline:
  - stage: gate
  - stage: review
    agent: reviewer
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline[0].Stage != StageImplement {
		t.Fatalf("implement should be prepended, got %+v", cfg.Pipeline)
	}
	if len(cfg.Pipeline) != 3 {
		t.Fatalf("want 3 stages after prepend, got %d", len(cfg.Pipeline))
	}
}

func TestValidationRejectsContradictions(t *testing.T) {
	cases := map[string]string{
		"agent and run together": `
pipeline:
  - stage: implement
  - stage: review
    agent: reviewer
    run: ./x.sh
`,
		"stage with neither": `
pipeline:
  - stage: implement
  - stage: mystery
`,
		"duplicate stage": `
pipeline:
  - stage: implement
  - stage: gate
  - stage: gate
`,
		"bad autonomy": `
autonomy: whenever
`,
		"bad discovered_work": `
discovered_work: sometimes
`,
		"branch prefix without slash": `
branch_prefix: bdauto
`,
		"gate without run": `
gate:
  - name: build
`,
		"a pull request with no branch to open it from": `
handoff:
  branch: false
  pr: true
`,
		"handoff prefix without slash": `
handoff:
  prefix: bd-auto-epic
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatal("expected a validation error, got none")
			}
		})
	}
}

func TestMalformedYAMLIsAnError(t *testing.T) {
	if _, err := Load(write(t, "gate: [unclosed\n")); err == nil {
		t.Fatal("malformed YAML must be an error, not silent defaults")
	}
}

func TestBranchName(t *testing.T) {
	cfg := Default()
	if got := cfg.Branch("bd-42"); got != "bd-auto/bd-42" {
		t.Fatalf("got %q", got)
	}
	cfg.BranchPrefix = "auto/"
	if got := cfg.Branch("x"); !strings.HasPrefix(got, "auto/") {
		t.Fatalf("got %q", got)
	}
}

func TestNegativeAndZeroValuesFallBackToDefaults(t *testing.T) {
	dir := write(t, "concurrency: 0\nmax_rounds: -4\noutput_tail_bytes: 0\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency != DefaultConcurrency ||
		cfg.MaxRounds != DefaultMaxRounds ||
		cfg.OutputTailBytes != DefaultOutputTailBytes {
		t.Fatalf("nonsense values should fall back to defaults: %+v", cfg)
	}
}

// The handoff is on by default, and its two switches are independent in exactly
// one direction: turning the pull request off leaves the epic branch, and
// turning the branch off takes the pull request with it.
func TestHandoffDefaultsAndSwitches(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		branch, pr     bool
		remote, prefix string
	}{
		{
			name:   "a repo with no config stages and opens a pull request",
			branch: true, pr: true,
			remote: DefaultHandoffRemote, prefix: DefaultEpicBranchPrefix,
		},
		{
			name:   "an empty handoff block changes nothing",
			body:   "handoff: {}\n",
			branch: true, pr: true,
			remote: DefaultHandoffRemote, prefix: DefaultEpicBranchPrefix,
		},
		{
			name:   "the pull request switches off on its own",
			body:   "handoff:\n  pr: false\n",
			branch: true, pr: false,
			remote: DefaultHandoffRemote, prefix: DefaultEpicBranchPrefix,
		},
		{
			name:   "no epic branch takes the pull request with it",
			body:   "handoff:\n  branch: false\n",
			branch: false, pr: false,
			remote: DefaultHandoffRemote, prefix: DefaultEpicBranchPrefix,
		},
		{
			name:   "the remote and the prefix are configurable",
			body:   "handoff:\n  remote: upstream\n  prefix: staging/\n",
			branch: true, pr: true,
			remote: "upstream", prefix: "staging/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(write(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.StageOnBranch() != tc.branch || cfg.OpenPR() != tc.pr {
				t.Fatalf("branch=%v pr=%v, want branch=%v pr=%v",
					cfg.StageOnBranch(), cfg.OpenPR(), tc.branch, tc.pr)
			}
			if cfg.HandoffRemote() != tc.remote || cfg.EpicBranchPrefix() != tc.prefix {
				t.Fatalf("remote=%q prefix=%q, want %q and %q",
					cfg.HandoffRemote(), cfg.EpicBranchPrefix(), tc.remote, tc.prefix)
			}
		})
	}
}

// --- ask ---

// The defaults are the whole interface for a repo that writes no ask: block,
// and each of them is a decision: the tool is on, a question waits an hour, one
// call blocks five minutes, and the reviewer does not get to ask the author of
// the work it is judging.
func TestAskDefaults(t *testing.T) {
	c := Default()
	if !c.AskEnabled() {
		t.Fatal("the tool is off by default")
	}
	if got := c.AskTimeout(); got != DefaultAskTimeout*time.Second {
		t.Fatalf("timeout is %s", got)
	}
	if got := c.AskHold(); got != DefaultAskHold*time.Second {
		t.Fatalf("hold is %s", got)
	}
	// The hold has to be comfortably inside a backend's own limit on one call,
	// or the ticket is never handed back at all.
	if c.AskHold() >= c.AskTimeout() {
		t.Fatalf("a hold of %s is not shorter than the %s a question waits", c.AskHold(), c.AskTimeout())
	}
	if !c.AskRole("worker") || !c.AskRole("integrator") {
		t.Fatal("the roles that meet ambiguity cannot ask")
	}
	if c.AskRole("reviewer") {
		t.Fatal("the reviewer can question the author of the work it is judging")
	}
}

func TestAskConfigured(t *testing.T) {
	dir := write(t, `
ask:
  timeout: 120
  hold: 30
  roles: [worker, reviewer]
`)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.AskTimeout(); got != 120*time.Second {
		t.Fatalf("timeout is %s", got)
	}
	if got := c.AskHold(); got != 30*time.Second {
		t.Fatalf("hold is %s", got)
	}
	if !c.AskRole("reviewer") {
		t.Fatal("a role the config named cannot ask")
	}
	if c.AskRole("integrator") {
		t.Fatal("an explicit roles list did not replace the default")
	}
}

// 0 is a setting rather than an absence: it means a question waits until it is
// answered, which is a thing a repo with somebody always watching can choose.
func TestAskTimeoutZeroWaitsForever(t *testing.T) {
	dir := write(t, "ask:\n  timeout: 0\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.AskTimeout(); got >= 0 {
		t.Fatalf("timeout is %s, which is a deadline rather than none", got)
	}
}

func TestAskDisabled(t *testing.T) {
	dir := write(t, "ask:\n  enabled: false\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.AskEnabled() || c.AskRole("worker") {
		t.Fatal("the tool survived being turned off")
	}
}

// A role that does not exist is a typo, and catching it at load costs a line of
// output where catching it mid-wave costs a worker that cannot ask.
func TestAskRejectsAnUnknownRole(t *testing.T) {
	dir := write(t, "ask:\n  roles: [worker, auditor]\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("an undefined role was accepted")
	} else if !strings.Contains(err.Error(), "auditor") {
		t.Fatalf("the error does not name the role: %v", err)
	}
}
