// Package claude runs Anthropic's claude CLI headless (`claude -p`) as
// bd-auto's shipping backend.
//
// It is the only place in bd-auto that knows what a Claude flag looks like.
// Everything above it speaks runner.Request and runner.Result, so the argv
// built here, the stream-json parsed here and the exit codes classified here
// are this package's whole surface.
package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bd-auto/internal/runner"
)

const (
	// Provider is the name this adapter registers under, and the value a
	// runners: entry names in provider:.
	Provider = "claude"
	// DefaultBin is the executable, resolved on PATH.
	DefaultBin = "claude"
	// BinEnv overrides the executable. It is how a pinned install is selected,
	// and how tests point the adapter at a script instead of a model.
	BinEnv = "BD_AUTO_CLAUDE_BIN"
	// DefaultKillGrace is how long a cancelled process group has between the
	// SIGTERM that asks and the SIGKILL that does not.
	DefaultKillGrace = 5 * time.Second
	// stderrCap bounds the stderr kept in memory. The tail is kept rather than
	// the head because that is where a CLI puts the thing that killed it.
	stderrCap = 32 << 10
)

func init() {
	runner.Register(Provider, func(spec runner.Spec) (runner.Runner, error) { return New(spec) })
}

// Runner is the claude CLI adapter.
type Runner struct {
	// Spec is the resolved per-role configuration this runner was built for.
	// Request fields win over it; the spec is only what config fixed.
	Spec runner.Spec
	// Bin is the executable. Empty means BinEnv, then DefaultBin.
	Bin string
	// KillGrace overrides DefaultKillGrace.
	KillGrace time.Duration
}

// New builds a runner for a resolved spec.
func New(spec runner.Spec) (*Runner, error) {
	return &Runner{Spec: spec}, nil
}

// Name implements runner.Runner.
func (r *Runner) Name() string { return Provider }

// Caps implements runner.Runner. The CLI does all four, which is why it is the
// backend the engine is written against.
func (r *Runner) Caps() runner.Capabilities {
	return runner.Capabilities{
		Resume:       true,
		Stream:       true,
		ReportsUsage: true,
		Permissions:  runner.AllPermissions(),
	}
}

func (r *Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	if env := os.Getenv(BinEnv); env != "" {
		return env
	}
	return DefaultBin
}

func (r *Runner) grace() time.Duration {
	if r.KillGrace > 0 {
		return r.KillGrace
	}
	return DefaultKillGrace
}

// args builds the argv for one request.
//
//	claude -p <task>
//	  --output-format stream-json --verbose --include-partial-messages
//	  --model <model>
//	  --append-system-prompt <role prompt>
//	  --permission-mode auto
//	  --session-id <uuid>   | --resume <uuid>
//	  --allowed-tools ...   (scoped only)
//
// Order is fixed so that the argv is a testable artefact rather than something
// only observable by running a model. ExtraArgs go last so they can override.
func (r *Runner) args(req runner.Request) ([]string, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("claude: request has no prompt")
	}
	if req.Resume && req.SessionID == "" {
		return nil, fmt.Errorf("claude: resume requested with no session id")
	}
	perms := req.Permissions
	if perms == "" {
		perms = runner.PermAuto
	}
	if !perms.Valid() {
		return nil, fmt.Errorf("claude: %q is not a permission level", req.Permissions)
	}

	args := []string{
		"-p", req.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		// Without this the flag list still works; the TUI's activity line just
		// becomes tool-call-granular instead of text-granular.
		"--include-partial-messages",
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}

	switch perms {
	case runner.PermAuto:
		args = append(args, "--permission-mode", "auto")
	case runner.PermBypass:
		args = append(args, "--permission-mode", "bypassPermissions")
	case runner.PermScoped:
		// No mode flag: headless with the default mode denies anything the
		// allowlist does not name, which is exactly what scoped means. A
		// scoped run with an empty list could not do anything at all, so it is
		// a config mistake rather than a very safe reviewer.
		if len(req.AllowedTools) == 0 {
			return nil, fmt.Errorf("claude: scoped permissions with no allowed tools")
		}
	}

	switch {
	case req.Resume:
		args = append(args, "--resume", req.SessionID)
	case req.SessionID != "":
		// The caller generates the id, so a later round resumes without
		// parsing any output to discover what the session was called.
		args = append(args, "--session-id", req.SessionID)
	}

	if perms == runner.PermScoped {
		args = append(args, "--allowed-tools", strings.Join(req.AllowedTools, ","))
	}

	return append(args, req.ExtraArgs...), nil
}

