package drain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/config"
	"bd-auto/internal/graph"
	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
)

// writeIndex puts a graph.json where graph.Read looks for one.
func writeIndex(t *testing.T, repoRoot string) string {
	t.Helper()
	path := graph.GraphPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"nodes":[],"edges":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func graphCfg(roles ...string) *config.Config {
	cfg := config.Default()
	cfg.Graph.Enabled = true
	cfg.Graph.Roles = roles
	return cfg
}

func TestAttachGraphTellsAnEnabledRole(t *testing.T) {
	root := t.TempDir()
	path := writeIndex(t, root)
	e := &Engine{RepoRoot: root, Cfg: graphCfg("worker")}

	req := runner.Request{SystemPrompt: "role prompt\n"}
	e.attachGraph(&req, invocation{Role: runner.RoleWorker})

	if !strings.Contains(req.SystemPrompt, "role prompt") {
		t.Fatal("the role's own prompt was dropped")
	}
	if !strings.Contains(req.SystemPrompt, path) {
		t.Fatalf("the index path is not in the prompt:\n%s", req.SystemPrompt)
	}
	for _, cmd := range []string{"god-nodes", "explain", "affected"} {
		if !strings.Contains(req.SystemPrompt, cmd) {
			t.Errorf("the prompt does not name %s", cmd)
		}
	}
	if len(req.AllowedTools) != 1 || req.AllowedTools[0] != "Bash(graphify:*)" {
		t.Fatalf("allowed tools = %v", req.AllowedTools)
	}
}

// A role not named in graph.roles is told nothing, so it cannot spend tokens on
// a tool it was not meant to have.
func TestAttachGraphSkipsARoleNotNamed(t *testing.T) {
	root := t.TempDir()
	writeIndex(t, root)
	e := &Engine{RepoRoot: root, Cfg: graphCfg("worker")}

	req := runner.Request{SystemPrompt: "role prompt"}
	e.attachGraph(&req, invocation{Role: runner.RoleReviewer})

	if req.SystemPrompt != "role prompt" {
		t.Fatalf("the reviewer was told about the index:\n%s", req.SystemPrompt)
	}
	if req.AllowedTools != nil {
		t.Fatalf("the reviewer was widened: %v", req.AllowedTools)
	}
}

func TestAttachGraphSkipsWhenDisabled(t *testing.T) {
	root := t.TempDir()
	writeIndex(t, root)
	cfg := graphCfg("worker")
	cfg.Graph.Enabled = false
	e := &Engine{RepoRoot: root, Cfg: cfg}

	req := runner.Request{SystemPrompt: "role prompt"}
	e.attachGraph(&req, invocation{Role: runner.RoleWorker})

	if req.SystemPrompt != "role prompt" || req.AllowedTools != nil {
		t.Fatalf("a disabled index was still offered: %q %v", req.SystemPrompt, req.AllowedTools)
	}
}

// The case that matters most: enabled, the role is named, and the build failed
// or graphify is not installed. A model told about a tool that is not there will
// try it and read the error, which costs more than never mentioning it.
func TestAttachGraphSkipsWithNoIndexOnDisk(t *testing.T) {
	root := t.TempDir()
	e := &Engine{RepoRoot: root, Cfg: graphCfg("worker")}

	req := runner.Request{SystemPrompt: "role prompt"}
	e.attachGraph(&req, invocation{Role: runner.RoleWorker})

	if req.SystemPrompt != "role prompt" || req.AllowedTools != nil {
		t.Fatalf("an index that does not exist was offered: %q %v", req.SystemPrompt, req.AllowedTools)
	}
}

// A scoped role keeps the list it already had; graphify is added to it, not
// substituted for it.
func TestAttachGraphKeepsAnExistingAllowlist(t *testing.T) {
	root := t.TempDir()
	writeIndex(t, root)
	e := &Engine{RepoRoot: root, Cfg: graphCfg("reviewer")}

	req := runner.Request{AllowedTools: config.DefaultReviewerTools()}
	before := len(req.AllowedTools)
	e.attachGraph(&req, invocation{Role: runner.RoleReviewer})

	if len(req.AllowedTools) != before+1 {
		t.Fatalf("allowed tools = %v", req.AllowedTools)
	}
	if !strings.Contains(strings.Join(req.AllowedTools, ","), "Bash(git diff:*)") {
		t.Fatalf("the reviewer's own tools were lost: %v", req.AllowedTools)
	}
}

func TestAttachGraphNilConfig(t *testing.T) {
	e := &Engine{RepoRoot: t.TempDir()}
	req := runner.Request{SystemPrompt: "role prompt"}
	e.attachGraph(&req, invocation{Role: runner.RoleWorker})
	if req.SystemPrompt != "role prompt" {
		t.Fatal("a nil config still attached something")
	}
}

// `bd-auto issue run` is a whole entry point of its own, and the index has to
// reach it: a drain builds one for the run, and a single issue that skipped the
// build would leave graph.enabled true with no index for any worker to use —
// which is the failure that lies rather than fails, since the run still works.
func TestASingleIssueRunBuildsTheIndexToo(t *testing.T) {
	repo := testRepo(t)
	fakeGraphifyFor(t)
	cfg := graphCfg("worker")
	cfg.Gate = nil
	cfg.Pipeline = []config.Stage{{Stage: config.StageImplement}}
	iss := newIssues("t-1")

	e := engine(t, repo, cfg, iss,
		fake.New(fake.Step{Text: "done", Do: steps(commitWork("a.txt"), closes(iss, "t-1"))}),
		fake.New(fake.Step{Text: "VERDICT: PASS"}))
	if _, err := e.Issue(context.Background(), "t-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if idx := graph.Read(repo); !idx.Built {
		t.Fatalf("a single-issue run left graph.enabled true and no index: %s", idx.Why)
	}
}

// fakeGraphifyFor puts a graphify on PATH that writes the graph --out names, so
// the index path can be exercised without depending on the real one.
func fakeGraphifyFor(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
mkdir -p "$out/graphify-out"
printf '{"nodes":[{}],"edges":[]}' > "$out/graphify-out/graph.json"
`
	if err := os.WriteFile(filepath.Join(bin, "graphify"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
