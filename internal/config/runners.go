package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"bd-auto/internal/runner"
	"gopkg.in/yaml.v3"

	// The registry is only as complete as what the binary imports, so a
	// provider: check against it is only as good as this line: without it
	// validateRunners would reject `provider: claude` in a binary that ships
	// the adapter. Importing it here rather than leaving it to each command
	// keeps the two in step, because the check and the import now live in the
	// same package.
	_ "bd-auto/internal/runner/providers"
)

// RoleDefault is the runners: entry every other role resolves over. It is not
// itself a role anything can be dispatched as.
const RoleDefault = "default"

// Built-in runner defaults. A repo with no runners: block gets these.
const (
	DefaultProvider      = "claude"
	DefaultModel         = "opus"
	DefaultReviewerModel = "sonnet"
	CodexProvider        = "codex"
	DefaultCodexModel    = "gpt-5.6-sol"
	DefaultCodexReviewer = "gpt-5.6-terra"
)

// ClaudeRunnerConfig contains the Claude-native controls. The flat fields on
// RunnerSpec are their deprecated compatibility aliases.
type ClaudeRunnerConfig struct {
	Permissions  string   `yaml:"permissions"`
	AllowedTools []string `yaml:"allowed_tools"`
	DeniedTools  []string `yaml:"denied_tools"`
}

func (s *ClaudeRunnerConfig) merge(over *ClaudeRunnerConfig) *ClaudeRunnerConfig {
	if s == nil && over == nil {
		return nil
	}
	out := ClaudeRunnerConfig{}
	if s != nil {
		out = *s
		out.AllowedTools = append([]string(nil), s.AllowedTools...)
		out.DeniedTools = append([]string(nil), s.DeniedTools...)
	}
	if over == nil {
		return &out
	}
	if over.Permissions != "" {
		out.Permissions = over.Permissions
	}
	if over.AllowedTools != nil {
		out.AllowedTools = append([]string(nil), over.AllowedTools...)
	}
	if over.DeniedTools != nil {
		out.DeniedTools = append([]string(nil), over.DeniedTools...)
	}
	return &out
}

// CodexTools is deliberately a closed set. It is not a translation of Claude
// tool names: these switches are the native controls the Codex adapter reads.
type CodexTools struct {
	Shell     *bool `yaml:"shell"`
	WebSearch *bool `yaml:"web_search"`
	ViewImage *bool `yaml:"view_image"`
}

func (t *CodexTools) UnmarshalYAML(value *yaml.Node) error {
	var raw map[string]*bool
	if err := value.Decode(&raw); err != nil {
		return err
	}
	for key := range raw {
		switch key {
		case "shell", "web_search", "view_image":
		default:
			return fmt.Errorf("unknown Codex tool %q; valid tools are shell, web_search, view_image", key)
		}
	}
	t.Shell, t.WebSearch, t.ViewImage = raw["shell"], raw["web_search"], raw["view_image"]
	return nil
}

func (t CodexTools) merge(over CodexTools) CodexTools {
	out := t
	if over.Shell != nil {
		out.Shell = over.Shell
	}
	if over.WebSearch != nil {
		out.WebSearch = over.WebSearch
	}
	if over.ViewImage != nil {
		out.ViewImage = over.ViewImage
	}
	return out
}

// CodexRunnerConfig contains Codex-native controls. All values are optional so
// role entries can resolve over runners.default just like common fields do.
type CodexRunnerConfig struct {
	Sandbox        string `yaml:"sandbox"`
	ApprovalPolicy string `yaml:"approval_policy"`
	// AddDirs widens only the Codex workspace-write sandbox. Nil inherits and
	// an empty list explicitly removes inherited paths.
	AddDirs []string   `yaml:"add_dirs"`
	Tools   CodexTools `yaml:"tools"`
}

