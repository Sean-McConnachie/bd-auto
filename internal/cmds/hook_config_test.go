package cmds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The unit tests above hold Hook to its contract in-process. This file holds
// the shipped artefacts to it: the binary this repo builds, and the hooks
// config this repo checks in, run against each other the way Claude Code runs
// them.
//
// The bug this guards against was exactly a gap between those two halves. A
// session had hooks registered at
//
//	"${CLAUDE_PLUGIN_ROOT}"/bin/bd-auto hook pre-tool-use
//	"${CLAUDE_PLUGIN_ROOT}"/bin/bd-auto hook stop
//
// against a binary with no `hook` command. Both printed usage and exited 2, and
// exit 2 is what Claude Code reads as "block": every Bash call refused before
// it ran, every turn end refused and the model immediately re-invoked. In-
// process tests cannot see that gap, because in-process there is only one half.

// Every event the binary might be pointed at has to come back 0, whether or not
// bd-auto has anything to say about it. This is the set a future hooks config
// can draw from, so passing it is what makes adding one safe.
func TestBinaryExitsZeroForEveryHookEvent(t *testing.T) {
	bin := buildBDAuto(t)
	for _, event := range hookEvents {
		for _, spelling := range []string{event, kebab(event)} {
			out, code := runCommand(t, bin+" hook "+spelling, hookEnv(pluginRootOf(bin)), payloadFor(event))
			if code != 0 {
				t.Errorf("`bd-auto hook %s` exited %d: %s", spelling, code, out)
			}
		}
	}
	// And the two spellings from the report, together with the argument shapes a
	// config gets wrong: no event, an unknown event, a flag where the event goes.
	for _, args := range []string{"", "pre-tool-use", "stop", "no-such-event", "--help"} {
		out, code := runCommand(t, bin+" hook "+args, hookEnv(pluginRootOf(bin)), payloadFor("Stop"))
		if code != 0 {
			t.Errorf("`bd-auto hook %s` exited %d: %s", args, code, out)
		}
	}
}

// Nothing on stdout, from the binary rather than from the function. Claude Code
// parses a hook's stdout as JSON that can permit, deny or add context, so a
// stray line there is a decision nobody made.
func TestBinaryWritesNothingToStdoutOnTheHookPath(t *testing.T) {
	bin := buildBDAuto(t)
	for _, event := range hookEvents {
		cmd := exec.Command(bin, "hook", event)
		cmd.Env = hookEnv(pluginRootOf(bin))
		cmd.Stdin = strings.NewReader(payloadFor(event))
		out, err := cmd.Output() // stdout only; stderr is allowed to carry commentary
		if err != nil {
			t.Errorf("`bd-auto hook %s`: %v", event, err)
		}
		if len(out) > 0 {
			t.Errorf("`bd-auto hook %s` wrote %q to stdout", event, out)
		}
	}
}

// A Stop payload with stop_hook_active set, end to end. The in-process test
// proves the handler is skipped; this proves the process exits 0, which is the
// half that decides whether the session can ever end a turn again.
func TestBinaryHonoursStopHookActive(t *testing.T) {
	bin := buildBDAuto(t)
	for _, event := range []string{"Stop", "SubagentStop"} {
		payload := `{"session_id":"s1","hook_event_name":"` + event + `","stop_hook_active":true}`
		out, code := runCommand(t, bin+" hook "+kebab(event), hookEnv(pluginRootOf(bin)), payload)
		if code != 0 {
			t.Errorf("`bd-auto hook %s` with stop_hook_active exited %d: %s", event, code, out)
		}
	}
}

