package drain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/pipeline"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
	"bd-auto/internal/wave"
	"bd-auto/internal/worktree"
)

// Integration is the wave barrier. Every branch here already passed its gate and
// its review alone; merging them asks the one question none of that could: do
// they still work together?
//
// All of it is deterministic Go except a single step. A merge that stops with
// conflicts spawns exactly one model, because reconciling two changes that were
// each correct in isolation is judgement rather than bookkeeping. A clean merge
// spawns nothing, the gate is Go, the cleanup is Go, and the decision to close
// the epic is a tested function rather than something a model is asked to get
// right.

// StageIntegrate is the stage name recorded against an issue whose branch did
// not survive the barrier.
const StageIntegrate = "integrate"

// MergeOutcome is what became of one worker branch at the barrier.
type MergeOutcome string

const (
	// MergeClean is a branch git merged with no conflict and no model.
	MergeClean MergeOutcome = "clean"
	// MergeResolved is a branch whose conflict a model resolved.
	MergeResolved MergeOutcome = "resolved"
	// MergeParked is a branch that did not land. Its branch and worktree are
	// left where they are: the work is intact, it just is not integrated.
	MergeParked MergeOutcome = "parked"
	// MergeSkipped is a branch integration never reached a verdict on, because
	// the run was interrupted or the environment stopped answering.
	MergeSkipped MergeOutcome = "skipped"
)

// Merge is one branch's trip through the barrier.
type Merge struct {
	Issue     string       `json:"issue"`
	Branch    string       `json:"branch"`
	Outcome   MergeOutcome `json:"outcome"`
	Reason    string       `json:"reason,omitempty"`
	Conflicts []string     `json:"conflicts,omitempty"`
	Commit    string       `json:"commit,omitempty"`
	// Usage is what resolving this merge cost. It is zero for a clean merge,
	// which is the point: a clean merge spawns nothing.
	Usage   runner.Usage `json:"usage"`
	Seconds float64      `json:"seconds"`

	// before is the head this merge was made on, so a red gate can be traced
	// back to the branch that caused it. Internal: it is a rollback target, not
	// a fact about the wave.
	before string
}

// landed reports whether this branch is in the merged result.
func (m Merge) landed() bool { return m.Outcome == MergeClean || m.Outcome == MergeResolved }

// IntegrateOptions are the knobs on one barrier.
type IntegrateOptions struct {
	// All widens the candidates from the current wave to every branch the run
	// has touched. It is what settles a run whose earlier waves never merged.
	All bool
}

// IntegrateReport is what one barrier produced.
type IntegrateReport struct {
	Epic string `json:"epic"`
	Wave int    `json:"wave"`
	// Base is the branch merged into, and BaseHead where it stood before.
	Base     string `json:"base"`
	BaseHead string `json:"base_head"`
	Head     string `json:"head"`

	// EpicBranch is the temporary branch this barrier merged into, empty for a
	// run that merges straight into its base branch. Where it is set it IS
	// Base — every field above describes the epic branch — and Target names the
	// branch the epic branch will eventually be handed to.
	EpicBranch string `json:"epic_branch,omitempty"`
	// Target is the branch a pull request would target: the branch the run
	// started on. It equals Base for an unstaged run.
	Target string `json:"target,omitempty"`

	Merges []Merge `json:"merges"`

	// Gate is the gate run on the merged result, and GatePassed its verdict.
	Gate       []pipeline.Result `json:"gate,omitempty"`
	GatePassed bool              `json:"gate_passed"`

	// Stopped is set when integration ended on something that is not a verdict
	// on anyone's work: an interrupt, or an environment that kept failing.
	Stopped Outcome `json:"stopped,omitempty"`
	// Reason explains Stopped, or a rollback that could not be completed.
	Reason string `json:"reason,omitempty"`

	// EpicClosed reports whether this barrier closed the epic, and EpicReason
	// says why it did or did not.
	EpicClosed bool   `json:"epic_closed"`
	EpicReason string `json:"epic_reason,omitempty"`

	// Reconciled is what this barrier had to put back into bd because something
	// reverted it underneath the run. Empty is the expected case; a non-empty
	// one is evidence worth keeping in the report rather than only in the log.
	Reconciled Reconciliation `json:"reconciled,omitempty"`

	// Discoveries is the work this barrier filed on its workers' behalf: found
	// beside the issues in scope, deferred so it waits for a human.
	Discoveries DiscoveryFiling `json:"discoveries,omitempty"`

	Usage   runner.Usage `json:"usage"`
	Seconds float64      `json:"seconds"`
}

