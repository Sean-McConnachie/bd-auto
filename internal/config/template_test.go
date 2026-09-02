package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated file is the first thing a new user edits, so a template that
// does not parse is worse than no template at all.
func TestTemplateLoadsAndMatchesDefaults(t *testing.T) {
	dir := t.TempDir()
	path, err := Write(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, FileName) {
		t.Fatalf("wrote to %q", path)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("generated template must load: %v", err)
	}
	d := Default()
	if cfg.Concurrency != d.Concurrency || cfg.Autonomy != d.Autonomy ||
		cfg.Retry != d.Retry || cfg.DiscoveredWork != d.DiscoveredWork ||
		cfg.BranchPrefix != d.BranchPrefix || cfg.MaxRounds != d.MaxRounds {
		t.Fatalf("template drifted from defaults: %+v", cfg)
	}
}

// The template ships a review stage, which is the whole reason max_rounds is
// worth documenting; it must carry the current default, not a stale literal.
func TestTemplateReviewStageUsesDefaultMaxRounds(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var review *Stage
	for i := range cfg.Pipeline {
		if cfg.Pipeline[i].Stage == "review" {
			review = &cfg.Pipeline[i]
			break
		}
	}
	if review == nil {
		t.Fatal("template should include an agent review stage")
	}
	if review.Agent == "" {
		t.Fatal("the review stage should name the role that runs it")
	}
	if review.MaxRounds != DefaultMaxRounds {
		t.Fatalf("template max_rounds is %d, want DefaultMaxRounds %d",
			review.MaxRounds, DefaultMaxRounds)
	}
}

// The template is where most people learn the schema, so it has to show the
// rule rather than rely on the reader knowing it: every stage that runs under a
// role names that role, and the gate — the one step that is commands rather
// than a judgement — names none.
func TestTemplateNamesAnAgentOnEveryStageThatHasOne(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cfg.Pipeline {
		switch {
		case s.Stage == StageGate:
			if s.Agent != "" {
				t.Fatalf("the gate stage names %q; it runs no model", s.Agent)
			}
		case s.Run != "":
			// A command stage runs under nobody either.
		case s.Agent == "":
			t.Fatalf("stage %q names no agent", s.Stage)
		}
	}
	// Written out in the file rather than filled in by the loader, because the
	// file is what a reader learns the schema from.
	if !strings.Contains(string(Template()), "- stage: implement\n    agent: worker") {
		t.Fatal("the template should write the implement stage's agent out in full")
	}
}

// The gate is commented out on purpose: a generated gate would fail every issue
// in a repo that does not happen to use those commands.
func TestTemplateShipsNoActiveGate(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasGate() {
		t.Fatal("template must not configure a gate it cannot know is correct")
	}
	if !strings.Contains(string(Template()), "# gate:") {
		t.Fatal("template should show a commented gate example to edit")
	}
}

func TestWriteRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	if err := os.WriteFile(p, []byte("concurrency: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Write(dir, false); !errors.Is(err, ErrConfigExists) {
		t.Fatalf("want ErrConfigExists, got %v", err)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "concurrency: 2\n" {
		t.Fatal("an existing config must be left exactly as it was")
	}

	if _, err := Write(dir, true); err != nil {
		t.Fatalf("--force should overwrite: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency != DefaultConcurrency {
		t.Fatalf("force should have replaced the file, got concurrency %d", cfg.Concurrency)
	}
}

func TestCodexTemplateLoadsWithNativeDefaults(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteForProvider(dir, CodexProvider, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for role, model := range map[string]string{"worker": DefaultCodexModel, "reviewer": DefaultCodexReviewer, "integrator": DefaultCodexModel} {
		s := cfg.Runner(role)
		wantSandbox := "workspace-write"
		if role == "reviewer" {
			wantSandbox = "read-only"
		}
		wantShell := role != "reviewer"
		if s.Provider != CodexProvider || s.Model != model || s.Sandbox != wantSandbox || s.ApprovalPolicy != "never" || s.Shell != wantShell || s.WebSearch || s.ViewImage {
			t.Fatalf("%s = %+v", role, s)
		}
	}
}

func TestWriteForProviderRejectsBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteForProvider(dir, "fake", false); err == nil {
		t.Fatal("unsupported init provider succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("invalid provider wrote a config: %v", err)
	}
}
