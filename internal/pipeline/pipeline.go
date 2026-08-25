// Package pipeline executes the parts of a per-issue pipeline that are
// deterministic: the gate commands and any user-supplied run: stages.
//
// agent: stages are not executed here. This package shells out; spawning and
// supervising a model process is the drain engine's job, in internal/drain,
// where the session, its worktree and its feedback rounds are already tracked.
// That split is what lets a custom review pipeline be either a shell script or
// a model without the framework caring which.
package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"bd-auto/internal/config"
	"bd-auto/internal/gitx"
)

// Result is the outcome of one executed stage or gate command.
type Result struct {
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Command  string  `json:"command,omitempty"`
	Passed   bool    `json:"passed"`
	ExitCode int     `json:"exit_code"`
	TimedOut bool    `json:"timed_out,omitempty"`
	Output   string  `json:"output,omitempty"`
	Seconds  float64 `json:"seconds"`
}

// Env carries the values exposed to run: stages and to run: hooks.
type Env struct {
	Issue    string
	Branch   string
	Dir      string
	RepoRoot string
	DiffFile string
	// ReportFile is the report JSON a hook is handed, and Hook and HookPoint
	// say which hook is reading it. All three are empty for a pipeline stage.
	//
	// A hook gets a path rather than the report on stdin for the same reason a
	// run: stage gets $BD_DIFF_FILE: the thing being handed over is already a
	// file on disk with a name worth keeping, and a command that wants only two
	// fields of it should not have to consume the whole of it to get them.
	ReportFile string
	Hook       string
	HookPoint  string
}

func (e Env) environ() []string {
	env := os.Environ()
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("BD_ISSUE", e.Issue)
	add("BD_BRANCH", e.Branch)
	add("BD_WORKTREE", e.Dir)
	add("BD_REPO_ROOT", e.RepoRoot)
	add("BD_DIFF_FILE", e.DiffFile)
	add("BD_REPORT_FILE", e.ReportFile)
	add("BD_HOOK", e.Hook)
	add("BD_HOOK_POINT", e.HookPoint)
	return env
}

// Tail returns at most n bytes from the end of b, prefixed with a marker when
// truncated. Every command output that can reach a worker or the orchestrator
// goes through this: an unbounded failing test suite would otherwise blow the
// very context the tool exists to protect.
func Tail(b []byte, n int) string {
	s := strings.TrimRight(string(b), "\n")
	if n <= 0 || len(s) <= n {
		return s
	}
	return "...[truncated, showing last " + fmt.Sprint(n) + " bytes]...\n" + s[len(s)-n:]
}

// Exec runs one shell command and captures a bounded tail of its combined
// output.
func Exec(name, command string, timeoutSec, tailBytes int, env Env) Result {
	if timeoutSec <= 0 {
		timeoutSec = config.DefaultCommandTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = env.Dir
	cmd.Env = env.environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	r := Result{
		Name:    name,
		Kind:    "run",
		Command: command,
		Output:  Tail(buf.Bytes(), tailBytes),
		Seconds: time.Since(start).Seconds(),
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		r.TimedOut = true
		r.ExitCode = -1
		r.Passed = false
	case err == nil:
		r.Passed = true
	default:
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			r.ExitCode = ee.ExitCode()
		} else {
			r.ExitCode = -1
			r.Output = strings.TrimSpace(r.Output + "\n" + err.Error())
		}
	}
	return r
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// Gate runs every configured gate command in order and stops at the first
// failure. A repo with no gate configured passes trivially, which is what makes
// the tool usable in a repo with no test suite.
func Gate(cfg *config.Config, env Env) []Result {
	var results []Result
	for _, g := range cfg.Gate {
		r := Exec(g.Name, g.Run, g.Timeout, cfg.OutputTailBytes, env)
		r.Kind = "gate"
		results = append(results, r)
		if !r.Passed {
			break
		}
	}
	return results
}

// Passed reports whether every result in a set passed.
func Passed(rs []Result) bool {
	for _, r := range rs {
		if !r.Passed {
			return false
		}
	}
	return true
}

// FirstFailure returns the first failing result, or nil.
func FirstFailure(rs []Result) *Result {
	for i := range rs {
		if !rs[i].Passed {
			return &rs[i]
		}
	}
	return nil
}

// Summary renders results as short human-readable lines.
func Summary(rs []Result) string {
	var b strings.Builder
	for _, r := range rs {
		mark := "PASS"
		if !r.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "%s %s (%.1fs)", mark, r.Name, r.Seconds)
		if r.TimedOut {
			b.WriteString(" [timed out]")
		} else if !r.Passed {
			fmt.Fprintf(&b, " [exit %d]", r.ExitCode)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// WriteDiff writes the worker's diff against base to a temp file and returns
// its path, so run: stages can inspect it without recomputing it.
func WriteDiff(dir, base string) (string, error) {
	out, err := gitx.Cmd(dir, "diff", base+"...HEAD").Output()
	if err != nil {
		// Fall back to the working-tree diff, which is what a worker that has
		// not committed yet will have.
		out, err = gitx.Cmd(dir, "diff", "HEAD").Output()
		if err != nil {
			return "", err
		}
	}
	f, err := os.CreateTemp("", "bd-auto-diff-*.patch")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", err
	}
	return f.Name(), nil
}
