package cmds

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bd-auto/internal/config"
	"bd-auto/prompts"
)

// Version is the binary's version, set by main. It is recorded in the
// frontmatter of every agent file bd-auto materialises, so a file can say which
// bd-auto's prompt it is a copy of.
var Version = "dev"

// Agents implements `bd-auto agents <list|show|diff|update>`.
//
// An agent is one file — .beads-auto/agents/<role>.md, frontmatter over a
// prompt — and this command is what makes a directory of them legible: what
// each role actually resolves to, what the shipped prompt has done since a copy
// was taken, and how to take the new one.
func Agents(args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list":
		return agentsList(args)
	case "show":
		return agentsShow(args)
	case "diff":
		return agentsDiff(args)
	case "update":
		return agentsUpdate(args)
	default:
		return errors.New("usage: bd-auto agents <list|show|diff|update>")
	}
}

// agentsShow prints the system prompt a role will actually be spawned with:
// splices substituted, verdict contract in place. It is the only way to read
// what a model is going to be told without spawning one, and the splices are
// exactly the part a repo cannot check by reading its own file.
func agentsShow(args []string) error {
	fs := flag.NewFlagSet("agents show", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return errors.New("usage: bd-auto agents show <role>")
	}
	role := fs.Arg(0)
	c, err := NewCtx()
	if err != nil {
		return err
	}
	if !c.Cfg.RoleDefined(role) {
		return fmt.Errorf("agents show: %q is not a defined role; defined roles are %s",
			role, strings.Join(c.Cfg.Roles(), ", "))
	}
	info("bd-auto: %s prompt, from %s", role, c.Cfg.PromptSource(role))
	fmt.Print(c.Cfg.RolePrompt(role))
	return nil
}

