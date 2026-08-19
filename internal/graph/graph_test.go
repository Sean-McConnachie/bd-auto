package graph

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prependPath puts a directory of fakes ahead of the real PATH, rather than
// replacing it. Replacing it takes git, sh and sleep with it, and then every
// test measures the harness instead of the code.
func prependPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.invalid"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, dir, "seed")
	return dir
}

func commit(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// The index is an optimisation. Every way of not having one is ordinary, and
// none of them may cost a run anything — which is the property that lets this
// ship enabled-by-config rather than guarded at every call site.

func TestDisabledBuildsNothingAndSaysSo(t *testing.T) {
	dir := repo(t)
	got := Build(context.Background(), dir, Options{Enabled: false}, nil)
	if got.Built {
		t.Fatal("an index was built with graph.enabled off")
	}
	if got.Why == "" {
		t.Fatal("a silent 'no index' cannot be told from a forgotten one")
	}
	if _, err := os.Stat(Dir(dir)); !os.IsNotExist(err) {
		t.Fatal("the disabled path created the index directory anyway")
	}
}

func TestAMissingGraphifyIsNotAFailure(t *testing.T) {
	dir := repo(t)
	// The system directories without ~/.local/bin: the same situation as
	// graphify not being installed, while git and sh still work.
	t.Setenv("PATH", "/usr/bin:/bin")
	got := Build(context.Background(), dir, Options{Enabled: true}, nil)
	if got.Built {
		t.Fatal("an index was built with no graphify on PATH")
	}
	if !strings.Contains(got.Why, "graphify") {
		t.Fatalf("the reason does not name what is missing: %q", got.Why)
	}
	if !strings.Contains(got.Why, "unaffected") {
		t.Fatalf("the reason does not say the run is fine: %q", got.Why)
	}
}

func TestABuildThatFailsCostsOnlyTheIndex(t *testing.T) {
	dir := repo(t)
	// A graphify that exists and always fails.
	bin := t.TempDir()
	script := filepath.Join(bin, Binary)
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'broke' >&2\nexit 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, bin)

	var logged []string
	got := Build(context.Background(), dir, Options{Enabled: true},
		func(f string, a ...any) { logged = append(logged, f) })
	if got.Built {
		t.Fatal("a failing graphify reported a built index")
	}
	if len(logged) == 0 {
		t.Fatal("a failed build said nothing at all")
	}
}

func TestATimeoutIsReportedAsOneAndDoesNotHang(t *testing.T) {
	dir := repo(t)
	bin := t.TempDir()
	script := filepath.Join(bin, Binary)
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, bin)

	started := time.Now()
	got := Build(context.Background(), dir, Options{Enabled: true, Timeout: 200 * time.Millisecond}, nil)
	if got.Built {
		t.Fatal("a hung graphify reported a built index")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the build took %s to give up on a 200ms timeout", elapsed)
	}
	if !strings.Contains(got.Why, "finish") {
		t.Fatalf("a timeout is not reported as one: %q", got.Why)
	}
}

// A graphify that exits 0 and writes nothing must not be reported as success:
// every caller downstream would then point --graph at a file that is not there.
func TestSuccessWithNoGraphIsNotSuccess(t *testing.T) {
	dir := repo(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, Binary), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, bin)

	got := Build(context.Background(), dir, Options{Enabled: true}, nil)
	if got.Built {
		t.Fatal("graphify wrote no graph and the build reported success")
	}
	if !strings.Contains(got.Why, "no graph") {
		t.Fatalf("the reason does not say what was missing: %q", got.Why)
	}
}

// fakeGraphify writes a graph the way a real extraction would, so the stamp and
// the counting can be tested without depending on graphify being installed.
func fakeGraphify(t *testing.T, srcRoot string, nodes, edges int) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out) out="$2"; shift 2 ;;
    update) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