// Run implements runner.Runner.
//
// It returns an error only for a request it could not run at all. Everything
// the CLI can fail at once it is running — rate limits, crashes, cancellation,
// timeouts — comes back as a Result with a Class, because that is what the
// engine routes on.
func (r *Runner) Run(ctx context.Context, req runner.Request, sink runner.EventSink) (runner.Result, error) {
	args, err := r.args(req)
	if err != nil {
		return runner.Result{}, err
	}
	if req.Dir != "" {
		fi, err := os.Stat(req.Dir)
		if err != nil {
			return runner.Result{}, fmt.Errorf("claude: working directory: %w", err)
		}
		if !fi.IsDir() {
			return runner.Result{}, fmt.Errorf("claude: working directory %s is not a directory", req.Dir)
		}
	}

	var log *os.File
	if req.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(req.LogPath), 0o755); err != nil {
			return runner.Result{}, fmt.Errorf("claude: transcript directory: %w", err)
		}
		log, err = os.Create(req.LogPath)
		if err != nil {
			return runner.Result{}, fmt.Errorf("claude: transcript: %w", err)
		}
		defer log.Close()
	}

	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	cmd := exec.Command(r.bin(), args...)
	cmd.Dir = req.Dir
	cmd.Env = os.Environ()
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runner.Result{}, fmt.Errorf("claude: stdout: %w", err)
	}
	stderr := &capBuffer{limit: stderrCap}
	cmd.Stderr = stderr

	started := time.Now()
	p := newParser(req.Role, req.SessionID, sink, logWriter(log))
	p.emit(runner.Event{Kind: runner.EventStart})

	if err := cmd.Start(); err != nil {
		// A CLI that is missing or unrunnable is an environment problem, so it
		// goes back as infra-failed and costs the issue neither a round nor an
		// attempt.
		res := runner.Result{
			Class:     classify(outcome{startErr: err, exitCode: -1}),
			SessionID: req.SessionID,
			ExitCode:  -1,
			Err:       fmt.Errorf("claude: start %s: %w", r.bin(), err),
			Duration:  time.Since(started),
		}
		emitFinish(p, res)
		return res, nil
	}

	stop := &stopper{cmd: cmd, grace: r.grace(), done: make(chan struct{})}
	go stop.watch(runCtx)

	// stdout must be drained before Wait, which is also where every event and
	// all the result state comes from.
	readErr := p.consume(stdout)
	waitErr := cmd.Wait()
	stop.finish()

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	// A run that got all the way to a clean result is a clean result, whatever
	// the clock or the caller did in the moment after. Reading a finished round
	// as interrupted would throw away work that has already been paid for and
	// re-run it.
	completed := exitCode == 0 && p.sawResult && !p.resultErr
	ctxErr := ctx.Err()
	timedOut := req.Timeout > 0 && runCtx.Err() != nil && ctxErr == nil
	if completed {
		ctxErr, timedOut = nil, false
	}

	out := outcome{
		ctxErr:    ctxErr,
		timedOut:  timedOut,
		exitCode:  exitCode,
		sawResult: p.sawResult,
		resultErr: p.resultErr,
		failText:  p.failText(),
		stderr:    stderr.String(),
	}
	class := classify(out)

	res := runner.Result{
		Class:     class,
		Text:      p.text(),
		SessionID: p.sessionID,
		ExitCode:  exitCode,
		Usage:     p.usage,
		Duration:  time.Since(started),
		TimedOut:  timedOut,
	}
	if log != nil {
		res.LogPath = req.LogPath
	}
	if class != runner.ClassOK {
		res.Err = failure(class, out, waitErr, readErr)
	}
	emitFinish(p, res)
	return res, nil
}

// failure builds the error the log and the TUI show. Class is the signal the
// engine reads; this is for the human reading afterwards, so it says what the
// CLI said rather than restating the class.
func failure(class runner.Class, o outcome, waitErr, readErr error) error {
	detail := strings.TrimSpace(o.failText)
	if detail == "" {
		detail = strings.TrimSpace(o.stderr)
	}
	if detail == "" && waitErr != nil {
		detail = waitErr.Error()
	}
	if detail == "" && readErr != nil {
		detail = readErr.Error()
	}
	if detail == "" && !o.sawResult {
		detail = "the CLI exited without a result"
	}
	if len(detail) > 2000 {
		detail = "…" + detail[len(detail)-2000:]
	}

	switch {
	case class == runner.ClassInterrupted && o.timedOut:
		return fmt.Errorf("claude: timed out: %s", detail)
	case class == runner.ClassInterrupted:
		return fmt.Errorf("claude: cancelled: %w", o.ctxErr)
	case detail == "":
		return fmt.Errorf("claude: exit %d", o.exitCode)
	}
	return fmt.Errorf("claude: exit %d: %s", o.exitCode, detail)
}

// emitFinish emits the last two events of a run, so the TUI's per-worker line
// ends on the same events whatever happened.
func emitFinish(p *parser, res runner.Result) {
	if res.Err != nil {
		p.emit(runner.Event{Kind: runner.EventError, Text: res.Err.Error()})
	}
	p.emit(runner.Event{Kind: runner.EventDone, Usage: res.Usage})
}

// logWriter returns an untyped nil for a missing file. Handing the parser a
// typed nil *os.File would leave it holding a non-nil interface and writing
// every transcript line into a nil file.
func logWriter(f *os.File) io.Writer {
	if f == nil {
		return nil
	}
	return f
}

// stopper turns a cancelled context into a dead process group.
//
// exec.CommandContext would kill the direct child only, which for a worker
// forty seconds into `go test ./...` leaves the test running and holding its
// worktree. So the group gets SIGTERM, and SIGKILL if it is still there after
// the grace period.
type stopper struct {
	cmd   *exec.Cmd
	grace time.Duration

	mu   sync.Mutex
	over bool
	done chan struct{}
}

func (s *stopper) watch(ctx context.Context) {
	select {
	case <-s.done:
		return
	case <-ctx.Done():
	}
	if !s.signal(terminateProcess) {
		return
	}
	select {
	case <-s.done:
	case <-time.After(s.grace):
		s.signal(killProcess)
	}
}

// signal sends to the group unless the process has already been reaped, which
// is what keeps a group kill from landing on a recycled pid.
func (s *stopper) signal(send func(*exec.Cmd) error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.over {
		return false
	}
	_ = send(s.cmd)
	return true
}

// finish records that the process has been waited for.
func (s *stopper) finish() {
	s.mu.Lock()
	if s.over {
		s.mu.Unlock()
		return
	}
	s.over = true
	s.mu.Unlock()
	close(s.done)
}

// capBuffer keeps the last limit bytes written to it.
type capBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
