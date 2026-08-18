---
name: bd-auto
description: Drain a set of beads issues by launching the bd-auto engine, which runs one worktree-isolated model process per issue in dependency-ordered waves, with a gate, a review stage and an end-of-wave integrator. Use when asked to auto-implement, drain, or work through an epic's issues.
---

# bd-auto: launch a drain

`bd-auto` runs the whole thing itself — waves, workers, gate, review rounds,
retries, the barrier. You are not the orchestrator. Launch it, wait, report.
Keeping this session small is the point: do not read the drain log, `bd show`
the issues, or summarise them.

## 1. Name the scope

Nothing caps a drain's spend but its issue list, so a human picks that list:

- named issues → `--issues a,b,c`
- the whole epic, in as many words → `--epic <id> --all`
- anything vaguer → **ask which issues**, once. Never launch a bare
  `--epic <id>`; with no terminal it refuses and spawns nothing anyway.

Do not list the epic to help them choose. `bd-auto drain --epic <id> --dry-run`
does that, in their terminal.

## 2. Launch

One command, `run_in_background`, redirect included:

```bash
mkdir -p .beads/auto && bd-auto drain --issues a,b,c --plain >.beads/auto/drain.log 2>&1
```

`--plain` is required: it stops the engine drawing a live table for a terminal
that is not there. Add `--concurrency N`, `--rounds N`, `--retry N` only if
asked.

If it exits within seconds it refused to start. Only then, read the last 20
lines of the log, say why, and stop.

## 3. Wait

```bash
bd-auto run status --context --wait 1h
```

Blocks until the run stops, or an hour, then prints four lines. Repeat while the
status still says `active`. One call per hour — never a sleep-and-poll loop,
which buys the same four lines many times over.

Meanwhile leave the repo alone: workers branch from `HEAD`, a `bd update` on an
issue the run owns is a lost write, and the run moves the checkout onto its own
`bd-auto/epic/...` branch until it ends.

## 4. Report

From the final status alone: the pull request if there is one, what landed, what
is parked, and that the rest is in `.beads/auto/drain.log`. Only parked issues
want a human, and each carries its failure in its own beads notes.

A drain merges nothing into their branch. It stages on the epic branch and opens
a pull request only if the whole run finished clean and green; `staged on ...`
rather than `handed over` means it did not, and the log's last line says why.
`--no-pr` gives them the branch without the pull request.

If they want the pull request anyway — the run was interrupted, or they fixed a
parked issue themselves — `bd-auto handoff` opens it later, from the main
checkout with the epic branch checked out. Report that it exists; do not run it
over a refusal on their behalf. `--force` is theirs to type.

To stop early: `bd-auto run pause` / `resume` halts at the next wave boundary.
Killing the background command keeps the branches and worktrees, and relaunching
resumes those issues rather than restarting them.
