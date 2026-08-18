package drain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/config"
	"bd-auto/internal/runner/fake"
	"bd-auto/internal/runstate"
)

// commitOnBranch is a human working on the epic branch after the run that built
// it has ended: the second half of "a parked issue somebody unparked and fixed
// by hand".
func commitOnBranch(t *testing.T, repo, file, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, file), []byte("by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "--quiet", "-m", msg)
}

// The plain case: a finished run whose drain opened nothing, handed over
// afterwards by the command. Nothing is forced, because there is nothing to
// force — the run is green and clean, and the only reason it has no pull
// request is that the process that could have opened one is gone.
func TestHandoffOpensThePullRequestAFinishedRunNeverDid(t *testing.T) {
	repo := testRepo(t)
	mainHead := headOf(t, repo, "main")
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	// pr: false is one way to arrive here with a finished run and no pull
	// request; an interrupted drain is the other, and both leave exactly this.
	cfg := withGate(testCfg(1, 0), "build", "true")
	cfg.Handoff.PR = config.No()

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))
	workers.script("t-2", closeAndCommit(iss, "t-2", "two.txt"))

	e := drainEngine(t, repo, cfg, iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"}, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone || len(rep.Parked) > 0 {
		t.Fatalf("the run did not finish clean: %s %v", rep.Outcome, rep.Parked)
	}
	if pushes, opened := forge.calls(); len(pushes) != 0 || len(opened) != 0 {
		t.Fatalf("pr: false published something: pushes=%v prs=%d", pushes, len(opened))
	}
	branch := stagedBranch(t, repo)

	// The human turns the pull request back on and asks for it. A fresh engine,
	// because the one that ran the drain is what a real handoff no longer has.
	on := withGate(testCfg(1, 0), "build", "true")
	later := engine(t, repo, on, iss, fake.New(), fake.New())
	later.Forge = forge

	h, err := later.HandoffFromState(context.Background(), HandoffOptions{})
	if err != nil {
		t.Fatalf("HandoffFromState: %v", err)
	}
	if h.URL != forge.url || !h.Created || h.Forced != "" {
		t.Fatalf("no pull request from a finished run: %+v", h)
	}
	if !h.ByHand {
		t.Fatal("a handoff run by hand must say so; the document it writes is narrower")
	}
	if now := headOf(t, repo, "main"); now != mainHead {
		t.Fatalf("handing over wrote to the base branch: %s -> %s", mainHead, now)
	}

	pushes, opened := forge.calls()
	if len(pushes) != 1 || pushes[0] != "origin "+branch {
		t.Fatalf("pushed %v, want the epic branch once to origin", pushes)
	}
	if len(opened) != 1 {
		t.Fatalf("opened %d pull request(s), want exactly 1", len(opened))
	}
	pr := opened[0]
	if pr.Base != "main" || pr.Head != branch {
		t.Fatalf("pull request is %s <- %s, want main <- %s", pr.Base, pr.Head, branch)
	}
	// Both issues landed and were cleaned up, so neither branch is still in the
	// checkout: the rebuilt report has to find them anyway.
	for _, want := range []string{"t-1", "t-2", "nothing parked", "bd-auto handoff", "Green on the merged result"} {
		if !strings.Contains(pr.Body, want) {
			t.Fatalf("the pull request body never mentions %q:\n%s", want, pr.Body)
		}
	}
	if strings.Contains(pr.Body, "Forced") {
		t.Fatalf("nothing was forced, and the body says it was:\n%s", pr.Body)
	}

	st, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.PR != forge.url {
		t.Fatalf("run state does not record the pull request: %q", st.PR)
	}
}

