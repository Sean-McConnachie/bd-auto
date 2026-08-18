//go:build linux

package tui

// A screenshot harness for the wave table.
//
// Every other test in this package asserts on strings. That is the right way
// to test behaviour and the wrong way to answer "what does it actually look
// like on a terminal, in every state it can be in" — which is a question worth
// answering, because several of those states are reachable only when a run
// goes wrong and nobody wants to break a live run to look at one.
//
// So this drives the real UI, on the real terminal it was started from, over
// synthetic drain events, and stops at each scene for something outside to
// photograph it. It is inert under `go test ./...`: without BD_AUTO_SHOTS in
// the environment both tests skip, so the gate never sees it.
//
// It is meant to be run as a compiled test binary inside a terminal
// multiplexer, so that stdin and stdout are a real tty:
//
//	go test -c -o /tmp/tui.test ./internal/tui
//	BD_AUTO_SHOTS=/tmp/shots /tmp/tui.test -test.run TestScreenshots
//
// scripts/tui-shots.sh does that, and drives the keystrokes.
//
// The protocol with the driver is two files. The harness writes
// `ready-NN-<name>` once a scene is on screen and blocks until `go-NN`
// appears; the driver sends that scene's keys, captures the pane, and creates
// `go-NN`. Keys are sent from outside rather than injected here on purpose:
// the point of a screenshot is that it shows what a person would see after
// pressing what a person would press.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bd-auto/internal/ask"
	"bd-auto/internal/drain"
	"bd-auto/internal/runner"
)

// shotsDir is the directory the driver and the harness meet in, or "" when
// nobody is taking screenshots.
func shotsDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("BD_AUTO_SHOTS")
	if dir == "" {
		t.Skip("set BD_AUTO_SHOTS to a directory to capture screenshots; see scripts/tui-shots.sh")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("BD_AUTO_SHOTS=%s: %v", dir, err)
	}
	return dir
}

// scener runs the scene protocol.
type scener struct {
	t   *testing.T
	ui  *UI
	dir string
	n   int
}

