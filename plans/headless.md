# bd-auto: headless engine with a swappable agent backend

## Context

bd-auto today is a Claude Code plugin where **Go owns state and the model owns control flow**. `SKILL.md` is the program, the Stop hook is the instruction pointer, and a live orchestrator context coordinates every worker, reviewer and integrator. `internal/pipeline/pipeline.go:4-7` states the decision explicitly:

> *"agent: stages are not executed here. The binary only resolves and reports them, because dispatching a subagent is the orchestrator's job through the Agent tool."*

That is the source of the context blow-up: coordination is O(issues), it all lands in one append-only transcript, and that transcript is re-sent on every turn. A wave-runner subagent would only move the ceiling.

This inverts it. **Go owns control flow and spawns models as subprocesses.** No long-lived context exists anywhere, so nothing accumulates. Model invocation goes behind a `Runner` interface so the backend can be swapped later without touching the engine.

Scope confirmed with the user:

| Decision | Choice |
|---|---|
| Abstraction | Full `Runner` seam now, **only** the `claude -p` adapter ships (plus a `fake` for tests) |
| Permissions | `--permission-mode auto` — the backend's own classifier |
| Plugin surface | Delete `hooks/` and `agents/`; **keep** `.claude-plugin/plugin.json`; `SKILL.md` becomes a thin launcher |
| Worker git guard | Structural (worktree git config) **and** post-hoc verification in Go |
| Recovery from a **recoverable** failure | Resume the same session wherever the backend supports it; fresh only as a last resort |
| Runner config | Per-role (worker / reviewer / integrator / custom stages) over a default |
| Interrupt recovery | Resume the interrupted session if the worktree survived, with automatic fresh fallback when the resumed turn errors |
| Integrator | Go-driven; a model is spawned **only** when a merge actually conflicts |
| Output | Live bubbletea TUI, over an event stream that also renders plain / JSON |
| What bounds a run | **A human-selected set of issues, fixed before any model is spawned.** No budgets, no per-request timeouts, no circuit breakers |

The last row is the load-bearing one. There is no automatic limit on spend anywhere in this design, so the bound has to be applied by a human *up front*, at the only moment they can see what they are agreeing to: the run's scope is chosen before the first subprocess starts, and the engine treats that list as a hard allowlist for the run's whole life. The TUI's stop controls remain — they are how you abandon a run in flight — but they are the second line, not the first.

---

## Scope: the run's only bound

`bd-auto drain` does not "drain an epic". It drains **a set of issues a human named**.