// The issue this command exists for: a run with a parked issue publishes
// nothing, a human fixes it on the branch, and --force is how that branch
// reaches review. The refusal survives into the pull request, because a review
// request bd-auto would not have made is one the reviewer has to be told about.
func TestHandoffForcesOverAParkedIssueAndSaysSoInThePullRequest(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1", "t-2").under("epic-1", "t-1", "t-2")

	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))
	workers.script("t-2", fake.Step{Text: "I had a think about it"})

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1", "t-2"}, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !has(rep.Parked, "t-2") {
		t.Fatalf("t-2 did nothing and must be parked: %+v", rep.Parked)
	}
	branch := stagedBranch(t, repo)

	later := engine(t, repo, testCfg(1, 0), iss, fake.New(), fake.New())
	later.Forge = forge

	// Asked plainly, the answer is the same one the drain gave.
	h, err := later.HandoffFromState(context.Background(), HandoffOptions{})
	if err != nil {
		t.Fatalf("HandoffFromState: %v", err)
	}
	if h.URL != "" {
		t.Fatalf("a run with parked work was published without --force: %+v", h)
	}
	if !strings.Contains(h.Reason, "t-2") {
		t.Fatalf("the refusal does not name the parked issue: %q", h.Reason)
	}
	if !h.Forceable {
		t.Fatal("a parked issue is a judgement about the run, and --force must be offered for it")
	}
	if pushes, opened := forge.calls(); len(pushes) != 0 || len(opened) != 0 {
		t.Fatalf("a refused handoff published something: pushes=%v prs=%d", pushes, len(opened))
	}

	// The human does t-2 themselves, on the branch, and asks again.
	commitOnBranch(t, repo, "two.txt", "t-2 by hand")

	h, err = later.HandoffFromState(context.Background(), HandoffOptions{Force: true})
	if err != nil {
		t.Fatalf("HandoffFromState --force: %v", err)
	}
	if h.URL != forge.url || !h.Created {
		t.Fatalf("--force opened no pull request: %+v", h)
	}
	if !strings.Contains(h.Forced, "t-2") {
		t.Fatalf("the report does not keep the refusal that was overridden: %q", h.Forced)
	}

	pushes, opened := forge.calls()
	if len(pushes) != 1 || pushes[0] != "origin "+branch {
		t.Fatalf("pushed %v, want the epic branch once to origin", pushes)
	}
	if len(opened) != 1 {
		t.Fatalf("opened %d pull request(s), want exactly 1", len(opened))
	}
	body := opened[0].Body
	for _, want := range []string{"Forced", "--force", "1 PARKED: t-2", "t-1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("a forced pull request must not read like a clean one; no %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "nothing parked") {
		t.Fatalf("the body claims nothing is parked over a parked issue:\n%s", body)
	}
}

// --force overrides a judgement about a run. It does not manufacture a branch,
// and it does not publish one with nothing on it: those refusals are facts, and
// the report says so rather than quietly opening a pull request over an empty
// diff.
func TestForceCannotPublishABranchWithNothingOnIt(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	// The only issue in scope parks, so the branch is minted and stays empty.
	workers := newByIssue()
	workers.script("t-1", fake.Step{Text: "I had a think about it"})

	e := drainEngine(t, repo, testCfg(1, 0), iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	if _, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	}); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	later := engine(t, repo, testCfg(1, 0), iss, fake.New(), fake.New())
	later.Forge = forge

	h, err := later.HandoffFromState(context.Background(), HandoffOptions{Force: true})
	if err != nil {
		t.Fatalf("HandoffFromState --force: %v", err)
	}
	if h.URL != "" {
		t.Fatalf("--force published a branch with nothing on it: %+v", h)
	}
	if h.Forceable {
		t.Fatal("nothing landed, so --force must not be offered as a way past it")
	}
	if !strings.Contains(h.Reason, "--force cannot change that") {
		t.Fatalf("the reason does not say that --force was tried and refused: %q", h.Reason)
	}
	if pushes, opened := forge.calls(); len(pushes) != 0 || len(opened) != 0 {
		t.Fatalf("--force published something over an empty branch: pushes=%v prs=%d", pushes, len(opened))
	}
}

// The gate is re-run rather than remembered, because the whole reason to hand
// over after the fact is that something happened after the fact. A branch a
// human has since broken is a red branch, whatever the run recorded.
func TestHandoffRegatesTheBranchAsItStandsNow(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	cfg := withGate(testCfg(1, 0), "no-bad", "test ! -f bad.txt")
	cfg.Handoff.PR = config.No()
	workers := newByIssue()
	workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))

	e := drainEngine(t, repo, cfg, iss, workers, fake.New())
	forge := newForge()
	e.Forge = forge

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeDone || len(rep.Parked) > 0 {
		t.Fatalf("the run did not finish green: %s %v", rep.Outcome, rep.Parked)
	}

	// A human breaks the gate on the branch after the run has ended.
	commitOnBranch(t, repo, "bad.txt", "something half-finished")

	on := withGate(testCfg(1, 0), "no-bad", "test ! -f bad.txt")
	later := engine(t, repo, on, iss, fake.New(), fake.New())
	later.Forge = forge

	h, err := later.HandoffFromState(context.Background(), HandoffOptions{})
	if err != nil {
		t.Fatalf("HandoffFromState: %v", err)
	}
	if h.URL != "" {
		t.Fatalf("a red branch was published: %+v", h)
	}
	if !strings.Contains(h.Reason, "gate is red") {
		t.Fatalf("the gate was not re-run on the branch as it stands: %q", h.Reason)
	}

	// Forced anyway, the pull request has to lead with it.
	h, err = later.HandoffFromState(context.Background(), HandoffOptions{Force: true})
	if err != nil {
		t.Fatalf("HandoffFromState --force: %v", err)
	}
	if h.URL != forge.url {
		t.Fatalf("--force opened no pull request over a red gate: %+v", h)
	}
	_, opened := forge.calls()
	if len(opened) != 1 {
		t.Fatalf("opened %d pull request(s), want exactly 1", len(opened))
	}
	if !strings.Contains(opened[0].Body, "RED on the merged result") {
		t.Fatalf("a forced pull request over a red gate must say so:\n%s", opened[0].Body)
	}
}

