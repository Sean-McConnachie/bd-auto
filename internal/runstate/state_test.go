package runstate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func tempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestActiveFalseWithoutRun(t *testing.T) {
	dir := tempRepo(t)
	if Active(dir) {
		t.Fatal("Active should be false with no run state; hooks must no-op in ordinary sessions")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := tempRepo(t)
	st := New("epic-1", 5, "auto", 1)
	st.WaveIssues = []string{"a", "b"}
	if err := Save(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Epic != "epic-1" || got.Concurrency != 5 || len(got.WaveIssues) != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if !Active(dir) {
		t.Fatal("Active should be true after Save")
	}
}

func TestClearDisarms(t *testing.T) {
	dir := tempRepo(t)
	if err := Save(dir, New("e", 1, "auto", 1)); err != nil {
		t.Fatal(err)
	}
	if err := Clear(dir); err != nil {
		t.Fatal(err)
	}
	if Active(dir) {
		t.Fatal("Clear must disarm every hook")
	}
	if err := Clear(dir); err != nil {
		t.Fatalf("Clear must be idempotent, got %v", err)
	}
}

// TestConcurrentUpdateDoesNotLoseWrites is the regression test for the failure
// the eqc.1 spike found in bd's own notes field: five concurrent
// read-modify-writes, all reporting success, with one silently lost.
//
// The run state is written concurrently by PreToolUse hooks firing inside
// parallel workers, so it must not repeat that bug.
func TestConcurrentUpdateDoesNotLoseWrites(t *testing.T) {
	dir := tempRepo(t)
	if err := Save(dir, New("epic", 5, "auto", 1)); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := Update(dir, false, func(s *State) error {
				s.Bindings[fmt.Sprintf("agent-%d", i)] = fmt.Sprintf("issue-%d", i)
				return nil
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Update failed: %v", err)
		}
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Bindings) != n {
		t.Fatalf("lost writes: want %d bindings, got %d", n, len(got.Bindings))
	}
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("agent-%d", i)
		if got.Bindings[k] != fmt.Sprintf("issue-%d", i) {
			t.Fatalf("binding %s missing or wrong: %q", k, got.Bindings[k])
		}
	}
}

// TestHelperProcess is not a real test. It is the child half of
// TestConcurrentUpdateAcrossProcesses, re-executed as a separate process.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("BD_AUTO_HELPER") != "1" {
		return
	}
	_, err := Update(os.Getenv("BD_AUTO_HELPER_DIR"), false, func(s *State) error {
		s.Bindings[os.Getenv("BD_AUTO_HELPER_KEY")] = "issue"
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestConcurrentUpdateAcrossProcesses proves the lock is a real file lock and
// not merely a mutex. This matters because every hook fires as its own process:
// five workers claiming at once means five separate bd-auto processes writing
// this file, which is precisely the shape that loses data when unlocked.
func TestConcurrentUpdateAcrossProcesses(t *testing.T) {
	dir := tempRepo(t)
	if err := Save(dir, New("epic", 5, "auto", 1)); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	fails := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
			cmd.Env = append(os.Environ(),
				"BD_AUTO_HELPER=1",
				"BD_AUTO_HELPER_DIR="+dir,
				fmt.Sprintf("BD_AUTO_HELPER_KEY=proc-%d", i),
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				fails <- fmt.Sprintf("proc %d: %v: %s", i, err, out)
			}
		}(i)
	}
	wg.Wait()
	close(fails)
	for f := range fails {
		t.Fatalf("helper process failed: %s", f)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Bindings) != n {
		t.Fatalf("cross-process lost writes: want %d bindings, got %d", n, len(got.Bindings))
	}
}

func TestParkAndDone(t *testing.T) {
	s := New("e", 5, "auto", 1)
	s.InFlight["a"] = Attempt{Branch: "bd-auto/a", Attempt: 1}
	s.InFlight["b"] = Attempt{Branch: "bd-auto/b", Attempt: 2}
	s.Attempts["b"] = 2

	s.MarkDone("a")
	if !s.IsDone("a") || len(s.InFlight) != 1 {
		t.Fatalf("MarkDone should complete a and clear its in-flight entry: %+v", s)
	}
	s.Park("b", "gate never passed", "gate")
	if !s.IsParked("b") || len(s.InFlight) != 0 {
		t.Fatalf("Park should set b aside: %+v", s)
	}
	if s.Parked[0].Attempts != 2 {
		t.Fatalf("parked issue should carry its attempt count, got %d", s.Parked[0].Attempts)
	}
	if !s.Excluded("a") || !s.Excluded("b") {
		t.Fatal("done and parked issues must both be excluded from a new wave")
	}
	if s.Excluded("c") {
		t.Fatal("an untouched issue must not be excluded")
	}
}

func TestNotesAreCapped(t *testing.T) {
	s := New("e", 1, "auto", 1)
	for i := 0; i < maxNotes*3; i++ {
		s.Note("note %d", i)
	}
	if len(s.Notes) != maxNotes {
		t.Fatalf("notes must stay capped at %d, got %d", maxNotes, len(s.Notes))
	}
}
