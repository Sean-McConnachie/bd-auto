## The code index

This repo has a pre-built code index at `{{GRAPH}}`. It is a typed,
cross-referenced map of the symbols in the code — extracted from the AST by
`graphify`, with no model involved — and it was built from the commit this run
started on.

**It is a finding aid, not a source of truth.** Every fact it gives you is a
claim about code you have not read: a name, a file, a line, an edge. Confirm it
by reading the file before you act on it. Where the index and the code disagree,
the code is right and the index is out of date.

Four commands are cheap and exact. Each takes `--graph {{GRAPH}}`:

- `graphify god-nodes --graph {{GRAPH}}` — the most connected symbols, which is
  the fastest way to see what the architecture actually hangs off.
- `graphify explain "Name" --graph {{GRAPH}}` — one symbol and its neighbours:
  where it is defined and what it touches.
- `graphify affected "Name" --graph {{GRAPH}}` — what depends on a symbol, which
  is the blast radius of changing it.
- `graphify path "A" "B" --graph {{GRAPH}}` — how two symbols are connected.

`graphify query "<question>"` is the one to avoid. It answers a broad question
with a truncated list of symbol locations rather than an explanation, so it can
cost a thousand tokens of node list that does not contain what you asked for.
Ask it for a named symbol or do not ask it.

None of this replaces reading. The index saves you the search, not the file.
