package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"bd-auto/internal/runner"
)

// outcome contains only process and structured stream facts. Assistant prose
// is deliberately absent: a successful report about a 429 handler is not an
// account outage.
type outcome struct {
	ctxErr            error
	timedOut          bool
	startErr          error
	exitCode          int
	terminalComplete  bool
	terminalFailed    bool
	missingSession    bool
	worked            bool
	completedAt       time.Time
	cancelledAt       time.Time
	structuredFailure []errorInfo
	stderr            string
}

// classify applies the engine's ordering. A completed event can beat a
// cancellation only when it was observed first; otherwise cancellation is the
// only reliable explanation for the child's shutdown status.
func classify(o outcome) runner.Class {
	completedBeforeCancel := o.terminalComplete && !o.completedAt.IsZero() &&
		(o.cancelledAt.IsZero() || !o.completedAt.After(o.cancelledAt))
	if o.startErr != nil {
		return runner.ClassInfraFailed
	}
	if (o.ctxErr != nil || o.timedOut) && !completedBeforeCancel {
		return runner.ClassInterrupted
	}

	failed := o.exitCode != 0 || o.terminalFailed || !o.terminalComplete || o.missingSession
	if !failed || ((o.ctxErr != nil || o.timedOut) && completedBeforeCancel) {
		return runner.ClassOK
	}
	if structuredInfra(o.structuredFailure) {
		return runner.ClassInfraFailed
	}
	// A structured terminal failure is a verdict. Once Codex supplied one, a
	// stray old warning on stderr must not rewrite it as an outage.
	if o.terminalFailed && len(o.structuredFailure) > 0 {
		if o.worked {
			return runner.ClassWorkFailed
		}
		// A terminal model error before Codex emitted any model or tool work did
		// not give the issue a fair attempt. Known infrastructure categories were
		// handled above; an unknown startup-side failure is still infrastructure.
		return runner.ClassInfraFailed
	}
	if infraText(o.stderr) {
		return runner.ClassInfraFailed
	}
	if (!o.terminalComplete && !o.terminalFailed) || o.missingSession {
		return runner.ClassInfraFailed
	}
	if o.terminalComplete && o.exitCode != 0 && !o.terminalFailed {
		// The turn reached a verdict, then the CLI itself died. There is no
		// structured work failure to feed back to the model.
		return runner.ClassInfraFailed
	}
	return runner.ClassWorkFailed
}

type errorInfo struct {
	Text       string
	Codes      []string
	Statuses   []int
	ResetAt    time.Time
	Structured bool
}

func parseErrorInfo(message string, raw json.RawMessage) errorInfo {
	info := errorInfo{Text: errorText(message, raw)}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return info
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return info
	}
	info.Structured = true
	collectErrorFields(value, "", time.Now(), &info)
	return info
}

func collectErrorFields(value any, key string, now time.Time, info *errorInfo) {
	switch value := value.(type) {
	case map[string]any:
		for childKey, child := range value {
			collectErrorFields(child, strings.ToLower(childKey), now, info)
		}
	case []any:
		for _, child := range value {
			collectErrorFields(child, key, now, info)
		}
	case string:
		if codeKey(key) && strings.TrimSpace(value) != "" {
			info.Codes = append(info.Codes, strings.ToLower(strings.TrimSpace(value)))
		}
		if statusKey(key) {
			if status, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				info.Statuses = append(info.Statuses, status)
			}
		}
		if resetKey(key) && info.ResetAt.IsZero() {
			if number, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				info.ResetAt, _ = numericReset(number, key, now)
			} else {
				info.ResetAt, _ = parseAbsoluteReset(value, now)
			}
		}
	case float64:
		if statusKey(key) {
			info.Statuses = append(info.Statuses, int(value))
		}
		if resetKey(key) && info.ResetAt.IsZero() {
			info.ResetAt, _ = numericReset(value, key, now)
		}
	}
}

func codeKey(key string) bool {
	return key == "type" || key == "code" || key == "kind" || key == "category" || strings.HasSuffix(key, "_type")
}
func statusKey(key string) bool {
	return key == "status" || key == "status_code" || key == "http_status"
}
func resetKey(key string) bool {
	return strings.Contains(key, "reset") || key == "retry_after" || key == "retry_after_seconds"
}

func numericReset(value float64, key string, now time.Time) (time.Time, bool) {
	if strings.Contains(key, "after") {
		unit := time.Second
		if strings.HasSuffix(key, "_ms") || strings.Contains(key, "millisecond") {
			unit = time.Millisecond
		}
		return now.Add(time.Duration(value * float64(unit))), true
	}
	seconds := int64(value)
	if seconds > 1e12 {
		return time.UnixMilli(seconds), true
	}
	if seconds > 1e9 {
		return time.Unix(seconds, 0), true
	}
	return time.Time{}, false
}

