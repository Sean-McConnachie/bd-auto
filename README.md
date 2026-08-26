# bd-auto

A binary that drains a set of [beads](https://github.com/gastownhall/beads)
issues in dependency order. It runs one worktree-isolated model process per
issue and merges each completed issue through an integrator.

## The problem it solves

On a long multi-step task, the session context grows until compaction removes
important instructions. The issues already contain the required information,
but a long-running model session can lose it.

bd-auto keeps control flow in Go instead of a model context:

- **Each issue gets a fresh model process.** The worker reads the issue, works
  in its own worktree, commits the result, and exits.
- **Files store the run state.** Beads and `.beads/auto/run.json` let an
  interrupted run resume from disk.
- **Monitoring has a fixed context cost.** `bd-auto run status --context`
  returns a small summary at any epic size.

Independent issues run in parallel. Each worker uses a separate git worktree,
so parallel implementation does not create shared working-tree changes.

## Install

```bash
make build      # produces bin/bd-auto
```

Put `bin/` on your `PATH`.

You can also install the Claude Code plugin. It adds a `/bd-auto` skill that
starts and monitors a background drain.

```bash
claude --plugin-dir /path/to/beads-auto-imp
make install-check
```

## How a run works

```text
drain ──> workers ──> gate ──> review ──> integrator ──> handoff
           │                                  │
           └── one worktree per issue         └── one merge at a time
```

The default pipeline processes each issue as follows:

1. The implement stage creates a branch and a new worktree.
2. The worker implements the issue in that worktree.
3. The gate runs the configured build and test commands.
4. The reviewer checks the change and can return feedback to the worker.
5. The integrator merges the branch and gates the merged result.
6. The scheduler starts newly ready issues when their dependencies land.

`concurrency` limits active workers. Integration runs beside those workers, but
only one integration can change the epic branch at a time.

The run supports two autonomy modes:

- `auto` integrates each completed issue immediately and keeps worker slots
  full. This is the default.
- `wave` waits for all workers in a wave, integrates the wave, and then opens
  the next wave.

Before it creates worktrees, bd-auto checks each configured backend. The check
detects missing commands, authentication errors, and unsupported flags. Use
`--no-preflight` to skip it.

Codex billing authorization is a separate, non-skippable check. A ChatGPT login
continues on the authenticated plan. `CODEX_API_KEY` or an API-key login stops
before preflight or filesystem changes unless the current `drain` or `issue run`
command includes `--allow-api-billing`. That flag is consent for one invocation;
it is never written to configuration. Signed-out or unrecognized Codex status
stops with an instruction to run `codex login`.

The drain continues until each issue lands or parks. If you interrupt it,
repeat the same command to resume the saved run.

## Where the work ends up

By default, bd-auto creates a temporary epic branch:

```text
bd-auto/epic/<epic>-<timestamp>
```

The integrator merges every accepted issue into this branch. The original
branch remains unchanged. The checkout stays on the epic branch after the run.

bd-auto opens a pull request only when all these conditions are true:

- Every issue in scope landed.
- No issue parked.
- The run finished without interruption or an outage.
- The final gate passed on the epic branch.

If handoff fails, the epic branch and all landed work remain available. Run
`bd-auto handoff` after you resolve the problem. This command gates the current
epic branch again before it opens the pull request.

Use `bd-auto handoff --force` to override a run-state refusal. It cannot create
a missing branch or publish an empty branch.

- `--no-pr` creates the epic branch but does not open a pull request.
- `--no-epic-branch` merges into the current branch and disables pull requests.
- `git switch <branch>` returns the main checkout to your original branch.

## Requirements and quick start

A target repository needs Git, `bd`, and at least one supported model backend.
bd-auto stores worktrees and run state under `.beads/auto/`.

```bash
cd /path/to/repository
bd-auto init                     # Claude configuration
bd-auto init --provider codex    # Codex configuration
bd-auto drain --epic <id>
```

For a headless Claude run, configure worker permissions as `bypass`. Permission
prompts have no user to answer them. Keep the reviewer scoped when possible.

## Configuration

bd-auto reads `.beads-auto.yaml` from the repository root. Every field has a
default, and `bd-auto init` writes a starter file plus built-in agent files.

```yaml
gate:
  - name: build
    run: go build ./...
  - name: test
    run: go test ./...

pipeline:
  - stage: implement
    agent: worker
  - stage: gate
  - stage: review
    agent: reviewer
    max_rounds: 3

runners:
  default:
    provider: claude
    model: opus
    claude:
      permissions: bypass
  reviewer:
    model: sonnet
    claude:
      permissions: scoped
    resume: false

concurrency: 5
autonomy: auto
retry: 1
discovered_work: triage

handoff:
  branch: true
  pr: true
  remote: origin
  prefix: bd-auto/epic/

ask:
  enabled: true
  timeout: 3600
  hold: 300
  roles: [worker, integrator]

graph:
  enabled: false
```

The main configuration groups are:

- **Gate:** Each command must exit with status 0. The generated gate stays
  commented because bd-auto cannot infer a repository's build commands.
- **Pipeline:** `implement` creates the worktree. `gate` runs gate commands.
  Other stages use an `agent` role or a `run` command.
- **Runners:** Each role inherits settings from `default`. Claude and Codex use
  separate provider-specific permission and tool settings.
- **Retries:** `max_rounds` reuses the current session for feedback. `retry`
  starts a fresh worker after that session fails.
- **Handoff:** `branch` controls the epic branch. `pr` controls pull-request
  creation. A pull request requires an epic branch.
- **Discovered work:** `triage` saves findings for human review. `defer` files
  deferred issues. `immediate` files issues without deferral.
- **Questions:** Selected roles can ask a user without ending their sessions.
  Headless runs tell the worker to make and record an assumption.
- **Code index:** When enabled, bd-auto uses `graphify` to build an optional
  search index. A missing or failed index does not stop the run.

### Stages, roles, and agents

`stage` names a pipeline step. `agent` names the runner role for that step.
`run` replaces the model with a shell command.

The built-in roles are `worker`, `reviewer`, and `integrator`. Add custom roles
under `runners` or with an agent file:

```text
.beads-auto/agents/<role>.md
```

An agent file contains runner frontmatter and a system prompt. The prompt can
include `{{BUILTIN}}`, `{{GRAPH}}`, and `{{VERDICT}}` splice tokens.

Use these commands to inspect and update agent prompts:

```bash
bd-auto agents
bd-auto agents show <role>
bd-auto agents diff [<role>]
bd-auto agents update <role>
```

### Hooks

Hooks read completed reports at `on_issue_end`, `on_barrier`, or `on_run_end`.
A hook can run a command or an agent role.

- Hooks are advisory. They cannot change a verdict, stop a run, or block a
  pull request.
- Command hooks receive the report path in `$BD_REPORT_FILE`.
- Hooks run in the main checkout and must not run git commands there.
- Every hook has a timeout. The default is 300 seconds.
- `on_issue_end` hooks can run concurrently for different issues.

See [plans/hooks.md](plans/hooks.md) for the full hook design.

### Permissions

Claude workers need `permissions: bypass` for unattended work. The worktree,
branch, scope, and generated git guards limit each worker.

Keep reviewers at `scoped` when possible. Their default denied tools prevent
beads writes even when another permission setting allows more access.

`--dangerously-skip-permissions` forces bypass for every role in one run. Use
the configuration file for normal repository settings.

## Commands

Setup and inspection:

```bash
bd-auto init [--provider claude|codex] [--force] [--dir <path>]
bd-auto config show
bd-auto agents [list|show|diff|update]
```

Start work:

```bash
bd-auto drain --epic <id>
bd-auto drain --issues a,b,c
bd-auto issue run --issue <id>
bd-auto run start --epic <id>
```

Common drain options include:

```text
--concurrency N       --autonomy auto|wave
--rounds N            --retry N
--base <ref>          --no-preflight
--allow-api-billing
--no-pr               --no-epic-branch
--plain               --json
--dry-run             --quiet
```

Inspect and control a run:

```bash
bd-auto run status [--context] [--wait <duration>]
bd-auto run pause
bd-auto run resume
bd-auto run stop [--keep-state]
bd-auto run unpark --issue <id>
```

Integration and recovery:

```bash
bd-auto gate
bd-auto integrate [--all]
bd-auto handoff [--force]
bd-auto triage
```

Use `bd-auto <command> --help` for all flags and lower-level commands.

## Watch and control a run

In a terminal, `bd-auto drain` shows one row per issue. Each row shows the
attempt, active stage, elapsed time, cost, and latest activity.

```text
ISSUE      ATT STATE          TIME   COST     ACTIVITY
t-1        1   done           2m43s  $0.8135  finished
t-2        1   reviewer (0)     25s  $0.4210  Read
t-3        2   worker (1)       23s  -        Bash
t-4        1   gate (1)       1m04s  $0.6602  go test ./...
```

- `up` and `down` select an issue.
- `enter` opens its transcript.
- `k` kills and parks the selected worker.
- `q` or `ctrl-c` stops the run without judging active work.

A round is another turn in the same session and worktree. An attempt starts a
new session and worktree. Costs appear after a model process exits.

The transcript includes model messages, tool calls, earlier rounds, and earlier
attempts. bd-auto stores transcripts under `.beads/auto/logs/`. Display limits
do not delete the full log files.

The integrator has a separate display area. It shows merges, conflict
resolution, and the gate on the merged branch.

For a background run, use:

```bash
bd-auto run status --context --wait 30m
```

`--context` prints a fixed-size summary. `--wait` returns when the run stops or
the duration expires.

`run pause` lets active work reach the next scheduler boundary. `run resume`
continues dispatch. `run stop` ends the run and removes its state unless you
set `--keep-state`.

## Failure and recovery

When a stage fails, bd-auto sends its output to the next feedback round. After
the round limit, bd-auto retries the issue with a fresh worker. The default
allows one retry.

If the fresh attempt fails, bd-auto blocks the issue, adds the `human` label,
and continues with independent work. Evidence from each attempt stays on the
issue.

A worker can park itself when it cannot proceed. Parking does not spend the
remaining retry budget. The issue branch remains available but is not merged.

After you fix the cause, run:

```bash
bd-auto run unpark --issue <id> --reason "what you fixed"
```

This command reopens the issue, clears the `human` label, and resets its
attempt count. If the run has ended, reopen the issue with `bd` and start a new
run.

An interrupted run preserves its branches, worktrees, sessions, and state.
Repeat the drain command to resume it.

## Safety and design

bd-auto enforces these rules in code:

- **One writer per issue:** The engine does not write an issue while its worker
  runs. File locks protect shared run state.
- **Isolated workers:** Each attempt uses its own worktree and branch.
- **No worker integration:** Generated git guards reject push, merge, and
  rebase commands from worker worktrees.
- **Verified completion:** The engine checks beads state, commit history, and
  gate results. It does not trust the worker's report alone.
- **Serialized integration:** One integrator changes the epic branch at a time.
  A failing merged branch parks without discarding independent branches.
- **Controlled discovery:** Workers report findings to bd-auto. The default
  triage mode does not create issues without human review.
- **Advisory hooks:** Hooks can interpret reports but cannot change engine
  decisions.
- **Protected repository hooks:** Internal git commands suppress repository
  hooks only for that command. Human and worker git commands remain unchanged.

These controls keep model processes away from shared integration state. They
also preserve branches and evidence when work fails.

## Development

```bash
make check              # build, vet, and test
make smoke              # run an end-to-end disposable epic
make integrator-stress  # test difficult integration states
make launch-cost        # check monitoring context cost
make tui-shots          # capture terminal interface states
```

The smoke and stress commands create test state. Do not run scripts that change
`.beads/auto/` while a live drain uses the same repository.
