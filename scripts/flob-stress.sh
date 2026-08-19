#!/usr/bin/env bash
# flob-stress.sh — prove a bd-auto drain cannot lose a flob record.
#
# beads-auto-imp-3k2. flob (~/dev/full-observability) is a second Dolt-backed
# tool that travels on the git remote under refs/dolt/flob, beside beads' own
# refs/dolt/data. A drain runs N worker worktrees off one checkout and merges
# them at a barrier, so every hazard beads has hit applies to flob too — over
# data that is far more expensive to lose, because a flob record is the only
# artefact of the training time that produced it.
#
# WHAT THIS RUNS AGAINST
#
# Its own throwaway repo and its own store, built here and deleted at the end.
# It never touches ~/dev/full-observability or any other real store, and it
# checks that rather than trusting it: see guard_store below, which resolves
# where flob would actually write and refuses to run if that is anywhere but
# this fixture. A record in a real store cannot be reproduced without paying
# for the training that produced it again.
#
# WHAT IT PROVES
#
#  1. Store resolution. From every worker worktree, flob resolves its store to
#     the MAIN checkout, and its project root to the worktree. That pairing is
#     the whole safety property: records outlive the worktree, provenance does
#     not lie about which tree ran.
#  2. Nothing on a branch. No worker branch carries a byte of the store, so a
#     barrier merge has nothing to resolve and cannot drop a side.
#  3. Concurrency. N workers recording at once all land, and the count is exact.
#  4. The barrier. Every record survives merging every worker branch.
#  5. Teardown. Removing a worktree that just wrote loses nothing.
#  6. A kill mid-write costs at most the killed worker's own run.
#
# Loss is reported as which records went missing, never only as a count.

set -uo pipefail

WORKERS=${WORKERS:-5}
RUNS_PER_WORKER=${RUNS_PER_WORKER:-4}
FLOB_PY=${FLOB_PY:-$HOME/.local/share/uv/tools/flob/bin/python}
FLOB=${FLOB:-$HOME/.local/bin/flob}

