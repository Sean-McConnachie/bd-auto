package cmds

import (
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"bd-auto/internal/runstate"
)

// Worker implements `bd-auto worker <done|fail|status>`: the bookkeeping for an
// issue whose worker has finished, exposed as commands so a run driven by hand
// records outcomes the same way a drain does.
//
// This is where the failure policy lives: retry once with a fresh worker, then
// park and keep going. A single bad issue must never stall the drain.
func Worker(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bd-auto worker <done|fail|status>")
	}
	switch args[0] {
	case "done":
		return workerDone(args[1:])
	case "fail":
		return workerFail(args[1:])
	case "status":
		return workerStatus(args[1:])
	default:
		return fmt.Errorf("unknown worker subcommand %q", args[0])
	}
}

func workerDone(args []string) error {
	fs := flag.NewFlagSet("worker done", flag.ContinueOnError)
	issue := fs.String("issue", "", "issue ID (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *issue == "" {
		return errors.New("--issue is required")
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}

	// Trust but verify: the issue must actually be closed.
	iss, err := c.BD.Show(*issue)
	if err != nil {
		return err
	}
	if !iss.Closed() {
		return fmt.Errorf("%s is %q, not closed; close it before recording it done", *issue, iss.Status)
	}

	st, err := runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
		s.MarkDone(*issue)
		s.Note("%s done", *issue)
		return nil
	})
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{
		"issue": *issue, "recorded": "done",
		"remaining_in_flight": st.Remaining(), "done_count": len(st.Done),
	})
}

func workerFail(args []string) error {
	fs := flag.NewFlagSet("worker fail", flag.ContinueOnError)
	issue := fs.String("issue", "", "issue ID (required)")
	stage := fs.String("stage", "", "stage that failed (implement, gate, review, ...)")
	reason := fs.String("reason", "", "what went wrong (required)")
	keepBranch := fs.Bool("keep-branch", false, "do not delete the failed attempt's branch and worktree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *issue == "" || *reason == "" {
		return errors.New("--issue and --reason are required")
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

	attempts := st.Attempts[*issue]
	allowed := st.Retry + 1 // first attempt plus retries
	willRetry := attempts < allowed

	// Record the attempt on the issue itself, so the next worker starts
	// informed and the evidence outlives any context window.
	//
	// Safe to write here: this runs only after the worker has finished, which
	// is what keeps us inside the one-writer-per-issue rule the spike imposed.
	note := fmt.Sprintf("bd-auto attempt %d/%d failed at stage %q on %s:\n%s",
		attempts, allowed, stageOr(*stage), time.Now().UTC().Format(time.RFC3339), *reason)
	if err := c.BD.AppendNotes(*issue, note); err != nil {
		info("warning: could not append notes to %s: %v", *issue, err)
	}

	branch := c.Cfg.Branch(*issue)
	if !*keepBranch {
		if err := discardAttempt(c.RepoRoot, branch); err != nil {
			info("warning: could not discard attempt branch %s: %v", branch, err)
		}
	}

	if willRetry {
		_, err = runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
			delete(s.InFlight, *issue)
			s.Note("%s failed at %s, will retry (attempt %d of %d)", *issue, stageOr(*stage), attempts, allowed)
			return nil
		})
		if err != nil {
			return err
		}
		// Make sure the issue is claimable again by the retry worker.
		if iss, e := c.BD.Show(*issue); e == nil && iss.Status == "in_progress" {
			if _, e := c.BD.Run("update", *issue, "--status=open", "--assignee="); e != nil {
				info("warning: could not reset %s to open: %v", *issue, e)
			}
		}
		return emitJSON(map[string]any{
			"issue": *issue, "recorded": "retry",
			"attempt": attempts, "allowed": allowed,
			"note": "issue returned to the ready queue for a fresh worker",
		})
	}

	// Attempts exhausted: park it and move on.
	if err := c.BD.Park(*issue, fmt.Sprintf(
		"bd-auto parked after %d attempts. Last failure at stage %q: %s", attempts, stageOr(*stage), *reason)); err != nil {
		info("warning: could not park %s: %v", *issue, err)
	}
	_, err = runstate.Update(c.RepoRoot, false, func(s *runstate.State) error {
		s.Park(*issue, *reason, stageOr(*stage))
		s.Note("%s parked after %d attempts", *issue, attempts)
		return nil
	})
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{
		"issue": *issue, "recorded": "parked",
		"attempt": attempts, "allowed": allowed,
		"note": "issue set to blocked, labelled human, and left out of this run",
	})
}

func stageOr(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// discardAttempt removes a failed attempt's worktree and branch so the retry
// starts from a clean base rather than inheriting half-done work.
func discardAttempt(repoRoot, branch string) error {
	if wt := listWorktrees(repoRoot)[branch]; wt != "" {
		cmd := exec.Command("git", "worktree", "remove", "--force", wt)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if !branchExists(repoRoot, branch) {
		return nil
	}
	cmd := exec.Command("git", "branch", "-D", branch)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func workerStatus(args []string) error {
	c, err := NewCtx()
	if err != nil {
		return err
	}
	st, err := c.State()
	if errors.Is(err, runstate.ErrNoRun) {
		return emitJSON(map[string]any{"active": false})
	}
	if err != nil {
		return err
	}
	type row struct {
		Issue   string `json:"issue"`
		Branch  string `json:"branch"`
		Attempt int    `json:"attempt"`
		Session string `json:"worker_session,omitempty"`
		Status  string `json:"issue_status,omitempty"`
	}
	var rows []row
	for id, a := range st.InFlight {
		r := row{Issue: id, Branch: a.Branch, Attempt: a.Attempt, Session: a.WorkerSession}
		if iss, err := c.BD.Show(id); err == nil {
			r.Status = iss.Status
		}
		rows = append(rows, r)
	}
	return emitJSON(map[string]any{"active": true, "in_flight": rows, "wave": st.Wave})
}