// Merged lists the issues whose branches are in the merged result.
func (r IntegrateReport) Merged() []string { return r.issues(func(m Merge) bool { return m.landed() }) }

// Parked lists the issues whose branches did not survive the barrier.
func (r IntegrateReport) Parked() []string {
	return r.issues(func(m Merge) bool { return m.Outcome == MergeParked })
}

func (r IntegrateReport) issues(keep func(Merge) bool) []string {
	var out []string
	for _, m := range r.Merges {
		if keep(m) {
			out = append(out, m.Issue)
		}
	}
	return out
}

// Integrate merges a completed wave into the main checkout, gates the merged
// result, cleans up what landed and closes the epic if the run finished it.
//
// It returns an error only for a failure that is not about the work: an
// unreadable run state, a checkout already mid-merge, a runner that cannot be
// built. Everything a branch can fail at comes back in the report.
func (e *Engine) Integrate(ctx context.Context, opts IntegrateOptions) (IntegrateReport, error) {
	started := time.Now()
	switch {
	case e.RepoRoot == "":
		return IntegrateReport{}, errors.New("drain: RepoRoot is required")
	case e.Cfg == nil:
		return IntegrateReport{}, errors.New("drain: Cfg is required")
	case e.BD == nil:
		return IntegrateReport{}, errors.New("drain: BD is required")
	}

	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		return IntegrateReport{}, err
	}

	rep := IntegrateReport{Epic: st.Epic, Wave: st.Wave, Base: currentBranch(e.RepoRoot)}
	rep.Target = rep.Base

	// A checkout already mid-merge is somebody else's half-finished work, and
	// committing on top of it would attribute their conflict resolution to this
	// wave. It is asked before the staging branch, because a checkout that
	// cannot be switched is exactly what a half-finished merge leaves behind.
	if mergeInProgress(e.RepoRoot) {
		return rep, fmt.Errorf("drain: %s is mid-merge; finish or abort that merge before integrating", e.RepoRoot)
	}

	// Here for the branch switch stage is about to do, which git refuses on the
	// same grounds a merge does. Each merge unstages again for itself.
	e.unstageBeadsExport()

	if err := e.stage(st, &rep); err != nil {
		return rep, err
	}
	if rep.BaseHead, err = git(e.RepoRoot, "rev-parse", "HEAD"); err != nil {
		return rep, err
	}
	rep.Head = rep.BaseHead

	order := wave.Mergeable(wave.Order(e.candidates(st, opts.All)))

	// Said before the first merge rather than after the last one. A barrier
	// that has to spawn an integrator runs for minutes, and until this event
	// existed the live view spent all of them showing a table of finished rows
	// and nothing else — indistinguishable from a run that had hung.
	e.Bus.Emit(Event{Kind: EventWaveIntegrating, Wave: rep.Wave, Issues: candidateIssues(order)})

	for _, c := range order {
		m, stop, err := e.mergeBranch(ctx, c, st, rep.Base)
		rep.Merges = append(rep.Merges, m)
		rep.Usage = rep.Usage.Add(m.Usage)
		if err != nil {
			rep.Seconds = time.Since(started).Seconds()
			return rep, err
		}
		if stop != "" {
			// Neither an interrupt nor an outage is a verdict on the rest of the
			// wave, so the remaining branches are left for the next barrier
			// rather than merged unwatched.
			rep.Stopped, rep.Reason = stop, m.Reason
			break
		}
	}

	// One gate run on the merged result. That single run is the whole point of
	// the barrier: each branch already passed alone, and this asks whether they
	// pass together.
	if rep.Stopped == "" {
		rep.Gate = e.gateRepo()
		rep.GatePassed = pipeline.Passed(rep.Gate)
		if !rep.GatePassed {
			e.blameGate(&rep)
		}
	}

	e.cleanup(rep)
	rep.Head, _ = git(e.RepoRoot, "rev-parse", "HEAD")

	// Before the epic decision, never after it. EpicComplete asks bd whether
	// every child issue is closed, so an issue this run finished and something
	// else reverted would keep the epic open for good. See reconcile.
	rep.Reconciled = e.reconcile()

	// Also before it, and for a related reason. A discovered issue is filed
	// deferred and outside the epic, so it cannot change the close decision —
	// but filing after the epic closed would leave the run's last findings
	// unfiled whenever the epic closed on this barrier. See discover.go.
	rep.Discoveries = e.fileDiscoveries()

	e.closeEpic(&rep)
	e.noteIntegration(rep)

	rep.Seconds = time.Since(started).Seconds()
	return rep, nil
}

