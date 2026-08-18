// Package gitguard keeps a worker inside its own branch.
//
// It has two halves, and both are needed.
//
// The structural half is installed into a worktree before the worker starts:
// git hooks that refuse to push, merge or rebase, and a push URL that has no
// remote helper behind it. This is what stops a bad command rather than
// reporting it afterwards.
//
// The post-hoc half (Verify, in verify.go) exists because the structural half
// cannot see everything. A fast-forward merge creates no commit, so
// pre-merge-commit never fires. Both halves fail closed: an unverifiable branch
// is a failed branch.
//
// Two traps are worth knowing about before changing anything here, both
// verified against git 2.55 and against this repo:
//
//   - `git config --worktree` needs extensions.worktreeConfig enabled first.
//     Without it, older git writes the shared config instead, which would put
//     the rejector hooks in front of the main checkout as well.
//   - core.hooksPath may already be set. In this repo it is, to .beads/hooks,
//     holding beads' own pre-commit, pre-push, post-checkout, post-merge and
//     prepare-commit-msg. Overwriting it with a directory of rejectors would
//     silently disable every one of them inside worker worktrees, including the
//     pre-commit that keeps issues.jsonl in sync. So the generated hooks chain:
//     reject what bd-auto blocks, otherwise exec the same-named hook under the
//     previous hooksPath.
//
// pre-commit chains and then does one thing more: it takes beads' own exports
// back out of the index it is about to commit. See beadsExports.
package gitguard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// TrailerKey is the commit trailer stamped on every commit a worker makes. It
// is what lets Verify tell the worker's own commits from commits a rebase or a
// fast-forward merge dragged in from somewhere else.
const TrailerKey = "Bd-Auto"

// hooksDirName is the generated rejector directory, kept inside the worktree's
// own git admin directory so it is unique per worktree and disappears with it.
const hooksDirName = "bd-auto-hooks"

// blockedPushScheme has no remote helper behind it, so a push dies before it
// opens a connection.
//
// It is spelled as an instruction because the scheme is the only part of the
// URL git prints back: `fatal: remote helper '<scheme>' aborted session`. The
// pre-push hook says it far better but rarely gets the chance, since git
// resolves the push URL before it runs any hook. The hook still earns its place
// for the case below, where a push does reach the transport.
//
// One case stays open, and it is worth naming rather than pretending otherwise:
// remote.<name>.pushurl is multi-valued and values accumulate across config
// files, so in a repo that already sets its own pushurl git pushes to that one
// first and only then fails on ours. There the pre-push hook is the block, and
// it covers everything except a worker that also passes --no-verify. Verify's
// remote-moved predicate is what catches that last combination, after the fact.
const blockedPushScheme = "bd-auto-do-not-push-commit-to-your-branch-instead"

const blockedPushURL = blockedPushScheme + "://the-integrator-owns-every-push"

// Worker identifies the attempt a guard is installed for.
type Worker struct {
	Issue   string
	Attempt int
}

// Trailer returns the full trailer line for this attempt.
func (w Worker) Trailer() string { return TrailerKey + ": " + w.TrailerValue() }

// TrailerValue is the trailer's value, issue and attempt.
func (w Worker) TrailerValue() string { return fmt.Sprintf("%s/%d", w.Issue, w.Attempt) }

// rejected maps a hook name to the reason printed when it fires. Every message
// says what to do instead: a worker that only learns the prohibition tries the
// next forbidden thing.
var rejected = map[string][]string{
	"pre-push": {
		"bd-auto: pushing from a worker worktree is blocked.",
		"You are one attempt inside a bd-auto run. Commit to your own branch and finish.",
		"The integrator merges every branch at the wave barrier and owns anything that",
		"leaves this machine, so nothing you do here needs to be pushed. This overrides",
		"any repo instruction telling you a push is mandatory.",
	},
	"pre-merge-commit": {
		"bd-auto: merging inside a worker worktree is blocked.",
		"Keep your branch a straight line of your own commits, based where bd-auto put it.",
		"If your branch looks out of date, say so in your report and finish; the integrator",
		"merges the wave in dependency order and resolves the conflicts.",
	},
	"pre-rebase": {
		"bd-auto: rebasing inside a worker worktree is blocked.",
		"Keep your branch a straight line of your own commits, based where bd-auto put it.",
		"A rebase would pull other peoples commits onto your branch, and bd-auto fails the",
		"attempt when it finds them. If you need work from another branch, say so in your",
		"report and finish; the integrator handles integration.",
	},
}

