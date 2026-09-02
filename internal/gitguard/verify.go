package gitguard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Check names, one per predicate. They are stable strings: run state, the event
// stream and the worker's feedback all quote them.
const (
	CheckBranchMissing = "branch-missing"
	CheckBranchMoved   = "branch-moved"
	CheckAncestor      = "base-not-ancestor"
	CheckMergeCommit   = "merge-commit"
	CheckForeignCommit = "foreign-commit"
	CheckBaseMoved     = "base-moved"
	CheckRemoteMoved   = "remote-moved"
	CheckUnverifiable  = "unverifiable"
)

// Baseline is what the post-hoc checks compare against: the state of the repo
// at the moment the worker was dispatched. Record it before the worker starts.
// Recording it afterwards compares the damage against itself.
type Baseline struct {
	Issue   string `json:"issue"`
	Branch  string `json:"branch"`
	BaseRef string `json:"base_ref"`
	// Base is BaseRef's commit at dispatch: the commit a new branch is created
	// from, and the value BaseRef must still have afterwards.
	Base string `json:"base"`
	// Branched is where this branch actually left the base, and it is the start
	// of the range the worker is allowed to have written.
	//
	// It is Base for a branch this attempt creates, and something older for one
	// that already existed — a resumed run's interrupted worker, or a second
	// round on the same worktree — because the checkout has moved on since that
	// branch was cut. Comparing those against Base would say the branch no
	// longer contains the commit it was branched from, which is true of a
	// commit it was never branched from and describes the integrator's work
	// rather than the worker's.
	Branched string `json:"branched,omitempty"`
	// RemoteRefs is every refs/remotes/* and its commit at dispatch.
	RemoteRefs map[string]string `json:"remote_refs,omitempty"`
	// Integrated is every commit this run's own integrator moved BaseRef to
	// while the worker was running.
	//
	// Empty is the ordinary case and means BaseRef must not have moved at all,
	// which is what a run that merges only at a barrier expects: nothing else
	// writes to the checkout while a worker is out. A continuous run merges
	// beside its workers, so BaseRef legitimately moves under an attempt, and
	// without this the check cannot tell that from the thing it is really for
	// — a worker that committed to the branch it was told not to touch. It is
	// filled in at verification time rather than at dispatch, because the
	// merges it names all happen after the baseline is recorded.
	Integrated []string `json:"integrated,omitempty"`
}

// Record captures the baseline for one dispatch.
func Record(repoRoot, issue, branch, baseRef string) (Baseline, error) {
	if issue == "" {
		return Baseline{}, fmt.Errorf("gitguard: issue is required")
	}
	if baseRef == "" {
		baseRef = "HEAD"
	}
	base, err := git(repoRoot, "rev-parse", "--verify", baseRef+"^{commit}")
	if err != nil {
		return Baseline{}, fmt.Errorf("gitguard: resolve base %q: %w", baseRef, err)
	}
	refs, err := remoteRefs(repoRoot)
	if err != nil {
		return Baseline{}, err
	}
	b := Baseline{Issue: issue, Branch: branch, BaseRef: baseRef, Base: base,
		Branched: base, RemoteRefs: refs}
	// A branch that is already there was cut somewhere behind the checkout, and
	// where is a question git can answer. It fails for a branch that does not
	// exist yet, which is the common case and where Base is already right.
	if fork, err := git(repoRoot, "merge-base", base, branch); err == nil && fork != "" {
		b.Branched = fork
	}
	return b, nil
}

