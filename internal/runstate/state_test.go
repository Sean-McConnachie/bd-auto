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
		t.Fatal("Active should be false with no run state; a repo with no run must look idle")
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

// The failure record is bd-auto's own copy of why an attempt failed, and the
// reason it exists is that the copy on the issue does not survive a checkout.
// It is worth nothing unless it survives a process, so that is what is pinned.
func TestFailuresSurviveASaveAndAreClearedByTheirEnd(t *testing.T) {
	dir := tempRepo(t)
	st := New("epic-1", 1, "auto", 1)
	st.RecordFailure("a", Failure{Attempt: 1, Of: 2, Stage: "gate", Reason: "go vet failed"})
	if err := Save(dir, st); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := got.LastFailure("a")
	if !ok || f.Reason != "go vet failed" || f.Attempt != 1 {
		t.Fatalf("the failure did not survive the round trip: %+v (ok=%v)", f, ok)
	}
	if f.At.IsZero() {
		t.Fatal("RecordFailure left no timestamp, so nothing can tell a stale record from a fresh one")
	}
	if want := `attempt 1/2 failed at stage "gate":` + "\ngo vet failed"; f.Summary() != want {
		t.Fatalf("Summary() = %q, want %q", f.Summary(), want)
	}

	// An issue that finished, or that a human handed back with a fresh budget,
	// has no previous attempt left to be told about.
	got.MarkDone("a")
	if _, ok := got.LastFailure("a"); ok {
		t.Fatal("a completed issue kept its failure record")
	}
	got.RecordFailure("b", Failure{Attempt: 1, Reason: "x"})
	got.Park("b", "parked", "gate")
	if !got.Unpark("b") {
		t.Fatal("Unpark did not find the parked issue")
	}
	if _, ok := got.LastFailure("b"); ok {
		t.Fatal("Unpark reset the attempt count but left the failure it belonged to")
	}
}

func TestClearLeavesNoRun(t *testing.T) {
	dir := tempRepo(t)
	if err := Save(dir, New("e", 1, "auto", 1)); err != nil {
		t.Fatal(err)
	}
	if err := Clear(dir); err != nil {
		t.Fatal(err)
	}
	if Active(dir) {
		t.Fatal("Clear must leave no run behind, or the next drain resumes a dead one")
	}
	if err := Clear(dir); err != nil {
		t.Fatalf("Clear must be idempotent, got %v", err)
	}
}

