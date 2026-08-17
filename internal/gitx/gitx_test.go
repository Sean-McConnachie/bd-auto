package gitx_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/gitx"
)

// Every test here asserts the same property twice: that plain git DOES fire the
// hook, and that gitx does not. The control half is not ceremony — a hooks
// directory that is misconfigured fires nothing either way, and without it this
// whole file would pass while proving nothing.

// repo builds a throwaway repo whose post-checkout, post-merge and pre-commit
// hooks each append their name to a marker file, standing in for beads' hooks
// running `bd hooks run <hook>`.
func repo(t *testing.T) (dir, marker string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	// The marker and the hooks live OUTSIDE the working tree. Inside it, an
	// `add -A` would commit the marker and the next merge would refuse to
	// overwrite it, which is a property of the fixture rather than of gitx.
	base := t.TempDir()
	dir = filepath.Join(base, "repo")
	marker = filepath.Join(base, "fired.log")
	hooks := filepath.Join(base, "hooks")
	for _, d := range []string{dir, hooks} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"post-checkout", "post-merge", "pre-commit"} {
		body := "#!/bin/sh\necho " + name + " >> " + marker + "\n"
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	plain(t, dir, "init", "--quiet", "-b", "main", ".")
	plain(t, dir, "config", "user.name", "bd-auto test")
	plain(t, dir, "config", "user.email", "test@example.invalid")
	plain(t, dir, "config", "commit.gpgsign", "false")
	plain(t, dir, "config", "core.hooksPath", hooks)
	write(t, filepath.Join(dir, "seed.txt"), "seed\n")
	plain(t, dir, "add", "-A")
	plain(t, dir, "commit", "--quiet", "--no-verify", "-m", "seed")
	truncate(t, marker)
	return dir, marker
}

// fired reports which hooks have run since the last truncate.
func fired(t *testing.T, marker string) []string {
	t.Helper()
	b, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

func truncate(t *testing.T, marker string) {
	t.Helper()
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// plain runs git the way anyone other than bd-auto runs it: hooks live.
func plain(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// TestWorktreeAddFiresNoHooks is the case that costs a bd-auto run the most.
// Every attempt creates a worktree, and beads' post-checkout hook imports
// .beads/issues.jsonl over its database — so without this, each new attempt
// reverts every close the run has earned since the last export.
func TestWorktreeAddFiresNoHooks(t *testing.T) {
	dir, marker := repo(t)

	if _, err := gitx.Run(dir, "worktree", "add", "--quiet", filepath.Join(dir, "..", "wt-quiet"), "-b", "quiet"); err != nil {
		t.Fatal(err)
	}
	if got := fired(t, marker); len(got) != 0 {
		t.Fatalf("gitx fired hooks on worktree add: %v", got)
	}

	// The control. Without it a broken hooks setup would pass the assertion
	// above for the wrong reason.
	plain(t, dir, "worktree", "add", "--quiet", filepath.Join(dir, "..", "wt-loud"), "-b", "loud")
	if got := fired(t, marker); len(got) == 0 {
		t.Fatal("plain git fired no hooks either, so this test proves nothing")
	}
}

// TestMergeFiresNoHooks is the barrier's case: a wave merges one branch per
// issue into the main checkout, and post-merge imports over the database each
// time.
func TestMergeFiresNoHooks(t *testing.T) {
	dir, marker := repo(t)

	branch := func(name, file string) {
		plain(t, dir, "checkout", "--quiet", "-b", name, "main")
		write(t, filepath.Join(dir, file), file+"\n")
		plain(t, dir, "add", "-A")
		plain(t, dir, "commit", "--quiet", "--no-verify", "-m", file)
		plain(t, dir, "checkout", "--quiet", "main")
	}
	branch("one", "one.txt")
	branch("two", "two.txt")
	truncate(t, marker)

	if _, err := gitx.Run(dir, "merge", "--no-edit", "--quiet", "one"); err != nil {
		t.Fatal(err)
	}
	if got := fired(t, marker); len(got) != 0 {
		t.Fatalf("gitx fired hooks on merge: %v", got)
	}

	plain(t, dir, "merge", "--no-edit", "--quiet", "two")
	if got := fired(t, marker); len(got) == 0 {
		t.Fatal("plain git fired no hooks either, so this test proves nothing")
	}
}

// TestCheckoutFiresNoHooks covers the barrier's other half: staging a run puts
// the main checkout on the epic branch, which is a checkout like any other.
func TestCheckoutFiresNoHooks(t *testing.T) {
	dir, marker := repo(t)

	if _, err := gitx.Run(dir, "checkout", "--quiet", "-b", "epic"); err != nil {
		t.Fatal(err)
	}
	if got := fired(t, marker); len(got) != 0 {
		t.Fatalf("gitx fired hooks on checkout: %v", got)
	}

	plain(t, dir, "checkout", "--quiet", "-b", "epic-loud")
	if got := fired(t, marker); len(got) == 0 {
		t.Fatal("plain git fired no hooks either, so this test proves nothing")
	}
}

// TestSuppressionIsPerInvocation is the limit of what bd-auto is entitled to do.
// Disabling a repository's hooks for anyone else — a worker committing in its
// worktree, a human in the same checkout — would be a different and much larger
// change than declining to fire them on bd-auto's own behalf.
func TestSuppressionIsPerInvocation(t *testing.T) {
	dir, marker := repo(t)

	if _, err := gitx.Run(dir, "checkout", "--quiet", "-b", "work"); err != nil {
		t.Fatal(err)
	}
	truncate(t, marker)

	write(t, filepath.Join(dir, "after.txt"), "after\n")
	plain(t, dir, "add", "-A")
	plain(t, dir, "commit", "--quiet", "-m", "after")

	if got := fired(t, marker); len(got) == 0 {
		t.Fatal("a gitx call left hooks disabled for the git calls after it")
	}
}