// Every hook this repo checks in that points at bd-auto, run as written.
//
// Today the plugin registers none, and this reports that rather than passing
// silently — "we ship no hooks" is a real answer to "which hooks should bd-auto
// register", and worth seeing in the test output rather than inferring from a
// test that did nothing. The moment a config appears it is checked twice over,
// below.
func TestShippedHooksConfigCannotWedgeASession(t *testing.T) {
	root := repoRoot(t)
	hooks := shippedHooks(t, root)
	if len(hooks) == 0 {
		t.Log("no checked-in hooks config points at bd-auto; nothing registered means nothing to wedge")
		return
	}

	for _, h := range hooks {
		checkHookCommandCannotBlock(t, h)
	}
}

// The same two checks against a config that is not shipped, so the machinery
// above is exercised whether or not the plugin registers anything, and so the
// wrapping the second check demands has a worked example to point at.
//
// The example also proves the second check is about the wrapping rather than
// about something incidental: the same command unwrapped fails it.
func TestAWrappedHookCommandSurvivesAMissingBinary(t *testing.T) {
	for _, h := range parseHooksConfig(t, repoRoot(t), "testdata/hooks-example.json",
		readFile(t, filepath.Join(repoRoot(t), "internal", "cmds", "testdata", "hooks-example.json"))) {
		checkHookCommandCannotBlock(t, h)

		unwrapped := strings.TrimSuffix(h.command, " || true")
		if unwrapped == h.command {
			t.Fatalf("the example command is not wrapped, so it proves nothing: %s", h.command)
		}
		if _, code := runCommand(t, unwrapped, hookEnv(absentPluginRoot(t)), payloadFor(h.event)); code == 0 {
			t.Errorf("`%s` exited 0 with bd-auto absent; the missing-binary check cannot fail, so it checks nothing", unwrapped)
		}
	}
}

// checkHookCommandCannotBlock runs one hook command the two ways it has to
// survive.
func checkHookCommandCannotBlock(t *testing.T, h shippedHook) {
	t.Helper()

	// As written, against the binary as built. This is the ordinary case, and
	// the one the original bug failed.
	out, code := runCommand(t, h.command, hookEnv(pluginRootOf(buildBDAuto(t))), payloadFor(h.event))
	if code != 0 {
		t.Errorf("%s\n  %s\n  exited %d against the built binary: %s", h.source, h.command, code, out)
	}

	// And with no bd-auto anywhere: the version-skew case in the other
	// direction, where the config ships ahead of the binary, or the plugin
	// directory moved, or the build has not been run yet. A hook command we ship
	// has to exit 0 on its own when the thing it calls is missing — there is no
	// binary left to fail open on its behalf.
	out, code = runCommand(t, h.command, hookEnv(absentPluginRoot(t)), payloadFor(h.event))
	if code != 0 {
		t.Errorf("%s\n  %s\n  exited %d with bd-auto absent: %s\n"+
			"  wrap it so it cannot block, e.g. `... || true`", h.source, h.command, code, out)
	}
}

// shippedHook is one hook command found in a checked-in config.
type shippedHook struct {
	source  string // where it was found, for the failure message
	event   string // the Claude Code event it is registered under
	command string // the shell command, verbatim
}

// shippedHooks collects every hook command this repo checks in that invokes
// bd-auto. Hooks that call something else are somebody else's contract; this
// test is about the plugin keeping its own two halves in step.
func shippedHooks(t *testing.T, root string) []shippedHook {
	t.Helper()
	var found []shippedHook
	for _, rel := range []string{
		filepath.Join(".claude-plugin", "plugin.json"),
		filepath.Join("hooks", "hooks.json"),
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".claude", "settings.local.json"),
	} {
		path := filepath.Join(root, rel)
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, h := range parseHooksConfig(t, root, rel, raw) {
			if strings.Contains(h.command, "bd-auto") {
				found = append(found, h)
			}
		}
	}
	return found
}

