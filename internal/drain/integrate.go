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
	"bd-auto/internal/config"
	"bd-auto/internal/gitx"
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
	// MergeResolved is a branch that conflicted and was resolved anyway: by a
	// model, or by the rule that settles beads' own exports. Conflicts and
	// Settled are what say which, and a merge can be both.
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
	Issue   string       `json:"issue"`
	Branch  string       `json:"branch"`
	Outcome MergeOutcome `json:"outcome"`
	Reason  string       `json:"reason,omitempty"`
	// Conflicts is what git left conflicted that judgement had to settle, so a
	// model ran for this merge exactly when it is non-empty.
	Conflicts []string `json:"conflicts,omitempty"`
	// Settled is what git left conflicted that a rule settled instead: beads'
	// own exports, resolved to the copy the branch being merged into already
	// had. See resolveExportConflicts.
	Settled []string `json:"settled,omitempty"`
	Commit  string   `json:"commit,omitempty"`
	// Usage is what resolving this merge cost. It is zero for a clean merge and
	// for one only the export rule settled, which is the point: neither spawns
	// anything.
	Usage   runner.Usage `json:"usage"`
	Seconds float64      `json:"seconds"`

	// before is the head this merge was made on, so a red gate can be traced
	// back to the branch that caused it. Internal: it is a rollback target, not
	// a fact about the wave.
	before string
}

// resolution says what settled a resolved merge's conflicts. A merge can have
// been both: a model for the work, and the rule for a beads export alongside it.
func (m Merge) resolution() string {
	switch {
	case len(m.Conflicts) == 0:
		return fmt.Sprintf("settling %d beads export(s), with no model", len(m.Settled))
	case len(m.Settled) == 0:
		return fmt.Sprintf("a model resolved %d conflicted file(s)", len(m.Conflicts))
	}
	return fmt.Sprintf("a model resolved %d conflicted file(s), beside %d beads export(s) settled without one",
		len(m.Conflicts), len(m.Settled))
}

// landed reports whether this branch is in the merged result.
func (m Merge) landed() bool { return m.Outcome.landedOutcome() }

// landedOutcome is the same question of an outcome on its own, which is what a
// watcher holding a merge-end event has.
func (o MergeOutcome) landedOutcome() bool { return o == MergeClean || o == MergeResolved }

