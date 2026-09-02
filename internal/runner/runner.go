// Package runner is the seam between bd-auto's engine and whatever actually
// runs a model.
//
// The engine owns control flow and spawns models as subprocesses, so everything
// it needs to know about a backend has to fit through this one interface: how
// to invoke it (Request), what came back (Result), and what the backend can do
// at all (Capabilities). Nothing above this package names a vendor, and nothing
// in this package knows what a Claude flag looks like — adapters live in
// runner/<provider> and register themselves here.
//
// Three things keep the seam honest rather than shaped around one backend:
// Permissions is a coarse enum rather than a mode string, there is no agent
// concept (a role is a prompt plus a tool list, both of which have rough
// analogues everywhere), and Capabilities is branched on by the engine so a
// backend without resume degrades instead of failing.
package runner

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Role names who is being run. The three built-in roles cover the pipeline the
// engine drives itself; a custom pipeline stage is a role too, which is why
// this is an open string type rather than a closed enum.
type Role string

// The built-in roles.
const (
	RoleWorker     Role = "worker"
	RoleReviewer   Role = "reviewer"
	RoleIntegrator Role = "integrator"
)

// BuiltinRoles returns the roles the engine drives itself, in a stable order.
func BuiltinRoles() []Role { return []Role{RoleWorker, RoleReviewer, RoleIntegrator} }

// Permissions is how much a run is allowed to do, as a coarse enum rather than
// a backend's mode string. Adapters translate: for the Claude CLI, PermAuto is
// --permission-mode auto. Raw backend flags belong in Request.ExtraArgs, not
// here, so that adding a backend does not add a permission level.
type Permissions string

// The permission levels. Ordered from least to most permitted.
const (
	// PermScoped allows only the tools listed in Request.AllowedTools. This is
	// what a reviewer runs under: a reviewer that can run bare Bash is a
	// reviewer that can push.
	//
	// It is the level, not the whole guard: it stops applying the moment
	// somebody widens the level, which --dangerously-skip-permissions does to
	// every role at once. Request.DeniedTools is what survives that.
	PermScoped Permissions = "scoped"
	// PermAuto delegates the decision to the backend's own classifier.
	PermAuto Permissions = "auto"
	// PermBypass skips permission checks entirely.
	PermBypass Permissions = "bypass"
)

// Valid reports whether p is a recognised permission level.
func (p Permissions) Valid() bool {
	switch p {
	case PermScoped, PermAuto, PermBypass:
		return true
	}
	return false
}

// AllPermissions returns every permission level, least permitted first.
func AllPermissions() []Permissions { return []Permissions{PermScoped, PermAuto, PermBypass} }

// Class is the field the engine reads first on a Result, and it exists because
// "the process exited non-zero" is not one thing.
//
// Without it, five parallel workers meeting one rate limit each burn every
// round and every retry they have against a 429, then park five perfectly good
// issues with a nonsense reason while the epic never closes. At concurrency 5
// against one account that is a likely Tuesday, not an edge case.
type Class string

// The result classes.
const (
	// ClassOK means the run happened and produced work to judge.
	ClassOK Class = "ok"
	// ClassWorkFailed means the run happened and the work is wrong. This is
	// the feedback path: another round on the same session.
	ClassWorkFailed Class = "work-failed"
	// ClassInfraFailed means the run never got a fair chance: usage limit,
	// 429/529, expired auth, network, CLI crash. The engine backs off and
	// re-runs the same round.
	ClassInfraFailed Class = "infra-failed"
	// ClassInterrupted means the context was cancelled — a stop from the TUI,
	// or a kill. The run is resumable; nothing about it is a verdict.
	ClassInterrupted Class = "interrupted"
)

// Valid reports whether c is a recognised class. A zero Class is not: an
// adapter that forgets to set one must not be read as success.
func (c Class) Valid() bool {
	switch c {
	case ClassOK, ClassWorkFailed, ClassInfraFailed, ClassInterrupted:
		return true
	}
	return false
}

