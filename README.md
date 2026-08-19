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

Before the first wave, the run checks the backends it is about to spawn: one
`claude --version` and one trivial `-p` call, per distinct runner configuration,
built by the same code that builds a worker's. A CLI that is missing,
unauthorised, or no longer accepts a flag bd-auto passes fails there — one
error, before a worktree, a branch or a claimed issue exists. Without it that
same failure arrives once per worker, as a process that dies before printing a
result, which the engine reads as an outage and retries. `--no-preflight` skips
the check.

A wave is not a fixed list. `concurrency` is a cap on workers in flight, not a
batch size: when a worker finishes — done, parked, or killed — the run asks bd
what is ready and puts the next in-scope issue into the freed slot, in the same
wave. An issue that parks in its first minute costs a minute, not a wave. What
waits for the barrier is only what needs it: an issue that depends on one of
this wave's own issues is held back, because its dependency's branch is not in
`HEAD` until the barrier merges it.

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

`bd-auto handoff` opens that pull request afterwards. It is for the two ordinary
ways a branch ends up finished with no pull request in front of it: a run that
was interrupted, and a run whose parked issue you unparked and did yourself. Run
it in the main checkout, on the epic branch. It reads the branch and the base out
of run state, works out what actually landed by asking git rather than by reading
the run's own done list, re-runs the gate on the branch as it stands now — the
point of handing over late is that something happened late — and then asks the
same question `drain` asks.

`--force` opens it over a refusal. It only overrides a judgement about the run:
that it did not finish, that something is parked, that the gate is red, that
`pr: false` said not to. It cannot conjure an epic branch, and it will not
publish one with nothing on it. A forced pull request says so at the top, names
what was parked, and prints a red gate as a red gate — the point is a review
request a human asked for, not one that reads as if bd-auto approved it.

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
discovered_work: triage        # triage | defer | immediate — see below

handoff:                       # where the finished run ends up
  branch: true                 # stage on a temp epic branch, never on yours
  pr: true                     # open a pull request once it is all green
  remote: origin
  prefix: bd-auto/epic/

ask:                           # letting a worker ask you a question
  enabled: true
  timeout: 3600                # seconds a question waits; 0 waits forever
  hold: 300                    # seconds one tool call blocks before a ticket
  roles: [worker, integrator]  # not the reviewer; see below

graph:                         # a code index for the roles — off, see below
  enabled: false
  exclude_tests: true
  refresh: true                # rebuild at each barrier that merged something
  roles: [worker, reviewer, integrator]
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
a key under `runners:`.

**Breaking in 0.2.0:** the plugin-era names `bd-worker`, `bd-reviewer` and
`bd-integrator` no longer resolve to those roles. A config still using one fails
to load, naming the roles it may use instead. Rename it to the role — usually
`bd-reviewer` to `reviewer` — or, if you want the old name, define it as a role
of its own under `runners:`.

A failing stage feeds its output — truncated to a fixed budget — into the next
round, so the worker resumes informed without a human reading a test log.

### Runner roles

Each entry under `runners:` resolves over `default`, so it only names what it
changes. Anything set on `default` beats a built-in role default.

| Field | Meaning |
|---|---|
| `provider` | which runner adapter spawns the model |
| `model` | passed to the backend unchanged |
| `permissions` | `scoped`, `auto` or `bypass`; `auto` is the default, `scoped` for the reviewer |
| `allowed_tools` | the tool list under `scoped` |
| `denied_tools` | tools refused at every level; checked ahead of `permissions` |
| `timeout` | seconds bounding one invocation; `0` is unlimited, and is the default |
| `resume` | whether feedback rounds continue the same session |
| `extra_args` | the per-backend escape hatch |

### Permissions

`permissions` defaults to `auto`, and `scoped` for the reviewer. Nothing bd-auto
ships turns permission checks off by itself; widening them is your decision.

Know the cost of the default before your first run. Headless there is nobody to
answer a permission prompt, so under `auto` a worker is refused every write and
every shell command, spends its attempt looking for a way round, and comes back
having changed nothing. `acceptEdits` is no better here: it grants the edits but
still refuses the plain shell the gate, `git` and `bd` all need. **A worker can
only finish under `bypass`.**