// IntegrateOptions are the knobs on one barrier.
type IntegrateOptions struct {
	// All widens the candidates from the current wave to every branch the run
	// has touched. It is what settles a run whose earlier waves never merged.
	All bool
	// Only narrows the candidates to these issues, which is the other
	// direction: a continuous run integrates one landed issue at a time, so
	// that whatever depended on it can start against a checkout that has it.
	// It is applied after All, so a caller can ask for one branch out of every
	// branch the run has touched.
	Only []string
	// Lane says this barrier is one step of a continuous run rather than a
	// boundary between two waves. Two things follow from it.
	//
	// The events it raises are tagged as a lane, because there is no boundary
	// to draw: the workers are still running beside it, and a watcher shown a
	// barrier between them would be shown something that is not happening.
	//
	// And it leaves the code index alone. The index is read when a worker is
	// dispatched, so the scheduler refreshes it there instead — a run that
	// rebuilt it after each of its merges would pay for it once per issue
	// whether or not anything was left to start. See beads-auto-imp-xhw.
	Lane bool
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

	// Hooks is what the repo's on_barrier hooks said about this barrier. It is
	// advisory: nothing in it merged, parked or closed anything. See hooks.go.
	Hooks []HookResult `json:"hooks,omitempty"`

	Usage   runner.Usage `json:"usage"`
	Seconds float64      `json:"seconds"`

	// only is the IntegrateOptions.Only this barrier was asked for, kept so the
	// bookkeeping at the end can say which issues it settled. Unexported: it is
	// an input rather than a result, and the JSON contract is the result.
	only []string
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

// Integrate merges branches into the main checkout, gates the merged result,
// cleans up what landed and closes the epic if the run finished it.
//
// What it merges is the caller's choice: a whole wave at a barrier, every branch
// the run ever touched when it is settling one, or a single issue that has just
// landed while its siblings keep working. The last of those is Lane, and it is
// the only one that does not also do the run's end-of-run bookkeeping — see the
// bottom of this function.
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

	rep := IntegrateReport{Epic: st.Epic, Wave: st.Wave, Base: gitx.CurrentBranch(e.RepoRoot)}
	rep.Target = rep.Base
	rep.only = opts.Only

	// A checkout already mid-merge is somebody else's half-finished work, and
	// committing on top of it would attribute their conflict resolution to this
	// wave. It is asked before the staging branch, because a checkout that
	// cannot be switched is exactly what a half-finished merge leaves behind.
	if mergeInProgress(e.RepoRoot) {
		return rep, fmt.Errorf("drain: %s is mid-merge; finish or abort that merge before integrating", e.RepoRoot)
	}

	// Here for the branch switch stage is about to do, which git refuses on the
	// same grounds a merge does — and on the working tree copy as well as the
	// staged one. Each merge clears again for itself.
	e.clearBeadsExport()

	if err := e.stage(st, &rep); err != nil {
		return rep, err
	}
	if rep.BaseHead, err = git(e.RepoRoot, "rev-parse", "HEAD"); err != nil {
		return rep, err
	}
	rep.Head = rep.BaseHead

	order := wave.Mergeable(wave.Order(e.candidates(st, opts)))

	// Said before the first merge rather than after the last one. A barrier
	// that has to spawn an integrator runs for minutes, and until this event
	// existed the live view spent all of them showing a table of finished rows
	// and nothing else — indistinguishable from a run that had hung.
	e.Bus.Emit(Event{Kind: EventWaveIntegrating, Wave: rep.Wave, Lane: opts.Lane,
		Issues: candidateIssues(order)})

	for _, c := range order {
		e.Bus.Emit(Event{Kind: EventMergeStart, Wave: rep.Wave, Issue: c.Issue,
			Text: c.Branch, Merge: &Merge{Issue: c.Issue, Branch: c.Branch}})
		m, stop, err := e.mergeBranch(ctx, c, st, rep.Base)
		rep.Merges = append(rep.Merges, m)
		rep.Usage = rep.Usage.Add(m.Usage)
		// Said whatever happened, including the error path below: a branch that
		// starts on the stream and never ends leaves a watcher showing it as
		// still merging, which is the one thing it is certainly not doing.
		e.emitMergeEnd(rep.Wave, m)
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
		rep.Gate = e.gateRepo(rep.Wave)
		rep.GatePassed = pipeline.Passed(rep.Gate)
		if !rep.GatePassed {
			e.blameGate(&rep)
		}
	}

	e.cleanup(rep)
	rep.Head, _ = git(e.RepoRoot, "rev-parse", "HEAD")

	// The next wave's workers read the index built at run start, and by now the
	// code it describes has moved under them. Only when a merge actually landed:
	// a barrier that merged nothing has nothing to re-extract, and `graphify
	// update` is cheap but not free. Refresh never fails a barrier — an index is
	// an optimisation, and the run must finish exactly as it would without one.
	if rep.Head != rep.BaseHead {
		e.merged.record(rep.Head)
		if !opts.Lane {
			e.refreshIndex(ctx)
		}
	}

	// Whatever this barrier decided about a branch — merged, or parked without
	// merging — it decided, so the issue stops counting as work waiting to land.
	// A continuous run reads exactly that list to know which ready issues would
	// be implemented against a tree their dependency is missing from, so leaving
	// a settled issue on it holds its dependents back for the rest of the run.
	e.clearIntegrated(rep)

	// The run's own bookkeeping, and only where this barrier is one: a lane is
	// one step of a run that has many, and all three of these are things that
	// belong to the end of it.
	//
	// Cost is the reason it is a rule rather than a preference. reconcile asks
	// bd about every issue the run has finished, so doing it once per merge is
	// quadratic in the size of the run for an answer that only matters in front
	// of the epic decision — and the epic decision that matters is the last one.
	// Skipping them here changes nothing a run reports: the settling barrier at
	// the end of a continuous run does all three, once. See drainContinuously.
	if !opts.Lane {
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
	}
	e.noteIntegration(rep)

	// Last, and only for a barrier that reached verdicts. Every merge, every
	// park, the gate, the discoveries and the epic decision are already in the
	// report and already done, so a hook here is reading a finished thing and
	// no worker is live to be a second writer to any of it.
	if rep.Stopped == "" {
		rep.Hooks = e.barrierHooks(ctx, rep)
		rep.Usage = rep.Usage.Add(hookUsage(rep.Hooks))
	}

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
	case !gitx.BranchExists(e.RepoRoot, branch):
		// Created from HEAD, so it carries whatever the run has already merged
		// and nothing else. A dirty checkout survives this untouched: the new
		// branch names the commit the checkout is already on.
		if _, err := git(e.RepoRoot, "switch", "--quiet", "-c", branch); err != nil {
			return fmt.Errorf("drain: create the epic branch %s: %w", branch, err)
		}
		e.logf("staging this run on %s; %s is not written to", branch, base)
	case gitx.CurrentBranch(e.RepoRoot) != branch:
		if _, err := git(e.RepoRoot, "switch", "--quiet", branch); err != nil {
			return fmt.Errorf("drain: check out the epic branch %s to integrate onto: %w", branch, err)
		}
	}
	rep.Base, rep.EpicBranch = branch, branch
	return e.recordStaging(base, branch)
}

