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
# scenario <name> <tracked-exports: 0|1> [shape: flat|diamond] [gate-limit] [break] [autonomy] [hooks] [issues]
#
# Whether the repo has ever committed .beads/issues.jsonl decides which way git
# refuses, and the two refusals need different answers. Tracked: bd rewrote a
# file HEAD has, so HEAD's copy goes back. Untracked: the branch is what adds
# the file, and the copy in the way is a re-export standing where the branch's
# version wants to land, with no HEAD copy to restore. Every repo is the second
# one until the first export lands, so both ship here.
# build_fixture <dir> -- the repo, the issues and the scripted worker. Both
# scenario() and resume() start from exactly this, so a difference between
# what they observe is a difference in the run and not in the fixture.
# Not indented: the heredocs below need their terminators in column one.
build_fixture() {
FIXTURE=$1
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
  # beads' own hooks import .beads/issues.jsonl back over the Dolt database on
  # post-checkout and post-merge, so a drain that ran git through them would
  # revert every close its workers made -- one `git pull --rebase` once took
  # eight issues from closed back to open. Every git command bd-auto runs goes
  # through internal/gitx, which points core.hooksPath at nowhere, so leaving
  # them installed is a live check that it really does. Most scenarios switch
  # them off so a failure is the barrier's and not beads'.
  [ "$HOOKS" = 1 ] || git config --unset core.hooksPath 2>/dev/null || true
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

# The committed export is a real snapshot of the database, taken now, with every
# issue still open. That is what makes beads' hooks dangerous and what makes the
# scenario that leaves them installed mean anything: a post-checkout or
# post-merge that imports this file puts every close a worker made back to open,
# which is how one `git pull --rebase` once took eight issues from closed back
# to open. A stub file imports as nothing and would prove nothing.
if [ "$TRACKED" = 1 ]; then
  bd export -o .beads/issues.jsonl >/dev/null
  git add -f .beads/issues.jsonl
  git commit --quiet -m "beads: the export, before any of this is done"
fi

# One issue is singled out to misbehave, so what the barrier does to it can be
# told apart from what it does to the branches around it.
eval "BAD=\${$#}"
printf 'BREAK=%s\nBAD=%s\nEPIC=%s\n' "$BREAK" "$BAD" "$EPIC" > stress.env
[ "$BREAK" = none ] || echo "  $BAD is the one that breaks ($BREAK)"

cat > worker.sh <<'WORKER'
#!/usr/bin/env bash
# Worker and integrator in one, told apart by the tree it is standing in.
set -eu

# The main checkout, not this worktree: --git-common-dir is the one .git both
# share, and its parent is where .beads and the run's state actually live.
main=$(dirname "$(git rev-parse --path-format=absolute --git-common-dir)")

BREAK=none; BAD=; EPIC=
. "$(dirname "$0")/stress.env"

# The beads export, rewritten and staged in the MAIN checkout, which is what
# beads' pre-commit hook does from inside a worktree: .beads lives there, so
# that is the index it lands in. This is the file that has repeatedly stopped
# the barrier, so the stress run puts it back on every single call.
printf '{"rewritten":"%s"}\n' "$(date +%s%N)" > "$main/.beads/issues.jsonl"
printf '{"seq":"%s"}\n' "$(date +%s%N)" > "$main/.beads/interactions.jsonl"
git -C "$main" add .beads/issues.jsonl 2>/dev/null || true

if [ -n "$(git ls-files --unmerged)" ]; then
  # Integrator: keep both sides, drop the markers, stage what was conflicted.
  # Unless this is the branch singled out to defeat it, in which case it walks
  # away leaving the markers where they are -- the barrier has to park that one
  # branch and no other.
  # Which branch is being merged, not what is in the file: if the branch it is
  # meant to refuse merged cleanly first, its line is in hot.txt for every
  # conflict after it and matching on the content would refuse them all.
  if [ "$BREAK" = integrator ]; then
    case $(cat "$main/.git/MERGE_MSG" 2>/dev/null) in
    *"bd-auto/$BAD'"*) exit 0 ;;
    esac
  fi
  [ "$BREAK" = slow-integrator ] && sleep 4
  for f in $(git diff --name-only --diff-filter=U); do
    grep -v '^<<<<<<<\|^=======$\|^>>>>>>>' "$f" > "$f.resolved"
    mv -f "$f.resolved" "$f"
    git add "$f"
  done
  exit 0
