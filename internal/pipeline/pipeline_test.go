package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/config"
	"bd-auto/internal/gitx"
)

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := gitx.Cmd(dir, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func TestWorktreeDiffIncludesTheWholeUncommittedSnapshot(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "--quiet", "-b", "main", ".")
	gitTest(t, dir, "config", "user.name", "test")
	gitTest(t, dir, "config", "user.email", "test@example.invalid")
	for name, body := range map[string]string{"edited.txt": "before\n", "deleted.txt": "gone\n", "binary.bin": "before\x00bytes"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "commit", "--quiet", "-m", "seed")

	if err := os.WriteFile(filepath.Join(dir, "edited.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binary.bin"), []byte("after\x00bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("ignore me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := WorktreeDiff(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	got := string(diff)
	for _, want := range []string{"edited.txt", "deleted.txt", "binary.bin", "new.txt", "untracked", ".gitignore"} {
		if !strings.Contains(got, want) {
			t.Fatalf("complete diff missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "diff --git a/ignored.txt b/ignored.txt") {
		t.Fatalf("ignored file leaked into diff:\n%s", got)
	}
	if status := strings.TrimSpace(string(mustOutput(t, gitx.Cmd(dir, "diff", "--cached", "--name-only")))); status != "" {
		t.Fatalf("WorktreeDiff changed the real index: %q", status)
	}
}

func mustOutput(t *testing.T, cmd interface{ Output() ([]byte, error) }) []byte {
	t.Helper()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTailBoundsOutput(t *testing.T) {
	long := strings.Repeat("x", 10_000)
	got := Tail([]byte(long), 100)
	if len(got) > 200 {
		t.Fatalf("tail should be bounded, got %d bytes", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatal("truncation should be visible, otherwise output looks complete when it is not")
	}
	if short := Tail([]byte("hello"), 100); short != "hello" {
		t.Fatalf("short output should pass through unchanged, got %q", short)
	}
	if Tail([]byte("a\n\n"), 0) != "a" {
		t.Fatal("a zero budget should not truncate, only trim")
	}
}

func TestExecCaptures(t *testing.T) {
	env := Env{Dir: t.TempDir()}

	ok := Exec("ok", "echo hello", 10, 1000, env)
	if !ok.Passed || ok.ExitCode != 0 {
		t.Fatalf("expected pass, got %+v", ok)
	}
	if !strings.Contains(ok.Output, "hello") {
		t.Fatalf("stdout not captured: %q", ok.Output)
	}

	bad := Exec("bad", "echo to-stderr >&2; exit 3", 10, 1000, env)
	if bad.Passed || bad.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %+v", bad)
	}
	if !strings.Contains(bad.Output, "to-stderr") {
		t.Fatal("stderr must be captured too: it is usually where the failure is explained")
	}
}

func TestExecTimeout(t *testing.T) {
	r := Exec("slow", "sleep 5", 1, 1000, Env{Dir: t.TempDir()})
	if r.Passed || !r.TimedOut {
		t.Fatalf("expected a timeout, got %+v", r)
	}
}

func TestExecExposesEnvToStages(t *testing.T) {
	env := Env{Issue: "bd-42", Branch: "bd-auto/bd-42", Dir: t.TempDir(), RepoRoot: "/repo"}
	r := Exec("env", `echo "$BD_ISSUE|$BD_BRANCH|$BD_REPO_ROOT"`, 10, 1000, env)
	if !r.Passed {
		t.Fatalf("stage failed: %+v", r)
	}
	if !strings.Contains(r.Output, "bd-42|bd-auto/bd-42|/repo") {
		t.Fatalf("stage env not exposed, got %q", r.Output)
	}
}

func TestGateStopsAtFirstFailure(t *testing.T) {
	cfg := config.Default()
	cfg.Gate = []config.Command{
		{Name: "first", Run: "true", Timeout: 10},
		{Name: "second", Run: "exit 1", Timeout: 10},
		{Name: "third", Run: "echo should-not-run", Timeout: 10},
	}
	cfg.OutputTailBytes = 1000

	rs := Gate(cfg, Env{Dir: t.TempDir()})
	if len(rs) != 2 {
		t.Fatalf("gate should stop at the first failure, ran %d commands", len(rs))
	}
	if Passed(rs) {
		t.Fatal("gate should not pass")
	}
	if f := FirstFailure(rs); f == nil || f.Name != "second" {
		t.Fatalf("wrong failure reported: %+v", f)
	}
}

func TestGateWithNoCommandsPasses(t *testing.T) {
	cfg := config.Default()
	rs := Gate(cfg, Env{Dir: t.TempDir()})
	if !Passed(rs) || len(rs) != 0 {
		t.Fatal("a repo with no gate configured must pass trivially")
	}
	if FirstFailure(rs) != nil {
		t.Fatal("no commands means no failure")
	}
}

func TestSummaryIsReadable(t *testing.T) {
	s := Summary([]Result{
		{Name: "build", Passed: true, Seconds: 1.2},
		{Name: "test", Passed: false, ExitCode: 1, Seconds: 3.4},
		{Name: "slow", Passed: false, TimedOut: true},
	})
	for _, want := range []string{"PASS build", "FAIL test", "exit 1", "timed out"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary missing %q:\n%s", want, s)
		}
	}
}
