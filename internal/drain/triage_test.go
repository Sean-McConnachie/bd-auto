package drain

import (
	"context"
	"os"
	"strings"
	"testing"

	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
)

// TestTheBarrierUnderTriageFilesNothing is the whole point of the gate. The
// count of issues a human reads must not go up because a run finished.
func TestTheBarrierUnderTriageFilesNothing(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	cfg.DiscoveredWork = "triage"
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

	if got := iss.createdIssues(); len(got) != 0 {
		t.Fatalf("the barrier filed %d issue(s) under triage: %+v", len(got), got)
	}
	if rep.Discoveries.Staged != 1 {
		t.Fatalf("reported %+v, want one staged", rep.Discoveries)
	}

	tr, err := LoadTriage(repo)
	if err != nil {
		t.Fatal(err)
	}
	pending := tr.Pending()
	if len(pending) != 1 {
		t.Fatalf("staged %d, want 1: %+v", len(pending), pending)
	}
	if pending[0].From != "t-1" {
		t.Fatalf("staged from %q, want t-1", pending[0].From)
	}
	if pending[0].Title != "The gate does not cover x" {
		t.Fatalf("staged title %q", pending[0].Title)
	}
	if pending[0].Type != "task" || pending[0].Priority != "2" {
		t.Fatalf("staging lost the type or priority: %+v", pending[0])
	}
}

// TestStagingOutlivesTheRunThatFoundIt. Run state is per-run and `run stop`
// clears it; a finding a human has not read must survive both.
func TestStagingOutlivesTheRunThatFoundIt(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	cfg.DiscoveredWork = "triage"
	iss := newIssues("t-1").under("epic-1", "t-1")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	seedDiscoveries(t, repo, "epic-1", []string{"t-1"},
		runstate.Discovery{From: "t-1", Title: "Something real", Description: "In x.go:40."})

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	if _, err := e.Integrate(context.Background(), IntegrateOptions{}); err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	// The run ends and its state goes with it.
	if err := os.Remove(runstate.Path(repo)); err != nil {
		t.Fatal(err)
	}
	tr, err := LoadTriage(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Pending()) != 1 {
		t.Fatalf("the staged finding did not survive the run ending: %+v", tr.Staged)
	}
}

// TestAcceptFilesExactlyOneIssueWithItsProvenance.
func TestAcceptFilesExactlyOneIssueWithItsProvenance(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	cfg.DiscoveredWork = "triage"
	iss := newIssues("t-1")
	e := engine(t, repo, cfg, iss, fake.New(), fake.New())

	tr := stage(t, repo, Staged{Key: "a leaking worktree", From: "t-1",
		Title: "A leaking worktree", Description: "Seen in x.go:40.", Type: "bug", Priority: "1"})

	d, err := e.Accept(tr, "a leaking")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if d.Outcome != "filed" || d.FiledAs == "" {
		t.Fatalf("decision %+v", d)
	}
	created := iss.createdIssues()
	if len(created) != 1 {
		t.Fatalf("filed %d issues, want 1: %+v", len(created), created)
	}
	n := created[0]
	if n.Title != "A leaking worktree" || n.Type != "bug" || n.Priority != "1" {
		t.Fatalf("filed the wrong thing: %+v", n)
	}
	if !hasString(n.Deps, "discovered-from:t-1") {
		t.Fatalf("filed without the discovered-from edge: %+v", n.Deps)
	}
	if !hasString(n.Labels, DiscoveredLabel) {
		t.Fatalf("filed without the %s label: %+v", DiscoveredLabel, n.Labels)
	}
	if !strings.Contains(n.Description, "t-1") {
		t.Fatalf("filed without provenance:\n%s", n.Description)
	}
	// A triaged issue is not deferred: a human just said they want it.
	if n.Defer != "" {
		t.Fatalf("a human accepted this and it was filed deferred until %q", n.Defer)
	}
	if len(tr.Pending()) != 0 {
		t.Fatal("the accepted discovery is still pending")
	}
}

// TestMergeAppendsToAnExistingIssueAndFilesNothing. Most of what did not clear
// the bar was context about something already tracked, and there was no way to
// say so: the only shape available was a new issue.
func TestMergeAppendsToAnExistingIssueAndFilesNothing(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	cfg.DiscoveredWork = "triage"
	iss := newIssues("t-1", "t-2")
	e := engine(t, repo, cfg, iss, fake.New(), fake.New())

	tr := stage(t, repo, Staged{Key: "same thing again", From: "t-1",
		Title: "Same thing again", Description: "Another way in to t-2."})

	d, err := e.Merge(tr, "same thing", "t-2")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if d.Outcome != "merged" || d.MergedInto != "t-2" {
		t.Fatalf("decision %+v", d)
	}
	if got := iss.createdIssues(); len(got) != 0 {
		t.Fatalf("a merge created %d issue(s): %+v", len(got), got)
	}
	note := iss.notesOf("t-2")
	if !strings.Contains(note, "Same thing again") || !strings.Contains(note, "t-1") {
		t.Fatalf("t-2's notes do not carry the finding or where it came from:\n%s", note)
	}
	if len(tr.Pending()) != 0 {
		t.Fatal("the merged discovery is still pending")
	}
}