fi

# Worker: rewrite the contested file, commit, close.
[ "$BREAK" = slow-worker ] && sleep 4
issue=$(git rev-parse --abbrev-ref HEAD | sed 's|^bd-auto/||')

if [ "$BREAK" = no-commit ] && [ "$issue" = "$BAD" ]; then
  # Says it is finished and leaves an empty branch behind. Nothing merged, and
  # the close is not evidence of anything.
  bd close "$issue" --reason="integrator stress" >/dev/null
  exit 0
fi
if [ "$BREAK" = red-gate ] && [ "$issue" = "$BAD" ] && [ ! -f .round-1 ]; then
  # Red on its own gate, once. The round after this one is handed the failure
  # and finds this marker, so the second attempt is the one that passes -- which
  # is the feedback loop doing what it is for, rather than an issue that was
  # never going to work.
  : > .round-1
  rm -f hot.txt
  printf 'from %s\n' "$issue" > "own-$issue.txt"
  git add -A
  git commit --no-verify -qm "$issue: broke the gate"
  # Closed, or the run reads this as a worker that never finished and simply
  # runs it again -- and the gate never sees the tree it was meant to judge.
  bd close "$issue" --reason="integrator stress" >/dev/null
  exit 0
fi
if [ "$BREAK" = discovery ]; then
  # Work found while doing the work, filed under the very epic being drained,
  # while the run is still going. The scope a human approved is a hard allowlist
  # for the run's whole life, so none of these may be picked up -- and the epic
  # they are under may not be closed out from over them.
  bd create --title="found while draining $issue" --type=task --parent="$EPIC" \
    --description="Filed by a worker mid-run." --silent >/dev/null 2>&1 || true
fi
if [ "$BREAK" = dirty-checkout ] && [ "$issue" = "$BAD" ]; then
  # Something in the main checkout that is not a beads export and not anybody's
  # to discard. The barrier has to stop on it rather than park the wave.
  printf 'somebody was editing this\n' > "$main/hot.txt"
fi
printf '%s was here\n' "$issue" > hot.txt
printf 'from %s\n' "$issue" > "own-$issue.txt"

# And put a differing export on the BRANCH, so merging it has to write the very
# path the main checkout is holding dirty. That is the shape of the run this
# script exists for: there, the difference arrived when the epic branch absorbed
# main's export; --no-verify is the short way to the same two trees, because
# bd-auto's own pre-commit strips these paths from the index on purpose.
if [ "$BREAK" = deleted-export ] && [ "$issue" = "$BAD" ]; then
  # This branch says the export should not be in the repo at all. That is a
  # decision somebody made, not a second view of the database, so it has to
  # reach a model rather than being settled by rule like a two-sided rewrite.
  rm -f .beads/issues.jsonl .beads/interactions.jsonl
else
  printf '{"branch":"%s"}\n' "$issue" > .beads/issues.jsonl
  printf '{"branch":"%s"}\n' "$issue" > .beads/interactions.jsonl
fi

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
autonomy: $AUTONOMY
retry: 0
discovered_work: triage
handoff:
  branch: true
  pr: false
YAML

}

scenario() {
  local NAME=$1 TRACKED=$2 SHAPE=${3:-flat} LIMIT=${4:-0} BREAK=${5:-none} AUTONOMY=${6:-auto} HOOKS=${7:-0}
  local ISSUES=${8:-$ISSUES}
  case $NAME in ${ONLY:-*}) ;; *) return ;; esac
  printf '\n### %s (exports %s)\n' "$NAME" "$([ "$TRACKED" = 1 ] && echo tracked || echo untracked)"
  build_fixture "$(mktemp -d "${TMPDIR:-/tmp}/bd-auto-istress.XXXXXX")"
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

WANT_PARKED=0
[ "$BREAK" = integrator ] && WANT_PARKED=1
[ "$BREAK" = no-commit ] && WANT_PARKED=1

if [ "$BREAK" = dirty-checkout ]; then :
elif [ "$LIMIT" = 0 ]; then
  if [ "$parked" = "$WANT_PARKED" ]; then pass "parked $parked, as expected"; else
    fail "parked $parked, expected $WANT_PARKED"
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
  if [ "$BREAK" = dirty-checkout ]; then
    # Stopped, not finished: there is still a wave to merge once the file in
    # the way is somebody's business again.
    if [ "$status" = active ]; then
      pass "the run is left open rather than declared finished"
    else
      fail "run status $status; a barrier that decided nothing has not finished the run"
    fi
  elif [ "$status" = "done" ]; then pass "the run finished"; else fail "run status $status"; fi
