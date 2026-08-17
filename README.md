# bd-auto

A binary that drains a set of [beads](https://github.com/gastownhall/beads)
issues by running **one worktree-isolated model process per issue**, in
dependency-ordered waves, with a configurable per-issue pipeline and an
integrator at each wave barrier.

## The problem it solves

On a long multi-step task, the session's context grows until autocompact fires
and the model forgets its instructions — even though everything it needed was
already written down in the issues.

bd-auto fixes this by having no such session. The control flow lives in Go, not
in a context window:

- **Each issue runs in a fresh model process.** The worker reads the issue from
  beads itself, does the work in its own worktree, and exits. Nothing about it
  accumulates anywhere.
- **Beads and `.beads/auto/run.json` are the state.** An interrupted run resumes
  from disk, because there was never a conversation to lose.
- **Watching costs a fixed amount.** `bd-auto run status --context` is four
  lines whatever the epic's size, so a session that launches a drain and follows
  it grows by a bounded amount rather than by the size of the work.

Issues the DAG says are independent run in parallel, each in its own git
worktree, so parallel implementation is safe rather than a merge hazard.

## Install

```bash
make build      # produces bin/bd-auto
```

Put `bin/` on your `PATH` and run it. It is an ordinary command-line tool and
needs nothing else.

Optionally, install it as a Claude Code plugin, which adds a `/bd-auto` skill
that launches a drain in the background and watches it for you:

```bash
claude --plugin-dir /path/to/beads-auto-imp
```

`make install-check` runs `claude plugin validate .`.

## How a run works

```
drain ──> plan wave ──> run N workers in parallel, one worktree each
             ▲                    │
             │              each worker: own worktree → implement →
             │              gate → commit → close
             │                    │
             │              review stage (fail → same session, same
             │              worktree, max 3 rounds; then a fresh attempt)
             │                    │
             └──── integrator ◄───┘  merge wave onto the epic branch, in
                   (wave barrier)     dependency order, resolve conflicts,
                                      gate the merged result
                          │
                          ▼
                   handoff: push the epic branch, open a pull request
```

The loop repeats until every issue in scope has landed or parked. All of it
happens inside one `bd-auto drain` process, which is resumable: kill it and
re-run the same command, and the interrupted issues pick up where they were.

## Where the work ends up

A drain publishes nothing on its own. Every issue branch is merged onto **one
temporary epic branch** — `bd-auto/epic/<epic>-<timestamp>` — and the branch you
were on is never written to. The checkout stays on the epic branch for the rest
of the run, which is also what lets a second wave build on the first.

The pull request is the handoff, and it opens only when the run is genuinely
finished:

- every issue in scope landed, none parked;
- the run ended on its own terms, not on an interrupt or an outage;
- the gate is green on the fully merged epic branch.

Anything else opens nothing and leaves the epic branch exactly where it is, with
everything that did land on it. A refused handoff is never destructive: the
reason is in the run report and in `bd-auto run status`, and the branch is there
to look at.

The run leaves your checkout on the epic branch, and deletes nothing. Going back
is `git switch <your branch>` when you are ready — bd-auto does not do it for
you, because by then the working tree holds the whole run and a switch that
fails halfway is a worse ending than one you asked for.

Both halves are switchable, and they are two switches rather than one:

| | epic branch | pull request |
|---|---|---|
| default | yes | yes |
| `--no-pr`, or `handoff: {pr: false}` | yes | no |
| `--no-epic-branch`, or `handoff: {branch: false}` | no | no |

`pr: false` is for a repo with no remote, no `gh`, or a review that happens
somewhere else: the epic branch is then the whole deliverable. `branch: false`
is the escape hatch back to merging straight into your own branch as each wave
finishes, and it turns the pull request off with it — there would be nothing to
open one from. `pr: true` with `branch: false` is refused at config load rather
than silently resolved.

The push runs in the main checkout, which is the only place bd-auto pushes from.
Worker worktrees keep the gitguard blocks described under [Design notes worth
knowing](#design-notes-worth-knowing); nothing about the handoff loosens them.

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
    agent: reviewer            # a runner role; bd-auto spawns the model
    max_rounds: 3
  - stage: security            # your own pipeline, as a command
    run: ./scripts/security-review.sh
    optional: true

runners:                       # how each role's model is run
  default:
    provider: claude
    model: opus
  reviewer:
    model: sonnet
    permissions: scoped
    resume: false              # judge the diff fresh each round

concurrency: 5                 # issues in flight per wave
autonomy: auto                 # auto | wave (pause at each wave barrier)
retry: 1                       # retry once fresh, then park
discovered_work: defer         # keep new findings out of the current run

handoff:                       # where the finished run ends up
  branch: true                 # stage on a temp epic branch, never on yours
  pr: true                     # open a pull request once it is all green
  remote: origin
  prefix: bd-auto/epic/
```

### Adding your own stage

A stage is either `agent: <role>` (a model, run with that role's entry under
`runners:`) or `run: <command>` (executed by `bd-auto`, must exit 0). `run:`
stages get `$BD_ISSUE`, `$BD_BRANCH`, `$BD_WORKTREE`, `$BD_REPO_ROOT` and
`$BD_DIFF_FILE`.

`agent:` used to name a Claude Code subagent to dispatch. It now names a runner
role that this binary spawns itself — the same field, a different meaning — so
config load rejects a name that is not a defined role and lists the ones that
are, rather than failing at the moment it would have spawned something. The
built-in roles are `worker`, `reviewer` and `integrator`; add your own by adding
a key under `runners:`. The three plugin-era names (`bd-worker`, `bd-reviewer`,
`bd-integrator`) still resolve to those roles, so an old config keeps loading.

A failing stage feeds its output — truncated to a fixed budget — into the next
round, so the worker resumes informed without a human reading a test log.

### Runner roles

Each entry under `runners:` resolves over `default`, so it only names what it
changes. Anything set on `default` beats a built-in role default.

| Field | Meaning |
|---|---|
| `provider` | which runner adapter spawns the model |
| `model` | passed to the backend unchanged |
| `permissions` | `scoped`, `auto` or `bypass` |
| `allowed_tools` | the tool list under `scoped` |
| `timeout` | seconds bounding one invocation; `0` is unlimited, and is the default |
| `resume` | whether feedback rounds continue the same session |
| `extra_args` | the per-backend escape hatch |

## What a repo needs

Nothing, beyond `bd` and a git repo. Worker worktrees are created under
`.beads/auto/worktrees/`, which is where run state already lives and is already
gitignored, so a drain leaves no untracked files in the main checkout.

You do **not** need to copy `.beads/` into a worktree: `bd` resolves the main
repo's database from inside one by itself.

## Commands

```bash
bd-auto init [--force] [--dir <path>]   # write a starter .beads-auto.yaml

bd-auto drain --epic <id>           # pick a scope, then run it to completion
bd-auto drain --epic <id> --all     # scope the run to every candidate
bd-auto drain --issues a,b,c        # scope the run to named issues
    [--concurrency N] [--autonomy auto|wave] [--rounds N] [--retry N]
    [--base <ref>] [--no-pr] [--no-epic-branch]
    [--plain] [--json] [--dry-run] [--quiet]

bd-auto run start --epic <id> [--concurrency N] [--autonomy auto|wave]
bd-auto run status [--context] [--wait <duration>]
                                    # --context is four lines instead of JSON;
                                    # --wait blocks until the run stops
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
bd-auto integrate [--all] [--quiet] # the wave barrier, in this process: merge in
                                    # dependency order onto the epic branch,
                                    # gate the merged result, clean up, close
                                    # the epic if it is finished. The pull
                                    # request is a whole-run step, so `drain`
                                    # opens it and this does not.
bd-auto config show
```

## Watching a run

On a terminal, `bd-auto drain` draws a live wave table: one row per issue in
scope, what each worker is doing right now, how long it has been doing it, and
what it has cost so far — per issue and for the whole run.

```
bd-auto drain · beads-auto-imp-wz9 · wave 2 · 4 issue(s) in scope

  ISSUE                  WAVE STATE      TIME     COST  ACTIVITY
  wz9.1                  1    done      2m43s  $0.8135  finished
  t-2                    2    running     25s  $0.4210  Edit
> t-3                    2    running     23s  $0.1130  Bash
  t-4                    2    waiting       -        -  queued

2 running · 1 done · 0 parked · 0 killed · run total $1.3475
↑/↓ select · k kill the selected worker · q stop the run
```

The activity column is text-granular: between tool calls it follows the message
the model is writing, so a worker that is thinking looks different from one that
has stalled.

| Key | What it does |
| --- | --- |
| `↑` / `↓` | move the selection |
| `k` | kill the selected worker. The process **and everything it started** die, and the issue is parked and reported failed. The rest of the wave carries on. |
| `q` / `ctrl-c` | stop the run. Nothing is parked and nothing is judged: worktrees, branches and sessions all survive, and re-running `drain` resumes the interrupted issues rather than restarting them. Press it again to leave the view while the run winds down. |

The cost is **displayed, never enforced**. There is no budget anywhere in this
engine — the scope you chose before anything was spawned is what bounds the
spend. This is so you can watch it and change your mind.

Off a terminal, and under `--plain`, `--json` or `--quiet`, the table is never
built and the run falls back to the line-per-event renderers. They carry the
same facts, so nothing a headless run needs is only visible here.

### Watching from another process

A drain launched in the background is followed with `run status`, which reads
`.beads/auto/run.json` and needs nothing from the drain itself:

```bash
bd-auto run status --context --wait 30m
```

`--context` prints four lines instead of JSON; `--wait` blocks until the run
leaves `active`, or for that long, whichever comes first. Together they are one
command per half hour of run rather than one every couple of minutes, and the
output is the same size whether the epic has four issues or four hundred: counts
for everything, names only for what is running now and what is parked.

That bound is the point, and it is what the `/bd-auto` skill is built on. A
session that launches a drain and follows it to the end grows by about **1.8k
tokens for a six-hour run, at any epic size** — 879 tokens of fixed cost (the
skill body, the launch call, the closing report) plus 154 tokens per hour of
run. `make launch-cost` adds it up and fails if it exceeds 2k:

```
skill body (SKILL.md)                 2379      679
launch tool call                       200       57
final report                           500      142
  fixed subtotal                      3079      879
per hour of run (one poll)             540      154
```

Nothing in that table has an issue count in it. The skill body is fixed, the
poll view is capped, and the poll *count* follows the clock rather than the
work — which is what makes the claim hold for four issues and four hundred
alike. The per-hour figure is the poll view's worst case, both named lists
saturated; a typical run prints about half that, and the 2k budget then covers
ten hours rather than seven.

## Stopping a run

`bd-auto run pause` holds the run at the next wave boundary — the wave in
flight finishes and merges first; `resume` releases it. `bd-auto run stop` ends
the run and deletes its state, so the next drain
starts fresh rather than resuming this one. Add `--keep-state` to stop but keep
the record of what landed.

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
gets a full retry budget, and lets the next wave offer it again. The failure
notes stay on the issue — the record of why it failed is the point.

If the run has already finished, there is nothing to unpark: fix the issue,
`bd update <id> --status=open --remove-label=human`, and start a new run.

## Design notes worth knowing

**One writer per issue, always.** A concurrency spike (`eqc.1`) found that
concurrent `bd note` calls against the *same* issue silently lose writes and
still exit 0. Five workers on five different issues are safe; two writers on one
issue are not. So the engine never writes to an issue while its worker is
running, and the run state — which parallel goroutines and separate bd-auto
processes *do* write concurrently — is guarded by a real `flock`, with a
cross-process regression test.

**Workers cannot merge, structurally.** Each worktree gets generated git hooks
that refuse push, merge and rebase, plus a push URL with no remote helper behind
it — see `internal/gitguard`. It is a real rejection rather than an instruction
a model can talk itself out of. Because a fast-forward merge creates no commit
and `git rebase` has no hook at all, a second, post-hoc half checks the finished
branch: base still an ancestor, no merge commits, every commit carrying this
attempt's trailer. Both halves fail closed — an unverifiable branch is a failed
branch.

The generated hooks **chain**. `core.hooksPath` is already set in this repo, to
beads' own hooks, and overwriting it would silently disable the pre-commit that
keeps `issues.jsonl` in sync inside every worker worktree. So each generated
hook rejects what bd-auto blocks and otherwise `exec`s the same-named hook under
the previous path.

**Workers cannot lie about finishing.** A worker's report is not evidence. The
engine asks beads whether the issue actually reached a terminal state, and the
gate whether the branch actually passes.

**Scripts must not assume there is no run.** `scripts/smoke.sh` tears down run
state as part of its cleanup, and run state is shared with the main checkout
even from inside a worktree — so running it during a live drain would have
destroyed that drain. It now refuses to start while a run is active. Anything
else you add that writes `.beads/auto/` needs the same check.

## What the dogfood run showed

bd-auto drained its own epic (`beads-auto-imp-eqc`, 13 issues) back when it was
a Claude Code plugin and a live orchestrator session ran the loop. That
architecture is gone — the engine below is the binary — but one finding from it
set a default that is still in force.

The wave contained a single issue: the merge guard. It took **three** review
rounds to pass. Anchoring the guard's pattern to command position fixed an
over-firing bug but let `sudo git push` and `env FOO=1 git push` through;
stripping quoted spans fixed that but let `eval "git push"` through, and an
unbalanced quote blanked the rest of the command. Round three — fall back to
matching the raw command whenever the shell parse is uncertain — passed.

This is why `max_rounds` defaults to **3**. At the original 2 this issue would
have been parked with a working guard replaced by a broken one. The guard itself
has since been replaced by the structural one above, but the shape of the
finding survives it: real work sometimes needs a third round, and the cost of
allowing one is far below the cost of parking.

**Still unproven live** at any scale: parallel workers under contention, an
integrator resolving a genuine conflict between two workers, and retry-then-park.
All are covered deterministically by `make smoke` and the drain package's tests;
none has been watched with several real models in flight. Point a run at three
or four genuinely independent issues, with `--autonomy wave` so there is a
barrier to inspect, before trusting it with a large epic.

## What the resume-versus-fresh measurement showed

`max_rounds` sends failed work back to the worker that did it; `retry` throws
that worker away and starts another. Which one should be the main recovery path
was an argument, so it was measured — `scripts/resume-vs-fresh.sh`, or
`make resume-vs-fresh`.

It drains one fixture epic twice from the same commit, `max_rounds: 1, retry: 3`
against `max_rounds: 4, retry: 1`, over a stage no worker can pass on its first
round. Both arms therefore recover once per issue and spend the same six model
processes; the only difference is whether the second process continues the first
or replaces it.

| arm | `total_cost_usd` | wall clock | attempts |
|---|---|---|---|
| fresh, `max_rounds 1, retry 3` | $1.7210 | 264s | 6 |
| resume, `max_rounds 4, retry 1` | $1.4055 | 195s | 3 |

Recovering in the session is **18% cheaper and 26% faster**, on all three issues
rather than on one outlier. The expected objection — a resumed session re-sends
its whole transcript, so its input is strictly larger — is true per turn and
wrong in total: the resume arm read 2.0M cached tokens against 3.0M and produced
half the output. A fresh worker re-runs its exploration, and then re-sends every
result of that exploration on every remaining turn.

So `max_rounds` is the primary knob and `retry` is the safety net for a session
that has gone wrong in itself. Compare `total_cost_usd`, never summed tokens:
cache reads bill far below input price, so a token count flatters whichever arm
reads more cache. Re-run it when the worker prompt or the default model changes.

## Development

```bash
make check        # build + vet + test, the same commands the gate runs
make smoke        # end-to-end run against a throwaway epic it creates and deletes
make launch-cost  # what a drain costs the session that launches it
```

`make smoke` covers what unit tests cannot: talking to `bd`, reading a real DAG,
driving run state across processes, and the whole command surface end to end. It
refuses to run while a drain is active, because its cleanup would delete that
run's state.

When bd-auto drains its own epic, `bd-auto` on a worker's PATH is the **main
checkout's** `bin/bd-auto` — the binary the running drain is using, not the
worker's build. A worker changing bd-auto itself must `make build` and then
invoke `./bin/bd-auto` to exercise its own change; typing `bd-auto` silently
tests the old binary.
