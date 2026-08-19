package drain

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/config"
	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
)

// withHook hangs one hook at a point.
func withHook(cfg *config.Config, p config.HookPoint, h config.Hook) *config.Config {
	if h.Timeout <= 0 {
		h.Timeout = config.DefaultHookTimeout
	}
	switch p {
	case config.HookIssueEnd:
		cfg.Hooks.OnIssueEnd = append(cfg.Hooks.OnIssueEnd, h)
	case config.HookBarrier:
		cfg.Hooks.OnBarrier = append(cfg.Hooks.OnBarrier, h)
	case config.HookRunEnd:
		cfg.Hooks.OnRunEnd = append(cfg.Hooks.OnRunEnd, h)
	}
	return cfg
}

// copyReport is a hook body that keeps what it was handed, so a test can read
// the input contract rather than infer it.
func copyReport(dest string) string {
	return "cat \"$BD_REPORT_FILE\" > " + dest
}

// appendReport is copyReport for a point that fires more than once in a run.
// Continuous scheduling gives a run a barrier per issue as it lands and a
// settling barrier at the end, so a hook that overwrites keeps only the last of
// them. The file it builds is concatenated report JSON, which readReports
// decodes back into one per firing.
func appendReport(dest string) string {
	return "cat \"$BD_REPORT_FILE\" >> " + dest
}

// readReports decodes every report appendReport collected, in the order the
// hook was fired.
func readReports[T any](t *testing.T, path string) []T {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the hook was handed no report: %v", err)
	}
	defer f.Close()
	var out []T
	dec := json.NewDecoder(f)
	for {
		var one T
		if err := dec.Decode(&one); err == io.EOF {
			return out
		} else if err != nil {
			t.Fatalf("the input is not report JSON: %v", err)
		}
		out = append(out, one)
	}
}

func hookNamed(t *testing.T, rs []HookResult, name string) HookResult {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no hook result named %q in %+v", name, rs)
	return HookResult{}
}

// The input contract: a hook is handed a file of the report bd-auto already
// publishes, not a shape invented for hooks. A hook written against `--json`
// output therefore keeps working, and cannot drift from what the run says it
// did.
func TestAnIssueHookIsHandedTheIssueReportAsPublishedJSON(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	dest := filepath.Join(t.TempDir(), "seen.json")

	cfg := withHook(testCfg(3, 0), config.HookIssueEnd,
		config.Hook{Name: "keep", Run: copyReport(dest)})
	worker := fake.New(closeAndCommit(iss, "t-1", "a.txt"))

	e := engine(t, repo, cfg, iss, worker, pass())
	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("the issue is %s (%s)", rep.Outcome, rep.Reason)
	}

	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the hook was handed no report: %v", err)
	}
	var seen Report
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatalf("the hook's input is not a Report: %v\n%s", err, raw)
	}
	if seen.Issue != "t-1" || seen.Outcome != OutcomeDone {
		t.Fatalf("the hook read issue %q outcome %q", seen.Issue, seen.Outcome)
	}
	if len(seen.Attempts) == 0 {
		t.Fatal("the hook's report has no attempts; it is not the issue's own report")
	}

	got := hookNamed(t, rep.Hooks, "keep")
	if !got.OK || got.Point != string(config.HookIssueEnd) || got.Issue != "t-1" {
		t.Fatalf("the result reached the report wrong: %+v", got)
	}
	if got.Input == "" || !exists(got.Input) {
		t.Fatalf("the report does not name the input the hook read: %+v", got)
	}
}

// The authority rule, at the point it would be broken. A hook that fails —
// exits non-zero, says the work is wrong, anything — changes nothing about the
// issue. Advisory is what makes an unreviewed prompt safe to put here at all.
func TestAFailingHookIsRecordedAndChangesNothing(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	cfg := withHook(testCfg(3, 0), config.HookIssueEnd,
		config.Hook{Name: "objects", Run: "echo 'this issue is wrong' >&2; exit 3"})

	e := engine(t, repo, cfg, iss, newIssuesWorker(iss, "t-1"), pass())
	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if rep.Outcome != OutcomeDone {
		t.Fatalf("a failing hook changed the verdict to %s (%s)", rep.Outcome, rep.Reason)
	}
	if _, parked, _ := iss.snapshot(); len(parked) != 0 {
		t.Fatalf("a failing hook parked %v", parked)
	}
	got := hookNamed(t, rep.Hooks, "objects")
	if got.OK || got.ExitCode != 3 {
		t.Fatalf("the failure was not recorded: %+v", got)
	}
	if !strings.Contains(got.Output, "this issue is wrong") {
		t.Fatalf("what the hook said did not reach the report: %+v", got)
	}
}

