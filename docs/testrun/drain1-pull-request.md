`bd-auto` drained 3 issue(s) onto `bd-auto/epic/kv-ctf-20260817-200555` in 2 wave(s) and is asking for a human to land it.
Nothing has been merged into `main`: this branch is the whole result.

## Issues

- `kv-ctf.1` — internal/store: the Store interface and an in-memory implementation
- `kv-ctf.2` — internal/store: save a store to JSON and load it back
- `kv-ctf.3` — internal/store: a TTL wrapper that expires keys

## Gate

Green on the merged result:

```
PASS build (0.2s)
PASS vet (0.1s)
PASS test (0.3s)
```

## Discovered, not done

Filed by the workers and deliberately kept out of this run:

- `kv-bw4` — bd's JSONL export dirties the integration checkout and blocks bd-auto's merge

## Run

- 2 wave(s), 3 issue(s) landed, nothing parked
- 345s, $3.04, 112 in / 34763 out tokens, 61 turns
- `kv-ctf` is closed in beads: 3 child issues completed, integrated and gated
