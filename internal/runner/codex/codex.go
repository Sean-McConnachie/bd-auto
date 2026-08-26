// Package codex runs OpenAI's Codex CLI non-interactively as a bd-auto runner.
//
// This file owns invocation and process lifecycle. JSONL event semantics live
// separately so CLI construction can be tested without coupling it to parser
// changes.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bd-auto/internal/runner"
)

const (
	Provider         = "codex"
	DefaultBin       = "codex"
	BinEnv           = "BD_AUTO_CODEX_BIN"
	DefaultKillGrace = 5 * time.Second
	stderrCap        = 32 << 10
)

func init() {
	runner.Register(Provider, func(spec runner.Spec) (runner.Runner, error) { return New(spec) })
}

// Runner invokes the Codex CLI with one resolved role specification.
type Runner struct {
	Spec      runner.Spec
	Bin       string
	KillGrace time.Duration
	// BillingTimeout bounds the local `codex login status` safety check.
	// Zero uses DefaultBillingTimeout.
	BillingTimeout time.Duration

	billingOnce   sync.Once
	billingSource runner.BillingSource
	billingErr    error
}

// New builds a Codex runner. Provider-specific values remain on Spec because
// they are fixed for the role, not varied by an individual request.
func New(spec runner.Spec) (*Runner, error) { return &Runner{Spec: spec}, nil }

func (r *Runner) Name() string { return Provider }

// Caps deliberately leaves Permissions empty: Codex has native sandbox and
// approval controls and does not implement Claude's permission vocabulary.
func (r *Runner) Caps() runner.Capabilities {
	return runner.Capabilities{Resume: true, Stream: true, ReportsUsage: true, Tools: true}
}

func (r *Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	if bin := os.Getenv(BinEnv); bin != "" {
		return bin
	}
	return DefaultBin
}

func (r *Runner) grace() time.Duration {
	if r.KillGrace > 0 {
		return r.KillGrace
	}
	return DefaultKillGrace
}

// args builds a deterministic argv. The prompt sentinel is last and the
// prompt itself is written to stdin by Run, so arbitrary prompt text never
// becomes process metadata.
func (r *Runner) args(req runner.Request) ([]string, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("codex: request has no prompt")
	}
	if req.Resume && strings.TrimSpace(req.SessionID) == "" {
		return nil, errors.New("codex: resume requested with no session id")
	}

	model := req.Model
	if model == "" {
		model = r.Spec.Model
	}

	args := []string{"exec"}
	if req.Resume {
		args = append(args, "resume", req.SessionID)
	}
	args = append(args, "--json", "--strict-config")
	if model != "" {
		args = append(args, "--model", model)
	}
	if r.Spec.Sandbox != "" {
		if req.Resume {
			// exec resume does not expose --sandbox, but it accepts config
			// overrides after the session id.
			args = addConfig(args, "sandbox_mode", tomlString(r.Spec.Sandbox))
		} else {
			args = append(args, "--sandbox", r.Spec.Sandbox)
		}
	}
	if req.SystemPrompt != "" {
		args = addConfig(args, "developer_instructions", tomlString(req.SystemPrompt))
	}
	if r.Spec.ApprovalPolicy != "" {
		args = addConfig(args, "approval_policy", tomlString(r.Spec.ApprovalPolicy))
	}
	args = addConfig(args, "features.shell_tool", strconv.FormatBool(r.Spec.Shell))
	args = addConfig(args, "tools.web_search", strconv.FormatBool(r.Spec.WebSearch))
	args = addConfig(args, "features.view_image", strconv.FormatBool(r.Spec.ViewImage))

	if len(req.ToolServers) > 0 {
		cfg, err := mcpServersTOML(req.ToolServers)
		if err != nil {
			return nil, err
		}
		args = addConfig(args, "mcp_servers", cfg)
	}

	args = append(args, req.ExtraArgs...)
	return append(args, "-"), nil
}

func addConfig(args []string, key, value string) []string {
	return append(args, "-c", key+"="+value)
}

// tomlString starts with JSON's string encoding, whose quoted form and escapes
// are also valid TOML basic strings. JSON may leave DEL unescaped while TOML
// forbids it, so that one remaining control byte is escaped explicitly.
func tomlString(value string) string {
	raw, _ := json.Marshal(value)
	return strings.ReplaceAll(string(raw), "\x7f", `\u007f`)
}

