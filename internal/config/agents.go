package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"bd-auto/internal/runner"
	"bd-auto/prompts"
)

// An agent is one file.
//
// .beads-auto/agents/<role>.md is frontmatter — the same fields a runners:
// entry takes — over a body that is the role's system prompt. That is the whole
// definition, which is the point: an agent is something you can read, diff and
// copy into another repo whole, rather than a yaml key in one file and a prompt
// somewhere else.
//
// The files are read once, in the main checkout, at config load, and the text
// is carried in the request. They are never read from a worktree: prompts are
// embedded rather than loaded from disk precisely because a run spawns
// processes in worktrees, where a file may be missing or on another commit, and
// reading agent files there would reintroduce the problem the embed avoids.
const (
	// AgentExt is the extension an agent file has.
	AgentExt = ".md"
	// agentsDirName is where they live, under the repo root.
	agentsDirName = ".beads-auto"
	agentsSubDir  = "agents"
)

// AgentsDir is the agents directory, relative to a repo root.
func AgentsDir() string { return filepath.Join(agentsDirName, agentsSubDir) }

// AgentPath is where a role's agent file lives under root.
func AgentPath(root, role string) string {
	return filepath.Join(root, AgentsDir(), role+AgentExt)
}

// AgentMeta is the frontmatter that is not runner configuration: what a human
// reading the directory needs, and where the file came from.
//
// Source and Version are provenance for a file `bd-auto init` materialised from
// a built-in prompt. They are what makes the trade honest: materialising pins
// the prompt so a bd-auto upgrade cannot change how this repo's runs behave,
// and the cost is that an improved shipped prompt never arrives on its own.
// Recording which built-in this was, and which bd-auto wrote it, is what lets
// `bd-auto agents diff` say so later.
type AgentMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Source is "builtin" for a file materialised from a shipped prompt, and
	// empty for one a human wrote.
	Source string `yaml:"source"`
	// Version is the bd-auto that wrote a materialised file.
	Version string `yaml:"bd_auto_version"`
}

// SourceBuiltin is the Source of a materialised built-in prompt.
const SourceBuiltin = "builtin"

// Agent is one parsed agent file.
type Agent struct {
	// Role is the file's basename without .md, which is the role it defines.
	Role string
	// Path is the file it was read from.
	Path string
	// Spec is the frontmatter's runner configuration. runners: in
	// .beads-auto.yaml wins over it; see Config.Runner.
	Spec RunnerSpec
	// Meta is the rest of the frontmatter.
	Meta AgentMeta
	// Body is the prompt, before splices.
	Body string
}

// agentFront is the whole frontmatter document: a runners: entry inline, plus
// the fields that describe the file rather than the model.
type agentFront struct {
	RunnerSpec `yaml:",inline"`
	AgentMeta  `yaml:",inline"`
}

// ParseAgent reads one agent file.
//
// Frontmatter is optional — a file that is nothing but a prompt is a valid
// agent, and is the shortest way to override a shipped one. What is not
// optional is that what is there parses: an unknown key is a typo that would
// otherwise be silently dropped, and a dropped `model:` is a role running on a
// model nobody chose.
func ParseAgent(role, path string, raw []byte) (*Agent, error) {
	a := &Agent{Role: role, Path: path}
	front, body, ok := splitFrontmatter(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	a.Body = strings.TrimSpace(body)
	if !ok {
		if strings.TrimSpace(front) != "" {
			return nil, fmt.Errorf("frontmatter opens with --- and is never closed")
		}
		if a.Body == "" {
			return nil, fmt.Errorf("the file is empty; its body is the role's system prompt")
		}
		return a, nil
	}
	var f agentFront
	dec := yaml.NewDecoder(strings.NewReader(front))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && err.Error() != "EOF" {
		// yaml names the Go type it could not fill, which is true and no use to
		// somebody reading their own file. Say what the fields are instead.
		msg := strings.ReplaceAll(err.Error(), "in type config.agentFront",
			"in an agent's frontmatter, which takes the fields a runners: entry takes "+
				"(provider, model, permissions, allowed_tools, denied_tools, extra_args, "+
				"timeout, resume) plus name, description, source and bd_auto_version")
		return nil, fmt.Errorf("frontmatter: %s", msg)
	}
	a.Spec, a.Meta = f.RunnerSpec, f.AgentMeta
	if a.Body == "" {
		return nil, fmt.Errorf("the file is frontmatter and no prompt; its body is the role's system prompt")
	}
	return a, nil
}

// splitFrontmatter cuts a leading --- fenced yaml block off the front of a
// file. It reports whether there was one; front is returned even when the fence
// was never closed, so the caller can tell a malformed file from a plain one.
func splitFrontmatter(s string) (front, body string, ok bool) {
	const fence = "---"
	rest, cut := strings.CutPrefix(s, fence+"\n")
	if !cut {
		return "", s, false
	}
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " \t") != fence {
			continue
		}
		return strings.Join(lines[:i], "\n"), strings.Join(lines[i+1:], "\n"), true
	}
	return rest, "", false
}

