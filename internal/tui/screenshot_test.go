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
	"strings"
	"testing"
	"time"

	"bd-auto/internal/ask"
	"bd-auto/internal/drain"
	"bd-auto/internal/pipeline"
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
//
// The attempt and the round are carried for the same reason one column over.
// Activity is the only thing that says which turn a worker is on — no event
// announces one beginning — so a picture built without them is a picture of an
// ATT column full of dashes.
func fragment(issue, text string, cost float64) drain.Event {
	return stamped(drain.Event{Kind: drain.EventActivity, At: ago(0), Wave: 1, Issue: issue,
		Role: runner.RoleWorker, Phase: runner.EventText, Text: text, Usage: usage(cost, 1200, 300)})
}

func tool(issue, name string, secs int, cost float64) drain.Event {
	return stamped(drain.Event{Kind: drain.EventActivity, At: ago(secs), Wave: 1, Issue: issue,
		Role: runner.RoleWorker, Phase: runner.EventToolUse, Tool: name, Text: name,
		Usage: usage(cost, 1200, 300)})
}

// where is how far each issue in this run has got: the attempt in flight, and
// the worker's turn inside it. Held here rather than passed to every call
// because a scene is written as what changed, and an issue's attempt does not
// change between one tool call and the next.
//
// kv-555.2 is the one on its second attempt — its first worktree and session
// were thrown away — while kv-555.4 and kv-555.6 are on their first attempt and
// their second round, which is the same worker still going in the same session.
// Between them the pictures show the two numbers disagreeing, which is the
// whole reason there are two of them.
var where = map[string][2]int{
	"kv-555.2": {2, 0},
	"kv-555.4": {1, 1},
	"kv-555.6": {1, 1},
}

// stamped puts where an issue has got to onto an event, as the engine's sink
// does on a real run.
func stamped(e drain.Event) drain.Event {
	w, ok := where[e.Issue]
	if !ok {
		w = [2]int{1, 0}
	}
	e.Attempt, e.Round = w[0], w[1]
	return e
}

// transcripts writes the fixture the transcript scenes are photographed
// against, into a temporary repo root the harness hands the view.
//
// It is on disk rather than on the bus because that is where the real thing is:
// every model bd-auto spawns writes its whole stream to
// .beads/auto/logs/<issue>-a<attempt>-r<round>-<role>.jsonl, and the live event
// stream carries a tool's name without its arguments. A picture of this view
// built out of events would be a picture of something the view cannot do.
func transcripts(t *testing.T, root string) {
	t.Helper()
	writeTranscript(t, root, "kv-555.2-a1-r0-worker.jsonl", 1,
		assistantLine(t, textBlock("Reading the store interface first, so get, set and del agree "+
			"on what a missing key is before any of them is written.")),
		assistantLine(t, toolBlock("Read", map[string]any{
			"file_path": "/home/kv/.beads/auto/wt/kv-555.2/internal/store/store.go"})),
		userLine(t, resultBlock(storeFixture, false)),
		assistantLine(t, textBlock("ErrNotFound is the whole contract: del is idempotent and get "+
			"is the only one of the three that fails on it.")),
		assistantLine(t, toolBlock("Edit", map[string]any{
			"file_path":  "/home/kv/.beads/auto/wt/kv-555.2/internal/cli/get.go",
			"old_string": "return fmt.Errorf(\"no such key\")", "new_string": "return ErrNotFound"})),
		userLine(t, resultBlock("String to replace not found in the file.", true)),
		assistantLine(t, toolBlock("Grep", map[string]any{
			"pattern": "no such key",
			"path":    "/home/kv/.beads/auto/wt/kv-555.2/internal/cli"})),
		userLine(t, resultBlock("internal/cli/get.go:41:\t\treturn fmt.Errorf(\"no such key: %s\", key)", false)),
		assistantLine(t, toolBlock("Edit", map[string]any{
			"file_path": "/home/kv/.beads/auto/wt/kv-555.2/internal/cli/get.go"})),
		userLine(t, resultBlock("The file internal/cli/get.go has been updated.", false)),
		assistantLine(t, toolBlock("Bash", map[string]any{
			"command": "go test ./internal/cli/... -run TestGet", "description": "run the get tests"})),
		userLine(t, resultBlock(testFixture, false)),
		assistantLine(t, textBlock("All three commands now route a missing key through "+
			"store.ErrNotFound, and get is the only one that turns it into a non-zero exit.")),
		endLine(t, "success", 34, 0.9214))
	writeTranscript(t, root, "kv-555.2-a1-r0-review.jsonl", 2,
		assistantLine(t, textBlock("The diff does what the issue asked and the exit codes match "+
			"the seed. One thing to fix: del still prints to stderr on a missing key, which the "+
			"issue says it must not.")))
}

