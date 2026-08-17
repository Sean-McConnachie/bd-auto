### Drain 2 — kv-555, every model process

| issue | attempt | round | role | model | turns | cost | in | out | cache read | cache write | seconds |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `kv-555.1` | 1 | 0 | review | claude-sonnet-5 | 5 | $0.1596 | 6 | 620 | 112124 | 19445 | 13.7 |
| `kv-555.1` | 1 | 0 | worker | claude-opus-5 | 14 | $0.9083 | 28 | 12851 | 528800 | 32248 | 162.0 |
| `kv-555.2` | 1 | 0 | review | claude-sonnet-5 | 10 | $0.2772 | 14 | 1640 | 312535 | 26467 | 29.5 |
| `kv-555.2` | 1 | 0 | worker | claude-opus-5 | 22 | $1.3335 | 30 | 20448 | 691588 | 47636 | 248.6 |
| `kv-555.3` | 1 | 0 | review | claude-sonnet-5 | 15 | $0.4195 | 20 | 4896 | 489040 | 33223 | 69.1 |
| `kv-555.3` | 1 | 0 | worker | claude-opus-5 | 19 | $1.1474 | 32 | 17128 | 674258 | 38195 | 266.9 |
| `kv-555.4` | 1 | 0 | review | claude-sonnet-5 | 14 | $0.3272 | 22 | 2280 | 494775 | 24085 | 48.1 |
| `kv-555.4` | 1 | 0 | worker | claude-opus-5 | 17 | $0.8151 | 34 | 6481 | 642051 | 33188 | 93.9 |
| **total** | | | **8 processes** | | **116** | **$5.3879** | **186** | **66344** | **3945171** | **254487** | **932** |

Drain 2's own reports add up to $5.8739 and 129 turns: $4.7316 / 98 for the
first three waves, $1.1423 / 31 for the leg run after `run unpark`. The table
above is $0.4860 and 13 turns short of that, and the missing process is the
worker that was killed in wave 3: unparking reset the attempt counter, so its
retry was named after the same transcript file and overwrote it. That is
`beads-auto-imp-xzx`, filed and fixed — a second process now writes beside the
first rather than over it.
