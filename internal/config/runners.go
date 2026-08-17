package config

import (
	"fmt"
	"sort"
	"time"

	"bd-auto/internal/runner"
)

// RoleDefault is the runners: entry every other role resolves over. It is not
// itself a role anything can be dispatched as.
const RoleDefault = "default"

// Built-in runner defaults. A repo with no runners: block gets these.
const (
	DefaultProvider      = "claude"
	DefaultModel         = "opus"
	DefaultReviewerModel = "sonnet"
)

// DefaultReviewerTools scopes the reviewer to the three things a reviewer
// actually runs. The list matters: a bare Bash entry here is a reviewer that
// can push.
func DefaultReviewerTools() []string {
	return []string{"Read", "Grep", "Glob", "Bash(git diff:*)", "Bash(git log:*)", "Bash(bd show:*)"}
}

// roleAliases maps the plugin-era subagent names onto the roles they always
// meant, so a config written before the engine existed keeps loading. The
// subagent definitions themselves are gone; these three strings survive them
// purely as a compatibility shim, and only a deliberate breaking change should
// remove them.
var roleAliases = map[string]string{
	"bd-worker":     string(runner.RoleWorker),
	"bd-reviewer":   string(runner.RoleReviewer),
	"bd-integrator": string(runner.RoleIntegrator),
}

// RunnerSpec is one entry of the runners: block, exactly as written.
//
// Every field is optional and an unset field inherits from runners.default.
// Pointers and nil slices are how "unset" is told apart from "set to the zero
// value" — which matters most for timeout, where 0 is a meaningful setting
// (unlimited) rather than an absence, and for resume, where false is.
type RunnerSpec struct {
	// Provider selects the runner adapter by its registered name.
	Provider string `yaml:"provider"`
	// Model is passed to the backend unchanged.
	Model string `yaml:"model"`
	// Permissions is one of scoped, auto, bypass.
	Permissions string `yaml:"permissions"`
	// AllowedTools limits the role's tools under scoped permissions. A nil
	// list inherits; an empty list overrides with nothing.
	AllowedTools []string `yaml:"allowed_tools"`
	// ExtraArgs is the per-backend escape hatch for flags this config
	// deliberately does not model.
	ExtraArgs []string `yaml:"extra_args"`
	// Timeout bounds one model invocation, in seconds. 0 means unlimited, and
	// unlimited is the default: what bounds a run is the set of issues a human
	// picked, not a clock.
	Timeout *int `yaml:"timeout"`
	// Resume is whether this role's feedback rounds continue the same session
	// rather than starting fresh.
	Resume *bool `yaml:"resume"`
}

// merge returns s with every field over sets applied on top.
func (s RunnerSpec) merge(over RunnerSpec) RunnerSpec {
	out := s
	if over.Provider != "" {
		out.Provider = over.Provider
	}
	if over.Model != "" {
		out.Model = over.Model
	}
	if over.Permissions != "" {
		out.Permissions = over.Permissions
	}
	if over.AllowedTools != nil {
		out.AllowedTools = over.AllowedTools
	}
	if over.ExtraArgs != nil {
		out.ExtraArgs = over.ExtraArgs
	}
	if over.Timeout != nil {
		out.Timeout = over.Timeout
	}
	if over.Resume != nil {
		out.Resume = over.Resume
	}
	return out
}

