package cmds

import (
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"bd-auto/internal/runstate"
)

// MergeCandidate is one worker branch waiting to be merged.
type MergeCandidate struct {
	Issue     string   `json:"issue"`
	Branch    string   `json:"branch"`
	Exists    bool     `json:"exists"`
	Commits   int      `json:"commits"`
	Worktree  string   `json:"worktree,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	Status    string   `json:"issue_status"`
}

// MergeOrder implements `bd-auto merge-order`: list the current wave's branches
// in dependency order for the integrator.
//
// Within a wave the issues are a ready front and therefore mutually
// independent, so order usually does not matter. It does after retries, when a
// requeued issue can land in a later wave than something that depends on it,
// so the ordering is computed rather than assumed.
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

	ids := st.WaveIssues
	if *all {
		seen := map[string]bool{}
		ids = nil
		for _, l := range [][]string{st.WaveIssues, st.Done} {
			for _, id := range l {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}

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

	var mergeable []MergeCandidate
	for _, m := range ordered {
		if m.Exists && m.Commits > 0 {
			mergeable = append(mergeable, m)
		}
	}

	return emitJSON(map[string]any{
		"epic":       st.Epic,
		"wave":       st.Wave,
		"candidates": ordered,
		"mergeable":  mergeable,
		"base":       currentBranch(c.RepoRoot),
	})
}

// topoOrder sorts candidates so a branch never merges before one it depends on.
// Ties break on issue ID for determinism.
func topoOrder(in []MergeCandidate) []MergeCandidate {
	inSet := map[string]bool{}
	for _, c := range in {
		inSet[c.Issue] = true
	}
	byID := map[string]MergeCandidate{}
	for _, c := range in {
		byID[c.Issue] = c
	}

	// Kahn's algorithm over the subgraph induced by this wave.
	indeg := map[string]int{}
	children := map[string][]string{}
	for _, c := range in {
		indeg[c.Issue] += 0
		for _, d := range c.DependsOn {
			if inSet[d] {
				indeg[c.Issue]++
				children[d] = append(children[d], c.Issue)
			}
		}
	}

	var ready []string
	for id, n := range indeg {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	var out []MergeCandidate
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, byID[id])
		var freed []string
		for _, ch := range children[id] {
			indeg[ch]--
			if indeg[ch] == 0 {
				freed = append(freed, ch)
			}
		}
		sort.Strings(freed)
		ready = append(ready, freed...)
		sort.Strings(ready)
	}

	// A cycle would drop issues; append any stragglers so nothing is lost.
	if len(out) < len(in) {
		seen := map[string]bool{}
		for _, o := range out {
			seen[o.Issue] = true
		}
		for _, c := range in {
			if !seen[c.Issue] {
				out = append(out, c)
			}
		}
	}
	return out
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
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