func tomlArray(values []string) string {
	encoded := make([]string, len(values))
	for i, value := range values {
		encoded[i] = tomlString(value)
	}
	return "[" + strings.Join(encoded, ",") + "]"
}

// mcpServersTOML encodes the entire dynamic table in one override. Quoted TOML
// keys keep server and environment names from becoming dotted config paths.
func mcpServersTOML(servers []runner.ToolServer) (string, error) {
	byName := make(map[string]runner.ToolServer, len(servers))
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		if strings.TrimSpace(server.Name) == "" || strings.TrimSpace(server.Command) == "" {
			return "", errors.New("codex: a tool server needs a name and a command")
		}
		if strings.ContainsRune(server.Name, 0) || strings.ContainsRune(server.Command, 0) {
			return "", fmt.Errorf("codex: tool server %q contains a NUL byte", server.Name)
		}
		if _, exists := byName[server.Name]; exists {
			return "", fmt.Errorf("codex: duplicate tool server name %q", server.Name)
		}
		if server.Timeout < 0 {
			return "", fmt.Errorf("codex: tool server %q has a negative timeout", server.Name)
		}
		byName[server.Name] = server
		names = append(names, server.Name)
	}
	sort.Strings(names)

	entries := make([]string, 0, len(names))
	for _, name := range names {
		server := byName[name]
		fields := []string{"command=" + tomlString(server.Command)}
		if len(server.Args) > 0 {
			for _, arg := range server.Args {
				if strings.ContainsRune(arg, 0) {
					return "", fmt.Errorf("codex: tool server %q has a NUL byte in its arguments", name)
				}
			}
			fields = append(fields, "args="+tomlArray(server.Args))
		}
		if len(server.Env) > 0 {
			env, err := tomlEnvironment(name, server.Env)
			if err != nil {
				return "", err
			}
			fields = append(fields, "env="+env)
		}
		if len(server.Tools) > 0 {
			fields = append(fields, "enabled_tools="+tomlArray(server.Tools))
		}
		fields = append(fields, "required="+strconv.FormatBool(server.Required))
		if server.Timeout > 0 {
			seconds := server.Timeout / time.Second
			if server.Timeout%time.Second != 0 {
				seconds++
			}
			fields = append(fields, "tool_timeout_sec="+strconv.FormatInt(int64(seconds), 10))
		}
		// Engine-provided tools must not stop to ask an absent terminal user.
		fields = append(fields, `default_tools_approval_mode="approve"`)
		entries = append(entries, tomlString(name)+"={"+strings.Join(fields, ",")+"}")
	}
	return "{" + strings.Join(entries, ",") + "}", nil
}

func tomlEnvironment(server string, values []string) (string, error) {
	env := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return "", fmt.Errorf("codex: tool server %q has invalid environment entry %q", server, entry)
		}
		if _, exists := env[key]; exists {
			return "", fmt.Errorf("codex: tool server %q repeats environment key %q", server, key)
		}
		env[key] = value
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, tomlString(key)+"="+tomlString(env[key]))
	}
	return "{" + strings.Join(fields, ",") + "}", nil
}

