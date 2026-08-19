package fake

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/runner"
)

func execFake(t *testing.T, argv ...string) runner.Runner {
	t.Helper()
	// Shared() takes precedence in factory, so a test that left one installed
	// would silently get that instead of the exec runner.
	if r := Shared(); r != nil {
		t.Fatal("a shared fake is installed; this test needs the factory's own path")
	}
	r, err := factory(runner.Spec{Provider: Provider, ExtraArgs: argv})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return r
}

// TestTheExecFakeDoesTheWorkInTheWorktree is why this exists: `provider: fake`
// with no command changes nothing, so a whole drain under it parks every issue
// and tests only the failure path.
func TestTheExecFakeDoesTheWorkInTheWorktree(t *testing.T) {
	dir := t.TempDir()
	r := execFake(t, "sh", "-c", "printf done > made.txt")

	res, err := r.Run(context.Background(), runner.Request{Role: runner.RoleWorker, Dir: dir}, runner.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Class != runner.ClassOK {
		t.Fatalf("class %s, want ok: %v", res.Class, res.Err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "made.txt"))
	if err != nil {
		t.Fatalf("the command did not run in the worktree: %v", err)
	}
	if string(got) != "done" {
		t.Fatalf("made.txt is %q", got)
	}
}

// A command that fails is work that failed, not infrastructure that broke.
// Classing it infra would spend the run's retry budget on a test's own
// assertion failing, which is the taxonomy beads-auto-imp-6no exists to keep
// straight.
func TestAFailingCommandIsAWorkFailureNotAnOutage(t *testing.T) {
	r := execFake(t, "sh", "-c", "echo nope >&2; exit 3")

	res, err := r.Run(context.Background(), runner.Request{Role: runner.RoleWorker, Dir: t.TempDir()}, runner.Discard)
	if err != nil {
		t.Fatalf("Run returned an error rather than a failed result: %v", err)
	}
	if res.Class != runner.ClassWorkFailed {
		t.Fatalf("class %s, want %s", res.Class, runner.ClassWorkFailed)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Text, "nope") {
		t.Fatalf("the command's output is not on the result: %q", res.Text)
	}
}

// The command is handed what it needs to act like a worker, and the prompt so
// it can tell an engine that stopped filling one in from one that did not.
func TestTheCommandIsToldWhereItIsAndWhatItWasAsked(t *testing.T) {
	dir := t.TempDir()
	r := execFake(t, "sh", "-c", `printf '%s\n%s\n%s\n' "$BD_WORKTREE" "$BD_ROLE" "$BD_PROMPT" > env.txt`)

	if _, err := r.Run(context.Background(), runner.Request{
		Role: runner.RoleWorker, Dir: dir, Prompt: "close t-1",
	}, runner.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{dir, string(runner.RoleWorker), "close t-1"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the command was not told %q; it saw:\n%s", want, got)
		}
	}
}

// It emits the same shape of events a real backend does, or the wave table and
// the transcript show an idle row for the whole of it.
func TestTheExecFakeReportsItselfToTheSink(t *testing.T) {
	var kinds []runner.EventKind
	sink := runner.SinkFunc(func(e runner.Event) { kinds = append(kinds, e.Kind) })
	r := execFake(t, "true")

	if _, err := r.Run(context.Background(), runner.Request{Role: runner.RoleWorker, Dir: t.TempDir()}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := map[runner.EventKind]bool{runner.EventStart: false, runner.EventToolUse: false, runner.EventDone: false}
	for _, k := range kinds {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("no %s event; a row driven by this fake would look idle", k)
		}
	}
}

// No resume: a command is not a session, and claiming otherwise would send a
// round of feedback into a process with no memory of what it is answering.
func TestTheExecFakeDoesNotClaimResume(t *testing.T) {
	if execFake(t, "true").Caps().Resume {
		t.Fatal("the exec fake claims resume")
	}
}

// Without ExtraArgs the factory keeps returning the scripted fake, so nothing
// that already used `provider: fake` changes behaviour.
func TestNoCommandStillGivesTheScriptedFake(t *testing.T) {
	r, err := factory(runner.Spec{Provider: Provider})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(*Runner); !ok {
		t.Fatalf("provider: fake with no command returned %T", r)
	}
}