fail() { printf '\033[31mFAIL\033[0m %s\n' "$*" >&2; FAILED=$((FAILED+1)); }
pass() { printf '\033[32mok\033[0m   %s\n' "$*"; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
die()  { printf '\033[31mfatal\033[0m %s\n' "$*" >&2; exit 2; }
FAILED=0

[ -x "$FLOB_PY" ] || die "no flob interpreter at $FLOB_PY (set FLOB_PY)"
[ -x "$FLOB" ] || die "no flob CLI at $FLOB (set FLOB)"
"$FLOB_PY" -c 'import flob' 2>/dev/null || die "$FLOB_PY cannot import flob"

# --- the fixture -------------------------------------------------------------

FIXTURE=$(mktemp -d "${TMPDIR:-/tmp}/flob-stress.XXXXXX") || die "mktemp"
REPO="$FIXTURE/main"
cleanup() { [ -n "${KEEP:-}" ] || rm -rf "$FIXTURE"; }
trap cleanup EXIT

step "fixture at $FIXTURE"
mkdir -p "$REPO"
git -C "$REPO" init -q
git -C "$REPO" config user.email flob-stress@example.invalid
git -C "$REPO" config user.name  "flob stress"
# A bare remote, so push/pull have somewhere to go that is not anybody's github.
git init -q --bare "$FIXTURE/remote.git"
# file:// rather than a bare path: Dolt resolves the remote itself and refuses a
# URL with no scheme ("unsupported git dbfactory scheme"), where git is happy
# with either.
git -C "$REPO" remote add origin "file://$FIXTURE/remote.git"

printf 'seed\n' > "$REPO/README.md"
git -C "$REPO" add -A
git -C "$REPO" commit -qm "seed"

( cd "$REPO" && "$FLOB" init --git >/dev/null 2>&1 ) \
  || die "flob init failed in the fixture"
[ -d "$REPO/.flob" ] || die "flob init left no .flob"
git -C "$REPO" add -A
git -C "$REPO" commit -qm "flob init" >/dev/null

# --- the guard ---------------------------------------------------------------
#
# Resolve where flob would actually write, from the fixture and from each
# worktree, and refuse to go on unless every answer is inside the fixture. This
# is the check that makes the script safe to run on a machine that has real
# records on it.

guard_store() {
  local from=$1
  local resolved
  resolved=$("$FLOB_PY" - "$from" <<'PY'
import sys
from pathlib import Path
from flob.project import find_root, shared_store, FLOB_DIR
root = find_root(Path(sys.argv[1]))
print(shared_store(root) or (root / FLOB_DIR))
PY
) || die "could not resolve the flob store from $from"
  case "$resolved" in
    "$FIXTURE"/*) ;;
    *) die "REFUSING TO RUN: from $from flob resolves its store to $resolved, which is outside the fixture $FIXTURE" ;;
  esac
  printf '%s' "$resolved"
}

step "safety guard"
STORE=$(guard_store "$REPO")
pass "the store resolves inside the fixture: $STORE"
case "$STORE" in
  "$HOME"/dev/*) die "REFUSING TO RUN: the store is under a development directory" ;;
esac
pass "the store is not under any development directory"

# --- worktrees, in bd-auto's layout -----------------------------------------

step "worktrees"
mkdir -p "$REPO/.beads/auto/wt"
for i in $(seq 1 "$WORKERS"); do
  git -C "$REPO" worktree add -q -b "bd-auto/t-$i" "$REPO/.beads/auto/wt/t-$i" >/dev/null 2>&1 \
    || die "could not create worktree t-$i"
done
pass "$WORKERS worktrees under .beads/auto/wt, the layout a drain uses"

# 1. store resolution, per worktree
step "1. every worker resolves the main checkout's store, and its own root"
BEFORE_STEP=$FAILED
for i in $(seq 1 "$WORKERS"); do
  wt="$REPO/.beads/auto/wt/t-$i"
  got=$(guard_store "$wt")
  if [ "$got" != "$REPO/.flob" ]; then
    fail "t-$i writes to $got, not the main checkout's $REPO/.flob — its records die with the worktree"
  fi
  root=$("$FLOB_PY" - "$wt" <<'PY'
import sys
from pathlib import Path
from flob.project import find_root
print(find_root(Path(sys.argv[1])))
PY
)
  if [ "$root" != "$wt" ]; then
    fail "t-$i resolves its project root to $root, not its own worktree — provenance would name the wrong tree"
  fi
done
[ "$FAILED" -eq "$BEFORE_STEP" ] && pass "all $WORKERS share the main store and keep their own root"

# --- writing, concurrently ---------------------------------------------------

record_one() { # worktree, experiment, tag
  "$FLOB_PY" - "$1" "$2" "$3" <<'PY'
import sys
from pathlib import Path
from pydantic import BaseModel
import flob
from flob.project import find_project

class Marker(BaseModel):
    """A result has to carry a schema, so the stress record declares one."""
    marker: str
    worker: str

wt, experiment, tag = sys.argv[1], sys.argv[2], sys.argv[3]
project = find_project(Path(wt))
with flob.run(experiment, project=project, tags=[tag], allow_dirty=True,
              result_schema=Marker) as r:
    r.log(Marker(marker=tag, worker=Path(wt).name))
print(r.uid)
PY
}

step "2. $WORKERS workers record $RUNS_PER_WORKER runs each, concurrently"
EXPECTED="$FIXTURE/expected.txt"
: > "$EXPECTED"
for i in $(seq 1 "$WORKERS"); do
  (
    for j in $(seq 1 "$RUNS_PER_WORKER"); do
      tag="t-$i-run-$j"
      if uid=$(record_one "$REPO/.beads/auto/wt/t-$i" "stress" "$tag" 2>"$FIXTURE/err-$i-$j.log" | tail -1); then
        printf '%s\t%s\n' "$tag" "$uid" >> "$EXPECTED"
      else
        printf '%s\tWRITE-FAILED\n' "$tag" >> "$EXPECTED"
      fi
    done
  ) &
done
wait
WROTE=$(grep -c . "$EXPECTED" 2>/dev/null || echo 0)
WANT=$((WORKERS * RUNS_PER_WORKER))
pass "attempted $WANT writes from $WORKERS concurrent workers, recorded $WROTE outcomes"
if grep -q "WRITE-FAILED" "$EXPECTED"; then
  fail "$(grep -c WRITE-FAILED "$EXPECTED") write(s) failed outright:"
  grep WRITE-FAILED "$EXPECTED" | sed 's/^/       /' >&2
  sed -n '1,12p' "$FIXTURE"/err-*.log 2>/dev/null | sed 's/^/       /' >&2
fi

# readable is the set of tags the store can actually return, read through the
# CLI's own JSON rather than through the library internals: this is the
# interface a human would use to find out whether a record survived, so it is
# the one the test should fail on.
readable() {
  ( cd "$REPO" && "$FLOB" ls --json -n 0 2>/dev/null ) | "$FLOB_PY" -c '
import json, sys
raw = sys.stdin.read().strip()
if raw:
    try:
        rows = json.loads(raw)
    except json.JSONDecodeError:
        rows = []
    if isinstance(rows, dict):
        rows = rows.get("runs", rows.get("data", []))
    for row in rows or []:
        for tag in (row.get("tags") or []):
            print(tag)
'
}

# missing reports which tags are gone, by name, never only a count.
check_all_present() { # label
  local label=$1 got missing
  got="$FIXTURE/got.txt"
  readable 2>/dev/null | sort -u > "$got"
  missing=$(comm -23 <(grep -v "WRITE-FAILED" "$EXPECTED" | cut -f1 | sort -u) "$got")
  if [ -n "$missing" ]; then
    fail "$label: these records are no longer readable:"
    printf '%s\n' "$missing" | sed 's/^/       /' >&2
    return 1
  fi
  pass "$label: every record written is readable ($(grep -c . "$got") tags)"
}

step "3. every record is readable straight after the concurrent writes"
check_all_present "after concurrent writes"

# 3b. exact count, not just presence
step "4. the count is exact, with no phantom or duplicated rows"
COUNT=$(readable 2>/dev/null | grep -c '^t-' || echo 0)
SUCCEEDED=$(grep -vc WRITE-FAILED "$EXPECTED" 2>/dev/null || echo 0)
if [ "$COUNT" != "$SUCCEEDED" ]; then
  fail "the store holds $COUNT stress tags, $SUCCEEDED were written successfully"
else
  pass "$COUNT records in, $COUNT records out"
fi

# --- nothing on a branch -----------------------------------------------------

step "5. no worker branch carries the store"
BEFORE_STEP=$FAILED
for i in $(seq 1 "$WORKERS"); do
  wt="$REPO/.beads/auto/wt/t-$i"
  dirty=$(git -C "$wt" status --porcelain -- .flob 2>/dev/null)
  if [ -n "$dirty" ]; then
    fail "t-$i has uncommitted .flob paths, which a 'git add -A' would sweep onto its branch:"
    printf '%s\n' "$dirty" | sed 's/^/       /' >&2
  fi
  git -C "$wt" add -A >/dev/null 2>&1
  if git -C "$wt" diff --cached --name-only | grep -q '^\.flob/dolt'; then
    fail "t-$i staged the store itself; a barrier merge would have to resolve it"
  fi
  git -C "$wt" reset -q >/dev/null 2>&1
done
[ "$FAILED" -eq "$BEFORE_STEP" ] && pass "the store is gitignored and invisible to every worker branch"

# --- the barrier -------------------------------------------------------------

step "6. the barrier merges every worker branch"
for i in $(seq 1 "$WORKERS"); do
  wt="$REPO/.beads/auto/wt/t-$i"
  printf 'worker %s\n' "$i" > "$wt/worker-$i.txt"
  git -C "$wt" add -A >/dev/null 2>&1
  git -C "$wt" commit -qm "t-$i: work" >/dev/null 2>&1
done
git -C "$REPO" checkout -q master 2>/dev/null || git -C "$REPO" checkout -q main
for i in $(seq 1 "$WORKERS"); do
  if ! git -C "$REPO" merge --no-edit -q "bd-auto/t-$i" >/dev/null 2>&1; then
    fail "the barrier could not merge bd-auto/t-$i"
    git -C "$REPO" merge --abort >/dev/null 2>&1
  fi
done
check_all_present "after the barrier merged all $WORKERS branches"

# --- teardown ----------------------------------------------------------------

step "7. tearing a worktree down loses nothing"
git -C "$REPO" worktree remove --force "$REPO/.beads/auto/wt/t-1" >/dev/null 2>&1 \
  || fail "could not remove t-1's worktree"
check_all_present "after t-1's worktree was removed"

# --- a kill mid-write --------------------------------------------------------

step "8. a worker killed mid-write costs at most its own run"
BEFORE=$(readable 2>/dev/null | sort -u | wc -l)
record_one "$REPO/.beads/auto/wt/t-2" "stress" "t-2-killed" >/dev/null 2>&1 &
KILLPID=$!
( sleep 0.35; kill -9 "$KILLPID" 2>/dev/null ) &
wait "$KILLPID" 2>/dev/null
sleep 0.5
check_all_present "after a worker was killed mid-write"
AFTER=$(readable 2>/dev/null | sort -u | wc -l)
if [ "$AFTER" -lt "$BEFORE" ]; then
  fail "the killed worker took $((BEFORE - AFTER)) other record(s) with it"
else
  pass "the store still holds every record it held before the kill"
fi

# --- the ref -----------------------------------------------------------------

step "9. the store reaches the remote under refs/dolt/flob"
# The git branch goes first. Dolt refuses to push into a remote with no branches
# at all, which is what a bare repo nobody has pushed to looks like.
git -C "$REPO" push -q origin HEAD >/dev/null 2>&1 || true
if ( cd "$REPO" && "$FLOB" push >/dev/null 2>"$FIXTURE/push.log" ); then
  if git -C "$FIXTURE/remote.git" show-ref | grep -q 'refs/dolt/flob'; then
    pass "refs/dolt/flob exists on the remote, beside beads' refs/dolt/data"
  else
    fail "flob push reported success but the remote has no refs/dolt/flob"
    git -C "$FIXTURE/remote.git" show-ref | sed 's/^/       /' >&2
  fi
else
  fail "flob push failed:"
  sed -n '1,10p' "$FIXTURE/push.log" | sed 's/^/       /' >&2
fi

# --- verdict -----------------------------------------------------------------

# The store's own server, if one was started, dies with the fixture. omc is the
# issue about it leaking; this makes sure this script is not another leak.
( cd "$REPO" && "$FLOB" servers --stop >/dev/null 2>&1 ) || true
pkill -f "dolt sql-server.*$FIXTURE" 2>/dev/null || true

echo
if [ "$FAILED" -eq 0 ]; then
  printf '\033[32mflob survived a %s-worker drain: no record lost.\033[0m\n' "$WORKERS"
  exit 0
fi
printf '\033[31m%s check(s) failed.\033[0m\n' "$FAILED"
exit 1