// scene emits a batch of events, waits for them to reach the screen, and holds
// there until the driver says it has the picture.
func (s *scener) scene(name string, events ...drain.Event) {
	s.t.Helper()
	for _, e := range events {
		s.ui.Observe(e)
	}
	s.n++
	// A short settle: Observe is a queue push, and the frame it causes is drawn
	// by bubbletea's own loop a tick later.
	time.Sleep(350 * time.Millisecond)

	ready := filepath.Join(s.dir, fmt.Sprintf("ready-%02d-%s", s.n, name))
	if err := os.WriteFile(ready, nil, 0o644); err != nil {
		s.t.Fatalf("scene %s: %v", name, err)
	}
	got := filepath.Join(s.dir, fmt.Sprintf("go-%02d", s.n))
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := os.Stat(got); err == nil {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("scene %s: the driver never captured it", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// base is when this harness started. Event times are offsets back from it
// rather than a fixed epoch: the elapsed column is rendered against the real
// clock, so a fixture dated 2026 would photograph every worker as having run
// for ten hours.
var base = time.Now()

func ago(secs int) time.Time { return base.Add(-time.Duration(secs) * time.Second) }

// after is the same for a time the scene wants to be later than the start it
// pairs with, so a finished row shows how long it ran rather than a dash.
func after(secs int) time.Time { return base.Add(time.Duration(secs) * time.Second) }

func usage(cost float64, in, out int) runner.Usage {
	return runner.Usage{CostUSD: cost, InputTokens: in, OutputTokens: out}
}

// The role is carried because the state column is written from it: activity
// with no role on it is activity nothing can attribute, and a row shown that
// way says only "running", which is the thing these pictures are meant to show
// the end of.
func fragment(issue, text string, cost float64) drain.Event {
	return drain.Event{Kind: drain.EventActivity, At: ago(0), Wave: 1, Issue: issue,
		Role: runner.RoleWorker, Phase: runner.EventText, Text: text, Usage: usage(cost, 1200, 300)}
}

func tool(issue, name string, secs int, cost float64) drain.Event {
	return drain.Event{Kind: drain.EventActivity, At: ago(secs), Wave: 1, Issue: issue,
		Role: runner.RoleWorker, Phase: runner.EventToolUse, Tool: name, Text: name,
		Usage: usage(cost, 1200, 300)}
}

func question(issue, id, header, text string, options ...[2]string) drain.Event {
	q := ask.Question{ID: id, Issue: issue, Role: "worker", Header: header, Text: text, AskedAt: ago(0)}
	for _, o := range options {
		q.Options = append(q.Options, ask.Option{Label: o[0], Description: o[1]})
	}
	return drain.Event{Kind: drain.EventQuestion, At: ago(0), Wave: 2, Issue: issue, Question: &q}
}

// TestScreenshots walks the table through every state it has, on a terminal.
//
// The scene list is the coverage claim: each name is one thing the display can
// be, and a state with no scene here is a state nobody has ever looked at.
func TestScreenshots(t *testing.T) {
	dir := shotsDir(t)
	control := newPressed("kv-ctf.1", "kv-555.2", "kv-555.3")
	answers := newAnswered("q1", "q2", "q3", "q4")

	ui := New(Options{Control: control, Ask: answers, Output: os.Stdout, Input: os.Stdin})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ui.Run(ctx) }()

	s := &scener{t: t, ui: ui, dir: dir}
	scope := []string{"kv-ctf.1", "kv-555.1", "kv-555.2", "kv-555.3", "kv-555.4", "kv-555.5", "kv-555.6"}

	// The scope, before anything has been spawned: six rows, all queued.
	s.scene("scope",
		drain.Event{Kind: drain.EventRunStart, At: ago(300), Text: "kv-555", Issues: scope})

	// An issue the run will never offer, parked before a worker exists: its
	// dependency is outside the scope somebody confirmed. It is the one row that
	// is finished before the run starts, and the only place the parked colour
	// appears without a kill behind it.
	s.scene("scope-parked",
		drain.Event{Kind: drain.EventScopeParked, At: ago(299), Issue: "kv-555.5",
			Text: "depends on kv-ctf.4, which is not in this run"})

	// A wave in flight: two workers running, one finished, the rest waiting.
	s.scene("wave-running",
		drain.Event{Kind: drain.EventWaveStart, At: ago(300), Wave: 1, Issues: scope},
		drain.Event{Kind: drain.EventIssueStart, At: ago(300), Wave: 1, Issue: "kv-ctf.1",
			Text: "internal/store: the Store interface and an in-memory implementation"},
		drain.Event{Kind: drain.EventIssueEnd, At: ago(137), Wave: 1, Issue: "kv-ctf.1",
			Outcome: drain.OutcomeDone, Text: "finished", Usage: usage(0.8135, 41000, 5200)},
		drain.Event{Kind: drain.EventIssueStart, At: ago(25), Wave: 1, Issue: "kv-555.1",
			Text: "internal/cli: the command dispatch table"},
		drain.Event{Kind: drain.EventIssueStart, At: ago(23), Wave: 1, Issue: "kv-555.2",
			Text: "kv get, set and del"},
		tool("kv-555.1", "Edit", 20, 0.4210),
		tool("kv-555.2", "Bash", 22, 0.1130))

	// The activity column following a message as it is written. This is what
	// separates a worker that is thinking from one that has stalled.
	s.scene("streaming",
		fragment("kv-555.1", "Registering the three commands from an init so that ", 0.4380),
		fragment("kv-555.1", "importing the package is enough, then checking Usage() ", 0.4390),
		fragment("kv-555.1", "still sorts them", 0.4400))

	// ↓ ↓: the cursor, moved by the driver.
	s.scene("selection")

	// k on the selected row. The row says killing before the process has died.
	s.scene("killing")

	// The kill landing: the issue is parked and reported failed.
	s.scene("killed",
		drain.Event{Kind: drain.EventIssueEnd, At: ago(0), Wave: 1, Issue: "kv-555.2",
			Outcome: drain.OutcomeParked, Text: "killed from the table", Usage: usage(0.2044, 9000, 1100),
			Report: &drain.Report{Stage: drain.StageKilled}})

	// The barrier, while it is still working: a model is resolving a conflict,
	// which is minutes in which every worker is finished and nothing else moves.
	s.scene("integrating",
		drain.Event{Kind: drain.EventIssueEnd, At: ago(0), Wave: 1, Issue: "kv-555.1",
			Outcome: drain.OutcomeDone, Text: "the dispatch table, with tests",
			Usage: usage(0.5512, 41000, 5200), Report: &drain.Report{Issue: "kv-555.1"}},
		drain.Event{Kind: drain.EventWaveIntegrating, At: ago(0), Wave: 1,
			Issues: []string{"kv-ctf.1", "kv-555.1"}},
		drain.Event{Kind: drain.EventActivity, At: ago(0), Wave: 1, Issue: "kv-ctf.1",
			Role: runner.RoleIntegrator, Phase: runner.EventToolUse,
			Text: "Edit internal/cli/cli.go"})

	// The barrier: what merged, what did not, and the gate on the merged result.
	s.scene("wave-integrated",
		drain.Event{Kind: drain.EventWaveEnd, At: ago(0), Wave: 1,
			Usage: usage(0.0510, 3000, 400),
			Integration: &drain.IntegrateReport{Epic: "kv-555", Wave: 1, GatePassed: true,
				Merges: []drain.Merge{
					{Issue: "kv-ctf.1", Branch: "bd-auto/kv-ctf.1", Outcome: drain.MergeResolved,
						Conflicts: []string{"internal/cli/cli.go", "internal/cli/cli_test.go"}},
					// Finished, gated, reviewed — and it still did not land.
					{Issue: "kv-555.1", Branch: "bd-auto/kv-555.1", Outcome: drain.MergeParked,
						Reason: "git would not merge bd-auto/kv-555.1 and left no conflicted paths"},
				}}})

	// autonomy: wave — held at the barrier until a human releases it.
	s.scene("paused", drain.Event{Kind: drain.EventPaused, At: ago(0), Wave: 1})
	s.scene("resumed", drain.Event{Kind: drain.EventResumed, At: ago(0), Wave: 1})

	// A worker with a genuine ambiguity, asking while the rest of the wave runs.
	s.scene("question",
		drain.Event{Kind: drain.EventWaveStart, At: ago(0), Wave: 2,
			Issues: []string{"kv-555.3", "kv-555.4", "kv-555.6"}},
		drain.Event{Kind: drain.EventIssueStart, At: ago(0), Wave: 2, Issue: "kv-555.3",
			Text: "kv list, in two output formats"},
		drain.Event{Kind: drain.EventIssueStart, At: ago(0), Wave: 2, Issue: "kv-555.4",
			Text: "cmd/kv: wire main to the cli package"},
		drain.Event{Kind: drain.EventIssueStart, At: ago(0), Wave: 2, Issue: "kv-555.6",
			Text: "kv del, and the exit code an absent key gets"},
		tool("kv-555.4", "Write", 70, 0.0902),
		tool("kv-555.6", "Bash", 55, 0.1471),
		question("kv-555.3", "q1", "JSON shape",
			"Which shape should `kv list --format json` print? It is the output another program parses, so it is not mine to guess.",
			[2]string{"a flat object", "{\"a\":\"1\",\"b\":\"2\"} — smallest, and loses ordering"},
			[2]string{"an array of objects", "[{\"key\":\"a\",\"value\":\"1\"}] — keeps the sorted order"}))

	// ↓: the choice cursor inside the box, which is a different cursor from the
	// table's and must not move it.
	s.scene("question-choice")

	// t: writing an answer the options did not offer. Every printable key
	// belongs to the field while it is open, digits included.
	s.scene("question-typing")

	// esc out of the field, then a second question arrives behind the first.
	s.scene("question-queued",
		question("kv-555.4", "q2", "Exit code",
			"Should an unknown subcommand exit 1 or 2? The seed used 2 for usage errors.",
			[2]string{"exit 2", "a usage error, like the seed"},
			[2]string{"exit 1", "an ordinary failure"}))

	// k, while a question is up. The table's keys are suspended: the worker it
	// would kill is the one waiting for this answer.
	s.scene("question-swallowed")

	// 1: answered by the number printed beside the option.
	s.scene("question-answered")

	// s: handed back to the model, which is a real answer — decide, and write
	// down what you assumed.
	s.scene("question-declined")

	// A question with no options at all: only free text will do.
	s.scene("question-free-text",
		question("kv-555.4", "q3", "Field name",
			"What should the environment variable holding the store path be called?"))

	// esc drops the question, and then the three shapes an issue's life takes,
	// side by side and all in flight at once: a worker still writing, a
	// reviewer judging what another worker already finished, and the gate —
	// which spawns no model at all, and which every one of these pictures used
	// to show as a worker with a climbing clock and nothing happening.
	s.scene("stages",
		drain.Event{Kind: drain.EventStageStart, At: ago(0), Wave: 2, Issue: "kv-555.3",
			Stage: "review", Role: runner.RoleReviewer},
		drain.Event{Kind: drain.EventStageStart, At: ago(0), Wave: 2, Issue: "kv-555.4",
			Stage: "gate"},
		fragment("kv-555.6", "Deleting a key that was never there is not an error, so ", 0.1490),
		fragment("kv-555.6", "del exits 0 and prints nothing", 0.1502))

	// The gate answering, and the round its answer buys: the row says which
	// command failed rather than leaving the next worker turn unexplained.
	s.scene("stage-failed",
		drain.Event{Kind: drain.EventStageEnd, At: ago(0), Wave: 2, Issue: "kv-555.4",
			Stage: "gate", Text: "gate: go test ./... failed: cli_test.go:41: unknown command \"del\""})

	// The states a run ends its issues in, side by side.
	s.scene("terminal-states",
		drain.Event{Kind: drain.EventIssueEnd, At: after(112), Wave: 2, Issue: "kv-555.3",
			Outcome: drain.OutcomeDone, Text: "review passed on round 2", Usage: usage(1.4102, 82000, 9100)},
		drain.Event{Kind: drain.EventIssueEnd, At: after(64), Wave: 2, Issue: "kv-555.4",
			Outcome: drain.OutcomeFailed, Text: "gate: go test ./... failed twice", Usage: usage(0.6610, 30000, 4000)},
		drain.Event{Kind: drain.EventIssueEnd, At: after(30), Wave: 1, Issue: "kv-555.1",
			Outcome: drain.OutcomeInterrupted, Text: "the run was stopped", Usage: usage(0.5010, 22000, 2600)},
		drain.Event{Kind: drain.EventIssueEnd, At: after(88), Wave: 2, Issue: "kv-555.6",
			Outcome: drain.OutcomeDone, Text: "gate and review passed first time", Usage: usage(0.5811, 34000, 4300)})

	// The same table on a narrow terminal: every column keeps its width and the
	// activity is what gives way.
	s.scene("narrow")
	s.scene("wide")

	// q once: the run is asked to stop and the table stays up, because
	// stopping is not instant.
	s.scene("stopping")

	// The run's own ending, with the total the report carries.
	s.scene("run-end",
		drain.Event{Kind: drain.EventRunEnd, At: ago(0), Run: &drain.DrainReport{
			Epic: "kv-555", Waves: 2, Outcome: drain.OutcomeDone,
			Done:   []string{"kv-ctf.1", "kv-555.1", "kv-555.3", "kv-555.6"},
			Parked: []string{"kv-555.2", "kv-555.4"},
			Usage:  usage(4.5218, 221000, 26700)}})

	ui.Finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the view never closed")
	}
	// The last frame the program draws is the one that says the run is over,
	// and it is only drawn on the way out — so it is photographed after the
	// view has gone rather than during it. The frame stays in the pane.
	s.scene("finished")

	// The keys were real keys on a real terminal, so what they asked the run to
	// do is worth reporting alongside the pictures.
	t.Logf("kills: %v; stops: %d", control.kills(), control.stopped())
}

// TestScreenshotsReadOnly is the same table with no channel back to the run:
// what a view attached to somebody else's drain can show, and what it has to
// admit it cannot do.
func TestScreenshotsReadOnly(t *testing.T) {
	dir := shotsDir(t)
	ui := New(Options{Output: os.Stdout, Input: os.Stdin})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ui.Run(ctx) }()

	s := &scener{t: t, ui: ui, dir: dir}
	s.scene("readonly",
		drain.Event{Kind: drain.EventRunStart, At: ago(120), Text: "kv-555",
			Issues: []string{"kv-555.3", "kv-555.4"}},
		drain.Event{Kind: drain.EventWaveStart, At: ago(120), Wave: 2,
			Issues: []string{"kv-555.3", "kv-555.4"}},
		drain.Event{Kind: drain.EventIssueStart, At: ago(95), Wave: 2, Issue: "kv-555.3",
			Text: "kv list, in two output formats"},
		drain.Event{Kind: drain.EventIssueStart, At: ago(88), Wave: 2, Issue: "kv-555.4",
			Text: "cmd/kv: wire main to the cli package"},
		tool("kv-555.3", "Read", 30, 0.2210),
		question("kv-555.3", "q4", "JSON shape",
			"Which shape should `kv list --format json` print?",
			[2]string{"a flat object", ""}, [2]string{"an array of objects", ""}))

	// k, on a view that cannot kill anything.
	s.scene("readonly-refused")

	// esc, which drops the question this view could never have answered, and
	// leaves the key line a watcher actually gets.
	s.scene("readonly-dismissed")

	ui.Finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the view never closed")
	}
}
