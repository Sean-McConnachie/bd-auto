package config

import (
	"fmt"
	"strings"
)

// A hook is somewhere to hang an agent that reads a result.
//
// Everything else in bd-auto interprets a result while it is still deciding
// something: a reviewer judges a diff and its verdict routes the issue, the
// integrator resolves a conflict and its tree becomes the merge. There was
// nowhere to attach something that reads what a stage or a run *produced* and
// says something about it without being in the path of the decision. Triage is
// the obvious one — is this discovery new work, a duplicate, or noise — and it
// is not the only one.
//
// # Advisory, and only advisory
//
// A hook reads. Its output is recorded on the run's report and shown to whoever
// is watching, and bd-auto reads nothing back out of it: no hook can change a
// verdict, park an issue, fail a run or stop a handoff. That is the whole
// authority rule and it is deliberately the smaller half of the choice.
//
// The reason is what the alternative costs. An authoritative hook puts a prompt
// nobody reviewed in front of every verdict the engine reaches, and a repo that
// gets that prompt slightly wrong does not get a slightly worse run — it gets
// finished work parked and nothing on screen explaining why. Advisory is useful
// on the first day and cannot make the engine wrong. Authority can be given
// later, per hook, explicitly, once something has needed it; it cannot be taken
// back once a repo depends on it.
//
// # The three points
//
// Each one is a moment where a result exists and nothing is still being decided
// about it:
//
//	on_issue_end  after an issue reaches its verdict; gets that issue's Report
//	on_barrier    after a wave has merged and gated; gets the IntegrateReport
//	on_run_end    after the run has finished and handed over; gets the DrainReport
//
// # What a hook may not do
//
// One writer per issue. At the barrier and at the end of a run no worker is
// live, so a hook there can write what it likes. on_issue_end is the one that
// runs beside live siblings, so it is handed exactly one issue — its own, whose
// worker has already exited — and it must not write to another.
//
// Not a place to run git. Hooks run in the main checkout, which the run is
// using: the barrier merges into it and the next wave's worktrees branch from
// it. A hook that commits, switches or resets there corrupts the run around it.
//
// Not a place to hang. Every hook is bounded by a timeout, the same treatment a
// run: stage gets, because a hook is advisory and a run must never be held up
// indefinitely by something that cannot change its outcome.

// HookPoint names one moment a run will run hooks at.
type HookPoint string

const (
	// HookIssueEnd fires after an issue reaches a verdict, with that issue's
	// Report. It does not fire for an outcome that is not a verdict.
	HookIssueEnd HookPoint = "on_issue_end"
	// HookBarrier fires after a wave barrier, with its IntegrateReport: what
	// merged, what conflicted, what the gate said.
	HookBarrier HookPoint = "on_barrier"
	// HookRunEnd fires once the run is over and handed over, with the whole
	// DrainReport.
	HookRunEnd HookPoint = "on_run_end"
)

// HookPoints lists the points, in the order a run reaches them.
func HookPoints() []HookPoint { return []HookPoint{HookIssueEnd, HookBarrier, HookRunEnd} }

// DefaultHookTimeout bounds one hook, in seconds.
//
// Shorter than DefaultCommandTimeout on purpose. A gate command is the thing
// the run is waiting to hear from, so it is worth waiting a quarter of an hour
// for; a hook is advisory, so every second it takes is a second the run spends
// on something that cannot change what it decided. Five minutes is room to read
// a report and say something about it. A hook that genuinely needs longer says
// so with timeout:.
const DefaultHookTimeout = 300

// Hook is one thing to run at a hook point. Exactly one of Agent or Run must be
// set, which is the same choice a pipeline stage offers and is validated the
// same way.
type Hook struct {
	// Name identifies the hook on the report and in the display. Unset gets one
	// derived from the point and the position.
	Name string `yaml:"name"`
	// Agent names a runner role, exactly as a pipeline stage's agent: does: a
	// key under runners:, an agent file, or a built-in role.
	Agent string `yaml:"agent"`
	// Run is a shell command executed by this binary.
	Run string `yaml:"run"`
	// Timeout in seconds. Zero means DefaultHookTimeout; there is no unlimited,
	// because a hook that cannot hang a run is the promise this field keeps.
	Timeout int `yaml:"timeout"`
}

