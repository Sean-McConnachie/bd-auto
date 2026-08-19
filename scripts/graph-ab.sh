#!/usr/bin/env bash
# Measure whether the code index pays for itself, so graph.enabled is set from a
# number rather than from the argument that sounded better.
#
# The claim under test: a worker that can query a pre-built symbol index spends
# less finding its way around a repo it has never seen. The counterclaim, which
# planning already half proved: `graphify query` returns a truncated list of
# file:line and never prose, so the index can replace a search phase but not a
# reading phase — and a role that has to read the file anyway may have paid for
# the index and got nothing.
#
# Both arms drain the same fixture epic from the same starting commit:
#
#   off   graph.enabled false   the worker greps, globs and reads
#   on    graph.enabled true    the worker also has the index and the four
#                               commands the prompt names
#
# The comparison is total_cost_usd, never summed tokens: cache reads bill at a
# fraction of input price, so a token count flatters whichever arm reads less
# transcript rather than whichever arm is cheaper.
#
# THE FIXTURE IS THIS REPO
#
# Deliberately, and it is the whole design. An index over a fixture invented for
# the experiment measures nothing: the saving being claimed is over a real
# codebase with real cross-references, and this is the codebase the plan measured
# at 1241 nodes and 3524 edges. Each arm gets its own clone at the current HEAD.
#
# The issues ask for documentation of machinery the worker has to navigate to
# describe — every function on one path, with its file and line. That is the
# shape of task where an index should win if it ever wins, and the deliverable is
# a new file under docs/, so no arm can break the other's gate. A `run:` stage
# checks the required symbols are named, so both arms are held to one standard.
#
# Usage:
#   scripts/graph-ab.sh [--model sonnet] [--out DIR] [--only off|on]
#                       [--keep] [--dry-run]
#
# --dry-run builds both fixtures, resolves both configs and prints what would be
# spawned, without spawning anything. It costs nothing and is how this script is
# tested.
#
# Without --dry-run this spawns real models and spends real money. It never
# touches this repo: every arm runs in its own throwaway clone with its own beads
# database.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BD_AUTO="$REPO/bin/bd-auto"

MODEL="${MODEL:-sonnet}"
OUT=""
ONLY=""
KEEP=0
DRY=0

usage() {
  sed -n '2,45p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
  --model) MODEL="$2"; shift 2 ;;
  --out) OUT="$2"; shift 2 ;;
  --only) ONLY="$2"; shift 2 ;;
  --keep) KEEP=1; shift ;;
  --dry-run) DRY=1; shift ;;
  -h | --help) usage; exit 0 ;;
  *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[ -x "$BD_AUTO" ] || { echo "build first: make build" >&2; exit 1; }
