# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:7510c1e2 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->

## What this repo is

`bd-auto` — a Go binary that drains a set of [beads](https://github.com/gastownhall/beads)
issues by running **one worktree-isolated model process per issue**, in
dependency-ordered waves, with a per-issue pipeline and an integrator at each
wave barrier. The control flow lives in Go, not in a context window: there is no
orchestrator session to autocompact, and an interrupted run resumes from
`.beads/auto/run.json`.

It also ships as a Claude Code plugin (`.claude-plugin/plugin.json`,
`skills/bd-auto/SKILL.md`), which only launches and watches the binary.

`README.md` is long and is the reference — it records *why* most of the
non-obvious decisions below were made, usually with the measurement that forced
them. Read the relevant section before changing behaviour it describes.

## Build & Test

```bash
make check        # build + vet + test — exactly what the gate runs
make build        # bin/bd-auto (put bin/ on PATH)
make fmt          # gofmt -w ./cmd ./internal
make smoke        # end-to-end against a throwaway epic it creates and deletes
make integrator-stress  # 16 drains through the barrier's worst states, no models
make install-check  # claude plugin validate .

go test ./internal/drain -run TestName    # one test
go test ./internal/drain -run TestName -v # with output
```

Tests spawn no models: `internal/runner/fake` is a scripted `runner.Runner`, and
`provider: fake` also works in a real config. Tests that do spend money are
opt-in by env var and stay out of `make check` and the gate:

| Gate | What it runs |
|---|---|
| `BD_AUTO_CLAUDE_LIVE` | `internal/runner/claude` preflight against the installed CLI — run after upgrading Claude Code |
| `BD_AUTO_ASK_LIVE` | `internal/ask` live question round-trip |
| `BD_AUTO_E2E_GRAPH` | `internal/drain` graphify end-to-end |
| `BD_AUTO_SHOTS` | `internal/tui` screenshot harness (`make tui-shots`) |

`make resume-vs-fresh` and `make graph-ab` are paid experiments in throwaway
repos, never part of `check`.

`make smoke` refuses to start while a drain is active, because its cleanup
deletes `.beads/auto/`. Anything new that writes `.beads/auto/` needs the same
check. Use `scripts/smoke.sh --isolated` to run it during a live drain.

## Architecture

`cmd/bd-auto/main.go` is a thin shell over `internal/cmds`, which parses flags
and does nothing else interesting. The layers below it:

- **`internal/drain`** — the engine. Runs one issue end to end (`issue.go`), a
  wave of them in parallel (`drain_wave.go`), the barrier (`integrate.go`), the
  handoff (`handoff.go`), discovery triage (`triage.go`), and the event bus
  (`bus.go`) that all three renderers read. Every failure kind funnels into one
  feedback channel; the ordering of those checks is load-bearing and documented
  at the top of `drain.go`.
- **`internal/runner`** — the seam between the engine and whatever runs a model.
  Nothing above it names a vendor; adapters (`runner/claude`, `runner/fake`)
  register themselves via `runner/providers`. A role is a prompt plus a tool
  list — there is no "subagent" concept.
- **`internal/runstate`** — `.beads/auto/run.json`, the durable state. Every
  mutation goes through `Update`, which holds an `flock` across the whole
  read-modify-write.
- **`internal/bd`** — wraps the `bd` CLI. bd is the source of truth for issues,
  dependencies and readiness; this package caches and reimplements none of it.
- **`internal/scope`**, **`internal/wave`**, **`internal/pipeline`**,
  **`internal/config`** — pure functions over explicit inputs. They run no
  processes and read no flags; that stays with the caller.
- **`internal/worktree`**, **`internal/gitguard`**, **`internal/gitx`** — the
  isolation layer (see below).
- **`internal/tui`** — bubbletea wave table, one of three renderers on the same
  bus and the least privileged. Off a terminal it is never entered.
- **`internal/ask`** — MCP-over-stdio question channel; `bd-auto ask` is the
  per-worker shim that forwards to the drain over a unix socket.
- **`internal/graph`**, **`internal/similar`** — the optional code index, and
  rarity-weighted text matching for deduplicating discovered work.
- **`prompts/`** — role prompts, `go:embed`ed because a run spawns processes in
  worktrees where the prompts do not exist.

## Invariants that are easy to break

**Every git command bd-auto runs must go through `internal/gitx`.** It passes
`-c core.hooksPath=<nowhere>`. beads' post-checkout/post-merge hooks import
`.beads/issues.jsonl` over the Dolt DB, which silently reverts every bd write
since the last export — one `git pull --rebase` took eight issues from closed
back to open. `internal/gitguard` is the sole deliberate exception (it reads
`core.hooksPath` to chain onto it, and runs only `config`/`rev-parse`).

**One writer per issue.** Concurrent `bd note` calls against the *same* issue
lose writes and still exit 0. The engine never writes to an issue while its
worker is running.

**Generated worktree hooks chain, never overwrite.** `core.hooksPath` is already
set in this repo to beads' hooks. Each generated hook rejects what bd-auto
blocks and otherwise `exec`s the same-named hook under the previous path.
`pre-commit` additionally removes `.beads/issues.jsonl` and
`.beads/interactions.jsonl` from the index — they are a re-export of a database
every worker writes to, and committing one makes every wave branch conflict.

**`bd-auto hook <event>` exits 0 for everything, always.** Claude Code reads a
non-zero exit as *block*, and a hook that blocks every tool call cannot be fixed
from inside the session it broke. Rules enforced by
`internal/cmds/hook_config_test.go`; any shipped hooks config also needs
`… || true`.

**A worker can only finish under `permissions: bypass`.** Under `auto` (the
default) a headless worker is refused every write and every shell command. Keep
the reviewer `scoped`, and keep its `denied_tools` — deny is checked before the
permission level, and a reviewer with bare `Bash` once closed the issue it was
reviewing.

**When bd-auto drains its own epic**, `bd-auto` on a worker's PATH is the *main
checkout's* `bin/bd-auto` — the running drain's binary. A worker changing
bd-auto must `make build` and invoke `./bin/bd-auto`; typing `bd-auto` silently
tests the old binary.

**Worktree paths are a pure function of the issue ID.** A backend resolves a
resumable session against the working directory, so a worktree that moves
between rounds is a session that cannot be resumed.

**Compare `total_cost_usd`, never summed tokens**, when measuring anything.
Cache reads bill far below input price, so token counts flatter whichever arm
reads more cache.

## Conventions

- **Prose carries the reasoning.** Package doc comments and README sections
  explain *why*, usually naming the incident or measurement behind a decision.
  Match that when changing them; a comment that only restates the code is drift.
- **Commit subjects are `<issue-id>: what changed`** — e.g.
  `beads-auto-imp-84g: a reviewer refused nothing writes no refusal line`.
  Bookkeeping-only commits use the `beads:` prefix.
- **Machine output to stdout as JSON, human commentary to stderr.**
- **Non-interactive shell flags always** (`cp -f`, `mv -f`, `rm -f`, `rm -rf`).
  `cp`/`mv`/`rm` may be aliased to `-i` and will hang an agent forever.
- Do not use `bd edit` — it opens `$EDITOR` and blocks.