// clearBeadsExport takes beads' exports out of git's way in the main checkout:
// out of the index, and out of the working tree where they differ from HEAD.
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
// The working tree copy is the same refusal by a different route, and unstaging
// does nothing about it. git switch declines on any uncommitted difference from
// HEAD, staged or not, when the branch being switched to has a different copy
// committed, and git merge declines the same way. A plain `bd show` rewrites
// the file, and the barrier runs one per candidate, so the working tree is
// dirty again between one barrier and the next with nobody having decided
// anything. On beads-auto-imp-04l that ended a run whose five issues had all
// finished, at the git switch in stage; the next run met it at the merge and
// parked all five, and because git refuses before it conflicts anything, each
// one was parked by the no-conflicted-paths path — without an integrator, and
// spending its only retry. See beads-auto-imp-zjf.
//
// It is called immediately before each merge, and once more before the checkout
// is switched, rather than once per barrier, because bd puts the export back.
// Not only bd write commands: a plain `bd show` re-exports and stages, and the
// barrier runs one per candidate to read its status. Clearing at the top of
// Integrate and merging afterwards therefore fixes nothing at all.
func (e *Engine) clearBeadsExport() {
	e.unstageBeadsExport()
	e.restoreBeadsExport()
}

// unstageBeadsExport clears a staged beads export out of the index, leaving the
// working tree alone.
//
// It discards nothing, which is why it can afford to name the whole of .beads
// rather than the two files restoreBeadsExport is allowed to overwrite: a
// human's staged work under .beads is still in their working tree afterwards,
// and nothing is committed on anybody's behalf.
func (e *Engine) unstageBeadsExport() {
	staged, err := git(e.RepoRoot, "diff", "--cached", "--name-only", "--", ".beads")
	if err != nil || strings.TrimSpace(staged) == "" {
		return
	}
	if _, err := gitWrite(e.RepoRoot, "reset", "--quiet", "HEAD", "--", ".beads"); err != nil {
		e.logf("warning: could not unstage the beads export, and the merge may refuse: %v", err)
		return
	}
	e.logf("unstaged %s from the index; a worker's commit staged it here and git will not merge over it",
		strings.Join(strings.Fields(staged), ", "))
}

