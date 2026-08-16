package prompts

import (
	"strings"
	"testing"
)

// Every role prompt has to override the repo's own CLAUDE.md, which in this
// repo says work is not complete until `git push` succeeds. A prompt that lost
// that section would leave a worker being told a push is mandatory by one
// instruction set and blocked from pushing by another.
func TestEveryRolePromptOverridesTheRepoInstructions(t *testing.T) {
	for _, role := range Roles() {
		p, err := For(role)
		if err != nil {
			t.Fatalf("For(%q): %v", role, err)
		}
		if len(p) < 200 {
			t.Fatalf("the %s prompt is %d bytes; it did not embed", role, len(p))
		}
		if !strings.Contains(p, "CLAUDE.md") {
			t.Fatalf("the %s prompt does not override the repo's CLAUDE.md", role)
		}
	}
}

func TestForRejectsAnUnknownRole(t *testing.T) {
	if _, err := For("nobody"); err == nil {
		t.Fatal("an unknown role returned a prompt")
	} else if !strings.Contains(err.Error(), "worker") {
		t.Fatalf("the error does not list the roles that exist: %v", err)
	}
}
