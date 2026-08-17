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
	c.Ask.Roles = []string{"bd-worker"}

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
	// A plugin-era role name resolves to the role it now means, so what is
	// printed is what the run will actually honour.
	if roles := got["roles"]; !reflect.DeepEqual(roles, []string{"worker"}) {
		t.Errorf("roles = %v, want [worker]", roles)
	}
}
