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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	// ToolTimeoutEnv overrides how long one call to an engine tool is allowed,
	// as a Go duration. See toolTimeout.
	ToolTimeoutEnv = "BD_AUTO_CLAUDE_MCP_TOOL_TIMEOUT"
	// DefaultKillGrace is how long a cancelled process group has between the
	// SIGTERM that asks and the SIGKILL that does not.
	DefaultKillGrace = 5 * time.Second
	// stderrCap bounds the stderr kept in memory. The tail is kept rather than
	// the head because that is where a CLI puts the thing that killed it.
	stderrCap = 32 << 10
)

// How long one call to an engine tool is allowed.
//
// This is the adapter's number and not the engine's, because the ceiling is a
// property of this backend: the CLI stores a per-server timeout as a signed
// 32-bit millisecond count and clamps anything above it, so MaxToolTimeout is
// what it will accept and not a preference.
//
// Two limits have to be cleared, and only one of them is documented. The
// per-server timeout is the wall-clock limit on a call. The other is an idle
// timeout — thirty minutes for a stdio server — and a call that waits without
// sending anything is idle for the whole of it. Setting the per-server timeout
// raises both together, which is why the adapter sets it rather than leaving
// the defaults alone: a question that waits an hour would otherwise be killed
// at thirty minutes with the documented limit still hours away.
//
// So the default asks for the ceiling less a margin. Nothing is lost by asking
// high: what actually ends one of these calls is the engine's own hold, which
// returns a ticket in minutes, and a run's cancellation kills the process group
// under all of it. What is lost by asking low is an answer a human was part-way
// through typing.
const (
	// MaxToolTimeout is the largest value the CLI will store.
	MaxToolTimeout = 2147483647 * time.Millisecond
	// ToolTimeoutMargin keeps the default clear of that boundary.
	ToolTimeoutMargin = time.Hour
	// MinToolTimeout is the smallest value the CLI honours; below it the key is
	// ignored and the default applies instead, which is the one outcome worth
	// refusing outright.
	MinToolTimeout = time.Second
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
	// ToolTimeout is how long one call to an engine tool is allowed. Zero means
	// ToolTimeoutEnv, then the ceiling this backend accepts less
	// ToolTimeoutMargin. See the constants above for why the default is high.
	ToolTimeout time.Duration
}

// New builds a runner for a resolved spec.
func New(spec runner.Spec) (*Runner, error) {
	return &Runner{Spec: spec}, nil
}

// Name implements runner.Runner.
func (r *Runner) Name() string { return Provider }

// Caps implements runner.Runner. The CLI does all of it, which is why it is the
// backend the engine is written against.
func (r *Runner) Caps() runner.Capabilities {
	return runner.Capabilities{
		Resume:       true,
		Stream:       true,
		ReportsUsage: true,
		Tools:        true,
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

// toolTimeout is what the CLI is told one call to an engine tool may take.
//
// want is what the request asked for, and it is a floor rather than the answer:
// the engine says what it needs one call to survive, and the adapter is the
// only side that knows what this backend will accept. Where the configured
// ceiling is already higher — which the default is, deliberately — the higher
// number wins, because a per-call limit is not what bounds one of these calls.
func (r *Runner) toolTimeout(want time.Duration) time.Duration {
	d := r.ToolTimeout
	if env := os.Getenv(ToolTimeoutEnv); env != "" {
		if parsed, err := time.ParseDuration(env); err == nil && parsed > 0 {
			d = parsed
		}
	}
	if d <= 0 {
		d = MaxToolTimeout - ToolTimeoutMargin
	}
	if want > d {
		d = want
	}
	if d > MaxToolTimeout {
		d = MaxToolTimeout
	}
	if d < MinToolTimeout {
		d = MinToolTimeout
	}
	return d
}

// args builds the argv for one request.
//
//	claude -p <task>
//	  --output-format stream-json --verbose --include-partial-messages
//	  --model <model>
//	  --append-system-prompt <role prompt>
//	  --permission-mode auto | manual (scoped) | bypassPermissions
//	  --session-id <uuid>   | --resume <uuid>
//	  --mcp-config <json>   (with tool servers)
//	  --allowed-tools ...   (scoped, or to permit the engine's own tools)
//	  --disallowed-tools ... (at every level; deny beats everything)
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
		// Named, not left to the default. The CLI's default mode is auto, whose
		// classifier judges each call on what it looks like — and it judged a
		// reviewer's `bd close` on the issue under review to be fine, which is
		// how a review came to close the work it was judging. manual asks
		// instead, and headless there is nobody to ask, so anything the
		// allowlist does not name is refused. That is what scoped means.
		//
		// A scoped run with an empty list could not do anything at all, so it
		// is a config mistake rather than a very safe reviewer.
		if len(req.AllowedTools) == 0 {
			return nil, fmt.Errorf("claude: scoped permissions with no allowed tools")
		}
		args = append(args, "--permission-mode", "manual")
	}

	switch {
	case req.Resume:
		args = append(args, "--resume", req.SessionID)
	case req.SessionID != "":
		// The caller generates the id, so a later round resumes without
		// parsing any output to discover what the session was called.
		args = append(args, "--session-id", req.SessionID)
	}

	if len(req.ToolServers) > 0 {
		cfg, err := r.mcpConfig(req.ToolServers)
		if err != nil {
			return nil, err
		}
		args = append(args, "--mcp-config", cfg)
	}

	// The allowlist carries two different things. Under scoped permissions it
	// is the whole of what the run may do. Under any level it also names the
	// engine's own tools, because an MCP tool the CLI has not been told about
	// is one it stops to ask about — and headless there is nobody to ask, so a
	// tool that is not named here is a tool that is refused.
	var allow []string
	if perms == runner.PermScoped {
		allow = append(allow, req.AllowedTools...)
	}
	allow = append(allow, qualifiedTools(req.ToolServers)...)
	if len(allow) > 0 {
		args = append(args, "--allowed-tools", strings.Join(allow, ","))
	}

	// Whatever the level. The CLI checks deny rules before it checks anything
	// else, including bypassPermissions, so this is the one part of a role's
	// scoping that --dangerously-skip-permissions does not switch off.
	if len(req.DeniedTools) > 0 {
		args = append(args, "--disallowed-tools", strings.Join(req.DeniedTools, ","))
	}

	return append(args, req.ExtraArgs...), nil
}