// TestDiscardNeedsAReason. A discard with no reason cannot be told from a lost
// finding, and the record of what a run decided not to file is the only
// evidence there is that the bar sits in the right place.
func TestDiscardNeedsAReason(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	iss := newIssues("t-1")
	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	tr := stage(t, repo, Staged{Key: "tidier", From: "t-1", Title: "Tidier", Description: "x could be tidier."})

	if _, err := e.Discard(tr, "tidier", "  "); err == nil {
		t.Fatal("discarded with no reason")
	}
	d, err := e.Discard(tr, "tidier", "documented where it lives")
	if err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if d.Outcome != "discarded" || d.Reason == "" {
		t.Fatalf("decision %+v", d)
	}
	if len(tr.Pending()) != 0 {
		t.Fatal("the discarded discovery is still pending")
	}
	// Kept, not deleted.
	if len(tr.Staged) != 1 || tr.Staged[0].Reason == "" {
		t.Fatalf("the discard was not recorded: %+v", tr.Staged)
	}
}

// TestADiscardedFindingIsNeverStagedAgain. Without this, a finding a human has
// already said no to comes back every run that rediscovers it.
func TestADiscardedFindingIsNeverStagedAgain(t *testing.T) {
	repo := testRepo(t)
	tr := stage(t, repo, Staged{Key: "no thanks", From: "t-1", Title: "No thanks",
		Description: "x.", Outcome: "discarded", Reason: "not work"})

	if tr.Add(Staged{Key: "no thanks", From: "t-2", Title: "No thanks", Description: "x."}) {
		t.Fatal("a finding a human already discarded was staged again")
	}
	if len(tr.Pending()) != 0 {
		t.Fatalf("it came back as pending: %+v", tr.Pending())
	}
}

// TestAnAmbiguousKeyIsRefusedRatherThanGuessed. These commands file issues and
// write notes; picking the wrong one of two findings is not something a human
// would notice afterwards.
func TestAnAmbiguousKeyIsRefusedRatherThanGuessed(t *testing.T) {
	repo := testRepo(t)
	tr := stage(t, repo,
		Staged{Key: "the gate is silent on x", From: "t-1", Title: "The gate is silent on x", Description: "a"},
		Staged{Key: "the gate is silent on y", From: "t-2", Title: "The gate is silent on y", Description: "b"},
	)
	if _, err := tr.Find("the gate"); err == nil {
		t.Fatal("an ambiguous prefix resolved to something")
	} else if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("unhelpful error: %v", err)
	}
	if _, err := tr.Find("the gate is silent on y"); err != nil {
		t.Fatalf("an exact key did not resolve: %v", err)
	}
}

// TestTheLookalikeIsRecordedNotActedOn for a match in the reporting band: the
// discovery is still staged, with what it resembles written on it.
func TestTheLookalikeIsRecordedNotActedOn(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(3, 0)
	cfg.DiscoveredWork = "triage"

	// t-9 is in bd already and says almost exactly what the discovery says.
	iss := newIssues("t-1", "t-9").under("epic-1", "t-1")
	iss.describe("t-9", "Concurrent git worktree add races inside .git/worktrees",
		"git worktree add fails with 'failed to read .git/worktrees/t-2/commondir' when a wave "+
			"creates several worktrees at once; internal/worktree needs a lock around add and remove.")

	finishedWorker(t, repo, cfg, "t-1", "a.txt", "a\n")
	iss.set("t-1", "closed")
	seedDiscoveries(t, repo, "epic-1", []string{"t-1"}, runstate.Discovery{
		From:  "t-1",
		Title: "Flaky: concurrent git worktree add fails on another worktree's commondir",
		Description: "git worktree add fails with 'failed to read .git/worktrees/t-2/commondir'. " +
			"A wave creates worktrees concurrently, so internal/worktree needs a lock around add and remove.",
	})

	e := engine(t, repo, cfg, iss, fake.New(), fake.New())
	if _, err := e.Integrate(context.Background(), IntegrateOptions{}); err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	tr, err := LoadTriage(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Staged) != 1 {
		t.Fatalf("staged %d, want 1: %+v", len(tr.Staged), tr.Staged)
	}
	got := tr.Staged[0]
	if got.Resembles != "t-9" {
		t.Fatalf("the lookalike was not recorded: %+v", got)
	}
	if got.Outcome == "" && got.Score < 0.28 {
		t.Fatalf("recorded a lookalike below the hint threshold: %.2f", got.Score)
	}
}

// --- harness ---

func stage(t *testing.T, repo string, ss ...Staged) *Triage {
	t.Helper()
	tr := &Triage{Version: 1}
	for _, s := range ss {
		tr.Staged = append(tr.Staged, s)
	}
	if err := tr.Save(repo); err != nil {
		t.Fatal(err)
	}
	return tr
}

func hasString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