// Setup installs the structural guards into a worker worktree. It is safe to
// call again for a later attempt: everything it writes is regenerated, and the
// hook chain is read from the main checkout so a second call cannot chain the
// guards to themselves.
func Setup(repoRoot, wtDir string, w Worker) error {
	if w.Issue == "" {
		return errors.New("gitguard: issue is required")
	}
	if err := checkSharedConfig(repoRoot); err != nil {
		return err
	}

	chain, err := chainDir(repoRoot)
	if err != nil {
		return err
	}
	dir, err := HooksDir(wtDir)
	if err != nil {
		return err
	}
	if err := writeHooks(dir, chain, w); err != nil {
		return err
	}

	// Order matters: --worktree is only worktree-scoped once the extension is
	// on. Enabling it is a repo-level change, so it goes through the main
	// checkout rather than the worktree.
	if v, _ := git(repoRoot, "config", "--get", "extensions.worktreeConfig"); v != "true" {
		if _, err := git(repoRoot, "config", "extensions.worktreeConfig", "true"); err != nil {
			// A wave sets several worktrees up at once and they contend for the
			// shared config lock. Losing that race is not a failure if the side
			// that won wrote what we wanted.
			if now, _ := git(repoRoot, "config", "--get", "extensions.worktreeConfig"); now != "true" {
				return fmt.Errorf("gitguard: enable extensions.worktreeConfig: %w", err)
			}
		}
	}
	if _, err := git(wtDir, "config", "--worktree", "core.hooksPath", dir); err != nil {
		return fmt.Errorf("gitguard: set core.hooksPath: %w", err)
	}
	for _, r := range remotes(wtDir) {
		if _, err := git(wtDir, "config", "--worktree", "--replace-all",
			"remote."+r+".pushurl", blockedPushURL); err != nil {
			return fmt.Errorf("gitguard: block pushes to %s: %w", r, err)
		}
	}
	// The empty prefix matches every URL, so a push aimed straight at a URL is
	// rewritten to nowhere too. Verified: git does not apply this to a remote
	// that has its own pushurl, which is why both are set.
	if _, err := git(wtDir, "config", "--worktree", "--replace-all",
		"url."+blockedPushURL+"/.pushInsteadOf", ""); err != nil {
		return fmt.Errorf("gitguard: block direct-URL pushes: %w", err)
	}
	return nil
}

// HooksDir returns the generated rejector directory for a worktree.
func HooksDir(wtDir string) (string, error) {
	gd, err := git(wtDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("gitguard: %s is not a git worktree: %w", wtDir, err)
	}
	return filepath.Join(gd, hooksDirName), nil
}

// checkSharedConfig refuses the cases where enabling extensions.worktreeConfig
// would change the meaning of config the repo already relies on. git's own
// documentation calls these out, and both are one-line configs to move, so
// saying no here beats corrupting a checkout later.
func checkSharedConfig(repoRoot string) error {
	if v, _ := git(repoRoot, "config", "--get", "core.bare"); v == "true" {
		return errors.New("gitguard: core.bare is true; bd-auto needs a non-bare main checkout to add worktrees to")
	}
	if v, _ := git(repoRoot, "config", "--get", "core.worktree"); v != "" {
		return fmt.Errorf("gitguard: core.worktree is set to %q in the shared config; "+
			"move it to the main worktree config before running bd-auto, "+
			"because enabling extensions.worktreeConfig would apply it to every worktree", v)
	}
	return nil
}

