package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/prompts"
)

// agentFile writes .beads-auto/agents/<role>.md under dir.
func agentFile(t *testing.T, dir, role, body string) string {
	t.Helper()
	d := filepath.Join(dir, AgentsDir())
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, role+AgentExt)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, dir string) *Config {
	t.Helper()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s): %v", dir, err)
	}
	return cfg
}

// The headline: dropping one file in overrides the shipped reviewer, with no
// other config change and no runners: entry to go with it.
func TestAnAgentFileOverridesTheShippedPrompt(t *testing.T) {
	dir := t.TempDir()
	p := agentFile(t, dir, "reviewer", "---\nmodel: haiku\n---\nJudge only the tests.\n")

	cfg := load(t, dir)
	if got := cfg.RolePrompt("reviewer"); !strings.HasPrefix(got, "Judge only the tests.") {
		t.Fatalf("the file did not replace the shipped reviewer prompt: %q", got)
	}
	if got := cfg.Runner("reviewer").Model; got != "haiku" {
		t.Fatalf("frontmatter did not set the model: %q", got)
	}
	src := cfg.PromptSource("reviewer")
	if src.Origin != OriginFile || src.Path != p {
		t.Fatalf("the source should be the file, got %+v", src)
	}
}

// Resolution order, all three rungs of it.
func TestPromptResolutionOrder(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "security", "Judge the diff for security defects.\n")
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`
pipeline:
  - stage: implement
  - stage: security
    agent: security
  - stage: audit
    agent: audit
runners:
  audit: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := load(t, dir)

	for _, tc := range []struct {
		role   string
		origin PromptOrigin
		want   string
	}{
		// A file wins.
		{"security", OriginFile, "Judge the diff for security defects."},
		// No file, but a prompt of this name ships.
		{"worker", OriginBuiltin, "bd-auto worker"},
		// Neither: the reviewer, because a custom stage judges a diff. What is
		// new is that it says so rather than doing it silently.
		{"audit", OriginReviewer, "bd-auto reviewer"},
	} {
		src := cfg.PromptSource(tc.role)
		if src.Origin != tc.origin {
			t.Fatalf("%s: origin %q, want %q", tc.role, src.Origin, tc.origin)
		}
		if got := cfg.RolePrompt(tc.role); !strings.Contains(got, tc.want) {
			t.Fatalf("%s: prompt does not contain %q:\n%s", tc.role, tc.want, got)
		}
	}
	if got := cfg.PromptSource("audit").String(); !strings.Contains(got, "no prompt of its own") {
		t.Fatalf("the fallback should say so, got %q", got)
	}
}

// A file is the whole definition of an agent: no runners: entry beside it.
func TestAPipelineMayNameARoleThatOnlyAFileDefines(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "security", "Judge the diff for security defects.\n")
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`
pipeline:
  - stage: implement
  - stage: security
    agent: security
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := load(t, dir)
	if !cfg.RoleDefined("security") {
		t.Fatal("an agent file should define its role")
	}
	if !contains(cfg.Roles(), "security") {
		t.Fatalf("Roles() should list a role defined by a file, got %v", cfg.Roles())
	}
}

