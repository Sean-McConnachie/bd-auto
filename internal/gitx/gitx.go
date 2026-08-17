// Package gitx runs git the way bd-auto has to run it: with the repository's
// hooks suppressed.
//
// # Why every git call in bd-auto goes through here
//
// beads sets core.hooksPath to .beads/hooks and installs post-checkout and
// post-merge hooks that run `bd hooks run <hook>`, which imports
// .beads/issues.jsonl over the Dolt database. The jsonl is a passive export, so
// that import replays whatever state was exported onto whatever state the
// database is actually in — and every bd write made since that export is
// reverted, silently, with a zero exit code.
//
// That is fine for a human who checks out a branch and wants the issues that
// belong to it. It is not fine for bd-auto, which creates a worktree per
// attempt and merges a branch per issue at every barrier. Each of those fires
// the hook, and each one throws away the closes, the parks and the notes the
// run has written since the last export. Observed live in this repo: a
// `git pull --rebase` in the main checkout reverted eight issues from closed
// back to open, which is how a drain reaches its last wave with an epic full of
// work that was done and no longer says so.
//
// The fix is narrow on purpose. bd-auto suppresses hooks for the git commands
// bd-auto itself runs, and for nothing else: `-c` binds to one invocation, so a
// worker's own `git commit` in its worktree still fires gitguard's hooks, and a
// human's git in the same repo is untouched. bd-auto is not entitled to disable
// a repository's hooks — only to decline to fire them on its own behalf.
//
// The hooks are beads' and stay beads'; run state is bd-auto's own copy and
// survives regardless. What this protects is the state a human reads out of bd
// when the run is over.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// NoHooks is the core.hooksPath every bd-auto git invocation overrides to.
//
// Any path without hooks in it would do, including the empty string. It is
// spelled as a sentence because it is the one part of this that can surface to
// a human: a git error naming the path should explain itself without needing
// this file.
const NoHooks = "/nonexistent/bd-auto-runs-git-without-hooks"

// Cmd builds a git command that fires no hooks, to be run in dir.
//
// Use it where the caller needs the *exec.Cmd itself — a context, a custom
// environment, streamed output. Everything else wants Run.
func Cmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", Args(args)...)
	cmd.Dir = dir
	return cmd
}

// CmdContext is Cmd bound to a context, for the git commands a run can be
// interrupted in the middle of.
func CmdContext(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", Args(args)...)
	cmd.Dir = dir
	return cmd
}

// Args prefixes a git argument list with the hooks override.
//
// Exported for the callers that build their own *exec.Cmd — a shared command
// runner that takes a program name and an argv, say — and would otherwise have
// to spell the override themselves and get it wrong once.
func Args(args []string) []string {
	full := make([]string, 0, len(args)+2)
	full = append(full, "-c", "core.hooksPath="+NoHooks)
	return append(full, args...)
}

// Run executes a hook-free git command in dir and returns trimmed stdout.
//
// A failure carries stderr, so a broken git operation says what git actually
// complained about rather than only that it exited non-zero. The command is
// reported without the hooks override, which is bd-auto's business and not part
// of what the caller asked for.
func Run(dir string, args ...string) (string, error) {
	cmd := Cmd(dir, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = out
		}
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}
