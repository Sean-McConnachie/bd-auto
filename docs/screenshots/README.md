# Screenshots of the live view

Two sets. `tui/` is the deterministic harness — `make tui-shots`, no models, the
real view driven over synthetic events on a real terminal with real keystrokes.
`live/` is three real drains against real models, captured from the tmux pane
they ran in. Two of the live shots are evidence for bugs the exercise found; the
rest is the tool working.

Each `.png` has the `.ansi` capture it was rendered from beside it. The capture
is the exact bytes the view wrote, so it diffs and it greps; the image is for
reading.

## `tui/` — every state the table has

| shot | what it shows |
|---|---|
| `tui-01-scope` | the scope, before anything is spawned: every issue queued |
| `tui-02-scope-parked` | an issue parked before a worker exists — its dependency is outside the scope |
| `tui-03-wave-running` | a wave in flight — one done, two running, the rest waiting, costs and clocks |
| `tui-04-streaming` | the activity column following a message as it is written |
| `tui-05-selection` | `↓` `↓`: the cursor, and a table that does not shift sideways |
| `tui-06-killing` | `k`: the row says `killing` before the process has died |
| `tui-07-killed` | the kill landing — parked, reported failed, the wave carries on |
| `tui-08-integrating` | the barrier at work: a model resolving a conflict, on the row whose branch it is |
| `tui-09-wave-integrated` | the barrier's verdict — what merged, what did not, and the gate on the merged result |
| `tui-10-paused` | held at the barrier under `autonomy: wave` |
| `tui-11-resumed` | released |
| `tui-12-question` | a worker asking, with the rest of the wave still running |
| `tui-13-question-choice` | `↓` inside the box moves the choice, not the table |
| `tui-14-question-typing` | `t`: an answer the options did not offer, spaces and all |
| `tui-15-question-queued` | two workers asking at once — `(1 more waiting)` |
| `tui-16-question-swallowed` | `k` while a question is up: the table's keys are suspended |
| `tui-17-question-answered` | answered by the number printed beside the option |
| `tui-18-question-declined` | `s`: handed back to the model, told to record what it assumed |
| `tui-19-question-free-text` | a question with no options at all |
| `tui-20-terminal-states` | done, failed, killed and stopped, side by side |
| `tui-21-narrow` | 64 columns: the fixed columns hold and the activity gives way |
| `tui-22-wide` | back to 118 |
| `tui-23-stopping` | `q` once: the run is asked to stop and the table stays up |
| `tui-24-run-end` | the run's own ending, with the total the report carries |
| `tui-25-finished` | the last frame — the one the renderer used to eat |
| `ro-01-readonly` | a view with no channel back: it shows the question, and says so |
| `ro-02-readonly-refused` | `k` on a view that cannot kill anything |
| `ro-03-readonly-dismissed` | the key line a watcher gets |

## `live/` — the three real drains

| shot | what it shows |
|---|---|
| `bug-wave-numbering` | the evidence for `beads-auto-imp-dql.8`: rows saying wave `-` above a status line saying "wave 1 integrated" |
| `bug-invisible-integrator` | the evidence for `beads-auto-imp-wne`: a model resolving a conflict, and a table that says nothing |
| `live-01-wave2-running` | drain 1, wave 2, two real workers in parallel |
| `live-02-drain1-finished` | drain 1 finished: 3 done, 0 parked |
| `live-03-cost-lands-when-a-stage-ends` | a cost appearing the instant the implement stage ends |
| `live-04-paused-at-barrier` | drain 2 held at the wave 1 barrier |
| `live-05-resumed-wave2` | released, and wave 2 dispatched |
| `live-06-question` | a real worker asking a real question, mid-wave |
| `live-07-question-choice` | the choice cursor moved to the second option |
| `live-08-question-swallows-k` | `k`, refused while the question is up |
| `live-09-question-typing` | typing an answer the worker did not offer |
| `live-10-question-answered` | answered, and the worker back to running |
| `live-11-streaming` | a real message arriving token by token in the activity column |
| `live-12-wave2-barrier` | drain 2's wave 2 barrier, both branches merged and the gate green |
| `live-13-selection` | the cursor moved down a live table |
| `live-14-killing` | `k` on a running worker: the row says killing before the process dies |
| `live-15-killed` | the kill landed — parked, and the run went on to its barrier |
| `live-16-unparked-resumed` | `run unpark` and a fresh leg: wave 4, the same issue running again |
| `live-17-two-workers-one-file` | drain 3: two workers rewriting the same function, in parallel |
| `live-18-drain3-finished` | drain 3 finished — the conflict resolved, the gate green, a pull request opened |
