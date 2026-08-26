package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/runner"
)

func TestBillingStatusVariants(t *testing.T) {
	tests := []struct {
		status string
		want   runner.BillingSource
	}{
		{"Logged in using ChatGPT", runner.BillingChatGPTPlan},
		{"Authenticated with a ChatGPT plan", runner.BillingChatGPTPlan},
		{"Logged in using an API key", runner.BillingAPIKey},
		{"Logged in using API key", runner.BillingAPIKey},
		{"Not logged in", runner.BillingUnknown},
		{"surprising future output", runner.BillingUnknown},
	}
	for _, test := range tests {
		if got := parseBillingStatus(test.status); got != test.want {
			t.Errorf("parseBillingStatus(%q) = %q, want %q", test.status, got, test.want)
		}
	}
}

func TestBillingSourceEnvironmentOverrideAndWhitespace(t *testing.T) {
	t.Run("real override wins without consulting saved login", func(t *testing.T) {
		t.Setenv("CODEX_API_KEY", "  sk-test  ")
		r := &Runner{Bin: fakeCLI(t, "exit 99")}
		got, err := r.BillingSource(context.Background(), t.TempDir())
		if err != nil || got != runner.BillingAPIKey {
			t.Fatalf("BillingSource = %q, %v", got, err)
		}
	})

	t.Run("whitespace is not an override", func(t *testing.T) {
		t.Setenv("CODEX_API_KEY", " \t\n")
		r := &Runner{Bin: fakeCLI(t, `printf '%s\n' 'Logged in using ChatGPT'`)}
		got, err := r.BillingSource(context.Background(), t.TempDir())
		if err != nil || got != runner.BillingChatGPTPlan {
			t.Fatalf("BillingSource = %q, %v", got, err)
		}
	})
}

func TestBillingSourceUnknownFailureAndTimeoutAreActionableInputs(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	tests := []struct {
		name string
		body string
		text string
	}{
		{"signed out", `printf '%s\n' 'Not logged in'; exit 1`, "Not logged in"},
		{"unrecognized", `printf '%s\n' 'new auth source'`, "not recognized"},
		{"failure", `printf '%s\n' 'credential store unavailable'; exit 2`, "failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := (&Runner{Bin: fakeCLI(t, test.body)}).BillingSource(context.Background(), t.TempDir())
			if got != runner.BillingUnknown || err == nil || !strings.Contains(err.Error(), test.text) {
				t.Fatalf("BillingSource = %q, %v; want unknown containing %q", got, err, test.text)
			}
		})
	}

	r := &Runner{Bin: fakeCLI(t, `while :; do sleep 1; done`), BillingTimeout: 20 * time.Millisecond}
	got, err := r.BillingSource(context.Background(), t.TempDir())
	if got != runner.BillingUnknown || err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timed BillingSource = %q, %v", got, err)
	}
}
