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

// A wave is bd's ready front narrowed to the run's scope, so no issue in one
// can block another in it. A worker that stops waiting for a sibling has lost
// the run an issue for a reason that cannot happen, and the only thing standing
// between a model and that reading is this text.
func TestTheWorkerPromptSaysASiblingIsNotABlocker(t *testing.T) {
	p, err := For("worker")
	if err != nil {
		t.Fatalf("For(worker): %v", err)
	}
	for _, want := range []string{
		// That it cannot be a blocker...
		"never a blocker",
		// ...and what to do instead of parking, which is the half a worker
		// acts on.
		"record it as a discovery",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("the worker prompt does not say %q, so a worker may park on a sibling", want)
		}
	}
}