func (s *CodexRunnerConfig) merge(over *CodexRunnerConfig) *CodexRunnerConfig {
	if s == nil && over == nil {
		return nil
	}
	out := CodexRunnerConfig{}
	if s != nil {
		out = *s
		out.AddDirs = append([]string(nil), s.AddDirs...)
	}
	if over == nil {
		return &out
	}
	if over.Sandbox != "" {
		out.Sandbox = over.Sandbox
	}
	if over.ApprovalPolicy != "" {
		out.ApprovalPolicy = over.ApprovalPolicy
	}
	if over.AddDirs != nil {
		out.AddDirs = append([]string(nil), over.AddDirs...)
	}
	out.Tools = out.Tools.merge(over.Tools)
	return &out
}

// DefaultReviewerTools scopes the reviewer to repository file reads. The
// engine supplies the full issue record and candidate patch, so review does not
// need access to Git metadata or the Beads database.
func DefaultReviewerTools() []string {
	return []string{"Read", "Grep", "Glob"}
}

// DefaultReviewerDenied is every bd verb that writes the record, denied to the
// reviewer by name.
//
// The allowlist above permits no shell commands, so under scoped this list
// changes nothing. It exists for the level it is not: deny rules are
// checked ahead of the permission mode, so they are the only part of a
// reviewer's scoping that survives permissions: bypass and
// --dangerously-skip-permissions.
//
// It is a backstop and not a proof. Rules match a command by its prefix, so
// `bd -C <dir> close` is not this list's `bd close`; what actually holds a
// scoped reviewer to reading is the allowlist. What this holds is the widened
// reviewer, against the mistake that has already happened once: a review that
// ran bd close on the issue under review, and overwrote a finished task's close
// reason with "review only, not closing" (beads-auto-imp-46o, since put back).
//
// Verbs, not flags: bd's own --readonly would be a cleaner guard, but it is the
// caller who passes it, and the caller here is the model being guarded.
func DefaultReviewerDenied() []string {
	return []string{
		"Bash(bd assign:*)",
		"Bash(bd batch:*)",
		"Bash(bd close:*)",
		"Bash(bd comment:*)",
		"Bash(bd create:*)",
		"Bash(bd defer:*)",
		"Bash(bd delete:*)",
		"Bash(bd dep:*)",
		"Bash(bd dolt:*)",
		"Bash(bd edit:*)",
		"Bash(bd import:*)",
		"Bash(bd label:*)",
		"Bash(bd link:*)",
		"Bash(bd note:*)",
		"Bash(bd priority:*)",
		"Bash(bd q:*)",
		"Bash(bd remember:*)",
		"Bash(bd rename:*)",
		"Bash(bd reopen:*)",
		"Bash(bd set-state:*)",
		"Bash(bd sql:*)",
		"Bash(bd supersede:*)",
		"Bash(bd sync:*)",
		"Bash(bd tag:*)",
		"Bash(bd undefer:*)",
		"Bash(bd update:*)",
	}
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
	// DeniedTools names tools the role may not use at any permission level. A
	// nil list inherits; an empty list overrides with nothing.
	DeniedTools []string `yaml:"denied_tools"`
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
	// Claude is the native Claude configuration. Permissions and tools above
	// remain supported only as deprecated Claude compatibility aliases.
	Claude *ClaudeRunnerConfig `yaml:"claude"`
	// Codex is the native Codex configuration.
	Codex *CodexRunnerConfig `yaml:"codex"`
}

