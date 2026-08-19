package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHooksLoadWithTheirDefaults(t *testing.T) {
	dir := write(t, `
hooks:
  on_issue_end:
    - name: log
      run: ./scripts/log.sh
  on_barrier:
    - agent: reviewer
  on_run_end:
    - name: summarise
      run: ./scripts/summary.sh
      timeout: 30
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.HasHooks() {
		t.Fatal("a config with three hooks reports having none")
	}

	issue := cfg.HooksAt(HookIssueEnd)
	if len(issue) != 1 || issue[0].Kind() != "run" {
		t.Fatalf("on_issue_end: %+v", issue)
	}
	if issue[0].Timeout != DefaultHookTimeout {
		t.Fatalf("a hook with no timeout resolved to %d, want the default %d",
			issue[0].Timeout, DefaultHookTimeout)
	}

	// A hook that did not name itself still has to be nameable on a report and
	// in the display, so the point and the position become the name.
	barrier := cfg.HooksAt(HookBarrier)
	if len(barrier) != 1 || barrier[0].Name != "on_barrier-1" || barrier[0].Kind() != "agent" {
		t.Fatalf("on_barrier: %+v", barrier)
	}

	if got := cfg.HooksAt(HookRunEnd); got[0].Timeout != 30 {
		t.Fatalf("an explicit timeout was replaced: %+v", got)
	}
}

// The same validation a pipeline stage gets, for the same reason: catching a
// stale name at load costs a line of output, and catching it at a barrier costs
// the barrier.
func TestHookValidationRefusesWhatAStageWouldRefuse(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"both agent and run": {`
hooks:
  on_barrier:
    - name: two
      agent: reviewer
      run: ./x.sh
`, "set agent or run, not both"},
		"neither": {`
hooks:
  on_barrier:
    - name: nothing
`, "needs either agent or run"},
		"undefined role": {`
hooks:
  on_run_end:
    - name: triage
      agent: triager
`, "is not a defined runner role"},
		"duplicate name": {`
hooks:
  on_issue_end:
    - name: same
      run: ./a.sh
    - name: same
      run: ./b.sh
`, "duplicate hook name"},
		"negative timeout": {`
hooks:
  on_issue_end:
    - name: forever
      run: ./a.sh
      timeout: -1
`, "is negative"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, c.body))
			if err == nil {
				t.Fatalf("%s loaded", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s failed with %q, which does not say %q", name, err, c.want)
			}
		})
	}
}

// A hook's role is a role like any other: an agent file defines one on its own,
// with no runners: entry beside it.
func TestAnAgentFileDefinesAHooksRole(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, AgentsDir())
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "triager.md"),
		[]byte("Decide whether each finding is new work, a duplicate, or noise.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`
hooks:
  on_barrier:
    - name: triage
      agent: triager
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if roles := cfg.HookRoles(); len(roles) != 1 || roles[0] != "triager" {
		t.Fatalf("HookRoles: %v", roles)
	}

	// And it is reported, because a hook handed the reviewer's prompt by
	// accident is an interpreter that has been told to judge a diff.
	var found bool
	for _, s := range cfg.PromptSources() {
		if s.Role != "triager" {
			continue
		}
		found = true
		if s.Origin != OriginFile {
			t.Fatalf("the triager's prompt came from %s, not its file", s.Origin)
		}
		if s.Judging {
			t.Fatal("a hook does not judge: nothing reads a verdict from one")
		}
	}
	if !found {
		t.Fatalf("a hook's role is missing from PromptSources: %+v", cfg.PromptSources())
	}
}

// A hook role with no prompt of its own falls back to the reviewer's, which is
// the repo-wide rule — and the whole point of listing it is that a repo can see
// that happened rather than discover it in a transcript.
func TestAHookRoleWithNoPromptIsVisiblyTheReviewers(t *testing.T) {
	cfg, err := Load(write(t, `
runners:
  triager:
    model: sonnet
hooks:
  on_barrier:
    - name: triage
      agent: triager
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range cfg.PromptSources() {
		if s.Role == "triager" {
			if s.Origin != OriginReviewer {
				t.Fatalf("the triager's prompt reports as %s", s.Origin)
			}
			return
		}
	}
	t.Fatalf("the triager is not in PromptSources: %+v", cfg.PromptSources())
}

// The starter config has to describe hooks, because a feature nobody can find
// is a feature nobody has.
func TestTheTemplateDocumentsHooks(t *testing.T) {
	body := string(Template())
	for _, want := range []string{
		"on_issue_end", "on_barrier", "on_run_end",
		"ADVISORY", "$BD_REPORT_FILE",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the starter config never mentions %q", want)
		}
	}
	// And it still has to load: a commented example that would not parse is
	// worse than none, because somebody will uncomment it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), Template(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("the starter config does not load: %v", err)
	}
}
