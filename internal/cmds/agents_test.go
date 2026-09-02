package cmds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/config"
	"bd-auto/prompts"
)

// `bd-auto init` materialises the built-in agents beside the config file, so a
// repo's behaviour is traceable in its own history rather than in whichever
// bd-auto happened to be installed.
func TestInitMaterialisesTheBuiltinAgents(t *testing.T) {
	dir := t.TempDir()
	if err := Init([]string{"--dir", dir}); err != nil {
		t.Fatal(err)
	}
	for _, role := range prompts.Roles() {
		p := filepath.Join(dir, config.AgentsDir(), role+config.AgentExt)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("init wrote no agent for %s: %v", role, err)
		}
		if !strings.Contains(string(raw), "source: "+config.SourceBuiltin) {
			t.Fatalf("%s has no provenance in its frontmatter", p)
		}
	}

	// The config that init writes has to load with the agents it wrote beside
	// it — a template and a directory that disagree would fail on first use.
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("the config init wrote does not load beside the agents it wrote: %v", err)
	}
	if src := cfg.PromptSource("reviewer"); src.Origin != config.OriginFile {
		t.Fatalf("the materialised reviewer is not what the run would use: %+v", src)
	}
}

func TestInitLeavesAnExistingAgentAlone(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, config.AgentsDir())
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "reviewer"+config.AgentExt)
	if err := os.WriteFile(p, []byte("Mine.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Init([]string{"--dir", dir}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "Mine.\n" {
		t.Fatal("init overwrote a prompt somebody wrote")
	}
	// ...and still wrote the ones that were missing.
	if _, err := os.Stat(filepath.Join(d, "worker"+config.AgentExt)); err != nil {
		t.Fatalf("init skipped the agents that were not there: %v", err)
	}
}

func TestInitProviderCodexAndInvalidProvider(t *testing.T) {
	dir := t.TempDir()
	if err := Init([]string{"--provider", "codex", "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Runner("reviewer"); got.Provider != config.CodexProvider || got.Model != config.DefaultCodexReviewer || got.Sandbox != "read-only" || got.Resume || got.Shell {
		t.Fatalf("Codex reviewer = %+v", got)
	}

	bad := t.TempDir()
	if err := Init([]string{"--provider", "fake", "--dir", bad}); err == nil {
		t.Fatal("unsupported provider succeeded")
	}
	if _, err := os.Stat(filepath.Join(bad, config.FileName)); !os.IsNotExist(err) {
		t.Fatalf("unsupported provider wrote a config: %v", err)
	}
}

// The diff is what makes a materialised prompt's pinning visible, so it has to
// be readable: identical inputs produce nothing at all, and a change produces a
// hunk a human recognises.
func TestUnifiedDiff(t *testing.T) {
	same := []string{"a", "b", "c"}
	if d := unifiedDiff("x", "y", same, same); d != "" {
		t.Fatalf("identical input produced a diff:\n%s", d)
	}

	d := unifiedDiff("builtin/reviewer.md", "agents/reviewer.md",
		[]string{"one", "two", "three"},
		[]string{"one", "two and a half", "three"})
	for _, want := range []string{
		"--- builtin/reviewer.md",
		"+++ agents/reviewer.md",
		"@@ -1,3 +1,3 @@",
		"-two",
		"+two and a half",
		" three",
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("the diff does not contain %q:\n%s", want, d)
		}
	}
}

// Two changes far apart are two hunks; two changes close together are one.
func TestUnifiedDiffGroupsNearbyChanges(t *testing.T) {
	a := make([]string, 30)
	for i := range a {
		a[i] = string(rune('a' + i%26))
	}
	b := append([]string(nil), a...)
	b[1], b[2] = "changed", "also changed"
	b[25] = "far away"

	d := unifiedDiff("a", "b", a, b)
	if got := strings.Count(d, "@@ -"); got != 2 {
		t.Fatalf("want 2 hunks for two distant changes, got %d:\n%s", got, d)
	}
}
