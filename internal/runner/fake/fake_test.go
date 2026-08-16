package fake

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/runner"
)

func run(t *testing.T, r *Runner, req runner.Request) runner.Result {
	t.Helper()
	res, err := r.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// The point of the double: a scripted sequence of classes, replayed in order,
// so the engine's budget rules can be asserted without a model.
func TestScriptedClasses(t *testing.T) {
	r := New(Classes(runner.ClassInfraFailed, runner.ClassWorkFailed, runner.ClassOK)...)
	want := []runner.Class{runner.ClassInfraFailed, runner.ClassWorkFailed, runner.ClassOK}
	for i, w := range want {
		if got := run(t, r, runner.Request{Role: runner.RoleWorker}).Class; got != w {
			t.Errorf("call %d: Class = %s, want %s", i+1, got, w)
		}
	}
	// A script that has run out replays its last step, so a loop that runs one
	// round too many fails on an assertion about Calls rather than here.
	if got := run(t, r, runner.Request{}).Class; got != runner.ClassOK {
		t.Errorf("call 4: Class = %s, want the last step replayed", got)
	}
	if r.Calls() != 4 {
		t.Errorf("Calls = %d, want 4", r.Calls())
	}
}

func TestZeroValueSucceeds(t *testing.T) {
	var r Runner
	res := run(t, &r, runner.Request{Role: runner.RoleWorker})
	if res.Class != runner.ClassOK {
		t.Errorf("Class = %s, want ok from an unscripted fake", res.Class)
	}
	if res.ExitCode != 0 || res.Err != nil {
		t.Errorf("ExitCode = %d, Err = %v; want a clean success", res.ExitCode, res.Err)
	}
}

// Recording every request is the other half of the double. Without it a resume
// test can only assert what came back, not that the engine asked for the same
// session with the feedback in the prompt.
func TestRecordsEveryRequest(t *testing.T) {
	r := New(Step{Class: runner.ClassWorkFailed}, Step{Class: runner.ClassOK})
	run(t, r, runner.Request{Role: runner.RoleWorker, SessionID: "S1", Prompt: "implement bd-1"})
	run(t, r, runner.Request{Role: runner.RoleWorker, SessionID: "S1", Prompt: "the gate failed", Resume: true})

	got := r.Requests()
	if len(got) != 2 {
		t.Fatalf("Requests = %d, want 2", len(got))
	}
	if got[0].Resume {
		t.Error("the first request resumed")
	}
	if !got[1].Resume || got[1].SessionID != "S1" {
		t.Errorf("second request: Resume = %v, SessionID = %q; want a resume of S1", got[1].Resume, got[1].SessionID)
	}
	if !strings.Contains(got[1].Prompt, "gate failed") {
		t.Errorf("second prompt = %q, want the feedback in it", got[1].Prompt)
	}

	// The copy is defensive: a test must not be able to rewrite the record it
	// is asserting against.
	got[0].SessionID = "rewritten"
	if r.Requests()[0].SessionID != "S1" {
		t.Error("Requests returned the live slice")
	}
}

// A step's side effects are what make the checks between rounds real: the
// engine's progress check reads the worktree, not the Result.
func TestStepDoRuns(t *testing.T) {
	dir := t.TempDir()
	r := New(Step{
		Class: runner.ClassOK,
		Do: func(_ context.Context, req runner.Request) error {
			return os.WriteFile(filepath.Join(req.Dir, "worked.txt"), []byte("x"), 0o644)
		},
	})
	if res := run(t, r, runner.Request{Role: runner.RoleWorker, Dir: dir}); res.Class != runner.ClassOK {
		t.Fatalf("Class = %s (%v)", res.Class, res.Err)
	}
	if _, err := os.Stat(filepath.Join(dir, "worked.txt")); err != nil {
		t.Errorf("the step's side effect did not happen: %v", err)
	}
}

// A broken fixture is not a verdict on the work, so it fails the way an outage
// does rather than pretending the model produced something.
func TestStepDoErrorIsInfra(t *testing.T) {
	boom := errors.New("no such worktree")
	r := New(Step{Do: func(context.Context, runner.Request) error { return boom }})
	res := run(t, r, runner.Request{Role: runner.RoleWorker})
	if res.Class != runner.ClassInfraFailed {
		t.Errorf("Class = %s, want infra-failed", res.Class)
	}
	if !errors.Is(res.Err, boom) {
		t.Errorf("Err = %v, want it to wrap the step's error", res.Err)
	}
}

// Cancellation outranks the script, exactly as it does for a real backend.
func TestCancelWins(t *testing.T) {
	r := New(Step{Class: runner.ClassOK, Delay: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	res, err := r.Run(ctx, runner.Request{Role: runner.RoleWorker}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Class != runner.ClassInterrupted {
		t.Errorf("Class = %s, want interrupted", res.Class)
	}
	if res.TimedOut {
		t.Error("TimedOut = true for a cancellation")
	}
}

func TestTimeout(t *testing.T) {
	r := New(Step{Class: runner.ClassOK, Delay: 2 * time.Second})
	res := run(t, r, runner.Request{Role: runner.RoleWorker, Timeout: 20 * time.Millisecond})
	if res.Class != runner.ClassInterrupted {
		t.Errorf("Class = %s, want interrupted", res.Class)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false, want true")
	}
}

func TestEvents(t *testing.T) {
	var events []runner.Event
	sink := runner.SinkFunc(func(e runner.Event) { events = append(events, e) })
	r := New(Step{Class: runner.ClassWorkFailed, Text: "the gate failed"})
	if _, err := r.Run(context.Background(), runner.Request{Role: runner.RoleWorker, SessionID: "S1"}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var kinds []runner.EventKind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
		if e.Role != runner.RoleWorker || e.SessionID != "S1" {
			t.Errorf("event %s: role %q session %q", e.Kind, e.Role, e.SessionID)
		}
	}
	want := []runner.EventKind{runner.EventStart, runner.EventText, runner.EventError, runner.EventDone}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events = %v, want %v", kinds, want)
		}
	}
}

// The degraded path: a backend without resume makes the engine fall back to a
// fresh process with the feedback in its prompt, so the fake has to be able to
// claim less than it can do.
func TestCapsOverride(t *testing.T) {
	r := New()
	if !r.Caps().Resume {
		t.Error("an unset fake must claim the full capability set")
	}
	r.SetCaps(runner.Capabilities{Permissions: runner.AllPermissions()})
	if r.Caps().Resume {
		t.Error("SetCaps did not take")
	}
	if !r.Caps().Supports(runner.PermScoped) {
		t.Error("Supports(scoped) = false")
	}
}

func TestNoRepeatRunsOut(t *testing.T) {
	r := New(Step{Class: runner.ClassOK})
	r.Repeat = false
	run(t, r, runner.Request{})
	if _, err := r.Run(context.Background(), runner.Request{}, nil); err == nil {
		t.Fatal("want an error once the script is exhausted")
	}
}

// `provider: fake` has to resolve through the registry, because that is how a
// whole drain is run against a script: config says fake, the engine builds it
// the same way it builds anything.
func TestRegisteredProvider(t *testing.T) {
	scripted := New(Step{Class: runner.ClassWorkFailed, Text: "nope"})
	defer Install(scripted)()

	built, err := runner.New(runner.Spec{Provider: Provider, Model: "irrelevant"})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if built.Name() != Provider {
		t.Errorf("Name = %q, want %q", built.Name(), Provider)
	}
	res, err := built.Run(context.Background(), runner.Request{Role: runner.RoleReviewer}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Class != runner.ClassWorkFailed {
		t.Errorf("Class = %s, want the installed script's class", res.Class)
	}
	if len(scripted.Requests()) != 1 {
		t.Errorf("the installed runner recorded %d requests, want 1", len(scripted.Requests()))
	}
}

func TestInstallRestores(t *testing.T) {
	if Shared() != nil {
		t.Fatal("a runner is installed before any test installed one")
	}
	r := New()
	restore := Install(r)
	if Shared() != r {
		t.Error("Install did not take")
	}
	restore()
	if Shared() != nil {
		t.Error("the restore function did not remove the installed runner")
	}
}
