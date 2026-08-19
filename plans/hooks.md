# bd-auto: hooks — somewhere to hang an agent that reads a result

## Context

`beads-auto-imp-04l.5` asked for a custom post-result interpreter: an agent of
the repo's own that reads what a stage or a run produced and does something with
it. There was nowhere to put one, and the issue said the design had to be
settled before anything was built. This is that decision. The code that
implements it is `internal/config/hooks.go` and `internal/drain/hooks.go`; both
carry the reasoning at the point it applies, and this document is the whole of
it in one place.

## What already interprets a result

Worth listing first, because most of it is deliberately not a model and stays
that way. Only two of these are models at all:

| Where | What decides | Model? |
|---|---|---|
| review stage | whether a diff meets the issue's acceptance criteria | yes, the reviewer |
| merge conflict | how two correct changes reconcile | yes, the integrator, once per conflict |
| feedback composition | every string sent back to a failed worker | no — Go, `internal/drain/text.go` |
| gate blame | which merge turned the wave gate red | no — peel and re-merge, `integrate.go` |
| discovery dedup | whether a finding is one bd already has | no — text matching |
| handoff | the pull request body | no — Go, `handoff.go` |
| epic close | whether the run finished the epic | no — a tested function |

The shape of that table is the point. The pipeline is per-issue and it stops at
the verdict; everything after the verdict is Go with nothing attached to it. A
repo could add a stage, and that stage would be *in the path of the decision*.
It could not add something that reads the decision afterwards.

## The three points

A hook point is a moment where a result exists and nothing is still being
decided about it. There are exactly three:

| Point | Fires | Receives |
|---|---|---|
| `on_issue_end` | after an issue reaches a verdict, before the wave loop records it | that issue's `Report` |
| `on_barrier` | after a wave has merged, gated, reconciled, filed discoveries and settled the epic | the `IntegrateReport` |
| `on_run_end` | after the run has finished and handed over | the `DrainReport` |

Each takes `agent: <role>` or `run: <command>` — the same two forms a pipeline
stage takes, and validated at config load the same way, so a hook naming a role
that does not exist fails with the roles it may use rather than at the barrier
it would have run at.

### Why these three and not others

The obvious fourth candidate is per-stage: a hook after the review stage. It was
left out because a stage's result is not final — the same stage runs again next
round on the same issue — so a hook there would fire several times about work
that is still moving, and the natural thing to do with its output would be to
feed it back, which is the authority this deliberately does not have.

`on_run_end` fires **after** the handoff rather than before it. Before the
handoff, a hook would look like something that could stop one; after it, the
report it reads is the whole run including where it was handed over. Since a
hook cannot stop a handoff either way, the honest position is the one that also
gives it the better input.

## The authority rule: advisory

**A hook reads. Its output is recorded on the run's report and shown to whoever
is watching. bd-auto reads nothing back out of it.** No hook can change a
verdict, park an issue, fail a run or stop a pull request. No verdict is parsed
from an agent hook's reply — a hook that says `VERDICT: fail` about a finished
issue changes nothing about it, which matters because a hook role with no prompt
of its own falls back to the *reviewer's* prompt, and that prompt asks for a
verdict line.

The argument is asymmetric cost, not caution:

- An advisory hook that is wrong costs a paragraph of wrong text on a report
  nobody has to act on.
- An authoritative hook that is wrong parks finished work, and the repo cannot
  see why without reading a transcript. It puts a prompt nobody reviewed in
  front of every verdict the engine reaches.

And the two are not symmetric in time either. Authority can be added later, per
hook, explicitly, once something has actually needed it. It cannot be taken back
once a repo's workflow depends on it.

The consequence worth stating plainly: **the engine never acts on a hook**, so
the engine never becomes a second writer on a hook's behalf. That is not a
side-effect of advisory-ness; it is most of what makes it safe.

## The input contract

A hook is handed a **file of already-defined report JSON** — the same `Report`,
`IntegrateReport` and `DrainReport` a `--json` run emits, with the same field
names — written to `.beads/auto/hooks/<point>[-<key>].json`.

Three things follow from "already-defined" that would not follow from a shape
invented for hooks:

1. A hook written against a report cannot drift from what the run says it did,
   because there is one serialisation and the renderers use it too.
2. A hook can be developed against a saved `--json` run with no run in flight.
3. Adding a field to a report is not a second thing to remember.

