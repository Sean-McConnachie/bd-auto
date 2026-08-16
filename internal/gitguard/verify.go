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
	// Base is BaseRef's commit at dispatch. It is both the start of the range
	// the worker is allowed to have written and the value BaseRef must still
	// have afterwards.
	Base string `json:"base"`
	// RemoteRefs is every refs/remotes/* and its commit at dispatch.
	RemoteRefs map[string]string `json:"remote_refs,omitempty"`
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
	return Baseline{Issue: issue, Branch: branch, BaseRef: baseRef, Base: base, RemoteRefs: refs}, nil
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
// The trailer check is the one that closes the holes the structural guards
// cannot. After `git rebase origin/main` the base is still an ancestor, there
// are still no merge commits, and the local and remote refs still hold their
// recorded values, but the range now contains origin's commits, and those carry
// no trailer. A fast-forward merge behaves the same way. The shape checks stay
// because they produce a better message when they are the ones that fire.
func Verify(repoRoot string, b Baseline) Result {
	res := Result{OK: true}
	add := func(check, detail, fix string) {
		res.OK = false
		res.Violations = append(res.Violations, Violation{Check: check, Detail: detail, Fix: fix})
	}

	if _, err := git(repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+b.Branch); err != nil {
		add(CheckBranchMissing,
			fmt.Sprintf("branch %s does not exist", b.Branch),
			fmt.Sprintf("commit your work on %s; bd-auto merges that branch and nothing else", b.Branch))
		return res
	}

	if _, err := git(repoRoot, "merge-base", "--is-ancestor", b.Base, b.Branch); err != nil {
		add(CheckAncestor,
			fmt.Sprintf("%s no longer contains the commit it was branched from (%s)", b.Branch, short(b.Base)),
			fmt.Sprintf("reset the branch to %s and re-apply your own work on top of it", short(b.Base)))
	}

	rng := b.Base + ".." + b.Branch

	if n, err := countCommits(repoRoot, rng, "--min-parents=2"); err != nil {
		add(CheckUnverifiable, "could not count merge commits: "+err.Error(),
			"leave the branch alone and report the failure")
	} else if n > 0 {
		add(CheckMergeCommit,
			fmt.Sprintf("%d merge commit(s) on %s", n, b.Branch),
			"keep the branch a straight line of your own commits; the integrator merges the wave")
	}

	cs, err := commits(repoRoot, rng)
	if err != nil {
		add(CheckUnverifiable, "could not read the commits on this branch: "+err.Error(),
			"leave the branch alone and report the failure")
	}
	for _, c := range cs {
		if hasTrailer(c.Body, b.Issue) {
			continue
		}
		add(CheckForeignCommit,
			fmt.Sprintf("%s %q carries no %s: %s trailer, so it was not written by this attempt",
				short(c.SHA), subject(c.Body), TrailerKey, b.Issue),
			fmt.Sprintf("a rebase or a fast-forward merge pulls other peoples commits onto your branch; "+
				"reset to %s, re-apply only your own work as ordinary commits, and let the integrator "+
				"do the integrating", short(b.Base)))
	}

	if b.BaseRef != "" {
		if now, err := git(repoRoot, "rev-parse", "--verify", b.BaseRef+"^{commit}"); err == nil && now != b.Base {
			add(CheckBaseMoved,
				fmt.Sprintf("%s moved from %s to %s during this attempt", b.BaseRef, short(b.Base), short(now)),
				fmt.Sprintf("never commit to %s; commit to %s and stop", b.BaseRef, b.Branch))
		}
	}

	if now, err := remoteRefs(repoRoot); err != nil {
		add(CheckUnverifiable, "could not read remote-tracking refs: "+err.Error(),
			"leave the branch alone and report the failure")
	} else {
		for _, d := range diffRefs(b.RemoteRefs, now) {
			add(CheckRemoteMoved, d,
				"nothing in a worker worktree may reach a remote; commit locally and finish, "+
					"and the integrator decides what leaves this machine")
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
