package cmds

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bd-auto/internal/bd"
	"bd-auto/internal/runstate"
)

// A manual dispatch must refuse before it asks bd anything or changes any
// part of the run. In particular WaveIssues is the live drain's list of work
// that has not landed yet, so losing it can start a dependent on the wrong
// base commit.
func TestPlanDispatchRefusesWhileADrainOwnsTheRun(t *testing.T) {
	repo := planRepo(t)
	st := runstate.New("epic-1", 2, "auto", 1)
	st.Wave = 7
	st.WaveIssues = []string{"t-live"}
	st.InFlight["t-live"] = runstate.Attempt{Branch: "bd-auto/t-live", Attempt: 2}
	st.Attempts["t-live"] = 2
	st.Done = []string{"t-done"}
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}
	before, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}

	called := planBD(t, `[{"id":"t-new","title":"new","status":"open","priority":2,"issue_type":"task"}]`)
	release, err := runstate.Hold(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	err = Plan([]string{"--dispatch"})
	if err == nil || !strings.Contains(err.Error(), "another drain") || !strings.Contains(err.Error(), "without --dispatch") {
		t.Fatalf("Plan --dispatch error = %v, want an actionable live-drain refusal", err)
	}
	if raw, _ := os.ReadFile(called); len(raw) != 0 {
		t.Fatalf("the refused plan invoked bd: %s", raw)
	}
	after, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		b, _ := json.MarshalIndent(before, "", "  ")
		a, _ := json.MarshalIndent(after, "", "  ")
		t.Fatalf("refusal changed run state\nbefore: %s\nafter: %s", b, a)
	}
}

func TestPlainPlanRemainsAvailableWhileADrainOwnsTheRun(t *testing.T) {
	repo := planRepo(t)
	st := runstate.New("epic-1", 2, "auto", 1)
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}
	called := planBD(t, `[{"id":"t-new","title":"new","status":"open","priority":2,"issue_type":"task"}]`)
	release, err := runstate.Hold(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := discardStdout(t, func() error { return Plan(nil) }); err != nil {
		t.Fatalf("plain Plan beside a drain: %v", err)
	}
	if raw, _ := os.ReadFile(called); !strings.Contains(string(raw), "ready") {
		t.Fatalf("plain planning did not invoke bd ready: %q", raw)
	}
	got, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Wave != 0 || len(got.WaveIssues) != 0 || len(got.InFlight) != 0 {
		t.Fatalf("plain planning mutated the run: %+v", got)
	}
}

// StatusActive survives an abandoned process so the run can be resumed. The
// kernel lock, not that status, is the evidence that dispatch would collide.
func TestPlanDispatchesAnAbandonedActiveRunWhenNoProcessOwnsTheLock(t *testing.T) {
	repo := planRepo(t)
	st := runstate.New("epic-1", 2, "auto", 1)
	st.Status = runstate.StatusActive
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}
	planBD(t, `[{"id":"t-new","title":"new","status":"open","priority":2,"issue_type":"task"}]`)

	if err := discardStdout(t, func() error { return Plan([]string{"--dispatch"}) }); err != nil {
		t.Fatalf("manual dispatch without a live owner: %v", err)
	}
	got, err := runstate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Wave != 1 || !reflect.DeepEqual(got.WaveIssues, []string{"t-new"}) {
		t.Fatalf("recorded wave = %d, issues = %v", got.Wave, got.WaveIssues)
	}
	if _, ok := got.InFlight["t-new"]; !ok {
		t.Fatal("dispatched issue was not recorded in flight")
	}
}

func planRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_AUTO_REPO", repo)
	t.Chdir(repo)
	return repo
}

func planBD(t *testing.T, ready string) string {
	t.Helper()
	dir := t.TempDir()
	called := filepath.Join(dir, "called")
	script := filepath.Join(dir, "bd")
	body := "#!/usr/bin/env sh\nprintf '%s\\n' \"$*\" >> '" + called + "'\nprintf '%s\\n' '" + ready + "'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := bd.Binary
	bd.Binary = script
	t.Cleanup(func() { bd.Binary = prev })
	return called
}

func discardStdout(t *testing.T, fn func() error) error {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		return err
	}
	old := os.Stdout
	os.Stdout = f
	err = fn()
	os.Stdout = old
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