fi

# Every done branch left a file of its own, and none of the merges dropped one.
# Nothing merges at all when the barrier stopped on the checkout, and the point
# there is that the branches are untouched and waiting, not that they landed.
missing=""
[ "$BREAK" = dirty-checkout ] && done_ids="" || done_ids=$(python3 -c "
import json;print(' '.join(json.load(open('$STATE')).get('done') or []))" 2>/dev/null)
for id in $done_ids; do
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

if [ "$BREAK" = dirty-checkout ]; then
  # Nothing was merged, so every branch has to still be there for the barrier
  # that runs once somebody has dealt with their own file. Checked before that
  # run, which is what merges them.
  standing=""
  for n in $(seq 1 "$ISSUES"); do
    git rev-parse --verify --quiet "bd-auto/$EPIC.$n" >/dev/null || standing="$standing $EPIC.$n"
  done
  if [ -z "$standing" ]; then
    pass "every branch is still standing for the next barrier"
  else
    fail "branches gone after a barrier that decided nothing:$standing"
  fi

  step "the same command again, once the file is nobody's work in progress"
  git checkout --quiet -- hot.txt
  set +e
  "$BD_AUTO" drain --epic "$EPIC" --all --plain > again.log 2>&1
  set -e
  again=$(python3 -c "
import json;d=json.load(open('$STATE'));print(d.get('status'))" 2>/dev/null || echo "?")
  if [ "$again" = done ]; then pass "the second run finished what the first would not start"; else
    fail "the second run is $again"
    tail -3 again.log | sed 's/^/            /'
  fi
  lost=""
  for n in $(seq 1 "$ISSUES"); do
    [ -f "own-$EPIC.$n.txt" ] || lost="$lost $EPIC.$n"
  done
  if [ -z "$lost" ]; then pass "and every branch landed"; else fail "lost:$lost"; fi
fi

case $BREAK in
red-gate)
  # The gate on the branch is what a round is for. One failure, one round of
  # feedback, and the issue lands: parking it would be the run giving up on the
  # first refusal, and landing it without a second round would be the gate not
  # being read at all.
  if grep -q "$BAD.*round 1" drain.log; then
    pass "$BAD was given a second round after its gate went red"
  else
    fail "no second round after a red gate"
    grep "$BAD" drain.log | grep -i "gate\|round" | head -3 | sed 's/^/            /'
  fi
  if [ -f "own-$BAD.txt" ]; then
    pass "and it landed on the round that passed"
  else
    fail "$BAD never landed"
  fi
  ;;
discovery)
  filed=$(bd list --status=open 2>/dev/null | grep -c "found while draining" || true)
  if [ "$filed" -ge 1 ]; then
    pass "$filed issue(s) filed under the epic mid-run are still open"
  else
    fail "the issues the workers filed mid-run are not open in bd"
  fi
  # The scope is the list a human approved, and it does not grow because the
  # epic did. A run that swept these up would be spending on work nobody agreed
  # to, found by the very models it is paying for.
  in_scope=$(python3 -c "
import json;d=json.load(open('$STATE'));print(len(d.get('scope') or []))" 2>/dev/null || echo "?")
  if [ "$in_scope" = "$ISSUES" ]; then
    pass "the run's scope is still the $ISSUES issues it was given"
  else
    fail "scope is now $in_scope issues; the run picked up work it was not given"
  fi
  if grep -q "found while draining" drain.log; then
    fail "the run touched an issue filed after it started"
  else
    pass "and it never touched one of them"
  fi
  ;;
no-commit)
  # An empty branch is not work. It parks, and it parks alone.
  if python3 -c "
import json,sys
p=[x['id'] for x in (json.load(open('$STATE')).get('parked') or [])]
sys.exit(0 if p == ['$BAD'] else 1)" 2>/dev/null; then
    pass "$BAD parked alone for having committed nothing"
  else
    fail "an empty branch did not park by itself"
  fi
  ;;