// resolved converts a fully merged spec into what the engine consumes. Slices
// are copied so a caller cannot rewrite a role's configuration through the
// spec it was handed.
func (s RunnerSpec) resolved() runner.Spec {
	out := runner.Spec{
		Provider:     s.Provider,
		Model:        s.Model,
		Permissions:  runner.Permissions(s.Permissions),
		AllowedTools: append([]string(nil), s.AllowedTools...),
		ExtraArgs:    append([]string(nil), s.ExtraArgs...),
	}
	if s.Timeout != nil && *s.Timeout > 0 {
		out.Timeout = time.Duration(*s.Timeout) * time.Second
	}
	if s.Resume != nil {
		out.Resume = *s.Resume
	}
	return out
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// builtinRunners are the shipped role defaults. Only the reviewer differs from
// the default entry, and it differs on purpose: it judges a diff, so it runs
// cheaper, read-only, and fresh — a resumed reviewer carries its own previous
// verdict and checks whether its findings were addressed instead of re-judging
// the diff.
//
// The default is bypass, and that is a deliberate choice rather than a
// convenience. Measured against claude 2.1.233 in this repo's own worker
// worktrees: under auto a headless worker is refused every Write and every
// Bash, under acceptEdits it gets its edits but is still refused the plain
// shell the gate, git and bd all need, and only under bypass can it do the job
// it was given. A default that asks a human is not a safer default when there
// is no human — it is a default that burns a whole attempt at full price and
// reports it as a model that did nothing.
//
// What contains a worker is structural rather than a prompt, which is what
// makes this defensible: it runs in a throwaway git worktree under .beads/auto/,
// on its own branch, behind gitguard's hooks that refuse push, merge and
// rebase, over a scope a human selected and confirmed before anything was
// spawned. The reviewer stays scoped because none of that applies to it: it
// only reads, and a reviewer that can run bare Bash is a reviewer that can push.
func builtinRunners() map[string]RunnerSpec {
	return map[string]RunnerSpec{
		RoleDefault: {
			Provider:    DefaultProvider,
			Model:       DefaultModel,
			Permissions: string(runner.PermBypass),
			Timeout:     intPtr(0),
			Resume:      boolPtr(true),
		},
		string(runner.RoleReviewer): {
			Model:        DefaultReviewerModel,
			Permissions:  string(runner.PermScoped),
			AllowedTools: DefaultReviewerTools(),
			Resume:       boolPtr(false),
		},
	}
}

// Role canonicalises the name a stage's agent: field uses. A name defined in
// the config always wins; otherwise a plugin-era subagent name resolves to the
// role it meant.
func (c *Config) Role(name string) string {
	if _, ok := c.Runners[name]; ok {
		return name
	}
	if canon, ok := roleAliases[name]; ok {
		return canon
	}
	return name
}

// RoleDefined reports whether name resolves to a runner role this config knows
// about: a built-in role, or one the runners: block defines.
func (c *Config) RoleDefined(name string) bool {
	role := c.Role(name)
	if role == RoleDefault {
		return false
	}
	if _, ok := c.Runners[role]; ok {
		return true
	}
	for _, r := range runner.BuiltinRoles() {
		if role == string(r) {
			return true
		}
	}
	return false
}

// Roles lists every role that can be dispatched, sorted. This is what an
// unknown agent: value is reported against, so it is the answer to "what am I
// allowed to write there".
func (c *Config) Roles() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == RoleDefault || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, r := range runner.BuiltinRoles() {
		add(string(r))
	}
	for name := range c.Runners {
		add(name)
	}
	sort.Strings(out)
	return out
}

// Runner resolves the runner configuration for a role.
//
// Precedence, weakest first: the built-in default, the built-in specialisation
// for this role, the config's runners.default, the config's runners.<role>.
// Anything you set on default therefore beats a built-in role default — to
// keep the reviewer on a cheap model while moving everything else, set it on
// the reviewer.
func (c *Config) Runner(role string) runner.Spec {
	role = c.Role(role)
	builtin := builtinRunners()

	spec := builtin[RoleDefault]
	if role != RoleDefault {
		if b, ok := builtin[role]; ok {
			spec = spec.merge(b)
		}
	}
	if u, ok := c.Runners[RoleDefault]; ok {
		spec = spec.merge(u)
	}
	if role != RoleDefault {
		if u, ok := c.Runners[role]; ok {
			spec = spec.merge(u)
		}
	}
	out := spec.resolved()
	// Last, and over everything, including a role that named its own level.
	// --dangerously-skip-permissions is the answer to a run that is stuck on
	// permissions, and a flag that quietly left one role behind would not be one.
	if c.ForcePermissions != "" {
		out.Permissions = c.ForcePermissions
	}
	return out
}

// validateRunners checks the runners: block for values that would only fail
// once a model was about to be spawned.
func (c *Config) validateRunners() error {
	names := make([]string, 0, len(c.Runners))
	for name := range c.Runners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" {
			return fmt.Errorf("runners: a role name is required")
		}
		s := c.Runners[name]
		if s.Permissions != "" && !runner.Permissions(s.Permissions).Valid() {
			return fmt.Errorf("runners.%s: permissions: %q is not one of %s",
				name, s.Permissions, joinPermissions())
		}
		if s.Timeout != nil && *s.Timeout < 0 {
			return fmt.Errorf("runners.%s: timeout: %d is negative; use 0 for unlimited", name, *s.Timeout)
		}
	}
	return nil
}

func joinPermissions() string {
	var out string
	for i, p := range runner.AllPermissions() {
		if i > 0 {
			out += ", "
		}
		out += string(p)
	}
	return out
}
