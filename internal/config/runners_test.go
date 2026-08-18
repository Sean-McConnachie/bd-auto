package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/runner"
)

// A repo with no runners: block still has to be able to spawn something, and
// the reviewer still has to come out read-only.
func TestRunnerDefaultsWithoutARunnersBlock(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	worker := cfg.Runner(string(runner.RoleWorker))
	want := runner.Spec{
		Provider:    DefaultProvider,
		Model:       DefaultModel,
		Permissions: runner.PermAuto,
		Resume:      true,
	}
	if !reflect.DeepEqual(worker, want) {
		t.Fatalf("worker = %+v, want %+v", worker, want)
	}
	if worker.Timeout != 0 {
		t.Fatalf("the default timeout must be unlimited, got %v", worker.Timeout)
	}

	rev := cfg.Runner(string(runner.RoleReviewer))
	if rev.Model != DefaultReviewerModel || rev.Permissions != runner.PermScoped {
		t.Fatalf("reviewer = %+v", rev)
	}
	if rev.Resume {
		t.Fatal("reviewers judge fresh: resume must default to false")
	}
	if !reflect.DeepEqual(rev.AllowedTools, DefaultReviewerTools()) {
		t.Fatalf("reviewer tools = %v", rev.AllowedTools)
	}
	if rev.Provider != DefaultProvider {
		t.Fatalf("reviewer should inherit the default provider, got %q", rev.Provider)
	}
}

