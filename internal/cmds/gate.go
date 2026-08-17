package cmds

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"bd-auto/internal/config"
	"bd-auto/internal/pipeline"
)

// Gate implements `bd-auto gate`: run the configured gate commands.
//
// Workers call this from inside their worktree, so it runs in the working
// directory rather than the repo root.
func Gate(args []string) error {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	issue := fs.String("issue", "", "issue ID this gate is for (labels output only)")
	branch := fs.String("branch", "", "branch under test (labels output only)")
	quiet := fs.Bool("quiet", false, "suppress the human summary on stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}

	if !c.Cfg.HasGate() {
		if !*quiet {
			info("no gate configured in %s; gate passes trivially", configPathOrDefault(c.Cfg))
		}
		return emitJSON(map[string]any{
			"passed": true, "configured": false, "results": []pipeline.Result{},
		})
	}

	env := pipeline.Env{
		Issue:    *issue,
		Branch:   *branch,
		Dir:      c.Cwd,
		RepoRoot: c.RepoRoot,
	}
	results := pipeline.Gate(c.Cfg, env)
	passed := pipeline.Passed(results)

	if !*quiet {
		fmt.Fprint(os.Stderr, pipeline.Summary(results))
	}

	out := map[string]any{
		"passed":     passed,
		"configured": true,
		"results":    results,
	}
	if f := pipeline.FirstFailure(results); f != nil {
		out["failed_stage"] = f.Name
		out["output"] = f.Output
	}
	if err := emitJSON(out); err != nil {
		return err
	}
	if !passed {
		return errSilentExit{code: 1}
	}
	return nil
}

// Stage implements `bd-auto stage <list|run>`.
func Stage(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bd-auto stage <list|run>")
	}
	switch args[0] {
	case "list":
		return stageList(args[1:])
	case "run":
		return stageRun(args[1:])
	default:
		return fmt.Errorf("unknown stage subcommand %q", args[0])
	}
}

func stageList(args []string) error {
	c, err := NewCtx()
	if err != nil {
		return err
	}
	return emitJSON(map[string]any{
		"config":   configPathOrDefault(c.Cfg),
		"pipeline": describePipeline(c.Cfg),
		"gate":     gateNames(c.Cfg),
	})
}

// stageRun executes one run: stage. agent: stages are refused here on purpose:
// spawning a model belongs to the drain engine, and silently skipping one would
// let a review stage appear to pass without running.
func stageRun(args []string) error {
	fs := flag.NewFlagSet("stage run", flag.ContinueOnError)
	name := fs.String("name", "", "stage name (required)")
	issue := fs.String("issue", "", "issue ID")
	branch := fs.String("branch", "", "branch under test")
	base := fs.String("base", "", "base ref for the diff handed to the stage")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}

	var stage *config.Stage
	for i := range c.Cfg.Pipeline {
		if c.Cfg.Pipeline[i].Stage == *name {
			stage = &c.Cfg.Pipeline[i]
			break
		}
	}
	if stage == nil {
		return fmt.Errorf("no stage %q in %s", *name, configPathOrDefault(c.Cfg))
	}

	switch stage.Kind() {
	case "agent":
		return fmt.Errorf("stage %q is an agent stage (agent: %s); dispatch it with the Agent tool, not bd-auto",
			*name, stage.Agent)
	case "builtin-gate":
		return Gate([]string{"--issue", *issue, "--branch", *branch})
	case "builtin-implement":
		return fmt.Errorf("stage %q is the implement stage; it is the worker itself", *name)
	}

	env := pipeline.Env{Issue: *issue, Branch: *branch, Dir: c.Cwd, RepoRoot: c.RepoRoot}
	if *base != "" {
		if p, err := pipeline.WriteDiff(c.Cwd, *base); err == nil {
			env.DiffFile = p
			defer os.Remove(p)
		}
	}

	r := pipeline.Exec(stage.Stage, stage.Run, stage.Timeout, c.Cfg.OutputTailBytes, env)
	fmt.Fprint(os.Stderr, pipeline.Summary([]pipeline.Result{r}))
	if err := emitJSON(map[string]any{
		"stage":    r.Name,
		"passed":   r.Passed || stage.Optional,
		"optional": stage.Optional,
		"result":   r,
	}); err != nil {
		return err
	}
	if !r.Passed && !stage.Optional {
		return errSilentExit{code: 1}
	}
	return nil
}

// errSilentExit sets a non-zero exit code without printing another error, so a
// failing gate reports through its JSON rather than twice.
type errSilentExit struct{ code int }

func (e errSilentExit) Error() string { return fmt.Sprintf("exit %d", e.code) }

// ExitCode extracts a requested exit code from an error, if any.
func ExitCode(err error) (int, bool) {
	var se errSilentExit
	if errors.As(err, &se) {
		return se.code, true
	}
	return 0, false
}
