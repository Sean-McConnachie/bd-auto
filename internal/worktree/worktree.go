// Package worktree owns the per-issue git worktrees a bd-auto run works in.
//
// Worktree creation used to be Claude Code's job, via `isolation: worktree` in
// the agent frontmatter. That made the plugin load-bearing for something the
// engine cannot run without, so Go owns the whole lifecycle here: create,
// reuse, prune, remove.
//
// The path is a pure function of the issue ID, and that is the point rather
// than a convenience. A backend resolves a resumable session against the
// project derived from the process working directory, so a worktree that moves
// between rounds is a session that can no longer be resumed. Reuse the path,
// keep the session.
package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"bd-auto/internal/gitx"
)

// registry serialises everything that reads or writes .git/worktrees/.
//
// A wave dispatches its workers at once and every one of them starts by
// creating a worktree, so these run concurrently by design. git does not:
// `worktree add` scans the registry as it goes, and a second add that reads an
// entry another one has made a directory for but not yet finished writing dies
// with `failed to read .git/worktrees/<id>/commondir: Success`. Observed as a
// wave losing an issue to worktree creation for no reason a human could act on.
//
// A process-wide mutex is the right granularity: one drain process owns
// .beads/auto/wt, and the operations under it are all short git calls.
var registry sync.Mutex

// dirName is the directory under .beads/auto/ that holds every worker worktree.
// .beads/auto/ is already gitignored, so the trees never show up as untracked
// files in the main checkout.
const dirName = "wt"

// Root returns the directory holding this repo's worker worktrees.
func Root(repoRoot string) string {
	return filepath.Join(repoRoot, ".beads", "auto", dirName)
}

// Path returns the worktree directory for an issue.
func Path(repoRoot, issue string) string {
	return filepath.Join(Root(repoRoot), safeName(issue))
}

// safeName keeps an issue ID from escaping the worktree root. bd IDs are
// already path-safe, so this is a guard rather than a transformation.
func safeName(issue string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, issue)
	s = strings.TrimLeft(s, ".")
	if s == "" {
		return "issue"
	}
	return s
}

// Entry is one registered worktree.
type Entry struct {
	Path   string
	Branch string // empty when detached
}

// Ensure returns the worktree for issue, creating it if it is not there and
// reusing it if it is.
//
// base is the commit a new branch starts from; empty means HEAD. An existing
// branch is never moved: on a later round or a resumed run the branch already
// carries the attempt's commits, and resetting it to base would throw them
// away. Discarding work is discardAttempt's job, and it happens between
// attempts, not between rounds.
func Ensure(repoRoot, issue, branch, base string) (string, error) {
	if issue == "" {
		return "", errors.New("worktree: issue is required")
	}
	if branch == "" {
		return "", errors.New("worktree: branch is required")
	}
	if base == "" {
		base = "HEAD"
	}
	registry.Lock()
	defer registry.Unlock()

	path := Path(repoRoot, issue)
	if err := os.MkdirAll(Root(repoRoot), 0o755); err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	// A branch git already has checked out elsewhere cannot be adopted, and
	// finding that out after tearing this worktree down would cost the attempt
	// the work it was holding.
	if wt := list(repoRoot)[branch]; wt != "" && !samePath(wt, path) {
		return "", fmt.Errorf("worktree: branch %s is already checked out in %s", branch, wt)
	}

	e, registered := lookupPath(repoRoot, path)
	if registered && !exists(path) {
		// A registration whose directory is gone still occupies the path, and
		// `git worktree add` refuses it. Pruning is narrowed to this case on
		// purpose: prune is repo-wide, and a wave runs several Ensure calls at
		// once, so it is not something to do speculatively.
		_ = prune(repoRoot)
		registered = false
	}

	if registered {
		if e.Branch == branch && gitx.BranchExists(repoRoot, branch) {
			return path, nil
		}
		// Adopt-or-recreate. The common case is "worktree exists, branch does
		// not": an interrupted run, or a branch deleted underneath a tree that
		// still holds the commits.
		if err := adopt(repoRoot, path, branch, base); err == nil {
			return path, nil
		}
		if err := remove(repoRoot, path); err != nil {
			return "", err
		}
	}

	// Present but unregistered: the leftovers of an interrupted run. Everything
	// under Root is bd-auto's, so clearing it is safe.
	if exists(path) {
		if err := os.RemoveAll(path); err != nil {
			return "", fmt.Errorf("worktree: clear %s: %w", path, err)
		}
	}

	args := []string{"worktree", "add", "--quiet"}
	if gitx.BranchExists(repoRoot, branch) {
		args = append(args, path, branch)
	} else {
		args = append(args, "-b", branch, path, base)
	}
	if _, err := git(repoRoot, args...); err != nil {
		return "", fmt.Errorf("worktree: create %s: %w", path, err)
	}
	return path, nil
}

