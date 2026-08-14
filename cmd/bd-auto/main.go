// Command bd-auto orchestrates a beads epic across worktree-isolated Claude
// Code subagents: one issue per agent, dependency-ordered waves, a configurable
// per-issue pipeline, and an integrator at each wave barrier.
//
// It is both the hook entrypoint and the orchestrator's helper. Machine output
// goes to stdout as JSON; human commentary goes to stderr.
package main

import (
	"fmt"
	"os"

	"bd-auto/internal/cmds"
)

// Version is the binary version, overridable at build time with
// -ldflags "-X main.Version=...".
var Version = "0.1.0"

const usage = `bd-auto - beads-driven subagent orchestration for Claude Code

Usage:
  bd-auto run start --epic <id> [--concurrency N] [--autonomy auto|wave|issue] [--retry N]
  bd-auto run status [--context]
  bd-auto run stop [--keep-state]
  bd-auto run pause | resume

  bd-auto plan [--dispatch] [--limit N]     compute (and claim) the next wave
  bd-auto worker done --issue <id>          record a completed issue
  bd-auto worker fail --issue <id> --reason <text> [--stage <s>]
  bd-auto worker status                     what is in flight

  bd-auto gate [--issue <id>] [--branch <b>]        run the configured gate
  bd-auto stage list                                show the resolved pipeline
  bd-auto stage run --name <s> [--issue <id>]       run one run: stage
  bd-auto merge-order [--all]                       wave branches, dependency ordered

  bd-auto hook <stop|session-start|post-compact|subagent-stop|pre-tool-use>
  bd-auto config show
  bd-auto version

Run state lives in .beads/auto/run.json. Configuration is .beads-auto.yaml at
the repo root; every field has a default, so a repo without one still works.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "run":
		err = cmds.Run(os.Args[2:])
	case "plan":
		err = cmds.Plan(os.Args[2:])
	case "worker":
		err = cmds.Worker(os.Args[2:])
	case "gate":
		err = cmds.Gate(os.Args[2:])
	case "stage":
		err = cmds.Stage(os.Args[2:])
	case "merge-order":
		err = cmds.MergeOrder(os.Args[2:])
	case "hook":
		err = cmds.Hook(os.Args[2:])
	case "config":
		err = cmds.Config(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("bd-auto " + Version)
		return
	case "help", "--help", "-h":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		// A requested exit code carries its own reporting; anything else is a
		// real error worth printing.
		if code, ok := cmds.ExitCode(err); ok {
			os.Exit(code)
		}
		fmt.Fprintf(os.Stderr, "bd-auto: %v\n", err)
		os.Exit(1)
	}
}
