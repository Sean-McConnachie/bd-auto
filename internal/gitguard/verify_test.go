package gitguard

import (
	"strings"
	"testing"
)

// only asserts that exactly the named check failed. The interesting property of
// this guard is not that something failed but which predicate caught it: the
// shape predicates pass clean on both of the holes below.
func only(t *testing.T, res Result, check string) {
	t.Helper()
	if res.OK {
		t.Fatalf("branch accepted; expected %s", check)
	}
	if !res.Has(check) {
		t.Fatalf("expected %s, got:\n%s", check, res.Reason())
	}
	if len(res.Violations) != 1 {
		t.Fatalf("expected only %s, got:\n%s", check, res.Reason())
	}
}

func TestVerifyAcceptsABranchOfOnlyItsOwnCommits(t *testing.T) {
	f := newFixture(t)
	b := f.setup(t)
	f.work(t, "a.txt")
	f.work(t, "b.txt")

	if res := Verify(f.Repo, b); !res.OK {
		t.Fatalf("clean branch rejected:\n%s", res.Reason())
	}
}

// The first hole. pre-merge-commit does not fire on a fast-forward, because no
// commit is created, so nothing structural can stop this one.
func TestVerifyCatchesAFastForwardMergeFromOrigin(t *testing.T) {
	f := newFixture(t)
	upstream := f.advanceOrigin(t, "upstream.txt")
	// Fetching before the baseline is recorded is what makes this test about
	// the merge rather than about the fetch.
	mustGit(t, f.WT, "fetch", "--quiet", "origin")
	b := f.setup(t)

	if _, err := git(f.WT, "merge", "--ff-only", "origin/main"); err != nil {
		t.Fatalf("a fast-forward merge is exactly what the structural half cannot see: %v", err)
	}
	f.work(t, "a.txt")

	res := Verify(f.Repo, b)
	only(t, res, CheckForeignCommit)
	if !strings.Contains(res.Reason(), short(upstream)) {
		t.Fatalf("violation does not name the imported commit %s:\n%s", short(upstream), res.Reason())
	}
}

// The second hole. The structural guard refuses a rebase, so the bypass here is
// deliberate: it is how the test reaches the case where the guard was talked
// around, which is the case the post-hoc predicate exists for.
func TestVerifyCatchesARebaseOntoOrigin(t *testing.T) {
	f := newFixture(t)
	upstream := f.advanceOrigin(t, "upstream.txt")
	mustGit(t, f.WT, "fetch", "--quiet", "origin")
	b := f.setup(t)
	f.work(t, "a.txt")

	mustGit(t, f.WT, "-c", "core.hooksPath=", "rebase", "origin/main")

	res := Verify(f.Repo, b)
	// Every shape predicate still passes: the base is an ancestor, there is no
	// merge commit, and neither the base branch nor any remote ref moved. Only
	// the trailer notices.
	only(t, res, CheckForeignCommit)
	if !strings.Contains(res.Reason(), short(upstream)) {
		t.Fatalf("violation does not name the imported commit %s:\n%s", short(upstream), res.Reason())
	}
}

func TestVerifyCatchesACommitThatSkippedTheHooks(t *testing.T) {
	f := newFixture(t)
	b := f.setup(t)
	writeFile(t, f.WT+"/sneaky.txt", "sneaky\n")
	mustGit(t, f.WT, "add", "-A")
	mustGit(t, f.WT, "-c", "core.hooksPath=", "commit", "--quiet", "-m", "no trailer here")

	// Fail closed: an unstamped commit is treated as foreign, because a guard
	// that trusted unverifiable commits would verify nothing at all.
	only(t, Verify(f.Repo, b), CheckForeignCommit)
}

func TestVerifyCatchesAMergeCommit(t *testing.T) {
	f := newFixture(t)
	f.advanceOrigin(t, "upstream.txt")
	mustGit(t, f.WT, "fetch", "--quiet", "origin")
	b := f.setup(t)
	f.work(t, "a.txt")

	mustGit(t, f.WT, "-c", "core.hooksPath=", "merge", "--no-ff", "--no-edit", "origin/main")

	res := Verify(f.Repo, b)
	if !res.Has(CheckMergeCommit) {
		t.Fatalf("merge commit not reported:\n%s", res.Reason())
	}
}

func TestVerifyCatchesAPushThatReachedTheRemote(t *testing.T) {
	f := newFixture(t)
	b := f.setup(t)
	f.work(t, "a.txt")

	// Every structural block removed, which is the only way a push gets out.
	// They are unset rather than overridden with -c: both keys are multi-valued,
	// so -c would add to them and the blocked URL would still be tried.
	mustGit(t, f.WT, "config", "--worktree", "--unset-all", "remote.origin.pushurl")
	mustGit(t, f.WT, "config", "--worktree", "--remove-section", "url."+blockedPushURL+"/")
	mustGit(t, f.WT, "-c", "core.hooksPath=", "push", "--quiet", "origin", "HEAD:refs/heads/leak")

	res := Verify(f.Repo, b)
	only(t, res, CheckRemoteMoved)
	if !strings.Contains(res.Reason(), "refs/remotes/origin/leak") {
		t.Fatalf("violation does not name the ref that moved:\n%s", res.Reason())
	}
}