// Run starts one isolated Codex process and drains its JSONL stream. Detailed
// event and result parsing is layered on this lifecycle separately; thread IDs
// are captured here because a fresh Codex session, unlike Claude, owns its ID.
func (r *Runner) Run(ctx context.Context, req runner.Request, sink runner.EventSink) (runner.Result, error) {
	args, err := r.args(req)
	if err != nil {
		return runner.Result{}, err
	}
	if strings.TrimSpace(req.Dir) == "" {
		return runner.Result{}, errors.New("codex: request has no working directory")
	}
	info, err := os.Stat(req.Dir)
	if err != nil {
		return runner.Result{}, fmt.Errorf("codex: working directory: %w", err)
	}
	if !info.IsDir() {
		return runner.Result{}, fmt.Errorf("codex: working directory %s is not a directory", req.Dir)
	}

	var log *os.File
	logPath := req.LogPath
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return runner.Result{}, fmt.Errorf("codex: transcript directory: %w", err)
		}
		log, logPath, err = transcript(logPath)
		if err != nil {
			return runner.Result{}, fmt.Errorf("codex: transcript: %w", err)
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
	cmd.Stdin = strings.NewReader(req.Prompt)
	setProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runner.Result{}, fmt.Errorf("codex: stdout: %w", err)
	}
	stderr := &capBuffer{limit: stderrCap}
	cmd.Stderr = stderr

	started := time.Now()
	sessionID := ""
	if req.Resume {
		sessionID = req.SessionID
	}
	runner.Emit(sink, runner.Event{Kind: runner.EventStart, Role: req.Role, SessionID: sessionID, At: started})
	if err := cmd.Start(); err != nil {
		res := runner.Result{Class: classify(outcome{startErr: err, exitCode: -1}), SessionID: sessionID, ExitCode: -1,
			Err: fmt.Errorf("codex: start %s: %w", r.bin(), err), Duration: time.Since(started)}
		emitFinish(sink, req.Role, res)
		return res, nil
	}

	stop := &stopper{cmd: cmd, grace: r.grace(), done: make(chan struct{}), cancelSeen: make(chan struct{})}
	go stop.watch(runCtx)
	stream := newInvocationStream(req.Role, sessionID, sink, log)
	readErr := stream.consume(stdout)
	waitErr := cmd.Wait()
	stop.waitCancellation(runCtx)
	stop.finish()

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if stream.sessionID != "" {
		sessionID = stream.sessionID
	}

	ctxErr := ctx.Err()
	timedOut := req.Timeout > 0 && runCtx.Err() != nil && ctxErr == nil
	out := outcome{
		ctxErr: ctxErr, timedOut: timedOut, exitCode: exitCode,
		terminalComplete: stream.terminalComplete, terminalFailed: stream.terminalFailed,
		worked: stream.worked, completedAt: stream.completedAt, cancelledAt: stop.cancelled(),
		missingSession: !req.Resume && sessionID == "", structuredFailure: stream.failures,
		stderr: stderr.String(),
	}
	class := classify(out)
	if class == runner.ClassOK {
		ctxErr, timedOut = nil, false
		out.ctxErr, out.timedOut = nil, false
	}
	res := runner.Result{Class: class, Text: stream.text, SessionID: sessionID, ExitCode: exitCode, Usage: stream.usage,
		Denials: stream.denials, Duration: time.Since(started), TimedOut: timedOut}
	if log != nil {
		res.LogPath = logPath
	}
	if class != runner.ClassOK {
		res.Err = outcomeError(out, waitErr, readErr, stream.diagnostic())
	}
	if class == runner.ClassInfraFailed {
		res.ResetAt = resetFromFailures(time.Now(), stream.failures, stderr.String())
	}
	emitFinish(sink, req.Role, res)
	return res, nil
}

func emitFinish(sink runner.EventSink, role runner.Role, res runner.Result) {
	if res.Err != nil {
		runner.Emit(sink, runner.Event{Kind: runner.EventError, Role: role, SessionID: res.SessionID, Text: res.Err.Error(), At: time.Now()})
	}
	runner.Emit(sink, runner.Event{Kind: runner.EventDone, Role: role, SessionID: res.SessionID, Usage: res.Usage, At: time.Now()})
}

func transcript(path string) (*os.File, string, error) {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for n := 1; n <= 100; n++ {
		candidate := path
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d%s", stem, n, ext)
		}
		file, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return file, candidate, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("no free transcript name beside %s", path)
}

type stopper struct {
	cmd        *exec.Cmd
	grace      time.Duration
	mu         sync.Mutex
	over       bool
	cancelAt   time.Time
	done       chan struct{}
	cancelSeen chan struct{}
}

func (s *stopper) watch(ctx context.Context) {
	select {
	case <-s.done:
		return
	case <-ctx.Done():
	}
	s.markCancelled(time.Now())
	close(s.cancelSeen)
	if !s.signal(terminateProcess) {
		return
	}
	select {
	case <-s.done:
	case <-time.After(s.grace):
		s.signal(killProcess)
	}
}

func (s *stopper) waitCancellation(ctx context.Context) {
	if ctx.Err() != nil {
		<-s.cancelSeen
	}
}

func (s *stopper) cancelled() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelAt
}

func (s *stopper) markCancelled(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelAt.IsZero() {
		s.cancelAt = at
	}
}

func (s *stopper) signal(send func(*exec.Cmd) error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.over {
		return false
	}
	_ = send(s.cmd)
	return true
}

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

type capBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (b *capBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, value...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(value), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
