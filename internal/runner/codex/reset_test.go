package codex

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseCodexResetTimes(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		text string
		want time.Time
	}{
		{"relative", "usage limit; resets in 26 minutes", now.Add(26 * time.Minute)},
		{"try again", "try again in 45 seconds", now.Add(45 * time.Second)},
		{"absolute ISO", "plan limit resets at 2026-08-27T09:30:00Z", time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)},
		{"clock", "session limit resets 3:20pm (UTC)", time.Date(2026, 8, 26, 15, 20, 0, 0, time.UTC)},
		{"weekly", "weekly limit resets Monday 9am", time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseReset(test.text, now)
			if !ok || !got.Equal(test.want) {
				t.Fatalf("parseReset = %s, %v; want %s, true", got, ok, test.want)
			}
		})
	}
}

func TestResetClockUsesNamedTimezone(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	at, ok := parseReset("usage limit resets at 3 PM (PDT)", now)
	want := time.Date(2026, 8, 26, 22, 0, 0, 0, time.UTC)
	if !ok || !at.Equal(want) {
		t.Fatalf("PDT reset = %s, %t; want %s", at, ok, want)
	}
	if at, ok := parseReset("usage limit resets at 3 PM (UNKNOWN)", now); ok || !at.IsZero() {
		t.Fatalf("unknown timezone reset = %s, %t; want rejection", at, ok)
	}
	at, ok = parseReset("usage limit resets at 15:00 (+02:00)", now)
	want = time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	if !ok || !at.Equal(want) {
		t.Fatalf("numeric-zone reset = %s, %t; want %s", at, ok, want)
	}
}

func TestStructuredResetFields(t *testing.T) {
	now := time.Now()
	absolute := parseErrorInfo("limit", json.RawMessage(`{"type":"usage_limit","reset_at":"2026-08-27T09:30:00Z"}`))
	if want := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC); !absolute.ResetAt.Equal(want) {
		t.Fatalf("absolute reset = %s, want %s", absolute.ResetAt, want)
	}
	relative := errorInfo{}
	collectErrorFields(map[string]any{"retry_after_seconds": float64(90)}, "", now, &relative)
	if delta := relative.ResetAt.Sub(now); delta != 90*time.Second {
		t.Fatalf("relative reset delta = %s", delta)
	}
}

func TestResetIsLimitedToLimitFailures(t *testing.T) {
	now := time.Now()
	service := parseErrorInfo("service unavailable; resets in 20 minutes", json.RawMessage(`{"status_code":503}`))
	if got := resetFromFailures(now, []errorInfo{service}, ""); !got.IsZero() {
		t.Fatalf("service failure exposed a plan reset: %s", got)
	}
	limit := parseErrorInfo("rate limit; resets in 20 minutes", json.RawMessage(`{"status_code":429}`))
	if got := resetFromFailures(now, []errorInfo{limit}, ""); got.Sub(now) != 20*time.Minute {
		t.Fatalf("limit reset = %s", got)
	}
}
