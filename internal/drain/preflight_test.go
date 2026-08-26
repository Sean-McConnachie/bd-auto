package drain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
	"bd-auto/internal/worktree"
)

// fakeCfg is a run whose every role resolves to the fake backend, so a whole
// drain goes through the registry the way a real repo does.
func fakeCfg(rounds, retry int) *config.Config {
	cfg := testCfg(rounds, retry)
	cfg.Runners = map[string]config.RunnerSpec{config.RoleDefault: {Provider: fake.Provider}}
	return cfg
}

// The point of the whole check: a backend that cannot be spawned costs one
// error, and nothing else. No worktree, no branch, no claimed issue, no run
// state, and not one model call.
func TestPreflightFailureDispatchesNothing(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	model := fake.New()
	model.PreflightErr = errors.New("unknown option '--include-partial-messages'")
	defer fake.Install(model)()

	e := &Engine{
		RepoRoot: repo, Cfg: fakeCfg(1, 0), BD: iss, Bus: NewBus(collector()),
		Prompt:  func(r runner.Role) (string, error) { return "system prompt", nil },
		Sleep:   func(context.Context, time.Duration) error { return nil },
		Backoff: func(int) time.Duration { return 0 },
	}

	_, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"}, Concurrency: 2,
	})
	if err == nil {
		t.Fatal("Drain ran a scope against a backend that failed its preflight")
	}
	if !strings.Contains(err.Error(), "unknown option") {
		t.Errorf("error = %v, want it to carry what the backend said", err)
	}
	if !strings.Contains(err.Error(), "worker") {
		t.Errorf("error = %v, want it to name the role whose runner failed", err)
	}

	if n := model.Calls(); n != 0 {
		t.Errorf("%d model call(s) were made after the preflight failed", n)
	}
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := os.Stat(worktree.Path(repo, id)); err == nil {
			t.Errorf("%s has a worktree; the run got as far as creating one", id)
		}
	}
	if _, err := runstate.Load(repo); err == nil {
		t.Error("run state was written for a run that never started")
	}
}

// Two roles on one configuration is one backend, and checking it twice would
// charge twice for the same answer. The default run is exactly that case:
// worker and integrator resolve to the same provider and model.
func TestPreflightChecksOneConfigurationOnce(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	model := fake.New(fake.Step{Text: "done", Do: steps(commitWork("t-1.txt"), closes(iss, "t-1"))})
	defer fake.Install(model)()

	var logged []string
	var logMu sync.Mutex
	e := &Engine{
		RepoRoot: repo, Cfg: fakeCfg(1, 0), BD: iss, Bus: NewBus(collector()),
		Prompt:  func(r runner.Role) (string, error) { return "system prompt", nil },
		Sleep:   func(context.Context, time.Duration) error { return nil },
		Backoff: func(int) time.Duration { return 0 },
		Log: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	}
	if _, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	}); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// What was checked is worth a line: it is the only place a run says which
	// backend it is about to spend, while nothing has been spent.
	logMu.Lock()
	defer logMu.Unlock()
	var said string
	for _, l := range logged {
		if strings.HasPrefix(l, "preflight:") {
			said = l
		}
	}
	if !strings.Contains(said, "worker") || !strings.Contains(said, "integrator") {
		t.Errorf("the preflight logged %q, want it to name the roles it covered", said)
	}

	if got := model.Preflights(); len(got) != 1 {
		t.Fatalf("%d preflights for one configuration: %v", len(got), got)
	}
	if got := model.Preflights()[0]; got != repo {
		t.Errorf("the backend was checked in %q, want the repo root %q", got, repo)
	}
}