// mcpServerEntry is one server as the CLI's --mcp-config expects it.
type mcpServerEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Timeout is the CLI's per-server tool-call timeout, in milliseconds. See
	// toolTimeout for what decides it and why it is always set.
	Timeout int64 `json:"timeout,omitempty"`
}

// mcpConfig renders the tool servers as the JSON --mcp-config takes.
//
// Inline JSON rather than a temp file: a file would have to outlive the process
// that wrote it and be cleaned up after a kill, and there is nothing here that
// is secret enough to be worth keeping off an argv the run's own transcript
// records anyway.
func (r *Runner) mcpConfig(servers []runner.ToolServer) (string, error) {
	out := map[string]mcpServerEntry{}
	for _, s := range servers {
		if s.Name == "" || s.Command == "" {
			return "", fmt.Errorf("claude: a tool server needs a name and a command")
		}
		entry := mcpServerEntry{Type: "stdio", Command: s.Command, Args: s.Args}
		if len(s.Env) > 0 {
			entry.Env = map[string]string{}
			for _, kv := range s.Env {
				if k, v, ok := strings.Cut(kv, "="); ok {
					entry.Env[k] = v
				}
			}
		}
		entry.Timeout = r.toolTimeout(s.Timeout).Milliseconds()
		out[s.Name] = entry
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": out})
	if err != nil {
		return "", fmt.Errorf("claude: mcp config: %w", err)
	}
	return string(raw), nil
}

// qualifiedTools names the engine's tools the way the CLI's allowlist does:
// mcp__<server>__<tool>, sorted, so the argv is stable.
func qualifiedTools(servers []runner.ToolServer) []string {
	var out []string
	for _, s := range servers {
		for _, t := range s.Tools {
			out = append(out, "mcp__"+s.Name+"__"+t)
		}
	}
	sort.Strings(out)
	return out
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
	logPath := req.LogPath
	if req.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(req.LogPath), 0o755); err != nil {
			return runner.Result{}, fmt.Errorf("claude: transcript directory: %w", err)
		}
		log, logPath, err = transcript(req.LogPath)
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
		ctxErr:         ctxErr,
		timedOut:       timedOut,
		exitCode:       exitCode,
		sawResult:      p.sawResult,
		resultErr:      p.resultErr,
		failText:       p.failText(),
		stderr:         stderr.String(),
		apiStatus:      p.apiErrorStatus,
		terminalReason: p.terminalReason,
	}
	class := classify(out)

	res := runner.Result{
		Class:     class,
		Text:      p.text(),
		SessionID: p.sessionID,
		ExitCode:  exitCode,
		Denials:   p.denied,
		Usage:     p.usage,
		Duration:  time.Since(started),
		TimedOut:  timedOut,
	}
	if log != nil {
		res.LogPath = logPath
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
	case apiOutage(o.apiStatus, o.terminalReason) && detail == "":
		return fmt.Errorf("claude: the API call failed%s", apiStatusSuffix(o.apiStatus))
	case apiOutage(o.apiStatus, o.terminalReason):
		// Says what it failed on, because "exit 1" beside a limit message reads
		// as the model failing rather than the request never reaching one.
		return fmt.Errorf("claude: the API call failed%s: %s", apiStatusSuffix(o.apiStatus), detail)
	case detail == "":
		return fmt.Errorf("claude: exit %d", o.exitCode)
	}
	return fmt.Errorf("claude: exit %d: %s", o.exitCode, detail)
}

// apiStatusSuffix names the HTTP status where the CLI reported one.
func apiStatusSuffix(status int) string {
	if status == 0 {
		return ""
	}
	return fmt.Sprintf(" (HTTP %d)", status)
}

// emitFinish emits the last two events of a run, so the TUI's per-worker line
// ends on the same events whatever happened.
func emitFinish(p *parser, res runner.Result) {
	if res.Err != nil {
		p.emit(runner.Event{Kind: runner.EventError, Text: res.Err.Error()})
	}
	p.emit(runner.Event{Kind: runner.EventDone, Usage: res.Usage})
}

// transcript creates the file a process's raw stream is written to, and returns
// the name it actually got.
//
// It never overwrites. The engine names a transcript after the issue, attempt,
// round and role that produced it, and that is unique right up until something
// resets the attempt counter: `run unpark` does exactly that, deliberately, so
// a killed worker's retry asks for the same name its own corpse is written to.
// Truncating there destroys the only record of what the killed process did,
// which is precisely what somebody investigating the kill came to read. So a
// name already taken takes the next free -N beside it, and the caller reports
// back the name it got rather than the one it asked for.
func transcript(path string) (*os.File, string, error) {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for n := 1; ; n++ {
		try := path
		if n > 1 {
			try = fmt.Sprintf("%s-%d%s", stem, n, ext)
		}
		f, err := os.OpenFile(try, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, try, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
		// A directory that has somehow filled up with them is not worth
		// spinning on; the transcript is observability, not the run.
		if n >= 100 {
			f, err := os.Create(path)
			return f, path, err
		}
	}
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
