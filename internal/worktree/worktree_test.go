package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo builds a throwaway repo with one commit. Global and system git
// config are pointed at /dev/null so the developer's own settings, hooks
// included, cannot change what these tests observe.
func testRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet", "-b", "main", ".")
	mustGit(t, dir, "config", "user.name", "bd-auto test")
	mustGit(t, dir, "config", "user.email", "test@example.invalid")
	mustGit(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "seed.txt"), "seed\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "--quiet", "-m", "seed")
	return dir
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func head(t *testing.T, dir string) string {
	t.Helper()
	return mustGit(t, dir, "rev-parse", "HEAD")
}

// commitIn makes a commit inside a worktree without going through any hook, so
// worktree tests stay independent of gitguard.
func commitIn(t *testing.T, dir, name, msg string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, name), name+"\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "core.hooksPath=", "commit", "--quiet", "-m", msg)
	return head(t, dir)
}

func TestPathIsStableAndInsideTheRunDirectory(t *testing.T) {
	repo := "/repo"
	got := Path(repo, "beads-auto-imp-wz9.5")
	want := filepath.Join(repo, ".beads", "auto", "wt", "beads-auto-imp-wz9.5")
	if got != want {
		t.Fatalf("path %q, want %q", got, want)
	}
	if Path(repo, "beads-auto-imp-wz9.5") != got {
		t.Fatal("path is not stable across calls")
	}
}

// A worktree path is derived from an ID bd hands us, so it must not be able to
// point outside the run directory.
func TestPathCannotEscapeTheRunDirectory(t *testing.T) {
	for _, id := range []string{"../../etc/passwd", "..", "a/b", "/abs"} {
		got := Path("/repo", id)
		if !strings.HasPrefix(got, Root("/repo")+string(filepath.Separator)) {
			t.Fatalf("id %q escaped the root: %s", id, got)
		}
	}
}

func TestEnsureCreatesTheWorktree(t *testing.T) {
	repo := testRepo(t)
	base := head(t, repo)

	path, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base)
	if err != nil {
		t.Fatal(err)
	}
	if path != Path(repo, "iss-1") {
		t.Fatalf("path %q, want %q", path, Path(repo, "iss-1"))
	}
	if _, err := os.Stat(filepath.Join(path, "seed.txt")); err != nil {
		t.Fatalf("worktree was not checked out: %v", err)
	}
	if got := mustGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "bd-auto/iss-1" {
		t.Fatalf("branch %q, want bd-auto/iss-1", got)
	}
	if got := head(t, path); got != base {
		t.Fatalf("branched from %s, want %s", got, base)
	}
}

// Reuse is the whole reason the path is stable: a later round has to land in
// the same directory, with the previous round's work still in it.
func TestEnsureReusesTheWorktreeAndKeepsItsWork(t *testing.T) {
	repo := testRepo(t)
	base := head(t, repo)
	path, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base)
	if err != nil {
		t.Fatal(err)
	}
	work := commitIn(t, path, "round1.txt", "round one")
	writeFile(t, filepath.Join(path, "dirty.txt"), "uncommitted\n")

	again, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base)
	if err != nil {
		t.Fatal(err)
	}
	if again != path {
		t.Fatalf("reuse moved the worktree: %s then %s", path, again)
	}
	if got := head(t, path); got != work {
		t.Fatalf("reuse reset the branch to %s, want %s", got, work)
	}
	if _, err := os.Stat(filepath.Join(path, "dirty.txt")); err != nil {
		t.Fatalf("reuse discarded uncommitted work: %v", err)
	}
}

// The adopt half of adopt-or-recreate: a discarded attempt deletes the branch
// and can leave the tree behind.
func TestEnsureAdoptsAWorktreeWhoseBranchIsGone(t *testing.T) {
	repo := testRepo(t)
	base := head(t, repo)
	path, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base)
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, path, "round1.txt", "round one")
	mustGit(t, repo, "update-ref", "-d", "refs/heads/bd-auto/iss-1")

	again, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base)
	if err != nil {
		t.Fatal(err)
	}
	if again != path {
		t.Fatalf("adopt moved the worktree: %s then %s", path, again)
	}
	if got := mustGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "bd-auto/iss-1" {
		t.Fatalf("branch %q, want bd-auto/iss-1", got)
	}
	if !branchExists(repo, "bd-auto/iss-1") {
		t.Fatal("branch was not recreated")
	}
}

// The recreate half: the directory is gone but git still has it registered, so
// a plain `git worktree add` would refuse the path.
func TestEnsureRecreatesAfterTheDirectoryIsDeleted(t *testing.T) {
	repo := testRepo(t)
	base := head(t, repo)
	path, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base)
	if err != nil {
		t.Fatal(err)
	}
	work := commitIn(t, path, "round1.txt", "round one")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}

	again, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base)
	if err != nil {
		t.Fatal(err)
	}
	if again != path {
		t.Fatalf("recreate moved the worktree: %s then %s", path, again)
	}
	// The branch survived, so the work does too.
	if got := head(t, path); got != work {
		t.Fatalf("head %s, want the surviving branch tip %s", got, work)
	}
}

func TestEnsureRefusesABranchCheckedOutElsewhere(t *testing.T) {
	repo := testRepo(t)
	base := head(t, repo)
	other := filepath.Join(t.TempDir(), "elsewhere")
	mustGit(t, repo, "worktree", "add", "--quiet", "-b", "bd-auto/iss-1", other, base)

	if _, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base); err == nil {
		t.Fatal("want an error when the branch is checked out in another worktree")
	}
}

func TestRemoveDropsTheWorktreeAndKeepsTheBranch(t *testing.T) {
	repo := testRepo(t)
	base := head(t, repo)
	path, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base)
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, path, "round1.txt", "round one")

	if err := Remove(repo, "iss-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory survived: %v", err)
	}
	if _, ok := lookupPath(repo, path); ok {
		t.Fatal("worktree is still registered")
	}
	if !branchExists(repo, "bd-auto/iss-1") {
		t.Fatal("removing the worktree must not delete the branch; the integrator still needs it")
	}
	// Removing something that is already gone is not an error: cleanup runs
	// after a merge and again at the end of a run.
	if err := Remove(repo, "iss-1"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

func TestPruneDropsStaleRegistrations(t *testing.T) {
	repo := testRepo(t)
	base := head(t, repo)
	path, err := Ensure(repo, "iss-1", "bd-auto/iss-1", base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := Prune(repo); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupPath(repo, path); ok {
		t.Fatal("stale registration survived the prune")
	}
}

func TestSnapshotSeesCommitsAndDirtyFiles(t *testing.T) {
	repo := testRepo(t)
	path, err := Ensure(repo, "iss-1", "bd-auto/iss-1", head(t, repo))
	if err != nil {
		t.Fatal(err)
	}

	mark := Snapshot(path)
	if Changed(path, mark) {
		t.Fatal("an untouched worktree must compare unchanged")
	}
	writeFile(t, filepath.Join(path, "untracked.txt"), "new\n")
	if !Changed(path, mark) {
		t.Fatal("a new untracked file is progress and must be visible")
	}

	mark = Snapshot(path)
	commitIn(t, path, "committed.txt", "work")
	if !Changed(path, mark) {
		t.Fatal("a new commit must be visible")
	}
}