command -v bd >/dev/null || { echo "bd is not on PATH" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is not on PATH" >&2; exit 1; }
if [ "$DRY" -eq 0 ]; then
  command -v claude >/dev/null || { echo "claude is not on PATH" >&2; exit 1; }
fi

# The one check without which the experiment silently measures nothing. With no
# graphify the on arm builds no index, attaches no prompt section and is the off
# arm under another name — and the report would print a difference of noise as
# though it were a result.
command -v graphify >/dev/null || {
  echo "graphify is not on PATH, so the 'on' arm would be identical to 'off'" >&2
  echo "install it (pipx install graphifyy) or this measures nothing" >&2
  exit 1
}

if [ -z "$OUT" ]; then
  OUT="$(mktemp -d -t graph-ab-XXXXXX)"
fi
mkdir -p "$OUT"
SEED="$OUT/seed"

step() { printf '\n== %s\n' "$1"; }

# --- the fixture --------------------------------------------------------------

# The three paths a worker is asked to describe. Each one crosses several files
# and is real machinery in this repo, so the answer cannot be guessed from a
# file listing — which is exactly the navigation an index claims to make cheap.
#
# Fields: slug | title | the symbols the check demands
TOPICS='ask-deadline|how a worker'"'"'s question gets a deadline and what happens when it expires|Broker,enqueue,deadline,Poll
discovery-triage|how work a worker discovered reaches a human|harvest,fileDiscoveries,stageDiscoveries,Accept
merge-conflict|what happens at the barrier when a merge conflicts|mergeBranch,conflictRequest,completeMerge,blameGate'

# seed builds one throwaway clone of this repo: a git history, a beads database,
# an epic with one issue per topic, and the check stage. Each arm gets a copy, so
# both arms start from the same commit with the same issue IDs.
seed() {
  step "fixture: a clone of this repo at $(git -C "$REPO" rev-parse --short HEAD)"
  rm -rf "$SEED"
  git clone -q --no-hardlinks "$REPO" "$SEED" || return 1

  # The fixture gets its own beads database. The clone carries this repo's, and
  # an experiment that files against the real backlog is one that edits the thing
  # it is measuring.
  rm -rf "$SEED/.beads"
  mkdir -p "$SEED/docs/machinery"

  cat >"$SEED/check.sh" <<'CHECK'
#!/usr/bin/env bash
# Held to one standard in both arms: the document exists, is substantial, and
# names every symbol on the path it was asked to describe.
#
# It checks for the symbol names rather than for prose quality because the
# experiment is about what navigation costs, not about writing. A worker that
# names all four has been to the right places whichever way it got there.
set -u
if [ -z "${BD_DIFF_FILE:-}" ]; then
  echo "check: not invoked by bd-auto; nothing to check here"
  exit 0
fi

doc="${CHECK_DOC:?CHECK_DOC is required}"
if [ ! -f "$doc" ]; then
  echo "check: $doc does not exist. The issue names the file to write."
  ls docs/machinery/*.md >/dev/null 2>&1 && echo "There is a document under docs/machinery/, but not that one."
  exit 1
fi
if [ "$(wc -l <"$doc")" -lt 15 ]; then
  echo "check: $doc is under fifteen lines. The issue asks for each function, its file and its line."
  exit 1
fi
missing=""
IFS=',' read -ra want <<<"${CHECK_SYMBOLS:?CHECK_SYMBOLS is required}"
for sym in "${want[@]}"; do
  grep -q "$sym" "$doc" || missing="$missing $sym"
done
if [ -n "$missing" ]; then
  echo "check: $doc does not mention:$missing"
  echo "Each is on the path the issue asks about. Find it and say where it is."
  exit 1
fi
echo "check: $doc names every symbol on the path"
exit 0
CHECK
  chmod +x "$SEED/check.sh"

  (
    cd "$SEED" || exit 1
    git config user.email "graph-ab@bd-auto.invalid"
    git config user.name "bd-auto experiment"
    git add -A
    git commit -qm "fixture: the check stage and the docs directory"
    bd init --prefix=gx >/dev/null 2>&1
    # beads' post-checkout hook imports .beads/issues.jsonl back over the
    # database, which reverts the failure note bd-auto writes between attempts.
    # Off here for the same reason resume-vs-fresh turns it off: with it on, the
    # experiment measures that bug instead of its own question.
    git config --unset core.hooksPath 2>/dev/null
    git add -A
    git commit -qm "fixture: beads" >/dev/null 2>&1
    true
  ) || exit 1

  EPIC=$(cd "$SEED" && bd create --title="graph A/B fixture epic" --type=epic --silent) || exit 1
  : >"$SEED/issues.txt"
  : >"$SEED/symbols.txt"
  while IFS='|' read -r slug title symbols; do
    [ -n "$slug" ] || continue
    desc="Write docs/machinery/$slug.md describing $title.

Name every function involved, in the order control reaches them, and give the
file and line of each. Read the code; do not guess. Four hundred words is
plenty — this is a map, not an essay.

Commit it on your branch, then close this issue."
    ac="- docs/machinery/$slug.md exists and is at least fifteen lines.
- It names every function on the path, with the file and line of each.
- The work is committed on the issue's branch."
    id=$(cd "$SEED" && bd create --title="document $title" \
      --parent="$EPIC" --description="$desc" --acceptance="$ac" --silent) || exit 1
    echo "$id" >>"$SEED/issues.txt"
    echo "$id|docs/machinery/$slug.md|$symbols" >>"$SEED/symbols.txt"
  done <<<"$TOPICS"
  (cd "$SEED" && git add -A && git commit -qm "fixture: the epic" >/dev/null 2>&1)
  echo "$EPIC" >"$SEED/epic.txt"
  echo "  epic=$EPIC issues=$(tr '\n' ' ' <"$SEED/issues.txt")"
  echo "  seed=$SEED"
}

# config writes an arm's .beads-auto.yaml. The arms differ in graph.enabled and
# nothing else; every other knob is held fixed so the difference is the thing
# measured.
config() { # config <dir> <enabled>
  cat >"$1/.beads-auto.yaml" <<CONFIG
# Written by scripts/graph-ab.sh. Only graph.enabled differs between the arms.
gate:
  - name: build
    run: go build ./...

pipeline:
  - stage: implement
  - stage: gate
  - stage: check
    run: ./check.sh

runners:
  default:
    provider: claude
    model: $MODEL
    # bypass, not the shipped default of auto. Under auto a headless worker is
    # refused every file write and burns its attempt discovering that, which
    # measures the permission prompt rather than the index. Both arms share it,
    # so it cancels out.
    permissions: bypass

max_rounds: 3
retry: 1
concurrency: 1
autonomy: auto

graph:
  enabled: $2
  exclude_tests: true
  refresh: true
  roles: [worker]
CONFIG
}

# arm runs one configuration over its own copy of the fixture.
arm() { # arm <name> <enabled>
  local name="$1" enabled="$2"
  local dir="$OUT/$name"
  step "arm $name: graph.enabled=$enabled model=$MODEL"
  rm -rf "$dir"
  cp -a "$SEED" "$dir" || return 1
  # git config holds absolute paths, and a copied repo inherits the originals.
  git -C "$dir" config --unset core.hooksPath 2>/dev/null
  config "$dir" "$enabled"
  (cd "$dir" && git add -A && git commit -qm "arm $name: config" >/dev/null 2>&1)

  mkdir -p "$OUT/reports/$name"
  local started ended
  started=$(date +%s)
  while read -r line; do
    [ -n "$line" ] || continue
    local id doc symbols rest
    id="${line%%|*}"
    rest="${line#*|}"
    doc="${rest%%|*}"
    symbols="${rest#*|}"
    printf '  %s ... ' "$id"
    if [ "$DRY" -eq 1 ]; then
      printf 'would run (%s; symbols: %s)\n' "$doc" "$symbols"
      continue
    fi
    local t0 t1
    t0=$(date +%s)
    (cd "$dir" && CHECK_DOC="$doc" CHECK_SYMBOLS="$symbols" \
      "$BD_AUTO" issue run --issue "$id" --quiet) \
      >"$OUT/reports/$name/$id.json" 2>"$OUT/reports/$name/$id.log"
    t1=$(date +%s)
    printf '%s (%ss)\n' \
      "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("outcome","no-report"))' \
        "$OUT/reports/$name/$id.json" 2>/dev/null || echo crashed)" \
      "$((t1 - t0))"
  done <"$SEED/symbols.txt"
  ended=$(date +%s)
  echo "$((ended - started))" >"$OUT/reports/$name/wall-seconds.txt"
  echo "  arm $name wall clock: $((ended - started))s"

  # Said out loud, because the on arm silently having no index is the one way
  # this experiment lies rather than fails.
  if [ "$enabled" = "true" ] && [ "$DRY" -eq 0 ]; then
    local built
    built=$(cd "$dir" && "$BD_AUTO" config show 2>/dev/null |
      python3 -c 'import json,sys;print(json.load(sys.stdin)["graph"]["built"])' 2>/dev/null)
    echo "  index built in the on arm: ${built:-unknown}"
  fi
}

# --- the comparison -----------------------------------------------------------

summarise() {
  step "result"
  mkdir -p "$OUT/reports"
  {
    echo "model: $MODEL"
    echo "issues: $(wc -l <"$SEED/issues.txt" | tr -d ' ')"
    echo "fixture: a clone of bd-auto at $(git -C "$REPO" rev-parse HEAD)"
    echo "graphify: $(graphify --version 2>/dev/null || echo unknown)"
    echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >"$OUT/reports/meta.txt"
  python3 "$REPO/scripts/ab_report.py" "$OUT/reports" \
    --arms off,on \
    --labels "no index|index, roles: [worker]" \
    --title "The code index: measured" | tee "$OUT/RESULT.md"
}

# --- main ---------------------------------------------------------------------

seed || exit 1
case "$ONLY" in
off) arm off false ;;
on) arm on true ;;
"")
  arm off false
  arm on true
  ;;
*)
  echo "--only takes off or on" >&2
  exit 2
  ;;
esac

if [ "$DRY" -eq 1 ]; then
  step "dry run"
  echo "  both fixtures built and configured; nothing was spawned and nothing was spent"
  echo "  arms under $OUT"
  exit 0
fi

summarise

if [ "$KEEP" -eq 0 ] && [ -z "$ONLY" ]; then
  rm -rf "$SEED" "$OUT/off" "$OUT/on"
  echo
  echo "arms removed; reports kept under $OUT/reports (pass --keep to keep the repos)"
else
  echo
  echo "everything kept under $OUT"
fi