So a real drain needs `permissions: bypass` set for this repo under `runners:`,
or `--dangerously-skip-permissions` for one run. What keeps a worker in bounds
once you do is structural rather than a prompt: a throwaway worktree per issue,
a branch per issue, git hooks that refuse push, merge and rebase, and a scope
you confirmed before anything was spawned. Leave the reviewer `scoped` where you
can — it only reads, and a reviewer that can run bare `Bash` is a reviewer that
can push.

`denied_tools` is the part of a role's scoping that a widened `permissions` does
not switch off, because deny rules are checked before the permission level is.
The reviewer's default list is every `bd` verb that writes the record, and it is
there because the alternative has been measured too: under the backend's own
`auto` classifier a review judged `bd close` on the issue it was reviewing to be
a reasonable thing to run, and ran it.

`bd-auto drain` and `bd-auto issue run` both take
`--dangerously-skip-permissions`, which forces `bypass` on every role for one
run, the reviewer included. It is the one-line answer to a run that is stuck on
permissions; it is not the way to configure a repo, which is what the
`permissions` field above is for.

A round that was refused a tool and then changed nothing is reported as an
environment failure rather than as failed work: it costs the issue neither a
round nor an attempt, nothing is parked, and the run stops with the permission
level to change.

### The code index

Off by default, and it stays off until somebody measures whether it pays.

Every model a drain spawns starts knowing nothing about the repo, and each one
re-derives the same map with grep, glob and read. That is paid for in the only
currency this engine spends: transcript size times turns, since every turn
re-sends the whole transcript as cache reads. One drain billed 2,405,147
cache-read tokens against 22,450 output tokens.