// A role that resolves to something else is a different invocation, so it is
// checked separately: a reviewer on another model under scoped permissions is
// the shipped example.
func TestPreflightChecksEachDistinctConfiguration(t *testing.T) {
	repo := testRepo(t)
	cfg := withReview(fakeCfg(1, 0))
	e := &Engine{RepoRoot: repo, Cfg: cfg}

	groups := e.preflightGroups()
	if len(groups) != 2 {
		t.Fatalf("%d group(s) for a run with a reviewer on its own model: %v", len(groups), groups)
	}
	if got := groups[0].names(); !has(got, "worker") || !has(got, "integrator") {
		t.Errorf("the first group is %v, want the roles that share the default configuration", got)
	}
	if got := groups[1].names(); len(got) != 1 || got[0] != "reviewer" {
		t.Errorf("the second group is %v, want the reviewer alone", got)
	}
}

// The check costs a model call per configuration, so it can be turned off — and
// turning it off must not turn off anything else.
func TestSkipPreflightRunsTheDrainAnyway(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	model := fake.New(fake.Step{Text: "done", Do: steps(commitWork("t-1.txt"), closes(iss, "t-1"))})
	model.PreflightErr = errors.New("this backend must never be asked")
	defer fake.Install(model)()

	e := &Engine{
		RepoRoot: repo, Cfg: fakeCfg(1, 0), BD: iss, Bus: NewBus(collector()),
		SkipPreflight: true,
		Prompt:        func(r runner.Role) (string, error) { return "system prompt", nil },
		Sleep:         func(context.Context, time.Duration) error { return nil },
		Backoff:       func(int) time.Duration { return 0 },
	}
	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(model.Preflights()) != 0 {
		t.Error("the backend was checked with the preflight turned off")
	}
	if got := outcomeOf(t, rep, "t-1"); got.Outcome != OutcomeDone {
		t.Fatalf("t-1 came back %s (%s)", got.Outcome, got.Reason)
	}
}

// Preflight is exported, so a caller may run it before handing the engine a
// scope. Paying for it twice would be a silent tax on doing that.
func TestPreflightIsNotPaidForTwice(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	model := fake.New(fake.Step{Text: "done", Do: steps(commitWork("t-1.txt"), closes(iss, "t-1"))})
	defer fake.Install(model)()

	e := &Engine{
		RepoRoot: repo, Cfg: fakeCfg(1, 0), BD: iss, Bus: NewBus(collector()),
		Prompt:  func(r runner.Role) (string, error) { return "system prompt", nil },
		Sleep:   func(context.Context, time.Duration) error { return nil },
		Backoff: func(int) time.Duration { return 0 },
	}
	if err := e.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := model.Preflights(); len(got) != 1 {
		t.Errorf("%d preflights, want the one that was already paid for: %v", len(got), got)
	}
}

// A backend that offers no preflight has not failed one. Every adapter but the
// shipped two is in this case, and so is the fake before it grew one.
func TestABackendWithNoPreflightIsNotAFailedPreflight(t *testing.T) {
	if desc, err := runner.Preflight(context.Background(), uncheckable{}, t.TempDir()); err != nil || desc != "" {
		t.Fatalf("Preflight = %q, %v; want nothing checked and no failure", desc, err)
	}
}

// Billing authorization is deliberately above the optional preflight. With an
// API-backed Codex configuration, --no-preflight must still refuse before run
// state, worktrees, claims, or model processes exist.
func TestNoPreflightCannotBypassAPIBillingConsent(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")
	t.Setenv("CODEX_API_KEY", "sk-test")
	cfg := testCfg(1, 0)
	cfg.Runners = map[string]config.RunnerSpec{config.RoleDefault: {Provider: config.CodexProvider}}
	e := &Engine{RepoRoot: repo, Cfg: cfg, BD: iss, SkipPreflight: true}
	e.NewRunner = func(runner.Role, runner.Spec) (runner.Runner, error) {
		return billingRunner{source: runner.BillingAPIKey}, nil
	}

	_, err := e.Drain(context.Background(), DrainOptions{Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1})
	var refusal *BillingError
	if !errors.As(err, &refusal) || refusal.Source != runner.BillingAPIKey {
		t.Fatalf("Drain error = %v, want API billing refusal", err)
	}
	if !strings.Contains(err.Error(), "--allow-api-billing") {
		t.Fatalf("refusal does not name the exact rerun flag: %v", err)
	}
	if _, err := os.Stat(worktree.Path(repo, "t-1")); err == nil {
		t.Fatal("billing refusal created a worktree")
	}
	if _, err := runstate.Load(repo); err == nil {
		t.Fatal("billing refusal wrote run state")
	}
}

