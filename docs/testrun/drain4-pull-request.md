`bd-auto` drained 2 issue(s) onto `bd-auto/epic/kv-169-20260817-213149` in 1 wave(s) and is asking for a human to land it.
Nothing has been merged into `main`: this branch is the whole result.

## Issues

- `kv-169.1` — cli: suggest the nearest command when one is misspelled
- `kv-169.2` — cli: -h and --help print the listing and succeed

## Conflicts a model resolved

Each of these merged two changes that were correct alone. They are the parts of this diff worth reading closely.

- `kv-169.2` (bd-auto/kv-169.2): internal/cli/cli_test.go

## Gate

Green on the merged result:

```
PASS build (0.2s)
PASS vet (0.1s)
PASS test (0.2s)
```

## Run

- 1 wave(s), 2 issue(s) landed, nothing parked
- 197s, $2.75, 120 in / 22450 out tokens, 72 turns
- `kv-169` is closed in beads: 2 child issues completed, integrated and gated
