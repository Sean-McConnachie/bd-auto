# bd-auto

A Claude Code plugin that drains a [beads](https://github.com/gastownhall/beads)
epic by dispatching **one worktree-isolated subagent per issue**, in
dependency-ordered waves, with a configurable per-issue pipeline and an
integrator at each wave barrier.

## The problem it solves

On a long multi-step task, the session's context grows until autocompact fires
and the model forgets its instructions — even though everything it needed was
already written down in the issues.

bd-auto fixes this from two directions:

- **Each issue runs in a fresh subagent context.** The worker reads the issue
  from beads itself, does the work, and returns a report capped at a few lines.
  The verbose middle never touches the orchestrator's context.
- **Beads is the state, not the conversation.** Run state lives in
  `.beads/auto/run.json`, and a `SessionStart`/`PostCompact` hook reprints it.
  After a compaction the orchestrator rebuilds epic, wave, in-flight and parked
  work from disk rather than from history.

Issues the DAG says are independent run in parallel, each in its own git
worktree, so parallel implementation is safe rather than a merge hazard.

## Install

```bash
make build                       # produces bin/bd-auto; the plugin puts bin/ on PATH
claude --plugin-dir /path/to/beads-auto-imp
```

Then, in the session:

```
/bd-auto <epic-id>
```

`make install-check` runs `claude plugin validate .`.

## How a run works

```
run start ──> plan wave ──> dispatch N workers (one message, parallel)
                 ▲                    │
                 │              each worker: claim → own worktree →
                 │              implement → gate → commit → close
                 │                    │
                 │              review stage (fail → back to the same worker,
                 │              max 2 rounds)
                 │                    │
                 └──── integrator ◄───┘  merge wave in dependency order,
                       (wave barrier)     resolve conflicts, gate the result
```

The loop repeats until `bd ready --parent <epic>` is empty. A `Stop` hook
refuses to let the session stop while work remains, and prints exactly what to
do next — so the loop survives the model losing the thread.

## Configuration

`.beads-auto.yaml` at the repo root. Every field has a default; a repo without
the file still works.

```yaml
gate:                          # all must exit 0
  - name: build
    run: go build ./...
  - name: test
    run: go test ./...

pipeline:                      # ordered, per issue
  - stage: implement           # built in: the worker
  - stage: gate                # built in: the gate commands above
  - stage: review
    agent: bd-reviewer         # any subagent; dispatched by the orchestrator
    max_rounds: 2
  - stage: security            # your own pipeline, as a command
    run: ./scripts/security-review.sh
    optional: true

concurrency: 5                 # workers per wave
autonomy: auto                 # auto | wave | issue
retry: 1                       # retry once fresh, then park
discovered_work: defer         # keep new findings out of the current run
```

### Adding your own stage

A stage is either `agent: <name>` (a subagent, dispatched by the orchestrator)
or `run: <command>` (executed by `bd-auto`, must exit 0). `run:` stages get
`$BD_ISSUE`, `$BD_BRANCH`, `$BD_WORKTREE`, `$BD_REPO_ROOT` and `$BD_DIFF_FILE`.

A failing stage feeds its output — truncated to a fixed budget — into the retry,
so the next attempt starts informed without the orchestrator reading a test log.

## Required repo settings

Applied here already; needed in any repo you use this in.

| Setting | Why |
|---|---|
| `worktree.baseRef: "head"` in `.claude/settings.json` | Subagent worktrees otherwise branch from the **default** branch, and wave N+1 would not see wave N's merge. |
| `.claude/worktrees/` in `.gitignore` | Worker worktrees live there and would otherwise show as untracked. |
| An accepting permission mode | `permissionMode` in frontmatter is **ignored for plugin subagents**, so five workers in a prompting mode means a prompt storm. |

You do **not** need to copy `.beads/` into worktrees: `bd` resolves the main
repo's database from inside a worktree by itself.

## Commands

```bash
bd-auto run start --epic <id> [--concurrency N] [--autonomy auto|wave|issue]
bd-auto run status [--context]      # --context is what the rehydration hook prints
bd-auto run stop | pause | resume

bd-auto plan [--dispatch]           # next wave; --dispatch records it as in-flight
bd-auto worker done --issue <id>
bd-auto worker fail --issue <id> --reason <text> [--stage <s>]
bd-auto gate                        # run the gate here
bd-auto stage list | stage run --name <s>
bd-auto merge-order                 # wave branches, dependency ordered
bd-auto config show
```

## Failure handling

A failed issue is retried **once** with a fresh worker, seeded with the previous
failure. If it fails again it is set to `blocked`, labelled `human` (so
`bd human list` finds it), and the run moves on. One bad issue never stalls the
drain.

Every attempt appends its evidence to the issue, so the history outlives any
context window.

## Design notes worth knowing

**One writer per issue, always.** A concurrency spike (`eqc.1`) found that
concurrent `bd note` calls against the *same* issue silently lose writes and
still exit 0. Five workers on five different issues are safe; two writers on one
issue are not. So the orchestrator never writes to an issue while its worker is
running, and the run state — which parallel hooks *do* write concurrently — is
guarded by a real `flock`, with a cross-process regression test.

**Workers cannot merge.** A `PreToolUse` hook denies `git merge|rebase|push|cherry-pick`
to any agent that is not the integrator. Enforcing it beats asking for it.

**Workers cannot lie about finishing.** A `SubagentStop` hook checks that the
issue actually reached a terminal state and sends the worker back if not.

**Shell constraints in worktrees.** Claude Code refuses commands it cannot
statically verify stay inside the worktree, so workers must avoid heredocs with
unquoted delimiters and brace expansion. The worker prompt says so explicitly.

## Development

```bash
make check    # build + vet + test, the same commands the gate runs
make smoke    # end-to-end run against a throwaway epic it creates and deletes
```

`make smoke` covers what unit tests cannot: talking to `bd`, reading a real DAG,
driving run state across processes, and every hook decision.