// yaml has the last word: the file carries the agent's own defaults, and
// .beads-auto.yaml is where this run's configuration is decided.
func TestRunnersYamlOverridesAgentFrontmatter(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "reviewer", "---\nmodel: haiku\ntimeout: 30\n---\nJudge.\n")
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`
runners:
  reviewer:
    model: sonnet
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := load(t, dir)
	spec := cfg.Runner("reviewer")
	if spec.Model != "sonnet" {
		t.Fatalf("runners: should win over frontmatter, got model %q", spec.Model)
	}
	if spec.Timeout.Seconds() != 30 {
		t.Fatalf("frontmatter should still supply what the yaml did not say, got %v", spec.Timeout)
	}
	// And frontmatter still beats the built-in role default it replaces.
	if got := cfg.Runner("reviewer").Permissions; string(got) != "scoped" {
		t.Fatalf("the built-in reviewer scoping should survive, got %q", got)
	}
}

func TestFrontmatterBeatsTheBuiltinRoleDefault(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "reviewer", "---\nmodel: haiku\n---\nJudge.\n")
	if got := load(t, dir).Runner("reviewer").Model; got != "haiku" {
		t.Fatalf("frontmatter should beat the built-in reviewer model, got %q", got)
	}
}

// No file is the common case and is not an error: the built-in answers.
func TestAMissingAgentFileFallsBackWithoutError(t *testing.T) {
	cfg := load(t, t.TempDir())
	if src := cfg.PromptSource("reviewer"); src.Origin != OriginBuiltin {
		t.Fatalf("a repo with no agents directory should get the built-ins, got %+v", src)
	}
	if cfg.Agent("reviewer") != nil {
		t.Fatal("there is no agent file to report")
	}
}

// An unreadable or unparseable file is a config-load error naming the path,
// because the alternative is a prompt nobody wrote running in place of one
// somebody did.
func TestAnUnparseableAgentFileFailsAtLoad(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"unknown frontmatter key", "---\nmodl: haiku\n---\nJudge.\n", "modl"},
		{"unclosed frontmatter", "---\nmodel: haiku\nJudge.\n", "never closed"},
		{"no body at all", "---\nmodel: haiku\n---\n", "its body is the role's system prompt"},
		{"an empty file", "", "the file is empty"},
		{"bad permissions", "---\npermissions: whenever\n---\nJudge.\n", "permissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := agentFile(t, dir, "reviewer", tc.body)
			_, err := Load(dir)
			if err == nil {
				t.Fatal("a broken agent file loaded cleanly")
			}
			if !strings.Contains(err.Error(), p) {
				t.Fatalf("the error does not name the file: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error does not say what is wrong (%q): %v", tc.want, err)
			}
		})
	}
}

// An agent file that cannot be read at all — here a directory where a file
// should be — stops the load rather than being resolved into the built-in.
func TestAnUnreadableAgentFileFailsAtLoad(t *testing.T) {
	dir := t.TempDir()
	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable directory")
	}
	d := filepath.Join(dir, AgentsDir())
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(d, 0o755)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), d) {
		t.Fatalf("an unreadable agents directory should name itself, got %v", err)
	}
}

// --- splices ---

func TestBuiltinSplice(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "reviewer", prompts.TokenBuiltin+"\n\nAlso check the licence headers.\n")
	got := load(t, dir).RolePrompt("reviewer")
	if !strings.Contains(got, "bd-auto reviewer") {
		t.Fatalf("{{BUILTIN}} did not bring the shipped reviewer in:\n%s", got)
	}
	if !strings.Contains(got, "Also check the licence headers.") {
		t.Fatalf("the repo's own addition is missing:\n%s", got)
	}
}

// What {{BUILTIN}} brings in has splices of its own: the shipped reviewer places
// {{VERDICT}}. A repo that adds to the reviewer has to end up with the contract
// where the built-in put it, exactly once, and with no token left in the text.
func TestBuiltinSpliceResolvesTheSplicesItBringsWithIt(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "reviewer", prompts.TokenBuiltin+"\n\nAlso check the licence headers.\n")
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`
pipeline:
  - stage: implement
  - stage: review
    agent: reviewer
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := load(t, dir).RolePrompt("reviewer")
	if strings.Contains(got, prompts.TokenVerdict) {
		t.Fatalf("a token the built-in placed survived into the prompt:\n%s", got)
	}
	if n := strings.Count(got, "VERDICT: pass"); n != 1 {
		t.Fatalf("the verdict contract appears %d times, want once:\n%s", n, got)
	}
	if !strings.Contains(got, "Also check the licence headers.") {
		t.Fatalf("the repo's own addition is missing:\n%s", got)
	}
}

// A name with no built-in splices nothing, which is not an error.
func TestBuiltinSpliceIsEmptyForANameWithNoBuiltin(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "security", "Judge security.\n\n"+prompts.TokenBuiltin+"\n")
	got := load(t, dir).RolePrompt("security")
	if strings.Contains(got, prompts.TokenBuiltin) {
		t.Fatalf("the literal token reached the prompt:\n%s", got)
	}
	if strings.TrimSpace(got) != "Judge security." {
		t.Fatalf("an empty splice left something behind: %q", got)
	}
}

// {{GRAPH}} is empty until there is a code index to name, and empty means
// nothing rather than a literal or an error.
func TestGraphSpliceIsEmptyWithNoIndex(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "security", "Judge security.\n\n"+prompts.TokenGraph+"\n")
	got := load(t, dir).RolePrompt("security")
	if strings.Contains(got, prompts.TokenGraph) {
		t.Fatalf("the literal token reached the prompt:\n%s", got)
	}
	if strings.TrimSpace(got) != "Judge security." {
		t.Fatalf("an empty splice left something behind: %q", got)
	}
}

// The sharp edge: ParseVerdict fails closed, so a judging prompt without the
// contract fails every issue it sees and looks like a strict reviewer doing it.
// The author does not have to remember.
func TestAJudgingStageAlwaysCarriesTheVerdictContract(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "security", "Judge the diff for security defects.\n")
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`
pipeline:
  - stage: implement
  - stage: security
    agent: security
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := load(t, dir)
	got := cfg.RolePrompt("security")
	if !strings.Contains(got, "VERDICT: pass") {
		t.Fatalf("a judging stage's prompt does not state the contract:\n%s", got)
	}
	if !strings.HasPrefix(got, "Judge the diff") {
		t.Fatalf("the contract displaced the author's own prompt:\n%s", got)
	}
}

// {{VERDICT}} only chooses where the contract lands.
func TestVerdictSplicePlacesTheContract(t *testing.T) {
	dir := t.TempDir()
	agentFile(t, dir, "security", prompts.TokenVerdict+"\n\nJudge the diff for security defects.\n")
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`
pipeline:
  - stage: implement
  - stage: security
    agent: security
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := load(t, dir).RolePrompt("security")
	if !strings.HasPrefix(got, "## Your verdict") {
		t.Fatalf("the contract did not land where the file put it:\n%s", got)
	}
	if strings.Count(got, "VERDICT: pass") != 1 {
		t.Fatalf("the contract should appear once, not %d times", strings.Count(got, "VERDICT: pass"))
	}
}

