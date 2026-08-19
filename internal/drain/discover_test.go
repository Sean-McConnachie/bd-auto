package drain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/bd"
	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
)

// --- harness ---

// discovers is a scripted worker step that writes a discoveries file, the way a
// real worker is told to in step 6 of its prompt. It writes to the path the
// engine puts in the task rather than one the test invents, so a change to
// either side breaks this rather than silently passing.
func discovers(repo, issue, body string) func(context.Context, runner.Request) error {
	return func(_ context.Context, _ runner.Request) error {
		path := DiscoveriesPath(repo, issue)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(body), 0o644)
	}
}

// discoveryOfTitle finds a filed create request by title.
func discoveryOfTitle(t *testing.T, created []bd.NewIssue, title string) bd.NewIssue {
	t.Helper()
	for _, n := range created {
		if n.Title == title {
			return n
		}
	}
	t.Fatalf("nothing was filed with the title %q; filed: %+v", title, created)
	return bd.NewIssue{}
}

// --- the worker's half ---

// TestTheWorkerIsToldWhereToWriteWhatItFound. The path is in the task rather
// than in the role prompt because it names an issue, and a role prompt is one
// string shared by every worker in the run.
func TestTheWorkerIsToldWhereToWriteWhatItFound(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")
	worker := fake.New(fake.Step{Text: "done", Do: steps(commitWork("a.txt"), closes(iss, "t-1"))})

	e := engine(t, repo, testCfg(3, 0), iss, worker, pass())
	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	want := DiscoveriesPath(repo, "t-1")
	if p := worker.Requests()[0].Prompt; !strings.Contains(p, want) {
		t.Fatalf("the task does not name the discoveries path %s:\n%s", want, p)
	}
	if !filepath.IsAbs(want) {
		t.Fatalf("the discoveries path %s is relative; the worker's working directory is a worktree elsewhere", want)
	}
	// Outside the worktree, so no `git add -A` can sweep it onto the branch.
	if wt := worker.Requests()[0].Dir; strings.HasPrefix(want, wt+string(filepath.Separator)) {
		t.Fatalf("the discoveries file %s is inside the worktree %s", want, wt)
	}
}

// TestAWorkerDiscoveryReachesRunStateNotBd is the first half of the change: a
// worker writes a file and nothing is filed yet. Filing during the issue is
// what produced this repo's duplicate pair, so "not yet" is the property.
func TestAWorkerDiscoveryReachesRunStateNotBd(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	worker := fake.New(fake.Step{
		Text: "done",
		Do: steps(commitWork("a.txt"), closes(iss, "t-1"),
			discovers(repo, "t-1", `[{"title":"The retry loop leaks a worktree","description":"Seen in x.go:40."}]`)),
	})

	e := engine(t, repo, testCfg(3, 0), iss, worker, pass())
	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if got := iss.createdIssues(); len(got) != 0 {
		t.Fatalf("the issue run filed %d issue(s) in bd; the barrier files them: %+v", len(got), got)
	}

	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	pending := st.PendingDiscoveries()
	if len(pending) != 1 {
		t.Fatalf("run state holds %d discover(ies), want 1: %+v", len(pending), pending)
	}
	if pending[0].From != "t-1" {
		t.Fatalf("discovery came from %q, want t-1", pending[0].From)
	}
}

// TestTheDiscoveriesFileIsConsumed keeps an attempt's findings its own. Left in
// place, the next attempt on the same issue re-harvests the last one's and a
// resumed run re-harvests every attempt it ever ran.
func TestTheDiscoveriesFileIsConsumed(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	worker := fake.New(fake.Step{
		Text: "done",
		Do: steps(commitWork("a.txt"), closes(iss, "t-1"),
			discovers(repo, "t-1", `[{"title":"Something","description":"Somewhere."}]`)),
	})

	e := engine(t, repo, testCfg(3, 0), iss, worker, pass())
	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if exists(DiscoveriesPath(repo, "t-1")) {
		t.Fatal("the discoveries file survived the attempt that wrote it")
	}
}

