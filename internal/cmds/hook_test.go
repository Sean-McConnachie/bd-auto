package cmds

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// hookEvents is the event names Claude Code sends a hook, in the PascalCase the
// payload uses. It is the set any hooks config we might ever ship draws from, so
// it is the set the binary has to survive.
//
// It is also a snapshot of one CLI version, and deliberately not maintained as
// an allowlist: the tests below cover names that are not on it just as hard,
// because an event this list has not heard of is the version-skew case, and the
// answer to that is the same silent zero.
var hookEvents = []string{
	"PreToolUse",
	"PostToolUse",
	"UserPromptSubmit",
	"Notification",
	"Stop",
	"SubagentStop",
	"PreCompact",
	"SessionStart",
	"SessionEnd",
}

// The headline guarantee, and the whole reason this command exists: nothing you
// can put after `bd-auto hook` makes it exit non-zero or print to stdout.
//
// Both halves matter. A non-zero exit is what Claude Code reads as "block", and
// on PreToolUse that refuses every tool call while on Stop it refuses every
// turn end and re-invokes the model — a session that can neither act nor stop,
// and cannot fix the hook because fixing it needs the shell being blocked. A
// stray line on stdout is milder but still wrong: Claude Code parses stdout as
// hook JSON, and a hook that only meant to log ends up making a decision.
func TestHookExitsZeroAndSaysNothingForEveryEvent(t *testing.T) {
	names := []string{"", "not-an-event", "--help", "-", "Stop; rm -rf /", strings.Repeat("x", 4096)}
	for _, e := range hookEvents {
		names = append(names, e, kebab(e), strings.ToUpper(e))
	}

	for _, name := range names {
		t.Run(displayName(name), func(t *testing.T) {
			var args []string
			if name != "" {
				args = []string{name}
			}
			out, err := runHook(t, args, `{"session_id":"s1"}`)
			if err != nil {
				t.Fatalf("bd-auto hook %q returned %v; a hook that errors blocks the session", name, err)
			}
			if out != "" {
				t.Fatalf("bd-auto hook %q wrote %q to stdout; stdout is parsed as a hook decision", name, out)
			}
		})
	}
}

// No arguments at all, which is what a hooks config with a missing event looks
// like. It prints usage — to stderr, so it cannot be read as a decision — and
// still exits 0, because an argument-parsing mistake in a config file is not
// worth a session.
func TestHookWithNoArgumentsStillExitsZero(t *testing.T) {
	out, err := runHook(t, nil, "")
	if err != nil {
		t.Fatalf("bare `bd-auto hook` returned %v", err)
	}
	if out != "" {
		t.Fatalf("bare `bd-auto hook` wrote %q to stdout", out)
	}
}

// A payload that is not JSON, or is JSON of the wrong shape, or is absent. Hook
// input comes from a CLI that adds fields between versions; refusing to proceed
// on input we cannot parse would turn every such addition into a wedged
// session.
func TestHookIgnoresUnreadablePayloads(t *testing.T) {
	for _, in := range []string{"", "not json at all", "[]", `{"stop_hook_active":"yes"}`, "{"} {
		if _, err := runHook(t, []string{"Stop"}, in); err != nil {
			t.Fatalf("payload %q returned %v", in, err)
		}
	}
}

// stop_hook_active is Claude Code's own escape hatch: it is true when the model
// is only running because a Stop hook blocked the last turn end. A handler that
// runs anyway can block a second time, and a Stop hook that blocks every time
// blocks forever.
func TestHookHonoursStopHookActive(t *testing.T) {
	for _, event := range []string{"Stop", "SubagentStop"} {
		t.Run(event, func(t *testing.T) {
			ran := false
			registerHook(t, normalizeHookEvent(event), func(hookPayload) error {
				ran = true
				return nil
			})

			if _, err := runHook(t, []string{event}, `{"stop_hook_active":true}`); err != nil {
				t.Fatalf("returned %v", err)
			}
			if ran {
				t.Fatal("the handler ran with stop_hook_active set; a second block here traps the session")
			}

			if _, err := runHook(t, []string{event}, `{"stop_hook_active":false}`); err != nil {
				t.Fatalf("returned %v", err)
			}
			if !ran {
				t.Fatal("the handler did not run on an ordinary stop; the guard is too wide")
			}
		})
	}
}

// The guard is for stop events only. Reading it as a blanket off-switch would
// silently disable every other hook for the rest of a turn that once blocked.
func TestStopHookActiveDoesNotSuppressOtherEvents(t *testing.T) {
	ran := false
	registerHook(t, "pretooluse", func(hookPayload) error {
		ran = true
		return nil
	})
	if _, err := runHook(t, []string{"PreToolUse"}, `{"stop_hook_active":true}`); err != nil {
		t.Fatalf("returned %v", err)
	}
	if !ran {
		t.Fatal("stop_hook_active suppressed a PreToolUse handler; it applies to stop events only")
	}
}