const storeFixture = `package store

import "errors"

// ErrNotFound is what every read of an absent key returns.
var ErrNotFound = errors.New("store: key not found")

// Store is the whole of what the commands may do to the data.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Del(key string) error
	List() ([]string, error)
}`

const testFixture = `=== RUN   TestGetReturnsTheValue
--- PASS: TestGetReturnsTheValue (0.00s)
=== RUN   TestGetOnAMissingKeyExitsNonZero
--- PASS: TestGetOnAMissingKeyExitsNonZero (0.00s)
=== RUN   TestGetPrintsNothingOnAMissingKey
--- PASS: TestGetPrintsNothingOnAMissingKey (0.00s)
PASS
ok  	kv/internal/cli	0.412s`

// merged is one branch's verdict at the barrier. secs and cost are the merge's
// own, so the barrier's time and cost columns are the numbers the report
// carries rather than whatever the harness's clock happens to say.
func merged(issue string, outcome drain.MergeOutcome, secsAgo int, reason string,
	secs, cost float64, conflicts ...string) drain.Event {
	m := &drain.Merge{Issue: issue, Branch: "bd-auto/" + issue, Outcome: outcome,
		Reason: reason, Conflicts: conflicts, Seconds: secs, Usage: usage(cost, 9000, 700)}
	return drain.Event{Kind: drain.EventMergeEnd, At: ago(secsAgo), Wave: 1, Issue: issue,
		Text: reason, Usage: m.Usage, Merge: m}
}