// stage puts the main checkout on the branch this barrier's merges land in.
//
// With handoff.branch on — the default — that is a temporary epic branch, and
// the point of it is that a run publishes nothing: the branch the human works
// on is never written to, and whether this work lands is their decision rather
// than a side effect of the run finishing.
//
// The branch is minted once per run, recorded in run state, and the checkout
// stays on it between waves. That last part is load-bearing rather than
// convenience: a worker branches from the main checkout's HEAD, so a run that
// moved the checkout back to the base branch between waves would hand wave two
// a tree with wave one missing from it, and every dependency across the barrier
// would break.
func (e *Engine) stage(st *runstate.State, rep *IntegrateReport) error {
	// The base is read from run state first, because from the second barrier
	// onwards the checkout is on the epic branch and can no longer say what the
	// run was branched from.
	cur := rep.Base
	base := st.Base
	if base == "" {
		base = cur
	}

	if !e.Cfg.StageOnBranch() {
		// Unstaged, the merge target is whatever the checkout is on, which is
		// what it has always been. The recorded base is still written down, so a
		// report can say what the run was for even when nobody staged anything.
		rep.Base, rep.Target = cur, cur
		return e.recordStaging(base, "")
	}
	rep.Target = base

	branch := st.EpicBranch
	if branch == "" {
		branch = EpicBranchName(e.Cfg.EpicBranchPrefix(), st.Epic, time.Now())
	}
	switch {
	case !branchExists(e.RepoRoot, branch):
		// Created from HEAD, so it carries whatever the run has already merged
		// and nothing else. A dirty checkout survives this untouched: the new
		// branch names the commit the checkout is already on.
		if _, err := git(e.RepoRoot, "switch", "--quiet", "-c", branch); err != nil {
			return fmt.Errorf("drain: create the epic branch %s: %w", branch, err)
		}
		e.logf("staging this run on %s; %s is not written to", branch, base)
	case currentBranch(e.RepoRoot) != branch:
		if _, err := git(e.RepoRoot, "switch", "--quiet", branch); err != nil {
			return fmt.Errorf("drain: check out the epic branch %s to integrate onto: %w", branch, err)
		}
	}
	rep.Base, rep.EpicBranch = branch, branch
	return e.recordStaging(base, branch)
}

// unstageBeadsExport clears a staged beads export out of the main checkout's
// index, leaving the file in the working tree exactly as it is.
//
// It is here because the barrier cannot merge without it. A worker's git is
// deliberately not suppressed, so its commit fires beads' pre-commit hook,
// which re-exports .beads/issues.jsonl and stages it — and the index it lands
// in is the main checkout's, not the worktree's, because that is where .beads
// lives. git then refuses every merge into the epic branch with "your local
// changes would be overwritten", even for a branch that does not touch the
// file at all: ort writes a fresh index and will not overwrite a staged path.
// Left alone, that parks every issue after the first worker commit — finished,
// gated, reviewed work, recorded as failed.
//
// It is called immediately before each merge, and once more before the checkout
// is switched, rather than once per barrier, because bd puts the export back.
// Not only bd write commands: a plain `bd show` re-exports and stages, and the
// barrier runs one per candidate to read its status. Unstaging at the top of
// Integrate and merging afterwards therefore fixes nothing at all.
//
// Unstaging is the smallest thing that fixes it and the only one that discards
// nothing. The export is a passive re-export of the Dolt database, regenerable
// with bd export; the working tree copy is untouched; nothing is committed on
// anybody's behalf. It is scoped to .beads so that a human's staged work
// elsewhere in the same checkout is none of this function's business.
func (e *Engine) unstageBeadsExport() {
	staged, err := git(e.RepoRoot, "diff", "--cached", "--name-only", "--", ".beads")
	if err != nil || strings.TrimSpace(staged) == "" {
		return
	}
	if _, err := git(e.RepoRoot, "reset", "--quiet", "HEAD", "--", ".beads"); err != nil {
		e.logf("warning: could not unstage the beads export, and the merge may refuse: %v", err)
		return
	}
	e.logf("unstaged %s from the index; a worker's commit staged it here and git will not merge over it",
		strings.Join(strings.Fields(staged), ", "))
}

