### Drain 3 — kv-jp9, every model process

| issue | attempt | round | role | model | turns | cost | in | out | cache read | cache write | seconds |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `kv-jp9.1` | 1 | 0 | review | claude-sonnet-5 | 9 | $0.2577 | 14 | 1362 | 309365 | 24078 | 30.1 |
| `kv-jp9.1` | 1 | 0 | worker | claude-opus-5 | 29 | $1.2703 | 58 | 12255 | 1210227 | 35857 | 178.0 |
| `kv-jp9.2` | 1 | 0 | integrator | claude-opus-5 | 15 | $0.6814 | 24 | 6429 | 448767 | 29615 | 82.6 |
| `kv-jp9.2` | 1 | 0 | review | claude-sonnet-5 | 6 | $0.2347 | 8 | 3441 | 159281 | 22544 | 45.7 |
| `kv-jp9.2` | 1 | 0 | worker | claude-opus-5 | 16 | $0.7516 | 26 | 8847 | 478361 | 29116 | 125.8 |
| **total** | | | **5 processes** | | **75** | **$3.1958** | **130** | **32334** | **2606001** | **141210** | **462** |