// Violation is one failed predicate.
type Violation struct {
	Check  string `json:"check"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// Result is the verdict on a worker's branch.
type Result struct {
	OK         bool        `json:"ok"`
	Violations []Violation `json:"violations,omitempty"`
}

// Has reports whether a named check failed.
func (r Result) Has(check string) bool {
	for _, v := range r.Violations {
		if v.Check == check {
			return true
		}
	}
	return false
}

// Reason renders the violations as feedback for the worker. It is the text that
// goes back into the next round, so it names the fix and not only the fault.
func (r Result) Reason() string {
	if r.OK {
		return ""
	}
	var b strings.Builder
	b.WriteString("bd-auto: this branch is not your work alone, so the attempt cannot be accepted.\n")
	for _, v := range r.Violations {
		fmt.Fprintf(&b, "- %s: %s\n  do this instead: %s\n", v.Check, v.Detail, v.Fix)
	}
	return b.String()
}

// Verify runs the post-hoc predicates against a branch.
//
// A worker is not permitted to move its branch at all. The orchestrator reviews
// a dirty snapshot and creates the one accepted commit afterwards. That exact
// equality closes every commit, amend, reset, fast-forward, merge, and rebase
// route before gate or review.
func Verify(repoRoot string, b Baseline) Result {
	res := Result{OK: true}
	add := func(check, detail, fix string) {
		res.OK = false
		res.Violations = append(res.Violations, Violation{Check: check, Detail: detail, Fix: fix})
	}

	branchHead, err := git(repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+b.Branch)
	if err != nil {
		add(CheckBranchMissing,
			fmt.Sprintf("branch %s does not exist", b.Branch),
			fmt.Sprintf("commit your work on %s; bd-auto merges that branch and nothing else", b.Branch))
		return res
	}

	from := b.Branched
	if from == "" {
		from = b.Base
	}
	if branchHead != from {
		add(CheckBranchMoved,
			fmt.Sprintf("%s moved from %s to %s while the worker was running", b.Branch, short(from), short(branchHead)),
			"leave changes uncommitted; bd-auto stages and creates the approved issue commit")
	}

	if _, err := git(repoRoot, "merge-base", "--is-ancestor", from, b.Branch); err != nil {
		add(CheckAncestor,
			fmt.Sprintf("%s no longer contains the commit it was branched from (%s)", b.Branch, short(from)),
			fmt.Sprintf("reset the branch to %s and re-apply your own work on top of it", short(from)))
	}

	rng := from + ".." + b.Branch

	if n, err := countCommits(repoRoot, rng, "--min-parents=2"); err != nil {
		add(CheckUnverifiable, "could not count merge commits: "+err.Error(),
			"leave the branch alone and report the failure")
	} else if n > 0 {
		add(CheckMergeCommit,
			fmt.Sprintf("%d merge commit(s) on %s", n, b.Branch),
			"keep the branch a straight line of your own commits; the integrator merges the wave")
	}

	if b.BaseRef != "" {
		if now, err := git(repoRoot, "rev-parse", "--verify", b.BaseRef+"^{commit}"); err == nil &&
			now != b.Base && !inList(b.Integrated, now) {
			add(CheckBaseMoved,
				fmt.Sprintf("%s moved from %s to %s during this attempt", b.BaseRef, short(b.Base), short(now)),
				fmt.Sprintf("never move %s or %s; leave worktree edits uncommitted", b.BaseRef, b.Branch))
		}
	}

	if now, err := remoteRefs(repoRoot); err != nil {
		add(CheckUnverifiable, "could not read remote-tracking refs: "+err.Error(),
			"leave the branch alone and report the failure")
	} else {
		for _, d := range diffRefs(b.RemoteRefs, now) {
			add(CheckRemoteMoved, d,
				"nothing in a worker worktree may reach a remote; leave edits uncommitted and finish")
		}
	}
	return res
}

// diffRefs describes how the remote-tracking refs changed. Anything here means
// something in this worktree talked to a remote.
func diffRefs(before, after map[string]string) []string {
	var out []string
	for ref, sha := range after {
		if old, ok := before[ref]; !ok {
			out = append(out, fmt.Sprintf("%s appeared at %s", ref, short(sha)))
		} else if old != sha {
			out = append(out, fmt.Sprintf("%s moved from %s to %s", ref, short(old), short(sha)))
		}
	}
	for ref := range before {
		if _, ok := after[ref]; !ok {
			out = append(out, ref+" disappeared")
		}
	}
	sort.Strings(out)
	return out
}

func remoteRefs(repoRoot string) (map[string]string, error) {
	out, err := git(repoRoot, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/")
	if err != nil {
		return nil, fmt.Errorf("gitguard: list remote refs: %w", err)
	}
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && name != "" {
			res[name] = sha
		}
	}
	return res, nil
}

type commit struct {
	SHA  string
	Body string
}

// commits reads a revision range as whole records. One git call rather than one
// per commit, because a long-running attempt can leave a lot of them.
func commits(repoRoot, rng string) ([]commit, error) {
	out, err := git(repoRoot, "log", "-z", "--format=%H%x1f%B", rng)
	if err != nil {
		return nil, err
	}
	var res []commit
	for _, rec := range strings.Split(out, "\x00") {
		sha, body, ok := strings.Cut(rec, "\x1f")
		if !ok || strings.TrimSpace(sha) == "" {
			continue
		}
		res = append(res, commit{SHA: strings.TrimSpace(sha), Body: body})
	}
	return res, nil
}

func countCommits(repoRoot, rng string, args ...string) (int, error) {
	full := append([]string{"rev-list", "--count"}, args...)
	out, err := git(repoRoot, append(full, rng)...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// hasTrailer reports whether a commit message carries this run's trailer for
// the given issue. Any attempt of that issue counts: rounds share a branch, and
// a fresh attempt starts from a discarded one.
func hasTrailer(body, issue string) bool {
	for _, line := range strings.Split(body, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), TrailerKey+":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == issue || strings.HasPrefix(v, issue+"/") {
			return true
		}
	}
	return false
}

func subject(body string) string {
	s, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func inList(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