func structuredInfra(infos []errorInfo) bool {
	for _, info := range infos {
		for _, status := range info.Statuses {
			if status == 401 || status == 403 || status == 429 || status >= 500 && status <= 599 {
				return true
			}
		}
		if infraText(append(info.Codes, info.Text)...) {
			return true
		}
	}
	return false
}

var infraPatterns = []string{
	"usage limit", "usage_limit", "session limit", "weekly limit", "plan limit", "rate limit", "rate_limit",
	"quota exceeded", "insufficient_quota", "too many requests", "http 429", "status 429",
	"internal server error", "internal_server_error", "server_error", "service_error", "service unavailable", "bad gateway", "gateway timeout", "overloaded",
	"authentication", "unauthorized", "invalid api key", "invalid_api_key", "missing api key", "login required", "not logged in", "not_logged_in", "please login",
	"econnreset", "econnrefused", "enotfound", "etimedout", "connection refused", "network error", "network_error", "fetch failed", "getaddrinfo", "tls handshake",
	"segmentation fault", "panic:", "fatal runtime error", "cannot find module",
}

func infraText(texts ...string) bool {
	for _, text := range texts {
		low := strings.ToLower(text)
		for _, pattern := range infraPatterns {
			if strings.Contains(low, pattern) {
				return true
			}
		}
		// Keep the fallback bounded to explicit HTTP status syntax. Bare numbers
		// occur routinely in build and test output.
		for _, prefix := range []string{"http ", "status ", "status_code="} {
			if at := strings.Index(low, prefix); at >= 0 {
				field := strings.Fields(low[at+len(prefix):])
				if len(field) > 0 {
					status, _ := strconv.Atoi(strings.Trim(field[0], ":,;()[]{}\""))
					if status == 429 || status >= 500 && status <= 599 {
						return true
					}
				}
			}
		}
	}
	return false
}

func denialError(raw json.RawMessage) bool {
	info := parseErrorInfo("", raw)
	for _, text := range append(info.Codes, info.Text) {
		low := strings.ToLower(text)
		for _, pattern := range []string{"sandbox_denied", "sandbox denied", "denied by sandbox", "permission_denied", "permission denied", "approval_denied", "approval denied", "approval rejected", "denied by policy", "rejected by user", "not approved"} {
			if strings.Contains(low, pattern) {
				return true
			}
		}
	}
	return false
}

func resetFromFailures(now time.Time, infos []errorInfo, stderr string) time.Time {
	for _, info := range infos {
		if !limitError(info) {
			continue
		}
		if !info.ResetAt.IsZero() {
			return info.ResetAt
		}
		if at, ok := parseReset(info.Text, now); ok {
			return at
		}
	}
	if limitText(stderr) {
		if at, ok := parseReset(stderr, now); ok {
			return at
		}
	}
	return time.Time{}
}

func limitError(info errorInfo) bool {
	for _, status := range info.Statuses {
		if status == 429 {
			return true
		}
	}
	return limitText(append(info.Codes, info.Text)...)
}

func limitText(texts ...string) bool {
	for _, text := range texts {
		low := strings.ToLower(text)
		for _, pattern := range []string{"usage limit", "usage_limit", "session limit", "weekly limit", "plan limit", "rate limit", "rate_limit", "quota", "too many requests", "http 429", "status 429"} {
			if strings.Contains(low, pattern) {
				return true
			}
		}
	}
	return false
}

func outcomeError(o outcome, waitErr, readErr error, diagnostic string) error {
	if o.timedOut {
		return errors.New("codex: timed out")
	}
	if o.ctxErr != nil {
		return fmt.Errorf("codex: cancelled: %w", o.ctxErr)
	}
	detail := ""
	for _, info := range o.structuredFailure {
		if strings.TrimSpace(info.Text) != "" {
			detail = strings.TrimSpace(info.Text)
			break
		}
	}
	if detail == "" {
		detail = strings.TrimSpace(o.stderr)
	}
	if detail == "" {
		detail = strings.TrimSpace(diagnostic)
	}
	if detail == "" && o.missingSession {
		detail = "the CLI exited without a thread id"
	}
	if detail == "" && !o.terminalComplete && !o.terminalFailed {
		detail = "the CLI exited without a completed turn"
	}
	if detail == "" && waitErr != nil {
		detail = waitErr.Error()
	}
	if detail == "" && readErr != nil {
		detail = readErr.Error()
	}
	if len(detail) > 2000 {
		detail = "…" + detail[len(detail)-2000:]
	}
	if detail == "" {
		return fmt.Errorf("codex: exit %d", o.exitCode)
	}
	return fmt.Errorf("codex: exit %d: %s", o.exitCode, detail)
}