// conflicted is git stopping on a branch, and the one model a barrier ever
// spawns being put on it.
func conflicted(issue string, paths ...string) drain.Event {
	return drain.Event{Kind: drain.EventMergeConflict, At: ago(47), Wave: 1, Issue: issue,
		Role: runner.RoleIntegrator, Text: strings.Join(paths, ", "),
		Merge: &drain.Merge{Issue: issue, Branch: "bd-auto/" + issue, Conflicts: paths}}
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

	root := t.TempDir()
	transcripts(t, root)
	ui := New(Options{Control: control, Ask: answers, Output: os.Stdout, Input: os.Stdin, RepoRoot: root})
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
		stamped(drain.Event{Kind: drain.EventIssueEnd, At: ago(137), Wave: 1, Issue: "kv-ctf.1",
			Outcome: drain.OutcomeDone, Text: "finished", Usage: usage(0.8135, 41000, 5200)}),
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

	// enter: the transcript of the selected row, at its end, which is where a
	// watcher who wants to know what a worker is doing now needs to land. This
	// is the only picture in this set whose content comes off disk rather than
	// off the bus.
	s.scene("transcript")

	// g: the top of the same transcript. The separator naming the process, the
	// prose wrapped, each tool call carrying what it was called with, and a
	// result cut off with a count of what is missing.
	s.scene("transcript-top")

	// esc, ↓ ↓, enter: a row nothing has been spawned for yet. An empty pane
	// there is indistinguishable from a broken one.
	s.scene("transcript-empty")

	// esc, ↑ ↑, k: back to the table with the cursor exactly where it was left,
	// and k on it. The row says killing before the process has died.
	s.scene("killing")

	// The kill landing: the issue is parked and reported failed.
	s.scene("killed",
		stamped(drain.Event{Kind: drain.EventIssueEnd, At: ago(0), Wave: 1, Issue: "kv-555.2",
			Outcome: drain.OutcomeParked, Text: "killed from the table", Usage: usage(0.2044, 9000, 1100),
			Report: &drain.Report{Stage: drain.StageKilled}}))

	// The barrier, while it is still working: one branch merged and gone, one
	// with a model on it resolving a conflict, and the block saying which is
	// which. This is the picture the barrier used to have no way to draw — the
	// minutes it spends here were one status line over a table of finished
	// workers, and a run that had hung looked exactly the same.
	s.scene("integrating",
		stamped(drain.Event{Kind: drain.EventIssueEnd, At: ago(0), Wave: 1, Issue: "kv-555.1",
			Outcome: drain.OutcomeDone, Text: "the dispatch table, with tests",
			Usage: usage(0.5512, 41000, 5200), Report: &drain.Report{Issue: "kv-555.1"}}),
		drain.Event{Kind: drain.EventWaveIntegrating, At: ago(0), Wave: 1,
			Issues: []string{"kv-ctf.1", "kv-555.1"}},
		drain.Event{Kind: drain.EventMergeStart, At: ago(51), Wave: 1, Issue: "kv-ctf.1",
			Text: "bd-auto/kv-ctf.1"},
		merged("kv-ctf.1", drain.MergeClean, 48, "", 3, 0),
		drain.Event{Kind: drain.EventMergeStart, At: ago(48), Wave: 1, Issue: "kv-555.1",
			Text: "bd-auto/kv-555.1"},
		conflicted("kv-555.1", "internal/cli/cli.go", "internal/cli/cli_test.go"),
		drain.Event{Kind: drain.EventActivity, At: ago(0), Wave: 1, Issue: "kv-555.1",
			Role: runner.RoleIntegrator, Phase: runner.EventToolUse,
			Text: "Edit(internal/cli/cli.go)", Usage: usage(0.0210, 9000, 700)})

	// The conflict resolved, and the one run the whole barrier exists for: the
	// gate on everything together. It spawns no model, so without a row of its
	// own it is a whole test suite of nothing happening on screen.
	s.scene("barrier-gating",
		merged("kv-555.1", drain.MergeResolved, 0, "", 47, 0.0210,
			"internal/cli/cli.go", "internal/cli/cli_test.go"),
		drain.Event{Kind: drain.EventWaveGateStart, At: ago(11), Wave: 1,
			Text: "go build ./... · go test ./..."})

	// The gate red, and the only thing that can say which branch did it: the
	// merges come back off one at a time until the tree goes green, and the
	// branch that fixed it is the one that gets parked. Nothing is wrong with
	// the work — it is on its own branch, and the next barrier can merge it
	// again once the issue it broke is fixed.
	s.scene("barrier-red-gate",
		drain.Event{Kind: drain.EventWaveGateEnd, At: ago(0), Wave: 1, Passed: false,
			Text: "test failed (exit 1)"},
		drain.Event{Kind: drain.EventWaveRollback, At: ago(0), Wave: 1, Issue: "kv-555.1",
			Text:  "rolled back to find out what the gate is red on",
			Merge: &drain.Merge{Issue: "kv-555.1", Branch: "bd-auto/kv-555.1"}},
		drain.Event{Kind: drain.EventWaveGateStart, At: ago(6), Wave: 1,
			Text: "go build ./... · go test ./..."})

	// The barrier: what merged, what did not, the gate, and the branch the
	// rollback blamed.
	s.scene("wave-integrated",
		drain.Event{Kind: drain.EventWaveGateEnd, At: ago(0), Wave: 1, Passed: true,
			Text: "build · test"},
		merged("kv-555.1", drain.MergeParked, 0,
			"the wave gate failed on the merged result and went green once "+
				"bd-auto/kv-555.1 was rolled back", 47, 0.0210),
		drain.Event{Kind: drain.EventWaveEnd, At: ago(0), Wave: 1,
			Usage: usage(0.0210, 9000, 700),
			Integration: &drain.IntegrateReport{Epic: "kv-555", Wave: 1, GatePassed: true,
				Reason: "the gate was red on the merged result and is green with " +
					"bd-auto/kv-555.1 rolled back",
				Gate: []pipeline.Result{{Name: "build", Passed: true}, {Name: "test", Passed: true}},
				Merges: []drain.Merge{
					{Issue: "kv-ctf.1", Branch: "bd-auto/kv-ctf.1", Outcome: drain.MergeClean,
						Seconds: 3},
					// Finished, gated, reviewed, merged — and it still did not
					// survive the wave's own gate.
					{Issue: "kv-555.1", Branch: "bd-auto/kv-555.1", Outcome: drain.MergeParked,
						Seconds: 47, Usage: usage(0.0210, 9000, 700),
						Conflicts: []string{"internal/cli/cli.go", "internal/cli/cli_test.go"},
						Reason: "the wave gate failed on the merged result and went green once " +
							"bd-auto/kv-555.1 was rolled back"},
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

	// The same question on a terminal too narrow for it, and then on one too
	// short. Nothing the reader has to act on may be lost to either edge: the
	// options wrap and indent under their numbers, and where the box still does
	// not fit it is the question that gives way, counted rather than dropped.
	s.scene("question-narrow")
	s.scene("question-short")

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
	//
	// And the two numbers beside them: kv-555.3 is being reviewed for the first
	// time, while kv-555.4's gate and kv-555.6's worker are both on their
	// second round. kv-555.2, killed a few scenes back, is the one that shows
	// the other number — a second attempt, which is a different thing.
	s.scene("stages",
		stamped(drain.Event{Kind: drain.EventStageStart, At: ago(0), Wave: 2, Issue: "kv-555.3",
			Stage: "review", Role: runner.RoleReviewer}),
		stamped(drain.Event{Kind: drain.EventStageStart, At: ago(0), Wave: 2, Issue: "kv-555.4",
			Stage: "gate"}),
		fragment("kv-555.6", "Deleting a key that was never there is not an error, so ", 0.1490),
		fragment("kv-555.6", "del exits 0 and prints nothing", 0.1502))

	// The gate answering, and the round its answer buys: the row says which
	// command failed rather than leaving the next worker turn unexplained.
	s.scene("stage-failed",
		stamped(drain.Event{Kind: drain.EventStageEnd, At: ago(0), Wave: 2, Issue: "kv-555.4",
			Stage: "gate", Text: "gate: go test ./... failed: cli_test.go:41: unknown command \"del\""}))

	// The states a run ends its issues in, side by side.
	s.scene("terminal-states",
		stamped(drain.Event{Kind: drain.EventIssueEnd, At: after(112), Wave: 2, Issue: "kv-555.3",
			Outcome: drain.OutcomeDone, Text: "review passed on round 2", Usage: usage(1.4102, 82000, 9100)}),
		stamped(drain.Event{Kind: drain.EventIssueEnd, At: after(64), Wave: 2, Issue: "kv-555.4",
			Outcome: drain.OutcomeFailed, Text: "gate: go test ./... failed twice", Usage: usage(0.6610, 30000, 4000)}),
		stamped(drain.Event{Kind: drain.EventIssueEnd, At: after(30), Wave: 1, Issue: "kv-555.1",
			Outcome: drain.OutcomeInterrupted, Text: "the run was stopped", Usage: usage(0.5010, 22000, 2600)}),
		stamped(drain.Event{Kind: drain.EventIssueEnd, At: after(88), Wave: 2, Issue: "kv-555.6",
			Outcome: drain.OutcomeDone, Text: "gate and review passed first time", Usage: usage(0.5811, 34000, 4300)}))

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
			Usage:  usage(4.5218, 221000, 26700), Seconds: 344}})

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
