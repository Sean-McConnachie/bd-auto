package cmds

import (
	"reflect"
	"strings"
	"testing"

	"bd-auto/internal/scope"
)

func candidates(ids ...string) scope.Set {
	set := scope.Set{Epic: "epic-1", Skipped: map[string]string{}}
	for _, id := range ids {
		set.Issues = append(set.Issues, scope.Issue{ID: id, Title: "issue " + id, Status: "open"})
	}
	return set
}

// The rule the whole command exists for. There is no budget anywhere in this
// engine, so a run with nobody watching has to name its work — and the refusal
// has to happen before anything is spawned, which is what makes it a decision
// over flags rather than something the engine discovers.
func TestHeadlessDrainWithNoExplicitScopeRefusesToStart(t *testing.T) {
	set := candidates("t-1", "t-2", "t-3")

	sel, prompt, err := chooseScope(set, "", false, false, true)
	if err == nil {
		t.Fatal("a headless drain with no scope must fail, not run everything")
	}
	if sel != nil || prompt {
		t.Fatalf("nothing may be selected and nothing prompted: sel=%v prompt=%v", sel, prompt)
	}
	for _, want := range []string{"--issues", "--all", "epic-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal %q does not say how to fix it (%q missing)", err, want)
		}
	}
}

// The explicit forms work with or without a terminal: they are the ones that
// name their work.
func TestExplicitScopeNeedsNoTerminal(t *testing.T) {
	set := candidates("t-1", "t-2", "t-3")

	sel, prompt, err := chooseScope(set, "t-1,t-3", false, false, true)
	if err != nil || prompt {
		t.Fatalf("--issues headless: %v (prompt=%v)", err, prompt)
	}
	if !reflect.DeepEqual(sel, []string{"t-1", "t-3"}) {
		t.Fatalf("selected %v, want the named issues", sel)
	}

	sel, prompt, err = chooseScope(set, "", true, false, true)
	if err != nil || prompt {
		t.Fatalf("--all headless: %v (prompt=%v)", err, prompt)
	}
	if !reflect.DeepEqual(sel, set.IDs()) {
		t.Fatalf("--all selected %v, want every candidate", sel)
	}

	// A name that is not a candidate is a typo, a closed issue or something from
	// another epic, and every one of those is better said now.
	if _, _, err := chooseScope(set, "t-1,nope", false, false, true); err == nil {
		t.Fatal("an unknown ID must be rejected before anything is spawned")
	}
}

// On a terminal the answer comes from a human, so the flags alone must not
// produce one.
func TestATerminalWithNoFlagsAsksTheHuman(t *testing.T) {
	sel, prompt, err := chooseScope(candidates("t-1"), "", false, false, false)
	if err != nil || !prompt || sel != nil {
		t.Fatalf("want a prompt and no selection: sel=%v prompt=%v err=%v", sel, prompt, err)
	}
}

// --dry-run spawns nothing, so there is nothing to approve; it shows the whole
// candidate set as the hypothetical scope even with no terminal.
func TestDryRunNeedsNoSelection(t *testing.T) {
	set := candidates("t-1", "t-2")
	sel, prompt, err := chooseScope(set, "", false, true, true)
	if err != nil || prompt {
		t.Fatalf("--dry-run: %v (prompt=%v)", err, prompt)
	}
	if !reflect.DeepEqual(sel, set.IDs()) {
		t.Fatalf("selected %v, want every candidate", sel)
	}
}

// The list is numbered on screen, so a human should be able to answer with what
// they see — and an answer that is off by one must be rejected rather than
// silently running the wrong issue.
func TestPickAcceptsNumbersRangesAndIDs(t *testing.T) {
	set := candidates("t-1", "t-2", "t-3", "t-4")

	cases := map[string][]string{
		"all":       {"t-1", "t-2", "t-3", "t-4"},
		"2":         {"t-2"},
		"1,3":       {"t-1", "t-3"},
		"2-4":       {"t-2", "t-3", "t-4"},
		"t-3":       {"t-3"},
		"3, 1 ,t-3": {"t-1", "t-3"},
	}
	for answer, want := range cases {
		got, err := pick(set, answer)
		if err != nil {
			t.Fatalf("pick(%q): %v", answer, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("pick(%q) = %v, want %v", answer, got, want)
		}
	}

	for _, bad := range []string{"0", "5", "2-9", "t-9", "banana", ""} {
		if got, err := pick(set, bad); err == nil {
			t.Fatalf("pick(%q) returned %v; it must be rejected", bad, got)
		}
	}
}
