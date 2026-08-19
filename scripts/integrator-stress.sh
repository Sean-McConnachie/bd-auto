#!/usr/bin/env bash
# Drive a real drain into the states that have actually cost this project work,
# and check the barrier comes out of them with the work intact.
#
#   scripts/integrator-stress.sh [--keep] [issues]
#
# What makes this different from scripts/smoke.sh: smoke drains issues that
# cannot collide, and asks whether the machinery runs at all. This one is built
# so that every issue after the first conflicts with what the last one landed,
# so the barrier has to spawn an integrator for each of them -- and while it is
# doing that, the beads export is being rewritten underneath it in the main
# checkout, staged and unstaged, exactly as a worker's commit and a plain
# `bd show` leave it.
#
# The two together are what beads-auto-imp-zjf was: separately each is handled,
# and the run that met both parked five reviewed branches in six seconds.
#
# It spawns no models. `provider: fake` with a command runs a shell script in
# place of one (internal/runner/fake/exec.go), so the engine is the real engine
# and the work is scripted. The same script is the worker and the integrator: it
# resolves if it is standing in a conflicted tree and implements otherwise.
#
# It builds its own throwaway repo and deletes it again, so it is safe to run
# during a live drain in this one.
set -euo pipefail

SOURCE_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
KEEP=${KEEP:-}
ISSUES=4
for arg in "$@"; do
  case $arg in
  --keep) KEEP=1 ;;
  [0-9]*) ISSUES=$arg ;;
  *) echo "usage: integrator-stress.sh [--keep] [issues]" >&2; exit 2 ;;
  esac
done

[ -x "$SOURCE_REPO/bin/bd-auto" ] || { echo "build first: make build" >&2; exit 1; }
command -v bd >/dev/null || { echo "integrator-stress: bd is required" >&2; exit 1; }

FIXTURES=
cleanup() { [ -n "$KEEP" ] || { for f in $FIXTURES; do rm -rf "$f"; done; }; }
trap cleanup EXIT

FAILURES=0
pass() { printf '    PASS  %s\n' "$1"; }
fail() { printf '    FAIL  %s\n' "$1"; FAILURES=$((FAILURES + 1)); }
step() { printf '  -- %s\n' "$1"; }

