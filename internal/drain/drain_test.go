package drain

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
)

// touch writes an empty transcript with a fixed modification time, which is
// what orders two processes from the same round.
func touch(t *testing.T, repo, name string, nth int) {
	t.Helper()
	dir := filepath.Join(runstate.Dir(repo), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 17, 9, nth, 0, 0, time.UTC)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// The transcripts on disk are the only complete account of what an issue's
// models did, and reading them is only worth anything if they come back in the
// order they ran: an issue's processes are sequential, and a view that showed
// round 1 above round 0 would be worse than showing nothing.
func TestLogFilesListEveryProcessOfAnIssueInOrder(t *testing.T) {
	repo := t.TempDir()
	// Deliberately out of order on disk, and interleaved with a second issue
	// whose ID has this one's as a prefix.
	touch(t, repo, "t-1-a1-r1-review.jsonl", 4)
	touch(t, repo, "t-1-a1-r0-worker.jsonl", 1)
	touch(t, repo, "t-1-a1-r1-worker.jsonl", 3)
	touch(t, repo, "t-1-a1-r0-review.jsonl", 2)
	touch(t, repo, "t-1-a2-r0-worker.jsonl", 5)
	touch(t, repo, "t-1-a2-r0-integrator.jsonl", 6)
	touch(t, repo, "t-10-a1-r0-worker.jsonl", 7)
	// Not a transcript this names: no attempt, and a stray file beside them.
	touch(t, repo, "t-1-worker.jsonl", 8)
	touch(t, repo, "notes.md", 9)

	want := []string{
		"t-1-a1-r0-worker.jsonl", "t-1-a1-r0-review.jsonl",
		"t-1-a1-r1-worker.jsonl", "t-1-a1-r1-review.jsonl",
		"t-1-a2-r0-worker.jsonl", "t-1-a2-r0-integrator.jsonl",
	}
	got := LogFiles(repo, "t-1")
	if len(got) != len(want) {
		t.Fatalf("LogFiles returned %d transcripts, want %d: %v", len(got), len(want), got)
	}
	for i, name := range want {
		if base := filepath.Base(got[i].Path); base != name {
			t.Fatalf("transcript %d is %s, want %s", i, base, name)
		}
	}
	if f := got[2]; f.Attempt != 1 || f.Round != 1 || f.Role != runner.RoleWorker {
		t.Fatalf("the name was read back as attempt %d round %d role %q", f.Attempt, f.Round, f.Role)
	}
}

// `run unpark` resets the attempt counter on purpose, so a retried worker asks
// for the same transcript name its own corpse is written to and the adapter
// writes beside it. Both are worth reading, and the second has to be readable
// as a second process rather than as a role called worker-2.
func TestLogFilesReadBackTheNameAnAdapterTookWhenTheFirstWasOccupied(t *testing.T) {
	repo := t.TempDir()
	touch(t, repo, "t-1-a1-r0-worker.jsonl", 1)
	touch(t, repo, "t-1-a1-r0-worker-2.jsonl", 2)

	got := LogFiles(repo, "t-1")
	if len(got) != 2 {
		t.Fatalf("LogFiles returned %d transcripts, want both", len(got))
	}
	if f := got[1]; f.Role != runner.RoleWorker || f.Dup != 2 {
		t.Fatalf("the second process reads as role %q dup %d, want worker and 2", f.Role, f.Dup)
	}
}

// An issue with nothing spawned for it has no transcripts, and that is not an
// error: it is the difference the view shows between a queued issue and a
// worker that has written nothing yet.
func TestLogFilesAreEmptyBeforeAnythingIsSpawned(t *testing.T) {
	if got := LogFiles(t.TempDir(), "t-1"); len(got) != 0 {
		t.Fatalf("LogFiles found %v in a repo that has run nothing", got)
	}
}