mkdir -p "$out/graphify-out"
# Record what the source root looked like while the extraction was running.
# The ignore file is removed as soon as this returns, so afterwards is too late
# to ask whether graphify would have seen one.
[ -f "$SRC/.graphifyignore" ] && cp "$SRC/.graphifyignore" "$out/seen-ignore"
printf '{"nodes":[%s],"edges":[%s]}' "$NODES" "$EDGES" > "$out/graphify-out/graph.json"
`
	if err := os.WriteFile(filepath.Join(bin, Binary), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, bin)
	t.Setenv("SRC", srcRoot)
	t.Setenv("NODES", strings.TrimSuffix(strings.Repeat(`{},`, nodes), ","))
	t.Setenv("EDGES", strings.TrimSuffix(strings.Repeat(`{},`, edges), ","))
}

func TestABuiltIndexIsStampedWithTheCommitItDescribes(t *testing.T) {
	dir := repo(t)
	fakeGraphify(t, dir, 3, 2)

	got := Build(context.Background(), dir, Options{Enabled: true}, nil)
	if !got.Built {
		t.Fatalf("no index: %s", got.Why)
	}
	if got.Stamp == nil || got.Stamp.Commit == "" {
		t.Fatalf("the index is not stamped with a commit: %+v", got.Stamp)
	}
	if got.Stamp.Nodes != 3 || got.Stamp.Edges != 2 {
		t.Fatalf("stamp counts %d/%d, want 3/2", got.Stamp.Nodes, got.Stamp.Edges)
	}
	// And it is readable back, which is what a later wave does.
	if read := Read(dir); !read.Built || read.Stamp == nil || read.Stamp.Commit != got.Stamp.Commit {
		t.Fatalf("Read did not recover the stamp: %+v", read)
	}
}

// Stale is for reporting, not refusing: the symbols a slightly old index names
// mostly still exist, and refusing to use one would throw away most of its value
// at the moment it is most likely to be a little behind.
func TestAnIndexKnowsWhenItsCommitHasMovedOn(t *testing.T) {
	dir := repo(t)
	fakeGraphify(t, dir, 1, 0)

	got := Build(context.Background(), dir, Options{Enabled: true}, nil)
	if got.Stale(dir) {
		t.Fatal("a freshly built index reports itself stale")
	}
	if err := os.WriteFile(filepath.Join(dir, "next.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, dir, "move on")
	if !got.Stale(dir) {
		t.Fatal("the commit moved and the index does not report itself stale")
	}
}

// The exclusion list has to be in the repository being indexed, because that is
// the only place graphify reads it from. Written into the output directory
// instead — which is where this put it first — it excluded nothing at all.
func TestExcludingTestsPutsTheIgnoreFileWhereGraphifyReadsIt(t *testing.T) {
	dir := repo(t)
	fakeGraphify(t, dir, 1, 0)

	Build(context.Background(), dir, Options{Enabled: true, ExcludeTests: true}, nil)

	raw, err := os.ReadFile(filepath.Join(Dir(dir), "seen-ignore"))
	if err != nil {
		t.Fatalf("graphify saw no ignore file in the repository it was indexing: %v", err)
	}
	for _, want := range []string{"*_test.go", "testdata/"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the ignore file does not exclude %q:\n%s", want, raw)
		}
	}
}

// And it is gone again afterwards. It lives in a working tree bd-auto was asked
// to read, not to edit, and a file left behind is one a worker's `git add -A`
// would commit onto a branch.
func TestTheIgnoreFileIsRemovedAfterTheBuild(t *testing.T) {
	dir := repo(t)
	fakeGraphify(t, dir, 1, 0)

	Build(context.Background(), dir, Options{Enabled: true, ExcludeTests: true}, nil)

	if _, err := os.Stat(filepath.Join(dir, ignoreFile)); err == nil {
		t.Fatal("the ignore file was left in the working tree")
	}
}

// A repository with its own exclusions keeps them. Ours is a default for a repo
// that has not said, not an edit to one that has.
func TestARepositorysOwnIgnoreFileIsNeitherOverwrittenNorRemoved(t *testing.T) {
	dir := repo(t)
	fakeGraphify(t, dir, 1, 0)
	theirs := "# mine\n*.generated.go\n"
	if err := os.WriteFile(filepath.Join(dir, ignoreFile), []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	Build(context.Background(), dir, Options{Enabled: true, ExcludeTests: true}, nil)

	raw, err := os.ReadFile(filepath.Join(dir, ignoreFile))
	if err != nil {
		t.Fatalf("the repository's own ignore file was removed: %v", err)
	}
	if string(raw) != theirs {
		t.Fatalf("the repository's own ignore file was overwritten:\n%s", raw)
	}
}

// A run killed mid-extract leaves the file behind. The next run must recognise
// its own and clean up, or the marker would make the leftover permanent.
func TestALeftoverIgnoreFileIsReclaimed(t *testing.T) {
	dir := repo(t)
	fakeGraphify(t, dir, 1, 0)
	if err := os.WriteFile(filepath.Join(dir, ignoreFile), []byte(ignoreMark+" stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	Build(context.Background(), dir, Options{Enabled: true, ExcludeTests: true}, nil)

	if _, err := os.Stat(filepath.Join(dir, ignoreFile)); err == nil {
		t.Fatal("a leftover ignore file written by bd-auto was not cleaned up")
	}
}

// The index is three megabytes on this repo. It lives under the run directory,
// which is gitignored, so no worker's `git add -A` can commit it onto a branch.
func TestTheIndexLivesWhereNoWorkerCanCommitIt(t *testing.T) {
	dir := repo(t)
	got := Dir(dir)
	if !strings.Contains(got, filepath.Join(".beads", "auto")) {
		t.Fatalf("the index is at %s, outside the gitignored run directory", got)
	}
}

func TestRefreshFallsBackToABuildWhenThereIsNoIndexYet(t *testing.T) {
	dir := repo(t)
	fakeGraphify(t, dir, 2, 1)

	got := Refresh(context.Background(), dir, Options{Enabled: true}, nil)
	if !got.Built {
		t.Fatalf("refresh with no index did not build one: %s", got.Why)
	}
}

// A refresh that fails leaves the previous index in place: stale is better than
// absent, and the stamp says how stale.
func TestAFailedRefreshKeepsThePreviousIndex(t *testing.T) {
	dir := repo(t)
	fakeGraphify(t, dir, 5, 4)
	first := Build(context.Background(), dir, Options{Enabled: true}, nil)
	if !first.Built {
		t.Fatalf("setup: %s", first.Why)
	}

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, Binary), []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, bin)

	got := Refresh(context.Background(), dir, Options{Enabled: true}, nil)
	if !got.Built {
		t.Fatalf("a failed refresh threw away a working index: %s", got.Why)
	}
	raw, err := os.ReadFile(GraphPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var g struct{ Nodes []json.RawMessage }
	if json.Unmarshal(raw, &g) != nil || len(g.Nodes) != 5 {
		t.Fatalf("the previous index was damaged by the failed refresh")
	}
}

// graphify leaves an incremental cache in the tree it indexed. That tree is one
// bd-auto was asked to read, and one where every worker runs `git add -A`.
func TestTheCacheGraphifyLeavesInTheRepositoryIsSweptUp(t *testing.T) {
	dir := repo(t)
	fakeGraphifyLeavingCache(t, dir)

	Build(context.Background(), dir, Options{Enabled: true, ExcludeTests: true}, nil)

	if _, err := os.Stat(filepath.Join(dir, cacheDir)); err == nil {
		t.Fatal("graphify-out was left in the working tree")
	}
}

// Unless it was already there. A repository that indexes itself with graphify
// has its own graphify-out, and deleting somebody's index is a bad trade for
// the seconds it saves them.
func TestARepositorysOwnGraphifyOutIsLeftAlone(t *testing.T) {
	dir := repo(t)
	fakeGraphifyLeavingCache(t, dir)
	theirs := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(theirs, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(theirs, "graph.json")
	if err := os.WriteFile(keep, []byte(`{"nodes":[{}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	Build(context.Background(), dir, Options{Enabled: true, ExcludeTests: true}, nil)

	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the repository's own graphify index was deleted: %v", err)
	}
}

// fakeGraphifyLeavingCache behaves like the real one: it writes the graph where
// --out says, and a cache in the tree it was pointed at.
func fakeGraphifyLeavingCache(t *testing.T, srcRoot string) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
out=""
src=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out) out="$2"; shift 2 ;;
    extract) src="$2"; shift 2 ;;
    *) shift ;;
  esac
done
mkdir -p "$out/graphify-out" "$src/graphify-out/cache"
echo '{}' > "$src/graphify-out/cache/stat-index.json"
printf '{"nodes":[{}],"edges":[]}' > "$out/graphify-out/graph.json"
`
	if err := os.WriteFile(filepath.Join(bin, Binary), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, bin)
	t.Setenv("SRC", srcRoot)
}