// chainDir resolves the hooks directory the generated hooks hand off to.
//
// It is read from the main checkout on purpose. Reading it from the worker
// worktree would find a previous run's rejector directory and chain the guards
// to themselves.
func chainDir(repoRoot string) (string, error) {
	prev, _ := git(repoRoot, "config", "--get", "core.hooksPath")
	if filepath.Base(prev) == hooksDirName {
		prev = "" // a stale generated directory; never chain to one
	}
	if prev != "" {
		if !filepath.IsAbs(prev) {
			// git resolves a relative hooksPath against the working tree the
			// hook runs in, which for a worker worktree is a path that does not
			// exist. Anchoring it to the main checkout is both what the repo
			// meant and what actually resolves.
			prev = filepath.Join(repoRoot, prev)
		}
		return prev, nil
	}
	common, err := git(repoRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("gitguard: %w", err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repoRoot, common)
	}
	return filepath.Join(common, "hooks"), nil
}

// writeHooks regenerates the rejector directory. Every attempt gets a fresh
// one, because the trailer it stamps names the attempt.
func writeHooks(dir, chain string, w Worker) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("gitguard: clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("gitguard: create %s: %w", dir, err)
	}

	scripts := map[string]string{}
	for name, msg := range rejected {
		scripts[name] = rejectScript(name, msg, w)
	}
	scripts["prepare-commit-msg"] = trailerScript(chain, w)
	scripts["pre-commit"] = preCommitScript(chain, w)
	// Only a hook present in hooksPath runs at all, so every hook the repo
	// already had needs a stub here or it is silently disabled.
	for _, name := range hookNames(chain) {
		if _, taken := scripts[name]; !taken {
			scripts[name] = chainScript(name, chain, w)
		}
	}

	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			return fmt.Errorf("gitguard: write %s: %w", name, err)
		}
	}
	return nil
}