// agentsList reports what every role resolves to: its prompt's source, its
// model, and whether a materialised copy has fallen behind the shipped prompt.
func agentsList(args []string) error {
	fs := flag.NewFlagSet("agents list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := NewCtx()
	if err != nil {
		return err
	}

	var out []map[string]any
	seen := map[string]bool{}
	for _, s := range c.Cfg.PromptSources() {
		seen[s.Role] = true
		out = append(out, describeAgent(c.Cfg, s, true))
	}
	for _, role := range c.Cfg.Agents() {
		if seen[role] {
			continue
		}
		out = append(out, describeAgent(c.Cfg, c.Cfg.PromptSource(role), false))
	}
	return emitJSON(map[string]any{
		"dir":    filepath.Join(c.RepoRoot, config.AgentsDir()),
		"agents": out,
	})
}

// describeAgent is one role's line in `agents list`.
func describeAgent(cfg *config.Config, s config.PromptSource, dispatched bool) map[string]any {
	spec := cfg.Runner(s.Role)
	e := map[string]any{
		"role":        s.Role,
		"prompt":      s.String(),
		"origin":      string(s.Origin),
		"judging":     s.Judging,
		"dispatched":  dispatched,
		"model":       spec.Model,
		"permissions": string(spec.Permissions),
	}
	if a := cfg.Agent(s.Role); a != nil {
		e["path"] = a.Path
		if a.Meta.Source != "" {
			e["source"] = a.Meta.Source
		}
		if a.Meta.Version != "" {
			e["bd_auto_version"] = a.Meta.Version
		}
		if a.Meta.Description != "" {
			e["description"] = a.Meta.Description
		}
		if a.Drifted() {
			e["drifted"] = true
			e["take_it_with"] = "bd-auto agents update " + s.Role
		}
	}
	return e
}

// agentsDiff prints what the shipped prompt has done since a copy of it was
// materialised into this repo.
//
// This is the other half of the trade `bd-auto init` makes. Materialising pins
// the prompt, which is what stops an upgrade changing a repo's runs underneath
// it; the cost is that an improvement never arrives on its own, and a cost with
// no way to see it is a trap rather than a trade.
func agentsDiff(args []string) error {
	fs := flag.NewFlagSet("agents diff", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := NewCtx()
	if err != nil {
		return err
	}
	roles := fs.Args()
	if len(roles) == 0 {
		for _, role := range c.Cfg.Agents() {
			if _, err := prompts.For(role); err == nil {
				roles = append(roles, role)
			}
		}
	}
	if len(roles) == 0 {
		info("bd-auto: no agent file in %s is a copy of a built-in prompt", config.AgentsDir())
		return nil
	}

	same := 0
	for _, role := range roles {
		builtin, err := prompts.For(role)
		if err != nil {
			return fmt.Errorf("agents diff: %w", err)
		}
		a := c.Cfg.Agent(role)
		if a == nil {
			return fmt.Errorf("agents diff: no agent file for %q; expected %s",
				role, config.AgentPath(c.RepoRoot, role))
		}
		d := unifiedDiff("builtin/"+role+config.AgentExt, rel(c.RepoRoot, a.Path),
			lines(builtin), lines(a.Body))
		if d == "" {
			same++
			continue
		}
		fmt.Print(d)
	}
	if same == len(roles) {
		info("bd-auto: every agent checked matches the prompt this binary ships")
	}
	return nil
}

// agentsUpdate re-materialises agents from the built-in prompts, discarding
// whatever was in the file. It takes the roles by name, or --all, because
// overwriting a prompt somebody tuned is not something to do on a bare verb.
func agentsUpdate(args []string) error {
	fs := flag.NewFlagSet("agents update", flag.ContinueOnError)
	all := fs.Bool("all", false, "update every role that has a built-in prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := NewCtx()
	if err != nil {
		return err
	}
	roles := fs.Args()
	if *all {
		roles = prompts.Roles()
	}
	if len(roles) == 0 {
		return errors.New("usage: bd-auto agents update <role>... | --all")
	}

	dir := filepath.Join(c.RepoRoot, config.AgentsDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	var out []map[string]any
	for _, role := range roles {
		content, err := config.BuiltinAgentFile(role, Version)
		if err != nil {
			return fmt.Errorf("agents update: %w", err)
		}
		p := filepath.Join(dir, role+config.AgentExt)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		info("bd-auto: %s now carries the %s prompt this binary ships", rel(c.RepoRoot, p), role)
		out = append(out, map[string]any{"role": role, "path": p, "written": true})
	}
	return emitJSON(map[string]any{"updated": out})
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

func lines(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }

// --- a unified diff, in about as many lines as importing one would cost ---

// diffOp is one line of an edit script: kept, removed or added.
type diffOp struct {
	kind byte // ' ', '-' or '+'
	text string
}

// diffOps is the shortest edit script from a to b, by the textbook
// longest-common-subsequence table. The inputs are two prompts, so the O(n*m)
// table is a few thousand cells and the simplicity is worth more than the
// speed.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffOp{' ', a[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, diffOp{'-', a[i]})
			i++
		default:
			out = append(out, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffOp{'+', b[j]})
	}
	return out
}

// diffContext is how many unchanged lines surround a change.
const diffContext = 3

// unifiedDiff renders a to b in the format every reader already knows. It
// returns "" when the two are identical, which is how the caller says "this
// agent has not drifted" without a second comparison.
func unifiedDiff(oldName, newName string, a, b []string) string {
	ops := diffOps(a, b)
	var changed []int
	for i, op := range ops {
		if op.kind != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return ""
	}

	// Line numbers each op sits at, counted separately in each file.
	oldAt := make([]int, len(ops))
	newAt := make([]int, len(ops))
	ol, nl := 1, 1
	for i, op := range ops {
		oldAt[i], newAt[i] = ol, nl
		if op.kind != '+' {
			ol++
		}
		if op.kind != '-' {
			nl++
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	for k := 0; k < len(changed); {
		start := max(0, changed[k]-diffContext)
		end := min(len(ops)-1, changed[k]+diffContext)
		// Absorb every later change whose own context touches this hunk's.
		for k+1 < len(changed) && changed[k+1]-diffContext <= end+1 {
			k++
			end = min(len(ops)-1, changed[k]+diffContext)
		}
		k++

		var oldN, newN int
		for _, op := range ops[start : end+1] {
			if op.kind != '+' {
				oldN++
			}
			if op.kind != '-' {
				newN++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldAt[start], oldN, newAt[start], newN)
		for _, op := range ops[start : end+1] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
	}
	return out.String()
}