// A handler that returns an error has a bug, or hit a git repo in a state it
// did not expect. Neither is a reason to refuse the tool call that triggered it.
func TestHookSwallowsAHandlerError(t *testing.T) {
	registerHook(t, "pretooluse", func(hookPayload) error {
		return errors.New("boom")
	})
	if _, err := runHook(t, []string{"PreToolUse"}, "{}"); err != nil {
		t.Fatalf("a handler error escaped as %v; it would exit non-zero and block the call", err)
	}
}

// Same for a panic, which matters more than it looks: an unrecovered Go panic
// exits 2, and 2 is the exact code Claude Code reads as "block".
func TestHookSurvivesAPanickingHandler(t *testing.T) {
	registerHook(t, "pretooluse", func(hookPayload) error {
		var m map[string]string
		m["nil"] = "deref"
		return nil
	})
	if _, err := runHook(t, []string{"PreToolUse"}, "{}"); err != nil {
		t.Fatalf("a panicking handler escaped as %v", err)
	}
}

// The one way out with a non-zero exit, so that "fail open" does not also mean
// "bd-auto can never block anything on purpose".
func TestHookBlockIsTheOnlyNonZeroExit(t *testing.T) {
	registerHook(t, "pretooluse", func(hookPayload) error {
		return hookBlock()
	})
	_, err := runHook(t, []string{"PreToolUse"}, "{}")
	code, ok := ExitCode(err)
	if !ok || code != 2 {
		t.Fatalf("a deliberate block came out as (%d, %v) from %v; Claude Code reads 2 as block", code, ok, err)
	}
}

// The handler sees the payload it was sent, including the parts hookPayload
// does not name — an event's own fields live in Raw rather than in a struct
// this package has to keep in step with the CLI.
func TestHookPassesThePayloadThrough(t *testing.T) {
	var got hookPayload
	registerHook(t, "pretooluse", func(p hookPayload) error {
		got = p
		return nil
	})
	if _, err := runHook(t, []string{"PreToolUse"}, `{"hook_event_name":"PreToolUse","tool_name":"Bash"}`); err != nil {
		t.Fatalf("returned %v", err)
	}
	if got.Event != "PreToolUse" {
		t.Errorf("event came through as %q", got.Event)
	}
	if !strings.Contains(string(got.Raw), "Bash") {
		t.Errorf("the unnamed half of the payload did not survive: %q", got.Raw)
	}
}

// A hooks config that names no event on the command line still reaches the
// right handler, because the payload names the event too.
func TestHookFallsBackToTheEventInThePayload(t *testing.T) {
	ran := false
	registerHook(t, "posttooluse", func(hookPayload) error {
		ran = true
		return nil
	})
	if _, err := runHook(t, nil, `{"hook_event_name":"PostToolUse"}`); err != nil {
		t.Fatalf("returned %v", err)
	}
	if !ran {
		t.Fatal("the payload's hook_event_name was ignored when no event was given on the command line")
	}
}

func TestNormalizeHookEvent(t *testing.T) {
	for in, want := range map[string]string{
		"PreToolUse":   "pretooluse",
		"pre-tool-use": "pretooluse",
		"pre_tool_use": "pretooluse",
		"  Stop  ":     "stop",
		"":             "",
		"---":          "",
	} {
		if got := normalizeHookEvent(in); got != want {
			t.Errorf("normalizeHookEvent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadHookPayloadIsBounded(t *testing.T) {
	p := readHookPayload(strings.NewReader(strings.Repeat("x", hookStdinLimit*2)))
	if len(p.Raw) != hookStdinLimit {
		t.Fatalf("read %d bytes, want the %d-byte cap", len(p.Raw), hookStdinLimit)
	}
}

// runHook calls Hook with stdin and stdout replaced, and returns whatever it
// wrote to stdout. Hook reads os.Stdin directly because that is what a hook
// process is handed; the test has to hand it the same thing.
func runHook(t *testing.T, args []string, stdin string) (string, error) {
	t.Helper()
	swapStdin(t, stdin)
	out := captureStdout(t)
	err := Hook(args)
	return out(), err
}

func swapStdin(t *testing.T, contents string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer w.Close()
		io.WriteString(w, contents)
	}()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
}

// captureStdout redirects os.Stdout and returns a function yielding what was
// written to it. Calling it restores stdout.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	var out string
	read := func() string {
		if w != nil {
			os.Stdout = old
			w.Close()
			w = nil
			out = <-done
			r.Close()
		}
		return out
	}
	t.Cleanup(func() { read() })
	return read
}

// registerHook installs a handler for one event for the duration of a test.
// hookHandlers ships empty, so this is the only way to exercise the guarantees
// that sit around a handler.
func registerHook(t *testing.T, event string, h hookHandler) {
	t.Helper()
	prev, had := hookHandlers[event]
	hookHandlers[event] = h
	t.Cleanup(func() {
		if had {
			hookHandlers[event] = prev
			return
		}
		delete(hookHandlers, event)
	})
}

// kebab renders a PascalCase event name the way a hooks config command line
// spells it: PreToolUse -> pre-tool-use.
func kebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func displayName(s string) string {
	switch {
	case s == "":
		return "empty"
	case len(s) > 24:
		return s[:24] + "..."
	}
	return s
}