// Two refusals that are not about the work, and so come back as errors rather
// than as a report: there is no epic branch to hand over, or the checkout is
// standing somewhere the gate would prove the wrong tree.
func TestHandoffRefusesWhatIsNotAVerdictOnTheWork(t *testing.T) {
	t.Run("no epic branch", func(t *testing.T) {
		repo := testRepo(t)
		iss := newIssues("t-1").under("epic-1", "t-1")

		cfg := testCfg(1, 0)
		cfg.Handoff.Branch, cfg.Handoff.PR = config.No(), config.No()
		workers := newByIssue()
		workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))

		e := drainEngine(t, repo, cfg, iss, workers, fake.New())
		forge := newForge()
		e.Forge = forge
		if _, err := e.Drain(context.Background(), DrainOptions{
			Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
		}); err != nil {
			t.Fatalf("Drain: %v", err)
		}

		later := engine(t, repo, testCfg(1, 0), iss, fake.New(), fake.New())
		later.Forge = forge
		_, err := later.HandoffFromState(context.Background(), HandoffOptions{Force: true})
		if err == nil || !strings.Contains(err.Error(), "epic branch") {
			t.Fatalf("an unstaged run must refuse by name, got %v", err)
		}
		if pushes, opened := forge.calls(); len(pushes) != 0 || len(opened) != 0 {
			t.Fatalf("an unstaged run published something: pushes=%v prs=%d", pushes, len(opened))
		}
	})

	t.Run("checkout somewhere else", func(t *testing.T) {
		repo := testRepo(t)
		iss := newIssues("t-1").under("epic-1", "t-1")

		cfg := testCfg(1, 0)
		cfg.Handoff.PR = config.No()
		workers := newByIssue()
		workers.script("t-1", closeAndCommit(iss, "t-1", "one.txt"))

		e := drainEngine(t, repo, cfg, iss, workers, fake.New())
		forge := newForge()
		e.Forge = forge
		if _, err := e.Drain(context.Background(), DrainOptions{
			Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
		}); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		branch := stagedBranch(t, repo)
		mustGit(t, repo, "switch", "--quiet", "main")

		later := engine(t, repo, testCfg(1, 0), iss, fake.New(), fake.New())
		later.Forge = forge
		_, err := later.HandoffFromState(context.Background(), HandoffOptions{Force: true})
		if err == nil || !strings.Contains(err.Error(), branch) {
			t.Fatalf("a checkout on the wrong branch must say which one to switch to, got %v", err)
		}
		if pushes, opened := forge.calls(); len(pushes) != 0 || len(opened) != 0 {
			t.Fatalf("published from the wrong branch: pushes=%v prs=%d", pushes, len(opened))
		}
	})
}

// What landed is asked of git, not read off the done list. A run that stopped
// between a worker closing its issue and the barrier that would have merged its
// branch has an issue that is done and not on the branch, and a pull request
// that listed it would be describing work the reviewer cannot see.
func TestTheRebuiltReportBelievesGitOverTheDoneList(t *testing.T) {
	repo := testRepo(t)
	cfg := testCfg(1, 0)

	// One branch merged into the epic branch, one left where the barrier never
	// reached it. Both are recorded done.
	mustGit(t, repo, "switch", "--quiet", "-c", cfg.Branch("t-1"))
	commitOnBranch(t, repo, "one.txt", "t-1")
	mustGit(t, repo, "switch", "--quiet", "main")
	mustGit(t, repo, "switch", "--quiet", "-c", cfg.Branch("t-2"))
	commitOnBranch(t, repo, "two.txt", "t-2")
	mustGit(t, repo, "switch", "--quiet", "main")
	mustGit(t, repo, "switch", "--quiet", "-c", "bd-auto/epic/epic-1")
	mustGit(t, repo, "merge", "--quiet", "--no-ff", "-m", "merge t-1", cfg.Branch("t-1"))

	st := runstate.New("epic-1", 1, "auto", 0)
	st.Base, st.EpicBranch = "main", "bd-auto/epic/epic-1"
	st.Wave = 1
	st.Done = []string{"t-1", "t-2"}
	st.Status = runstate.StatusDone
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}

	e := engine(t, repo, cfg, newIssues("t-1", "t-2"), fake.New(), fake.New())
	rep := e.stateReport(st)
	if got := rep.Landed(); len(got) != 1 || got[0] != "t-1" {
		t.Fatalf("landed %v; only t-1 is on the epic branch", got)
	}
	if len(rep.Done) != 2 {
		t.Fatalf("the run finished two issues and the report must still say so: %v", rep.Done)
	}

	// And the document says where the other one went, rather than leaving a
	// reviewer to notice that an issue the run closed is not in the diff.
	body := e.pullRequestBody(rep, HandoffReport{
		Branch: rep.EpicBranch, Base: rep.Base, Issues: rep.Landed(), ByHand: true,
	})
	if !strings.Contains(body, "NOT on this branch: t-2") {
		t.Fatalf("the body does not account for the issue that never merged:\n%s", body)
	}
}