// TestAFailedAttemptStillReportsWhatItFound. The exploration happened whatever
// the verdict on the code was, and an attempt stopped by the environment did
// nothing wrong at all.
func TestAFailedAttemptStillReportsWhatItFound(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	// Commits and discovers, but never closes the issue: the attempt fails.
	worker := fake.New(fake.Step{
		Text: "did not finish",
		Do: steps(commitWork("a.txt"),
			discovers(repo, "t-1", `[{"title":"Found on the way down","description":"Real all the same."}]`)),
	})

	e := engine(t, repo, testCfg(1, 0), iss, worker, pass())
	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeParked {
		t.Fatalf("outcome %s, want parked; this test needs a failed attempt", rep.Outcome)
	}

	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PendingDiscoveries()) != 1 {
		t.Fatalf("a failed attempt lost its findings: %+v", st.Discovered)
	}
}

// TestOnlyUsableEntriesAreKept. A title with nothing under it is a note to
// nobody, and the bar the prompt sets — would a human schedule this — cannot be
// met by a line of text.
func TestOnlyUsableEntriesAreKept(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	body := `[
	  {"title":"Keeps this one","description":"Because it has both.","type":"bug","priority":"P1"},
	  {"title":"No description"},
	  {"description":"No title."},
	  {"title":"Bad type and priority","description":"Kept, with bd's defaults.","type":"catastrophe","priority":"urgent"}
	]`
	worker := fake.New(fake.Step{
		Text: "done",
		Do:   steps(commitWork("a.txt"), closes(iss, "t-1"), discovers(repo, "t-1", body)),
	})

	e := engine(t, repo, testCfg(3, 0), iss, worker, pass())
	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := st.PendingDiscoveries()
	if len(got) != 2 {
		t.Fatalf("kept %d entr(ies), want 2: %+v", len(got), got)
	}
	if got[0].Type != "bug" || got[0].Priority != "1" {
		t.Fatalf("type %q priority %q, want bug and 1", got[0].Type, got[0].Priority)
	}
	// An invented type or priority costs the field, never the finding.
	if got[1].Type != "" || got[1].Priority != "" {
		t.Fatalf("kept an invented type %q / priority %q instead of bd's defaults", got[1].Type, got[1].Priority)
	}
}

