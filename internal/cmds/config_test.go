package cmds

import (
	"reflect"
	"testing"

	"bd-auto/internal/config"
)

// `config show` is how a reader confirms the file was read at all, so a block
// it leaves out is a block nobody can check. ask is the one whose defaults are
// least guessable — on unless turned off, and deliberately not the reviewer.
func TestShowResolvesTheAskBlock(t *testing.T) {
	got := describeAsk(config.Default())
	want := map[string]any{
		"enabled": true,
		"timeout": config.DefaultAskTimeout,
		"hold":    config.DefaultAskHold,
		"roles":   []string{"worker", "integrator"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("describeAsk(defaults) = %v, want %v", got, want)
	}
}

func TestShowReportsAWaitForeverTimeoutTheWayItIsWritten(t *testing.T) {
	c := config.Default()
	off := false
	zero, hold := 0, 30
	c.Ask.Enabled, c.Ask.Timeout, c.Ask.Hold = &off, &zero, &hold
	c.Ask.Roles = []string{"worker"}

	got := describeAsk(c)
	// AskTimeout spells "forever" as a negative duration; the file spells it 0,
	// and this is the file's view.
	if got["timeout"] != 0 {
		t.Errorf("timeout = %v, want 0 for a question that waits forever", got["timeout"])
	}
	if got["enabled"] != false {
		t.Errorf("enabled = %v, want false", got["enabled"])
	}
	if got["hold"] != 30 {
		t.Errorf("hold = %v, want 30", got["hold"])
	}
	// The roles are reported as configured, so what is printed is what the run
	// will actually honour.
	if roles := got["roles"]; !reflect.DeepEqual(roles, []string{"worker"}) {
		t.Errorf("roles = %v, want [worker]", roles)
	}
}

func TestShowReportsProviderNativeRunnerSettings(t *testing.T) {
	c := config.Default()
	c.Runners = map[string]config.RunnerSpec{
		config.RoleDefault: {
			Provider: config.CodexProvider,
			Model:    config.DefaultCodexModel,
			Codex: &config.CodexRunnerConfig{
				Sandbox: "workspace-write", ApprovalPolicy: "never",
				Tools: config.CodexTools{Shell: config.Yes(), WebSearch: config.No(), ViewImage: config.No()},
			},
		},
	}
	got, ok := describeRunners(c)["worker"].(map[string]any)
	if !ok {
		t.Fatalf("worker runner has unexpected shape: %T", describeRunners(c)["worker"])
	}
	if got["sandbox"] != "workspace-write" || got["approval_policy"] != "never" {
		t.Fatalf("Codex settings = %v", got)
	}
	if _, ok := got["permissions"]; ok {
		t.Fatalf("a Codex runner must not be displayed with Claude permissions: %v", got)
	}
	tools, ok := got["tools"].(map[string]bool)
	if !ok || !tools["shell"] || tools["web_search"] || tools["view_image"] {
		t.Fatalf("Codex tools = %v", got["tools"])
	}
}
