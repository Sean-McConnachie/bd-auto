// Package graph builds a code index the roles can query, or cleanly does
// nothing.
//
// # Why
//
// Every worker, reviewer and integrator a drain spawns starts knowing nothing
// about the repo it has been pointed at, and each one re-derives the same map
// with grep, glob and read. That is paid for in the only currency this engine
// spends: transcript size multiplied by turns, because every turn re-sends the
// whole transcript as cache reads. Drain 4's five processes billed 2,405,147
// cache-read tokens against 22,450 output tokens.
//
// # What it is, and is not
//
// plans/graph-index.md measured this before anything was designed around it,
// and the premise only half survived. `graphify extract --code-only` is pure
// AST extraction — no LLM, no API key, and 1.9s for 2199 nodes on this repo, so
// building it is free. But a broad query returns a truncated list of symbol
// locations, not an explanation: it is a typed, cross-referenced grep, and an
// agent still has to read the file. The saving is on searching, not reading.
//
// So this exists to serve four cheap, exact commands — god-nodes, explain,
// affected and path — and prompts/graph.md says as much, including that a broad
// `graphify query` can cost a thousand tokens of node list that does not contain
// the answer.
//
// # No tool server
//
// beads-auto-imp-1xg planned this as an MCP tool server as well. `graphify.serve`
// imports `mcp`, which a plain `graphify` install does not pull in and which is
// not installed here, so what ships is the index and the four CLI commands: the
// role gets one allowlist entry, Bash(graphify:*), rather than seven MCP tools.
// That also drops the reason the plan needed a shebang-resolved interpreter and
// an explicit tool list excluding the three that reach GitHub.
//
// # Off by default
//
// The value is unproven and this repo sets knobs from measurements. Turning it
// on before the A/B in beads-auto-imp-xhw would be exactly the guess the rest of
// the config avoids.
package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"bd-auto/internal/runstate"
)

// Binary is the command this looks for. Absent, there is no index and that is
// not an error: a repo without graphify installed drains exactly as it did
// before.
const Binary = "graphify"

// Dir is where the index lives, under the already-gitignored run directory so
// no worker's `git add -A` can commit a three-megabyte graph.
func Dir(repoRoot string) string { return filepath.Join(runstate.Dir(repoRoot), "graph") }

// GraphPath is the file every graphify query command takes as --graph.
func GraphPath(repoRoot string) string {
	return filepath.Join(Dir(repoRoot), "graphify-out", "graph.json")
}

// stampPath records what the index was built from, so a reader can tell a
// current index from one built three merges ago.
func stampPath(repoRoot string) string { return filepath.Join(Dir(repoRoot), "stamp.json") }

// Stamp is what the index was built from and when.
type Stamp struct {
	Commit string    `json:"commit"`
	Built  time.Time `json:"built"`
	Nodes  int       `json:"nodes,omitempty"`
	Edges  int       `json:"edges,omitempty"`
}

// Index is the result of trying to build one.
type Index struct {
	// Built is false when there is no index, for any reason. Every reason is
	// ordinary — disabled, no binary, a build that failed — and none of them is
	// worth failing a run over.
	Built bool `json:"built"`
	// Why says what happened, whether or not it worked. It is always set: a
	// silent "no index" is indistinguishable from a forgotten one.
	Why   string        `json:"why"`
	Path  string        `json:"path,omitempty"`
	Stamp *Stamp        `json:"stamp,omitempty"`
	Took  time.Duration `json:"took,omitempty"`
}

// Available reports whether the graphify binary can be found at all.
func Available() bool {
	_, err := exec.LookPath(Binary)
	return err == nil
}

// Options are the knobs Build reads. They come from the graph: config block.
type Options struct {
	// Enabled is off by default; see the package comment.
	Enabled bool
	// ExcludeTests keeps test files out of the index. Measured: without it the
	// most-connected nodes on this repo are testRepo, newIssues, testCfg and
	// engine — the test harness, not the architecture. It trades away "where is
	// this tested?" as an index question, which is the other thing a human
	// might reasonably want back.
	ExcludeTests bool
	// Timeout bounds one build. A build is seconds; anything approaching this
	// is a hang, and a hung index must not hold up a wave.
	Timeout time.Duration
}

// DefaultTimeout is generous against a measured 1.9s, because the cost of
// cutting a slow build short is only losing the index.
const DefaultTimeout = 5 * time.Minute

