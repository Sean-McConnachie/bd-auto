package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"bd-auto/internal/runner"
)

// The preflight exists because this adapter's argv is written against one
// CLI's flag list, and nothing above it can tell whether the installed CLI
// still has that list.
//
// Without it the discovery is made per spawn, by every worker in the wave, and
// it is made in the most expensive form there is: a CLI that rejects a flag
// exits before it prints a result line, so the run classifies it as
// infra-failed, backs off, and retries a command that cannot work — five
// worktrees deep, with a branch and a claimed issue behind each one. Checking
// once, first, turns all of that into a single error while nothing has been
// created.
const (
	// DefaultVersionTimeout bounds `claude --version`. It prints a string and
	// exits, so anything past this is a CLI that is not going to answer.
	DefaultVersionTimeout = 30 * time.Second
	// DefaultPreflightTimeout bounds the probe run. It is deliberately short
	// relative to a worker's: the probe asks for one word, and a probe that
	// hangs is the failure it was added to catch arriving in its worst form.
	DefaultPreflightTimeout = 3 * time.Minute
)

// The adapter is only preflighted if it still satisfies the optional
// interface, and a signature that drifts out of it fails silently — the engine
// would simply stop checking. So it is asserted here.
var _ runner.Preflighter = (*Runner)(nil)

// preflightRole is the role the probe is attributed to. Nothing dispatches it;
// it exists so the request the probe builds is a whole request, and so a
// transcript or a log line that somehow carries one says what it was.
const preflightRole = runner.Role("preflight")

// The probe's prompt and role prompt. They ask for the cheapest possible turn:
// what is being tested is whether the CLI accepts the argv and the account can
// answer at all, and every token past that is spent proving something the run
// itself will prove.
const (
	probeSystemPrompt = "You are a preflight check. Answer in one word and use no tools."
	probePrompt       = "Reply with the single word: ok"
)

// Preflight implements runner.Preflighter.
//
// Two checks, in the order that makes the failure legible. `--version` first,
// because a CLI that is missing, unrunnable or not the CLI at all should not be
// reported as a model failure. Then one trivial `-p` run built by the same args
// as every real invocation, because that — not the presence of the binary — is
// what says the installed version still accepts what this adapter builds.
//
// The one flag it cannot reach is --resume, which needs a session that exists
// and so would cost a second process. That is the flag whose loss the engine
// survives anyway: a resumed round that fails falls back to a fresh dispatch
// carrying its feedback, so the run degrades where a rejected --session-id or
// --permission-mode stops it dead.
func (r *Runner) Preflight(ctx context.Context, dir string) (string, error) {
	version, err := r.version(ctx, dir)
	if err != nil {
		return "", err
	}
	if err := r.probe(ctx, dir, version); err != nil {
		return "", err
	}
	desc := "claude " + version
	if r.Spec.Model != "" {
		desc += ", model " + r.Spec.Model
	}
	return desc, nil
}

// version runs `claude --version` and returns what it printed.
//
// A missing binary is named with the two ways to fix it, because it is the
// likeliest failure here and neither PATH nor BinEnv is guessable from
// "executable file not found".
func (r *Runner) version(ctx context.Context, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultVersionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.bin(), "--version")
	cmd.Dir = dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("claude: %s cannot be run: %w; install the CLI, or set %s to the one to use",
				r.bin(), err, BinEnv)
		}
		return "", fmt.Errorf("claude: %s --version: %w%s", r.bin(), err, detail(out))
	}
	v := firstLine(out)
	if v == "" {
		return "", fmt.Errorf("claude: %s --version printed nothing, so it is not the claude CLI", r.bin())
	}
	return v, nil
}

// probe spends one trivial run on the argv this adapter builds.
//
// It goes through Run rather than around it, so the thing being checked is the
// whole path a worker takes: the argv, the process, the stream, and the
// classification of what came back. A probe that rebuilt any of that would
// pass in exactly the case worth failing.
func (r *Runner) probe(ctx context.Context, dir, version string) error {
	req := r.Spec.Request(preflightRole)
	req.SystemPrompt = probeSystemPrompt
	req.Prompt = probePrompt
	req.Dir = dir
	// Minted here, and fresh, for the same reason the engine mints one: it is
	// what puts --session-id in the argv, which is one of the flags being
	// checked.
	req.SessionID = newSessionID()
	req.Timeout = r.preflightTimeout()

	res, err := r.Run(ctx, req, runner.Discard)
	if err != nil {
		// Run refuses a request it cannot build at all, which here means the
		// role's own configuration is unusable — scoped permissions with an
		// empty allowlist, most likely. It would have failed identically on
		// every spawn of the run.
		return fmt.Errorf("claude %s: %w", version, err)
	}
	if res.Class != runner.ClassOK {
		return fmt.Errorf("claude %s: a one-word test run came back %s: %w", version, res.Class, res.Err)
	}
	return nil
}

// preflightTimeout bounds the probe. A role configured with a shorter timeout
// than the default keeps it: the probe must not outlive the invocation it
// stands in for.
func (r *Runner) preflightTimeout() time.Duration {
	d := DefaultPreflightTimeout
	if r.Spec.Timeout > 0 && r.Spec.Timeout < d {
		d = r.Spec.Timeout
	}
	return d
}

// detail renders captured output for an error message, or nothing when there
// was none.
func detail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return ": " + s
}

// firstLine is what `--version` is read as: the version string is the first
// line, and a CLI that prints a banner after it is still that version.
func firstLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// newSessionID returns a random UUIDv4, which is the only shape the CLI accepts
// for --session-id. The engine mints its own for the runs it will resume; this
// one is for the probe, which nothing resumes.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1e12)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
