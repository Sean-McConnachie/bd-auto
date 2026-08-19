package cmds

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/drain"
)

// triageRepo is a repo with a staged discovery and nothing else: the command
// under test reads a file and talks to bd, and neither needs a run.
func triageRepo(t *testing.T, staged ...drain.Staged) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, ".beads", "auto"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := &drain.Triage{Version: 1, Staged: staged}
	if err := tr.Save(repo); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_AUTO_REPO", repo)
	t.Chdir(repo)
	return repo
}

// TestTriageListsWhatIsWaitingAsJSON is the contract a smoke test drives: the
// command has to be usable without a terminal.
func TestTriageListsWhatIsWaitingAsJSON(t *testing.T) {
	triageRepo(t,
		drain.Staged{Key: "a real bug", From: "t-1", Title: "A real bug", Description: "In x.go:40."},
		drain.Staged{Key: "already decided", From: "t-2", Title: "Already decided",
			Description: "x.", Outcome: "discarded", Reason: "documented where it lives"},
	)

	read := captureStdout(t)
	if err := Triage([]string{"--list", "--json"}); err != nil {
		t.Fatalf("Triage: %v", err)
	}
	out := read()
	var got []drain.Staged
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Key != "a real bug" {
		t.Fatalf("listed %+v, want only the pending one", got)
	}

	readAll := captureStdout(t)
	if err := Triage([]string{"--list", "--all", "--json"}); err != nil {
		t.Fatalf("Triage --all: %v", err)
	}
	all := readAll()
	var everything []drain.Staged
	if err := json.Unmarshal([]byte(all), &everything); err != nil {
		t.Fatal(err)
	}
	if len(everything) != 2 {
		t.Fatalf("--all listed %d, want 2", len(everything))
	}
}

// TestTriageWithNothingStagedSaysSoAndSucceeds. A repo that has never staged
// anything is the normal case, not an error.
func TestTriageWithNothingStagedSaysSoAndSucceeds(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Setenv("BD_AUTO_REPO", repo)
	t.Chdir(repo)

	read := captureStdout(t)
	if err := Triage(nil); err != nil {
		t.Fatalf("Triage: %v", err)
	}
	out := read()
	if !strings.Contains(out, "nothing staged") {
		t.Fatalf("unhelpful output for an empty triage list:\n%s", out)
	}
}

// TestIntoWithoutAcceptIsRefused. --into names where a finding goes and needs
// --accept to say which finding; silently listing instead would look like the
// merge happened.
func TestIntoWithoutAcceptIsRefused(t *testing.T) {
	triageRepo(t, drain.Staged{Key: "a real bug", From: "t-1", Title: "A real bug", Description: "x."})
	err := Triage([]string{"--into", "t-2"})
	if err == nil {
		t.Fatal("--into with no --accept was accepted")
	}
	if !strings.Contains(err.Error(), "--accept") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
}

// TestDiscardWithoutAReasonIsRefusedAtTheCommand, not only in the engine: this
// is the layer a human types at.
func TestDiscardWithoutAReasonIsRefusedAtTheCommand(t *testing.T) {
	repo := triageRepo(t, drain.Staged{Key: "tidier", From: "t-1", Title: "Tidier", Description: "x."})
	if err := Triage([]string{"--discard", "tidier"}); err == nil {
		t.Fatal("discarded with no reason")
	}
	tr, err := drain.LoadTriage(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Pending()) != 1 {
		t.Fatal("the refused discard changed the file anyway")
	}
}
