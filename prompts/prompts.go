// Package prompts carries the role prompts a bd-auto run sends as the system
// prompt of every model it spawns.
//
// They are embedded rather than read from disk because a run spawns processes
// in worker worktrees, and a worktree is not where the prompts live. Embedding
// also means the binary cannot be installed without them: a role prompt is not
// optional decoration, it is the half of the contract the issue text does not
// cover.
package prompts

import (
	_ "embed"
	"fmt"
	"sort"
)

//go:embed worker.md
var worker string

//go:embed reviewer.md
var reviewer string

//go:embed integrator.md
var integrator string

// Worker is the prompt for the role that implements an issue.
func Worker() string { return worker }

// Reviewer is the prompt for the role that judges a worker's diff.
func Reviewer() string { return reviewer }

// Integrator is the prompt for the role that resolves a merge conflict.
func Integrator() string { return integrator }

// byRole is the lookup For uses. Keyed by the runner role name, spelled here as
// a plain string so this package stays free of every other one.
var byRole = map[string]func() string{
	"worker":     Worker,
	"reviewer":   Reviewer,
	"integrator": Integrator,
}

// For returns the prompt for a role.
//
// An unknown role is an error rather than an empty prompt: a model spawned with
// no role prompt inherits only the repo's CLAUDE.md, which is exactly the
// instruction set these prompts exist to override.
func For(role string) (string, error) {
	if f, ok := byRole[role]; ok {
		return f(), nil
	}
	return "", fmt.Errorf("prompts: no prompt for role %q; known roles are %v", role, Roles())
}

// Roles lists the roles a prompt exists for, sorted.
func Roles() []string {
	out := make([]string, 0, len(byRole))
	for name := range byRole {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