// restoreBeadsExport puts the working tree copy of beads' exports back to what
// HEAD has, for the ones that differ.
//
// This one does discard, and it is the only thing here that does, so it names
// the two files rather than the directory. Both are a machine-written view of
// the Dolt database: bd regenerates them in full from it, the next bd command
// writes them again anyway, and neither is a record of anything the database
// does not already hold. Everything else under .beads — a repo's config, its
// hooks, the run's own state — is somebody's, and is left where it is.
//
// What it restores is HEAD's copy, which can be older than the database. That
// is inert for a drain: every git command bd-auto runs goes through gitx with
// hooks disabled, so nothing here imports the file back over the database. See
// internal/gitx.
func (e *Engine) restoreBeadsExport() {
	paths := make([]string, 0, len(beadsExports))
	for p := range beadsExports {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	dirty, err := git(e.RepoRoot, append([]string{"diff", "--name-only", "HEAD", "--"}, paths...)...)
	if err != nil || strings.TrimSpace(dirty) == "" {
		return
	}
	// Only the ones git just named. A path the repo does not track is not in
	// this list, and checking one out would fail the whole command.
	names := strings.Fields(dirty)
	if _, err := gitWrite(e.RepoRoot, append([]string{"checkout", "--quiet", "HEAD", "--"}, names...)...); err != nil {
		e.logf("warning: could not restore the beads export, and the switch or merge may refuse: %v", err)
		return
	}
	e.logf("restored %s from HEAD; bd rewrote it here and git will not switch or merge over it",
		strings.Join(names, ", "))
}

// beadsExports are the tracked files beads keeps in step with the Dolt
// database: a full re-export of it, and an append-only log of every field
// change in it. internal/gitguard has the same list, for the same reason.
var beadsExports = map[string]bool{
	".beads/issues.jsonl":       true,
	".beads/interactions.jsonl": true,
}

// resolveExportConflicts settles the conflicted paths that are beads exports
// and returns the ones it settled. It resolves to the version already on the
// branch being merged into, and stages that.
//
// A conflict in one of these is never a disagreement about the work. Both sides
// are a machine-written view of one database that both branches were writing to
// at the same time, and neither is a decision anybody made: the run's own
// record is run.json, and bd's is the database the exports are generated from.
// So there is nothing here for a model to weigh, and handing it one costs a
// call per merge on a file `bd export` regenerates in full.
//
// Keeping the base's copy rather than the branch's is what makes a wave of them
// converge: five branches carrying five snapshots of the same file land as the
// one snapshot the checkout already had, and the next bd write exports over it
// anyway. Nothing is discarded that the database does not still hold.
func (e *Engine) resolveExportConflicts(paths []string) []string {
	var done []string
	for _, p := range paths {
		if !beadsExports[p] {
			continue
		}
		// --ours is the branch being merged into. A path with no such stage --
		// added on one side only, deleted on the other -- is left conflicted
		// for the model, because then this is not the case described above.
		if _, err := git(e.RepoRoot, "checkout", "--ours", "--", p); err != nil {
			continue
		}
		if _, err := git(e.RepoRoot, "add", "--", p); err != nil {
			continue
		}
		done = append(done, p)
	}
	return done
}

// without returns paths with drop removed, order kept.
func without(paths, drop []string) []string {
	gone := map[string]bool{}
	for _, d := range drop {
		gone[d] = true
	}
	var out []string
	for _, p := range paths {
		if !gone[p] {
			out = append(out, p)
		}
	}
	return out
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
func (e *Engine) candidates(st *runstate.State, opts IntegrateOptions) []wave.Candidate {
	base := gitx.CurrentBranch(e.RepoRoot)
	trees := worktree.List(e.RepoRoot)
	only := map[string]bool{}
	for _, id := range opts.Only {
		only[id] = true
	}
	var out []wave.Candidate
	for _, id := range wave.CandidateIDs(st, opts.All) {
		if st.IsParked(id) {
			continue
		}
		if len(only) > 0 && !only[id] {
			continue
		}
		c := wave.Candidate{Issue: id, Branch: e.Cfg.Branch(id)}
		c.Exists = gitx.BranchExists(e.RepoRoot, c.Branch)
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

// MergeOrderReport is what the next barrier would merge, reported without
// merging it.
type MergeOrderReport struct {
	Epic string `json:"epic"`
	Wave int    `json:"wave"`
	// Candidates is every branch considered, in the order Integrate would take
	// them; Mergeable is the subset that has something to merge.
	Candidates []wave.Candidate `json:"candidates"`
	Mergeable  []wave.Candidate `json:"mergeable"`
	Base       string           `json:"base"`
}

// MergeOrder reports the branches Integrate would merge next, in the order it
// would merge them, and touches nothing.
//
// It is the barrier's own candidate gathering stopped one step short of the
// merge, on purpose. `bd-auto merge-order` used to gather its own, and the two
// had already parted company over parked issues: the barrier leaves them out,
// and the command listed them as work waiting to land.
func (e *Engine) MergeOrder(st *runstate.State, all bool) MergeOrderReport {
	ordered := wave.Order(e.candidates(st, IntegrateOptions{All: all}))
	return MergeOrderReport{
		Epic:       st.Epic,
		Wave:       st.Wave,
		Candidates: ordered,
		Mergeable:  wave.Mergeable(ordered),
		Base:       gitx.CurrentBranch(e.RepoRoot),
	}
}

// gitWrite runs a git command that writes to the main checkout, retrying it
// while the only thing wrong is that something else held git's lock.
//
// The something else is a worker. Beads' pre-commit hook re-exports its database
// and stages the result, and .beads lives in the main checkout rather than in
// the worktree the worker is committing from — so every worker commit takes the
// main checkout's index lock for a moment. Under waves that never met anything:
// the barrier ran when no worker was live. A continuous run merges beside them,
// and git does not retry, so without this a merge that arrived in one of those
// moments would park finished work with "git would not merge".
//
// Only lock contention is retried. A lock error means git did nothing at all,
// so there is no half-finished merge to reason about; anything else is a real
// answer and is returned as one.
func gitWrite(dir string, args ...string) (string, error) {
	var out string
	var err error
	for try := 0; ; try++ {
		out, err = git(dir, args...)
		if err == nil || try >= gitLockRetries || !gitLocked(err) {
			return out, err
		}
		time.Sleep(gitLockWait)
	}
}

// gitLockRetries and gitLockWait bound that. A worker's hook holds the index
// for milliseconds, so a second of patience is far more than the case needs and
// far less than a human would notice.
const (
	gitLockRetries = 10
	gitLockWait    = 100 * time.Millisecond
)

// gitLocked reports whether a git failure was somebody else holding its lock.
// The strings are git's, matched loosely because they differ by lock file and
// by version, and the cost of a false positive is one retry.
func gitLocked(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, ".lock") &&
		(strings.Contains(msg, "File exists") ||
			strings.Contains(msg, "Unable to create") ||
			strings.Contains(msg, "unable to create") ||
			strings.Contains(msg, "another git process"))
}

// blockedByCheckout names the paths git refused to overwrite, for an operation
// it declined before doing any of it.
//
// It reads git's words, for the same reason gitLocked does: git has no exit
// code for this, and the alternative is asking the working tree afterwards --
// which is a different question, because by then the export has usually been
// rewritten again. The two messages are the ones git prints for a merge, a
// switch and a checkout alike, and they have been stable for many versions;
// a miss costs the old behaviour, which is a park.
func blockedByCheckout(err error) []string {
	if err == nil {
		return nil
	}
	var out []string
	collecting := false
	for _, line := range strings.Split(err.Error(), "\n") {
		switch {
		case strings.Contains(line, "would be overwritten by"):
			// "Your local changes to the following files would be overwritten
			// by merge:", and the untracked-files wording beside it.
			collecting = true
		case collecting && strings.HasPrefix(line, "\t"):
			if p := strings.TrimSpace(line); p != "" {
				out = append(out, p)
			}
		case collecting:
			collecting = false
		}
	}
	sort.Strings(out)
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
	e.clearBeadsExport()

	release := e.checkoutMove()
	_, mergeErr := gitWrite(e.RepoRoot, "merge", "--no-ff", "--no-edit", c.Branch)
	release()
	if mergeErr == nil {
		m.Outcome = MergeClean
		m.Commit, _ = git(e.RepoRoot, "rev-parse", "HEAD")
		m.Seconds = time.Since(start).Seconds()
		e.logf("%s: merged %s cleanly", c.Issue, c.Branch)
		return m, "", nil
	}

	m.Conflicts = unmergedPaths(e.RepoRoot)
	if len(m.Conflicts) == 0 {
		abortMerge(e.RepoRoot)

		// A refusal about this checkout rather than about the branch. git
		// declines before it conflicts anything when the working tree or the
		// index holds a change the merge would have to overwrite, so there is
		// nothing here that judgement or another attempt at this branch could
		// settle -- and every branch still queued behind it would meet exactly
		// the same file. The barrier stops on it once instead of working
		// through the wave parking each in turn, which is how one rewritten
		// export turned five reviewed, gated branches into five parks in six
		// seconds, none of them spawning an integrator and each spending its
		// only retry. See beads-auto-imp-zjf.
		if paths := blockedByCheckout(mergeErr); len(paths) > 0 {
			m.Outcome = MergeSkipped
			m.Reason = fmt.Sprintf(
				"the checkout has uncommitted changes to %s that git will not merge over; nothing was decided about %s",
				strings.Join(paths, ", "), c.Branch)
			m.Seconds = time.Since(start).Seconds()
			e.logf("%s: %s", c.Issue, m.Reason)
			return m, OutcomeInfra, nil
		}

		// git refused for a reason no amount of judgement fixes: a branch that
		// vanished, a hook, a ref it cannot read. Spawning a model here would
		// spend a call on guesswork.
		reason := fmt.Sprintf("git would not merge %s and left no conflicted paths: %v", c.Branch, mergeErr)
		return e.parkMerge(m, reason, start), "", nil
	}

	// Beads' own exports are settled here rather than by the model, and before
	// the conflict is announced, so a wave whose branches all carry one is not
	// five model calls and five chances to park finished work.
	if settled := e.resolveExportConflicts(m.Conflicts); len(settled) > 0 {
		e.logf("%s: %s conflicts in %s, which beads regenerates; kept the copy %s already had",
			c.Issue, c.Branch, strings.Join(settled, ", "), base)
		// What is left is what a model is asked to resolve, and what the report
		// and the display call this branch's conflict.
		m.Settled, m.Conflicts = settled, without(m.Conflicts, settled)
		if len(m.Conflicts) == 0 {
			release := e.checkoutMove()
			why := e.completeMerge(settled)
			release()
			if why != "" {
				abortMerge(e.RepoRoot)
				return e.parkMerge(m, "the beads exports were settled but the merge would not complete: "+why, start), "", nil
			}
			m.Outcome = MergeResolved
			m.Commit, _ = git(e.RepoRoot, "rev-parse", "HEAD")
			m.Seconds = time.Since(start).Seconds()
			e.logf("%s: merged %s; every conflict was a beads export, so no model ran", c.Issue, c.Branch)
			return m, "", nil
		}
	}

	e.logf("%s: %s conflicts in %s", c.Issue, c.Branch, strings.Join(m.Conflicts, ", "))
	// Before the runner is built rather than after it, because building one can
	// fail and a watcher that never heard about the conflict cannot say why the
	// barrier stopped.
	conflicted := m
	e.Bus.Emit(Event{Kind: EventMergeConflict, Wave: st.Wave, Issue: c.Issue,
		Role: runner.RoleIntegrator, Text: strings.Join(m.Conflicts, ", "), Merge: &conflicted})

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

	release = e.checkoutMove()
	why := e.completeMerge(m.Conflicts)
	release()
	if why != "" {
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

// completeMerge finishes a conflicted merge somebody resolved, over the paths
// that were resolved. It returns the reason the merge cannot be completed, or
// "" once the merge commit exists.
//
// The checks are in this order on purpose: a file with markers still in it is a
// resolution that was never finished, and staging it would commit the markers.
func (e *Engine) completeMerge(paths []string) string {
	if bad := conflictMarkers(e.RepoRoot, paths); len(bad) > 0 {
		return "conflict markers are still in " + strings.Join(bad, ", ")
	}
	// -A so a resolution that deleted a file counts as staged too.
	if _, err := gitWrite(e.RepoRoot, append([]string{"add", "-A", "--"}, paths...)...); err != nil {
		return "the resolved files would not stage: " + err.Error()
	}
	if left := unmergedPaths(e.RepoRoot); len(left) > 0 {
		return "still unmerged: " + strings.Join(left, ", ")
	}
	if _, err := gitWrite(e.RepoRoot, "commit", "--no-edit"); err != nil {
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
	if _, err := e.recordParked(id, reason, StageIntegrate); err != nil {
		e.logf("warning: could not record %s as parked: %v", id, err)
	}
}

// gateRepo runs the gate on the main checkout as it stands.
//
// It is bracketed by events because it is the longest silent thing a barrier
// does: no model runs, so nothing streams, and a whole test suite can go by
// with the display saying only that the barrier is integrating. It is called
// again for every branch blameGate peels back, and each of those runs says so
// for itself.
func (e *Engine) gateRepo(waveNo int) []pipeline.Result {
	if !e.Cfg.HasGate() {
		return nil
	}
	e.Bus.Emit(Event{Kind: EventWaveGateStart, Wave: waveNo, Stage: config.StageGate,
		Text: gateCommands(e.Cfg)})
	rs := pipeline.Gate(e.Cfg, pipeline.Env{Dir: e.RepoRoot, RepoRoot: e.RepoRoot})
	e.Bus.Emit(Event{Kind: EventWaveGateEnd, Wave: waveNo, Stage: config.StageGate,
		Passed: pipeline.Passed(rs), Text: gateVerdict(rs)})
	return rs
}

// gateCommands is what the gate is about to run, for a watcher that would
// otherwise be looking at a row that says only "running".
func gateCommands(cfg *config.Config) string {
	out := make([]string, 0, len(cfg.Gate))
	for _, g := range cfg.Gate {
		if g.Run != "" {
			out = append(out, g.Run)
		} else {
			out = append(out, g.Name)
		}
	}
	return strings.Join(out, " · ")
}

// gateVerdict names the command that failed, or the ones that passed. The
// failing command is the whole of what a red gate means to whoever is watching;
// the output behind it is in the report and in the log.
func gateVerdict(rs []pipeline.Result) string {
	if f := pipeline.FirstFailure(rs); f != nil {
		if f.TimedOut {
			return f.Name + " timed out"
		}
		return fmt.Sprintf("%s failed (exit %d)", f.Name, f.ExitCode)
	}
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, r.Name)
	}
	return strings.Join(names, " · ")
}

// emitMergeEnd says what became of one branch.
func (e *Engine) emitMergeEnd(waveNo int, m Merge) {
	e.Bus.Emit(Event{Kind: EventMergeEnd, Wave: waveNo, Issue: m.Issue,
		Text: m.Reason, Usage: m.Usage, Merge: &m})
}

// blameGate finds which merge a red gate is about, and takes that branch back
// out.
//
// The gate runs once on the merged result, and when that one run comes back red
// something has to say WHICH merge did it — no inspection of the tree can. So
// the merges are peeled back newest first, gating after each, until the tree
// goes green. The branch whose removal fixed it is the offender.
//
// Peeling newest first means every branch merged after the offender comes off
// with it, and nothing is wrong with any of them. They go back on and the tree
// is gated once more, so that a red branch parks itself rather than everything
// that happened to follow it. See remergePeeled.
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
		release := e.checkoutMove()
		_, err := gitWrite(e.RepoRoot, "reset", "--keep", rep.Merges[i].before)
		release()
		if err != nil {
			rep.Gate, rep.GatePassed = red, false
			rep.Reason = fmt.Sprintf("the gate is red and %s could not be rolled back to find out why: %v",
				rep.Merges[i].Branch, err)
			return
		}
		peeled = append(peeled, i)
		rolled := rep.Merges[i]
		e.Bus.Emit(Event{Kind: EventWaveRollback, Wave: rep.Wave, Issue: rolled.Issue,
			Text: "rolled back to find out what the gate is red on", Merge: &rolled})

		rep.Gate = e.gateRepo(rep.Wave)
		if !pipeline.Passed(rep.Gate) {
			continue
		}
		rep.GatePassed = true

		offender := &rep.Merges[peeled[len(peeled)-1]]
		summary := strings.TrimSpace(pipeline.Summary(red))
		rep.Reason = fmt.Sprintf("the gate was red on the merged result and is green with %s rolled back",
			offender.Branch)
		e.parkLanded(rep.Wave, offender, fmt.Sprintf(
			"the wave gate failed on the merged result and went green once %s was rolled back, "+
				"so this branch is what the rest of the wave did not survive:\n%s", offender.Branch, summary))
		e.remergePeeled(rep, peeled[:len(peeled)-1], offender)
		return
	}

	// Every merge is out and the gate is still red, so the base was broken
	// before this wave touched it. Put the wave back and blame nobody.
	restored := true
	restore := e.checkoutMove()
	_, err := gitWrite(e.RepoRoot, "reset", "--keep", landed)
	restore()
	if err != nil {
		e.logf("warning: could not restore the merged result after gating the base: %v", err)
		restored = false
	}
	rep.Gate, rep.GatePassed = red, false
	rep.Reason = "the gate is red on " + rep.Base + " with every branch of this wave rolled back, " +
		"so the base was already red and no branch was parked"
	if !restored {
		// The tree is not what any of these rows say it is, and saying they
		// landed would be the one thing worse than saying nothing.
		return
	}
	// Every branch is back in. Each one was announced as rolled back on its way
	// out, so each one is announced as landed on its way back: a watcher told
	// only half of this shows a whole wave rolled back off a tree that has all
	// of it in.
	for _, idx := range peeled {
		e.emitMergeEnd(rep.Wave, rep.Merges[idx])
	}
}

// remergePeeled puts back the branches that came off the tree with the offender.
//
// Peeling newest first is what finds the offender, and it costs every branch
// merged after it: their work is intact on its own branch, it is simply no
// longer in the merged result. Parking those branches asks a human to merge by
// hand what the barrier already knows how to merge, so instead they go back on
// in the order they landed and the tree is gated once more — one extra gate run
// in a path that has already failed.
//
// That second gate is the arbiter. Green, and only the offender is parked. Red,
// and the peeled set comes back off and is parked exactly as it would have been:
// the barrier has one blame to hand out per barrier, and it never leaves a red
// tree behind to buy a second.
//
// No model runs here. A branch that will not merge without the offender
// underneath it is parked on the spot, because this is already the failing path
// and a conflict that exists only because a branch was rolled back is not the
// worker's to resolve.
func (e *Engine) remergePeeled(rep *IntegrateReport, idxs []int, offender *Merge) {
	if len(idxs) == 0 {
		return
	}
	green, err := git(e.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		e.parkCollateral(rep, idxs, offender, "the barrier could not read the tree to merge them back onto")
		return
	}
	greenGate := rep.Gate

	// idxs is newest first, the order they were peeled off in. They go back on
	// in the order they originally landed in, which is the order their
	// dependencies were merged in.
	var back, stuck []int
	for i := len(idxs) - 1; i >= 0; i-- {
		idx := idxs[i]
		m := &rep.Merges[idx]
		before, err := git(e.RepoRoot, "rev-parse", "HEAD")
		if err != nil {
			stuck = append(stuck, idx)
			continue
		}
		if !e.remergeOne(m) {
			stuck = append(stuck, idx)
			continue
		}
		m.before = before
		m.Commit, _ = git(e.RepoRoot, "rev-parse", "HEAD")
		back = append(back, idx)
		e.logf("%s: merged %s again with %s rolled back", m.Issue, m.Branch, offender.Branch)
	}

	if len(back) > 0 {
		rep.Gate = e.gateRepo(rep.Wave)
		if !pipeline.Passed(rep.Gate) {
			restore := e.checkoutMove()
			_, err := gitWrite(e.RepoRoot, "reset", "--keep", green)
			restore()
			if err != nil {
				rep.Gate, rep.GatePassed = greenGate, false
				rep.Reason = fmt.Sprintf("the gate went green with %s rolled back, but is red again with the "+
					"branch(es) rolled back with it merged in, and the tree could not be restored: %v",
					offender.Branch, err)
				return
			}
			rep.Gate = greenGate
			rep.Reason += fmt.Sprintf("; merging the %d branch(es) rolled back with it left the gate "+
				"red again, so they are parked with it", len(idxs))
			e.parkCollateral(rep, idxs, offender, "the gate was red again with them merged back in without it")
			return
		}
		rep.GatePassed = true
		rep.Reason += fmt.Sprintf("; the %d branch(es) rolled back with it were merged again and the gate is green",
			len(back))
	}

	// Announced again on their way back in. Each of these rows was told it was
	// rolled back, and a watcher left with only that half shows work as gone
	// from a tree that has it.
	for _, idx := range back {
		e.emitMergeEnd(rep.Wave, rep.Merges[idx])
	}
	e.parkCollateral(rep, stuck, offender, "it would not merge again without that branch underneath it")
}

// remergeOne puts one peeled branch back on, and reports whether it went.
//
// It settles a conflict in beads' exports and nothing else. The export rule
// holds here for the reason it holds on the way in: both sides are a machine's
// view of one database, neither is a decision anybody made, and there is
// nothing to weigh. Without it a red gate parks every branch that carries an
// export -- which is every branch cut from a main that has been closing issues,
// so a wave of them would be parked wholesale by one unrelated red gate.
//
// Anything else conflicted is left conflicted and the branch is stuck. No model
// runs on this path: it is already the failing one, and a conflict that exists
// only because a branch was rolled back is not the worker's to resolve.
func (e *Engine) remergeOne(m *Merge) bool {
	// Parking the offender went through bd, and bd writes the export again.
	// See clearBeadsExport: without this the merge below refuses.
	e.clearBeadsExport()
	release := e.checkoutMove()
	_, err := gitWrite(e.RepoRoot, "merge", "--no-ff", "--no-edit", m.Branch)
	release()
	if err == nil {
		return true
	}

	conflicts := unmergedPaths(e.RepoRoot)
	settled := e.resolveExportConflicts(conflicts)
	if len(conflicts) == 0 || len(settled) != len(conflicts) {
		abortMerge(e.RepoRoot)
		return false
	}
	release = e.checkoutMove()
	why := e.completeMerge(settled)
	release()
	if why != "" {
		abortMerge(e.RepoRoot)
		return false
	}
	m.Settled = union(m.Settled, settled)
	e.logf("%s: merged %s again; every conflict was a beads export, so no model ran", m.Issue, m.Branch)
	return true
}

// union appends what b has and a does not, keeping a's order.
func union(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			a = append(a, s)
		}
	}
	return a
}

