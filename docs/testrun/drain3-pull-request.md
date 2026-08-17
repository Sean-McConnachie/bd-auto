`bd-auto` drained 2 issue(s) onto `bd-auto/epic/kv-jp9-20260817-205111` in 1 wave(s) and is asking for a human to land it.
Nothing has been merged into `main`: this branch is the whole result.

## Issues

- `kv-jp9.1` — cli: mark the commands that write to the store
- `kv-jp9.2` — cli: wrap the usage listing at 80 columns

## Conflicts a model resolved

Each of these merged two changes that were correct alone. They are the parts of this diff worth reading closely.

- `kv-jp9.2` (bd-auto/kv-jp9.2): internal/cli/cli.go, internal/cli/cli_test.go

## Gate

Green on the merged result:

```
PASS build (0.2s)
PASS vet (0.1s)
PASS test (0.2s)
```

## Run

- 1 wave(s), 2 issue(s) landed, nothing parked
- 304s, $3.20, 130 in / 32334 out tokens, 75 turns
- `kv-jp9` is closed in beads: 2 child issues completed, integrated and gated
