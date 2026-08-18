package drain

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"bd-auto/internal/config"
	"bd-auto/internal/graph"
	"bd-auto/internal/runner"
)

// The whole path against the real graphify, on a real repo: build, read back,
// and render the prompt a worker would actually be given.
//
// Skipped by default and env-gated rather than gated on graph.Available(),
// because it extracts a whole repo and writes into it. The rest of the graph
// tests use a fake graphify on PATH and prove the failure modes; this one
// proves the thing works, which no fake can. Run it as:
//
//	git clone --no-hardlinks . /tmp/x && BD_AUTO_E2E_GRAPH=/tmp/x go test ./internal/drain -run E2EGraph -v
func TestE2EGraphIndexEndToEnd(t *testing.T) {
	root := os.Getenv("BD_AUTO_E2E_GRAPH")
	if root == "" {
		t.Skip("set BD_AUTO_E2E_GRAPH to a throwaway checkout to run this")
	}

	cfg := config.Default()
	cfg.Graph.Enabled = true
	cfg.Graph.Roles = []string{"worker"}
	e := &Engine{RepoRoot: root, Cfg: cfg, Log: func(f string, a ...any) { t.Logf(f, a...) }}
	e.buildIndex(context.Background())

	idx := graph.Read(root)
	if !idx.Built {
		t.Fatalf("no index: %s", idx.Why)
	}
	if idx.Stamp == nil || idx.Stamp.Nodes == 0 {
		t.Fatalf("index has no stamp or no nodes: %+v", idx.Stamp)
	}
	t.Logf("%d nodes, %d edges, from %s", idx.Stamp.Nodes, idx.Stamp.Edges, idx.Stamp.Commit)

	// And the barrier's refresh, which is the call that erodes the index if it
	// points graphify at the wrong tree: an update aimed at the output directory
	// took this repo from 2198 nodes to 1956 and found only stamp.json.
	after := graph.Refresh(context.Background(), root, e.graphOptions(), func(f string, a ...any) { t.Logf(f, a...) })
	if !after.Built || after.Stamp == nil {
		t.Fatalf("refresh lost the index: %s", after.Why)
	}
	if after.Stamp.Nodes < idx.Stamp.Nodes {
		t.Fatalf("refresh shrank the index: %d nodes, was %d", after.Stamp.Nodes, idx.Stamp.Nodes)
	}
	t.Logf("after refresh: %d nodes, %d edges", after.Stamp.Nodes, after.Stamp.Edges)

	req := runner.Request{SystemPrompt: "ROLE"}
	e.attachGraph(&req, invocation{Role: runner.RoleWorker})
	if len(req.AllowedTools) != 1 {
		t.Fatalf("allowed tools = %v", req.AllowedTools)
	}
	t.Logf("prompt:\n%s", req.SystemPrompt)

	// The strongest guard there is on a working tree bd-auto was asked to read
	// and not to edit: after two extractions, git has nothing to report.
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if len(out) > 0 {
		t.Fatalf("indexing left something in the working tree:\n%s", out)
	}
}