// TestConcurrentUpdateDoesNotLoseWrites is the regression test for the failure
// the eqc.1 spike found in bd's own notes field: five concurrent
// read-modify-writes, all reporting success, with one silently lost.
//
// The run state is written concurrently by every issue goroutine in a wave, so
// it must not repeat that bug.
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
				s.Attempts[fmt.Sprintf("issue-%d", i)] = i + 1
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
	if len(got.Attempts) != n {
		t.Fatalf("lost writes: want %d attempt records, got %d", n, len(got.Attempts))
	}
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("issue-%d", i)
		if got.Attempts[k] != i+1 {
			t.Fatalf("attempt record %s missing or wrong: %d", k, got.Attempts[k])
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
		s.Attempts[os.Getenv("BD_AUTO_HELPER_KEY")] = 1
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestConcurrentUpdateAcrossProcesses proves the lock is a real file lock and
// not merely a mutex. This matters because run state is shared across
// processes: a drain, a `bd-auto run status` beside it and a worker's own
// bd-auto call are three separate processes writing this one file, which is
// precisely the shape that loses data when unlocked.
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
	if len(got.Attempts) != n {
		t.Fatalf("cross-process lost writes: want %d attempt records, got %d", n, len(got.Attempts))
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

func TestUnparkReturnsIssueToTheRun(t *testing.T) {
	s := New("e", 5, "auto", 1)
	s.Attempts["b"] = 2
	s.Park("b", "gate never passed", "gate")

	if s.Unpark("nope") {
		t.Fatal("unparking an issue that was never parked must report false")
	}
	if !s.Unpark("b") {
		t.Fatal("Unpark should report that b was parked")
	}
	if s.IsParked("b") || s.Excluded("b") {
		t.Fatalf("b must be offerable again after unpark: %+v", s)
	}
	// Without this the retry budget is already spent and the issue would be
	// parked again by its first failure.
	if s.Attempts["b"] != 0 {
		t.Fatalf("unpark must reset the attempt count, got %d", s.Attempts["b"])
	}
	if s.Unpark("b") {
		t.Fatal("unparking twice must be a no-op")
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

// --- questions ---

// A resumed run must not ask what a human already answered: the worker is a
// fresh process with no memory of having asked, so this file is the only place
// that memory can live.
func TestAnsweredQuestionsSurviveAResume(t *testing.T) {
	dir := t.TempDir()
	if _, err := Update(dir, true, func(s *State) error {
		s.RecordQuestion(Question{
			Issue: "t-1", Role: "worker",
			Question: "Which config key should the timeout live under?",
			Options:  []string{"ask.timeout", "runners.timeout"},
			Answer:   "ask.timeout", Source: "human",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A second process, reading the file back.
	st, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The same question, spelled the way a fresh worker would spell it.
	got, ok := st.AnswerFor("t-1", "  which CONFIG key should the timeout   live under? ")
	if !ok {
		t.Fatal("the recorded answer was not found for the same question asked again")
	}
	if got.Answer != "ask.timeout" {
		t.Fatalf("got %q", got.Answer)
	}
	if _, ok := st.AnswerFor("t-1", "something else entirely?"); ok {
		t.Fatal("a different question matched")
	}
	// Per-issue: the same sentence from another issue is another question.
	if _, ok := st.AnswerFor("t-2", "Which config key should the timeout live under?"); ok {
		t.Fatal("an answer leaked across issues")
	}
}

// Asking the same thing again on a later round must not grow the file once per
// round; the last answer stands.
func TestRecordingTheSameQuestionTwiceReplaces(t *testing.T) {
	s := &State{}
	s.RecordQuestion(Question{Issue: "t-1", Question: "which one?", Answer: "a", Source: "human"})
	s.RecordQuestion(Question{Issue: "t-1", Question: "Which one?", Answer: "b", Source: "human"})
	if len(s.Questions) != 1 {
		t.Fatalf("recorded %d entries for one question", len(s.Questions))
	}
	got, _ := s.AnswerFor("t-1", "which one?")
	if got.Answer != "b" {
		t.Fatalf("the later answer did not win: %q", got.Answer)
	}
}

// TestUpdateCreatesAStandaloneRun pins what `bd-auto issue run` leaves behind.
// It runs with no drain around it, so its first incidental write is what brings
// run.json into being — and that state must name itself, because a status field
// nobody set is a state every reader is free to guess at. Guessing "active" is
// what put an epic-less run at the top of `bd-auto run status`.
func TestUpdateCreatesAStandaloneRun(t *testing.T) {
	dir := tempRepo(t)
	st, err := Update(dir, true, func(s *State) error {
		s.InFlight["a"] = Attempt{Branch: "bd-auto/a", Attempt: 1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusStandalone {
		t.Fatalf("status %q, want %q: a created run must say it was not started", st.Status, StatusStandalone)
	}
	if st.Active() {
		t.Fatal("a standalone run is not armed; reporting it as active arms hooks and blocks drains")
	}
	if Active(dir) {
		t.Fatal("Active(repo) disagreed with the state it loaded")
	}

	// A drain adopting the same file sets its own status over this one, which
	// is what keeps the marker on the standalone case alone.
	if _, err := Update(dir, true, func(s *State) error {
		s.Epic, s.Status = "epic-1", StatusActive
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !Active(dir) {
		t.Fatal("a drain that adopted a standalone run must leave it armed")
	}
}