dirty-checkout)
  # git refuses before it conflicts anything, and every branch queued behind
  # this one would meet the same file. So the barrier stops once instead of
  # working through the wave parking each in turn -- which is how one rewritten
  # export once turned five reviewed, gated branches into five parks in six
  # seconds. Parking anything here would be that bug again.
  if grep -q "the checkout has uncommitted changes to hot.txt" drain.log; then
    pass "the barrier named the file it could not merge over"
  else
    fail "the barrier did not stop on the dirty checkout"
  fi
  if [ "$parked" = 0 ]; then
    pass "and parked nothing, because nothing was decided about any branch"
  else
    fail "parked $parked over a dirty checkout that says nothing about any branch"
  fi
  ;;
integrator)
  # An integrator that walks away from the markers costs its own branch and
  # nothing else. Parking the wave around it would be the peel-back blaming
  # branches that were already merged and gated.
  if python3 -c "
import json,sys
p=[x['id'] for x in (json.load(open('$STATE')).get('parked') or [])]
sys.exit(0 if p == ['$BAD'] else 1)" 2>/dev/null; then
    pass "$BAD parked alone"
  else
    fail "the branch the integrator refused is not the only one parked"
  fi
  ;;
deleted-export)
  # A branch that deletes the export disagrees about whether the file belongs
  # in the repo. Settling that by rule would undo the deletion without anyone
  # deciding anything, so it has to reach a model like any other disagreement.
  # Merged last, so the branches that rewrote the export are already on the
  # epic branch and the deletion meets one of them. That ordering is why this
  # scenario runs under wave scheduling: continuous merges a lane the moment it
  # lands, and a deletion that happens to go first meets nothing.
  if grep -q "$BAD:.*conflicts in.*a model is resolving them" drain.log; then
    pass "the deleted export reached a model"
  else
    fail "the deleted export never reached a model"
    grep "$BAD:" drain.log | head -3 | sed 's/^/            /'
  fi
  if grep -q "$BAD: merged .*every conflict was a beads export, so no model ran" drain.log; then
    fail "the deletion was settled by rule and silently undone"
  else
    pass "the deletion was not settled by rule"
  fi
  ;;
esac

# With beads' hooks installed, every worktree bd-auto creates and every merge it
# makes is a chance for one to import the export over the database. What the
# drain closed has to still be closed when it is over.
if [ "$HOOKS" = 1 ]; then
  reverted=""
  for id in $(python3 -c "
import json;print(' '.join(json.load(open('$STATE')).get('done') or []))" 2>/dev/null); do
    # Captured rather than piped: grep -q closes the pipe on its first match,
    # and under pipefail bd's SIGPIPE would read as the issue being open.
    shown=$(bd show "$id" 2>/dev/null || true)
    case $shown in
    *CLOSED*) ;;
    *) reverted="$reverted $id" ;;
    esac
  done
  if [ -z "$reverted" ]; then
    pass "every close survived beads' hooks"
  else
    fail "reopened by a hook:$reverted"
  fi

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
if [ "$BREAK" = dirty-checkout ]; then :
elif grep -q "conflicts in" drain.log; then pass "the barrier hit real conflicts"; else
  fail "no conflict reached the integrator -- the scenario did not stress anything"
fi
if grep -qi "would not merge\|would be overwritten" drain.log; then
  fail "git refused a merge over the checkout"
  grep -i "would not merge\|would be overwritten" drain.log | head -3 | sed 's/^/        /'
else
  pass "no merge was refused over a rewritten export"
fi

# Last, because it stashes the run's own log out of the way and every check
# above reads it.
if [ "$HOOKS" = 1 ]; then
  # The control, because a check that cannot fail is not a check. The same
  # repo, one plain `git checkout main` -- not through gitx, so beads' hooks
  # run -- and main's export is the snapshot taken before any of this was done.
  # Every close has to come back open. If it does not, beads stopped importing
  # the export over the database and the survival above proved nothing.
  git stash --include-untracked --quiet >/dev/null 2>&1 || true
  git checkout --quiet main >/dev/null 2>&1 || true
  survived=""
  for id in $(python3 -c "
import json;print(' '.join(json.load(open('$STATE')).get('done') or []))" 2>/dev/null); do
    shown=$(bd show "$id" 2>/dev/null || true)
    case $shown in
    *CLOSED*) survived="$survived $id" ;;
    esac
  done
  if [ -z "$survived" ]; then
    pass "and a plain checkout does revert them, so that check has teeth"
  else
    fail "beads' hooks no longer revert a close:$survived — the check above is inert"
  fi
fi

  [ -n "$KEEP" ] && echo "    fixture kept at $FIXTURE"
  cd "$SOURCE_REPO"
}

