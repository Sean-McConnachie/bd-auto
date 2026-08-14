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
		cfg.BranchPrefix != d.BranchPrefix || cfg.ReportMaxLines != d.ReportMaxLines {
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
		if cfg.Pipeline[i].Agent != "" {
			review = &cfg.Pipeline[i]
			break
		}
	}
	if review == nil {
		t.Fatal("template should include an agent review stage")
	}
	if review.MaxRounds != DefaultMaxRounds {
		t.Fatalf("template max_rounds is %d, want DefaultMaxRounds %d",
			review.MaxRounds, DefaultMaxRounds)
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
