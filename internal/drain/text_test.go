package drain

import (
	"strings"
	"testing"

	"bd-auto/internal/bd"
	"bd-auto/internal/config"
)

func TestAgentTasksContainTheCompleteIssueWithoutMetadataLookups(t *testing.T) {
	iss := &bd.Issue{
		ID:                 "t-1",
		Title:              "Complete lifecycle",
		Description:        "Move ownership into the engine.",
		Design:             "Agents edit files only.",
		AcceptanceCriteria: "Close after the merged gate.",
		Notes:              "Keep retry evidence.",
		Dependencies: []bd.Ref{{
			ID: "t-0", Title: "Foundation", Status: "closed", Type: "blocks",
		}},
	}
	task := task{
		Issue: iss, ID: iss.ID, Branch: "bd-auto/t-1", Base: "base-sha",
		Worktree: "/repo/.beads/auto/wt/t-1", Attempt: 2,
		Diff: "diff --git a/old.txt b/old.txt\ndeleted file mode 100644\n" +
			"diff --git a/new.txt b/new.txt\nnew file mode 100644\n" +
			"GIT binary patch\nliteral 1\n",
	}
	prompts := map[string]string{
		"worker":     workerPrompt(task, false, "", ""),
		"reviewer":   reviewPrompt(task, config.Stage{Stage: "review"}, false),
		"integrator": conflictPrompt(Merge{Issue: iss.ID, Branch: task.Branch, Conflicts: []string{"merge.go"}}, "epic", iss),
	}
	for role, prompt := range prompts {
		for _, want := range []string{
			iss.Title, iss.Description, iss.Design, iss.AcceptanceCriteria, iss.Notes,
			"t-0", "Foundation", "closed", "blocks", task.Branch,
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s task omitted %q:\n%s", role, want, prompt)
			}
		}
	}
	for _, want := range []string{"old.txt", "new.txt", "GIT binary patch"} {
		if !strings.Contains(prompts["reviewer"], want) {
			t.Errorf("reviewer task omitted patch evidence %q", want)
		}
	}
	for role, prompt := range prompts {
		if strings.Contains(prompt, "Run `bd show") || strings.Contains(prompt, "run `bd show") {
			t.Errorf("%s task requires an inaccessible Beads lookup", role)
		}
	}
}

func TestWorkerStatusContractIsStrict(t *testing.T) {
	cases := []struct {
		text   string
		valid  bool
		status string
	}{
		{"WORKER_STATUS: ready", true, "ready"},
		{"WORKER_STATUS: blocked\nWORKER_REASON: dependency is missing", true, "blocked"},
		{"done", false, ""},
		{"WORKER_STATUS: blocked", false, ""},
		{"WORKER_STATUS: ready\nWORKER_REASON: extra", false, ""},
		{"WORKER_STATUS: ready\nWORKER_STATUS: ready", false, ""},
	}
	for _, tc := range cases {
		got := parseWorkerResult(tc.text)
		if got.Valid != tc.valid || got.Status != tc.status {
			t.Errorf("parseWorkerResult(%q) = %+v", tc.text, got)
		}
	}
}
