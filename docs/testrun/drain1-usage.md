### Drain 1 — the store epic (kv-ctf), per model process

| issue | attempt | round | role | model | turns | cost | in | out | cache read | cache write | seconds |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `kv-ctf.1` | 1 | 0 | review | claude-sonnet-5 | 5 | $0.1582 | 6 | 824 | 111056 | 18747 | 13.7 |
| `kv-ctf.1` | 1 | 0 | worker | claude-opus-5 | 14 | $0.6124 | 28 | 6424 | 453726 | 22481 | 93.5 |
| `kv-ctf.2` | 1 | 0 | review | claude-sonnet-5 | 8 | $0.2240 | 12 | 1232 | 253299 | 21585 | 22.9 |
| `kv-ctf.2` | 1 | 0 | worker | claude-opus-5 | 15 | $0.9929 | 30 | 14165 | 592975 | 34214 | 194.5 |
| `kv-ctf.3` | 1 | 0 | review | claude-sonnet-5 | 6 | $0.2213 | 10 | 1507 | 208769 | 22667 | 27.4 |
| `kv-ctf.3` | 1 | 0 | worker | claude-opus-5 | 13 | $0.8295 | 26 | 10611 | 495118 | 31658 | 124.5 |
| **total** | | | **6 processes** | | **61** | **$3.0383** | **112** | **34763** | **2114943** | **151352** | **476** |
