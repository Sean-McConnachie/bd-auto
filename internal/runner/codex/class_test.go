package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"bd-auto/internal/runner"
)

func TestClassifyCodexOutcomes(t *testing.T) {
	now := time.Now()
	structured := func(raw string) errorInfo { return parseErrorInfo("", json.RawMessage(raw)) }
	tests := []struct {
		name string
		out  outcome
		want runner.Class
	}{
		{"success", outcome{exitCode: 0, terminalComplete: true}, runner.ClassOK},
		{"successful prose is not inspected", outcome{exitCode: 0, terminalComplete: true, stderr: ""}, runner.ClassOK},
		{"plan limit", outcome{exitCode: 1, terminalFailed: true, structuredFailure: []errorInfo{structured(`{"type":"usage_limit_reached","message":"ChatGPT usage limit reached"}`)}}, runner.ClassInfraFailed},
		{"structured 429", outcome{exitCode: 1, terminalFailed: true, structuredFailure: []errorInfo{structured(`{"status_code":429,"message":"slow down"}`)}}, runner.ClassInfraFailed},
		{"structured 503", outcome{exitCode: 1, terminalFailed: true, structuredFailure: []errorInfo{structured(`{"http_status":503,"message":"upstream"}`)}}, runner.ClassInfraFailed},
		{"missing authentication", outcome{exitCode: 1, terminalFailed: true, structuredFailure: []errorInfo{structured(`{"code":"not_logged_in"}`)}}, runner.ClassInfraFailed},
		{"network", outcome{exitCode: 1, terminalFailed: true, structuredFailure: []errorInfo{structured(`{"type":"network_error","message":"connection failed"}`)}}, runner.ClassInfraFailed},
		{"work failure after work", outcome{exitCode: 1, terminalFailed: true, worked: true, structuredFailure: []errorInfo{structured(`{"type":"model_error","message":"tool loop failed"}`)}}, runner.ClassWorkFailed},
		{"structured work failure beats misleading stderr", outcome{exitCode: 1, terminalFailed: true, worked: true, structuredFailure: []errorInfo{structured(`{"type":"model_error","message":"tests failed"}`)}, stderr: "old request HTTP 429"}, runner.ClassWorkFailed},
		{"stderr fallback rate limit", outcome{exitCode: 1, stderr: "request failed: HTTP 429"}, runner.ClassInfraFailed},
		{"stderr fallback service failure", outcome{exitCode: 1, stderr: "HTTP 502 bad gateway"}, runner.ClassInfraFailed},
		{"crash before terminal event", outcome{exitCode: 2, worked: true, stderr: "unexpected crash"}, runner.ClassInfraFailed},
		{"cli crashes after completion", outcome{exitCode: 2, terminalComplete: true, completedAt: now, stderr: "unexpected crash"}, runner.ClassInfraFailed},
		{"zero exit without terminal event", outcome{exitCode: 0}, runner.ClassInfraFailed},
		{"missing binary", outcome{startErr: context.DeadlineExceeded, exitCode: -1}, runner.ClassInfraFailed},
		{"cancelled", outcome{ctxErr: context.Canceled, exitCode: 1}, runner.ClassInterrupted},
		{"timed out", outcome{timedOut: true, exitCode: -1}, runner.ClassInterrupted},
		{"completion before cancellation", outcome{ctxErr: context.Canceled, exitCode: 143, terminalComplete: true, completedAt: now, cancelledAt: now.Add(time.Millisecond)}, runner.ClassOK},
		{"completion after cancellation", outcome{ctxErr: context.Canceled, exitCode: 143, terminalComplete: true, completedAt: now.Add(time.Millisecond), cancelledAt: now}, runner.ClassInterrupted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classify(test.out); got != test.want {
				t.Fatalf("classify = %s, want %s", got, test.want)
			}
		})
	}
}

func TestStructuredErrorFieldsTakePriority(t *testing.T) {
	info := parseErrorInfo("model failed", json.RawMessage(`{"code":"work_failed","nested":{"status_code":429}}`))
	if !info.Structured || !structuredInfra([]errorInfo{info}) {
		t.Fatalf("structured nested status was not detected: %+v", info)
	}
}