# --- the fixture --------------------------------------------------------------
#
# beads' own git hooks are turned off, as every other fixture here turns them
# off: the post-checkout hook imports .beads/issues.jsonl back over the Dolt
# database, so creating a worktree would revert whatever bd-auto has written and
# the run's own bookkeeping would be the thing under test.
#
# The export is still rewritten, and that is the point -- the worker script does
# it, byte for byte the way beads' pre-commit hook does, and stages it into the
# main checkout's index the way that hook does. What is dropped here is beads
# reverting the database, which is a different fault and not this one.
# scenario <name> <tracked-exports: 0|1> [shape: flat|diamond] [gate-limit]
#
# Whether the repo has ever committed .beads/issues.jsonl decides which way git
# refuses, and the two refusals need different answers. Tracked: bd rewrote a
# file HEAD has, so HEAD's copy goes back. Untracked: the branch is what adds
# the file, and the copy in the way is a re-export standing where the branch's
# version wants to land, with no HEAD copy to restore. Every repo is the second
# one until the first export lands, so both ship here.
scenario() {
  local NAME=$1 TRACKED=$2 SHAPE=${3:-flat} LIMIT=${4:-0}
  printf '\n### %s (exports %s)\n' "$NAME" "$([ "$TRACKED" = 1 ] && echo tracked || echo untracked)"
  local FIXTURE
  FIXTURE=$(mktemp -d "${TMPDIR:-/tmp}/bd-auto-istress.XXXXXX")
  FIXTURES="$FIXTURES $FIXTURE"

step "fixture"
(
  cd "$FIXTURE"
  git init -q -b main .
  git config user.email "istress@bd-auto.invalid"
  git config user.name "bd-auto integrator stress"
  mkdir -p bin
  cp "$SOURCE_REPO/bin/bd-auto" bin/bd-auto
  printf 'base\n' > hot.txt
  printf 'fixture\n' > README.md
  git add -A && git commit -qm "fixture: a file every branch will want"
  bd init --prefix=ist >/dev/null 2>&1
  git config --unset core.hooksPath 2>/dev/null || true
  # The dogfood repo tracks both exports, so the tracked scenario commits an
  # empty one for each: what matters is that HEAD has the path, not what is in
  # it, because every worker rewrites it anyway.
  if [ "$TRACKED" = 1 ]; then
    [ -s .beads/issues.jsonl ] || printf '\n' > .beads/issues.jsonl
    [ -s .beads/interactions.jsonl ] || printf '\n' > .beads/interactions.jsonl
    git add -f .beads/issues.jsonl .beads/interactions.jsonl
  fi
  git add -A >/dev/null 2>&1 || true
  git commit -qm "fixture: beads" >/dev/null 2>&1 || true
)
echo "  $FIXTURE"
cd "$FIXTURE"
BD_AUTO="$FIXTURE/bin/bd-auto"
export PATH="$FIXTURE/bin:$PATH"

# --- the work -----------------------------------------------------------------
#
# Every issue appends to the same line of the same file, so the first merges
# clean and every one after it collides with the result of the last.
step "issues"
EPIC=$(bd create --title="integrator stress epic" --type=epic --silent)
KIDS=
for n in $(seq 1 "$ISSUES"); do
  KIDS="$KIDS $(bd create --title="stress issue $n" --parent="$EPIC" \
    --description="Rewrite hot.txt, commit, close." --silent)"
done
set -- $KIDS
if [ "$SHAPE" = diamond ]; then
  # One issue, then a layer of issues that all conflict with each other, then
  # one more behind all of them: three barriers, and the middle one is the wave
  # that has to reconcile. A chain would give the same number of barriers and
  # nothing to reconcile, because each branch would be cut from the merged tip.
  for k in $(seq 2 $(($# - 1))); do
    eval "mid=\${$k}"
    bd dep add "$mid" "$1" >/dev/null
    eval "last=\${$#}"
    bd dep add "$last" "$mid" >/dev/null
  done
fi
echo "  $EPIC with $ISSUES children ($SHAPE)"

cat > worker.sh <<'WORKER'
#!/usr/bin/env bash
# Worker and integrator in one, told apart by the tree it is standing in.
set -eu
root=$(git rev-parse --show-toplevel)

# The beads export, rewritten and staged in the MAIN checkout, which is what
# beads' pre-commit hook does from inside a worktree: .beads lives there, so
# that is the index it lands in. This is the file that has repeatedly stopped
# the barrier, so the stress run puts it back on every single call.
main=${BD_REPO_ROOT:-$root}
printf '{"rewritten":"%s"}\n' "$(date +%s%N)" > "$main/.beads/issues.jsonl"
printf '{"seq":"%s"}\n' "$(date +%s%N)" > "$main/.beads/interactions.jsonl"
git -C "$main" add .beads/issues.jsonl 2>/dev/null || true

if [ -n "$(git ls-files --unmerged)" ]; then
  # Integrator: keep both sides, drop the markers, stage what was conflicted.
  for f in $(git diff --name-only --diff-filter=U); do
    grep -v '^<<<<<<<\|^=======$\|^>>>>>>>' "$f" > "$f.resolved"
    mv -f "$f.resolved" "$f"
    git add "$f"
  done
  exit 0
fi

# Worker: rewrite the contested file, commit, close.
issue=$(git rev-parse --abbrev-ref HEAD | sed 's|^bd-auto/||')
printf '%s was here\n' "$issue" > hot.txt
printf 'from %s\n' "$issue" > "own-$issue.txt"

# And put a differing export on the BRANCH, so merging it has to write the very
# path the main checkout is holding dirty. That is the shape of the run this
# script exists for: there, the difference arrived when the epic branch absorbed
# main's export; --no-verify is the short way to the same two trees, because
# bd-auto's own pre-commit strips these paths from the index on purpose.
printf '{"branch":"%s"}\n' "$issue" > .beads/issues.jsonl
printf '{"branch":"%s"}\n' "$issue" > .beads/interactions.jsonl

git add -A
git commit --no-verify -qm "$issue: rewrote hot.txt"
bd close "$issue" --reason="integrator stress" >/dev/null
WORKER
chmod +x worker.sh

# A limit counts lines in the contested file, so every branch passes on its own
# and the merged result does not. That is the only way to reach the peel-back:
# a branch that fails its own gate never gets as far as the barrier.
GATE='test -f hot.txt'
[ "$LIMIT" = 0 ] || GATE="test \"\$(grep -c . hot.txt)\" -le $LIMIT"

cat > .beads-auto.yaml <<YAML
gate:
  - name: sanity
    run: $GATE
pipeline:
  - stage: implement
  - stage: gate
runners:
  default:
    provider: fake
    extra_args: ["$FIXTURE/worker.sh"]
concurrency: $ISSUES
autonomy: auto
retry: 0
discovered_work: triage
handoff:
  branch: true
  pr: false
YAML

# --- the drain ----------------------------------------------------------------
step "drain"
set +e
"$BD_AUTO" drain --epic "$EPIC" --all --plain > drain.log 2>&1
DRAIN_RC=$?
set -e
echo "  exit $DRAIN_RC, $(wc -l < drain.log) lines of log"

# --- what it has to have done -------------------------------------------------
step "results"
STATE=.beads/auto/run.json

parked=$(python3 -c "
import json;d=json.load(open('$STATE'));print(len(d.get('parked') or []))" 2>/dev/null || echo "?")
done_n=$(python3 -c "
import json;d=json.load(open('$STATE'));print(len(d.get('done') or []))" 2>/dev/null || echo "?")
status=$(python3 -c "
import json;d=json.load(open('$STATE'));print(d.get('status'))" 2>/dev/null || echo "?")

if [ "$LIMIT" = 0 ]; then
  if [ "$parked" = "0" ]; then pass "nothing parked"; else
    fail "parked $parked"
    python3 -c "
import json
for p in json.load(open('$STATE')).get('parked') or []:
    print('          ', p['id'], '--', p['reason'][:200])" 2>/dev/null || true
  fi
else
  # A gate that only goes red on the merged result has to peel the wave back,
  # and what it peels off is the park. Parking none would mean the gate never
  # ran on the merged tree; parking all of them would mean the peel-back
  # blamed the innocent branches too.
  if [ "$parked" != "0" ] && [ "$parked" != "$ISSUES" ]; then
    pass "the red gate peeled back and parked $parked of $ISSUES"
  else
    fail "parked $parked of $ISSUES on a gate that only the merged result fails"
  fi
  if [ "$(grep -c . hot.txt)" -le "$LIMIT" ]; then
    pass "the gate is green on what was left standing"
  else
    fail "hot.txt has $(grep -c . hot.txt) lines against a limit of $LIMIT"
  fi
fi

if [ "$((done_n + parked))" = "$ISSUES" ]; then
  pass "every issue was accounted for ($done_n done, $parked parked)"
else
  fail "$done_n done and $parked parked, of $ISSUES"
fi

if [ "$LIMIT" = 0 ]; then
  if [ "$status" = "done" ]; then pass "the run finished"; else fail "run status $status"; fi
fi

# Every done branch left a file of its own, and none of the merges dropped one.
missing=""
for id in $(python3 -c "
import json;print(' '.join(json.load(open('$STATE')).get('done') or []))" 2>/dev/null); do
  [ -f "own-$id.txt" ] || missing="$missing own-$id"
  # The contested file is only additive where the branches raced for it. Behind
  # a dependency the later worker is cut from the merged tip and overwrites it,
  # which is the worker's doing and not the barrier's.
  [ "$SHAPE" = flat ] || continue
  grep -q "$id was here" hot.txt || missing="$missing $id"
done
if [ -z "$missing" ]; then pass "every branch that landed is in the tree"; else
  fail "lost:$missing"
  echo "          hot.txt is:"; sed 's/^/            /' hot.txt
fi

# A shape with layers has to reach the barrier more than once.
barriers=$(grep -c "wave [0-9]*: integrating\|integrating .* while the other workers run" drain.log || true)
if [ "$SHAPE" = flat ] || [ "$barriers" -gt 2 ]; then
  pass "$barriers barrier(s) ran"
else
  fail "only $barriers barrier(s) ran for a layered shape"
fi

# The integrator has to have run: with every branch rewriting one line, only the
# first can merge clean. A run that reports no conflict never tested anything.
if grep -q "conflicts in" drain.log; then pass "the barrier hit real conflicts"; else
  fail "no conflict reached the integrator -- the scenario did not stress anything"
fi
if grep -qi "would not merge\|would be overwritten" drain.log; then
  fail "git refused a merge over the checkout"
  grep -i "would not merge\|would be overwritten" drain.log | head -3 | sed 's/^/        /'
else
  pass "no merge was refused over a rewritten export"
fi

  [ -n "$KEEP" ] && echo "    fixture kept at $FIXTURE"
  cd "$SOURCE_REPO"
}

scenario "a wave of conflicts over a rewritten export" 1
scenario "a wave of conflicts over an export no commit has yet" 0
scenario "three barriers, the middle one a wave that all conflicts" 1 diamond
scenario "a gate that only the merged result fails" 1 flat 2

printf '\n'
if [ "$FAILURES" -eq 0 ]; then
  echo "integrator-stress: everything held"
else
  echo "integrator-stress: $FAILURES check(s) failed"
fi
exit $([ "$FAILURES" -eq 0 ] && echo 0 || echo 1)
