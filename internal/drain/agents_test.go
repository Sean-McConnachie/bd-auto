package drain

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
	"bd-auto/internal/runner"
)

// The prompt a model is spawned with comes from the main checkout, and travels
// to the worktree in the request.
//
// This is the whole reason the shipped prompts are embedded rather than read
// from disk: a run spawns its processes in worktrees, and a worktree may be on
// a commit where an agent file differs or does not exist yet. Resolving in the
// main checkout at config load and carrying the text keeps that property while
// letting a repo define its own agents.
func TestTheWorkerPromptComesFromTheMainCheckoutNotTheWorktree(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "worker", "Implement it, in this repo's own words.\n")
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}

	// A worktree with an agent file of its own, which nothing may read: on a
	// real run this is a checkout of another commit.
	wt := filepath.Join(root, "wt")
	writeAgent(t, wt, "worker", "SOMETHING ELSE ENTIRELY\n")

	e := &Engine{RepoRoot: root, Cfg: cfg}
	req := e.workerRequest(task{ID: "x-1", Branch: "b", Worktree: wt, Issue: &bd.Issue{ID: "x-1"}}, false, "", "")
	if req.Dir != wt {
		t.Fatalf("the worker should run in its worktree, got %q", req.Dir)
	}
	if !strings.HasPrefix(req.SystemPrompt, "Implement it, in this repo's own words.") {
		t.Fatalf("the prompt did not come from the main checkout:\n%s", req.SystemPrompt)
	}
	if strings.Contains(req.SystemPrompt, "SOMETHING ELSE") {
		t.Fatal("a prompt was read out of the worktree")
	}
}

// A judging stage's request carries the verdict contract even when the repo's
// own agent file never mentions it, because ParseVerdict fails closed.
func TestAJudgingStageRequestCarriesTheVerdictContract(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "security", "Judge the diff for security defects.\n")
	if err := os.WriteFile(filepath.Join(root, config.FileName), []byte(`
pipeline:
  - stage: implement
  - stage: security
    agent: security
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{RepoRoot: root, Cfg: cfg}
	stage := cfg.Pipeline[len(cfg.Pipeline)-1]
	req := e.reviewRequest(task{ID: "x-1", Worktree: root, Issue: &bd.Issue{ID: "x-1"}},
		stage, runner.Role(stage.Agent), false)

	v := ParseVerdict("VERDICT: pass")
	if !v.Found || !v.Pass {
		t.Fatal("the parser this contract is written against no longer reads it")
	}
	if !strings.Contains(req.SystemPrompt, "VERDICT: pass") {
		t.Fatalf("a judging stage was spawned without the contract:\n%s", req.SystemPrompt)
	}
}

// A role that fell back to the reviewer used to be invisible. The run log says
// it now, once, before anything is spawned.
func TestTheRunLogSaysWhereEveryPromptCameFrom(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "worker", "Implement it.\n")
	if err := os.WriteFile(filepath.Join(root, config.FileName), []byte(`
pipeline:
  - stage: implement
  - stage: audit
    agent: audit
runners:
  audit: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}

	var log []string
	e := &Engine{RepoRoot: root, Cfg: cfg}
	e.Log = func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }
	e.logPromptSources()

	if len(log) != 1 {
		t.Fatalf("want one line at the start of a run, got %d: %v", len(log), log)
	}
	line := log[0]
	for _, want := range []string{
		"worker: " + config.AgentPath(root, "worker"),
		"audit: reviewer (no prompt of its own)",
		"integrator: builtin",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("the run log does not say %q:\n%s", want, line)
		}
	}
}

func writeAgent(t *testing.T, root, role, body string) {
	t.Helper()
	d := filepath.Join(root, config.AgentsDir())
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, role+config.AgentExt), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
