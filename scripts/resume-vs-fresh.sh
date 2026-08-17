#!/usr/bin/env bash
# Measure what recovery costs each way, so max_rounds and retry are set from a
# number rather than from the argument that sounded better.
#
# The claim under test: resuming a session is cheaper than a fresh worker,
# because a fresh worker re-reads the issue, re-explores the code and re-derives
# its plan before writing a line. The counterclaim: a resumed session re-sends
# its whole transcript every turn, and the cache warmth that would offset that
# has a five-minute TTL the pipeline can easily outlive.
#
# Both arms drain the same fixture epic from the same starting commit:
#
#   fresh    max_rounds 1, retry 3   every recovery is a new process
#   resume   max_rounds 4, retry 1   every recovery continues the session
#
# The comparison is total_cost_usd, never summed tokens: cache reads bill at a
# fraction of input price, so a token count flatters whichever arm reads more
# cache — which is the resume arm, by construction.
#
# HOW THE FIXTURE FORCES A RECOVERY ROUND
#
# A comparison over work that passes first time measures nothing: both arms
# spend one process per issue and tie. So the fixture contains a stage that no
# worker can pass on its first round. `verify.sh` runs as a `run:` pipeline
# stage after every round; the first time it runs for an issue it mints a random
# token and fails, printing the line the worker must add. The token does not
# exist until the stage has run once, so round one always fails and round two
# always can pass. Exactly one recovery per issue, in both arms, by construction.
#
# The task itself is deliberately not trivial: the worker has to read every file
# under docs/ to produce its answer. That re-derivation is the cost the fresh arm
# pays twice and the resume arm pays once, and a fixture without it would measure
# a saving that does not exist in a real repo.
#
# Usage:
#   scripts/resume-vs-fresh.sh [--model sonnet] [--issues 3] [--out DIR]
#                              [--only fresh|resume] [--keep]
#
# This spawns real models and spends real money. It never touches this repo:
# every arm runs in its own throwaway git repo with its own beads database.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BD_AUTO="$REPO/bin/bd-auto"

MODEL="${MODEL:-sonnet}"
ISSUES="${ISSUES:-3}"
OUT=""
ONLY=""
KEEP=0

usage() {
  sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
  --model) MODEL="$2"; shift 2 ;;
  --issues) ISSUES="$2"; shift 2 ;;
  --out) OUT="$2"; shift 2 ;;
  --only) ONLY="$2"; shift 2 ;;
  --keep) KEEP=1; shift ;;
  -h | --help) usage; exit 0 ;;
  *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[ -x "$BD_AUTO" ] || { echo "build first: make build" >&2; exit 1; }