// recordStaging writes the two branch names a run cannot re-derive later: what
// it was branched from, and what it is staged on.
func (e *Engine) recordStaging(base, branch string) error {
	_, err := runstate.Update(e.RepoRoot, false, func(s *runstate.State) error {
		if s.Base == "" {
			s.Base = base
		}
		if branch != "" && s.EpicBranch == "" {
			s.EpicBranch = branch
			s.Note("staging on %s; %s is not written to", branch, base)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("drain: record the staging branch: %w", err)
	}
	return nil
}

// EpicBranchName is the branch a run is staged on: the configured prefix, the
// epic, and the minute the run reached its first barrier.
//
// The timestamp is what makes it unique, and uniqueness is the requirement. A
// second run over the same epic — a re-run after a stop, a follow-up after a
// review — must not land on a branch that already has a pull request open
// against it, because that turns two separate results into one review nobody
// asked for.
func EpicBranchName(prefix, epic string, at time.Time) string {
	name := "run"
	if epic != "" {
		name = safeName(epic)
	}
	return prefix + name + "-" + at.UTC().Format("20060102-150405")
}

// candidates gathers the git and bd facts each branch needs to be ordered.
//
// A parked issue is left out. Its branch may well exist and carry commits, but
// they are the commits of an attempt that was judged unfinished, and merging
// half-done work is the one thing parking exists to prevent.
func (e *Engine) candidates(st *runstate.State, all bool) []wave.Candidate {
	base := currentBranch(e.RepoRoot)
	trees := worktree.List(e.RepoRoot)
	var out []wave.Candidate
	for _, id := range wave.CandidateIDs(st, all) {
		if st.IsParked(id) {
			continue
		}
		c := wave.Candidate{Issue: id, Branch: e.Cfg.Branch(id)}
		c.Exists = branchExists(e.RepoRoot, c.Branch)
		if c.Exists {
			c.Commits = commitsAhead(e.RepoRoot, base, c.Branch)
		}
		c.Worktree = trees[c.Branch]
		if iss, err := e.BD.Show(id); err == nil {
			c.Status = iss.Status
			for _, d := range iss.Dependencies {
				c.DependsOn = append(c.DependsOn, d.ID)
			}
		}
		out = append(out, c)
	}
	return out
}

// candidateIssues names the issues a barrier is about to try to merge.
func candidateIssues(cs []wave.Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Issue)
	}
	return out
}

