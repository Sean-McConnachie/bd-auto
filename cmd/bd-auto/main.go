// Command bd-auto orchestrates a beads epic across worktree-isolated model
// processes: one issue per process, dependency-ordered waves, a configurable
// per-issue pipeline, and an integrator at each wave barrier.
//
// The binary owns the control flow. Machine output goes to stdout as JSON;
// human commentary goes to stderr.
package main

import (
	"fmt"
	"os"

	"bd-auto/internal/cmds"
)

// Version is the binary version, overridable at build time with
// -ldflags "-X main.Version=...".
var Version = "0.2.0"

const usage = `bd-auto - beads-driven, headless orchestration of coding models

Usage:
  bd-auto init [--provider claude|codex] [--force] [--dir <path>]
                                            write a starter .beads-auto.yaml and
                                            .beads-auto/agents/

  bd-auto drain --epic <id>                 pick a scope, then run it to
                                            completion in this process
  bd-auto drain --epic <id> --all           scope the run to every candidate
  bd-auto drain --issues a,b,c              scope the run to named issues
    [--concurrency N] [--autonomy auto|wave] [--rounds N] [--retry N]
    [--base <ref>] [--no-pr] [--no-epic-branch] [--no-preflight]
    [--allow-api-billing]
    [--plain] [--json] [--dry-run] [--quiet]

  bd-auto run start --epic <id> [--concurrency N] [--autonomy auto|wave] [--retry N]
  bd-auto run status [--context] [--wait <duration>]  watch a run in a few lines
  bd-auto run stop [--keep-state]
  bd-auto run pause | resume
  bd-auto run unpark --issue <id> [--reason <text>]   retry a parked issue

  bd-auto issue run --issue <id> [--base <ref>] [--rounds N] [--retry N] [--quiet]
    [--allow-api-billing]
                                            drive one issue through the whole
                                            pipeline in this process

  bd-auto plan [--dispatch] [--limit N]     compute (and claim) the next wave
  bd-auto worker done --issue <id>          deprecated manual bookkeeping
  bd-auto worker fail --issue <id> --reason <text> [--stage <s>]  (deprecated)
  bd-auto worker status                     what is in flight

  bd-auto gate [--issue <id>] [--branch <b>]        run the configured gate
  bd-auto stage list                                show the resolved pipeline
  bd-auto stage run --name <s> [--issue <id>]       run one run: stage
  bd-auto merge-order [--all]                       wave branches, dependency ordered
  bd-auto integrate [--all] [--quiet]               merge the wave, gate it, settle
                                                    the epic
  bd-auto handoff [--force] [--quiet]               re-gate the epic branch of a
                                                    run that already finished and
                                                    open its pull request

  bd-auto triage [--list] [--all]           what a run's workers found, waiting
  bd-auto triage --accept <key>                     file it as an issue
  bd-auto triage --accept <key> --into <issue>      fold it into an issue instead
  bd-auto triage --discard <key> --reason <text>    say it is not work
  bd-auto triage --accept-all

  bd-auto config show
  bd-auto agents [list]                     what each role's prompt resolves to
  bd-auto agents show <role>                the prompt that role is spawned with
  bd-auto agents diff [<role>...]           a materialised agent against the
                                            prompt this binary ships
  bd-auto agents update <role>... | --all   take the shipped prompt again
  bd-auto version

  bd-auto ask --socket <path> --issue <id> [--role <r>]
                                            the question channel a drain hands
                                            its own workers. Started by bd-auto,
                                            not by hand.

  bd-auto hook <event>                      the Claude Code hook entry point.
                                            Reads the hook payload on stdin and
                                            exits 0 for every event, known or
                                            not. Called by Claude Code, not by
                                            hand.

A drain publishes nothing by itself. Every issue branch is merged, in dependency
order, onto one temporary branch under bd-auto/epic/, and the branch you are on
is never written to. Once the whole run has landed clean and the gate is green
on the merged result, that branch is pushed and a pull request opens against the
branch the run started from — a parked issue or a red gate opens nothing and
leaves the branch for you. --no-pr keeps the branch and skips the pull request;
--no-epic-branch merges straight into your branch instead. ` + "`bd-auto handoff`" + `
opens that pull request later, for a run that was interrupted or one you finished
by hand; --force opens it over a refusal you have looked at and disagree with.

Before any of that, Codex authentication is checked locally. ChatGPT login uses
the authenticated plan. API-key authentication refuses unless this invocation
includes --allow-api-billing; --no-preflight never skips that safety gate.

After billing authorization, a drain spends one trivial model call per distinct runner
configuration checking that the backend can be spawned at all. A configured CLI
that is missing, unauthorized, or no longer accepts an adapter flag stops the
run there, with one error and no worktrees. --no-preflight skips it.

A wave barrier files no issues of its own. What its workers found is staged in
.beads/auto/triage.json and waits there for ` + "`bd-auto triage`" + `, because filing
is the irreversible half: a backlog grows whether or not a run learned anything,
and this repo's own history peaked at 2.27 issues created per issue closed.
` + "`discovered_work: defer`" + ` restores the old behaviour of filing each finding
hidden from ` + "`bd ready`" + `, and ` + "`immediate`" + ` files and offers it.

Run state lives in .beads/auto/run.json. Configuration is .beads-auto.yaml at
the repo root; every field has a default, so a repo without one still works.
` + "`bd-auto init`" + ` writes one for you, and ` + "`run start`" + ` does the same
if the repo has none.

An agent is one file: .beads-auto/agents/<role>.md, frontmatter over the system
prompt. Whatever is there wins over the prompt this binary ships, and
` + "`bd-auto init`" + ` writes the shipped ones out so a repo's own history says what
it ran. ` + "`bd-auto agents`" + ` reports what each role resolved to.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	// The version is recorded in the frontmatter of every agent file bd-auto
	// materialises, so it has to reach the package that writes them.
	cmds.Version = Version

	var err error
	switch os.Args[1] {
	case "init":
		err = cmds.Init(os.Args[2:])
	case "drain":
		err = cmds.Drain(os.Args[2:])
	case "run":
		err = cmds.Run(os.Args[2:])
	case "issue":
		err = cmds.Issue(os.Args[2:])
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
	case "integrate":
		err = cmds.Integrate(os.Args[2:])
	case "handoff":
		err = cmds.Handoff(os.Args[2:])
	case "ask":
		err = cmds.Ask(os.Args[2:])
	case "hook":
		err = cmds.Hook(os.Args[2:])
	case "triage":
		err = cmds.Triage(os.Args[2:])
	case "config":
		err = cmds.Config(os.Args[2:])
	case "agents":
		err = cmds.Agents(os.Args[2:])
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