func TestVerifyCatchesTheBaseBranchMoving(t *testing.T) {
	f := newFixture(t)
	b := f.setup(t)
	f.work(t, "a.txt")

	writeFile(t, f.Repo+"/main-side.txt", "main\n")
	mustGit(t, f.Repo, "add", "-A")
	mustGit(t, f.Repo, "-c", "core.hooksPath=", "commit", "--quiet", "-m", "committed to main")

	res := Verify(f.Repo, b)
	if !res.Has(CheckBaseMoved) {
		t.Fatalf("base branch moving not reported:\n%s", res.Reason())
	}
}

// A branch that no longer contains the commit it was recorded against: a rewind,
// or a `rebase --onto` that dropped it.
func TestVerifyCatchesABranchThatLostItsBase(t *testing.T) {
	f := newFixture(t)
	b := f.setup(t)
	f.work(t, "a.txt")

	// The rewind. The branch is moved to a commit that is not a descendant of
	// the one it was cut at, which is what a `rebase --onto` onto something
	// else leaves behind.
	mustGit(t, f.Repo, "-c", "core.hooksPath=", "checkout", "--quiet", "--orphan", "elsewhere")
	writeFile(t, f.Repo+"/elsewhere.txt", "elsewhere\n")
	mustGit(t, f.Repo, "add", "-A")
	mustGit(t, f.Repo, "-c", "core.hooksPath=", "commit", "--quiet", "-m", "elsewhere")
	orphan := mustGit(t, f.Repo, "rev-parse", "HEAD")
	mustGit(t, f.Repo, "-c", "core.hooksPath=", "checkout", "--quiet", "main")
	mustGit(t, f.WT, "reset", "--hard", "--quiet", orphan)

	if !Verify(f.Repo, b).Has(CheckAncestor) {
		t.Fatalf("a branch rewound off its own base was accepted:\n%s", Verify(f.Repo, b).Reason())
	}
}

// The base branch moving ahead of a branch that was cut before it is not that,
// and must not be reported as it.
//
// It is the ordinary shape of a resumed run and of every worker in a continuous
// one: the integrator merges onto the checkout while a branch cut earlier is
// still out, so the checkout genuinely holds a commit that branch does not.
// Reported as base-not-ancestor it tells the worker to reset its branch and
// re-apply its work, which is the one thing that would actually damage it.
func TestVerifyAcceptsABranchTheCheckoutHasMovedAheadOf(t *testing.T) {
	f := newFixture(t)
	f.setup(t) // the first attempt: hooks installed, branch cut here
	f.work(t, "a.txt")

	writeFile(t, f.Repo+"/later.txt", "later\n")
	mustGit(t, f.Repo, "add", "-A")
	mustGit(t, f.Repo, "-c", "core.hooksPath=", "commit", "--quiet", "-m", "later")

	// Recorded again after the checkout moved, which is what a resumed attempt
	// does: the branch is already there and main is already ahead of where it
	// was cut.
	b := f.setup(t)

	if res := Verify(f.Repo, b); !res.OK {
		t.Fatalf("a branch the checkout moved ahead of was refused:\n%s", res.Reason())
	}
}

func TestVerifyCatchesAMissingBranch(t *testing.T) {
	f := newFixture(t)
	b := f.setup(t)
	b.Branch = "bd-auto/never-created"

	only(t, Verify(f.Repo, b), CheckBranchMissing)
}

func TestReasonSaysWhatToDoInstead(t *testing.T) {
	f := newFixture(t)
	b := f.setup(t)
	writeFile(t, f.WT+"/sneaky.txt", "sneaky\n")
	mustGit(t, f.WT, "add", "-A")
	mustGit(t, f.WT, "-c", "core.hooksPath=", "commit", "--quiet", "-m", "no trailer here")

	reason := Verify(f.Repo, b).Reason()
	if !strings.Contains(reason, "do this instead:") {
		t.Fatalf("feedback names no alternative:\n%s", reason)
	}
	if strings.TrimSpace(Result{OK: true}.Reason()) != "" {
		t.Fatal("a clean result must produce no feedback")
	}
}

func TestHasTrailer(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"trailer block", "subject\n\nbody\n\nBd-Auto: iss-1/1\n", true},
		{"other attempt of the same issue", "subject\n\nBd-Auto: iss-1/3\n", true},
		{"no attempt suffix", "subject\n\nBd-Auto: iss-1\n", true},
		{"alongside other trailers", "s\n\nCo-Authored-By: x <x@y>\nBd-Auto: iss-1/1\n", true},
		{"indented", "s\n\n  Bd-Auto: iss-1/1\n", true},
		{"another issue", "subject\n\nBd-Auto: iss-2/1\n", false},
		{"prefix of another issue", "subject\n\nBd-Auto: iss-11/1\n", false},
		{"no trailer", "subject\n\nbody\n", false},
		{"only mentioned in prose", "subject\n\nI thought about Bd-Auto: iss-1/1 here\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasTrailer(tc.body, "iss-1"); got != tc.want {
				t.Fatalf("hasTrailer(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