// Build extracts the index, or explains why it did not.
//
// It never returns an error for an absent or broken graphify. The index is an
// optimisation, and a run that cannot have one must drain exactly as it would
// have without it — the whole point of measuring first is that nothing depends
// on this working.
func Build(ctx context.Context, repoRoot string, opt Options, logf func(string, ...any)) Index {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !opt.Enabled {
		return Index{Why: "the graph index is off; set graph.enabled to turn it on"}
	}
	if !Available() {
		return Index{Why: "graphify is not on PATH, so there is no code index; the run is unaffected"}
	}

	dir := Dir(repoRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Index{Why: fmt.Sprintf("could not make %s: %v", dir, err)}
	}
	if opt.ExcludeTests {
		undo, err := useIgnore(repoRoot)
		if err != nil {
			logf("warning: could not write the graph's ignore file: %v", err)
		}
		defer undo()
	}
	defer sweepCache(repoRoot)()

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	// --code-only is what keeps this free: local AST, no API key, no model.
	// --no-cluster because only naming communities calls a model, and clustering
	// without naming buys nothing the three cheap commands read.
	//
	// No --no-gitignore. That flag turns .gitignore off in favour of
	// .graphifyignore, and .gitignore is worth keeping: it is what holds the
	// index to source rather than to build output and vendored trees.
	args := []string{"extract", repoRoot, "--code-only", "--no-cluster", "--out", dir}
	cmd := exec.CommandContext(runCtx, Binary, args...)
	// Run from the index directory, not the repository. --out moves the graph
	// but not graphify's incremental stat cache, which lands in a graphify-out/
	// directory relative to the working directory — measured: extracting from
	// the repository root left a `?? graphify-out/` in `git status`, which is
	// untracked build output sitting in a tree where every worker runs
	// `git add -A`. From here it lands beside the graph it belongs to, and the
	// cache is worth keeping: it is what makes a barrier's rebuild 295ms rather
	// than a second.
	cmd.Dir = dir
	bound(cmd)
	out, err := cmd.CombinedOutput()
	took := time.Since(started)

	if err != nil {
		why := fmt.Sprintf("graphify could not build the index (%v); the run is unaffected", err)
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			why = fmt.Sprintf("graphify did not finish within %s; the run is unaffected", timeout)
		}
		logf("%s", why)
		if tail := lastLines(string(out), 3); tail != "" {
			logf("  %s", tail)
		}
		return Index{Why: why, Took: took}
	}

	path := GraphPath(repoRoot)
	if _, err := os.Stat(path); err != nil {
		return Index{
			Why:  fmt.Sprintf("graphify reported success but wrote no graph at %s; the run is unaffected", path),
			Took: took,
		}
	}

	st := &Stamp{Commit: headCommit(repoRoot), Built: time.Now().UTC()}
	st.Nodes, st.Edges = countGraph(path)
	if err := writeStamp(repoRoot, st); err != nil {
		logf("warning: could not stamp the graph index: %v", err)
	}
	logf("code index: %d nodes, %d edges in %s", st.Nodes, st.Edges, took.Round(time.Millisecond))
	return Index{Built: true, Why: "built", Path: path, Stamp: st, Took: took}
}

// Refresh rebuilds the index after a barrier has merged, so the next wave reads
// the code as it now is rather than as it was when the run started.
//
// A full extract rather than `graphify update`, and that is a correction rather
// than a preference. `update <path>` re-extracts <path> and writes the result to
// <path>/graphify-out — it takes no --out — so with the index deliberately kept
// outside the working tree there is no path that names both the source and the
// destination. Pointed at the index directory, which is what this did first, it
// dutifully re-extracted the index directory: one run took this repo's graph
// from 2198 nodes to 1956 and reported that the only source file it had found
// was stamp.json. Left on a barrier it would erode the index toward nothing.
//
// The cost of doing it properly is the whole extraction, measured at 1.9s for
// 2199 nodes. That is cheap enough for a barrier that already runs the gate.
func Refresh(ctx context.Context, repoRoot string, opt Options, logf func(string, ...any)) Index {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	had := Read(repoRoot)
	idx := Build(ctx, repoRoot, opt, logf)
	if idx.Built {
		return idx
	}
	// A rebuild that fails leaves the previous graph where it was, which is
	// stale rather than absent — better than nothing, and the stamp says how
	// stale. Report that one rather than the failure.
	if had.Built {
		logf("the code index could not be rebuilt; the previous one still stands")
		return had
	}
	return idx
}

// Read reports the index already on disk, without building anything.
func Read(repoRoot string) Index {
	path := GraphPath(repoRoot)
	if _, err := os.Stat(path); err != nil {
		return Index{Why: "no code index has been built"}
	}
	idx := Index{Built: true, Why: "already built", Path: path}
	raw, err := os.ReadFile(stampPath(repoRoot))
	if err != nil {
		return idx
	}
	var st Stamp
	if json.Unmarshal(raw, &st) == nil {
		idx.Stamp = &st
	}
	return idx
}