func TestRunnerRoleResolution(t *testing.T) {
	full := `
runners:
  default:
    provider: fake
    model: opus
    permissions: auto
    timeout: 600
    extra_args: [--verbose]
  reviewer:
    model: sonnet
    permissions: scoped
    resume: false
  integrator:
    model: haiku
  security:
    permissions: bypass
    timeout: 0
pipeline:
  - stage: implement
  - stage: security
    agent: security
`
	cases := []struct {
		name string
		body string
		role string
		want runner.Spec
	}{
		{
			name: "a role with no entry is the default",
			body: full,
			role: "worker",
			want: runner.Spec{
				Provider: "fake", Model: "opus", Permissions: runner.PermAuto,
				ExtraArgs: []string{"--verbose"}, Timeout: 600 * time.Second, Resume: true,
			},
		},
		{
			name: "a partial override keeps every field it does not name",
			body: full,
			role: "reviewer",
			want: runner.Spec{
				Provider: "fake", Model: "sonnet", Permissions: runner.PermScoped,
				AllowedTools: DefaultReviewerTools(), DeniedTools: DefaultReviewerDenied(),
				ExtraArgs: []string{"--verbose"}, Timeout: 600 * time.Second, Resume: false,
			},
		},
		{
			name: "one field overridden, the rest inherited",
			body: full,
			role: "integrator",
			want: runner.Spec{
				Provider: "fake", Model: "haiku", Permissions: runner.PermAuto,
				ExtraArgs: []string{"--verbose"}, Timeout: 600 * time.Second, Resume: true,
			},
		},
		{
			name: "a custom role is a role like any other",
			body: full,
			role: "security",
			want: runner.Spec{
				Provider: "fake", Model: "opus", Permissions: runner.PermBypass,
				ExtraArgs: []string{"--verbose"}, Timeout: 0, Resume: true,
			},
		},
		{
			name: "an empty deny list overrides rather than inherits",
			body: "runners:\n  reviewer:\n    denied_tools: []\n",
			role: "reviewer",
			want: runner.Spec{
				Provider: DefaultProvider, Model: DefaultReviewerModel,
				Permissions: runner.PermScoped, AllowedTools: DefaultReviewerTools(),
				Resume: false,
			},
		},
		{
			name: "an empty tool list overrides rather than inherits",
			body: "runners:\n  reviewer:\n    allowed_tools: []\n",
			role: "reviewer",
			want: runner.Spec{
				Provider: DefaultProvider, Model: DefaultReviewerModel,
				Permissions: runner.PermScoped, DeniedTools: DefaultReviewerDenied(),
				Resume: false,
			},
		},
		{
			name: "resume can be turned on for the reviewer",
			body: "runners:\n  reviewer:\n    resume: true\n",
			role: "reviewer",
			want: runner.Spec{
				Provider: DefaultProvider, Model: DefaultReviewerModel,
				Permissions: runner.PermScoped, AllowedTools: DefaultReviewerTools(),
				DeniedTools: DefaultReviewerDenied(), Resume: true,
			},
		},
		{
			name: "an explicit zero timeout means unlimited, not unset",
			body: "runners:\n  default:\n    timeout: 900\n  worker:\n    timeout: 0\n",
			role: "worker",
			want: runner.Spec{
				Provider: DefaultProvider, Model: DefaultModel,
				Permissions: runner.PermAuto, Resume: true, Timeout: 0,
			},
		},
		{
			name: "the default entry is resolvable on its own",
			body: "runners:\n  default:\n    model: sonnet\n",
			role: RoleDefault,
			want: runner.Spec{
				Provider: DefaultProvider, Model: "sonnet",
				Permissions: runner.PermAuto, Resume: true,
			},
		},
		{
			name: "a plugin-era agent name resolves to the role it meant",
			body: "runners:\n  reviewer:\n    model: haiku\n",
			role: "bd-reviewer",
			want: runner.Spec{
				Provider: DefaultProvider, Model: "haiku",
				Permissions: runner.PermScoped, AllowedTools: DefaultReviewerTools(),
				DeniedTools: DefaultReviewerDenied(), Resume: false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(write(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Runner(tc.role)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Runner(%q):\n got %+v\nwant %+v", tc.role, got, tc.want)
			}
		})
	}
}

// A config entry defined under its own name wins over the alias table, so a
// repo that really does have a role called bd-reviewer gets that role.
func TestExplicitRoleBeatsAlias(t *testing.T) {
	cfg, err := Load(write(t, "runners:\n  bd-reviewer:\n    model: haiku\n  reviewer:\n    model: sonnet\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Runner("bd-reviewer").Model; got != "haiku" {
		t.Fatalf("model = %q, want the explicitly defined role's haiku", got)
	}
}

// Nothing bd-auto ships may turn permission checks off by itself.
//
// This is pinned rather than left to the defaults test because it is a decision
// and not an incidental value, and because the argument for changing it is a
// good one that will be made again: measured against claude 2.1.233, a worker
// cannot finish under anything but bypass, so this default is knowingly one a
// plain drain will hit. It stands anyway — widening what a model may do is the
// user's call, made per repo under runners: or per run with
// --dangerously-skip-permissions. What that costs is paid in legibility
// instead, by deniedReason in internal/drain: a run that hits the refusal stops
// naming the tools and the flag rather than parking the issue as failed work.
func TestBypassIsNeverTheShippedDefault(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]runner.Permissions{
		string(runner.RoleWorker):     runner.PermAuto,
		string(runner.RoleIntegrator): runner.PermAuto,
		string(runner.RoleReviewer):   runner.PermScoped,
	}
	for role, w := range want {
		if got := cfg.Runner(role).Permissions; got != w {
			t.Errorf("%s permissions = %q, want %q", role, got, w)
		}
	}
}

// --dangerously-skip-permissions has to reach every role, including one that
// named its own level. A flag that quietly left the reviewer on scoped would
// leave a run just as stuck as it was.
func TestForcePermissionsOverridesEveryRole(t *testing.T) {
	cfg, err := Load(write(t, `
runners:
  default:
    permissions: auto
  reviewer:
    permissions: scoped
  security:
    permissions: auto
pipeline:
  - stage: implement
  - stage: security
    agent: security
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ForcePermissions = runner.PermBypass
	for _, role := range []string{"worker", "reviewer", "integrator", "security", RoleDefault} {
		if got := cfg.Runner(role).Permissions; got != runner.PermBypass {
			t.Fatalf("Runner(%q).Permissions = %q, want bypass", role, got)
		}
	}
}

// The reviewer judges the record; it does not write it. A review that ran
// bd close on the issue under review is what this asserts against, so it is
// asserted twice over: the allowlist names no verb that writes, and the deny
// list names each of them, because the deny list is the half that still applies
// when a run widens the level.
func TestReviewerCannotWriteIssueState(t *testing.T) {
	// Every bd verb that changes an issue, a dependency or the database under
	// it. bd show, list, ready and the rest of the read side are deliberately
	// absent: the reviewer needs them.
	writes := []string{
		"assign", "batch", "close", "comment", "create", "defer", "delete", "dep",
		"dolt", "edit", "import", "label", "link", "note", "priority", "q",
		"remember", "rename", "reopen", "set-state", "sql", "supersede", "sync",
		"tag", "undefer", "update",
	}

	cfg, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	spec := cfg.Runner(string(runner.RoleReviewer))
	denied := map[string]bool{}
	for _, rule := range spec.DeniedTools {
		denied[rule] = true
	}
	for _, verb := range writes {
		rule := "Bash(bd " + verb + ":*)"
		if !denied[rule] {
			t.Errorf("the reviewer's denied_tools is missing %s", rule)
		}
		for _, allowed := range spec.AllowedTools {
			if strings.HasPrefix(allowed, "Bash(bd "+verb) {
				t.Errorf("the reviewer's allowed_tools permits %q, which writes issue state", allowed)
			}
		}
	}

	// The reviewer still has to be able to read the issue it is judging, which
	// is the whole reason this is a verb list rather than Bash(bd:*).
	var canRead bool
	for _, allowed := range spec.AllowedTools {
		if allowed == "Bash(bd show:*)" {
			canRead = true
		}
	}
	if !canRead {
		t.Error("the reviewer cannot run bd show, so it cannot read what it judges")
	}
	if denied["Bash(bd show:*)"] {
		t.Error("bd show is denied; a deny rule beats the allowlist, so the reviewer reads nothing")
	}
}

// The one part of the reviewer's scoping that --dangerously-skip-permissions
// does not switch off. Deny rules are checked ahead of the permission level, so
// a run that had to be widened for its workers still has a reviewer that cannot
// close the issue it is judging.
func TestForcePermissionsKeepsTheDenyList(t *testing.T) {
	cfg, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ForcePermissions = runner.PermBypass
	spec := cfg.Runner(string(runner.RoleReviewer))
	if spec.Permissions != runner.PermBypass {
		t.Fatalf("reviewer permissions = %q, want the forced bypass", spec.Permissions)
	}
	if !reflect.DeepEqual(spec.DeniedTools, DefaultReviewerDenied()) {
		t.Fatalf("reviewer denied_tools = %v, want the built-in list", spec.DeniedTools)
	}
}

// The override is a flag, not a config key: a repo cannot arm it from the file.
func TestForcePermissionsIsNotAYamlKey(t *testing.T) {
	cfg, err := Load(write(t, "forcepermissions: bypass\nforce_permissions: bypass\nrunners:\n  default:\n    permissions: auto\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ForcePermissions != "" {
		t.Fatalf("ForcePermissions = %q; it must only ever be set in code", cfg.ForcePermissions)
	}
	if got := cfg.Runner("worker").Permissions; got != runner.PermAuto {
		t.Fatalf("worker permissions = %q, want the auto the file asked for", got)
	}
}

// A resolved spec is handed to a runner that may keep it; it must not be a
// window onto the config every later resolution shares.
func TestResolvedSpecDoesNotAliasConfig(t *testing.T) {
	cfg, err := Load(write(t, "runners:\n  default:\n    extra_args: [--a]\n"))
	if err != nil {
		t.Fatal(err)
	}
	first := cfg.Runner("worker")
	first.ExtraArgs[0] = "--mutated"
	if got := cfg.Runner("worker").ExtraArgs[0]; got != "--a" {
		t.Fatalf("resolution is shared state: got %q", got)
	}
	rev := cfg.Runner("reviewer")
	rev.AllowedTools[0] = "Bash"
	if got := cfg.Runner("reviewer").AllowedTools[0]; got != "Read" {
		t.Fatalf("reviewer tools are shared state: got %q", got)
	}
	rev.DeniedTools[0] = "Bash"
	if got := cfg.Runner("reviewer").DeniedTools[0]; got == "Bash" {
		t.Fatal("reviewer deny rules are shared state: a runner can drop its own")
	}
}

// agent: changed meaning without changing name, so a config written for the
// plugin has to fail loudly and say what the field now accepts.
func TestUnknownAgentRoleFailsWithTheValidRoles(t *testing.T) {
	_, err := Load(write(t, `
runners:
  security:
    model: opus
pipeline:
  - stage: implement
  - stage: review
    agent: some-old-subagent
`))
	if err == nil {
		t.Fatal("a stage naming an undefined role must fail to load")
	}
	msg := err.Error()
	for _, want := range []string{"some-old-subagent", "worker", "reviewer", "integrator", "security"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error should mention %q, got: %v", want, msg)
		}
	}
	if strings.Contains(msg, RoleDefault) {
		t.Fatalf("default is not a dispatchable role and should not be offered: %v", msg)
	}
}

func TestDefinedRolesAreAcceptedAsStageAgents(t *testing.T) {
	bodies := map[string]string{
		"built-in role":    "pipeline:\n  - stage: review\n    agent: reviewer\n",
		"plugin-era alias": "pipeline:\n  - stage: review\n    agent: bd-reviewer\n",
		"custom role from the runners block": "runners:\n  security:\n    model: opus\n" +
			"pipeline:\n  - stage: security\n    agent: security\n",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err != nil {
				t.Fatalf("should load: %v", err)
			}
		})
	}
}

// default is the base every role resolves over, not something a stage can be
// dispatched as.
func TestDefaultIsNotADispatchableRole(t *testing.T) {
	if _, err := Load(write(t, "pipeline:\n  - stage: review\n    agent: default\n")); err == nil {
		t.Fatal("agent: default must be rejected")
	}
	cfg := Default()
	for _, r := range cfg.Roles() {
		if r == RoleDefault {
			t.Fatal("Roles() must not offer default")
		}
	}
}

func TestRolesListsBuiltinsAndCustomRoles(t *testing.T) {
	cfg, err := Load(write(t, "runners:\n  default:\n    model: opus\n  security:\n    model: opus\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"integrator", "reviewer", "security", "worker"}
	if got := cfg.Roles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Roles() = %v, want %v", got, want)
	}
}

func TestRunnersValidation(t *testing.T) {
	cases := map[string]string{
		"unknown permission level": "runners:\n  default:\n    permissions: yolo\n",
		"negative timeout":         "runners:\n  worker:\n    timeout: -1\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatal("expected a validation error, got none")
			}
		})
	}

	// The message has to say what is allowed, not only that this is not.
	_, err := Load(write(t, "runners:\n  default:\n    permissions: yolo\n"))
	if err == nil || !strings.Contains(err.Error(), "scoped, auto, bypass") {
		t.Fatalf("error should list the permission levels, got: %v", err)
	}
}

// max_rounds now exists at two levels, so which one wins is written down here
// rather than discovered in a run.
func TestMaxRoundsPrecedence(t *testing.T) {
	cfg, err := Load(write(t, `
max_rounds: 5
pipeline:
  - stage: implement
  - stage: gate
  - stage: review
    agent: reviewer
  - stage: audit
    agent: worker
    max_rounds: 2
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxRounds != 5 {
		t.Fatalf("run-level max_rounds = %d, want 5", cfg.MaxRounds)
	}

	byStage := map[string]Stage{}
	for _, s := range cfg.Pipeline {
		byStage[s.Stage] = s
	}
	if got := cfg.MaxRoundsFor(byStage["review"]); got != 5 {
		t.Fatalf("a stage with no max_rounds should inherit the run-level 5, got %d", got)
	}
	if got := cfg.MaxRoundsFor(byStage["audit"]); got != 2 {
		t.Fatalf("a stage's own max_rounds must win over the run-level one, got %d", got)
	}
	if got := byStage["audit"].MaxRounds; got != 2 {
		t.Fatalf("loading must not overwrite an explicit stage max_rounds, got %d", got)
	}
	if got := byStage["review"].MaxRounds; got != 5 {
		t.Fatalf("an unset stage max_rounds should resolve to the run-level 5, got %d", got)
	}
}

func TestMaxRoundsDefaults(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxRounds != DefaultMaxRounds {
		t.Fatalf("max_rounds = %d, want %d", cfg.MaxRounds, DefaultMaxRounds)
	}
	if got := cfg.MaxRoundsFor(Stage{Stage: "review", Agent: "reviewer"}); got != DefaultMaxRounds {
		t.Fatalf("MaxRoundsFor = %d, want %d", got, DefaultMaxRounds)
	}
	// Nonsense values fall back rather than disabling recovery entirely.
	zeroed, err := Load(write(t, "max_rounds: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if zeroed.MaxRounds != DefaultMaxRounds {
		t.Fatalf("max_rounds 0 should fall back to %d, got %d", DefaultMaxRounds, zeroed.MaxRounds)
	}
}
