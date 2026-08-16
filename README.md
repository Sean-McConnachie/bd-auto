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
                 │              max 3 rounds)
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

You do not have to write it by hand:

```bash
bd-auto init                # starter config in the working directory
bd-auto init --force        # replace an existing one
bd-auto init --dir <path>   # somewhere other than here
```

`run start` does the same thing on your behalf when the repo has no config at
all, and reports the path it wrote as `config_created`. Both refuse to touch a
file that already exists, so a config you have tuned is never overwritten by
accident. The generated gate is commented out — bd-auto cannot know which build
and test commands your repo uses, and a wrong gate fails every issue.

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
    max_rounds: 3
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
bd-auto init [--force] [--dir <path>]   # write a starter .beads-auto.yaml

bd-auto run start --epic <id> [--concurrency N] [--autonomy auto|wave|issue]
bd-auto run status [--context]      # --context is what the rehydration hook prints
bd-auto run stop | pause | resume
bd-auto run unpark --issue <id>     # put a parked issue back into the run

bd-auto issue run --issue <id> [--base <ref>] [--rounds N] [--retry N] [--quiet]
                                    # one issue, end to end, in this process:
                                    # worktree, guards, worker, gate, review,
                                    # feedback rounds, retry, park

bd-auto plan [--dispatch]           # next wave; --dispatch records it as in-flight
bd-auto worker done --issue <id>
bd-auto worker fail --issue <id> --reason <text> [--stage <s>]
bd-auto gate                        # run the gate here
bd-auto stage list | stage run --name <s>
bd-auto merge-order                 # wave branches, dependency ordered
bd-auto config show
```

## Stopping a run

`bd-auto run pause` leaves the state in place and disarms the Stop hook's
nagging, so the session becomes an ordinary one; `resume` picks the loop back
up. `bd-auto run stop` ends the run and deletes the state — hooks go quiet and
the next `/bd-auto` starts fresh. Add `--keep-state` to stop but keep the record
of what landed.

Neither touches your branches. Work already committed by a worker stays on
`bd-auto/<issue-id>` whether or not the integrator got to it.

## Failure handling

A failed issue is retried **once** with a fresh worker, seeded with the previous
failure. If it fails again it is set to `blocked`, labelled `human` (so
`bd human list` finds it), and the run moves on. One bad issue never stalls the
drain.

Every attempt appends its evidence to the issue, so the history outlives any
context window.

### Recovering a parked issue

Parking is sticky on purpose: the issue is excluded by the **run state**, not
just by its status in beads, so reopening it with `bd` alone will not bring it
back. Once you have fixed whatever defeated the worker:

```bash
bd-auto run unpark --issue <id> --reason "what you fixed"
```

That reopens the issue, clears the `human` label, resets its attempt count so it
gets a full retry budget, and lets the next `bd-auto plan` offer it again. The
failure notes stay on the issue — the record of why it failed is the point.

If the run has already finished, there is nothing to unpark: fix the issue,
`bd update <id> --status=open --remove-label=human`, and start a new run.

## Design notes worth knowing

**One writer per issue, always.** A concurrency spike (`eqc.1`) found that
concurrent `bd note` calls against the *same* issue silently lose writes and
still exit 0. Five workers on five different issues are safe; two writers on one
issue are not. So the orchestrator never writes to an issue while its worker is
running, and the run state — which parallel hooks *do* write concurrently — is
guarded by a real `flock`, with a cross-process regression test.

**Workers cannot merge.** A `PreToolUse` hook denies `git merge|rebase|push|cherry-pick`
to any agent that is not the integrator. Enforcing it beats asking for it. The
match is deliberately unanchored, because git is reachable through `sudo`, `env`,
`nohup`, a loop body or an `if` condition; quoted spans are blanked out first, so
a worker's `bd update --append-notes="..."` can quote one of those commands
without being denied. Quoted text is data, not something about to run — except
when it is. Stripping is skipped, and the raw command matched instead, if the
command contains an executor (`eval`, `sh -c`, `xargs`), which runs its quoted
payload, or if the quotes do not balance, since an unterminated quote otherwise
blanks every line after it. Both were review findings, not theory.

**Workers cannot lie about finishing.** A `SubagentStop` hook checks that the
issue actually reached a terminal state and sends the worker back if not.

**Shell constraints in worktrees.** Claude Code refuses commands it cannot
statically verify stay inside the worktree, so workers must avoid heredocs with
unquoted delimiters and brace expansion. The worker prompt says so explicitly.
The live run showed the guard is stricter than "no exotic syntax": a worker
cannot `cd` to the main checkout to run *any* git command there, and even a
plain `&&` chain of five safe commands is refused as too complex to verify. So a
worker sees only its own branch, and scripts a worker might run must not assume
they can read the main checkout's git state.

**Scripts must not assume there is no run.** `scripts/smoke.sh` tears down run
state as part of its cleanup, and run state is shared with the main checkout
even from inside a worktree — so running it during a live drain would have
destroyed that drain. It now refuses to start while a run is active. Anything
else you add that writes `.beads/auto/` needs the same check.

## What the dogfood run showed

bd-auto drained its own epic (`beads-auto-imp-eqc`, 13 issues) with the plugin
loaded. Twelve issues were already closed when the run started, so wave 1 was a
single issue — the dogfood issue itself, implemented by a worker under the
orchestrator it was documenting.

| | |
|---|---|
| Orchestrator overhead, `run start` to wave dispatched | 8s |
| Retries | 0 |
| Review rounds to reach a pass | 3 |
| Parked | 0 |
| Merge conflicts | 0 (one branch in the wave) |
| Manual intervention | none beyond permission approvals |

Verified live rather than by test double:

- The `PreToolUse` binding recorded the worker's agent ID against its issue from
  the worker's own `bd update --claim`, with nothing passed in to say which
  issue it was working on.
- The merge guard denied a real integration command typed by a real worker, not
  a synthetic hook payload — and then over-fired on a note *quoting* that
  command, which is how the quote-stripping fix above got found.
- `bd` resolved the main repo's database from inside the worktree with no setup,
  as designed — worker `bd show`, `bd create` and `bd close` all just worked.
- The reviewer's send-back loop earned its keep. The merge guard took **three**
  review rounds: anchoring the pattern to command position fixed the over-firing
  but let `sudo git push` and `env FOO=1 git push` through; stripping quoted
  spans fixed that but let `eval "git push"` through, and an unbalanced quote
  blanked the rest of the command. Round three — fall back to matching the raw
  command whenever the shell parse is uncertain — passed. Each round was
  verified against the built binary with real hook payloads.

  This is why `max_rounds` defaults to **3**. At the original 2 this issue would
  have been parked with a working guard replaced by a broken one.

A one-issue wave cannot show everything. **Still unproven live**: parallel
workers under contention, an integrator resolving a genuine conflict between two
workers, retry-then-park, and whether the Stop hook holds the orchestrator on
task across a real autocompact. All are covered
deterministically by `make smoke`; none has been watched with several real
agents in flight. Point a run at an epic of three or four genuinely independent
issues, with `--autonomy wave` so there is a barrier to inspect, before trusting
it with a large epic.

## Development

```bash
make check    # build + vet + test, the same commands the gate runs
make smoke    # end-to-end run against a throwaway epic it creates and deletes
```

`make smoke` covers what unit tests cannot: talking to `bd`, reading a real DAG,
driving run state across processes, and every hook decision. It refuses to run
while a drain is active, because its cleanup would delete that run's state.

When bd-auto drains its own epic, `bd-auto` on a worker's PATH is the **main
checkout's** `bin/bd-auto` — the binary the running orchestrator is using, not
the worker's build. A worker changing bd-auto itself must `make build` and then
invoke `./bin/bd-auto` to exercise its own change; typing `bd-auto` silently
tests the old binary.