func TestBillingChecksSharedConfigurationOnceAndConsentContinues(t *testing.T) {
	cfg := withReview(testCfg(1, 0))
	cfg.Runners = map[string]config.RunnerSpec{config.RoleDefault: {Provider: config.CodexProvider}}
	checks := 0
	var logged []string
	e := &Engine{
		RepoRoot: t.TempDir(), Cfg: cfg, AllowAPIBilling: true,
		NewRunner: func(_ runner.Role, _ runner.Spec) (runner.Runner, error) {
			return billingRunner{checks: &checks, source: runner.BillingAPIKey}, nil
		},
		Log: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}
	if err := e.AuthorizeBilling(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.AuthorizeBilling(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checks != 2 {
		t.Fatalf("%d billing checks, want one for shared worker/integrator and one for reviewer", checks)
	}
	if joined := strings.Join(logged, "\n"); !strings.Contains(joined, "billing warning") || !strings.Contains(joined, "--allow-api-billing") {
		t.Fatalf("consented API billing warning is not clear: %q", joined)
	}
}

func TestChatGPTBillingNeedsNoConsent(t *testing.T) {
	cfg := testCfg(1, 0)
	cfg.Runners = map[string]config.RunnerSpec{config.RoleDefault: {Provider: config.CodexProvider}}
	e := &Engine{
		RepoRoot: t.TempDir(), Cfg: cfg,
		NewRunner: func(runner.Role, runner.Spec) (runner.Runner, error) {
			return billingRunner{source: runner.BillingChatGPTPlan}, nil
		},
	}
	if err := e.AuthorizeBilling(context.Background()); err != nil {
		t.Fatalf("ChatGPT plan was refused without a flag: %v", err)
	}
}

type billingRunner struct {
	checks *int
	source runner.BillingSource
}

func (billingRunner) Name() string              { return "billing-test" }
func (billingRunner) Caps() runner.Capabilities { return runner.Capabilities{} }
func (billingRunner) Run(context.Context, runner.Request, runner.EventSink) (runner.Result, error) {
	return runner.Result{Class: runner.ClassOK}, nil
}
func (b billingRunner) BillingSource(context.Context, string) (runner.BillingSource, error) {
	if b.checks != nil {
		*b.checks++
	}
	return b.source, nil
}

func TestSpecKeyIncludesCodexSettingsAndStableToolOrder(t *testing.T) {
	base := runner.Spec{Provider: "codex", Model: "gpt-5.6-sol", Sandbox: "workspace-write", ApprovalPolicy: "never", Shell: true}
	if specKey(base) != specKey(base) {
		t.Fatal("the same spec must have the same key")
	}
	for _, changed := range []runner.Spec{
		{Provider: "codex", Model: "gpt-5.6-sol", Sandbox: "read-only", ApprovalPolicy: "never", Shell: true},
		{Provider: "codex", Model: "gpt-5.6-sol", Sandbox: "workspace-write", ApprovalPolicy: "on-request", Shell: true},
		{Provider: "codex", Model: "gpt-5.6-sol", Sandbox: "workspace-write", ApprovalPolicy: "never", WebSearch: true},
		{Provider: "codex", Model: "gpt-5.6-sol", Sandbox: "workspace-write", ApprovalPolicy: "never", Shell: true, ViewImage: true},
	} {
		if specKey(base) == specKey(changed) {
			t.Fatalf("Codex setting was omitted from spec key: %+v", changed)
		}
	}
}

// uncheckable is a Runner and nothing more.
type uncheckable struct{}

func (uncheckable) Name() string              { return "uncheckable" }
func (uncheckable) Caps() runner.Capabilities { return runner.Capabilities{} }
func (uncheckable) Run(context.Context, runner.Request, runner.EventSink) (runner.Result, error) {
	return runner.Result{Class: runner.ClassOK}, nil
}