// mergeBranch merges one branch, spawning a model only for a real conflict.
//
// The second return value is non-empty when integration must stop altogether
// rather than move to the next branch.
func (e *Engine) mergeBranch(ctx context.Context, c wave.Candidate, st *runstate.State, base string) (Merge, Outcome, error) {
	start := time.Now()
	m := Merge{Issue: c.Issue, Branch: c.Branch}
	head, err := git(e.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		return m, "", err
	}
	m.before = head

	// Last thing before the merge, because everything between here and the
	// previous call to it has been reading bd.
	e.unstageBeadsExport()

	_, mergeErr := git(e.RepoRoot, "merge", "--no-ff", "--no-edit", c.Branch)
	if mergeErr == nil {
		m.Outcome = MergeClean
		m.Commit, _ = git(e.RepoRoot, "rev-parse", "HEAD")
		m.Seconds = time.Since(start).Seconds()
		e.logf("%s: merged %s cleanly", c.Issue, c.Branch)
		return m, "", nil
	}

	m.Conflicts = unmergedPaths(e.RepoRoot)
	if len(m.Conflicts) == 0 {
		// git refused for a reason no amount of judgement fixes: a branch that
		// vanished, a hook, a tree it will not overwrite. Spawning a model here
		// would spend a call on guesswork.
		abortMerge(e.RepoRoot)
		reason := fmt.Sprintf("git would not merge %s and left no conflicted paths: %v", c.Branch, mergeErr)
		return e.parkMerge(m, reason, start), "", nil
	}

	e.logf("%s: %s conflicts in %s", c.Issue, c.Branch, strings.Join(m.Conflicts, ", "))

	rn, err := e.runnerFor(runner.RoleIntegrator)
	if err != nil {
		abortMerge(e.RepoRoot)
		return m, "", err
	}
	iss, _ := e.BD.Show(c.Issue)
	// Through the clone, so the integrator's tool calls reach the bus tagged
	// with the issue whose branch it is resolving — which is the only row a
	// watcher could reasonably expect to see them on.
	call, err := e.forIssue(st.Wave, c.Issue).invoke(ctx, invocation{
		Issue:  c.Issue,
		Branch: c.Branch,
		Role:   runner.RoleIntegrator,
		Runner: rn,
		Sess:   &session{},
		// One conflict, one call. There is no feedback round to resume into:
		// either the tree it leaves behind is resolved or the merge is aborted.
		CanResume: false,
		Ephemeral: true,
		Build:     func(bool) runner.Request { return e.conflictRequest(m, base, iss, st.Attempts[c.Issue]) },
	})
	m.Usage = call.Usage
	// The integrator is gone whatever it produced, so its question goes with it.
	e.cancelAsk(c.Issue)
	if err != nil {
		abortMerge(e.RepoRoot)
		return m, "", err
	}

	switch call.Result.Class {
	case runner.ClassInterrupted:
		abortMerge(e.RepoRoot)
		m.Outcome = MergeSkipped
		m.Reason = resultReason(call.Result, "integration was interrupted")
		m.Seconds = time.Since(start).Seconds()
		return m, OutcomeInterrupted, nil
	case runner.ClassInfraFailed:
		abortMerge(e.RepoRoot)
		m.Outcome = MergeSkipped
		m.Reason = resultReason(call.Result, "the integrator kept failing on the environment")
		m.Seconds = time.Since(start).Seconds()
		return m, OutcomeInfra, nil
	}

	if why := e.completeMerge(m); why != "" {
		abortMerge(e.RepoRoot)
		return e.parkMerge(m, conflictParkReason(why, call.Result.Text), start), "", nil
	}
	m.Outcome = MergeResolved
	m.Commit, _ = git(e.RepoRoot, "rev-parse", "HEAD")
	m.Seconds = time.Since(start).Seconds()
	e.logf("%s: merged %s after resolving %d conflicted file(s)", c.Issue, c.Branch, len(m.Conflicts))
	return m, "", nil
}

// conflictRequest is the one model invocation integration ever makes.
func (e *Engine) conflictRequest(m Merge, base string, iss *bd.Issue, attempt int) runner.Request {
	req := e.Cfg.Runner(string(runner.RoleIntegrator)).Request(runner.RoleIntegrator)
	// The main checkout, mid-merge. That is where the conflict is.
	req.Dir = e.RepoRoot
	req.SystemPrompt = e.promptFor(runner.RoleIntegrator)
	req.Prompt = conflictPrompt(m, base, iss)
	req.LogPath = LogPath(e.RepoRoot, m.Issue, attempt, 0, runner.RoleIntegrator)
	return req
}

// completeMerge finishes a conflicted merge the integrator resolved. It returns
// the reason the merge cannot be completed, or "" once the merge commit exists.
//
// The checks are in this order on purpose: a file with markers still in it is a
// resolution that was never finished, and staging it would commit the markers.
func (e *Engine) completeMerge(m Merge) string {
	if bad := conflictMarkers(e.RepoRoot, m.Conflicts); len(bad) > 0 {
		return "conflict markers are still in " + strings.Join(bad, ", ")
	}
	// -A so a resolution that deleted a file counts as staged too.
	if _, err := git(e.RepoRoot, append([]string{"add", "-A", "--"}, m.Conflicts...)...); err != nil {
		return "the resolved files would not stage: " + err.Error()
	}
	if left := unmergedPaths(e.RepoRoot); len(left) > 0 {
		return "still unmerged: " + strings.Join(left, ", ")
	}
	if _, err := git(e.RepoRoot, "commit", "--no-edit"); err != nil {
		return "the resolved merge would not commit: " + err.Error()
	}
	return ""
}

// parkMerge sets a branch aside. The branch and its worktree stay: the work is
// intact and a human, or a later wave, can pick it up.
func (e *Engine) parkMerge(m Merge, reason string, start time.Time) Merge {
	m.Outcome, m.Reason = MergeParked, reason
	m.Seconds = time.Since(start).Seconds()
	e.logf("%s: parked %s: %s", m.Issue, m.Branch, firstLine(reason))
	e.park(m.Issue, fmt.Sprintf("bd-auto parked %s at integration: %s", m.Issue, reason))
	return m
}

