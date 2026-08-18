//go:build unix

package drain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bd-auto/internal/runner"
	"bd-auto/internal/runner/claude"
)

// The whole chain, from the line a real CLI printed to the sentence a human
// reads off the stopped run.
//
// Each half of it is covered in its own package, and the halves have been wrong
// separately before: the adapter read the line and told nobody, and the engine
// had nothing to say about a wall it had been told the height of. What this
// test pins is that the number survives the trip — a result line the CLI
// emitted, through the classifier, into the reason the run stops on.
//
// The clock is computed rather than copied so the test does not depend on what
// time it is run at. Everything else about the line is the one from
// .beads/auto/logs/beads-auto-imp-pzi-a1-r0-worker.jsonl: subtype "success",
// is_error true, terminal_reason api_error, and the reset in product copy.
func TestASessionLimitsResetSurvivesIntoTheRunsStopReason(t *testing.T) {
	repo := testRepo(t)
	iss := newIssues("t-1").under("epic-1", "t-1")

	at := time.Now().Add(2 * time.Hour).Truncate(time.Minute)
	line := fmt.Sprintf(`{"type":"result","subtype":"success","is_error":true,"session_id":"S1",`+
		`"num_turns":1,"total_cost_usd":0,"terminal_reason":"api_error","api_error_status":429,`+
		`"result":"You've hit your session limit · resets %s"}`,
		strings.ToLower(at.Format("3:04pm")))

	// Through a file: the line carries an apostrophe, and a shell-quoting
	// accident here would be testing the fixture.
	out := filepath.Join(t.TempDir(), "result.jsonl")
	if err := os.WriteFile(out, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write result line: %v", err)
	}
	runs := filepath.Join(t.TempDir(), "runs")
	bin := stubCLI(t, "echo x >> "+runs+"\ncat "+out+"\nexit 1\n")

	e := drainEngine(t, repo, testCfg(1, 0), iss, nil, nil)
	// The stub answers everything with the 429 line, --version included, which
	// no real CLI does: a preflight would stop the run before the worker this
	// test is about, and would count as one of the runs it counts. What the
	// preflight makes of an outage is its own test.
	e.SkipPreflight = true
	e.NewRunner = func(runner.Role, runner.Spec) (runner.Runner, error) {
		return &claude.Runner{Bin: bin}, nil
	}

	rep, err := e.Drain(context.Background(), DrainOptions{
		Epic: "epic-1", Scope: []string{"t-1"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rep.Outcome != OutcomeInfra {
		t.Fatalf("run outcome %s (%s), want infra-failed", rep.Outcome, rep.Reason)
	}
	when := at.Format(resetTimeFormat)
	if !strings.Contains(rep.Reason, when) {
		t.Errorf("the run stopped saying:\n%s\nwhich never names the reset the CLI reported (%s)",
			rep.Reason, when)
	}
	if n := len(strings.Fields(readFile(t, runs))); n != 1 {
		t.Errorf("the CLI ran %d time(s), want 1: two hours is beyond anything the retries "+
			"can outlast", n)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