// loadAgents reads every agent file under repoRoot.
//
// The whole directory is read rather than only the roles the pipeline names,
// because a file is what defines a role: dropping security.md in is enough to
// write `agent: security` in the pipeline, with no runners: entry to go with
// it. A file that does not parse stops the load naming its path, in keeping
// with the rest of this config — the alternative is a prompt nobody reviewed
// running in place of the one they wrote.
func (c *Config) loadAgents(repoRoot string) error {
	c.agents = map[string]*Agent{}
	if repoRoot == "" {
		return nil
	}
	dir := filepath.Join(repoRoot, AgentsDir())
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", dir, err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, AgentExt) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		role := strings.TrimSuffix(name, AgentExt)
		a, err := ParseAgent(role, path, raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		c.agents[role] = a
	}
	return nil
}

// agentPathHint is where a role's agent file would be, for an error that has to
// tell somebody where to put one. It is relative to the config file, so it is
// the path they would type, and falls back to the repo-relative form for a
// config that was never loaded from disk.
func (c *Config) agentPathHint(role string) string {
	if c.path != "" {
		return AgentPath(filepath.Dir(c.path), role)
	}
	return filepath.Join(AgentsDir(), role+AgentExt)
}

// Agent returns the agent file defining a role, or nil where there is none.
func (c *Config) Agent(role string) *Agent { return c.agents[role] }

