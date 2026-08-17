package drain

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"bd-auto/internal/gitx"
)

// A forge is where a finished run is handed over: the remote the epic branch
// goes to and the pull request opened against it.
//
// It is behind an interface for two reasons that are really one. The handoff is
// the only step of a drain that reaches the network and somebody's account, so
// it is the only step a test must be able to substitute — a test that needs a
// live remote is a test nobody runs. And a repo whose review does not happen on
// GitHub needs the same seam to put its own forge in.

// PullRequest is what a forge is asked to open.
type PullRequest struct {
	// Base is the branch being merged into, Head the branch carrying the work.
	Base string
	Head string
	// Title is one line; Body is the handoff document.
	Title string
	Body  string
}

// Forge publishes a finished run.
type Forge interface {
	// Available reports why this forge cannot be used here, or "" when it can.
	//
	// It is asked before anything is pushed. A missing CLI should cost a line of
	// explanation and a branch left in place, never a branch published to a
	// remote with no pull request to explain it.
	Available(repoRoot, remote string) string
	// Push publishes branch to remote from the main checkout.
	Push(ctx context.Context, repoRoot, remote, branch string) error
	// Open opens the pull request and returns its URL, and whether this call is
	// what created it. An existing pull request for the same head is found
	// rather than treated as a failure: the branch has just been pushed, so the
	// one already open now carries this run's work.
	Open(ctx context.Context, repoRoot string, pr PullRequest) (url string, created bool, err error)
}

// GH is the default forge: git for the push, the gh CLI for the pull request.
//
// The push runs in the main checkout, which is the one place bd-auto is allowed
// to push from. Every worker worktree has the gitguard rejectors in front of it
// and a push URL with no remote helper behind it, so a worker that tries this
// fails before it opens a connection — and its rejector message says exactly
// this: the integrator owns every push.
type GH struct{}

// ghMissing and remoteMissing are the two reasons a repo cannot reach a forge,
// and both are ordinary rather than exceptional: a repo with no remote is a
// perfectly good repo to drain, it just cannot be handed over this way.
func (GH) Available(repoRoot, remote string) string {
	if _, err := exec.LookPath("gh"); err != nil {
		return "the gh CLI is not on PATH, so there is nothing to open a pull request with"
	}
	if _, err := git(repoRoot, "remote", "get-url", remote); err != nil {
		return "this repo has no remote called " + remote
	}
	return ""
}

// Push publishes the staged branch. It fires no hooks: beads' pre-push runs a
// bd hook, and bd-auto's git goes through internal/gitx everywhere so that no
// git operation it starts can import .beads/issues.jsonl over the run's work.
func (GH) Push(ctx context.Context, repoRoot, remote, branch string) error {
	_, err := runCmd(ctx, repoRoot, "git",
		gitx.Args([]string{"push", "--set-upstream", remote, branch})...)
	return err
}

func (GH) Open(ctx context.Context, repoRoot string, pr PullRequest) (string, bool, error) {
	// --body-file rather than --body: a body is long, contains newlines and
	// backticks, and an argv large enough to hold one is an argv that eventually
	// will not fit.
	body, err := os.CreateTemp("", "bd-auto-pr-*.md")
	if err != nil {
		return "", false, fmt.Errorf("write the pull request body: %w", err)
	}
	path := body.Name()
	defer os.Remove(path)
	if _, err := body.WriteString(pr.Body); err != nil {
		body.Close()
		return "", false, fmt.Errorf("write the pull request body: %w", err)
	}
	if err := body.Close(); err != nil {
		return "", false, fmt.Errorf("write the pull request body: %w", err)
	}

	out, err := runCmd(ctx, repoRoot, "gh", "pr", "create",
		"--base", pr.Base, "--head", pr.Head, "--title", pr.Title, "--body-file", path)
	if err == nil {
		return firstURL(out), true, nil
	}
	if existing, verr := runCmd(ctx, repoRoot, "gh", "pr", "view", pr.Head, "--json", "url", "--jq", ".url"); verr == nil {
		if u := firstURL(existing); u != "" {
			return u, false, nil
		}
	}
	return "", false, err
}

// forge resolves the engine's forge, defaulting to gh.
func (e *Engine) forge() Forge {
	if e.Forge != nil {
		return e.Forge
	}
	return GH{}
}

// firstURL picks the URL out of a command's output. gh prints the pull request
// URL on its own line, around whatever advisory noise it feels like adding.
func firstURL(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return ""
}

// runCmd runs a command in dir and returns its trimmed stdout, folding stderr
// into the error so a failure says what the tool actually complained about.
func runCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
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
		return out, fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, msg)
	}
	return out, nil
}