// TestAnUnreadableDiscoveriesFileCostsOnlyTheFindings. The attempt is already
// over by the time this is read; failing it here would throw away finished work
// over a malformed file.
func TestAnUnreadableDiscoveriesFileCostsOnlyTheFindings(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1")

	worker := fake.New(fake.Step{
		Text: "done",
		Do:   steps(commitWork("a.txt"), closes(iss, "t-1"), discovers(repo, "t-1", `{"discovered": [ NOT JSON`)),
	})

	e := engine(t, repo, testCfg(3, 0), iss, worker, pass())
	rep, err := e.Issue(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rep.Outcome != OutcomeDone {
		t.Fatalf("outcome %s, want done; a bad discoveries file must not fail the issue", rep.Outcome)
	}
	st, _ := runstate.Load(repo)
	if len(st.Discovered) != 0 {
		t.Fatalf("kept something out of an unparseable file: %+v", st.Discovered)
	}
}

// TestBothFileShapesAreAccepted. "Write a list" gets a bare array from some
// models and an object wrapping one from others, and spending a round teaching
// a worker the difference would buy nothing.
func TestBothFileShapesAreAccepted(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"bare array", `[{"title":"A","description":"a"}]`},
		{"wrapped", `{"discovered":[{"title":"A","description":"a"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testRepo(t)
			iss := newIssues("t-1")
			worker := fake.New(fake.Step{
				Text: "done",
				Do:   steps(commitWork("a.txt"), closes(iss, "t-1"), discovers(repo, "t-1", tc.body)),
			})
			e := engine(t, repo, testCfg(3, 0), iss, worker, pass())
			if _, err := e.Issue(context.Background(), "t-1"); err != nil {
				t.Fatalf("Issue: %v", err)
			}
			st, _ := runstate.Load(repo)
			if len(st.PendingDiscoveries()) != 1 {
				t.Fatalf("%s was not accepted: %+v", tc.name, st.Discovered)
			}
		})
	}
}

// --- the barrier's half ---

// seedDiscoveries puts a finished wave and some pending discoveries in run
// state, which is what a barrier finds when the issues before it have run.
func seedDiscoveries(t *testing.T, repo, epic string, done []string, ds ...runstate.Discovery) {
	t.Helper()
	st := runstate.New(epic, len(done), "auto", 0)
	st.WaveIssues = append([]string(nil), done...)
	for _, id := range done {
		st.MarkDone(id)
	}
	for _, d := range ds {
		st.AddDiscovery(d)
	}
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}
}

// TestTheBarrierFilesDiscoveriesDeferredAndLinked is the filing contract: the
// deferral is what keeps discovered work out of the next run, the
// discovered-from dependency is what says where it came from, and the label is
// what tells a run's backlog from a human's.
func TestTheBarrierFilesDiscoveriesDeferredAndLinked(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	// This is the filing contract, so the mode is named rather than inherited:
	// the default is triage, under which a barrier files nothing at all.
	cfg.DiscoveredWork = "defer"
	iss := newIssues("t-1").under("epic-1", "t-1")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	seedDiscoveries(t, repo, "epic-1", []string{"t-1"}, runstate.Discovery{
		From: "t-1", Title: "The gate does not cover x", Description: "Nothing runs x_test.go.",
		Type: "task", Priority: "2",
	})

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	if len(rep.Discoveries.Filed) != 1 {
		t.Fatalf("filed %+v, want exactly one issue", rep.Discoveries.Filed)
	}
	n := discoveryOfTitle(t, iss.createdIssues(), "The gate does not cover x")
	if n.Defer != DiscoveredDefer {
		t.Fatalf("filed with defer %q, want %q; undeferred, it joins the next run's ready front",
			n.Defer, DiscoveredDefer)
	}
	if want := "discovered-from:t-1"; len(n.Deps) != 1 || n.Deps[0] != want {
		t.Fatalf("filed with deps %v, want [%s]", n.Deps, want)
	}
	if len(n.Labels) != 1 || n.Labels[0] != DiscoveredLabel {
		t.Fatalf("filed with labels %v, want [%s]", n.Labels, DiscoveredLabel)
	}
	if !strings.Contains(n.Description, "Nothing runs x_test.go.") {
		t.Fatalf("the worker's description did not survive: %q", n.Description)
	}
	if !strings.Contains(n.Description, "t-1") {
		t.Fatalf("the description does not say which issue found it: %q", n.Description)
	}

	// And it is recorded, so a second barrier does not file it again.
	st, _ := runstate.Load(repo)
	if got := st.PendingDiscoveries(); len(got) != 0 {
		t.Fatalf("%d discover(ies) still pending after being filed: %+v", len(got), got)
	}
	if got := st.FiledDiscoveries(); len(got) != 1 {
		t.Fatalf("run state records %v as filed, want one issue", got)
	}
}

// TestTheSameFindingFromTwoWorkersIsFiledOnce is the duplicate this change
// exists to stop. beads-auto-imp-pzi and beads-auto-imp-6up are exactly this,
// filed three waves apart in almost the same words.
func TestTheSameFindingFromTwoWorkersIsFiledOnce(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	// This is the filing contract, so the mode is named rather than inherited:
	// the default is triage, under which a barrier files nothing at all.
	cfg.DiscoveredWork = "defer"
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	finishedWorker(t, repo, cfg, "t-2", "b.txt", "b\n")
	iss.set("t-1", "closed")
	iss.set("t-2", "closed")

	// The same fault, described by two workers with the spacing and case that
	// two independent writers actually differ by.
	seedDiscoveries(t, repo, "epic-1", []string{"t-1", "t-2"},
		runstate.Discovery{From: "t-1", Title: "Drop the plugin-era role aliases", Description: "In runners.go."},
		runstate.Discovery{From: "t-2", Title: "drop the  plugin-era role aliases.", Description: "Also found in runners.go."},
	)

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if len(rep.Discoveries.Filed) != 1 {
		t.Fatalf("filed %+v; the same finding from two workers is one issue", rep.Discoveries.Filed)
	}
	if got := iss.createdIssues(); len(got) != 1 {
		t.Fatalf("bd was asked to create %d issues, want 1: %+v", len(got), got)
	}
}

// TestAFindingBdAlreadyHasIsNotFiledAgain covers what run state cannot see: an
// issue filed by an earlier run, or by a human, or filed and already fixed.
// Closed issues count — otherwise every run re-files what the last one closed.
func TestAFindingBdAlreadyHasIsNotFiledAgain(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	// This is the filing contract, so the mode is named rather than inherited:
	// the default is triage, under which a barrier files nothing at all.
	cfg.DiscoveredWork = "defer"
	iss := newIssues("t-1").under("epic-1", "t-1")
	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")

	// Already in bd, and already closed.
	iss.status["old-1"] = "closed"
	iss.titles["old-1"] = "Preflight the claude CLI before a drain spends anything"

	seedDiscoveries(t, repo, "epic-1", []string{"t-1"},
		runstate.Discovery{
			From:        "t-1",
			Title:       "preflight the claude CLI before a drain spends anything",
			Description: "Nothing checks the CLI exists before the first worker is spawned.",
		},
		runstate.Discovery{From: "t-1", Title: "Genuinely new", Description: "Nothing like it in bd."},
	)

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if rep.Discoveries.Skipped != 1 {
		t.Fatalf("skipped %d, want 1", rep.Discoveries.Skipped)
	}
	if len(rep.Discoveries.Filed) != 1 {
		t.Fatalf("filed %+v, want only the new one", rep.Discoveries.Filed)
	}
	discoveryOfTitle(t, iss.createdIssues(), "Genuinely new")

	// The skip is recorded with its reason, so a human can see what the run
	// decided not to file rather than only what it did.
	st, _ := runstate.Load(repo)
	for _, d := range st.Discovered {
		if strings.EqualFold(d.Title, "preflight the claude CLI before a drain spends anything") {
			if d.FiledAs != "" || !strings.Contains(d.Skipped, "old-1") {
				t.Fatalf("skip recorded as filed=%q skipped=%q", d.FiledAs, d.Skipped)
			}
		}
	}
}

// TestABarrierThatCannotReadBdFilesNothing. A duplicate issue costs a human's
// attention for good; a discovery filed one barrier later costs nothing.
func TestABarrierThatCannotReadBdFilesNothing(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	// This is the filing contract, so the mode is named rather than inherited:
	// the default is triage, under which a barrier files nothing at all.
	cfg.DiscoveredWork = "defer"
	iss := newIssues("t-1").under("epic-1", "t-1")
	seedDiscoveries(t, repo, "epic-1", []string{"t-1"},
		runstate.Discovery{From: "t-1", Title: "Something", Description: "Somewhere."})

	iss.fail = errors.New("bd is unreachable")
	e := engine(t, repo, cfg, iss, fake.New(), fake.New())

	got := e.fileDiscoveries()
	if !got.Empty() {
		t.Fatalf("filed %+v with bd unreadable", got)
	}
	if n := len(iss.createdIssues()); n != 0 {
		t.Fatalf("created %d issue(s) without being able to check for duplicates", n)
	}
	// Still pending, so the next barrier tries again.
	st, _ := runstate.Load(repo)
	if len(st.PendingDiscoveries()) != 1 {
		t.Fatal("the discovery was dropped rather than left for the next barrier")
	}
}

// TestACreateThatFailsIsReportedAndRetriable. bd refusing one create must not
// cost the finding or stop the ones behind it.
func TestACreateThatFailsIsReportedAndRetriable(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	// This is the filing contract, so the mode is named rather than inherited:
	// the default is triage, under which a barrier files nothing at all.
	cfg.DiscoveredWork = "defer"
	iss := newIssues("t-1").under("epic-1", "t-1")
	seedDiscoveries(t, repo, "epic-1", []string{"t-1"},
		runstate.Discovery{From: "t-1", Title: "Something", Description: "Somewhere."})

	iss.createFail = errors.New("bd refused")
	e := engine(t, repo, cfg, iss, fake.New(), fake.New())

	got := e.fileDiscoveries()
	if got.Failed != 1 || len(got.Filed) != 0 {
		t.Fatalf("filing reported %+v, want one failure and nothing filed", got)
	}
	st, _ := runstate.Load(repo)
	if len(st.PendingDiscoveries()) != 1 {
		t.Fatal("a failed create resolved the discovery anyway, so it can never be retried")
	}
}

// TestDiscoveredWorkImmediateFilesWithoutADeferral. Until the barrier owned the
// filing, discovered_work was a documented, validated config key that nothing
// read: the worker prompt hard-coded the deferral and a repo asking for
// "immediate" got "defer" anyway.
func TestDiscoveredWorkImmediateFilesWithoutADeferral(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	cfg.DiscoveredWork = "immediate"
	iss := newIssues("t-1").under("epic-1", "t-1")
	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	seedDiscoveries(t, repo, "epic-1", []string{"t-1"},
		runstate.Discovery{From: "t-1", Title: "Something", Description: "Somewhere."})

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	if _, err := e.Integrate(context.Background(), IntegrateOptions{}); err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	n := discoveryOfTitle(t, iss.createdIssues(), "Something")
	if n.Defer != "" {
		t.Fatalf("filed with defer %q under discovered_work: immediate", n.Defer)
	}
	// The label is not conditional: it is how a run's backlog is told from a
	// human's, whichever way the deferral went.
	if len(n.Labels) != 1 || n.Labels[0] != DiscoveredLabel {
		t.Fatalf("filed with labels %v, want [%s]", n.Labels, DiscoveredLabel)
	}
}

// TestAnUnsetDiscoveredWorkStillDefers. A Config built in code — a flag, a test
// — never ran Load's defaulting, and of the two ways to be wrong, holding work
// somebody wanted offered is much cheaper than offering work somebody wanted
// held.
func TestAnUnsetDiscoveredWorkStillDefers(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	cfg.DiscoveredWork = ""
	iss := newIssues("t-1").under("epic-1", "t-1")
	seedDiscoveries(t, repo, "epic-1", []string{"t-1"},
		runstate.Discovery{From: "t-1", Title: "Something", Description: "Somewhere."})

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	e.fileDiscoveries()

	if n := discoveryOfTitle(t, iss.createdIssues(), "Something"); n.Defer != DiscoveredDefer {
		t.Fatalf("filed with defer %q, want %q", n.Defer, DiscoveredDefer)
	}
}

// TestABarrierWithNothingToFileTouchesBd keeps the pass honest: the common case
// is a wave that discovered nothing, and it must cost no writes.
func TestABarrierWithNothingToFileTouchesBd(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1").under("epic-1", "t-1")
	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	waveState(t, repo, "epic-1", "t-1")

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	rep, err := e.Integrate(context.Background(), IntegrateOptions{})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if !rep.Discoveries.Empty() {
		t.Fatalf("a wave that discovered nothing reported %+v", rep.Discoveries)
	}
	if n := len(iss.createdIssues()); n != 0 {
		t.Fatalf("created %d issue(s) with nothing to file", n)
	}
}