command -v bd >/dev/null || { echo "bd is not on PATH" >&2; exit 1; }
command -v claude >/dev/null || { echo "claude is not on PATH" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is not on PATH" >&2; exit 1; }

if [ -z "$OUT" ]; then
  OUT="$(mktemp -d -t resume-vs-fresh-XXXXXX)"
fi
mkdir -p "$OUT"
SEED="$OUT/seed"

step() { printf '\n== %s\n' "$1"; }

# --- the fixture --------------------------------------------------------------

# seed builds one throwaway repo: a git history, a beads database, an epic with
# $ISSUES children, and the verify stage that forces the recovery round. Each arm
# gets a copy of it, so both arms genuinely start from the same commit with the
# same issue IDs.
seed() {
  step "fixture: a throwaway repo with an epic of $ISSUES issues"
  rm -rf "$SEED"
  mkdir -p "$SEED/docs"

  # The work: enough reading that re-deriving it costs something real.
  for name in ingest planner scheduler storage transport ui; do
    {
      printf '# %s\n\n' "$name"
      printf 'The %s component. Owner: the %s team.\n' "$name" "$name"
      printf 'Status: shipped.\n'
    } >"$SEED/docs/$name.md"
  done

  cat >"$SEED/verify.sh" <<'VERIFY'
#!/usr/bin/env bash
# The experiment's controlled failure, run by bd-auto after every worker round.
#
# The first invocation for an issue mints a random token and fails, printing the
# line the worker has to add. Nothing can pass this on round one: the token does
# not exist until the stage has run. Every later invocation checks both halves of
# the work, so a resumed round and a fresh attempt are held to the same standard.
#
# It does nothing unless BD_DIFF_FILE is set, which only bd-auto sets. A worker
# that runs this by hand learns no token and mints none, which is what keeps the
# forced round forced.
set -u
if [ -z "${BD_DIFF_FILE:-}" ]; then
  echo "verify: not invoked by bd-auto; nothing to check here"
  exit 0
fi

state="${BD_REPO_ROOT:?BD_REPO_ROOT is required}/.verify-state"
mkdir -p "$state"
tokfile="$state/${BD_ISSUE:?BD_ISSUE is required}"
n="${BD_ISSUE##*-}"

demand() {
  echo "verify: the release token is missing."
  echo
  echo "Create verify.txt in the repository root containing exactly this line:"
  echo
  echo "verified: $1"
  echo
  echo "Then commit it. Leave everything else you have already done in place."
}

if [ ! -f "$tokfile" ]; then
  od -An -N6 -tx1 /dev/urandom | tr -d ' \n' >"$tokfile"
  demand "$(cat "$tokfile")"
  exit 1
fi

tok="$(cat "$tokfile")"
feature="$(ls feature-*.txt 2>/dev/null | head -1)"
if [ -z "$feature" ] || [ ! -s "$feature" ]; then
  echo "verify: no non-empty feature-*.txt in the repository root."
  echo "The issue asks for one. Create it, then re-read the issue for its contents."
  exit 1
fi
if [ ! -f verify.txt ] || ! grep -qxF "verified: $tok" verify.txt; then
  demand "$tok"
  exit 1
fi
echo "verify: $feature and the release token are both present"
exit 0
VERIFY
  chmod +x "$SEED/verify.sh"

  cat >"$SEED/.gitignore" <<'IGNORE'
.beads/auto/
.verify-state/
IGNORE

  cat >"$SEED/README.md" <<'README'
# fixture

A throwaway repo for scripts/resume-vs-fresh.sh. Every component is documented
under docs/; an issue asks for a summary of them.
README

  (
    cd "$SEED" || exit 1
    git init -q -b main .
    git config user.email "resume-vs-fresh@bd-auto.invalid"
    git config user.name "bd-auto experiment"
    git add -A
    git commit -qm "fixture: docs, the verify stage and the ignore list"
    bd init --prefix=fx >/dev/null 2>&1
    # beads' own git hooks are turned off for the fixture, and the reason is not
    # tidiness. Its post-checkout hook imports .beads/issues.jsonl back over the
    # database, so creating the next attempt's worktree reverts the database to
    # the base commit's export — which deletes the failure note bd-auto wrote
    # after the previous attempt. The fresh arm then retries with no idea why it
    # failed and cannot recover at all, and the experiment measures that bug
    # instead of the cost question. Filed separately; disabled here so both arms
    # get their feedback.
    git config --unset core.hooksPath
    git add -A
    # bd init commits its own files, so this often has nothing to do. An empty
    # commit is not a failure here, which is why the subshell ends on true.
    git commit -qm "fixture: beads" >/dev/null 2>&1
    true
  ) || exit 1

  # The pipeline has no gate: verify is the only thing that can fail, which is
  # what keeps the round count the same in both arms.
  EPIC=$(cd "$SEED" && bd create --title="resume-vs-fresh fixture epic" --type=epic --silent) || exit 1
  : >"$SEED/issues.txt"
  i=1
  while [ "$i" -le "$ISSUES" ]; do
    desc="Create a file named feature-$i.txt in the repository root.

It must list, one per line and sorted alphabetically, the first-heading text of
every file under docs/. Read them; do not guess.

Commit it on your branch, then close this issue."
    ac="- feature-$i.txt exists at the repository root and is not empty.
- It lists the heading of every file under docs/, one per line, sorted.
- The work is committed on the issue's branch."
    id=$(cd "$SEED" && bd create --title="summarise docs into feature-$i.txt" \
      --parent="$EPIC" --description="$desc" --acceptance="$ac" --silent) || exit 1
    echo "$id" >>"$SEED/issues.txt"
    i=$((i + 1))
  done
  (cd "$SEED" && git add -A && git commit -qm "fixture: the epic" >/dev/null 2>&1)
  echo "$EPIC" >"$SEED/epic.txt"
  echo "  epic=$EPIC issues=$(tr '\n' ' ' <"$SEED/issues.txt")"
  echo "  seed=$SEED"
}

# config writes an arm's .beads-auto.yaml. The two arms differ in exactly two
# numbers; everything else is held fixed so the difference is the thing measured.
config() { # config <dir> <max_rounds> <retry>
  cat >"$1/.beads-auto.yaml" <<CONFIG
# Written by scripts/resume-vs-fresh.sh. Only max_rounds and retry differ
# between the two arms.
pipeline:
  - stage: implement
  - stage: verify
    run: ./verify.sh

runners:
  default:
    provider: claude
    model: $MODEL
    # bypass, not the shipped default of auto. Under auto a headless worker in a
    # throwaway repo is refused every file write — it has no one to ask — and
    # burns a whole attempt discovering that, which measures the permission
    # prompt rather than the recovery path. The two arms share this setting, so
    # it cancels out of the comparison.
    permissions: bypass

max_rounds: $2
retry: $3
concurrency: 1
autonomy: auto
CONFIG
}

# arm runs one configuration over its own copy of the fixture.
arm() { # arm <name> <max_rounds> <retry>
  local name="$1" rounds="$2" retry="$3"
  local dir="$OUT/$name"
  step "arm $name: max_rounds=$rounds retry=$retry model=$MODEL"
  rm -rf "$dir"
  cp -a "$SEED" "$dir" || return 1
  # git config holds absolute paths, and a copied repo inherits the originals.
  # Anything still pointing into the seed would have both arms sharing state.
  git -C "$dir" config --unset core.hooksPath 2>/dev/null
  config "$dir" "$rounds" "$retry"
  (cd "$dir" && git add -A && git commit -qm "arm $name: config" >/dev/null 2>&1)

  mkdir -p "$OUT/reports/$name"
  local started ended
  started=$(date +%s)
  while read -r id; do
    [ -n "$id" ] || continue
    printf '  %s ... ' "$id"
    local t0 t1
    t0=$(date +%s)
    (cd "$dir" && "$BD_AUTO" issue run --issue "$id" --quiet) \
      >"$OUT/reports/$name/$id.json" 2>"$OUT/reports/$name/$id.log"
    t1=$(date +%s)
    printf '%s (%ss)\n' \
      "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("outcome","no-report"))' \
        "$OUT/reports/$name/$id.json" 2>/dev/null || echo crashed)" \
      "$((t1 - t0))"
  done <"$SEED/issues.txt"
  ended=$(date +%s)
  echo "$((ended - started))" >"$OUT/reports/$name/wall-seconds.txt"
  echo "  arm $name wall clock: $((ended - started))s"
}

# --- the comparison -----------------------------------------------------------

summarise() {
  step "result"
  mkdir -p "$OUT/reports"
  {
    echo "model: $MODEL"
    echo "issues: $ISSUES"
    echo "base: $(git -C "$SEED" rev-parse HEAD 2>/dev/null)"
    echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >"$OUT/reports/meta.txt"
  python3 "$REPO/scripts/resume_vs_fresh_report.py" "$OUT/reports" | tee "$OUT/RESULT.md"
}

# --- main ---------------------------------------------------------------------

seed || exit 1
case "$ONLY" in
fresh) arm fresh 1 3 ;;
resume) arm resume 4 1 ;;
"")
  arm fresh 1 3
  arm resume 4 1
  ;;
*)
  echo "--only takes fresh or resume" >&2
  exit 2
  ;;
esac
summarise

if [ "$KEEP" -eq 0 ] && [ -z "$ONLY" ]; then
  rm -rf "$SEED" "$OUT/fresh" "$OUT/resume"
  echo
  echo "arms removed; reports kept under $OUT/reports (pass --keep to keep the repos)"
else
  echo
  echo "everything kept under $OUT"
fi
