# A live validation of bd-auto

A small program, built end to end by bd-auto in a throwaway repo, with every
state of the live view photographed and every model process accounted for.

Nothing in this exercise reached GitHub. `origin` is a bare repository on the
same machine, and the pull requests were opened through a recording stand-in for
`gh` that saves the body and answers with a URL — so the whole handoff path ran,
push included, without publishing anything.

## What was built

`kv`, a key-value store on the command line, in a fresh repo whose seed commit
held nothing but a stub `main` and a `go.mod`. Three beads epics, nine issues, a
dependency graph shaped to produce several waves and genuine parallelism:

```
kv-ctf  the store package          kv-555  the kv command line
  kv-ctf.1  Store + Memory           kv-555.1  the dispatch table
  kv-ctf.2  JSON save and load       kv-555.2  get, set, del
  kv-ctf.3  a TTL wrapper            kv-555.3  list, in two formats
                                     kv-555.4  main, wired up
kv-jp9  the usage listing
  kv-jp9.1  mark the commands that write     ┐ both rewrite the same loop,
  kv-jp9.2  wrap the listing at 80 columns   ┘ on purpose, in one wave
```

The gate is `go build ./...`, `go vet ./...` and `go test ./...`, run in each
worker's worktree and again on the merged result at every barrier. Workers are
opus, reviewers sonnet and read-only, permissions `bypass` — the configuration
the README says a real drain needs.

It works:

```
$ kv set greeting hello && kv set who world && kv list --format json
[
  {
    "key": "greeting",
    "value": "hello"
  },
  {
    "key": "who",
    "value": "world"
  }
]
$ kv
kv - a small key-value store
...
commands:
  del*  del <key>
  get   get <key>
  list  list [--format text|json]
  set*  set <key> <value>

* changes the store
```

That last listing is the merged result of two branches that both rewrote the
function printing it. A model resolved the conflict; see drain 3.

## The runs

| | drain 1 — `kv-ctf` | drain 2 — `kv-555` | drain 3 — `kv-jp9` |
|---|---|---|---|
| scope | 3 issues | 4 issues, over three legs | 2 issues |
| autonomy | `auto` | `wave` — paused at every barrier | `auto` |
| waves | 2 | 3 + 1 after an unpark | 1 |
| what it exercised | the happy path end to end | a question, a kill, a park, an unpark, a resume | two workers on one function, a real conflict |
| pull request | opened | refused — an issue was parked | opened |
| cost / turns | $3.0383 / 61 | $5.8739 / 129 | $3.1958 / 75 |

Reports, pull request bodies and per-process usage are the files beside this
one. The screenshots are in [`../screenshots`](../screenshots): `live/` is these
runs, `tui/` is the deterministic harness that walks the view through every
state it has.

Drain 2 opening no pull request is the tool behaving: a run that parked required
work hands nothing over. The park itself was a bug, `beads-auto-imp-joa` below.

## The goals, and how each was settled

| # | goal | verdict |
|---|---|---|
| 1 | A drain builds working software from beads issues alone | met — the program above, 9 issues, no human edit to any of it |
| 2 | Waves respect the dependency graph and run in parallel where it allows | met — drain 1 ran waves of 1 then 2, drain 2 of 1, 2, 1, drain 3 of 2 |
| 3 | The gate runs per worker and again on the merged result | met — every barrier reports its own gate; `gate_passed` in each report |
| 4 | A conflict between two correct changes is resolved by a model, not by hand | met — drain 3, `internal/cli/cli.go` and its test, resolved and gated green |
| 5 | A run publishes nothing until it is finished and clean | met — every merge landed on a `bd-auto/epic/…` branch; `main` was written to only by hand, afterwards |
| 6 | A finished, clean run pushes and opens a pull request | met — drains 1 and 3, through the recording `gh`; bodies beside this file |
| 7 | A run with parked work refuses to hand over | met — drain 2, "1 parked issue(s) are required work that did not get done" |
| 8 | A worker can ask a human a question and use the answer | met — drain 2, kv-555.3 asked which JSON shape to emit and built the one it was told |
| 9 | `k` kills the selected worker, parks it, and the run carries on | met — drain 2 wave 3; `run unpark` then retried it |
| 10 | `q` stops the run; `run resume` continues it | met — drain 2 paused at each barrier under `autonomy: wave` and was resumed three times |
| 11 | Every state of the live view renders correctly at 64 and 118 columns | met — 28 deterministic screenshots, `make tui-shots` |
| 12 | The run's own cost and turn totals match the raw transcripts | met — drain 1 $3.0383/61 and drain 3 $3.1958/75 reconcile exactly; drain 2 does not, and that is `beads-auto-imp-xzx` below |
| 13 | Every defect found is filed in beads and fixed immediately | met — nine issues, below |

## What it found

Every one of these was filed in beads, fixed, and covered by a test that was
watched failing before the fix went in.

| issue | what | how it showed up |
|---|---|---|
| `beads-auto-imp-dql.6` | `config show` omitted the whole `ask` block | reading the config before the first run |
| `beads-auto-imp-dql.7` | the renderer ate the last line of the final frame | photographing the end of a run |
| `beads-auto-imp-dql.8` | every row said wave `-` while the status line said wave 1 | drain 1, first screenshot |
| `beads-auto-imp-dql.9` | a staged beads export parked every issue at the barrier | drain 1: "1 done, 2 parked" printed above three `done` rows |
| `beads-auto-imp-dql.10` | turns were not carried at all | asked for by this exercise; now in `Usage` and the handoff line |
| `beads-auto-imp-dql.11` | the README's example table showed a cost a running row cannot have | comparing the README to a live frame |
| `beads-auto-imp-joa` | unstaging that export once per barrier is undone by the next `bd` read | drain 2 wave 4 parked finished, gated, reviewed work |
| `beads-auto-imp-crh` | a row the barrier parked kept saying `done` | drain 2's last frame: "1 done · 0 parked" above "3 done, 1 parked" |
| `beads-auto-imp-xzx` | an unparked retry overwrote the transcript of the attempt it retried | drain 2's per-process total is $0.4860 and 13 turns short of its report |
| `beads-auto-imp-wne` | the barrier was invisible: a model resolving a conflict showed as an idle table | drain 3 spent minutes there saying "0 running" |

`joa` is the one worth reading twice. `bd show` — a read — re-exports
`.beads/issues.jsonl` and stages it, so unstaging at the top of the barrier and
merging afterwards fixes nothing: the barrier reads one issue per candidate in
between. The proof is in the lab, on the branch that failed: with the export
staged the identical merge fails, and after the fix the same barrier reports
"1 merged, 0 parked, gate passed".