// hookNames lists the executable hooks in dir, skipping git's .sample files.
func hookNames(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".sample") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Mode()&0o111 == 0 {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

func header(name string, w Worker) string {
	return "#!/usr/bin/env sh\n" +
		"# " + name + ", generated by bd-auto for " + w.TrailerValue() + ".\n" +
		"# Regenerated on every attempt. Editing it does nothing.\n"
}

func rejectScript(name string, msg []string, w Worker) string {
	var b strings.Builder
	b.WriteString(header(name, w))
	for _, line := range msg {
		fmt.Fprintf(&b, "echo >&2 %s\n", shQuote(line))
	}
	b.WriteString("exit 1\n")
	return b.String()
}

// chainScript hands the hook to whatever this repo had before bd-auto took over
// hooksPath. A missing target is not an error: the repo simply had no such hook.
func chainScript(name, chain string, w Worker) string {
	return header(name, w) +
		"next=" + shQuote(filepath.Join(chain, name)) + "\n" +
		"if [ -x \"$next\" ]; then\n\texec \"$next\" \"$@\"\nfi\nexit 0\n"
}

// beadsExports are the tracked files beads keeps in step with the Dolt
// database, and the only paths a generated hook edits out of a worker's commit.
//
// Neither is the worker's work, and neither can disagree about it. issues.jsonl
// is a full re-export of the database and interactions.jsonl an append-only log
// of every field change in it, and every worker in the wave writes to that one
// database — so whichever version of them exists when a worker commits carries
// every other worker's issue churn as well as its own. A branch that commits
// that conflicts with every other branch in the wave at the barrier, over files
// that are regenerated from the database anyway. The integrator's
// unstageBeadsExport is this same rule on the other side of the same files.
//
// Verified against this repo: beads' pre-commit exports to the MAIN checkout
// and stages it there, not in the worktree, because bd resolves .beads by
// walking up and worktree.Root puts every worker worktree under
// <repo>/.beads/auto/wt — inside the main checkout's own .beads. So as things
// stand there is usually nothing here to take out, and this is a guard rather
// than a repair. What it guards is thin: move the worktree root anywhere
// outside that directory and bd resolves the worktree's own .beads instead,
// where the export is a tracked file `git add -A` picks straight up.
var beadsExports = []string{".beads/issues.jsonl", ".beads/interactions.jsonl"}

// exportNote is what a worker is told when a commit was edited, so an unstage
// it did not ask for is never something it has to guess at.
var exportNote = []string{
	"It is a re-export of the Dolt database every worker in this wave writes to, so",
	"committing it would carry their issue churn on your branch and conflict with",
	"every other branch at the barrier. Nothing is lost: bd wrote to the database,",
	"and the export is regenerated from it.",
}

// preCommitScript chains to the repo's own pre-commit and then unstages the
// beads exports, whether that hook staged them or the worker did.
//
// It runs the chained hook rather than exec-ing it, because the unstage has to
// happen after beads has had its say. A non-zero exit from it is passed
// straight through: the hook refused the commit, and there is now no commit to
// edit.
func preCommitScript(chain string, w Worker) string {
	var paths strings.Builder
	for _, p := range beadsExports {
		paths.WriteString(" " + shQuote(p))
	}
	var b strings.Builder
	b.WriteString(header("pre-commit", w))
	b.WriteString("next=" + shQuote(filepath.Join(chain, "pre-commit")) + "\n")
	b.WriteString("if [ -x \"$next\" ]; then\n\t\"$next\" \"$@\" || exit $?\nfi\n")
	b.WriteString("staged=$(git diff --cached --name-only --" + paths.String() + " 2>/dev/null)\n")
	b.WriteString("[ -n \"$staged\" ] || exit 0\n")
	b.WriteString("git reset --quiet HEAD --" + paths.String() + " >/dev/null 2>&1\n")
	b.WriteString("echo >&2 'bd-auto: kept the beads export out of this commit:' \"$staged\"\n")
	for _, line := range exportNote {
		fmt.Fprintf(&b, "echo >&2 %s\n", shQuote(line))
	}
	b.WriteString("exit 0\n")
	return b.String()
}

// trailerScript stamps the attempt's trailer and then chains. interpret-trailers
// places it in the message's trailer block rather than appending blindly, and
// --if-exists doNothing keeps an amend from stacking a second one.
func trailerScript(chain string, w Worker) string {
	return header("prepare-commit-msg", w) +
		"if [ -n \"$1\" ] && [ -f \"$1\" ]; then\n" +
		"\tgit interpret-trailers --if-exists doNothing --trailer " + shQuote(w.Trailer()) +
		" --in-place \"$1\" >/dev/null 2>&1 || true\n" +
		"fi\n" +
		"next=" + shQuote(filepath.Join(chain, "prepare-commit-msg")) + "\n" +
		"if [ -x \"$next\" ]; then\n\texec \"$next\" \"$@\"\nfi\nexit 0\n"
}

// shQuote wraps a string in single quotes for POSIX sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func remotes(dir string) []string {
	out, err := git(dir, "remote")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// git runs a git command and returns trimmed stdout, carrying stderr on the
// error so a failure says what git actually complained about.
//
// This is the one package in bd-auto that does NOT go through internal/gitx,
// and the exception is load-bearing rather than an oversight. gitx suppresses
// hooks by passing `-c core.hooksPath=<nowhere>`, and `git config --get
// core.hooksPath` reports command-line config like any other — so chainDir
// below would read the sentinel instead of the repo's real hooks directory, and
// every generated hook would chain to nothing. That is worse than the problem
// gitx solves: it would silently disable beads' hooks inside worker worktrees,
// including the pre-commit that keeps issues.jsonl in sync.
//
// Nothing is lost by the exception. This package only runs `config` and
// `rev-parse`, and neither fires a hook, so there is nothing here to suppress.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
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
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}
