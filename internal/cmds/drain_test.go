package cmds

import (
	"bd-auto/internal/runstate"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"bd-auto/internal/config"
	"bd-auto/internal/drain"
	"bd-auto/internal/runner"
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

// The permission escape hatch has to be spelled the same on every command that
// spawns a model. issue run is where a stuck run gets debugged and drain is
// where it gets fixed, so a flag on one and not the other sends someone round
// the loop twice. Reaching the "name your scope" error means drain parsed it.
func TestDrainAcceptsTheSkipPermissionsFlag(t *testing.T) {
	err := Drain([]string{"--dangerously-skip-permissions"})
	if err == nil {
		t.Fatal("a drain with no scope must still refuse to start")
	}
	if strings.Contains(err.Error(), "not defined") {
		t.Fatalf("drain does not accept the flag issue run does: %v", err)
	}
	if !strings.Contains(err.Error(), "--epic") {
		t.Fatalf("expected the missing-scope refusal, got: %v", err)
	}
}

func TestModelSpawningCommandsAcceptAPIBillingConsentFlag(t *testing.T) {
	if err := Drain([]string{"--allow-api-billing"}); err == nil || strings.Contains(err.Error(), "not defined") {
		t.Fatalf("drain did not parse --allow-api-billing: %v", err)
	}
	if err := Issue([]string{"run", "--allow-api-billing"}); err == nil || strings.Contains(err.Error(), "not defined") {
		t.Fatalf("issue run did not parse --allow-api-billing: %v", err)
	}
}

func TestStructuredBillingRefusalRemainsValidJSON(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	err = reportBillingRefusal(&drain.BillingError{Source: runner.BillingAPIKey, Roles: []string{"worker"}}, true)
	w.Close()
	os.Stdout = old
	defer r.Close()

	if code, ok := ExitCode(err); !ok || code != 1 {
		t.Fatalf("structured refusal error = %v, want silent exit 1", err)
	}
	var got map[string]any
	if err := json.NewDecoder(r).Decode(&got); err != nil {
		t.Fatalf("refusal is not valid JSON: %v", err)
	}
	if got["billing_source"] != string(runner.BillingAPIKey) || got["rerun_flag"] != "--allow-api-billing" || got["allowed"] != false {
		t.Fatalf("refusal JSON = %#v", got)
	}
}

// The preview is what a human approves, so the permission level each role will
// run at has to be in it — that is the other thing being agreed to alongside the
// spend, and --dangerously-skip-permissions is only honest if the table it lands
// in front of shows what it did.
func TestThePreviewNamesEachRolesPermissionLevel(t *testing.T) {
	c := &Ctx{RepoRoot: t.TempDir(), Cfg: config.Default()}
	before := preview(c, candidates("t-1"), []string{"t-1"}, 1)
	if !strings.Contains(before, "worker") || !strings.Contains(before, "(auto)") {
		t.Fatalf("the preview does not show the worker's permission level:\n%s", before)
	}
	if !strings.Contains(before, "(scoped)") {
		t.Fatalf("the preview does not show the reviewer's permission level:\n%s", before)
	}

	c.Cfg.ForcePermissions = runner.PermBypass
	after := preview(c, candidates("t-1"), []string{"t-1"}, 1)
	if strings.Contains(after, "(auto)") || strings.Contains(after, "(scoped)") {
		t.Fatalf("the flag was applied but the preview still shows the old levels:\n%s", after)
	}
	if !strings.Contains(after, "(bypass)") {
		t.Fatalf("the preview does not show the level the run will actually use:\n%s", after)
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

// The live view is a terminal's privilege and nothing else's. Every other form
// of the command — a skill launcher, CI, a redirected log, --json for a parser —
// falls back to a renderer that writes lines, and the decision is made here
// rather than discovered by a TUI trying to size a terminal that is not there.
func TestTheLiveViewIsForTerminalsOnly(t *testing.T) {
	cases := []struct {
		name                      string
		quiet, asJSON, plain, tty bool
		want                      bool
	}{
		{name: "a terminal with no flags gets the table", tty: true, want: true},
		{name: "no terminal, no table"},
		{name: "--plain is an explicit refusal", plain: true, tty: true},
		{name: "--json is for a parser", asJSON: true, tty: true},
		{name: "--quiet asked for nothing", quiet: true, tty: true},
		{name: "a flag beats a terminal", plain: true, asJSON: true, tty: true},
	}
	for _, c := range cases {
		if got := liveView(c.quiet, c.asJSON, c.plain, c.tty); got != c.want {
			t.Fatalf("%s: liveView(quiet=%v json=%v plain=%v tty=%v) = %v, want %v",
				c.name, c.quiet, c.asJSON, c.plain, c.tty, got, c.want)
		}
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

// The prompt hands the terminal straight to the live view, so a key typed into
// the gap between the confirmation and the first frame — a run can take seconds
// to spawn its first worker — has to survive the handover. It only survives if
// the prompt reads exactly its answer and no further: the view reads the
// terminal, not whatever the prompt buffered and then dropped.
func TestTheScopePromptLeavesTypeAheadForTheLiveView(t *testing.T) {
	hushStderr(t)
	c := &Ctx{RepoRoot: t.TempDir(), Cfg: config.Default()}
	set := candidates("t-1", "t-2")
	in := strings.NewReader("all\ny\nkq")

	sel, err := selectInteractively(c, in, set, 1)
	if err != nil {
		t.Fatalf("selectInteractively: %v", err)
	}
	if !reflect.DeepEqual(sel, set.IDs()) {
		t.Fatalf("selected %v, want every candidate", sel)
	}

	rest, err := io.ReadAll(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "kq" {
		t.Fatalf("the prompt left %q for the live view, want %q: those keys are lost", rest, "kq")
	}
}

// The property the prompt rests on, stated on its own.
func TestReadLineReadsOneLineAndStops(t *testing.T) {
	in := strings.NewReader("all\ny\nkq")

	for _, want := range []string{"all", "y"} {
		got, err := readLine(in)
		if got != want || err != nil {
			t.Fatalf("readLine = (%q, %v), want (%q, nil)", got, err, want)
		}
	}
	// An unterminated last line is still an answer, and it comes back with the
	// EOF so a caller can tell it from a line that was finished.
	if got, err := readLine(in); got != "kq" || err != io.EOF {
		t.Fatalf("readLine = (%q, %v), want (\"kq\", EOF)", got, err)
	}
	if got, err := readLine(in); got != "" || err != io.EOF {
		t.Fatalf("readLine = (%q, %v), want (\"\", EOF)", got, err)
	}
}

// hushStderr sends the prompt's own output where the test log is not. It writes
// a full preview twice, which is a page of text and none of it is what is being
// checked.
func hushStderr(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w

	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() {
		os.Stderr = old
		w.Close()
		<-done
		r.Close()
	})
}

// A run killed after its workers closed their issues and before its barrier
// finished leaves nothing open under the epic, branches nobody merged, and
// possibly a checkout mid-merge. Asking bd what to run refuses the one command
// that would finish it, so the unfinished run's own scope is what a restart
// picks up -- that is what run.json is for.
func TestARunLeftUnfinishedIsResumedFromItsOwnScope(t *testing.T) {
	dir := t.TempDir()
	c := &Ctx{RepoRoot: dir, Cfg: config.Default()}

	st := runstate.New("epic-1", 1, "auto", 0)
	st.Status = runstate.StatusActive
	st.Scope = []string{"t-1", "t-2"}
	if err := runstate.Save(dir, st); err != nil {
		t.Fatal(err)
	}

	got, ok := unfinishedScope(c, "epic-1")
	if !ok {
		t.Fatal("an active run with a scope was not offered for resuming")
	}
	if diff := strings.Join(got.IDs(), ","); diff != "t-1,t-2" {
		t.Fatalf("resumed scope is %q, want the run's own list", diff)
	}

	// Another epic's run is not this one's to finish.
	if _, ok := unfinishedScope(c, "epic-2"); ok {
		t.Fatal("a run under a different epic was offered for resuming")
	}

	// And a run that finished has nothing to resume.
	st.Status = runstate.StatusDone
	if err := runstate.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	if _, ok := unfinishedScope(c, "epic-1"); ok {
		t.Fatal("a finished run was offered for resuming")
	}
}
