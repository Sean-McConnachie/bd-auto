### Drain 4 — kv-169, every model process

| issue | attempt | round | role | model | turns | cost | in | out | cache read | cache write | seconds |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `kv-169.1` | 1 | 0 | review | claude-sonnet-5 | 8 | $0.2687 | 12 | 2017 | 262145 | 26620 | 35.4 |
| `kv-169.1` | 1 | 0 | worker | claude-opus-5 | 16 | $0.8063 | 32 | 8084 | 614512 | 29682 | 110.6 |
| `kv-169.2` | 1 | 0 | integrator | claude-opus-5 | 11 | $0.4770 | 18 | 2301 | 314115 | 26237 | 36.5 |
| `kv-169.2` | 1 | 0 | review | claude-sonnet-5 | 15 | $0.3399 | 22 | 3210 | 490165 | 24103 | 53.4 |
| `kv-169.2` | 1 | 0 | worker | claude-opus-5 | 22 | $0.8547 | 36 | 6838 | 724210 | 32145 | 93.8 |
| **total** | | | **5 processes** | | **72** | **$2.7466** | **120** | **22450** | **2405147** | **138787** | **330** |
