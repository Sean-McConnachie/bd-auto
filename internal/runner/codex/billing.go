package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"bd-auto/internal/runner"
)

const DefaultBillingTimeout = 3 * time.Second

// BillingSource establishes which account Codex will charge without making a
// model or API request. An environment override wins over saved credentials
// because it wins for the invocation that follows too.
func (r *Runner) BillingSource(ctx context.Context, dir string) (runner.BillingSource, error) {
	if strings.TrimSpace(os.Getenv("CODEX_API_KEY")) != "" {
		return runner.BillingAPIKey, nil
	}

	timeout := r.BillingTimeout
	if timeout <= 0 {
		timeout = DefaultBillingTimeout
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(r.bin(), "login", "status")
	if dir != "" {
		cmd.Dir = dir
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return runner.BillingUnknown, fmt.Errorf("codex login status failed to start: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-checkCtx.Done():
		_ = killProcess(cmd)
		<-done
		err = checkCtx.Err()
	}
	out := output.Bytes()
	if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		return runner.BillingUnknown, fmt.Errorf("codex login status timed out after %s", timeout)
	}
	if source := parseBillingStatus(string(out)); source != runner.BillingUnknown {
		return source, nil
	}
	status := strings.TrimSpace(string(out))
	if err != nil {
		if status != "" {
			return runner.BillingUnknown, fmt.Errorf("codex login status failed: %s", status)
		}
		return runner.BillingUnknown, fmt.Errorf("codex login status failed: %w", err)
	}
	if status == "" {
		return runner.BillingUnknown, errors.New("codex login status returned no authentication source")
	}
	return runner.BillingUnknown, fmt.Errorf("codex login status was not recognized: %s", status)
}

func parseBillingStatus(status string) runner.BillingSource {
	s := strings.ToLower(strings.Join(strings.Fields(status), " "))
	switch {
	case strings.Contains(s, "logged in using chatgpt"),
		strings.Contains(s, "chatgpt plan"),
		strings.Contains(s, "chatgpt account"):
		return runner.BillingChatGPTPlan
	case strings.Contains(s, "logged in using an api key"),
		strings.Contains(s, "logged in using api key"),
		strings.Contains(s, "api-key authentication"):
		return runner.BillingAPIKey
	default:
		return runner.BillingUnknown
	}
}