- **On a TTY**, `bd-auto drain --epic X` computes the candidate set (the epic's open, unparked children), shows it alongside the wave decomposition, the gate commands and the model chosen per role, and requires an explicit multi-select and confirmation. Nothing is spawned before the confirm. This is a preview of the shape of the spend, not a yes/no prompt.
- **Off a TTY** — the skill launcher, CI, `--plain` — the scope must be explicit: `--issues a,b,c`, or `--epic X --all` to mean "everything the epic has right now". A bare `drain --epic X` with no TTY and no explicit selection prints the candidate set and **exits non-zero without dispatching**. This is deliberate: the background path is the one with nobody watching, so it is the one that must name its work.
- The selected IDs are written to run state as `Scope []string` before anything runs. `wave.Plan` intersects `bd ready` with `Scope`; an issue outside it is never dispatched, whatever bd says about its readiness.
- **Discovered work is out of scope by construction.** Workers file it deferred; a deferred issue was not in the list the human approved and cannot join the run.
- An in-scope issue whose dependency is *out of* scope and unmet is **parked immediately** with `dependency <id> is out of scope`, rather than sitting unready until the run ends unable to explain itself.
- The run ends when every scoped issue is done or parked. The epic may well still be open; `epicComplete` refuses to close it unless every child completed.

`autonomy` shrinks to `auto | wave`: `wave` still pauses at each barrier and waits for `bd-auto run resume`, which is useful when the scope is large. `issue` is deleted — pausing after each issue is redundant when the human already chose the issues.

---

## Architecture

```
bd-auto drain --epic X            (scope selected first, then:)
  │
  ├─ internal/wave      plan the wave within scope, merge order   (extracted from cmds)
  ├─ internal/drain     wave loop, per-issue pipeline, integrator, event bus
  │     └─ internal/runner    Runner interface
  │           ├─ runner/claude    claude -p adapter
  │           └─ runner/fake      scripted adapter for tests
  ├─ internal/worktree  create / reuse / prune worker worktrees
  ├─ internal/gitguard  structural + post-hoc branch guards
  ├─ prompts/           role prompts (rewritten from agents/*.md)
  └─ internal/tui       bubbletea renderer + control channel
```

### The `Runner` seam — `internal/runner/runner.go`

```go
type Runner interface {
	Name() string
	Caps() Capabilities
	Run(ctx context.Context, req Request, sink EventSink) (Result, error)
}

type Request struct {
	Role         Role          // worker | reviewer | integrator | custom stage
	SystemPrompt string        // the role prompt
	Prompt       string        // the task
	Dir          string        // worktree
	SessionID    string        // caller-generated UUID
	Resume       bool          // continue SessionID rather than start it
	Model        string
	Permissions  Permissions   // scoped | auto | bypass — adapters translate
	AllowedTools []string
	ExtraArgs    []string      // per-backend escape hatch
	Timeout      time.Duration // 0 = unlimited, and 0 is the default
}

type Result struct {
	Class     Class  // see below — the engine branches on this before anything else
	Text      string // final assistant message
	SessionID string
	ExitCode  int
	Err       error  // backend-reported failure, for the log and the TUI
	Usage     Usage  // zero when the backend does not report it
	Duration  time.Duration
	TimedOut  bool
	LogPath   string
}

type Capabilities struct {
	Resume, Stream, ReportsUsage bool
	Permissions                  []Permissions
}
```

**`Class` is the field the engine reads first**, and it exists because "the process exited non-zero" is not one thing:

```go
const (
	ClassOK          Class = "ok"           // ran, produced work
	ClassWorkFailed  Class = "work-failed"  // ran, the work is wrong
	ClassInfraFailed Class = "infra-failed" // usage limit, 429/529, expired auth, network, CLI crash
	ClassInterrupted Class = "interrupted"  // ctx cancelled: a TUI stop, or a kill
)
```

Without this, five parallel workers meeting one rate limit each burn three rounds and a fresh attempt against a 429 and then park five perfectly good issues with a nonsense reason, and the epic never closes. At `concurrency: 5` against one account that is a likely Tuesday, not an edge case. **`ClassInfraFailed` consumes neither a round nor an attempt**: the engine backs off and re-runs the same round, and if infra failures keep coming it pauses the run with state intact rather than converting an outage into a pile of parked work. This is error handling, not a spend limit — the scope selection is the spend limit.

Three things keep the seam honest rather than Claude-shaped:

- **`Permissions` is a coarse enum, not a mode string.** `auto` maps to `--permission-mode auto` for Claude and to whatever the next backend calls it. Raw flags go in `ExtraArgs`.
- **No agent concept.** Role prompts go through `--append-system-prompt` and tool limits through `--allowed-tools`, both of which have rough analogues elsewhere. `--agent` does not, and it dies with the plugin anyway.
- **`Caps().Resume` is branched on by the engine**, so a backend without resume degrades to a fresh worker plus a notes file instead of failing.

`EventSink` receives parsed `--output-format=stream-json` events, which is what gives the TUI live per-worker activity instead of a three-minute spinner.

### `internal/runner/claude`

```
claude -p <task>
  --append-system-prompt <role prompt>
  --model <model>
  --permission-mode auto
  --session-id <uuid>          # or --resume <uuid> on a later round
  --output-format stream-json --verbose --include-partial-messages
  --allowed-tools ...          # reviewer only
```

All verified present in CLI 2.1.233. `--include-partial-messages` is what makes the TUI's activity line text-granular rather than tool-call-granular; without it the flag list still works, the display is just coarser. Generating the UUID in Go means a later round needs no output parsing to find the session. Full transcripts land in `.beads/auto/logs/<issue>-<round>.jsonl`; the engine keeps only the final message — better observability than today, where worker transcripts are invisible.

Two implementation details that are easy to get wrong and expensive to debug:

- **Cancellation must kill the process group.** `exec.CommandContext` kills the direct child only, so a worker forty seconds into `go test ./...` leaves the test running, holding its worktree. The adapter sets `SysProcAttr{Setpgid: true}` and signals the group (SIGTERM, then SIGKILL after a grace period). The TUI's `k` key, `q` on a five-wide wave, and every timeout depend on this.
- **Usage is recorded as cost, not as summed tokens.** Across resume rounds the same prefix is billed repeatedly as cache reads at a fraction of the input price, so summing `input_tokens` overstates a resumed attempt and understates a fresh one. Record `total_cost_usd` plus input / output / cache-read / cache-creation separately.

### `internal/worktree`

Today the worktree comes from `isolation: worktree` in the agent frontmatter — **Claude Code creates it**. Verified: `git worktree add` appears nowhere in the Go code; there is only `listWorktrees` (`merge.go:201`) and `git worktree remove` (`worker.go:176`). Deleting the plugin deletes worktree creation, so Go has to own the whole lifecycle:

- create at `.beads/auto/wt/<issue>`, branching from the base SHA recorded at dispatch (`.beads/auto/` is already gitignored)
- reuse the existing worktree on a later round and on a resumed run. **The path must be stable**, because `claude --resume` resolves a session against the project derived from cwd; a moved worktree is a lost session
- `git worktree prune` at drain start, and an adopt-or-recreate path for "worktree exists, branch does not"
- removal only in `discardAttempt` (between attempts, `worker.go:174`) and after a successful merge

Same call site as `gitguard.Setup`, so the two ship together.

### `internal/gitguard`

Replaces the `PreToolUse` deny at `hook.go:280`, which is Claude-specific and whose `agent_id == ""` guard (`hook.go:348`) silently stops firing headless.

**Structural**, at worktree creation:

```
prev=$(git config --get core.hooksPath)          # in this repo: .beads/hooks
git config extensions.worktreeConfig true
git config --worktree core.hooksPath <generated rejector dir>
git config --worktree remote.origin.pushurl no-push://bd-auto
```

Two traps, both confirmed against this repo:

- ⚠ `--worktree` and `extensions.worktreeConfig` are both required. A plain `git config` inside a worktree writes to the **shared** config and would break the main checkout.
- ⚠ **`core.hooksPath` is already set here**, to `.beads/hooks`, holding beads' `pre-commit`, `pre-push`, `post-checkout`, `post-merge` and `prepare-commit-msg`. Pointing it at a rejector dir disables every one of them inside worker worktrees, including the pre-commit that keeps `issues.jsonl` in sync. So the rejector dir is **generated per run and chains**: each hook rejects if it is one bd-auto blocks, and otherwise `exec`s the same-named hook under `prev` with `"$@"` (falling back to `.git/hooks` when `prev` is empty). Its own `prepare-commit-msg` appends a `Bd-Auto: <issue>/<attempt>` trailer before chaining — see the provenance check below.

What the structural half cannot catch: **`pre-merge-commit` does not fire on a fast-forward merge** (no commit is created), and **`git rebase` has no hook at all**. `pre-push` does fire for every push regardless of remote, so once chaining is fixed the push half is genuinely closed.

**Post-hoc**, after the worker exits, against the base SHA recorded at dispatch:

- `git merge-base --is-ancestor <base> <branch>`
- `git rev-list --min-parents=2 --count <base>..<branch>` is 0 (no merge commits)
- **every commit in `<base>..<branch>` carries this run's `Bd-Auto:` trailer**
- base branch SHA unchanged
- `refs/remotes/origin/*` unchanged

The trailer check is the one that closes the holes above, and it is why the shape checks alone were not enough. After `git rebase origin/main` the base is *still* an ancestor, there are *still* no merge commits, and the local base ref and remote refs are *still* unchanged — but `base..branch` now contains origin's commits, and those have no trailer. Same for a fast-forward `git merge origin/main`. The shape checks stay because they produce a better message when they fire.

Any failure routes into the existing `workerFail` path.

### `prompts/` — role prompts

`agents/*.md` are subagent definitions, not role prompts, and converting them is a rewrite rather than a frontmatter strip. `bd-worker.md` alone carries: the `BD-AUTO:` footer protocol (`hook.go:215`, deleted), "your report is the only thing that reaches the orchestrator" and its 25-line cap (there is no orchestrator context left to protect), "a hook blocks those anyway" (there is no hook), and a whole **Shell** section about Claude Code refusing shell commands it cannot statically verify under worktree isolation — which does not apply to a plain `claude -p` in a plain worktree.

Two things every role prompt gains:

- **An explicit override of the repo's own `CLAUDE.md`.** This repo's says *"Work is NOT complete until `git push` succeeds… NEVER stop before pushing."* A headless `claude -p` loads `CLAUDE.md` exactly like an interactive session does. Today the `PreToolUse` deny answers that with an actionable message; afterwards the worker gets a bare git hook rejection and, having been told push is mandatory, retries — or reaches for `git pull --rebase`, which is the rebase hole above. The worker prompt has to say in as many words: you are one attempt inside a bd-auto run, you commit to your own branch and stop, the integrator merges, nothing here pushes.
- **Rejector messages that say what to do instead.** The generated hooks print the same instruction, so a model that hits the guard learns the alternative rather than only the prohibition.

Because issues 6 onward cannot spawn anything without these, the prompt rewrite is its own early issue, not part of the final cleanup.

### Per-issue pipeline — `internal/drain/issue.go`

**Resume is the recovery path for every recoverable failure, not just review.** This was written as a hypothesis and has since been measured (issue 7, `scripts/resume-vs-fresh.sh`); the numbers are below, and they are why it is no longer hedged.

Both arms drained the same three-issue fixture epic from the same commit, differing in nothing but the two knobs. The fixture contains a stage no worker can pass on its first round — it mints a random token and fails, printing the line the worker must add — so every issue needs exactly one recovery in both arms, and both arms spend exactly six model processes. The only difference is whether process two is a resumed session or a new one.

| arm | config | issues done | model processes | attempts | `total_cost_usd` | wall clock |
|---|---|---|---|---|---|---|
| fresh | `max_rounds 1, retry 3` | 3/3 | 6 | 6 | **$1.7210** | 264s |
| resume | `max_rounds 4, retry 1` | 3/3 | 6 | 3 | **$1.4055** | 195s |

**Resume is 18% cheaper and 26% faster**, and it won on all three issues individually rather than on one outlier. A single-issue rehearsal on a cheaper model put the same gap at 36%.

The counterargument this plan made was that a resumed session re-sends its whole transcript every turn, so its per-turn input is strictly larger. That is true per turn and wrong in aggregate, and the token columns say why: the resume arm read **fewer** cached tokens (2.0M against 3.0M) and produced **half** the output (8.3k against 16.0k). A fresh attempt does not merely re-derive its plan — it re-runs the exploration, and every tool result that exploration produces is then itself re-sent on every remaining turn of the new session. Re-derivation is not a fixed cost paid once; it inflates the whole rest of the attempt.

Three honest limits on the number. The fixture's re-derivation is six small files, so a repo with heavier exploration should widen the gap, not narrow it, and a repo with almost none should narrow it. Both arms ran `sonnet`, not the default `opus`; what transfers is the ratio, not the dollars. And the recovery here is one round — the five-minute cache TTL and the autocompact cliff both remain untested past round two, which is a reason to keep `max_rounds` small rather than to raise it.

So there is **one loop with one feedback channel**, and every check that can fail feeds it:

```go
base := HEAD
wt   := worktree.Ensure(iss, branch, base)   // create or reuse; Go owns this now
gitguard.Setup(wt)                           // chained rejector hooks + pushurl
sid  := uuid.New()
var feedback string

for round := 0; round < maxRounds; round++ {
    mark := worktree.Snapshot(wt)                       // branch tip + dirty file set
    res  := runner.Run(ctx, workerRequest(iss, sid, round, feedback), sink)

    switch res.Class {
    case ClassInfraFailed:
        backoff(); round--; continue                    // costs neither a round nor an attempt
    case ClassInterrupted:
        return interrupted                              // resumable; attempt counter untouched
    }

    if !worktree.Changed(wt, mark) { return failAttempt(noProgress) }  // hard fail, not another round

    if !bd.Show(iss).Terminal()       { feedback = notClosedMsg;   continue }
    if v := gitguard.Verify(base); !v.OK   { feedback = v.Reason;  continue }
    if g := pipeline.Gate();       !g.Passed { feedback = Tail(g); continue }
    if r := review(iss);            r.Failed { feedback = r.NotesRef; continue }

    return done
}
wave.RecordFail(iss, lastStage, feedback)   // rounds exhausted
```

**The progress check has to come first, and it has to be a hard failure.** Every check below it is satisfiable by stale state: after round 1 the issue is already closed, so `bd.Show(iss).Terminal()` passes on round 2 even if round 2 did nothing at all, and `gitguard.Verify` passes on an unchanged branch too. A no-op round would sail through to the reviewer, which re-reads an identical diff, fails identically, and spends every remaining round in an empty loop at full price. A round that ends with no new commit and no changed file means the resume is not working for this issue, and the right move is to stop resuming, not to resume again.

Escalation, cheapest first:

1. **Resume rounds** (`max_rounds`, default 3) — same session, same worktree, same branch. Handles gate failures, review failures, guard violations and "you never closed the issue".
2. **Fresh attempt** (`retry`, default 1) — once rounds are exhausted or a round made no progress. `discardAttempt` (`worker.go:174`) wipes worktree and branch, and a new worker starts from the failure notes on the issue.
3. **Park.**

Consequences worth stating:

- `max_rounds` becomes the primary tuning knob and `retry` the safety net — the reverse of today. The measurement settles the defaults at `max_rounds: 3, retry: 1`: rounds are the cheaper recovery, and 3 is the number the dogfood run needed to clear a hard review (see README), while a fresh attempt stays available for the one thing rounds cannot fix, a session that has gone wrong in itself.
- `discardAttempt` must **not** fire between rounds, only between attempts. Wiping the worktree is what makes a resume pointless.
- **The reviewer defaults to a fresh session.** Reviewer resume is available (`resume: true` under `runners.reviewer`) but off, because a resumed reviewer carries its own previous `VERDICT: fail` and is anchored to checking whether its findings were addressed rather than re-judging the diff. It is also the cheap half: the diff dominates its input and has to be re-read after changes anyway.
- Where `Caps().Resume` is false, every `continue` degrades to a fresh process with the feedback in its prompt and the worktree left in place. Correct everywhere, just more expensive.

The `footerRe` protocol (`hook.go:215`) and the 25-line report cap both disappear — there is no orchestrator context left to protect, and `bd show` is ground truth.

### Session persistence — `internal/runstate`

`Attempt.AgentID` (`state.go:38`) becomes `WorkerSession` and `ReviewSession`, both written before the runner is invoked rather than by a hook afterwards. `Bindings` is deleted with the hook layer. `Scope` is added, and is written once at run start.

This is also what makes interrupt recovery cheap: on restart, if the worktree is intact and the backend supports resume, the interrupted session is resumed with "you were interrupted, continue"; the attempt counter is untouched either way.

The known hazard is specific. A `claude -p` killed mid-turn can leave a transcript ending in a `tool_use` block with no matching `tool_result`, and resuming that sends an invalid message sequence and errors immediately. With the `Class` taxonomy that is a `ClassInfraFailed` on the first resumed turn, which **falls back to a fresh dispatch automatically**. That is what makes the least-tested path in the system self-healing rather than a coin flip.

### Go-driven integrator — `internal/drain/integrate.go`

```go
for _, b := range mergeOrder {           // existing topoOrder, merge.go:105
    if err := gitMerge(b); err != nil {
        res := runner.Run(ctx, conflictRequest(b), sink)   // model ONLY here
        if !resolved { park(b); continue }
    }
}
pipeline.Gate(...)      // Go
cleanupWorktrees()      // Go
if epicComplete(st) { bd.Close(epic) }   // Go
```

`epicComplete` is the four-condition predicate currently written as prose at `bd-integrator.md:56-64`. It is a pure function of run state, so it becomes a tested Go function rather than something a model is asked to get right. Note it now has to reason about scope: a run whose scope was a subset of the epic will finish with children still open, and must leave the epic alone.

### Config — `internal/config`

```yaml
runners:
  default:
    provider: claude
    model: opus
    permissions: auto
    timeout: 0            # 0 = unlimited. The scope selection is the bound, not the clock.
  reviewer:
    model: sonnet
    permissions: scoped
    allowed_tools: [Read, Grep, Glob, "Bash(git diff:*)", "Bash(git log:*)", "Bash(bd show:*)"]
    resume: false         # reviewers judge fresh; see the pipeline section
  integrator:
    model: opus

max_rounds: 3     # resume rounds per attempt — the primary recovery knob, and
                  # the cheaper one: measured 18% under a fresh attempt
retry: 1          # fresh attempts after rounds are exhausted — the safety net
autonomy: auto    # auto | wave. `issue` is gone: the human already picked the issues.
```

Note the reviewer's tool list: today it is a read-only subagent protected by the `PreToolUse` deny that is being deleted, and a bare `Bash` entry after that deletion is a reviewer that can push. `--allowedTools` takes command patterns, so scope it to the three things a reviewer actually runs.

`max_rounds` moves out of the individual `review` stage and becomes a run-level setting, because it now governs the whole recovery loop rather than one stage. The per-stage field stays supported and **wins where set**; that precedence is written down and tested rather than left to discovery.

`Stage.Agent` keeps its name but changes meaning: it now names a **key under `runners:`**, executed by the binary, not a subagent to dispatch. That is a silent breaking change for any existing config — this repo's own `agent: bd-reviewer` only keeps working because the role name and the old agent filename happen to match — so config load validates that every `agent:` names a defined runner role and fails with the list of valid ones. `pipeline.go:4-7` gets rewritten; that comment is the symbolic centre of this change.

---

## Work breakdown

Filed as a beads epic; each issue builds and passes `go build ./... && go vet ./... && go test ./...`.

| # | Issue | Depends on |
|---|---|---|
| 1 | **`internal/runner` + config.** Interface, `Class`, `Permissions`, `Capabilities`, registry, `runners:` block with per-role resolution over `default`, `agent:`-names-a-role validation, `max_rounds` precedence. Table tests on role resolution. | — |
| 2 | **`runner/claude` + `runner/fake`.** Argv construction, stream-json parsing, usage/cost extraction, `Setpgid` + process-group kill, and the exit-code/stderr → `Class` mapping. `fake` replays scripted results *including classes* so the whole engine is testable with zero model calls. Table tests on argv. | 1 |
| 3 | **`prompts/`.** Rewrite the three role prompts for a headless process: drop the footer protocol, the report cap and the Claude-Code shell section; add the `CLAUDE.md` push override. | — |
| 4 | **`internal/wave`.** Extract plan and merge-order *logic* out of `cmds/plan.go` and `cmds/merge.go` into callable functions returning structs; CLI commands become thin wrappers. Pure refactor — existing tests must pass untouched. | — |
| 5 | **`internal/worktree` + `internal/gitguard`.** Create / reuse / prune worktrees at `.beads/auto/wt/<issue>`; chained rejector hooks with trailer injection; the five post-hoc predicates. Unit-tested against temp repos. | — |
| 6 | **Per-issue pipeline + `bd-auto issue run --issue X`.** The unified loop, the progress snapshot, `Class` handling, one issue end to end, standalone and debuggable before any wave logic exists. Sessions persisted to run state; review notes to `.beads/auto/review/<id>.md`. | 1, 2, 3, 5 |
| 7 | **Resume-vs-fresh measurement.** Run issue 6 both ways over a live epic, compare `total_cost_usd`, and set `max_rounds` / `retry` defaults from the result. Record the numbers in the issue. **Done:** resume 18% cheaper; defaults confirmed at 3 / 1. | 6 |
| 8 | **Go-driven integrator** and the `epicComplete` predicate (scope-aware), with the conflict-only model path. | 4, 6 |
| 9 | **Scope selection + `bd-auto drain`.** Candidate computation, TTY multi-select with the preview, the non-TTY explicit-scope requirement, `Scope` in run state and enforced in `wave.Plan`, out-of-scope-dependency parking; then the wave loop, `errgroup` bounded by `concurrency`, event bus, plain + `--json` renderers, and interrupt recovery. | 6, 8 |
| 10 | **TUI.** bubbletea + lipgloss wave table with live per-worker activity, accumulated cost per issue and for the run, and the control channel: `k` kills the selected worker, `q`/`ctrl-c` stops the run. Non-TTY falls back to the plain renderer from issue 9. | 9 |
| 11 | **Cut the plugin.** Delete `hooks/`, `agents/`, `internal/cmds/hook.go` and its tests, the `Continuations`/`maxContinuations` autonomy engine (`hook.go:25`), `Bindings` from run state, and `autonomy: issue`. **Keep `.claude-plugin/plugin.json`** — it costs nothing and keeps the skill installable. Rewrite `SKILL.md` as a launcher that must name its scope and then polls `bd-auto run status`. Update `README.md` and `.beads-auto.yaml`. | 9, 10 |

Issue 11 is deliberately last: everything it deletes stays working until the replacement is proven. Issue 3 moves *early*, unlike the rest of the cleanup, because issues 6 onward cannot spawn anything without role prompts.

---

## Verification

**Automated**

- `go test ./...` — unit tests per package; `internal/runner/fake` makes the drain loop deterministic.
- **Scope tests:** an issue outside `Scope` is never dispatched even when `bd ready` offers it; an in-scope issue with an unmet out-of-scope dependency parks immediately with that reason; a non-TTY `drain` with no explicit scope exits non-zero and spawns nothing.
- New `internal/drain` test: a 4-issue synthetic DAG in a temp git repo with `provider: fake`, exercising clean pass, a parked issue, and — the point of the resume work — that a gate failure, a review failure and a guard violation each produce a **`Resume: true` request on the same session ID**, that the worktree survives between rounds, and that a fresh attempt is taken only once `max_rounds` is exhausted. `fake` records every `Request` it receives, so these are exact assertions.
- **No-progress test:** a round that returns `ClassOK` with no commit and no file change fails the attempt outright and does *not* consume the remaining rounds.
- **Class test:** `fake` returns `ClassInfraFailed` then `ClassOK`; assert the same round index is re-run, and that neither the round counter nor the attempt counter advanced.
- A `Caps().Resume == false` variant of the resume test, asserting the fresh-process fallback still reaches the same outcomes.
- **Interrupt test:** a resumed first turn that returns `ClassInfraFailed` falls back to a fresh dispatch without consuming an attempt.
- `worktree`/`gitguard` tests against temp repos with a fake origin: a branch that fast-forward-merges origin, one that rebases onto origin, one that pushes, one that is clean — the trailer predicate must catch the first two, which the shape predicates alone do not. Plus a test that a **chained** hook still runs, so beads' own hooks are not disabled inside a worktree.
- **Process-group test:** a `fake` worker that spawns a long-running grandchild; cancel the context and assert the grandchild is gone.
- `epicComplete`: table test over run states, including "one child parked" (must **not** close) and "scope was a subset, other children still open" (must **not** close).

**Manual, end to end**

1. `scripts/smoke.sh` extended: temp repo, three trivial issues, `provider: fake`, explicit `--issues`, assert the epic closes and every branch merges.
2. Real drain against a small live epic in this repo with `--concurrency 2`. Confirm: scope preview matches what runs, worktrees created with chained guards, transcripts in `.beads/auto/logs/`, gate runs per branch and once on the merged result, epic closes only when clean.
3. Guard proof: point a role at a prompt that instructs `git push`, then one that instructs `git pull --rebase`, and confirm the structural hook rejects the first and the trailer predicate fails the second. Confirm `bd`'s own pre-commit still fires inside the worktree.
4. TUI proof: start a drain, press `k` on a running worker and confirm the process *and its children* die and the issue is recorded failed; press `q` and confirm the run stops with state intact, then re-run `drain` and confirm the interrupted issue **resumes its session** rather than restarting.
5. Scope proof: launch off a TTY with no `--issues` and confirm nothing is spawned; launch with two of an epic's five issues and confirm the other three are never touched and the epic stays open.
6. Cost proof (issue 7): **done.** `scripts/resume-vs-fresh.sh` drains the same fixture epic twice — once `max_rounds: 1, retry: 3`, once `max_rounds: 4, retry: 1` — and compares accumulated `total_cost_usd`. Resume came out 18% cheaper and 26% faster on equal work; see the per-issue pipeline section. `make resume-vs-fresh` re-runs it, and it should be re-run whenever the worker prompt or the default model changes, because both move the re-derivation cost the result turns on.
7. Context proof, and the epic's real acceptance criterion: run the drain from a Claude Code session via the rewritten skill and confirm **the launching session grows by under 2k tokens regardless of epic size**. Everything else here is in service of that number.