// adopt re-points an existing worktree at branch without discarding what it
// holds.
func adopt(repoRoot, path, branch, base string) error {
	if gitx.BranchExists(repoRoot, branch) {
		_, err := git(path, "checkout", "--quiet", branch)
		return err
	}
	// Prefer the tree's own HEAD over base: a deleted branch usually leaves its
	// commits reachable from HEAD, and inheriting them keeps the attempt's work.
	start := base
	if head, err := git(path, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil && head != "" {
		start = head
	}
	_, err := git(path, "checkout", "--quiet", "-b", branch, start)
	return err
}

// Prune drops registrations whose directory is gone. Called at drain start, and
// again before every Ensure, because a stale registration is indistinguishable
// from a live one until git is asked.
func Prune(repoRoot string) error {
	registry.Lock()
	defer registry.Unlock()
	return prune(repoRoot)
}

func prune(repoRoot string) error {
	_, err := git(repoRoot, "worktree", "prune")
	return err
}

// Remove deletes an issue's worktree. It is deliberately not called between
// rounds: wiping the tree is what makes a resumed session pointless. Call it
// between attempts and after a successful merge.
func Remove(repoRoot, issue string) error {
	registry.Lock()
	defer registry.Unlock()
	return remove(repoRoot, Path(repoRoot, issue))
}

func remove(repoRoot, path string) error {
	if _, ok := lookupPath(repoRoot, path); ok {
		if _, err := git(repoRoot, "worktree", "remove", "--force", path); err == nil {
			return nil // git took the directory and the registration with it
		}
		// git would not let go of it. Clear the directory ourselves, then prune,
		// or the path stays unusable for the next attempt.
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("worktree: remove %s: %w", path, err)
		}
		return prune(repoRoot)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("worktree: remove %s: %w", path, err)
	}
	return nil
}

// exists reports whether a path is present at all.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// List maps branch name to worktree path.
func List(repoRoot string) map[string]string {
	registry.Lock()
	defer registry.Unlock()
	return list(repoRoot)
}

func list(repoRoot string) map[string]string {
	out := map[string]string{}
	for _, e := range entries(repoRoot) {
		if e.Branch != "" {
			out[e.Branch] = e.Path
		}
	}
	return out
}

// Entries returns every registered worktree, main checkout included.
func Entries(repoRoot string) []Entry {
	registry.Lock()
	defer registry.Unlock()
	return entries(repoRoot)
}

func entries(repoRoot string) []Entry {
	out, err := git(repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var res []Entry
	var cur Entry
	flush := func() {
		if cur.Path != "" {
			res = append(res, cur)
		}
		cur = Entry{}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return res
}

// lookupPath finds a registered worktree by path. The caller holds registry.
func lookupPath(repoRoot, path string) (Entry, bool) {
	for _, e := range entries(repoRoot) {
		if samePath(e.Path, path) {
			return e, true
		}
	}
	return Entry{}, false
}

// samePath compares two paths git and Go may spell differently. Symlinked temp
// directories are the usual reason they disagree.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// Mark is a cheap fingerprint of a worktree: the branch tip plus a digest of
// the working-tree status.
//
// It exists for the no-progress check. A round that ends with the same tip and
// the same dirty set did nothing, and every check downstream of it is
// satisfiable by the previous round's state, so the round has to be caught
// here rather than allowed to pass.
type Mark struct {
	Head   string `json:"head"`
	Status string `json:"status"`
}

// Snapshot fingerprints a worktree. An unreadable tree yields a zero Mark,
// which compares unequal to any real one, so the caller sees "changed" rather
// than a silent pass.
func Snapshot(dir string) Mark {
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return Mark{}
	}
	status, err := git(dir, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Mark{}
	}
	sum := sha256.Sum256([]byte(status))
	return Mark{Head: head, Status: hex.EncodeToString(sum[:8])}
}

// Changed reports whether the worktree moved since m was taken.
func Changed(dir string, m Mark) bool {
	return Snapshot(dir) != m
}

// git runs a git command and returns trimmed stdout. Failures carry stderr, so
// a broken worktree operation says what git actually complained about.
//
// It fires no hooks. `git worktree add` is a checkout, and beads' post-checkout
// hook imports .beads/issues.jsonl over its database — so creating an attempt's
// worktree used to revert every bd write the run had made since the last
// export. See internal/gitx.
func git(dir string, args ...string) (string, error) {
	return gitx.Run(dir, args...)
}