// Kind classifies a hook the way Stage.Kind classifies a stage.
func (h Hook) Kind() string {
	switch {
	case h.Agent != "" && h.Run != "":
		return "invalid"
	case h.Agent != "":
		return "agent"
	case h.Run != "":
		return "run"
	}
	return "invalid"
}

// Hooks is the hooks: block: what runs at each point, in order.
type Hooks struct {
	OnIssueEnd []Hook `yaml:"on_issue_end"`
	OnBarrier  []Hook `yaml:"on_barrier"`
	OnRunEnd   []Hook `yaml:"on_run_end"`
}

// At returns the hooks configured for a point.
func (h Hooks) At(p HookPoint) []Hook {
	switch p {
	case HookIssueEnd:
		return h.OnIssueEnd
	case HookBarrier:
		return h.OnBarrier
	case HookRunEnd:
		return h.OnRunEnd
	}
	return nil
}

// HooksAt returns the hooks configured for a point.
func (c *Config) HooksAt(p HookPoint) []Hook { return c.Hooks.At(p) }

// HasHooks reports whether this config hangs anything anywhere.
func (c *Config) HasHooks() bool {
	for _, p := range HookPoints() {
		if len(c.HooksAt(p)) > 0 {
			return true
		}
	}
	return false
}

// HookTimeout is how long one hook may take, resolved.
func (c *Config) HookTimeout(h Hook) int {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return DefaultHookTimeout
}

// HookRoles lists every role a hook dispatches, sorted by the order the points
// are reached. It is what puts a hook's agent into PromptSources, so a hook that
// named a role with no prompt of its own is visible rather than silent.
func (c *Config) HookRoles() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range HookPoints() {
		for _, h := range c.HooksAt(p) {
			if h.Agent == "" || seen[h.Agent] {
				continue
			}
			seen[h.Agent] = true
			out = append(out, h.Agent)
		}
	}
	return out
}

// applyHookDefaults names the hooks that did not name themselves and fills in
// the timeout every hook has.
func (c *Config) applyHookDefaults() {
	for _, p := range HookPoints() {
		hooks := c.HooksAt(p)
		for i := range hooks {
			if hooks[i].Name == "" {
				hooks[i].Name = fmt.Sprintf("%s-%d", p, i+1)
			}
			// Only an absent timeout is filled in. A negative one is a
			// mistake worth reporting rather than quietly rounding up to the
			// default, and Validate is where it gets reported.
			if hooks[i].Timeout == 0 {
				hooks[i].Timeout = DefaultHookTimeout
			}
		}
	}
}

// validateHooks checks the hooks: block for the contradictions a pipeline stage
// is checked for, at load, where they cost a line of output rather than a wave.
func (c *Config) validateHooks() error {
	for _, p := range HookPoints() {
		seen := map[string]bool{}
		for i, h := range c.HooksAt(p) {
			where := fmt.Sprintf("hooks: %s[%d] (%s)", p, i, nameOrIndex(h.Name, i))
			if h.Agent != "" && h.Run != "" {
				return fmt.Errorf("%s: set agent or run, not both", where)
			}
			if h.Kind() == "invalid" {
				return fmt.Errorf("%s: needs either agent or run", where)
			}
			if seen[h.Name] {
				return fmt.Errorf("%s: duplicate hook name %q at this point", where, h.Name)
			}
			seen[h.Name] = true
			if h.Timeout < 0 {
				return fmt.Errorf("%s: timeout: %d is negative; leave it unset for the default of %ds, "+
					"and note that a hook has no unlimited", where, h.Timeout, DefaultHookTimeout)
			}
			if h.Agent != "" && !c.RoleDefined(h.Agent) {
				return fmt.Errorf("%s: agent: %q is not a defined runner role; valid roles are %s. "+
					"Define it with a key under runners:, or with an agent file at %s",
					where, h.Agent, strings.Join(c.Roles(), ", "), c.agentPathHint(h.Agent))
			}
		}
	}
	return nil
}

func nameOrIndex(name string, i int) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("#%d", i+1)
}
