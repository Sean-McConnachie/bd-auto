# bd-auto: a graphify-backed code index the agents can query

## Context

Every worker, reviewer and integrator bd-auto spawns starts knowing nothing about
the repo it has been pointed at. Each one re-derives the same map from scratch —
grep, glob, read, read again — and pays for it in the only currency this engine
spends: transcript size, multiplied by turns.

The proposal in `beads-auto-imp-tv9` was to build a persistent
[graphify](https://pypi.org/project/graphifyy/) knowledge graph over the codebase
at the start of every wave and let the agents query that instead.

This document answers the six questions that issue left open. Every number and
every command in it was measured against graphify 0.9.30 on this repo at
`a585bf5`, not estimated.

**Nobody was watching the run that produced this plan.** The `ask_user` call put
the lifecycle question to the human and was told there was no one to answer it,
so every decision below is the author's, made on the evidence recorded here and
marked as such. The one decision deliberately *not* taken is the default: the
index ships disabled until stage 4 measures it. See
[What was assumed](#what-was-assumed).

---

## The measurements, first

The premise deserved checking before anything was designed around it. It half
survived.

### Building the index is free

`graphify extract --code-only` is pure AST extraction. No LLM, no API key, no
model in the dispatch path. Clustering is Leiden and is free too — only *naming*
the communities calls a model, and that is a separate `graphify label` step this
design never runs.

| corpus | code files | nodes | edges | communities | wall clock | tokens |
|---|---|---|---|---|---|---|
| everything, unclustered | 120 | 1654 | 5572 | — | 1.85s | 0 |
| tests excluded, unclustered | 58 | 915 | 2558 | — | 0.56s | 0 |
| **tests excluded, clustered** (recommended) | 64 | 925 | 2153 | 35 | **0.92s** | **0** |

Sub-second and zero tokens. That single fact removes most of the design space:
there is no build budget to ration, no threshold issue count below which the
build is not worth it, and no reason to tolerate a stale index when refreshing it
costs under a second.

The 50 PNG screenshots and 20 markdown files under `docs/` are the only part of
this repo that would cost tokens to index, because images and prose need semantic
extraction. `--code-only` skips them, and screenshots of a TUI teach an index
nothing about the code anyway.

*(The three rows were taken with slightly different ignore files — the last two
differ by six files because only the middle one also excluded `.beads/`. The
comparison that matters, tests in or out, is unaffected.)*

### Excluding tests is what makes the index worth reading

`god-nodes` ranks the most connected symbols. Run on the full corpus, it reports
this repo's architecture as:

```
testRepo() - 74 edges · newIssues() - 71 · testCfg() - 68 · engine() - 60
```

Run with `*_test.go` excluded, on the same repo, same command:

```
Model - 44 · Broker - 35 · State - 30 · Engine - 29 · Config - 25 · DrainReport - 24
```

The second is correct and the first is noise. Tests are 45 of this repo's 99 Go
files and 41% of its source words, and a table-driven Go test cites every symbol
it exercises, so tests dominate degree wherever they are indexed. The difference
between those two outputs is the difference between an index an agent should read
and one it should ignore.

This is a design requirement, not a preference, so the exclude list is
engine-generated rather than left to whoever sets the repo up.

### The index replaces searching, not reading

This is where the issue's premise does not survive, and it is the finding that
shapes everything downstream.

`graphify query "how does the drain decide to park an issue"` answers in 0.25s
with a **truncated list of symbol locations** — 163 nodes matched, 31 shown under
a 700-token budget:

```
NODE .Park()   [src=internal/bd/bd.go        loc=L289 community=14]
NODE Drain()   [src=internal/cmds/drain.go   loc=L42  community=9]
NODE task      [src=internal/drain/issue.go  loc=L23  community=7]
```

Raising the budget to 4000 also surfaces `.runWave()` at
`internal/drain/drain_wave.go:L395`, which is the actual answer — it was among
the 132 nodes the smaller budget cut.

Those are the right places. But that is a typed, cross-referenced `grep -n`, not
an explanation: no prose, no behaviour, no answer. **An agent still has to read
the files.** The saving is on the search phase only, and a broad natural-language
query costs upwards of a thousand tokens of node list that may not even contain
what was wanted.

Three commands are unambiguously cheaper than the status quo, because they are
small and exact rather than broad:

| command | cost | what it gives |
|---|---|---|
| `god_nodes` | ~60 tokens | a correct 10-line architectural summary, once, at session start |
| `get_node` / `explain` | ~100 tokens | one symbol's exact `file:line` and its real callers and callees |
| `get_neighbors` | ~200 tokens | what one symbol reaches, filterable by relation |

`explain "attachAsk"` returns ten lines naming `internal/drain/ask.go:L92` and
its three real edges. Reaching that by grep costs several turns and far more
transcript.

### Why a modest saving is still worth having

Drain 4's five model processes billed **2,405,147 cache-read tokens against
22,450 output tokens** (`docs/testrun/drain4-usage.md`). Spend in this engine is
transcript size × turns, because every turn re-sends the whole transcript as
cache reads. Tokens kept out of turn 3 stay out of turns 4 through 16 as well.

That multiplier is the argument for the index and the argument against overusing
it in equal measure: a 1.2k-token query result that did not answer the question is
also re-sent on every remaining turn. Hence the default tool set is the cheap,
exact three, and the broad query is offered but described honestly in the prompt.

---

## Decisions

| Question | Decision |
|---|---|
| Where the graph lives | `.beads/auto/graph/graphify-out/graph.json` in the main checkout, engine-owned |
| When it is built | Once per run before wave 1, refreshed at every wave barrier after the integrator merges |
| How agents query it | The existing `runner.ToolServer` seam — graphify's stdio MCP server, on an explicit allowlist |
| Staleness | Stamped with its commit, refreshed per wave, and the prompt forbids acting on it unconfirmed |
| Cost threshold | None. The build is free, so issue count is irrelevant |
| Fallback | Fail open: no graph means no tool, no prompt text, and the run proceeds as it does today |

### Where the graph lives

`.beads/auto/graph/graphify-out/graph.json`, built by the engine in the main
checkout, one per repo per run.

`.beads/auto/` is already gitignored, which settles two problems at once. The
artifact never appears as untracked in the main checkout, and it can never be
swept into a worker's `git add -A`. `graphify extract --out DIR` writes
`DIR/graphify-out/`, so this path falls out of the tool's own contract rather
than being imposed on it.

Worktrees do not get their own copy. They get an absolute path to the one graph,
which nothing inside a worktree can write to.

Two alternatives were rejected. `graphify-out/` at the repo root is graphify's
own default, but it is not gitignored, so it would leave the main checkout dirty
and would commit this repo to a path graphify owns. A graph per worktree would
mean N builds instead of one, each inside a tree its worker commits wholesale.

### When it is built and refreshed

Built once before wave 1 at the run's base commit, then refreshed with
`graphify update` at each wave barrier, after the integrator has merged that
wave.

The measurement decides this. A refresh costs under a second and no tokens, so
there is no case for letting wave 3 navigate wave 1's codebase. The barrier is
the right moment because it is the only point where a single commit represents
every issue that has landed.

Relying on graphify's own post-commit hook was rejected. It fires in whatever
checkout made the commit, and a run's commits happen in N worker worktrees on N
branches — the hook would fire N times, each rebuilding from a different branch's
view, racing on one output file. Engine-owned and explicit is the only coherent
option once work is concurrent.

### How agents query it

Through `runner.ToolServer`, the seam that already exists for `ask_user`.
`internal/drain/ask.go`'s `attachAsk` is the exact shape to copy:

```go
func (e *Engine) attachGraph(req *runner.Request, in invocation) {
	if e.Graph == nil || in.Runner == nil || !in.Runner.Caps().Tools {
		return
	}
	if e.Cfg == nil || !e.Cfg.GraphRole(string(in.Role)) {
		return
	}
	req.ToolServers = append(req.ToolServers, e.Graph.Spec())
}
```

The server is graphify's own, `<interpreter> -m graphify.serve <abs graph.json>`.
Three things about it are not obvious and each was verified rather than assumed.

**The MCP server is an optional extra.** `graphify.serve` imports `mcp`, which a
plain `graphify` install does not pull in — it needs `pip install
"graphifyy[mcp]"`. A default install therefore has a working `graphify` binary, a
working build, and an MCP server that exits with `ModuleNotFoundError`. The
preflight check must confirm the extra, not merely that graphify is on PATH, and
a missing extra is a fail-open case like any other.

**The interpreter has to be resolved, not read.** The skill writes
`graphify-out/.graphify_python`, but `graphify extract` does not — the headless
build this design uses leaves no such file. Resolve it from the shebang of the
`graphify` binary, which is what the skill itself falls back to. Guessing at
`python3` fails under `uv tool install`, where graphify lives in its own tool
venv, and a worker's PATH is not the engine's — a trap `internal/ask/server.go`
already documents for its own binary.

**The allowlist must be explicit.** The server offers ten tools, and three of them
— `list_prs`, `get_pr_impact`, `triage_prs` — reach GitHub. Handing those to a
worker that runs behind a git guard whose entire job is to stop it publishing
anything would be a hole straight through that guard. `ToolServer.Tools` names
the seven that stay:

```
query_graph  get_node  get_neighbors  get_community  god_nodes  graph_stats  shortest_path
```

Note that `graphify affected` — depth-N reverse traversal, the reviewer's natural
question — is CLI-only and has no MCP equivalent. `get_neighbors` with a
`relation_filter` covers it at depth 1, which is most of the value.

A plain CLI the agents shell out to was rejected for one decisive reason: the
reviewer runs under `permissions: scoped` and therefore has no Bash. That is the
role with the strongest case for reverse traversal, and a CLI is unreachable for
it. Through MCP the tools arrive as `mcp__graph__*` and `qualifiedTools` puts
them in the allowlist automatically.

**All three roles get the index.** The reviewer's exclusion from `ask_user` does
not carry over: that exclusion exists because a reviewer questioning the author of
the work it is judging stops being an independent check, and a read-only index of
the base commit is not that. It is the same class of thing as reading files, which
the reviewer already does. Which roles get it is nonetheless config, mirroring
`ask.roles`.

### Staleness

Three parts, and the third is the one that matters.

1. The graph records the commit it was built from. The engine states that commit
   in the prompt, with the sentence *this index describes `<sha>`; your own
   uncommitted edits are not in it*.
2. The wave-barrier refresh keeps it within one wave of the merged truth.
3. **The prompt states that the index is a finding aid and never a source of
   truth: any fact it returns must be confirmed by reading the file before it is
   acted on.**

The third is not caution for its own sake. The index returns `file:line` and
edges, never behaviour — an agent that answers from it alone is not reading a
stale fact, it is inventing one. Worktrees diverging from base makes that worse,
but it would be true of a perfectly fresh graph too.

### Fallback

Fail open, on the `attachAsk` pattern: three conditions, each ruling out a
different way of getting it wrong. If graphify is not installed, the `[mcp]`
extra is missing, the build fails, or the graph is absent, then the tool is not
attached, the prompt says nothing about it, and every agent works exactly as it
does today. A failed build is logged and never fails a wave.

The index is an accelerator. A run that cannot build one must still be a run.

---

## Configuration

```yaml
# A code index the agents can query instead of grepping. Built by the engine
# with graphify, AST-only: no model, no API key, and no tokens.
#
# Needs `pip install "graphifyy[mcp]"` — the plain package builds a graph but
# cannot serve it. Where the extra is missing the index is silently skipped.
#
# Off until the measurement in stage 4 says otherwise. The build is free, but
# what the agents spend querying it is not, and this repo sets a knob from a
# number rather than from the argument that sounded better.
graph:
  enabled: false
  # Roles offered the tool. The reviewer is included: the index is read-only and
  # describes the base commit, so it is the same class of thing as reading files.
  roles: [worker, reviewer, integrator]
  # Paths kept out of the index. Tests are excluded by default because they
  # dominate connectivity wherever they are indexed and turn the architectural
  # summary into a list of test helpers — measured, see plans/graph-index.md.
  exclude: ["*_test.go", "docs/", "scripts/"]
  # Rebuild at each wave barrier after the integrator merges. Costs under a second.
  refresh: wave        # wave | run | never
```

Excluding tests has one real cost: *where is this tested?* stops being an index
question. That is deliberate. It is also the one search grep is genuinely good at
(`grep -rn TestFoo`), and `exclude` is config for a repo that disagrees.

---

## Proposed implementation issues

`bd-auto` files discovered work itself at the wave barrier, so these are written
here for a human to file rather than created by the run that wrote them.

| # | Title | What it covers |
|---|---|---|
| 1 | `internal/graph`: build the index, or cleanly do nothing | The `graph:` config block, the generated `.graphifyignore`, `graphify extract --code-only --out .beads/auto/graph`, the commit stamp, and a preflight that treats a missing binary or a missing `[mcp]` extra as "no index" rather than an error |
| 2 | Offer the index as a tool server | `attachGraph` mirroring `attachAsk`, the interpreter resolved from the `graphify` shebang, `graph.roles`, and the seven-tool allowlist that keeps the three GitHub tools away from a guarded worker |
| 3 | Tell the roles what the index is and is not | Prompt sections in `prompts/worker.md`, `reviewer.md`, `integrator.md`: start with `god_nodes`, prefer `get_node`/`get_neighbors` over broad queries, and confirm every fact by reading the file |
| 4 | Refresh at the barrier, and measure whether any of it paid | `graphify update` after the integrator merges, plus an A/B in the style of `scripts/resume-vs-fresh.sh` — one fixture epic drained with and without the index, compared on `total_cost_usd`. Flips `enabled` only if the saving is real |

Issue 4 is the one that decides whether this feature stays. Stages 1–3 are cheap
enough to build on an unproven premise; leaving the default on would not be.

---

## What was assumed

The `ask_user` call for the lifecycle decision returned *nobody is watching this
run*, so a human answered none of the six questions. What stands in for that
answer is the measurement section above: every decision here is traceable to a
number or a command output taken from this repo, and the two a human might still
reasonably overturn are called out.

- **The index ships disabled.** Its value is unproven, and this repo has a habit
  of setting knobs from measurements — `max_rounds: 3` cites a real experiment.
  Stage 4 produces the number that should decide the default. A human who wants
  it on before then only has to change one line.
- **Tests are excluded by default.** Measured, and the effect on `god_nodes` is
  stark, but it does trade away *where is this tested?* as an index question.

A human reviewing this plan should read those two as the places to push back.