// A hook cannot hang a run. There is no timeout: 0 here — unlimited is exactly
// what this promise excludes — so a hook that will not finish is stopped and
// recorded, and the run carries on.
func TestAHookThatWillNotFinishIsStoppedAndTheRunCarriesOn(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	cfg := withHook(testCfg(3, 0), config.HookIssueEnd,
		config.Hook{Name: "hangs", Run: "sleep 30", Timeout: 1})

	e := engine(t, repo, cfg, iss, newIssuesWorker(iss, "t-1"), pass())
	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("a hanging hook changed the verdict to %s (%s)", rep.Outcome, rep.Reason)
	}
	got := hookNamed(t, rep.Hooks, "hangs")
	if !got.TimedOut || got.OK {
		t.Fatalf("the hook was not stopped: %+v", got)
	}
	if got.Seconds > 20 {
		t.Fatalf("the hook ran for %.1fs; its timeout was 1s", got.Seconds)
	}
}

// An interrupt is not a result, so there is nothing to interpret. Firing here
// would spawn a hook into a context that is already cancelled and record it as
// interrupted too — noise on a report about a run that has not finished
// happening.
func TestNoHookFiresForAnOutcomeThatIsNotAVerdict(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	marker := filepath.Join(t.TempDir(), "fired")
	cfg := withHook(testCfg(3, 0), config.HookIssueEnd,
		config.Hook{Name: "never", Run: "touch " + marker})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e := engine(t, repo, cfg, iss, newIssuesWorker(iss, "t-1"), pass())
	rep, err := e.Issue(ctx, "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeInterrupted {
		t.Fatalf("the issue is %s, not interrupted; this test is not testing what it says", rep.Outcome)
	}
	if len(rep.Hooks) != 0 {
		t.Fatalf("hooks ran for an interrupted issue: %+v", rep.Hooks)
	}
	if exists(marker) {
		t.Fatal("a hook fired for an issue nothing reached a verdict on")
	}
}

// What an agent hook is told, and what it is not asked for.
//
// The task has to carry three things the agent file cannot: where the report
// is, that the hook is advisory, and the one issue it may write to. The last
// one is the one-writer rule at the only point that runs beside live siblings.
func TestAnAgentHookIsToldWhereTheReportIsAndWhatItMayNotTouch(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	cfg := withHook(testCfg(3, 0), config.HookIssueEnd,
		config.Hook{Name: "read-it", Agent: "reviewer"})

	hook := fake.New(fake.Step{Text: "nothing worth filing"})
	e := engine(t, repo, cfg, iss, newIssuesWorker(iss, "t-1"), hook)
	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	reqs := hook.Requests()
	if len(reqs) == 0 {
		t.Fatal("the agent hook never ran")
	}
	p := reqs[len(reqs)-1].Prompt
	for _, want := range []string{
		HookInputPath(repo, config.HookIssueEnd, "t-1"),
		"advisory", "t-1", "Run git",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("the hook's task never says %q:\n%s", want, p)
		}
	}
	if dir := reqs[len(reqs)-1].Dir; dir != repo {
		t.Fatalf("the hook ran in %s, not the main checkout %s", dir, repo)
	}
	got := hookNamed(t, rep.Hooks, "read-it")
	if !got.OK || got.Role != "reviewer" || got.Output != "nothing worth filing" {
		t.Fatalf("what the agent said did not reach the report: %+v", got)
	}
}

// No verdict is parsed from a hook. An agent hook whose reply happens to open
// with a failing verdict line — which is exactly what a role falling back to the
// reviewer's prompt will produce — must not fail the issue it was reading about.
func TestAnAgentHookVerdictIsNotReadAsOne(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	cfg := withHook(testCfg(3, 0), config.HookIssueEnd,
		config.Hook{Name: "judgey", Agent: "reviewer"})

	hook := fake.New(fake.Step{Text: "VERDICT: fail\nI would have done this differently."})
	e := engine(t, repo, cfg, iss, newIssuesWorker(iss, "t-1"), hook)
	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("a hook's verdict line failed the issue: %s (%s)", rep.Outcome, rep.Reason)
	}
	if got := hookNamed(t, rep.Hooks, "judgey"); !got.OK {
		t.Fatalf("a hook that said VERDICT: fail was recorded as not having completed: %+v", got)
	}
}