# resume <name>
#
# The claim the whole design rests on: the control flow is a Go process and the
# state is on disk, so a run that is killed is a run that can be started again.
# This kills it in the worst place there is -- inside the barrier, while the
# integrator is resolving a conflict -- which leaves the checkout mid-merge with
# MERGE_HEAD set and index entries at three stages, and a worktree per issue
# still on disk.
resume() {
  local NAME=$1 WATCH_FOR=$2 WHERE=$3 BREAK=${4:-slow-integrator}
  case $NAME in ${ONLY:-*}) ;; *) return ;; esac
  printf '\n### %s\n' "$NAME"
  TRACKED=1 SHAPE=flat LIMIT=0 AUTONOMY=auto HOOKS=0 \
    build_fixture "$(mktemp -d "${TMPDIR:-/tmp}/bd-auto-istress.XXXXXX")"

  step "the run, killed $WHERE"
  setsid "$FIXTURE/bin/bd-auto" drain --epic "$EPIC" --all --plain > drain.log 2>&1 &
  local pid=$! waited=0
  while [ "$waited" -lt 240 ]; do
    grep -q "$WATCH_FOR" drain.log 2>/dev/null && break
    sleep 0.25
    waited=$((waited + 1))
  done
  if ! grep -q "$WATCH_FOR" drain.log 2>/dev/null; then
    fail "the run never got $WHERE, so there was nothing to interrupt"
    kill -9 -"$pid" 2>/dev/null || true
    cd "$SOURCE_REPO"
    return
  fi
  kill -9 -"$pid" 2>/dev/null || true
  pkill -9 -f "$FIXTURE/bin/bd-auto" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  if [ -f .git/MERGE_HEAD ]; then
    pass "the checkout was left mid-merge, which is the state worth resuming from"
  elif [ "$WHERE" = "inside the barrier" ]; then
    echo "    note: the kill landed just outside the merge itself"
  else
    pass "killed with worktrees on disk and nothing merged"
  fi

  step "the run, started again"
  set +e
  "$FIXTURE/bin/bd-auto" drain --epic "$EPIC" --all --plain > resume.log 2>&1
  local rc=$?
  set -e
  echo "  exit $rc, $(wc -l < resume.log) lines"

  local STATE=.beads/auto/run.json
  local done_n parked status
  done_n=$(python3 -c "
import json;d=json.load(open('$STATE'));print(len(d.get('done') or []))" 2>/dev/null || echo "?")
  parked=$(python3 -c "
import json;d=json.load(open('$STATE'));print(len(d.get('parked') or []))" 2>/dev/null || echo "?")
  status=$(python3 -c "
import json;d=json.load(open('$STATE'));print(d.get('status'))" 2>/dev/null || echo "?")

  if [ "$status" = done ]; then pass "the second run finished"; else fail "run status $status"; fi
  if [ "$((done_n + parked))" = "$ISSUES" ]; then
    pass "every issue was accounted for ($done_n done, $parked parked)"
  else
    fail "$done_n done and $parked parked, of $ISSUES"
  fi
  local missing=""
  for id in $(python3 -c "
import json;print(' '.join(json.load(open('$STATE')).get('done') or []))" 2>/dev/null); do
    [ -f "own-$id.txt" ] || missing="$missing own-$id"
  done
  if [ -z "$missing" ]; then pass "every branch that landed is in the tree"; else fail "lost:$missing"; fi
  if [ -f .git/MERGE_HEAD ]; then
    fail "the checkout is still mid-merge after the second run"
  else
    pass "the half-finished merge was cleared, not inherited"
  fi

  [ -n "$KEEP" ] && echo "    fixture kept at $FIXTURE"
  cd "$SOURCE_REPO"
}

# second_drain <name>
#
# Two drains in one repo would share a run.json, a set of worktrees and a
# checkout, and the second would be writing the first's state from underneath
# it. The second has to refuse, and refusing has to cost the first nothing.
second_drain() {
  local NAME=$1
  case $NAME in ${ONLY:-*}) ;; *) return ;; esac
  printf '\n### %s\n' "$NAME"
  TRACKED=1 SHAPE=flat LIMIT=0 BREAK=slow-worker AUTONOMY=auto HOOKS=0 \
    build_fixture "$(mktemp -d "${TMPDIR:-/tmp}/bd-auto-istress.XXXXXX")"

  step "one drain running, another started on top of it"
  setsid "$FIXTURE/bin/bd-auto" drain --epic "$EPIC" --all --plain > drain.log 2>&1 &
  local pid=$! waited=0
  while [ "$waited" -lt 240 ]; do
    grep -q "\[worker\] started" drain.log 2>/dev/null && break
    sleep 0.25
    waited=$((waited + 1))
  done

  set +e
  "$FIXTURE/bin/bd-auto" drain --epic "$EPIC" --all --plain > second.log 2>&1
  local rc=$?
  set -e
  if [ "$rc" != 0 ]; then
    pass "the second drain refused (exit $rc)"
  else
    fail "a second drain ran against a live one"
  fi
  said=$(cat second.log)
  case $said in
  *"in progress"*|*already*|*"another run"*)
    pass "and said a run was already in progress" ;;
  *)
    fail "it refused, but not for being a second drain"
    printf '%s\n' "$said" | sed -n '1,4p' | sed 's/^/            /' ;;
  esac
  # And it must refuse before it spends anything: a second drain that dispatches
  # workers has already made worktrees and branches against a live run.
  case $said in
  *dispatching*|*"run start"*)
    fail "the second drain got as far as dispatching work" ;;
  *)
    pass "and refused before it dispatched anything" ;;
  esac

  step "the first, left alone to finish"
  wait "$pid" 2>/dev/null || true
  local STATE=.beads/auto/run.json
  local status done_n
  status=$(python3 -c "
import json;print(json.load(open('$STATE')).get('status'))" 2>/dev/null || echo "?")
  done_n=$(python3 -c "
import json;print(len(json.load(open('$STATE')).get('done') or []))" 2>/dev/null || echo "?")
  if [ "$status" = done ] && [ "$done_n" = "$ISSUES" ]; then
    pass "the first run finished all $ISSUES, untouched by the refusal"
  else
    fail "the first run is $status with $done_n done"
  fi
  local missing=""
  for n in $(seq 1 "$ISSUES"); do
    [ -f "own-$EPIC.$n.txt" ] || missing="$missing $EPIC.$n"
  done
  if [ -z "$missing" ]; then pass "and every branch landed"; else fail "lost:$missing"; fi

  [ -n "$KEEP" ] && echo "    fixture kept at $FIXTURE"
  cd "$SOURCE_REPO"
}

# count_own -- how many branches' files are in the working tree. A glob rather
# than ls piped into wc, because a glob that matches nothing is not an error and
# a pipeline under pipefail is.
count_own() {
  local n=0 f
  for f in own-*.txt; do
    [ -e "$f" ] && n=$((n + 1))
  done
  printf '%s\n' "$n"
}

# moved_checkout <name>
#
# The run that started all this. A worker branches from the main checkout's
# HEAD, so a run whose checkout moved off its epic branch between waves would
# cut its next wave from the wrong place and silently drop everything already
# merged -- and merging those branches back would drag in whatever the checkout
# had wandered onto. Here the checkout is moved to main, and main is advanced
# underneath it, between one barrier and the next.
moved_checkout() {
  local NAME=$1
  case $NAME in ${ONLY:-*}) ;; *) return ;; esac
  printf '\n### %s\n' "$NAME"
  # Continuous, not waves: under waves the run holds at each barrier for a
  # human, and this scenario is about what happens between two of them.
  TRACKED=1 SHAPE=diamond LIMIT=0 BREAK=slow-worker AUTONOMY=auto HOOKS=0 \
    build_fixture "$(mktemp -d "${TMPDIR:-/tmp}/bd-auto-istress.XXXXXX")"

  step "one barrier, then killed"
  setsid "$FIXTURE/bin/bd-auto" drain --epic "$EPIC" --all --plain > drain.log 2>&1 &
  local pid=$! waited=0
  while [ "$waited" -lt 240 ]; do
    grep -q "integrated .*merged" drain.log 2>/dev/null && break
    sleep 0.25
    waited=$((waited + 1))
  done
  kill -9 -"$pid" 2>/dev/null || true
  pkill -9 -f "$FIXTURE/bin/bd-auto" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  if ! grep -q "integrated" drain.log; then
    fail "nothing was merged before the kill; the scenario tests nothing"
    cd "$SOURCE_REPO"; return
  fi
  local killed_at
  killed_at=$(python3 -c "
import json;print(json.load(open('.beads/auto/run.json')).get('status'))" 2>/dev/null || echo "?")
  if [ "$killed_at" = active ]; then
    pass "a barrier ran and the run was killed still going"
  else
    fail "the run was $killed_at by the time it was killed, so nothing was interrupted"
    cd "$SOURCE_REPO"; return
  fi
  local landed
  landed=$(count_own)

  step "the checkout moved to main, and main moved on"
  git checkout --quiet -- . 2>/dev/null || true
  git checkout --quiet main
  printf 'main moved on\n' > elsewhere.txt
  git add elsewhere.txt
  git commit --quiet -m "main: something that was not in this run"

  step "the run, started again from the wrong branch"
  set +e
  timeout 300 "$FIXTURE/bin/bd-auto" drain --epic "$EPIC" --all --plain > again.log 2>&1
  local rc=$?
  set -e
  [ "$rc" = 124 ] && fail "the resumed run hung and was killed after five minutes"

  local status
  status=$(python3 -c "
import json;print(json.load(open('.beads/auto/run.json')).get('status'))" 2>/dev/null || echo "?")
  if [ "$status" = done ]; then pass "the resumed run finished"; else fail "run status $status"; fi

  # On the branch the run stages on, wherever the checkout happens to have been
  # left: the question is what landed, not where somebody is standing.
  local staged
  staged=$(python3 -c "
import json;print(json.load(open('.beads/auto/run.json')).get('epic_branch') or '')" 2>/dev/null || true)
  if [ -n "$staged" ]; then
    git checkout --quiet -- . 2>/dev/null || true
    git checkout --quiet "$staged" 2>/dev/null || fail "the epic branch $staged is gone"
  fi

  # Everything that had already been merged is still merged. A run that cut its
  # next wave from main would have left those behind on the epic branch nobody
  # went back to.
  local now
  now=$(count_own)
  if [ "$now" -ge "$landed" ]; then
    pass "the $landed file(s) merged before the kill are still here ($now now)"
  else
    fail "$landed file(s) were merged and only $now survived the move"
  fi
  local missing=""
  for n in $(seq 1 "$ISSUES"); do
    [ -f "own-$EPIC.$n.txt" ] || missing="$missing $EPIC.$n"
  done
  if [ -z "$missing" ]; then pass "and every issue's work is in the tree"; else fail "missing:$missing"; fi

  [ -n "$KEEP" ] && echo "    fixture kept at $FIXTURE"
  cd "$SOURCE_REPO"
}

scenario "a wave of conflicts over a rewritten export" 1
scenario "a wave of conflicts over an export no commit has yet" 0
scenario "three barriers, the middle one a wave that all conflicts" 1 diamond
scenario "a gate that only the merged result fails" 1 flat 2
# Under waves, so the branch it refuses is merged last and therefore certain to
# conflict. Merged first it would go in cleanly, the integrator would never run
# for it, and the scenario would quietly test nothing.
scenario "an integrator that walks away from the markers" 1 flat 0 integrator wave
scenario "a branch that deletes the export the others rewrote" 1 flat 0 deleted-export wave
scenario "the same wave of conflicts, scheduled in waves" 1 flat 0 none wave
scenario "the same wave again, with beads' own hooks installed" 1 flat 0 none auto 1
resume "killed inside the barrier, then started again" "a model is resolving them" "inside the barrier"
resume "killed with its workers running, then started again" "\[worker\] started" "with its workers running" slow-worker

# Ten issues at once, all writing the same line of the same file. Every merge
# after the first conflicts, ten workers write the beads export underneath the
# barrier, and every mutation of run.json goes through one flock.
scenario "ten at once, all of them contending" 1 flat 0 none auto 0 10
scenario "a worker that says it is done and commits nothing" 1 flat 0 no-commit wave
scenario "a checkout dirtied with something nobody may discard" 1 flat 0 dirty-checkout wave
scenario "every worker filing new work while the barrier runs" 1 flat 0 discovery
scenario "a branch that goes red on its own gate, once" 1 flat 0 red-gate
second_drain "a second drain started on top of a live one"
moved_checkout "the checkout moved to main between two barriers"

printf '\n'
if [ "$FAILURES" -eq 0 ]; then
  echo "integrator-stress: everything held"
else
  echo "integrator-stress: $FAILURES check(s) failed"
fi
exit $([ "$FAILURES" -eq 0 ] && echo 0 || echo 1)