With `graph.enabled: true` and [graphify](https://github.com/gastownhall/graphify)
on `PATH`, a run extracts a code index once at the start and rebuilds it at each
barrier that merged something. It is pure AST extraction — no model, no API key,
measured at 1.9s for this repo — written under `.beads/auto/graph/`, which is
already gitignored, so no worker's `git add -A` can commit a three-megabyte
graph. Roles named in `graph.roles` get a prompt section naming the index and one
allowlist entry, `Bash(graphify:*)`.

The premise only half survived the planning that measured it. A broad `graphify
query` returns a truncated list of symbol locations rather than an explanation,
so the index is a typed, cross-referenced grep and an agent still has to read the
file: the saving is on searching, not on reading. The prompt says exactly that,
names the four commands that are cheap and exact — `god-nodes`, `explain`,
`affected`, `path` — and tells the role that every fact the index gives is a
claim about code it has not read.

Everything fails open. No graphify, or a build that fails, means no index, no
prompt section and no allowlist entry, and the run drains exactly as it does
without any of this. `bd-auto config show` reports `graph.built`, which is the
only way to tell an index that exists from one that was merely asked for.

Whether it pays for itself is not yet measured. `scripts/graph-ab.sh`, or `make
graph-ab`, is the experiment: it clones this repo twice, drains the same three
documentation issues through both, and compares `total_cost_usd` with the index
off and on. The tasks ask a worker to name every function on one path with its
file and line, which is the shape of work where an index should win if it wins
anywhere. `--dry-run` builds both fixtures and spawns nothing, and the script
refuses to start without graphify — an `on` arm with no index is the `off` arm
under another name, and the report would print noise as a result.

Indexing a tree it was only asked to read costs three corrections, each measured
rather than assumed. `.graphifyignore` is read from the repository, not from
`--out`: written into the output directory the exclusions did nothing, and this
repo's most-connected nodes came back as `testRepo`, `newIssues`, `testCfg` and
`engine` — the test harness rather than the architecture. `graphify update` takes
no `--out`, so pointing it at the index directory re-extracted the index
directory: one refresh took the graph from 2198 nodes to 1956 and reported that
the only source file it had found was `stamp.json`, which on every barrier would
erode the index toward nothing. And `extract` leaves an incremental cache beside
the source whatever `--out` says. So bd-auto writes the ignore file into the
working tree for the second the extraction takes and removes it again, never
touching a `.graphifyignore` the repository already has, rebuilds in full rather
than updating, and sweeps up a `graphify-out/` it created. After two extractions
`git status` reports nothing, which is what the end-to-end test asserts.

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
    [--base <ref>] [--no-pr] [--no-epic-branch] [--no-preflight]
    [--plain] [--json] [--dry-run] [--quiet]
    [--dangerously-skip-permissions]

bd-auto run start --epic <id> [--concurrency N] [--autonomy auto|wave]
bd-auto run status [--context] [--wait <duration>]
                                    # --context is four lines instead of JSON;
                                    # --wait blocks until the run stops
bd-auto run stop | pause | resume
bd-auto run unpark --issue <id>     # put a parked issue back into the run

bd-auto issue run --issue <id> [--base <ref>] [--rounds N] [--retry N] [--quiet]
    [--dangerously-skip-permissions] # force bypass on every role for this run
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
bd-auto handoff [--force] [--quiet] # open the pull request for a run that has
                                    # already finished: re-gate the epic branch
                                    # as it stands now, then the same decision
                                    # `drain` makes at the end of a run.
                                    # --force opens it over a refusal you have
                                    # looked at and disagree with.
bd-auto config show

bd-auto hook <event>                # the Claude Code hook entry point. Reads
                                    # the hook payload on stdin and exits 0 for
                                    # every event, known or not. Called by
                                    # Claude Code, not by hand — see "A Claude
                                    # Code hook fails open, always" below.
```

## Watching a run

On a terminal, `bd-auto drain` draws a live wave table: one row per issue in
scope, what each worker is doing right now, how long it has been doing it, and
what it has cost so far — per issue and for the whole run.

```
bd-auto drain · beads-auto-imp-wz9 · wave 2 · 5 issue(s) in scope

  ISSUE                  WAVE ATT STATE             TIME     COST  ACTIVITY
  wz9.1                  1    1   done             2m43s  $0.8135  finished
  t-2                    2    1   reviewer (0)       25s  $0.4210  Read internal/store/store.go
> t-3                    2    2   worker (1)         23s        -  Bash
  t-4                    2    1   gate (1)         1m04s  $0.6602  the gate stage is running
  t-5                    2    -   waiting              -        -  queued

3 running · 1 done · 0 parked · 0 killed · run total $1.8947
↑/↓ select · enter transcript · k kill · q stop the run
```

The state column names the process, not merely the fact of one. An issue is a
worker, then the gate, then a reviewer, then a worker again if the review sent
it back — and which of those is in flight is what says whether a slow row is
slow for a reason. `worker` and `reviewer` are models spending money; `gate` is
`go test ./...` running with no model anywhere, and a stage a repo added to the
pipeline appears under its own name. `killing` and `asking` displace it, because
a row that is dying and a row waiting on a person are more urgent than which
process it is.

The number after it is the round: which turn of that process this is, counted
per stage and from zero. `worker (0)` is a first draft and `worker (1)` is one
fixing what came back; `reviewer (2)` under `max_rounds: 3` is a review about to
run out of budget. A finished row shows no round, because nothing is running to
count.

`ATT` is the other number, and it is not the same one. A round is another turn
in the same worktree and the same session; an attempt is that worktree and
session discarded and started again from the issue. `worker (1)` on attempt 2 is
a run that has now spent two sessions on one issue, and reading the state cell
alone you would not know it. A row shows a dash there until something has run:
a resumed run starts at whichever attempt it left off on, and the table would
rather say nothing than guess.

That last point is what the column is for. The gate and any `run:` stage spawn
no model and so stream nothing, and a row with nothing to show used to go on
showing the worker's last tool call with the clock climbing — indistinguishable
from a worker that had hung, which is the one reading this display exists to
prevent.

The activity column is text-granular: between tool calls it follows the message
the model is writing, so a worker that is thinking looks different from one that
has stalled.

A cost appears when a model process ends, because that is when the backend
reports one: the claude CLI puts it on its result line and nowhere else. So an
issue in its first stage shows a dash however long it has been running — `t-3`
above — and one in a later stage shows what its earlier stages cost, which is
`t-2`. Only the run total and a finished row are ever the whole story.

| Key | What it does |
| --- | --- |
| `↑` / `↓` | move the selection |
| `enter` | open the selected issue's transcript. `esc` comes back, with the cursor where you left it. |
| `k` | kill the selected worker. The process **and everything it started** die, and the issue is parked and reported failed. The rest of the wave carries on. |
| `q` / `ctrl-c` | stop the run. Nothing is parked and nothing is judged: worktrees, branches and sessions all survive, and re-running `drain` resumes the interrupted issues rather than restarting them. Press it again to leave the view while the run winds down. |

### The transcript

A row is one line, and that line is the last thing that happened. `enter` opens
the rest of it: what the models actually did, arranged the way a Claude Code
session reads.

```
t-3 · kv get, set and del
worker · 4m12s · $0.9214 · lines 118-155 of 155

── worker · attempt 1 · round 0 ─────────────────────────────────────────────

Reading the store interface first, so get, set and del agree on what a missing
key is before any of them is written.

⏺ Read(…/internal/store/store.go)
  ⎿  package store
     …
     +6 more lines

⏺ Edit(…/internal/cli/get.go)
  ⎿  String to replace not found in the file.

↑/↓ scroll · pgup/pgdn page · g/G ends · esc back to the table
```

It is read off the transcripts every model writes to
`.beads/auto/logs/<issue>-a<attempt>-r<round>-<role>.jsonl`, not off the live
event stream, and that is what lets it show things the stream cannot: a tool
call's **arguments** rather than only its name, the earlier rounds, the earlier
attempts, and the reviewer and integrator that ran after the worker. Each
process is separated and named, so a third round does not read as a
continuation of the second.

It opens at the end, where whatever is happening now is, and stays pinned there
as the worker writes more — until you scroll up, after which it holds still. The
run is not paused underneath it: the table is folding events in the whole time,
and `esc` shows what arrived. A question from any worker still takes the keys,
so `enter` answers the question rather than opening anything.

What it holds is bounded on purpose. Each transcript is followed from a byte
offset rather than re-read, only the last few hundred entries are kept, and a
tool result keeps its head with a count of the lines it cut. Everything dropped
is said out loud rather than silently missing.

The cost is **displayed, never enforced**. There is no budget anywhere in this
engine — the scope you chose before anything was spawned is what bounds the
spend. This is so you can watch it and change your mind.

Off a terminal, and under `--plain`, `--json` or `--quiet`, the table is never
built and the run falls back to the line-per-event renderers. They carry the
same facts, so nothing a headless run needs is only visible here — the round and
the attempt included: every event under `--json` carries `attempt` and `round`
where it has them, and the plain renderer says both in words on the stage lines
that bracket a turn. Both keys are omitted where they mean nothing, so a reader
that ignores them sees the stream it saw before.

### The barrier

A barrier is work: it merges every branch in dependency order, spawns a model
for any conflict, gates the merged result and — when that gate comes back red —
peels the merges back off one at a time until it finds the branch to blame. It
can run for minutes and it spends real money, so it gets a block of its own
under the wave, in the same columns.

```
── wave 2 barrier ───────────────────────────────────────────────────────────
  kv-ctf.2               2    -   merged              3s        -  clean, no conflicts
  kv-ctf.4               2    -   resolving          47s  $0.0210  Edit(internal/wave/plan.go)
  kv-ctf.7               2    -   waiting              -        -  queued
  gate                   2    -   running            12s        -  go build ./... · go test ./...
```

A branch whose conflict a model is resolving shows that model's live tool calls
on its row, exactly as a wave row shows its worker's — which is the whole
difference between a barrier that is working and one that has hung. `enter` on
it opens the integrator's transcript, because the integrator writes into the
transcript of the issue whose branch it is merging. The gate is the one row with
nothing to open: it spawns no model, and its row exists precisely because it
would otherwise be a whole test suite of nothing happening on screen.

A red gate is rendered as what it is. The branch being peeled off says `rolled
back`, the gate runs again on the tree beneath it, and the branch whose removal
fixed it is parked with the gate's output as its reason — the gate row naming
it. Nothing is wrong with that work: it is still on its own branch, and the next
barrier merges it again once the issue it broke is fixed. The branches peeled
off after it are not what the gate was red about, so they go straight back on,
the tree is gated once more, and their rows return to `merged`: a red branch
parks itself rather than everything that happened to follow it. A base that was
already red blames nobody, and every row goes back to `merged`.

What the barrier spent appears as its own figure in the summary line as well as
inside the run total. It belongs to no issue, so no other number on that line
counts it.

Each wave row follows the barrier's verdict rather than its worker's: an issue
whose branch would not merge says `parked`, however well its worker did.

### Answering a worker's question

A worker that hits a genuine ambiguity can ask you, and get an answer back
without its session ending. The question appears under the table:

```
  ISSUE                  WAVE ATT STATE             TIME     COST  ACTIVITY
  t-1                    2    1   asking           4m12s  $0.9014  ask_user
  t-2                    2    1   worker (0)         25s  $0.4210  Edit

╭──────────────────────────────────────────────────────────────╮
│ t-1 asks · Config key                                        │
│ Which key should the request timeout live under?             │
│                                                              │
│ > 1. ask.timeout — a block of its own, next to ask.hold      │
│   2. runners.timeout — reuse the per-role timeout            │
│                                                              │
│ 1-2 or ↑/↓ and enter to answer · t type your own · s let it  │
│ decide · esc dismiss                                         │
╰──────────────────────────────────────────────────────────────╯
```

Only the asking worker is blocked; the rest of the wave keeps running, and its
row says `asking` so a stopped clock does not read as a hung process. Several
workers can ask at once — the questions queue, oldest first, and the box says
how many are behind. While one is up the table's own keys are suspended, so a
digit cannot kill a worker by accident.

Three things matter about the design, and all of them are about the case where
you are **not** at the keyboard:

- **A headless run never stalls.** Under `--quiet`, `--plain`, `--json` or off a
  terminal there is nobody to ask, so the tool returns at once telling the model
  to decide for itself and record the assumption in the issue. An unattended
  drain degrades to exactly what it did before this existed.
- **A question outlives any tool timeout.** One call blocks for five minutes and
  then hands the model a ticket to poll with, so an hour away from the desk
  costs a dozen cheap round-trips rather than a tool call the backend kills.
  Claude Code's own limit is thirty minutes idle, and the ticket is what makes
  that irrelevant.
- **It times out anyway.** A question waits an hour by default and then tells
  the model to proceed. Nothing can wait forever unless you set `ask.timeout: 0`
  and mean it.

Answers are written to `.beads/auto/run.json`, so an interrupted run that is
re-run does not ask you the same thing twice — the worker is a fresh process
with no memory of having asked, and this is the only place that memory lives.
Unanswered questions are not recorded as answers: a question nobody was there
for is asked again on a run where somebody is.

The reviewer does not get the tool. It is read-only and judging someone else's
work, and a reviewer that can question the author is no longer an independent
check. `ask.roles` changes that if you disagree; `ask.enabled: false` turns the
whole thing off.

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

A worker that parks *itself* — `bd update <id> --status=blocked`, which is what
the worker prompt tells it to do when it genuinely cannot proceed — is a
different case, and costs nothing further. It is a verdict rather than a failure
to produce one, so bd-auto stops there: the remaining attempts are not spent
asking a fresh worker the same question, the branch is not merged, and the issue
is parked carrying what the worker said about why.

One thing a worker may never park *for* is another issue in its own wave. A
wave is bd's ready front narrowed to the run's scope, so bd has already said
that no issue in it waits on another one — and each branch is cut from the base
rather than from a sibling's. The worker prompt says so, and if a park reason
names a wave sibling anyway, bd-auto reports it as what it is: a `blocks` edge
the graph is missing. It lands in the run's notes and in `missing_deps` on the
drain report, with the `bd dep add <issue> <sibling>` a human would run. The
edge is never added automatically — a graph edited on the strength of one
model's sentence is worse than a graph that is short an edge, because every
later run believes it.

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

`pre-commit` is the one that does more than chain. It runs beads' hook rather
than `exec`ing it, and then takes `.beads/issues.jsonl` and
`.beads/interactions.jsonl` back out of the index it is about to commit. Those
are a re-export of one database every worker in the wave writes to, so a commit
carrying one carries every other worker's issue churn with it, and every branch
in the wave then conflicts on the same file at the barrier. The export stays in
step — beads' hook still ran — it just is not the worker's to commit. Where the
integrator meets that conflict anyway, on a branch cut before this or in a
checkout that resolves `.beads` differently, it settles it the same way: keep
the copy the branch being merged into already had, and spend no model call on a
file `bd export` regenerates in full.

**Workers cannot lie about finishing.** A worker's report is not evidence. The
engine asks beads whether the issue actually reached a terminal state, and the
gate whether the branch actually passes.

**bd-auto's own git fires no hooks.** beads sets `core.hooksPath` and installs
post-checkout and post-merge hooks that run `bd hooks run`, which imports
`.beads/issues.jsonl` over the Dolt database. The jsonl is a passive export, so
that import replays whatever was exported onto whatever is in the database —
reverting every bd write since, silently, with a zero exit code. Observed here:
one `git pull --rebase` took eight issues from closed back to open.

A drain fires those hooks constantly — a worktree per attempt, a merge per issue
at every barrier — and the cost is not cosmetic. The epic-close predicate
requires every child issue to read as closed, so an epic whose children were
quietly reopened never closes and the drain never reaches an end.

So every git command bd-auto runs goes through `internal/gitx`, which passes
`-c core.hooksPath=<nowhere>`. The suppression binds to one invocation: a
worker's own git in its worktree still fires the guard hooks and beads' hooks
behind them, and a human's git is untouched. bd-auto declines to fire a
repository's hooks on its own behalf rather than disabling them for anyone else.

`internal/gitguard` is the one package that does not use it, and deliberately:
it reads `git config --get core.hooksPath` to chain its rejectors, and that read
reports command-line config like any other, so going through `gitx` would make
it chain to the sentinel. It only runs `config` and `rev-parse`, neither of
which fires a hook, so it has nothing to suppress.

The barrier then re-asserts run state onto bd before deciding the epic, covering
the causes the run does not control — a worker's git, a human in the same
checkout, any other beads import. It moves only in the direction of what the run
finished: an issue the run never judged is left alone, because a human reopening
something mid-run is making a decision, not suffering a hook.

**Discovered work is filed by the barrier, not by the worker.** A worker that
runs its own `bd create` is alone in two ways. It cannot see what any other
worker has filed, so a fault several issues touch is filed once per worker that
trips over it — two of this repo's own open issues are that same finding, filed
three waves apart in almost the same words. And it is a bd write from inside a
worktree, which is exactly the class of write the import hooks above revert.

Instead the worker writes JSON to a path bd-auto names in its task, in the main
checkout so nothing a `git add -A` can reach ends up committed to its branch.
bd-auto harvests that file at the end of every attempt — whatever the attempt
came to, since a failed one still did the exploring — and files what it holds at
the barrier, deduplicated by normalised title against both the rest of the run
and every issue already in bd, closed ones included.

Under the default `discovered_work: triage` the barrier files nothing at all. A
finding is staged in `.beads/auto/triage.json`, which outlives the run that
found it, and `bd-auto triage` is what turns one into an issue (`--accept`),
folds it into an issue that already exists (`--into`), or discards it with a
reason. A discarded finding is kept along with the reason, because the record of
what a run decided not to file is the only evidence for whether the bar is in
the right place.

Staging is set from this repo's own history: discovered work peaked at 2.27
issues created per issue closed, and nine different parent issues each produced
exactly two children. A constant two per issue is a model answering a question
it is expected to have an answer to. Most of what did not clear the bar was
context about something already tracked, and before fold existed the only way to
say so was to file another issue.

A finding is matched against the backlog by what it says rather than by its
title alone — rarity-weighted term overlap against every open and closed issue,
in `internal/similar` — so a near-duplicate is dropped and a probable one is
flagged for the human doing the triage.

`defer` is the older behaviour: the barrier files the issue with a `discovered`
label, a `discovered-from` dependency on the issue whose worker found it, and a
deferral that hides it from `bd ready`. That is belt and braces with the run's
scope allowlist, and the two protect against different mistakes: the allowlist
stops this run picking the work up, the deferral stops the next one. `immediate`
files without the deferral.

The worker prompt sets a bar for what is worth writing down at all — would a
human schedule this as a separate piece of work? — because "file anything you
discovered" with no threshold produced 1.54 new issues per issue worked, and
issues like a limitation the worker had already documented in a code comment.

**A Claude Code hook fails open, always.** `bd-auto hook <event>` exits 0 for
every event name, recognised or not, and writes nothing to stdout. This is the
opposite of every other subcommand, where an unknown argument is a typo worth
refusing, and the asymmetry is the whole point: a hook runs before every tool
call and at every turn end, and Claude Code reads a non-zero exit as *block*.

A hook that refuses what it does not recognise does not fail the hook, it fails
the session. A real one did: a session had `PreToolUse` and `Stop` pointed at
`"${CLAUDE_PLUGIN_ROOT}"/bin/bd-auto hook …` against a binary with no `hook`
command, so both printed usage and exited 2. Every Bash call was refused before
it ran, every attempt to end the turn was refused and the model immediately
re-invoked. Nothing inside the session could fix it, because fixing it needs the
shell the hook is blocking, and the config had already been deleted — the hooks
survived only in the running session's memory, so the fix was "restart Claude
Code", which the error does not say and no file records.

So the rules, enforced by tests in `internal/cmds/hook_config_test.go`:

- An unknown event is version skew between a shipped config and a shipped
  binary — the ordinary state of a repo where the plugin is built from the tree
  it configures — and skew is answered by doing nothing, quietly.
- `stop_hook_active` is honoured first. Claude Code sets it when the model is
  running *because* a Stop hook blocked the last turn end; a Stop hook that
  ignores it and blocks again blocks forever.
- A handler that errors, or panics, is reported on stderr and exits 0. An
  unrecovered Go panic exits 2, which is exactly the code that blocks.
- Only a deliberate `hookBlock()` exits non-zero.
- Any hooks config bd-auto ships must also exit 0 on its own when the binary is
  missing — `… || true` — because that is the skew in the other direction, and
  there is no bd-auto left to fail open on the config's behalf.
  `internal/cmds/testdata/hooks-example.json` is the worked shape.

bd-auto registers no hooks today, and `PreToolUse` in particular should stay
that way without a strong reason: it costs a process spawn on every tool call a
session makes, and it is the blast radius of exactly this bug.

**A question is a tool call, not a session exit.** A worker that needs to ask
something could always have failed the attempt and let the failure text reach a
human on the next round — but the next round is a new process that must be
re-sent the whole task, which is the exact cost this project exists to avoid.
`ask_user` blocks inside the live session instead, so an answer costs one tool
round-trip.

That only works if a single call is short. Claude Code kills an idle stdio tool
call after **thirty minutes** — a limit separate from, and far below, the
documented per-call timeout, and one that progress notifications do not extend.
A human who is away from the desk is away for longer than that. So `ask_user`
blocks for a five-minute hold and then returns a ticket, and the model collects
the answer with `ask_user_wait`, which holds again. The question lives in the
drain, not in the call, so an hour's wait is a dozen small round-trips and no
single call comes near any backend's ceiling. bd-auto also raises the per-server
timeout it configures to near the maximum the CLI will store, but that is the
belt: the ticket is the braces, and it is the half that works on a backend
nobody has met yet.

The tool itself is an MCP server over stdio — an open protocol rather than a
vendor's tool API, which is what keeps it above the runner seam. Backends start
their own MCP servers, so bd-auto cannot hand a running worker a server that
lives inside the drain: each worker gets a second bd-auto process (`bd-auto
ask`) that speaks MCP on its stdio and forwards down a unix socket to the drain.
The issue and role are fixed in the argv the drain generated, so a worker cannot
ask as somebody else, and a backend that reports no tool support is simply
offered nothing.

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
make tui-shots    # photograph the wave table in every state it has
```

`make tui-shots` drives the real view on a real terminal — a tmux pane, real
keystrokes — over synthetic events, and writes one PNG and one ANSI capture per
state to `docs/screenshots/tui`. It spawns no models and touches no repository
state. The states a run only reaches when something goes wrong are the reason it
exists: a killed worker, a parked issue, a question being typed into. The
harness itself is `internal/tui/screenshot_test.go`, which skips unless
`BD_AUTO_SHOTS` is set, so `go test ./...` never sees it.

`make smoke` covers what unit tests cannot: talking to `bd`, reading a real DAG,
driving run state across processes, and the whole command surface end to end. It
refuses to run while a drain is active, because its cleanup would delete that
run's state.

When bd-auto drains its own epic, `bd-auto` on a worker's PATH is the **main
checkout's** `bin/bd-auto` — the binary the running drain is using, not the
worker's build. A worker changing bd-auto itself must `make build` and then
invoke `./bin/bd-auto` to exercise its own change; typing `bd-auto` silently
tests the old binary.