// parseHooksConfig pulls the (event, command) pairs out of a Claude Code hooks
// config. The shape is the same wherever it lives:
//
//	{"hooks": {"<Event>": [{"matcher": "...", "hooks": [{"type": "command", "command": "..."}]}]}}
//
// A plugin manifest may instead point its "hooks" key at a file, so that case
// is followed rather than skipped.
func parseHooksConfig(t *testing.T, root, source string, raw []byte) []shippedHook {
	t.Helper()
	var outer struct {
		Hooks json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}
	if len(outer.Hooks) == 0 {
		return nil
	}

	// "hooks": "./hooks/hooks.json"
	var path string
	if json.Unmarshal(outer.Hooks, &path) == nil {
		nested, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("%s points its hooks at %s, which does not load: %v", source, path, err)
		}
		return parseHooksConfig(t, root, source+" -> "+path, nested)
	}

	var byEvent map[string][]struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(outer.Hooks, &byEvent); err != nil {
		t.Fatalf("parse the hooks block of %s: %v", source, err)
	}

	var found []shippedHook
	for event, matchers := range byEvent {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if h.Command != "" {
					found = append(found, shippedHook{source: source, event: event, command: h.Command})
				}
			}
		}
	}
	return found
}

// payloadFor is a minimal, well-formed stdin payload for an event. Hooks are
// handed more than this; the point is that the hook survives what it is given,
// not that this reproduces the CLI's exact JSON.
func payloadFor(event string) string {
	p := map[string]any{
		"session_id":      "hook-config-test",
		"transcript_path": "/dev/null",
		"cwd":             ".",
		"hook_event_name": event,
	}
	switch normalizeHookEvent(event) {
	case "pretooluse", "posttooluse":
		p["tool_name"] = "Bash"
		p["tool_input"] = map[string]any{"command": "true"}
	case "stop", "subagentstop":
		p["stop_hook_active"] = false
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// hookEnv is the environment Claude Code runs a hook command in, as far as the
// command can tell: CLAUDE_PLUGIN_ROOT pointing at the plugin, and its bin/ on
// PATH so a config that writes a bare `bd-auto` resolves too.
func hookEnv(pluginRoot string) []string {
	return append(os.Environ(),
		"CLAUDE_PLUGIN_ROOT="+pluginRoot,
		"PATH="+filepath.Join(pluginRoot, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
}

// pluginRootOf turns <root>/bin/bd-auto back into <root>.
func pluginRootOf(bin string) string { return filepath.Dir(filepath.Dir(bin)) }

// absentPluginRoot is a plugin root with no bd-auto in it, which is what a
// config that has outrun its binary is pointed at.
func absentPluginRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// runCommand runs a hook command the way Claude Code does — through a shell,
// with the payload on stdin — and returns its combined output and exit code.
func runCommand(t *testing.T, command string, env []string, stdin string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		// A hook that hangs is as bad as one that blocks: the tool call waits.
		t.Fatalf("`%s` did not finish within the timeout", command)
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("`%s`: %v", command, err)
		}
		code = ee.ExitCode()
	}
	return strings.TrimSpace(string(out)), code
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// buildBDAuto compiles the binary once per test run and returns its path. It is
// laid out as <dir>/bin/bd-auto so CLAUDE_PLUGIN_ROOT can point at <dir> and the
// path from the bug report — "${CLAUDE_PLUGIN_ROOT}"/bin/bd-auto — resolves.
func buildBDAuto(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		bin := filepath.Join(buildDir, "bin", "bd-auto")
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			buildErr = err
			return
		}
		cmd := exec.Command("go", "build", "-o", bin, "bd-auto/cmd/bd-auto")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("%w\n%s", err, out)
			return
		}
		builtBin = bin
	})
	if buildErr != nil {
		t.Fatalf("build bd-auto: %v", buildErr)
	}
	return builtBin
}

// buildDir holds the binary the tests in this file run. It outlives any one
// test, so it is torn down by TestMain rather than by t.Cleanup.
var buildDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bd-auto-hook-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	buildDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// repoRoot walks up from the package directory to the module root, which is
// where the checked-in configs live.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}