// parkCollateral parks the branches that came off with the offender and could
// not be put back. why is what stopped them.
func (e *Engine) parkCollateral(rep *IntegrateReport, idxs []int, offender *Merge, why string) {
	for _, idx := range idxs {
		e.parkLanded(rep.Wave, &rep.Merges[idx], fmt.Sprintf(
			"rolled back with %s, the branch the wave gate failed on, and %s. Nothing is known to be "+
				"wrong with this work on its own: it is still on its own branch and can be merged "+
				"again once %s is fixed.", offender.Branch, why, offender.Issue))
	}
}

// parkLanded turns a merge that had landed into a parked one.
//
// It says so on the stream again, over the merge-end that said the branch had
// landed. That is the honest shape of a red gate: the branch did land, minutes
// ago, and the gate is what took it back out.
func (e *Engine) parkLanded(waveNo int, m *Merge, reason string) {
	m.Outcome, m.Reason, m.Commit = MergeParked, reason, ""
	e.logf("%s: parked %s: %s", m.Issue, m.Branch, firstLine(reason))
	e.park(m.Issue, fmt.Sprintf("bd-auto parked %s at integration: %s", m.Issue, reason))
	e.emitMergeEnd(waveNo, *m)
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

// clearIntegrated takes the issues this barrier settled off the list of work
// waiting to land.
//
// Only for a barrier that was asked for named issues, and only when it ran to
// the end. A wave barrier merges whatever the wave holds and the next wave's
// planning clears the list wholesale, so there is nothing here for it to do;
// what needs it is the continuous scheduler, which reads the same list to
// decide which ready issue would be implemented against a tree its dependency
// is missing from. An issue this barrier merged, parked, or found nothing to
// merge for is none of those, whichever of the three it was.
func (e *Engine) clearIntegrated(rep IntegrateReport) {
	if len(rep.only) == 0 || rep.Stopped != "" {
		return
	}
	if _, err := wave.Settled(e.RepoRoot, rep.only); err != nil {
		e.logf("warning: could not record %s as integrated: %v", strings.Join(rep.only, ", "), err)
	}
}

// noteIntegration leaves the breadcrumb. create is false: a run state that
// disappeared underneath the barrier is not one to resurrect.
func (e *Engine) noteIntegration(rep IntegrateReport) {
	what := fmt.Sprintf("wave %d", rep.Wave)
	if len(rep.only) > 0 {
		// A lane's breadcrumb names the issue rather than the wave. There is one
		// wave for the whole run, so a trail of "integrated wave 1" twenty times
		// says nothing about which twenty.
		what = strings.Join(rep.only, ", ")
	}
	_, err := runstate.Update(e.RepoRoot, false, func(s *runstate.State) error {
		s.Note("integrated %s: %d merged, %d parked, gate %s",
			what, len(rep.Merged()), len(rep.Parked()), passFail(rep.GatePassed))
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
