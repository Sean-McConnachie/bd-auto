package codex

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"bd-auto/internal/runner"
)

const (
	DefaultVersionTimeout   = 30 * time.Second
	DefaultPreflightTimeout = 3 * time.Minute
)

var _ runner.Preflighter = (*Runner)(nil)

const (
	preflightRole         = runner.Role("preflight")
	preflightSystemPrompt = "You are a preflight check. Answer in one word and use no tools."
	preflightPrompt       = "Reply with the single word: ok"
)

// Preflight identifies the installed CLI and authentication source, then runs
// one minimal request through the same argv builder and JSONL parser as a real
// role. The engine's separate billing gate runs first and is the authority for
// API-key consent; this method only reports the credential source, never its
// value.
func (r *Runner) Preflight(ctx context.Context, dir string) (string, error) {
	version, err := r.version(ctx, dir)
	if err != nil {
		return "", err
	}
	source, err := r.BillingSource(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("codex %s: authentication: %w", version, err)
	}
	if err := r.probe(ctx, dir, version); err != nil {
		return "", err
	}
	desc := "codex " + version
	if r.Spec.Model != "" {
		desc += ", model " + r.Spec.Model
	}
	desc += ", billing " + string(source)
	return desc, nil
}

func (r *Runner) version(ctx context.Context, dir string) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, DefaultVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, r.bin(), "--version")
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("codex: %s cannot be run: %w; install the CLI, or set %s to the one to use", r.bin(), err, BinEnv)
		}
		return "", fmt.Errorf("codex: %s --version: %w%s", r.bin(), err, codexDetail(out))
	}
	version := firstOutputLine(out)
	if version == "" {
		return "", fmt.Errorf("codex: %s --version printed nothing, so it is not the Codex CLI", r.bin())
	}
	return version, nil
}

func (r *Runner) probe(ctx context.Context, dir, version string) error {
	req := r.Spec.Request(preflightRole)
	req.SystemPrompt = preflightSystemPrompt
	req.Prompt = preflightPrompt
	req.Dir = dir
	req.Timeout = r.preflightTimeout()
	res, err := r.Run(ctx, req, runner.Discard)
	if err != nil {
		return fmt.Errorf("codex %s: %w", version, err)
	}
	if res.Class != runner.ClassOK {
		return fmt.Errorf("codex %s: a one-word test run came back %s: %w", version, res.Class, res.Err)
	}
	return nil
}

func (r *Runner) preflightTimeout() time.Duration {
	timeout := DefaultPreflightTimeout
	if r.Spec.Timeout > 0 && r.Spec.Timeout < timeout {
		timeout = r.Spec.Timeout
	}
	return timeout
}

func codexDetail(out []byte) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return ""
	}
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	return ": " + text
}

func firstOutputLine(out []byte) string {
	text := strings.TrimSpace(string(out))
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text)
}