// park records a parked issue in bd and in run state. One bad branch never
// blocks the rest of the wave, so a failure to record is logged rather than
// returned.
func (e *Engine) park(id, reason string) {
	if err := e.BD.Park(id, reason); err != nil {
		e.logf("warning: could not park %s: %v", id, err)
	}
	if err := e.recordParked(id, reason, StageIntegrate); err != nil {
		e.logf("warning: could not record %s as parked: %v", id, err)
	}
}

// gateRepo runs the gate on the main checkout as it stands.
func (e *Engine) gateRepo() []pipeline.Result {
	if !e.Cfg.HasGate() {
		return nil
	}
	return pipeline.Gate(e.Cfg, pipeline.Env{Dir: e.RepoRoot, RepoRoot: e.RepoRoot})
}

// blameGate finds which merge a red gate is about, and takes that branch back
// out.
//
// The gate runs once on the merged result, and when that one run comes back red
// something has to say WHICH merge did it — no inspection of the tree can. So
// the merges are peeled back newest first, gating after each, until the tree
// goes green. The branch whose removal fixed it is the offender. Branches peeled
// off after it are collateral: they are parked too, with their branches and
// worktrees intact, so the next barrier can merge them again.
//
// If the tree is still red once every merge is peeled, the base was already red.
// Nothing is parked then, and the merges go back: blaming a worker for a broken
// base is exactly the mis-attribution this loop exists to prevent.
func (e *Engine) blameGate(rep *IntegrateReport) {
	red := rep.Gate
	landed, _ := git(e.RepoRoot, "rev-parse", "HEAD")

	var peeled []int
	for i := len(rep.Merges) - 1; i >= 0; i-- {
		if !rep.Merges[i].landed() {
			continue
		}
		// --keep rather than --hard: it refuses instead of destroying anything
		// uncommitted that happens to be in the main checkout.
		if _, err := git(e.RepoRoot, "reset", "--keep", rep.Merges[i].before); err != nil {
			rep.Gate, rep.GatePassed = red, false
			rep.Reason = fmt.Sprintf("the gate is red and %s could not be rolled back to find out why: %v",
				rep.Merges[i].Branch, err)
			return
		}
		peeled = append(peeled, i)

		rep.Gate = e.gateRepo()
		if !pipeline.Passed(rep.Gate) {
			continue
		}
		rep.GatePassed = true

		offender := &rep.Merges[peeled[len(peeled)-1]]
		summary := strings.TrimSpace(pipeline.Summary(red))
		rep.Reason = fmt.Sprintf("the gate was red on the merged result and is green with %s rolled back",
			offender.Branch)
		e.parkLanded(offender, fmt.Sprintf(
			"the wave gate failed on the merged result and went green once %s was rolled back, "+
				"so this branch is what the rest of the wave did not survive:\n%s", offender.Branch, summary))
		for _, idx := range peeled[:len(peeled)-1] {
			e.parkLanded(&rep.Merges[idx], fmt.Sprintf(
				"rolled back with %s, the branch the wave gate failed on. Nothing is wrong with this "+
					"work: it is still on its own branch and can be merged again once %s is fixed.",
				offender.Branch, offender.Issue))
		}
		return
	}

	// Every merge is out and the gate is still red, so the base was broken
	// before this wave touched it. Put the wave back and blame nobody.
	if _, err := git(e.RepoRoot, "reset", "--keep", landed); err != nil {
		e.logf("warning: could not restore the merged result after gating the base: %v", err)
	}
	rep.Gate, rep.GatePassed = red, false
	rep.Reason = "the gate is red on " + rep.Base + " with every branch of this wave rolled back, " +
		"so the base was already red and no branch was parked"
}

// parkLanded turns a merge that had landed into a parked one.
func (e *Engine) parkLanded(m *Merge, reason string) {
	m.Outcome, m.Reason, m.Commit = MergeParked, reason, ""
	e.logf("%s: parked %s: %s", m.Issue, m.Branch, firstLine(reason))
	e.park(m.Issue, fmt.Sprintf("bd-auto parked %s at integration: %s", m.Issue, reason))
}

// cleanup removes the worktree and branch of everything that landed. The
// worktree goes first: git will not delete a branch another worktree has checked
// out.
func (e *Engine) cleanup(rep IntegrateReport) {
	for _, m := range rep.Merges {
		if !m.landed() {
			continue
		}
		if err := worktree.Remove(e.RepoRoot, m.Issue); err != nil {
			e.logf("warning: could not remove the worktree for %s: %v", m.Issue, err)
		}
		if _, err := git(e.RepoRoot, "branch", "-d", m.Branch); err != nil {
			e.logf("warning: could not delete %s: %v", m.Branch, err)
		}
	}
}