A path rather than stdin, for the same reason a `run:` stage gets
`$BD_DIFF_FILE`: the thing being handed over is already a file worth keeping,
and a command that wants two fields of it should not have to consume all of it.
The file is left on disk after the hook exits — it is small, it is in the
gitignored run directory beside the transcripts and the review notes, and it is
the evidence for whatever the hook said about it.

A `run:` hook gets `$BD_REPORT_FILE`, `$BD_HOOK`, `$BD_HOOK_POINT`,
`$BD_REPO_ROOT`, and `$BD_ISSUE` at `on_issue_end`. An `agent:` hook is told the
path in its task text.

## What a hook may not do

These are the engine's existing constraints, not new ones, and they were not
negotiable.

**One writer per issue.** At `on_barrier` and `on_run_end` no worker is live at
all, so a hook there can write what it likes. `on_issue_end` is the one point
that runs beside live siblings, and it is handled structurally rather than by
instruction: it fires from inside `Engine.Issue`, after every bd write the
engine makes about that issue including the park, and it is handed exactly one
issue — its own, whose worker has exited. Its task text says so; its `$BD_ISSUE`
names only that one.

**Not a place to run git.** Hooks run in the main checkout, which the run is
using: the barrier merges into it and the next wave's worktrees branch from it.
The task text says so.

**Not a place to hang.** Every hook is bounded. A `run:` hook by
`pipeline.Exec`'s own timeout; an `agent:` hook by a context deadline wrapping
the *whole* invocation, retries and backoff included, so a hook that meets a rate
limit cannot turn an outage into an unbounded wait at a barrier. The default is
300s — shorter than the 900s a gate command gets, because a gate is what the run
is waiting to hear from and a hook is not. **There is no `timeout: 0`.**
Unlimited is exactly what this promise excludes, and a runner's `timeout: 0`
meaning unlimited is the one place hooks differ from the rest of the config.

**Nothing fires for a non-verdict.** An interrupt and an outage are not results.
Nothing was judged, the run is on its way out or waiting to be re-run, and a
hook fired on one would be reading a report about work that has not finished
happening — into a context that is already cancelled, so it would be recorded as
interrupted too.

## Where the output goes

`Report.Hooks`, `IntegrateReport.Hooks` and `DrainReport.Hooks` — a
`HookResult` each, carrying what the hook said (bounded by `output_tail_bytes`,
like everything else that reaches a report), what it cost, and why it did not
complete where it did not. `EventHookStart` and `EventHookEnd` carry the same
thing live, and both shipped renderers show them.

Two smaller choices inside that:

**`OK`, not `Passed`.** A stage's `Passed` is a verdict that routes an issue.
This says only that the hook ran to completion — exit 0 for a command, a
finished turn for a model — and nothing in the engine reads it. Naming it
`Passed` would invite the first patch that branches on it.

**A hook's model activity does not stream to a row.** The hook's own start and
end are on the bus, and the transcript is on disk at `LogPath`, but the live
tool calls are dropped. There is nowhere honest to put them: `on_issue_end` runs
after its row reached a terminal state holding the outcome a reader came for,
and the other two points have no row at all. The TUI puts hooks on the status
line for the same reason.

**A hook's cost is on the report.** A hook cannot decide anything, but it
spends, and its usage is added to the issue's or the run's total. A report that
hid it would make hooks look free.

## The first customer

Triage. Deciding whether a discovered finding is new work, a duplicate of
something already filed, or noise is exactly a post-result interpretation. It is
currently text matching plus a human running `bd-auto triage`, and the repo has
measured evidence about what workers discover: 2.27 issues created per issue
closed.

As an `on_barrier` agent hook, the barrier's report already carries what it
filed under `discoveries`, so the input needs nothing new. The hook reads it and
says, per finding, new / duplicate-of-X / noise. Nothing is filed, closed or
discarded because of it — that stays the human's call at `bd-auto triage`, which
now has the reading to go with the list.

That is also the test of whether advisory is enough. If reading the list next to
the list turns out to be worth less than a hook that could file, this is the
place the argument for authority will be made from, with evidence.

## What was not built

- **Authority of any kind.** No hook can affect the run. See above.
- **A per-stage hook point.** A stage's result is not final; see above.
- **A hook that fires on an interrupt.** Nothing to interpret.
- **Streaming a hook's tool calls into the live view.** No honest row to put
  them on.
- **The `ask_user` tool for a hook.** A hook cannot act on an answer, so a
  question from one asks a human to decide something nothing will then do —
  and at `on_run_end` it asks it of a view that is about to close. A hook that
  meets a genuine ambiguity says so in its output.
