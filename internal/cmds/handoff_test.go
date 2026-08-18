package cmds

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/drain"
	"bd-auto/internal/runstate"
)

// capture runs a command with stdout redirected, and decodes the report it
// emitted. The JSON is the command's actual output, so a test that reads it is
// testing what a caller sees rather than what the engine returned.
func capture(t *testing.T, run func() error) (drain.HandoffReport, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	cmdErr := run()
	os.Stdout = old
	w.Close()
	out, rerr := io.ReadAll(r)
	r.Close()
	if rerr != nil {
		t.Fatal(rerr)
	}
	var h drain.HandoffReport
	if len(out) > 0 {
		if err := json.Unmarshal(out, &h); err != nil {
			t.Fatalf("the command emitted no readable report: %v\n%s", err, out)
		}
	}
	return h, cmdErr
}

// mustGitOut runs git in a test repo and fails the test rather than the command.
func mustGitOut(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := gitOut(dir, args...); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// stagedRun is a repo left exactly as a finished drain leaves one: an epic
// branch with the work on it, the checkout standing on it, and run state that
// says what it was staged from.
func stagedRun(t *testing.T, status string) string {
	t.Helper()
	repo := cmdTestRepo(t)
	writeFile(t, filepath.Join(repo, ".beads-auto.yaml"), `gate:
  - name: ok
    run: "true"
pipeline:
  - stage: implement
runners:
  default:
    provider: fake
`)
	mustGitOut(t, repo, "add", "-A")
	mustGitOut(t, repo, "commit", "--quiet", "-m", "config")
	mustGitOut(t, repo, "switch", "--quiet", "-c", "bd-auto/epic/epic-1")
	writeFile(t, filepath.Join(repo, "one.txt"), "one\n")
	mustGitOut(t, repo, "add", "-A")
	mustGitOut(t, repo, "commit", "--quiet", "-m", "t-1")

	st := runstate.New("epic-1", 1, "auto", 0)
	st.Base, st.EpicBranch, st.Wave = "main", "bd-auto/epic/epic-1", 1
	st.Done = []string{"t-1"}
	st.Status = status
	if err := runstate.Save(repo, st); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_AUTO_REPO", repo)
	t.Chdir(repo)
	return repo
}

// Nothing to hand over is an error with a sentence in it, not a stack trace and
// not a zero exit that looks like success.
func TestHandoffWithNoRunSaysSo(t *testing.T) {
	repo := cmdTestRepo(t)
	t.Setenv("BD_AUTO_REPO", repo)
	t.Chdir(repo)

	err := Handoff(nil)
	if err == nil {
		t.Fatal("handing over a repo with no run must fail")
	}
	if _, silent := ExitCode(err); silent {
		t.Fatalf("the reason must be printed, not swallowed by an exit code: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing to hand over") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// The command end to end, as far as a repo with no forge behind it can go: run
// state read, the branch found, the gate re-run, and a refusal that exits
// non-zero without publishing anything. There is no remote here, so the forge
// stops it at the last step — which is itself the property worth having, since
// that is the step that would otherwise reach somebody's account.
func TestHandoffRunsTheGateAndRefusesRatherThanPublishingBlind(t *testing.T) {
	repo := stagedRun(t, runstate.StatusDone)

	h, err := capture(t, func() error { return Handoff([]string{"--quiet"}) })
	code, silent := ExitCode(err)
	if !silent || code != 1 {
		t.Fatalf("a handoff that opened nothing must exit 1, got %v", err)
	}
	if h.Branch != "bd-auto/epic/epic-1" || len(h.Issues) != 1 || h.Issues[0] != "t-1" {
		t.Fatalf("the report was not rebuilt from run state and git: %+v", h)
	}
	// The predicate said yes — the run is done, the gate is green on the branch,
	// t-1 is on it — and the only thing that refused is the forge this repo does
	// not have. Anything else here means the rebuilt report is wrong, not that
	// the environment is bare.
	if h.Pushed || !strings.Contains(h.Reason, "is ready to hand over but") {
		t.Fatalf("expected a refusal from the forge alone, got %q (pushed=%v)", h.Reason, h.Pushed)
	}
	st, lerr := runstate.Load(repo)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if st.PR != "" {
		t.Fatalf("nothing was published, and run state claims a pull request: %q", st.PR)
	}
}

// A checkout standing somewhere other than the epic branch is refused before
// the gate runs, because the gate proves the working tree and would otherwise
// report a verdict about the wrong branch.
func TestHandoffRefusesFromTheWrongBranch(t *testing.T) {
	repo := stagedRun(t, runstate.StatusDone)
	mustGitOut(t, repo, "switch", "--quiet", "main")

	err := Handoff([]string{"--quiet"})
	if err == nil {
		t.Fatal("handing over from the wrong branch must fail")
	}
	if !strings.Contains(err.Error(), "bd-auto/epic/epic-1") {
		t.Fatalf("the error does not say which branch to switch to: %v", err)
	}
}