// A role that judges nothing gets no contract: telling a worker to answer
// VERDICT: pass would be telling it to do somebody else's job.
func TestARoleThatJudgesNothingGetsNoContract(t *testing.T) {
	cfg := load(t, t.TempDir())
	if cfg.Judging("worker") {
		t.Fatal("the implement stage is not a judging stage")
	}
	if strings.Contains(cfg.RolePrompt("worker"), "VERDICT: pass") {
		t.Fatal("the worker prompt was handed the verdict contract")
	}
}

// No prompt reaches a model with a splice still in it, whatever it resolved
// from.
func TestNoResolvedPromptCarriesALiteralToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`
pipeline:
  - stage: implement
  - stage: review
    agent: reviewer
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := load(t, dir)
	for _, role := range cfg.Roles() {
		p := cfg.RolePrompt(role)
		for _, tok := range []string{prompts.TokenBuiltin, prompts.TokenGraph, prompts.TokenVerdict} {
			if strings.Contains(p, tok) {
				t.Fatalf("the %s prompt still contains %s", role, tok)
			}
		}
	}
}

// --- materialising, drift, and taking the new one ---

func TestWriteAgentsMaterialisesTheBuiltinsWithProvenance(t *testing.T) {
	dir := t.TempDir()
	out, err := WriteAgents(dir, "9.9.9", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(prompts.Roles()) {
		t.Fatalf("wrote %d agents, want %d", len(out), len(prompts.Roles()))
	}
	for _, w := range out {
		if !w.Written {
			t.Fatalf("%s was not written: %s", w.Role, w.Reason)
		}
	}
	cfg := load(t, dir)
	for _, role := range prompts.Roles() {
		a := cfg.Agent(role)
		if a == nil {
			t.Fatalf("%s did not load back", role)
		}
		if a.Meta.Source != SourceBuiltin || a.Meta.Version != "9.9.9" {
			t.Fatalf("%s lost its provenance: %+v", role, a.Meta)
		}
		if a.Drifted() {
			t.Fatalf("%s drifted the moment it was written", role)
		}
		// Verbatim: the file is the shipped prompt, so a repo's history says
		// what it ran.
		builtin, _ := prompts.For(role)
		if strings.TrimSpace(a.Body) != strings.TrimSpace(builtin) {
			t.Fatalf("%s was not materialised verbatim", role)
		}
	}
}

func TestWriteAgentsRefusesToClobberWithoutForce(t *testing.T) {
	dir := t.TempDir()
	p := agentFile(t, dir, "reviewer", "Mine.\n")
	if _, err := WriteAgents(dir, "1", false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "Mine.\n" {
		t.Fatal("init overwrote an agent file somebody wrote")
	}
	if _, err := WriteAgents(dir, "1", true); err != nil {
		t.Fatal(err)
	}
	if raw, _ = os.ReadFile(p); !strings.Contains(string(raw), SourceBuiltin) {
		t.Fatal("--force did not replace the file")
	}
}

// Drift is what makes the pinning honest: a materialised copy can say that the
// shipped prompt has moved on, and only a materialised copy can.
func TestDriftIsReportedOnlyForAMaterialisedCopy(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteAgents(dir, "1", false); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, AgentsDir(), "reviewer"+AgentExt)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(raw, []byte("\nAnd check the licence headers.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if !load(t, dir).Agent("reviewer").Drifted() {
		t.Fatal("an edited copy of a built-in should report as drifted")
	}

	// A file a human wrote was never a copy of anything, so calling it drifted
	// would be telling them their own prompt is out of date.
	other := t.TempDir()
	agentFile(t, other, "reviewer", "Mine.\n")
	if load(t, other).Agent("reviewer").Drifted() {
		t.Fatal("a hand-written agent cannot drift")
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// A pipeline naming an agent that exists nowhere says where to put its file.
// "not a defined role" was true and no help: a role can now be defined by a
// file, and a reader has to be told which file.
func TestAnUndefinedAgentNamesTheFileItCouldHaveBeen(t *testing.T) {
	dir := write(t, `
pipeline:
  - stage: implement
  - stage: security
    agent: security
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("a pipeline naming a role that exists nowhere loaded cleanly")
	}
	if want := filepath.Join(dir, AgentsDir(), "security"+AgentExt); !strings.Contains(err.Error(), want) {
		t.Fatalf("the error does not name %s: %v", want, err)
	}
}
