package gitguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testIssue  = "iss-1"
	testBranch = "bd-auto/iss-1"
)

// fixture is a main checkout with a fake origin, a worker worktree, and a hooks
// directory the repo already had before bd-auto touched it. The last part is
// the point: this repo really does set core.hooksPath, to beads' own hooks, and
// a guard that quietly disabled them would break issue tracking inside every
// worker worktree.
type fixture struct {
	Root   string
	Repo   string
	Origin string
	WT     string
	Prev   string // hooks directory the repo already had
	Marker string // file the prev hooks append to when they run
	Base   string // main's commit when the worktree was created
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	root := t.TempDir()
	f := &fixture{
		Root:   root,
		Repo:   filepath.Join(root, "work"),
		Origin: filepath.Join(root, "origin"),
		Prev:   filepath.Join(root, "prev-hooks"),
		Marker: filepath.Join(root, "hooks-that-ran"),
	}

	mustGit(t, root, "init", "--quiet", "--bare", "-b", "main", f.Origin)
	mustGit(t, root, "init", "--quiet", "-b", "main", f.Repo)
	mustGit(t, f.Repo, "config", "user.name", "bd-auto test")
	mustGit(t, f.Repo, "config", "user.email", "test@example.invalid")
	mustGit(t, f.Repo, "config", "commit.gpgsign", "false")

	// The hooks this repo already had. They stand in for beads' own hooks: each
	// records that it ran, so a test can prove the chain reached it.
	if err := os.MkdirAll(f.Prev, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-commit", "post-checkout", "prepare-commit-msg"} {
		body := "#!/usr/bin/env sh\necho " + name + " >> '" + f.Marker + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(f.Prev, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, f.Repo, "config", "core.hooksPath", f.Prev)

	writeFile(t, filepath.Join(f.Repo, "seed.txt"), "seed\n")
	mustGit(t, f.Repo, "add", "-A")
	mustGit(t, f.Repo, "commit", "--quiet", "-m", "seed")
	mustGit(t, f.Repo, "remote", "add", "origin", f.Origin)
	mustGit(t, f.Repo, "push", "--quiet", "origin", "main")
	f.Base = mustGit(t, f.Repo, "rev-parse", "HEAD")

	f.WT = filepath.Join(f.Repo, ".beads", "auto", "wt", testIssue)
	mustGit(t, f.Repo, "worktree", "add", "--quiet", "-b", testBranch, f.WT, f.Base)
	return f
}

// setup installs the guard and returns the baseline recorded at dispatch.
func (f *fixture) setup(t *testing.T) Baseline {
	t.Helper()
	if err := Setup(f.Repo, f.WT, Worker{Issue: testIssue, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	b, err := Record(f.Repo, testIssue, testBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// work makes an ordinary worker commit, through the installed hooks.
func (f *fixture) work(t *testing.T, name string) string {
	t.Helper()
	writeFile(t, filepath.Join(f.WT, name), name+"\n")
	mustGit(t, f.WT, "add", "-A")
	mustGit(t, f.WT, "commit", "--quiet", "-m", "worker: "+name)
	return mustGit(t, f.WT, "rev-parse", "HEAD")
}

// advanceOrigin puts a commit on origin/main that the worker never wrote, and
// leaves the local main where it was. This is the upstream every hole in the
// structural guard ends up importing.
func (f *fixture) advanceOrigin(t *testing.T, name string) string {
	t.Helper()
	writeFile(t, filepath.Join(f.Repo, name), name+"\n")
	mustGit(t, f.Repo, "add", "-A")
	mustGit(t, f.Repo, "-c", "core.hooksPath=", "commit", "--quiet", "-m", "upstream: "+name)
	sha := mustGit(t, f.Repo, "rev-parse", "HEAD")
	mustGit(t, f.Repo, "push", "--quiet", "origin", "main")
	mustGit(t, f.Repo, "reset", "--hard", "--quiet", f.Base)
	return sha
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

func lastMessage(t *testing.T, dir string) string {
	t.Helper()
	return mustGit(t, dir, "log", "-1", "--format=%B")
}

// --- structural half ---

// The trap this repo actually has: core.hooksPath is already set, so a rejector
// directory that does not chain would silently disable beads' own hooks inside
// every worker worktree.
func TestGeneratedHooksChainToTheHooksTheRepoAlreadyHad(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	if err := os.Remove(f.Marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	f.work(t, "a.txt")

	ran := readFile(t, f.Marker)
	if !strings.Contains(ran, "pre-commit") {
		t.Fatalf("the repo's own pre-commit did not run inside the worktree; markers: %q", ran)
	}
	if !strings.Contains(ran, "prepare-commit-msg") {
		t.Fatalf("the repo's own prepare-commit-msg did not run inside the worktree; markers: %q", ran)
	}
}

func TestCommitsInTheWorktreeCarryTheAttemptTrailer(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	f.work(t, "a.txt")

	msg := lastMessage(t, f.WT)
	if !strings.Contains(msg, "Bd-Auto: "+testIssue+"/1") {
		t.Fatalf("commit message carries no trailer:\n%s", msg)
	}
	// A second commit must not stack a second trailer onto an amend.
	mustGit(t, f.WT, "commit", "--quiet", "--amend", "--no-edit")
	if n := strings.Count(lastMessage(t, f.WT), "Bd-Auto:"); n != 1 {
		t.Fatalf("amend produced %d trailers, want 1", n)
	}
}

// The other trap: --worktree only scopes anything once extensions.worktreeConfig
// is on. Get that wrong and the main checkout inherits the rejectors.
func TestSetupLeavesTheMainCheckoutAlone(t *testing.T) {
	f := newFixture(t)
	f.setup(t)

	if got := mustGit(t, f.Repo, "config", "--get", "core.hooksPath"); got != f.Prev {
		t.Fatalf("main checkout hooksPath is now %q, want %q", got, f.Prev)
	}
	if got, _ := git(f.Repo, "config", "--get", "remote.origin.pushurl"); got != "" {
		t.Fatalf("main checkout push URL was rewritten to %q", got)
	}
	// And it can still do the things a worker cannot.
	writeFile(t, filepath.Join(f.Repo, "main.txt"), "main\n")
	mustGit(t, f.Repo, "add", "-A")
	mustGit(t, f.Repo, "commit", "--quiet", "-m", "main-side commit")
	if strings.Contains(lastMessage(t, f.Repo), "Bd-Auto:") {
		t.Fatal("the main checkout must not be stamped with a worker trailer")
	}
	if _, err := git(f.Repo, "push", "--quiet", "origin", "main"); err != nil {
		t.Fatalf("the main checkout must still be able to push: %v", err)
	}
}

func TestPushFromTheWorktreeIsBlocked(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	f.work(t, "a.txt")

	// --no-verify and a bare URL are the two ways round the hooks, and a worker
	// that has been told a push is mandatory will find both.
	cases := []struct {
		name string
		args []string
	}{
		{"named remote", []string{"push", "origin", "HEAD:refs/heads/leak"}},
		{"no-verify", []string{"push", "--no-verify", "origin", "HEAD:refs/heads/leak"}},
		{"bare url", []string{"push", f.Origin, "HEAD:refs/heads/leak"}},
		{"bare url, hooks bypassed", []string{"-c", "core.hooksPath=", "push", "--no-verify", f.Origin, "HEAD:refs/heads/leak"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := git(f.WT, tc.args...)
			if err == nil {
				t.Fatal("push succeeded from a worker worktree")
			}
			// The block lands before any hook runs, so the only channel left is
			// the URL git echoes back. It is spelled to be read.
			if !strings.Contains(err.Error(), "bd-auto-do-not-push-commit-to-your-branch-instead") {
				t.Fatalf("rejection does not say what to do instead: %v", err)
			}
		})
	}

	if out, _ := git(f.Origin, "branch", "--list", "leak"); out != "" {
		t.Fatalf("something reached the remote anyway: %q", out)
	}
}

// The pre-push hook is the block in the one case a push URL cannot cover: a
// repo that sets its own pushurl, whose value bd-auto cannot displace from a
// worktree. Its message is the one a worker should actually read.
func TestThePrePushHookRefusesAndSaysWhatToDoInstead(t *testing.T) {
	f := newFixture(t)
	f.setup(t)

	dir, err := HooksDir(f.WT)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, "pre-push"), "origin", f.Origin)
	cmd.Dir = f.WT
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("the pre-push hook allowed the push")
	}
	for _, want := range []string{"bd-auto", "Commit to your own branch", "integrator"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("pre-push message is missing %q:\n%s", want, out)
		}
	}
}

func TestRebaseAndMergeFromTheWorktreeAreBlocked(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	f.advanceOrigin(t, "upstream.txt")
	mustGit(t, f.WT, "fetch", "--quiet", "origin")
	f.work(t, "a.txt")

	_, err := git(f.WT, "rebase", "origin/main")
	if err == nil {
		t.Fatal("rebase succeeded inside a worker worktree")
	}
	if !strings.Contains(err.Error(), "bd-auto") || !strings.Contains(err.Error(), "integrator") {
		t.Fatalf("rebase rejection does not say what to do instead: %v", err)
	}

	_, err = git(f.WT, "merge", "--no-ff", "--no-edit", "origin/main")
	if err == nil {
		t.Fatal("merge commit succeeded inside a worker worktree")
	}
	if !strings.Contains(err.Error(), "bd-auto") || !strings.Contains(err.Error(), "integrator") {
		t.Fatalf("merge rejection does not say what to do instead: %v", err)
	}
	mustGit(t, f.WT, "merge", "--abort")
}

// Setup runs again for every attempt. It reads the chain from the main
// checkout, so a second call must not chain the generated hooks to themselves.
func TestSetupIsRepeatableAndNeverChainsToItself(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	if err := Setup(f.Repo, f.WT, Worker{Issue: testIssue, Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.Marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	f.work(t, "a.txt")

	if !strings.Contains(readFile(t, f.Marker), "pre-commit") {
		t.Fatal("the chain was lost on the second Setup")
	}
	if !strings.Contains(lastMessage(t, f.WT), "Bd-Auto: "+testIssue+"/2") {
		t.Fatalf("second attempt did not restamp the trailer:\n%s", lastMessage(t, f.WT))
	}
}

func TestSetupRefusesAConfigItWouldCorrupt(t *testing.T) {
	f := newFixture(t)
	// A path that exists, because git refuses to run at all with a core.worktree
	// that does not, and the check has to fire on the case that reaches us.
	mustGit(t, f.Repo, "config", "core.worktree", f.Repo)
	err := Setup(f.Repo, f.WT, Worker{Issue: testIssue, Attempt: 1})
	if err == nil {
		t.Fatal("want an error when core.worktree is set in the shared config")
	}
	if !strings.Contains(err.Error(), "core.worktree") {
		t.Fatalf("error does not name the problem: %v", err)
	}
}