// Counts reports whether a result of this class counts against the engine's
// budgets — both the per-attempt round counter and the per-issue attempt
// counter.
//
// Only work the model actually did counts. An infra failure consumes neither a
// round nor an attempt, and neither does an interrupt, because converting an
// outage or a keystroke into a pile of parked issues is the failure mode this
// whole taxonomy exists to prevent.
func (c Class) Counts() bool { return c == ClassOK || c == ClassWorkFailed }

// Recoverable reports whether the engine should back off and re-run the same
// round rather than judge the result.
func (c Class) Recoverable() bool { return c == ClassInfraFailed }

// Usage is what a run cost.
//
// Cost is the primary field, not summed tokens: across resumed rounds the same
// prefix is billed repeatedly as cache reads at a fraction of the input price,
// so adding up input tokens overstates a resumed attempt and understates a
// fresh one. The token counts are kept separately for exactly that reason.
type Usage struct {
	CostUSD             float64 `json:"cost_usd"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	// Turns is how many turns the model took, summed the same way as the rest.
	// It is the one figure here that is about the work rather than the bill: a
	// worker that finished in four turns and one that ground through forty can
	// cost the same, and only this tells them apart. A backend that does not
	// report it leaves it zero.
	Turns int `json:"turns,omitempty"`
}

// Add returns the sum of two usages, for accumulating a run's total.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		CostUSD:             u.CostUSD + o.CostUSD,
		InputTokens:         u.InputTokens + o.InputTokens,
		OutputTokens:        u.OutputTokens + o.OutputTokens,
		CacheReadTokens:     u.CacheReadTokens + o.CacheReadTokens,
		CacheCreationTokens: u.CacheCreationTokens + o.CacheCreationTokens,
		Turns:               u.Turns + o.Turns,
	}
}

// IsZero reports whether nothing was recorded, which is what a backend that
// does not report usage leaves behind.
func (u Usage) IsZero() bool { return u == Usage{} }

// Request is one invocation of a model.
type Request struct {
	// Role is who is being run: worker, reviewer, integrator or a custom
	// stage. Adapters may log it; the engine uses it to pick a Spec.
	Role Role
	// SystemPrompt is the role prompt, appended to whatever system prompt the
	// backend already has.
	SystemPrompt string
	// Prompt is the task.
	Prompt string
	// Dir is the working directory, normally the issue's worktree. It must be
	// stable across rounds: backends resolve a session against the project
	// derived from the working directory, so a moved worktree is a lost
	// session.
	Dir string
	// SessionID is generated by the caller, so a later round can resume
	// without parsing any output to find out what the session was called.
	SessionID string
	// Resume continues SessionID rather than starting it. Callers must check
	// Caps().Resume first; a backend without resume needs the feedback in the
	// prompt instead.
	Resume bool
	// Model is the backend's model name, passed through unchanged.
	Model string
	// Permissions is the coarse level; adapters translate it.
	Permissions Permissions
	// AllowedTools limits which tools the run may use. Required under
	// PermScoped, ignored otherwise.
	AllowedTools []string
	// DeniedTools names tools the run may not use whatever else permits them.
	//
	// It applies at every level, and that is the point: an allowlist is only a
	// control while the run is scoped, so a role that must never do a thing —
	// the reviewer and the record of the issue it is judging — needs a rule
	// that outlives a widened permission level. Adapters must map it onto
	// whatever their backend checks ahead of everything else, and a backend
	// with no such check must not silently accept the list.
	DeniedTools []string
	// ExtraArgs is the per-backend escape hatch for flags this seam
	// deliberately does not model.
	ExtraArgs []string
	// ToolServers are tools the engine offers this run on top of the backend's
	// own. Callers must check Caps().Tools first; a backend that cannot offer
	// tools ignores this rather than failing, and the engine simply does not
	// fill it in.
	ToolServers []ToolServer
	// Timeout bounds one invocation. Zero means unlimited, and zero is the
	// default: what bounds a run is the set of issues a human picked, not a
	// clock.
	Timeout time.Duration
	// LogPath is where the adapter writes the raw transcript, empty for none.
	// The caller picks it because the caller is the one that knows which issue
	// and round this is; the adapter only reports it back on Result.LogPath.
	LogPath string
}

// ToolServer is a tool the engine offers a run: a process the backend starts
// and talks MCP to over stdio.
//
// MCP is named at this seam where a vendor never is, and the distinction is the
// point. It is an open protocol several backends already speak, so describing a
// tool this way is describing it once rather than once per adapter — and an
// adapter whose backend speaks something else still has everything it needs
// here to translate, because a command, its arguments and the names of the
// tools it offers are what any of them would ask for.
//
// A backend that cannot offer tools at all reports Capabilities.Tools false,
// and the engine leaves Request.ToolServers empty rather than failing.
type ToolServer struct {
	// Name is the server's name, and normally half of each tool's qualified
	// name.
	Name string
	// Command and Args start it. It speaks its protocol on stdin and stdout, so
	// it must write nothing else to either.
	Command string
	Args    []string
	// Env is added to the server's environment, as KEY=VALUE entries.
	Env []string
	// Tools names what this server offers, unqualified. The adapter qualifies
	// them however its backend does and puts them on the run's allowlist, so a
	// scoped run can still call them.
	Tools []string
	// Required makes failure to initialize this server fail the model run. An
	// engine tool that the prompt relies on should set this rather than letting
	// the backend silently continue without it.
	Required bool
	// Timeout is the least the backend must allow one call to these tools. It
	// is a floor and not a cap: the adapter knows what its backend will accept
	// and may ask for far more, which the shipped one does. Zero leaves the
	// whole decision to the adapter.
	//
	// It bounds one call and not the wait behind it. A tool that may wait
	// longer than any backend will hold a call open is expected to hand back a
	// ticket and be polled, which is what bd-auto's own ask_user does.
	Timeout time.Duration
}

// Result is what came back. The engine branches on Class before reading any
// other field.
type Result struct {
	// Class is how to read everything below it.
	Class Class
	// Text is the final assistant message. Full transcripts go to LogPath;
	// only this is kept in memory.
	Text string
	// SessionID is the session that ran, echoed back so it can be resumed.
	SessionID string
	// ExitCode is the backend process's exit code, or -1 when it never ran.
	ExitCode int
	// Err is the backend-reported failure, for the log and the TUI. It is set
	// on every class but ClassOK, and it is never the only signal: Class is.
	Err error
	// Denials names the tools the backend refused to let the run use,
	// deduplicated and in the order they were first refused.
	//
	// It is not a class, because a denial is not by itself a failure: a run can
	// be refused one tool, take another route and finish. What makes it worth
	// carrying is the case where it did not — a headless run under a permission
	// level that asks a human is refused every write, and without this the
	// engine sees a process that exited 0 with an empty worktree and reads it as
	// a model that did nothing.
	//
	// Empty for a backend that does not report denials.
	Denials []string
	// Usage is zero when the backend does not report it.
	Usage Usage
	// Duration is wall time for this invocation.
	Duration time.Duration
	// TimedOut reports that Request.Timeout fired. It implies
	// ClassInterrupted.
	TimedOut bool
	// ResetAt is when the backend said the outage it just reported will lift.
	//
	// A plan limit is the one outage that knows its own end and says so, and it
	// is the one where backing off is useless: five retries doubling from five
	// seconds spend 75 seconds against a wall that has tens of minutes left on
	// it. An adapter that can read a reset time out of what its backend said
	// puts it here; zero means the backend did not say, which is the ordinary
	// case and every class but ClassInfraFailed.
	//
	// It is an instant rather than a duration because it is a fact about the
	// account rather than about this call: it survives being written into a
	// report and read later, and the engine subtracts the clock itself.
	ResetAt time.Time
	// LogPath is the full transcript on disk, empty when none was written.
	LogPath string
}

// Capabilities describes what a backend can do, so the engine can degrade
// rather than fail against a backend that does less.
type Capabilities struct {
	// Resume is whether a session can be continued. Where it is false, every
	// feedback round degrades to a fresh process carrying the feedback in its
	// prompt — correct everywhere, just more expensive.
	Resume bool
	// Stream is whether the adapter emits events while the run is in flight.
	// Where it is false the TUI shows a spinner instead of activity.
	Stream bool
	// ReportsUsage is whether Result.Usage is populated.
	ReportsUsage bool
	// Tools is whether the backend can be given tools of the engine's own, as
	// Request.ToolServers. Where it is false the run simply does not have them:
	// a worker cannot ask the human a question, and decides for itself instead,
	// which is what every run did before the tools existed.
	Tools bool
	// Permissions lists the levels this backend can express.
	Permissions []Permissions
}

// Supports reports whether the backend can express a permission level.
func (c Capabilities) Supports(p Permissions) bool {
	for _, have := range c.Permissions {
		if have == p {
			return true
		}
	}
	return false
}

// Runner runs one model invocation to completion.
//
// Run returns a non-nil error only when it could not produce a Result at all —
// a malformed Request, an unusable working directory. Everything a backend can
// fail at during a run (rate limits, crashes, cancellation, timeouts) comes
// back as a Result with a Class and Err set, because that is what the engine
// knows how to route.
type Runner interface {
	// Name is the provider name this runner was registered under.
	Name() string
	// Caps describes what this backend can do.
	Caps() Capabilities
	// Run executes req, emitting events to sink as they arrive. A nil sink is
	// allowed and means "no live output"; use Emit to honour that.
	Run(ctx context.Context, req Request, sink EventSink) (Result, error)
}

// Preflighter is a Runner whose backend can be checked before a run spends
// anything on discovering it is unusable.
//
// It is optional because the check only an adapter can make is the one worth
// making. "Is the binary on PATH" is answerable from anywhere and catches
// almost nothing; "will this backend accept the invocation this adapter is
// about to build" is the failure that matters — a flag the adapter is written
// against that the installed version renamed or dropped — and it is invisible
// above this seam. So what a preflight costs, and whether there is one at all,
// is the adapter's decision.
//
// dir is where to check, normally the repo the run is for; empty means this
// process's working directory. The description is one line naming what was
// found — a version, a model — for the log a human reads while nothing has
// been spent yet.
type Preflighter interface {
	Runner
	Preflight(ctx context.Context, dir string) (string, error)
}

// BillingSource says which account a backend invocation will charge. The
// distinction is deliberately small: bd-auto only needs to know whether an
// invocation is covered by an authenticated product plan, will create API
// charges, or cannot be established safely enough to start.
type BillingSource string

const (
	BillingChatGPTPlan BillingSource = "chatgpt-plan"
	BillingAPIKey      BillingSource = "api-key"
	BillingUnknown     BillingSource = "unknown"
)

// BillingChecker is implemented by a runner whose authentication source can
// change who pays for a run. It must inspect local CLI/environment state only;
// checking billing must never make a paid API request.
type BillingChecker interface {
	Runner
	BillingSource(ctx context.Context, dir string) (BillingSource, error)
}

// BillingSourceOf checks r when it has a billing-sensitive backend. checked is
// false for runners whose billing is outside this gate.
func BillingSourceOf(ctx context.Context, r Runner, dir string) (source BillingSource, checked bool, err error) {
	b, ok := r.(BillingChecker)
	if !ok {
		return "", false, nil
	}
	source, err = b.BillingSource(ctx, dir)
	return source, true, err
}

// Preflight checks a runner's backend where it can be checked, and reports
// nothing to check otherwise: a backend that offers no preflight has not
// failed one, and must not stop a run.
func Preflight(ctx context.Context, r Runner, dir string) (string, error) {
	p, ok := r.(Preflighter)
	if !ok {
		return "", nil
	}
	return p.Preflight(ctx, dir)
}

// EventKind classifies a live event from a run in flight.
type EventKind string

// The event kinds. Adapters map whatever their backend streams onto these; an
// adapter that cannot stream emits EventStart and EventDone only.
const (
	EventStart      EventKind = "start"
	EventText       EventKind = "text"
	EventToolUse    EventKind = "tool-use"
	EventToolResult EventKind = "tool-result"
	EventUsage      EventKind = "usage"
	EventError      EventKind = "error"
	EventDone       EventKind = "done"
)

// Event is one live update from a run. It is what turns a three-minute spinner
// into a per-worker activity line.
type Event struct {
	Kind      EventKind
	Role      Role
	SessionID string
	// Text is the message fragment for EventText, and the message for
	// EventError.
	Text string
	// Tool is the tool name for EventToolUse and EventToolResult.
	Tool string
	// Usage carries the running total on EventUsage and EventDone.
	Usage Usage
	// At is when the event was observed.
	At time.Time
}

// EventSink receives events from a run in flight.
type EventSink interface {
	Emit(Event)
}

// SinkFunc adapts a function to EventSink. A nil SinkFunc is a valid sink that
// drops everything.
type SinkFunc func(Event)

// Emit implements EventSink.
func (f SinkFunc) Emit(e Event) {
	if f != nil {
		f(e)
	}
}

// Discard is an EventSink that drops every event.
var Discard EventSink = SinkFunc(nil)

// Emit sends e to sink, tolerating a nil sink so adapters need no guard around
// every event they produce.
func Emit(sink EventSink, e Event) {
	if sink != nil {
		sink.Emit(e)
	}
}

// Spec is a resolved runner configuration for one role: what the config
// package hands the engine after resolving runners.<role> over runners.default.
// It is the only thing a Factory gets, which is what keeps provider-specific
// settings out of the engine.
type Spec struct {
	// Provider selects the adapter, by its registered name.
	Provider string
	// Model is passed to the backend unchanged.
	Model string
	// Permissions is the coarse level for this role.
	Permissions Permissions
	// AllowedTools limits the role's tools under PermScoped.
	AllowedTools []string
	// DeniedTools names tools this role may not use at any level.
	DeniedTools []string
	// ExtraArgs is the per-backend escape hatch.
	ExtraArgs []string
	// Timeout bounds one invocation; zero means unlimited.
	Timeout time.Duration
	// Resume is whether this role's feedback rounds continue the same session.
	// It is a preference, not a capability: the engine still checks
	// Caps().Resume.
	Resume bool
	// Sandbox and ApprovalPolicy are Codex-native controls. They intentionally
	// do not share Claude's permission vocabulary.
	Sandbox        string
	ApprovalPolicy string
	// AddDirs are explicit extra writable roots for a Codex workspace-write
	// sandbox. They are provider-native capabilities, not general permissions.
	AddDirs   []string
	Shell     bool
	WebSearch bool
	ViewImage bool
	// BillingSensitive asks the engine to run the adapter's BillingChecker
	// before any model or filesystem side effect. It is provider metadata set
	// by configuration resolution, not a user-facing setting.
	BillingSensitive bool
}

// Request returns a Request for role with everything the spec fixes already
// filled in. The caller adds the prompts, the directory and the session.
func (s Spec) Request(role Role) Request {
	return Request{
		Role:         role,
		Model:        s.Model,
		Permissions:  s.Permissions,
		AllowedTools: append([]string(nil), s.AllowedTools...),
		DeniedTools:  append([]string(nil), s.DeniedTools...),
		ExtraArgs:    append([]string(nil), s.ExtraArgs...),
		Timeout:      s.Timeout,
	}
}

// Factory builds a Runner for a resolved spec.
type Factory func(Spec) (Runner, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a provider to the registry, normally from an adapter package's
// init. Registering the same name twice is a programming error and panics,
// because the alternative is a backend silently replacing another.
func Register(provider string, f Factory) {
	if provider == "" {
		panic("runner: Register with an empty provider name")
	}
	if f == nil {
		panic("runner: Register " + provider + " with a nil factory")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[provider]; dup {
		panic("runner: provider " + provider + " registered twice")
	}
	registry[provider] = f
}

// Providers lists the registered provider names, sorted.
func Providers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// New builds the runner spec.Provider names.
//
// An unknown provider is reported with the list of known ones, because the
// realistic cause is a typo in .beads-auto.yaml and the realistic reader is
// someone who does not know what is available.
func New(spec Spec) (Runner, error) {
	if spec.Provider == "" {
		return nil, fmt.Errorf("runner: no provider set")
	}
	registryMu.RLock()
	f, ok := registry[spec.Provider]
	registryMu.RUnlock()
	if !ok {
		known := Providers()
		if len(known) == 0 {
			return nil, fmt.Errorf("runner: unknown provider %q: no providers are registered", spec.Provider)
		}
		return nil, fmt.Errorf("runner: unknown provider %q: known providers are %v", spec.Provider, known)
	}
	return f(spec)
}
