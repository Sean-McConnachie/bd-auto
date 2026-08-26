package drain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bd-auto/internal/runner"
)

// Preflight checks every backend this run would spawn, once, before the run
// creates anything.
//
// The failure it is here for is not exotic: this repo's claude adapter builds
// its argv against one CLI's flag list, and an install that renamed or dropped
// one of those flags rejects every invocation. Discovered during the run, that
// arrives as a process that exits before printing a result — which is
// infra-failed, which is the class the engine backs off and retries. So five
// workers each burn their infra retries on a command that cannot work, and the
// only trace of why is buried in five transcripts, behind five worktrees, five
// branches and five claimed issues.
//
// Checked here it is one error, before a worktree exists.
//
// What is checked is one backend per distinct configuration rather than one per
// role, because the roles of a default run resolve to the same provider and the
// same model, and a check that spent a model call per role would charge three
// times for one answer. Where a role does differ — the reviewer, normally, on a
// cheaper model under scoped permissions — it is a different invocation and it
// is checked separately.
//
// The integrator is included even though a run only spawns one when a merge
// conflicts. A backend that is going to be unusable at the barrier is worth
// knowing about before the wave, and by default it is the worker's
// configuration anyway, so knowing costs nothing.
//
// A backend that offers no preflight — the fake, anything a future adapter
// declines to check — is not a failure. See runner.Preflight.
func (e *Engine) Preflight(ctx context.Context) error {
	if e.SkipPreflight || e.preflighted {
		return nil
	}
	if e.Cfg == nil {
		return errors.New("drain: Preflight needs Cfg")
	}
	e.preflighted = true
	return e.preflight(ctx, e.preflightGroups())
}

func (e *Engine) preflightIssue(ctx context.Context) error {
	if e.SkipPreflight || e.preflighted || e.issuePreflighted {
		return nil
	}
	if e.Cfg == nil {
		return errors.New("drain: Preflight needs Cfg")
	}
	e.issuePreflighted = true
	return e.preflight(ctx, e.preflightGroupsFor(false))
}

func (e *Engine) preflight(ctx context.Context, groups []preflightGroup) error {
	for _, group := range groups {
		role := group.roles[0]
		rn, err := e.runnerFor(role)
		if err != nil {
			return err
		}
		desc, err := runner.Preflight(ctx, rn, e.RepoRoot)
		if err != nil {
			// Named by role rather than by provider: the reader's next move is
			// to edit a runners: entry or install a CLI, and the role is which
			// entry. The way past the check is named too, because a check that
			// is wrong about a backend that works is worth a flag rather than
			// an afternoon.
			return fmt.Errorf("drain: the %s runner is not usable, so nothing was dispatched: %w\n"+
				"Fix it, or re-run with --no-preflight to start anyway.",
				strings.Join(group.names(), "/"), err)
		}
		if desc != "" {
			e.logf("preflight: %s ok for %s", desc, strings.Join(group.names(), ", "))
		}
	}
	return nil
}

// preflightGroup is one backend configuration and the roles that share it.
type preflightGroup struct {
	roles []runner.Role
}

func (g preflightGroup) names() []string {
	out := make([]string, 0, len(g.roles))
	for _, r := range g.roles {
		out = append(out, string(r))
	}
	return out
}

// preflightGroups collects the roles this run can dispatch, in the order they
// would first run, grouped by the configuration they resolve to.
func (e *Engine) preflightGroups() []preflightGroup {
	return e.preflightGroupsFor(true)
}

func (e *Engine) preflightGroupsFor(includeIntegrator bool) []preflightGroup {
	var (
		order  []string
		groups = map[string]*preflightGroup{}
		seen   = map[runner.Role]bool{}
	)
	add := func(role runner.Role) {
		if role == "" || seen[role] {
			return
		}
		seen[role] = true
		key := specKey(e.Cfg.Runner(string(role)))
		g, ok := groups[key]
		if !ok {
			g = &preflightGroup{}
			groups[key] = g
			order = append(order, key)
		}
		g.roles = append(g.roles, role)
	}

	// The implement stage's role is added ahead of the loop rather than left to
	// it, so a Config built in code with no pipeline at all still checks the
	// backend the work will actually run on.
	add(e.implementRole())
	for _, s := range e.Cfg.Pipeline {
		add(e.stageRole(s))
	}
	if includeIntegrator {
		add(runner.RoleIntegrator)
	}

	out := make([]preflightGroup, 0, len(order))
	for _, key := range order {
		out = append(out, *groups[key])
	}
	return out
}

// specKey identifies a backend configuration by everything that changes what
// gets spawned. Two roles with the same key produce the same invocation, so
// checking one of them checks both; permissions and the tool list are in it
// because for at least one backend they are the argv, not a setting beside it.
func specKey(s runner.Spec) string {
	return strings.Join([]string{
		s.Provider,
		s.Model,
		string(s.Permissions),
		strings.Join(s.AllowedTools, "\x1f"),
		strings.Join(s.DeniedTools, "\x1f"),
		strings.Join(s.ExtraArgs, "\x1f"),
		s.Sandbox,
		s.ApprovalPolicy,
		fmt.Sprintf("%t", s.Shell),
		fmt.Sprintf("%t", s.WebSearch),
		fmt.Sprintf("%t", s.ViewImage),
		fmt.Sprintf("%t", s.BillingSensitive),
	}, "\x00")
}