// Stale reports whether the index was built from a different commit than the
// one checked out. A stale index is still useful — the symbols it names mostly
// still exist — so this is for reporting, not for refusing.
func (i Index) Stale(repoRoot string) bool {
	if !i.Built || i.Stamp == nil || i.Stamp.Commit == "" {
		return false
	}
	head := headCommit(repoRoot)
	return head != "" && head != i.Stamp.Commit
}

// ignoreFile is the exclusion list, and it goes in the repository being
// indexed rather than in the output directory.
//
// That placement is measured, not assumed. Written into --out and paired with
// --no-gitignore, which is what this did first, graphify read neither file: the
// index came back with testRepo, newIssues, testCfg and engine as its four
// most-connected nodes after Join — the test harness rather than the
// architecture, which is exactly what excluding tests is for. Moved to the
// source root with .gitignore left on, the same repo indexes to Join, Model,
// Broker, Engine, State, DrainReport, and 1241 nodes rather than 2198.
const ignoreFile = ".graphifyignore"

// ignoreMark identifies a file this wrote. A file without it belongs to the
// repository and is never touched: the repo's own exclusions are a better
// answer than ours, and overwriting them would be an edit bd-auto made to a
// working tree it was only asked to read.
const ignoreMark = "# Written by bd-auto."

// cacheDir is graphify's own output directory name, which it also uses for an
// incremental stat cache.
const cacheDir = "graphify-out"

// sweepCache removes the cache graphify leaves in the tree it indexed.
//
// --out moves the graph and most of the cache, but a graphify-out/cache/ still
// appears beside the source — measured on this repo, and only when a
// .graphifyignore is present, which is exactly the configuration this uses. It
// is untracked build output in a working tree where every worker runs
// `git add -A`, so it does not get to stay.
//
// Only when it was not there to begin with. A repository that indexes itself
// with graphify has its own graphify-out, and deleting somebody's index because
// bd-auto built one of its own would be a bad trade for the two seconds it
// saves them.
func sweepCache(repoRoot string) func() {
	path := filepath.Join(repoRoot, cacheDir)
	if _, err := os.Stat(path); err == nil {
		return func() {} // theirs
	}
	return func() { os.RemoveAll(path) }
}

// useIgnore puts the exclusion list where graphify reads it and returns the
// undo.
//
// It lives in the working tree for as long as one extraction takes, which is a
// second or two in the main checkout with no worker running — Build is called
// before the first worker is dispatched and Refresh at a barrier after the last
// one has finished. The marker is what makes a leftover from a killed run
// recoverable rather than permanent: the next run recognises its own file,
// overwrites it, and removes it on the way out.
func useIgnore(repoRoot string) (func(), error) {
	path := filepath.Join(repoRoot, ignoreFile)
	if raw, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(raw), ignoreMark) {
		return func() {}, nil // the repository's own; use it and leave it alone
	}
	body := strings.Join([]string{
		ignoreMark + " Tests are excluded so the index describes the",
		"# architecture rather than the test harness: with them in, this repo's",
		"# most-connected nodes are testRepo, newIssues, testCfg and engine.",
		"#",
		"# It is removed as soon as the extraction that needs it finishes.",
		"*_test.go",
		"testdata/",
		".beads/",
		"vendor/",
		"", // trailing newline
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return func() {}, err
	}
	return func() { os.Remove(path) }, nil
}

func writeStamp(repoRoot string, st *Stamp) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stampPath(repoRoot), append(raw, '\n'), 0o644)
}

// countGraph reads how big the index is, for the stamp. A graph it cannot parse
// reports zero rather than failing: the numbers are for a human reading the
// stamp, and nothing decides anything on them.
func countGraph(path string) (nodes, edges int) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	var g struct {
		Nodes []json.RawMessage `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	}
	if json.Unmarshal(raw, &g) != nil {
		return 0, 0
	}
	return len(g.Nodes), len(g.Edges)
}

// bound makes the timeout mean what it says.
//
// CommandContext kills the process when the context is cancelled, but
// CombinedOutput then waits for the output pipes to close — and a child that
// inherited them keeps them open after its parent is dead. Measured here: a
// graphify that shells out to something slow ignored a 200ms timeout and took
// the full 30s, which on a wave barrier is 30s of nothing happening for an
// index nobody needed.
//
// WaitDelay bounds that wait: once the process is killed the pipes get two
// seconds and then the read is abandoned. Two rather than more because this
// delay lands on a wave barrier, and losing the tail of a hung build's output
// is exactly the right thing to trade for it.
func bound(cmd *exec.Cmd) { cmd.WaitDelay = 2 * time.Second }

func headCommit(repoRoot string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n  "))
}