// closeEpic asks the predicate and acts on it.
//
// The engine is the only thing that runs after a wave has actually landed, so an
// epic left open here stays open forever — and an epic closed over a parked child
// hides the one thing a human needs to see. Child issues are never closed here:
// those belong to their workers, and two writers on one issue is how beads loses
// an update.
func (e *Engine) closeEpic(rep *IntegrateReport) {
	if rep.Epic == "" {
		rep.EpicReason = "this run has no epic"
		return
	}
	// Reloaded: the parks above are the whole reason the decision might change.
	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		rep.EpicReason = "could not re-read the run state: " + err.Error()
		return
	}
	children, err := e.BD.Children(rep.Epic)
	if err != nil {
		rep.EpicReason = "could not read the epic's children: " + err.Error()
		return
	}

	// A barrier that stopped early gated nothing, so it cannot claim a green
	// tree however the gate results read.
	v := EpicComplete(st, children, rep.GatePassed && rep.Stopped == "", time.Now())
	rep.EpicReason = v.Reason
	if !v.Close {
		e.logf("%s stays open: %s", rep.Epic, v.Reason)
		return
	}
	if err := e.BD.Close(rep.Epic, v.Reason); err != nil {
		rep.EpicReason = "could not close the epic: " + err.Error()
		e.logf("warning: could not close %s: %v", rep.Epic, err)
		return
	}
	rep.EpicClosed = true
	e.logf("closed %s: %s", rep.Epic, v.Reason)
}

// noteIntegration leaves the breadcrumb. create is false: a run state that
// disappeared underneath the barrier is not one to resurrect.
func (e *Engine) noteIntegration(rep IntegrateReport) {
	_, err := runstate.Update(e.RepoRoot, false, func(s *runstate.State) error {
		s.Note("integrated wave %d: %d merged, %d parked, gate %s",
			s.Wave, len(rep.Merged()), len(rep.Parked()), passFail(rep.GatePassed))
		if !rep.Reconciled.Empty() {
			s.Note("reconciled %d issue(s) bd had reverted underneath the run", rep.Reconciled.Total())
		}
		if !rep.Discoveries.Empty() {
			s.Note("filed %d discovered issue(s), skipped %d bd already had",
				len(rep.Discoveries.Filed), rep.Discoveries.Skipped)
		}
		if rep.EpicClosed {
			s.Note("closed epic %s", rep.Epic)
		}
		return nil
	})
	if err != nil {
		e.logf("warning: could not record the integration: %v", err)
	}
}

// --- the close predicate ---

// EpicVerdict is the close decision and the one condition that decided it.
type EpicVerdict struct {
	Close  bool   `json:"close"`
	Reason string `json:"reason"`
	Total  int    `json:"total"`
	Closed int    `json:"closed"`
	// Open is every child that has not reached closed and is not deferred.
	Open []string `json:"open,omitempty"`
	// OutOfScope is the subset of Open this run was never allowed to touch.
	OutOfScope []string `json:"out_of_scope,omitempty"`
	// Deferred is every child bd is hiding until a future date. They are not in
	// Open, and they are named here so a human can see what an epic closed over.
	Deferred []string `json:"deferred,omitempty"`
}