// merge returns s with every field over sets applied on top.
func (s RunnerSpec) merge(over RunnerSpec) RunnerSpec {
	out := s
	if over.Provider != "" {
		if out.Provider != "" && over.Provider != out.Provider &&
			(out.Provider == CodexProvider || over.Provider == CodexProvider) {
			// Provider-native settings never cross an adapter boundary. The model
			// also belongs to the backend even though it is a common field, so let
			// the newly selected provider supply its own role default unless this
			// same override names one below.
			out.Model = ""
			out.Permissions = ""
			out.AllowedTools = nil
			out.DeniedTools = nil
			out.Claude = nil
			out.Codex = nil
		}
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
	if over.DeniedTools != nil {
		out.DeniedTools = over.DeniedTools
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
	out.Claude = out.Claude.merge(over.Claude)
	out.Codex = out.Codex.merge(over.Codex)
	return out
}

// resolved converts a fully merged spec into what the engine consumes. Slices
// are copied so a caller cannot rewrite a role's configuration through the
// spec it was handed.
func (s RunnerSpec) resolved(role string) runner.Spec {
	out := runner.Spec{
		Provider:         s.Provider,
		Model:            s.Model,
		ExtraArgs:        append([]string(nil), s.ExtraArgs...),
		BillingSensitive: s.Provider == CodexProvider,
	}
	if s.Provider == CodexProvider && out.Model == "" {
		out.Model = DefaultCodexModel
		if role == string(runner.RoleReviewer) {
			out.Model = DefaultCodexReviewer
		}
	}
	if s.Provider == DefaultProvider && out.Model == "" {
		out.Model = DefaultModel
		if role == string(runner.RoleReviewer) {
			out.Model = DefaultReviewerModel
		}
	}
	if s.Provider != CodexProvider {
		if s.Claude != nil {
			if s.Claude.Permissions != "" {
				out.Permissions = runner.Permissions(s.Claude.Permissions)
			}
			if s.Claude.AllowedTools != nil {
				out.AllowedTools = append([]string(nil), s.Claude.AllowedTools...)
			}
			if s.Claude.DeniedTools != nil {
				out.DeniedTools = append([]string(nil), s.Claude.DeniedTools...)
			}
		}
		// Compatibility aliases deliberately win after the new block. They are
		// rejected when both forms set the same user setting, but this ordering
		// lets an existing flat value override a built-in nested Claude default.
		if s.Permissions != "" {
			out.Permissions = runner.Permissions(s.Permissions)
		}
		if s.AllowedTools != nil {
			out.AllowedTools = append([]string(nil), s.AllowedTools...)
		}
		if s.DeniedTools != nil {
			out.DeniedTools = append([]string(nil), s.DeniedTools...)
		}
		if s.Provider == DefaultProvider && s.Claude == nil && s.Permissions == "" &&
			s.AllowedTools == nil && s.DeniedTools == nil {
			out.Permissions = runner.PermAuto
			if role == string(runner.RoleReviewer) {
				out.Permissions = runner.PermScoped
				out.AllowedTools = DefaultReviewerTools()
				out.DeniedTools = DefaultReviewerDenied()
			}
		}
	}
	if s.Provider == CodexProvider && s.Codex != nil {
		out.Sandbox = s.Codex.Sandbox
		out.ApprovalPolicy = s.Codex.ApprovalPolicy
		out.AddDirs = append([]string(nil), s.Codex.AddDirs...)
		if s.Codex.Tools.Shell != nil {
			out.Shell = *s.Codex.Tools.Shell
		}
		if s.Codex.Tools.WebSearch != nil {
			out.WebSearch = *s.Codex.Tools.WebSearch
		}
		if s.Codex.Tools.ViewImage != nil {
			out.ViewImage = *s.Codex.Tools.ViewImage
		}
	}
	if s.Provider == CodexProvider {
		if out.Sandbox == "" {
			out.Sandbox = "workspace-write"
			if role == string(runner.RoleReviewer) {
				out.Sandbox = "read-only"
			}
		}
		if out.ApprovalPolicy == "" {
			out.ApprovalPolicy = "never"
		}
		if s.Codex == nil || s.Codex.Tools.Shell == nil {
			out.Shell = true
		}
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
// The default is auto, and it is opt-in to widen: nothing bd-auto ships turns
// permission checks off by itself. Measured against claude 2.1.233 in this
// repo's own worker worktrees, that has a cost worth knowing before a run —
// headless there is nobody to answer a prompt, so under auto a worker is
// refused every Write and every Bash, and under acceptEdits it gets its edits
// but is still refused the plain shell the gate, git and bd all need. Only
// bypass lets a worker finish, and reaching it is a decision a human makes,
// per repo under runners: or per run with --dangerously-skip-permissions.
//
// A run that hits the refusal is not left to guess: a round that was refused a
// tool and then changed nothing stops the run as an environment failure naming
// both the tools and the flag, rather than parking the issue as failed work.
// See deniedReason in internal/drain.
func builtinRunners() map[string]RunnerSpec {
	return map[string]RunnerSpec{
		RoleDefault: {
			Provider: DefaultProvider,
			Model:    DefaultModel,
			Claude:   &ClaudeRunnerConfig{Permissions: string(runner.PermAuto)},
			Timeout:  intPtr(0),
			Resume:   boolPtr(true),
		},
		string(runner.RoleReviewer): {
			Model: DefaultReviewerModel,
			Claude: &ClaudeRunnerConfig{
				Permissions:  string(runner.PermScoped),
				AllowedTools: DefaultReviewerTools(),
				DeniedTools:  DefaultReviewerDenied(),
			},
			Resume: boolPtr(false),
		},
	}
}

// RoleDefined reports whether name is a runner role this config knows about: a
// built-in role, one the runners: block defines, or one an agent file defines.
//
// An agent file counts on its own. .beads-auto/agents/security.md is the whole
// definition of a security agent — its prompt and its defaults in one file — so
// requiring an empty runners: entry beside it would be asking for the same fact
// twice.
//
// There is no aliasing left here. A role is called what the config calls it,
// and a name that is not defined is reported as such at load rather than
// resolved into something else.
func (c *Config) RoleDefined(name string) bool {
	if name == RoleDefault {
		return false
	}
	if _, ok := c.Runners[name]; ok {
		return true
	}
	if _, ok := c.agents[name]; ok {
		return true
	}
	for _, r := range runner.BuiltinRoles() {
		if name == string(r) {
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
	for name := range c.agents {
		add(name)
	}
	sort.Strings(out)
	return out
}

// Runner resolves the runner configuration for a role.
//
// Precedence, weakest first: the built-in default, the built-in specialisation
// for this role, the role's own agent file, the config's runners.default, the
// config's runners.<role>. Anything you set on default therefore beats a
// built-in role default — to keep the reviewer on a cheap model while moving
// everything else, set it on the reviewer.
//
// The agent file sits under both yaml keys on purpose: the file carries the
// agent's own defaults, so it can be copied between repos and still be the
// thing it was, and .beads-auto.yaml is this run's configuration surface, so a
// repo can retune a shared agent without editing it.
func (c *Config) Runner(role string) runner.Spec {
	builtin := builtinRunners()

	spec := builtin[RoleDefault]
	if role != RoleDefault {
		if b, ok := builtin[role]; ok {
			spec = spec.merge(b)
		}
		if a, ok := c.agents[role]; ok {
			spec = spec.merge(a.Spec)
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
	out := spec.resolved(role)
	// Last, and over everything, including a role that named its own level.
	// --dangerously-skip-permissions is the answer to a run that is stuck on
	// permissions, and a flag that quietly left one role behind would not be one.
	if out.Provider != CodexProvider && c.ForcePermissions != "" {
		out.Permissions = c.ForcePermissions
	}
	return out
}

// validateRunners checks the runners: block for values that would only fail
// once a model was about to be spawned.
//
// provider: is checked against the registry for the same reason the rest of
// this is checked at all: a typo there loads fine and only surfaces from
// runner.New, which the engine reaches after it has already resolved a scope,
// cut worktrees and dispatched a wave. Reading it back one line into the run
// costs nothing; reading it back once five workers are in flight costs the
// wave.
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
		// An empty provider inherits, and what it inherits from was itself
		// checked here — either another runners: entry or the built-in
		// default, which is a registered name by construction.
		if s.Provider != "" && !knownProvider(s.Provider) {
			return fmt.Errorf("runners.%s: provider: %q is not a registered runner adapter; known providers are %s",
				name, s.Provider, strings.Join(knownProviders(), ", "))
		}
		if s.Timeout != nil && *s.Timeout < 0 {
			return fmt.Errorf("runners.%s: timeout: %d is negative; use 0 for unlimited", name, *s.Timeout)
		}
	}
	return c.validateProviderSettings()
}

// knownProviders returns the adapters linked into the shipped binary.
func knownProviders() []string {
	out := append([]string(nil), runner.Providers()...)
	return out
}

func knownProvider(name string) bool { return slices.Contains(knownProviders(), name) }

func (c *Config) validateProviderSettings() error {
	roles := append([]string{RoleDefault}, c.Roles()...)
	for _, role := range roles {
		s := c.userRunnerSpec(role)
		resolved := c.Runner(role)
		if err := validateProviderSpec("runners."+role, s, resolved.Provider); err != nil {
			return err
		}
		if resolved.Provider == CodexProvider && len(resolved.AddDirs) > 0 && resolved.Sandbox != "workspace-write" {
			return fmt.Errorf("runners.%s.codex: add_dirs is valid only with sandbox: workspace-write (resolved sandbox is %q)", role, resolved.Sandbox)
		}
	}
	return nil
}

// userRunnerSpec is the part of a resolved role that came from a config or
// agent file, not the built-in Claude defaults. That distinction matters when
// a repo switches default provider: it must not inherit Claude settings merely
// because the built-in Claude profile supplied them before the switch.
func (c *Config) userRunnerSpec(role string) RunnerSpec {
	s := c.Runners[RoleDefault]
	if role != RoleDefault {
		if a := c.agents[role]; a != nil {
			s = s.merge(a.Spec)
		}
		s = s.merge(c.Runners[role])
	}
	return s
}

func validateProviderSpec(path string, s RunnerSpec, provider string) error {
	if s.Timeout != nil && *s.Timeout < 0 {
		return fmt.Errorf("%s: timeout: %d is negative; use 0 for unlimited", path, *s.Timeout)
	}
	switch provider {
	default:
		if s.Permissions != "" && !runner.Permissions(s.Permissions).Valid() {
			return fmt.Errorf("%s: permissions: %q is not one of %s", path, s.Permissions, joinPermissions())
		}
		if s.Claude != nil {
			if s.Claude.Permissions != "" && !runner.Permissions(s.Claude.Permissions).Valid() {
				return fmt.Errorf("%s.claude: permissions: %q is not one of %s", path, s.Claude.Permissions, joinPermissions())
			}
			if s.Permissions != "" && s.Claude.Permissions != "" {
				return fmt.Errorf("%s: permissions is a deprecated Claude alias and cannot be set with claude.permissions", path)
			}
			if s.AllowedTools != nil && s.Claude.AllowedTools != nil {
				return fmt.Errorf("%s: allowed_tools is a deprecated Claude alias and cannot be set with claude.allowed_tools", path)
			}
			if s.DeniedTools != nil && s.Claude.DeniedTools != nil {
				return fmt.Errorf("%s: denied_tools is a deprecated Claude alias and cannot be set with claude.denied_tools", path)
			}
		}
	case CodexProvider:
		if s.Permissions != "" || s.AllowedTools != nil || s.DeniedTools != nil || s.Claude != nil {
			return fmt.Errorf("%s: Claude-only permissions and tool settings cannot be used with provider codex; use codex.sandbox, codex.approval_policy, and codex.tools", path)
		}
		if s.Codex != nil {
			if s.Codex.Sandbox != "" && !slices.Contains([]string{"read-only", "workspace-write", "danger-full-access"}, s.Codex.Sandbox) {
				return fmt.Errorf("%s.codex: sandbox: %q is not one of read-only, workspace-write, danger-full-access", path, s.Codex.Sandbox)
			}
			if s.Codex.ApprovalPolicy != "" && !slices.Contains([]string{"untrusted", "on-failure", "on-request", "never"}, s.Codex.ApprovalPolicy) {
				return fmt.Errorf("%s.codex: approval_policy: %q is not one of untrusted, on-failure, on-request, never", path, s.Codex.ApprovalPolicy)
			}
			for i, dir := range s.Codex.AddDirs {
				switch {
				case strings.ContainsRune(dir, 0):
					return fmt.Errorf("%s.codex: add_dirs[%d] contains a NUL byte", path, i)
				case strings.TrimSpace(dir) == "":
					return fmt.Errorf("%s.codex: add_dirs[%d] is empty", path, i)
				case !filepath.IsAbs(dir):
					return fmt.Errorf("%s.codex: add_dirs[%d] %q is not an absolute path", path, i, dir)
				}
			}
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
