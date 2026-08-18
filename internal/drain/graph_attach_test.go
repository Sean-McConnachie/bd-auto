package drain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/config"
	"bd-auto/internal/graph"
	"bd-auto/internal/runner"
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