// Agents lists the roles an agent file defines, sorted.
func (c *Config) Agents() []string {
	out := make([]string, 0, len(c.agents))
	for role := range c.agents {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

// validateAgents checks an agent file's frontmatter for the same things
// validateRunners checks a runners: entry for, reported against the file rather
// than against the yaml the reader did not write it in.
func (c *Config) validateAgents() error {
	for _, role := range c.Agents() {
		a := c.agents[role]
		s := a.Spec
		if s.Provider != "" && !slices.Contains(runner.Providers(), s.Provider) {
			return fmt.Errorf("%s: provider: %q is not a registered runner adapter; known providers are %s",
				a.Path, s.Provider, strings.Join(runner.Providers(), ", "))
		}
		if s.Permissions != "" && !runner.Permissions(s.Permissions).Valid() {
			return fmt.Errorf("%s: permissions: %q is not one of %s", a.Path, s.Permissions, joinPermissions())
		}
		if s.Timeout != nil && *s.Timeout < 0 {
			return fmt.Errorf("%s: timeout: %d is negative; use 0 for unlimited", a.Path, *s.Timeout)
		}
	}
	return nil
}

// --- prompt resolution ---

// PromptOrigin is where a role's system prompt came from.
type PromptOrigin string

const (
	// OriginFile is a prompt read from .beads-auto/agents/<role>.md.
	OriginFile PromptOrigin = "file"
	// OriginBuiltin is a prompt shipped with the binary for a role of this name.
	OriginBuiltin PromptOrigin = "builtin"
	// OriginReviewer is the fallback: a role with no prompt of its own runs the
	// reviewer's, because a custom stage judges a diff.
	OriginReviewer PromptOrigin = "reviewer"
)

// PromptSource is the answer to "where did this role's prompt come from", which
// used to have no answer at all: a role with no prompt of its own silently got
// the reviewer's, so a repo could not tell a configured agent from an
// accidental one.
type PromptSource struct {
	Role   string       `json:"role"`
	Origin PromptOrigin `json:"origin"`
	// Path is the agent file, for OriginFile. Empty otherwise.
	Path string `json:"path,omitempty"`
	// Judging reports that a pipeline stage dispatches this role to judge a
	// diff, which is what decides whether it carries the verdict contract.
	Judging bool `json:"judging"`
}

// String is the one-line form used by `config show` and the run log.
func (s PromptSource) String() string {
	switch s.Origin {
	case OriginFile:
		return s.Path
	case OriginBuiltin:
		return string(OriginBuiltin)
	default:
		return "reviewer (no prompt of its own)"
	}
}

// PromptSource reports where a role's prompt comes from, without resolving it.
func (c *Config) PromptSource(role string) PromptSource {
	out := PromptSource{Role: role, Judging: c.Judging(role)}
	if a := c.agents[role]; a != nil {
		out.Origin, out.Path = OriginFile, a.Path
		return out
	}
	if _, err := prompts.For(role); err == nil {
		out.Origin = OriginBuiltin
		return out
	}
	out.Origin = OriginReviewer
	return out
}

// PromptSources reports the source of every role this configuration dispatches:
// the roles the pipeline names, the roles its hooks name, plus the two the
// engine drives itself.
//
// A hook's role belongs here for the reason the fallback exists at all: a role
// with no prompt of its own is handed the reviewer's, and a hook handed the
// reviewer's prompt is an interpreter that has been told to judge a diff and
// return a verdict nobody reads. Listing it is what makes that visible in
// `config show` and in the line a run logs before it starts.
func (c *Config) PromptSources() []PromptSource {
	seen := map[string]bool{}
	var roles []string
	add := func(role string) {
		if role == "" || seen[role] {
			return
		}
		seen[role] = true
		roles = append(roles, role)
	}
	add(string(runner.RoleWorker))
	for _, s := range c.Pipeline {
		add(s.Agent)
	}
	add(string(runner.RoleIntegrator))
	for _, role := range c.HookRoles() {
		add(role)
	}

	out := make([]PromptSource, 0, len(roles))
	for _, role := range roles {
		out = append(out, c.PromptSource(role))
	}
	return out
}

// Judging reports whether a pipeline stage dispatches this role to judge a
// diff. Only an agent: stage does: the implement stage is the work itself, and
// the gate is a list of commands.
func (c *Config) Judging(role string) bool {
	if role == "" {
		return false
	}
	for _, s := range c.Pipeline {
		if s.Kind() == "agent" && s.Agent == role {
			return true
		}
	}
	return false
}

// RolePrompt resolves a role's system prompt, splices and all.
//
// Resolution, in order: the repo's own .beads-auto/agents/<role>.md, else the
// prompt shipped for a role of that name, else the reviewer's. Then the splices
// are substituted, and a judging role that placed no {{VERDICT}} has the
// contract appended — because ParseVerdict fails closed on a missing verdict
// line, so a judging prompt that forgot to state the contract fails every issue
// it sees and looks like a strict reviewer while doing it.
func (c *Config) RolePrompt(role string) string {
	src := c.PromptSource(role)
	base := ""
	switch src.Origin {
	case OriginFile:
		base = c.agents[role].Body
	case OriginBuiltin:
		base, _ = prompts.For(role)
	default:
		base, _ = prompts.For(string(runner.RoleReviewer))
	}

	// {{BUILTIN}} is expanded first and on its own, because what it brings in
	// has splices of its own: the shipped reviewer places {{VERDICT}}, so a
	// repo whose file is {{BUILTIN}} plus its own additions has to end up with
	// the contract where the built-in put it, once, rather than with a literal
	// token and a second copy appended.
	expanded := base
	if src.Origin == OriginFile {
		// A name with no built-in splices nothing, which is not an error.
		builtin, _ := prompts.For(role)
		expanded = strings.ReplaceAll(base, prompts.TokenBuiltin, strings.TrimRight(builtin, "\n"))
	}

	sp := prompts.Splices{Graph: c.graphSection()}
	if src.Judging {
		sp.Verdict = prompts.VerdictContract()
	}
	out := prompts.Splice(expanded, sp)
	if src.Judging && !prompts.HasToken(expanded, prompts.TokenVerdict) {
		out = strings.TrimRight(out, "\n") + "\n\n" + prompts.VerdictContract() + "\n"
	}
	return out
}

// graphSection is the {{GRAPH}} splice: the code-index section, and empty where
// there is no index.
//
// It is empty for every repo today — building the index and attaching it is
// beads-auto-imp-1xg — and it is here rather than left out because the
// substitution is the contract: an agent file that places {{GRAPH}} keeps
// working when the index arrives, and yields nothing until it does.
func (c *Config) graphSection() string { return "" }

// --- materialising the built-ins ---

// AgentWrite is what WriteAgents did about one role.
type AgentWrite struct {
	Role    string `json:"role"`
	Path    string `json:"path"`
	Written bool   `json:"written"`
	Reason  string `json:"reason,omitempty"`
}

// BuiltinAgentFile is the file `bd-auto init` writes for a shipped role: the
// built-in prompt verbatim, under frontmatter that records where it came from.
//
// Verbatim matters. The file is the repo's own copy from the moment it is
// written, so what a run did is readable in that repo's git history, and a
// bd-auto upgrade that rewrites a shipped prompt cannot change how this repo's
// runs behave. The header says what to do about the other side of that trade.
func BuiltinAgentFile(role, version string) ([]byte, error) {
	body, err := prompts.For(role)
	if err != nil {
		return nil, err
	}
	if version == "" {
		version = "unknown"
	}
	header := fmt.Sprintf(`---
# The built-in %s prompt, written here by `+"`bd-auto init`"+` so this repo's runs
# are traceable in this repo's history. Edit it: it is yours now.
#
# Because it is a copy, an improved shipped prompt will not arrive on its own.
# `+"`bd-auto agents diff %s`"+` shows what the shipped one has done since, and
# `+"`bd-auto agents update %s`"+` takes it. To track upstream instead, replace the
# body below with {{BUILTIN}} and your own additions underneath it.
#
# Frontmatter takes the fields a runners: entry takes — model, permissions,
# allowed_tools, denied_tools, extra_args, timeout, resume — and .beads-auto.yaml
# wins over anything set here. The body is the system prompt, and may place
# {{BUILTIN}}, {{GRAPH}} and {{VERDICT}}.
source: %s
bd_auto_version: %s
---

`, role, role, role, SourceBuiltin, version)
	return []byte(header + strings.TrimRight(body, "\n") + "\n"), nil
}

// WriteAgents materialises the built-in agents into dir/.beads-auto/agents.
//
// It never clobbers without force, the same discipline Write has for the config
// file: a prompt somebody tuned by hand is not something to overwrite on a
// guess. Roles that already exist are reported as skipped rather than failing
// the whole call, so `init` in a repo that has some of them writes the rest.
func WriteAgents(dir, version string, force bool) ([]AgentWrite, error) {
	target := filepath.Join(dir, AgentsDir())
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", target, err)
	}
	var out []AgentWrite
	for _, role := range prompts.Roles() {
		w, err := writeAgent(target, role, version, force)
		if err != nil {
			return out, err
		}
		out = append(out, w)
	}
	return out, nil
}

func writeAgent(target, role, version string, force bool) (AgentWrite, error) {
	p := filepath.Join(target, role+AgentExt)
	w := AgentWrite{Role: role, Path: p}
	if !force {
		if _, err := os.Stat(p); err == nil {
			w.Reason = "already exists"
			return w, nil
		} else if !os.IsNotExist(err) {
			return w, fmt.Errorf("stat %s: %w", p, err)
		}
	}
	content, err := BuiltinAgentFile(role, version)
	if err != nil {
		return w, err
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		return w, fmt.Errorf("write %s: %w", p, err)
	}
	w.Written = true
	return w, nil
}

// Drifted reports whether a materialised agent's prompt no longer matches the
// built-in it was written from.
//
// Only a materialised one can drift: a file a human wrote was never a copy of
// anything, and calling it "drifted" would be telling them their own prompt is
// out of date. A role with no built-in cannot drift either.
func (a *Agent) Drifted() bool {
	if a == nil || a.Meta.Source != SourceBuiltin {
		return false
	}
	b, err := prompts.For(a.Role)
	if err != nil {
		return false
	}
	return strings.TrimSpace(b) != strings.TrimSpace(a.Body)
}