// EpicComplete decides whether a run may close its epic. It is a pure function
// of run state and the epic's children, which is why it is here and tested
// rather than described to a model.
//
// Every one of these must hold:
//
//   - Nothing is in flight. Something is still running.
//   - Nothing is parked. Parked work is required work that did not get done.
//   - Every child issue reached closed or is deferred. This subsumes the old
//     "nothing waiting to be dispatched" condition: an issue waiting for a
//     later wave is an open child. A deferred child is not: bd will not offer
//     it to this run or any other until its date, so an epic that waits for one
//     waits forever. bd's own counts do not make that distinction — see
//     bd.Issue.Deferred — so it is made here.
//   - The gate passes on the tree as it stands. Never close an epic over a red
//     tree.
//
// Scope is what the fourth condition means now that a run need not cover a whole
// epic. A run whose scope was a subset finishes with children still open, and it
// must leave the epic alone — but it must say so as a scope fact rather than as
// a failure, because nothing went wrong. An empty scope is an unrestricted run,
// never an empty one.
func EpicComplete(st *runstate.State, children []bd.Issue, gateGreen bool, now time.Time) EpicVerdict {
	var v EpicVerdict
	var inScopeOpen []string
	for _, c := range children {
		if c.ID == "" || c.ID == st.Epic {
			continue
		}
		v.Total++
		if c.Closed() {
			v.Closed++
			continue
		}
		if c.Deferred(now) {
			v.Deferred = append(v.Deferred, c.ID)
			continue
		}
		v.Open = append(v.Open, c.ID)
		if st.InScope(c.ID) {
			inScopeOpen = append(inScopeOpen, c.ID)
		} else {
			v.OutOfScope = append(v.OutOfScope, c.ID)
		}
	}

	switch {
	case len(st.InFlight) > 0:
		v.Reason = fmt.Sprintf("%d issue(s) still in flight: %s",
			len(st.InFlight), strings.Join(sorted(st.Remaining()), ", "))
	case len(st.Parked) > 0:
		v.Reason = fmt.Sprintf("%d parked issue(s) are required work that did not get done: %s",
			len(st.Parked), strings.Join(parkedIDs(st), ", "))
	case v.Total == 0:
		// Either the epic genuinely has no children or bd could not list them.
		// Closing on that is closing on no evidence.
		v.Reason = "the epic reports no child issues"
	case v.Closed == 0 && len(v.Open) == 0 && len(v.Deferred) > 0:
		// Same shape as the case above: every child is deferred and none was
		// ever finished, so there is nothing the run can point at. It has to
		// require an empty Open as well as an empty Closed, or an epic with one
		// deferred child and one genuinely open one would report the deferral
		// and say nothing about the open child that is actually holding it.
		v.Reason = fmt.Sprintf("all %d child issue(s) are deferred and none reached closed", len(v.Deferred))
	case len(inScopeOpen) > 0:
		v.Reason = fmt.Sprintf("%d child issue(s) are still open: %s",
			len(inScopeOpen), strings.Join(inScopeOpen, ", "))
	case len(v.OutOfScope) > 0:
		v.Reason = fmt.Sprintf("this run's scope covered %d of %d children; %d never in scope are still open: %s",
			len(st.Scope), v.Total, len(v.OutOfScope), strings.Join(v.OutOfScope, ", "))
	case !gateGreen:
		v.Reason = "the gate is red on the merged result"
	default:
		v.Close = true
		v.Reason = fmt.Sprintf("%d child issues completed, integrated and gated", v.Closed)
		if len(v.Deferred) > 0 {
			v.Reason += fmt.Sprintf("; %d deferred child issue(s) are not in this run's way: %s",
				len(v.Deferred), strings.Join(v.Deferred, ", "))
		}
	}
	return v
}

func parkedIDs(st *runstate.State) []string {
	out := make([]string, 0, len(st.Parked))
	for _, p := range st.Parked {
		out = append(out, p.ID)
	}
	return out
}

// --- git ---

// currentBranch is the branch the main checkout is on, and the branch every
// merge lands in.
func currentBranch(dir string) string {
	out, err := git(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out == "" {
		return "HEAD"
	}
	return out
}

func commitsAhead(dir, base, branch string) int {
	out, err := git(dir, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(out, "%d", &n)
	return n
}

// mergeInProgress reports whether the checkout is sitting in a half-finished
// merge.
func mergeInProgress(dir string) bool {
	gitDir, err := git(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(gitDir, "MERGE_HEAD"))
	return err == nil
}

// unmergedPaths lists the files git left conflicted.
func unmergedPaths(dir string) []string {
	out, err := git(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// conflictMarkers returns the paths that still carry conflict markers.
//
// Only the conflicted paths are checked: a repo that documents merge markers
// elsewhere is not a failed resolution, and searching the whole tree would make
// it one.
func conflictMarkers(dir string, paths []string) []string {
	var bad []string
	for _, p := range paths {
		raw, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			continue // deleted as the resolution; git add -A covers it
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") {
				bad = append(bad, p)
				break
			}
		}
	}
	return bad
}

func abortMerge(dir string) {
	_, _ = git(dir, "merge", "--abort")
}

// --- small helpers ---

func passFail(ok bool) string {
	if ok {
		return "passed"
	}
	return "failed"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