// A hook's cost is the run's cost. It cannot decide anything, but it spends,
// and a report that hid the spend would make hooks look free.
func TestAHooksUsageReachesTheIssuesTotal(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	cfg := withHook(testCfg(3, 0), config.HookIssueEnd,
		config.Hook{Name: "costly", Agent: "reviewer"})

	hook := fake.New(fake.Step{Text: "read it", Usage: runner.Usage{CostUSD: 0.25}})
	e := engine(t, repo, cfg, iss, newIssuesWorker(iss, "t-1"), hook)
	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got := hookNamed(t, rep.Hooks, "costly"); got.Usage.CostUSD != 0.25 {
		t.Fatalf("the hook's own cost is %v", got.Usage)
	}
	if rep.Usage.CostUSD < 0.25 {
		t.Fatalf("the issue's total %v does not include what its hook spent", rep.Usage)
	}
}

// The barrier's hook gets the barrier's report, at a point where every merge,
// park and gate verdict in it is already settled and no worker is live.
func TestABarrierHookGetsTheIntegrateReport(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")
	dest := filepath.Join(t.TempDir(), "barrier.json")

	cfg := withHook(withGate(testCfg(1, 0), "build", "true"), config.HookBarrier,
		config.Hook{Name: "triage", Run: appendReport(dest)})

	e := drainEngine(t, repo, cfg, iss, fake.New(closeAndCommit(iss, "t-1", "a.txt")), pass())
	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("run outcome %s (%s)", rep.Outcome, rep.Reason)
	}

	// A continuous run fires this point at every barrier: one per issue as it
	// lands, and the settling barrier at the end, which merges nothing. Every
	// one of them is a finished barrier, and one of them carries the merge.
	seen := readReports[IntegrateReport](t, dest)
	if len(seen) == 0 {
		t.Fatal("the barrier hook was handed no report")
	}
	merged := false
	for _, in := range seen {
		if !in.GatePassed {
			t.Fatalf("the barrier hook read a report with no gate verdict in it: %+v", in)
		}
		merged = merged || len(in.Merges) > 0
	}
	if !merged {
		t.Fatalf("no barrier the hook saw had merged anything: %+v", seen)
	}
	if len(rep.Integrations) == 0 || len(rep.Integrations[0].Hooks) == 0 {
		t.Fatalf("the barrier's hook result is not on the run's report: %+v", rep.Integrations)
	}
}

// The run's hook runs after the handoff, so what it reads is the whole run
// including where it was handed over — and cannot be mistaken for something
// that could have stopped it.
func TestARunEndHookGetsTheWholeRunIncludingTheHandoff(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")
	dest := filepath.Join(t.TempDir(), "run.json")

	cfg := withHook(testCfg(1, 0), config.HookRunEnd,
		config.Hook{Name: "summarise", Run: copyReport(dest)})

	e := drainEngine(t, repo, cfg, iss, fake.New(closeAndCommit(iss, "t-1", "a.txt")), pass())
	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the run hook was handed no report: %v", err)
	}
	var seen DrainReport
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatalf("the input is not a DrainReport: %v\n%s", err, raw)
	}
	if seen.Outcome != OutcomeDone || len(seen.Issues) == 0 {
		t.Fatalf("the run hook read a report of no run: %+v", seen)
	}
	if seen.Handoff == nil {
		t.Fatal("the run hook ran before the handoff; it cannot say where the run went")
	}
	if len(rep.Hooks) == 0 || !rep.Hooks[0].OK {
		t.Fatalf("the run hook's result is not on the run's report: %+v", rep.Hooks)
	}
}

// A repo with no hooks pays nothing for them: no directory, no file, no
// difference to a report.
func TestARepoWithNoHooksIsUntouched(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	e := engine(t, repo, testCfg(3, 0), iss, newIssuesWorker(iss, "t-1"), pass())
	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(rep.Hooks) != 0 {
		t.Fatalf("hooks appeared in a repo that configured none: %+v", rep.Hooks)
	}
	if exists(filepath.Dir(HookInputPath(repo, config.HookIssueEnd, "t-1"))) {
		t.Fatal("the hooks directory was created for a repo with no hooks")
	}
}

// newIssuesWorker is the whole of a successful worker for one issue.
func newIssuesWorker(iss *fakeIssues, id string) *fake.Runner {
	return fake.New(closeAndCommit(iss, id, id+".txt"))
}
