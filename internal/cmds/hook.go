package cmds

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

// hookUsage is what `bd-auto hook` prints when it cannot tell which event it is
// answering. It goes to stderr, and the command still exits 0 — see Hook.
const hookUsage = `bd-auto hook <event>

The entry point for a Claude Code hook. Reads the hook payload as JSON on
stdin and runs whatever bd-auto has registered for that event.

Exits 0 for every event name, recognised or not, and writes nothing to stdout.
Called by Claude Code, not by hand.
`

// hookStdinLimit caps the payload read from stdin. A hook is on the path of
// every tool call; an unbounded read there is a way for one enormous tool input
// to become a memory problem in a process that only wanted one boolean.
const hookStdinLimit = 1 << 20

// Hook implements `bd-auto hook <event>`, the entry point every Claude Code
// hook that points at this binary goes through.
//
// It is the one command in bd-auto that must not refuse anything. A hook runs
// before every tool call and at every turn end, and Claude Code reads a
// non-zero exit as "block". A hook that exits non-zero on input it does not
// recognise does not fail the hook, it fails the session: every Bash call
// refused before it runs, every attempt to end the turn refused and the model
// immediately re-invoked. That state is unrecoverable from inside the session,
// because the fix needs the shell the hook is blocking.
//
// So: any event name, known or unknown, exits 0 and prints nothing on stdout.
// An unknown event is version skew between a shipped hooks config and a shipped
// binary — the ordinary consequence of upgrading one and not the other — and
// the right answer to skew is to do nothing, quietly. This is the opposite of
// the rule everywhere else in bd-auto, where an unknown subcommand is a typo
// worth refusing; on this path the two mistakes do not cost the same.
//
// The only non-zero exit is a deliberate block, from a handler that decided to
// take one. See hookBlock.
func Hook(args []string) (err error) {
	// A Go panic exits 2, which is the exact code Claude Code reads as "block".
	// Nowhere else in bd-auto is a nil map deref worth catching; here it would
	// cost the operator their session, so it is.
	defer failOpenOnPanic(&err)

	in := readHookPayload(hookStdin())

	// The command line is what the hooks config says the event is, so it wins;
	// the payload names the event too, and answering that is what lets a config
	// that forgot the argument still work.
	name := in.Event
	if len(args) > 0 {
		name = args[0]
	}
	event := normalizeHookEvent(name)
	if event == "" {
		fmt.Fprint(os.Stderr, hookUsage)
		return nil
	}

	handler, ok := hookHandlers[event]
	if !ok {
		return nil // version skew, or an event we simply have nothing to say about
	}

	// Claude Code sets stop_hook_active when the model is running *because* a
	// Stop hook blocked the last turn end. It is there precisely so a hook
	// cannot trap a session, and honouring it is not optional: a Stop hook that
	// blocks unconditionally blocks forever, since each block re-invokes the
	// model, which tries to stop, and is blocked again.
	if in.StopHookActive && isStopEvent(event) {
		return nil
	}

	if err := handler(in); err != nil {
		// A requested exit code is a decision the handler made and already
		// explained on stderr. Anything else is a handler that broke, and a
		// broken handler is not a reason to block a tool call.
		if _, deliberate := ExitCode(err); deliberate {
			return err
		}
		fmt.Fprintf(os.Stderr, "bd-auto hook %s: %v\n", name, err)
	}
	return nil
}

// hookHandler is what bd-auto does about one hook event. Returning an error
// reports it on stderr and exits 0; only hookBlock exits non-zero.
type hookHandler func(hookPayload) error

// hookHandlers maps a normalised event name to its handler.
//
// It is empty, and that is the current answer to "which hooks should bd-auto
// register": none. The plugin manifest declares no hooks, so nothing points
// here yet. The map exists anyway because the guarantees around it — exit 0 on
// an unknown event, honour stop_hook_active, swallow a handler's error — have
// to be in place before the first handler is, not after. Add a handler here and
// it inherits all of them.
//
// Think hard before registering PreToolUse in particular. It costs a process
// spawn on every tool call a session makes, and it is the blast radius of
// exactly the bug this file exists to prevent.
var hookHandlers = map[string]hookHandler{}

// hookBlock is the only non-zero exit a hook may take. Exit 2 is what Claude
// Code reads as "block this tool call" or "do not end this turn", and it has to
// be a decision rather than a side effect of something going wrong: a handler
// that blocks writes its reason to stderr first — that text is what the model
// is shown — and returns this.
func hookBlock() error { return errSilentExit{code: 2} }

// hookPayload is the part of Claude Code's hook stdin JSON that bd-auto reads.
// Everything else stays in Raw on purpose: the fields differ per event and move
// between CLI versions, and a hook that insists on a shape it recognises is a
// hook that blocks the session on the day a field is added.
type hookPayload struct {
	Event          string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`

	Raw []byte `json:"-"` // the payload as it arrived, for a handler that needs more
}

// readHookPayload reads a hook payload, and cannot fail. Absent, empty,
// truncated and malformed input all produce the zero payload, which reads as
// "no event named, nothing active" and leads Hook to do nothing.
func readHookPayload(r io.Reader) hookPayload {
	if r == nil {
		return hookPayload{}
	}
	raw, err := io.ReadAll(io.LimitReader(r, hookStdinLimit))
	if err != nil {
		return hookPayload{}
	}
	p := hookPayload{Raw: raw}
	_ = json.Unmarshal(raw, &p)
	return p
}

// hookStdin is stdin, unless there is a human at it. Claude Code always pipes
// the payload in; a hook typed at a terminal has nothing to read and must not
// sit there waiting for an EOF that is not coming.
func hookStdin() io.Reader {
	if charDevice(os.Stdin) {
		return nil
	}
	return os.Stdin
}

// isStopEvent reports whether an event is one where the model is asking to end
// a turn, which is the pair stop_hook_active applies to.
func isStopEvent(event string) bool {
	return event == "stop" || event == "subagentstop"
}

// normalizeHookEvent folds an event name to one spelling. Claude Code writes
// PascalCase in the payload ("PreToolUse") while hooks configs are commonly
// written in kebab-case on the command line ("pre-tool-use"); both name the
// same event, and a hook that answers only one spelling silently does nothing
// for half its callers.
func normalizeHookEvent(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		}
		return -1
	}, s)
}

// failOpenOnPanic turns a panic below it into a stderr report and a zero exit.
func failOpenOnPanic(err *error) {
	r := recover()
	if r == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "bd-auto hook panicked: %v\nDoing nothing rather than blocking the session.\n%s",
		r, debug.Stack())
	*err = nil
}
