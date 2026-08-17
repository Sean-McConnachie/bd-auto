package cmds

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"bd-auto/internal/gitx"
	"bd-auto/internal/runstate"
	"bd-auto/internal/wave"
)

// MergeCandidate is one worker branch waiting to be merged.
type MergeCandidate = wave.Candidate

// MergeOrder implements `bd-auto merge-order`: list the current wave's branches
// in dependency order for the integrator.
//
// The ordering itself lives in internal/wave; this gathers the git and bd facts
// each candidate needs and emits JSON.
func MergeOrder(args []string) error {
	fs := flag.NewFlagSet("merge-order", flag.ContinueOnError)
	all := fs.Bool("all", false, "consider every branch from the run, not just the current wave")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}
	st, err := c.State()
	if errors.Is(err, runstate.ErrNoRun) {
		return errors.New("no active run")
	}
	if err != nil {
		return err
	}

	ids := wave.CandidateIDs(st, *all)

	worktrees := listWorktrees(c.RepoRoot)

	var cands []MergeCandidate
	for _, id := range ids {
		branch := c.Cfg.Branch(id)
		mc := MergeCandidate{Issue: id, Branch: branch}
		mc.Exists = branchExists(c.RepoRoot, branch)
		if mc.Exists {
			mc.Commits = commitsAhead(c.RepoRoot, branch)
		}
		mc.Worktree = worktrees[branch]
		if iss, err := c.BD.Show(id); err == nil {
			mc.Status = iss.Status
			for _, d := range iss.Dependencies {
				mc.DependsOn = append(mc.DependsOn, d.ID)
			}
		}
		cands = append(cands, mc)
	}

	ordered := topoOrder(cands)

	return emitJSON(map[string]any{
		"epic":       st.Epic,
		"wave":       st.Wave,
		"candidates": ordered,
		"mergeable":  wave.Mergeable(ordered),
		"base":       currentBranch(c.RepoRoot),
	})
}

// topoOrder sorts candidates so a branch never merges before one it depends on.
func topoOrder(in []MergeCandidate) []MergeCandidate { return wave.Order(in) }

// git runs a git command in dir. It fires no hooks: beads' post-checkout and
// post-merge hooks import .beads/issues.jsonl over its database, reverting
// whatever a run has written since. See internal/gitx.
func git(dir string, args ...string) (string, error) {
	return gitx.Run(dir, args...)
}

func branchExists(dir, branch string) bool {
	_, err := git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func commitsAhead(dir, branch string) int {
	base := currentBranch(dir)
	out, err := git(dir, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(out, "%d", &n)
	return n
}

func currentBranch(dir string) string {
	out, err := git(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out == "" {
		return "main"
	}
	return out
}

// listWorktrees maps branch name to worktree path, so the integrator can clean
// up a worktree once its branch has merged.
func listWorktrees(dir string) map[string]string {
	out, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	res := map[string]string{}
	var path string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			res[strings.TrimPrefix(ref, "refs/heads/")] = path
		}
	}
	return res
}
