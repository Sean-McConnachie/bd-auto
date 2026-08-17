---
name: bd-auto
description: Drain a beads epic autonomously by dispatching one worktree-isolated subagent per issue, in dependency-ordered waves, with a gate, a review stage and an end-of-wave integrator. Use when asked to auto-implement, drain, or work through an epic's issues.
---

# bd-auto: drain an epic

You are the **orchestrator**. You dispatch, record and integrate. You do not
implement issues yourself — that is the entire point, because your context is
the scarce resource here.

Usage: `/bd-auto <epic-id> [--concurrency N] [--autonomy auto|wave] [--dry-run]`

## Start

```bash
bd-auto run start --epic <epic-id>        # add --concurrency / --autonomy if given
```

Read the `swarm_validate` field it returns. If it reports cycles, wrong
dependency direction, or orphans, **stop and show the user** — a bad DAG means
the run will mis-order work. Otherwise continue.

For `--dry-run`, run `bd-auto plan` and `bd-auto stage list`, show the user the
wave and the pipeline, then `bd-auto run stop`. Do not dispatch.

## The loop

Repeat until `bd-auto plan` reports `"drained": true`.

**1. Plan the wave.**
```bash
bd-auto plan --dispatch
```
Returns the issues to dispatch, each with its branch, attempt number and, on a
retry, the previous failure.

**2. Dispatch the whole wave in ONE message.** One `bd-worker` subagent per
issue, all in a single message so they run in parallel. Give each worker only:
its issue ID, its branch name, and its `retry_context` if present. Do not
summarise the issue for it — the worker reads beads itself.

**3. Collect reports.** Each worker ends with
`BD-AUTO: issue=<id> branch=<branch> status=done|blocked`.

**4. Review each finished issue** (only if the pipeline has a `review` stage —
check `bd-auto stage list`). Dispatch `bd-reviewer` with the issue ID and branch.
- `VERDICT: pass` → step 5.
- `VERDICT: fail` → send the notes back to **the same worker** via SendMessage,
  so it keeps its context and worktree. At most `max_rounds` times (default 3).
  Still failing → treat as a failed attempt (step 5, fail branch).

Any `run:` stages in the pipeline execute with
`bd-auto stage run --name <stage> --issue <id> --branch <branch>`.

**5. Record every issue.** This is bookkeeping you must not skip; the run state
is how the loop survives a compaction.
```bash
bd-auto worker done --issue <id>                                    # passed everything
bd-auto worker fail --issue <id> --stage <stage> --reason "<what failed>"   # anything else
```
`worker fail` applies the retry policy itself: it retries once with a fresh
worker, then parks the issue and flags it for a human. Never decide that
yourself, and never retry an issue it has parked.

**6. Integrate at the barrier.** Once no issue from the wave is still in flight,
dispatch **one** `bd-integrator`. Wait for it before planning the next wave —
the next wave's worktrees branch from `HEAD`, so they only see this wave's work
after it merges.

## Finish

When drained: report to the user what landed, what is parked and why, and any
discovered work that was filed deferred. Then `bd-auto run stop`.

`drained` only counts the epic's *children*, so the epic issue itself can still
be open at this point — including on a run that had nothing to dispatch at all.
Check it, and if it is open, dispatch one `bd-integrator`. It owns that
decision and closes the epic only if every child completed with nothing parked.
Do not close the epic yourself; the rule below has no exception for parents.

If the user fixes a parked issue and asks for it to be retried, that is the one
way back into a live run: `bd-auto run unpark --issue <id> --reason "<fix>"`,
then continue the loop. Only on their say-so — never unpark on your own
judgement.

## Rules

- **Never implement an issue yourself.** If tempted, dispatch a worker.
- **Never close an issue yourself.** Workers close their own; `bd-auto worker`
  records it.
- **One writer per issue.** Never `bd update`/`bd note` an issue while its worker
  is running — beads loses concurrent writes to the same issue silently.
- **A Stop hook enforces this loop.** If it refuses to let you stop, it prints
  exactly what to do next. Do that; do not argue with it or work around it.
- Lost after a compaction? `bd-auto run status` rebuilds everything.
