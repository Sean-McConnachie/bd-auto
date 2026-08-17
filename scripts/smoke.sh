#!/usr/bin/env bash
# End-to-end smoke test for bd-auto against a throwaway epic.
#
# Unit tests cover the pure logic. This exercises the parts that only fail for
# real: talking to bd, reading the DAG, driving the run state across processes,
# and the scope refusal that stands between a headless launch and a run nobody
# chose. It creates its own issues and branches and deletes them again.
#
# It never spawns a model. Every stage here is a bd-auto command that decides
# something; the drain itself is covered by the drain package's tests, which
# spawn a fake runner.
#
# Usage: scripts/smoke.sh
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BD_AUTO="$REPO/bin/bd-auto"
LABEL="bd-auto-smoke"
FAILURES=0
cd "$REPO" || exit 1

pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; FAILURES=$((FAILURES + 1)); }
step() { printf '\n== %s\n' "$1"; }

check() { # check <description> <expected-substring> <actual>
  if printf '%s' "$3" | grep -q -- "$2"; then pass "$1"; else
    fail "$1 (wanted $2 in: $(printf '%s' "$3" | head -c 300))"
  fi
}

cleanup() {
  step "cleanup"
  "$BD_AUTO" run stop >/dev/null 2>&1
  # Branches are named bd-auto/<issue-id>, and issue IDs start with the epic ID,
  # so glob on the epic rather than on a label that never appears in a ref.
  for b in $(git branch --list "bd-auto/${EPIC:-__none__}*" --format='%(refname:short)' 2>/dev/null); do
    wt=$(git worktree list --porcelain | grep -B2 "branch refs/heads/$b" | head -1 | cut -d' ' -f2)
    [ -n "$wt" ] && git worktree remove --force "$wt" >/dev/null 2>&1
    git branch -D "$b" >/dev/null 2>&1
  done
  for id in $(bd list --label "$LABEL" --all --json 2>/dev/null |
    grep -o '"id": *"[^"]*"' | cut -d'"' -f4); do
    bd delete "$id" --force >/dev/null 2>&1
  done
  rm -rf "$REPO/.beads/auto"
  echo "  cleaned"
}
# Both preflight checks must run BEFORE the trap is armed: bailing out with
# cleanup installed would tear down state this script never created.
[ -x "$BD_AUTO" ] || { echo "build first: make build"; exit 1; }

# Cleanup calls `run stop` and deletes .beads/auto, and run state is shared with
# the main checkout even when this runs from a worktree. Against a live drain
# that silently destroys the run, so refuse rather than clobber.
if "$BD_AUTO" run status 2>/dev/null | grep -q '"active": true'; then
  echo "a bd-auto run is active; smoke would stop it and delete its state."
  echo "finish it, or 'bd-auto run stop', before running smoke."
  exit 1
fi

trap cleanup EXIT

step "fixture: an epic with two independent issues and one dependent"
EPIC=$(bd create --title="smoke epic" --type=epic --labels="$LABEL" --silent) || exit 1
A=$(bd create --title="smoke A" --parent="$EPIC" --labels="$LABEL" --silent)
B=$(bd create --title="smoke B" --parent="$EPIC" --labels="$LABEL" --silent)
C=$(bd create --title="smoke C" --parent="$EPIC" --labels="$LABEL" --silent)
bd dep add "$C" "$A" -q >/dev/null 2>&1
PREFIX="smoke"
echo "  epic=$EPIC a=$A b=$B c=$C"

step "run start"
OUT=$("$BD_AUTO" run start --epic "$EPIC" 2>/dev/null)
check "reports started" '"status": "started"' "$OUT"
check "swarm validate ran" 'Ready Fronts' "$OUT"
[ -f "$REPO/.beads/auto/run.json" ] && pass "run state written" || fail "run state missing"

step "a second run start is refused without --force"
OUT=$("$BD_AUTO" run start --epic "$EPIC" 2>&1)
check "refuses to clobber an active run" 'already active' "$OUT"

step "plan: wave 1 is the ready front, C excluded because it depends on A"
OUT=$("$BD_AUTO" plan 2>/dev/null)
check "A is in the wave" "\"id\": \"$A\"" "$OUT"
check "B is in the wave" "\"id\": \"$B\"" "$OUT"
if printf '%s' "$OUT" | grep -q "\"id\": \"$C\""; then
  fail "C must be blocked by its dependency on A"
else pass "C correctly excluded"; fi
check "not drained" '"drained": false' "$OUT"
check "branch names resolved" "bd-auto/$A" "$OUT"

step "plan --dispatch records the wave"
OUT=$("$BD_AUTO" plan --dispatch 2>/dev/null)
check "dispatched" '"dispatched": true' "$OUT"
check "wave incremented" '"wave": 1' "$OUT"
OUT=$("$BD_AUTO" worker status 2>/dev/null)
check "A is in flight" "$A" "$OUT"

step "planning again returns nothing: in-flight issues are not re-dispatched"
OUT=$("$BD_AUTO" plan 2>/dev/null)
if printf '%s' "$OUT" | grep -q "\"id\": \"$A\""; then
  fail "A was offered twice"
else pass "in-flight issues excluded from a new wave"; fi

step "worker done is refused while the issue is still open"
OUT=$("$BD_AUTO" worker done --issue "$A" 2>&1)
check "refuses to record an unclosed issue as done" 'not closed' "$OUT"

step "worker done accepted once the issue is really closed"
bd close "$A" -q >/dev/null 2>&1
OUT=$("$BD_AUTO" worker done --issue "$A" 2>/dev/null)
check "recorded" '"recorded": "done"' "$OUT"

step "closing A unblocked C, which now appears in the plan"
OUT=$("$BD_AUTO" plan 2>/dev/null)
check "C now ready" "\"id\": \"$C\"" "$OUT"

step "worker fail retries the first time"
OUT=$("$BD_AUTO" worker fail --issue "$B" --stage gate --reason "tests failed" 2>/dev/null)
check "retry, not park" '"recorded": "retry"' "$OUT"
OUT=$(bd show "$B" --json 2>/dev/null)
check "failure recorded on the issue" 'bd-auto attempt' "$OUT"
check "issue returned to open" '"status": "open"' "$OUT"

step "the retry is offered again, as attempt 2, with the failure as context"
OUT=$("$BD_AUTO" plan 2>/dev/null)
check "B offered again" "\"id\": \"$B\"" "$OUT"
check "as attempt 2" '"attempt": 2' "$OUT"
check "carries the previous failure" 'retry_context' "$OUT"

step "second failure parks it (retry: 1)"
"$BD_AUTO" plan --dispatch >/dev/null 2>&1
OUT=$("$BD_AUTO" worker fail --issue "$B" --stage gate --reason "tests failed again" 2>/dev/null)
check "parked" '"recorded": "parked"' "$OUT"
OUT=$(bd show "$B" --json 2>/dev/null)
check "issue blocked" '"status": "blocked"' "$OUT"
check "flagged for a human" '"human"' "$OUT"

step "a parked issue is never offered again"
OUT=$("$BD_AUTO" plan 2>/dev/null)
if printf '%s' "$OUT" | grep -q "\"id\": \"$B\""; then
  fail "parked issue was re-offered"
else pass "parked issue stays out of the run"; fi

step "merge-order reports branches in dependency order"
git switch -c "bd-auto/$C" -q 2>/dev/null
echo "smoke" >smoke-artifact.txt && git add smoke-artifact.txt &&
  git commit -qm "smoke: $C" >/dev/null 2>&1
git switch -q - 2>/dev/null
"$BD_AUTO" plan --dispatch >/dev/null 2>&1
OUT=$("$BD_AUTO" merge-order 2>/dev/null)
check "C's branch is mergeable" "bd-auto/$C" "$OUT"
check "commit counted" '"commits": 1' "$OUT"

step "run status --context is the poll view a launcher reads"
OUT=$("$BD_AUTO" run status --context 2>/dev/null)
check "identifies the run" "$EPIC" "$OUT"
check "reports the counts" 'scope 0 | running' "$OUT"
# Assert the parked line itself, not just the ID: an in-flight issue is named
# too, so a bare "$B" here passes even when nothing is parked. This must stay
# after the unpark steps below, which return B to the run.
check "lists parked work" "parked (needs a human" "$OUT"
check "names the parked issue" "$B" "$OUT"
# The whole point of this view is that it stays small enough to read repeatedly.
LINES=$(printf '%s\n' "$OUT" | wc -l)
[ "$LINES" -le 4 ] && pass "fits in $LINES lines" || fail "poll view grew to $LINES lines"

step "run status --wait returns at once when the run is not active"
"$BD_AUTO" run pause >/dev/null 2>&1
START=$(date +%s)
"$BD_AUTO" run status --context --wait 30s >/dev/null 2>&1
ELAPSED=$(($(date +%s) - START))
[ "$ELAPSED" -le 5 ] && pass "returned in ${ELAPSED}s" || fail "waited ${ELAPSED}s on a paused run"
"$BD_AUTO" run resume >/dev/null 2>&1

step "run unpark puts a parked issue back into the run"
OUT=$("$BD_AUTO" run unpark --issue "$B" --reason "fixed the flaky test" 2>/dev/null)
check "recorded" '"recorded": "unparked"' "$OUT"
OUT=$(bd show "$B" --json 2>/dev/null)
check "issue reopened" '"status": "open"' "$OUT"
if printf '%s' "$OUT" | grep -q '"human"'; then
  fail "the human label must be cleared on unpark"
else pass "human label cleared"; fi
OUT=$("$BD_AUTO" plan 2>/dev/null)
check "offered again" "\"id\": \"$B\"" "$OUT"
check "with a fresh retry budget" '"attempt": 1' "$OUT"
OUT=$("$BD_AUTO" run status --context 2>/dev/null)
if printf '%s' "$OUT" | grep -q "parked (needs a human"; then
  fail "nothing should be parked once B is unparked"
else pass "the parked line is gone"; fi

step "unparking an issue that is not parked is refused"
OUT=$("$BD_AUTO" run unpark --issue "$C" 2>&1)
check "refused" 'not parked' "$OUT"

step "run stop leaves no run behind"
"$BD_AUTO" run stop >/dev/null 2>&1
OUT=$("$BD_AUTO" run status 2>&1)
check "no run recorded" '"active": false' "$OUT"

# --- the headless launch surface ---
#
# These are the checks that stand between a background launch and a run nobody
# chose. Every one of them must hold with no terminal attached, which is exactly
# the condition this script runs under.

step "drain with no scope named and no terminal refuses, and spawns nothing"
OUT=$("$BD_AUTO" drain --epic "$EPIC" --plain 2>&1)
RC=$?
[ "$RC" -ne 0 ] && pass "exit $RC (nothing dispatched)" || fail "expected a non-zero exit, got 0"
check "says nothing was dispatched" 'nothing was dispatched' "$OUT"
check "names the flags that would work" 'Re-run with' "$OUT"
check "shows the candidates to choose from" "$C" "$OUT"
[ -f "$REPO/.beads/auto/run.json" ] && fail "a refused drain must not write run state" ||
  pass "no run state written"

step "drain --dry-run plans the whole scope without spawning anything"
OUT=$("$BD_AUTO" drain --epic "$EPIC" --all --dry-run --plain 2>/dev/null)
check "dry run" '"dry_run": true' "$OUT"
check "nothing dispatched" '"dispatched": false' "$OUT"
check "the scope is the candidate set" "$C" "$OUT"
check "decomposed into waves" '"waves"' "$OUT"
[ -f "$REPO/.beads/auto/run.json" ] && fail "a dry run must not write run state" ||
  pass "still no run state"

# --dry-run as well as a bogus ID: this asserts a refusal, and a step that
# asserts a refusal must not be one typo away from starting a real run.
step "drain --issues rejects an issue that does not exist"
OUT=$("$BD_AUTO" drain --issues "${EPIC}-no-such-issue" --plain --dry-run 2>&1)
RC=$?
[ "$RC" -ne 0 ] && pass "exit $RC" || fail "expected a non-zero exit, got 0"
check "names the issue it could not find" 'no-such-issue' "$OUT"

step "the resolved config is what a drain would use"
OUT=$("$BD_AUTO" config show 2>/dev/null)
check "runner roles resolved" '"worker"' "$OUT"
check "reviewer has its own model" '"reviewer"' "$OUT"
check "the pipeline is resolved" '"builtin-gate"' "$OUT"

step "the plugin surface is a skill and a manifest, and nothing else"
[ -f "$REPO/.claude-plugin/plugin.json" ] && pass "plugin.json present" ||
  fail "plugin.json must survive: it is what makes the skill installable"
if grep -q '"hooks"' "$REPO/.claude-plugin/plugin.json"; then
  fail "plugin.json still points at a hooks file"
else pass "no hooks declared"; fi
[ -d "$REPO/hooks" ] && fail "hooks/ is back" || pass "no hooks/ directory"
[ -d "$REPO/agents" ] && fail "agents/ is back" || pass "no agents/ directory"
[ -f "$REPO/skills/bd-auto/SKILL.md" ] && pass "the launcher skill is there" ||
  fail "skills/bd-auto/SKILL.md is what the plugin is for"
OUT=$("$BD_AUTO" hook stop 2>&1)
RC=$?
[ "$RC" -ne 0 ] && pass "the hook command is gone (exit $RC)" || fail "bd-auto hook still runs"

git rm -q --cached smoke-artifact.txt >/dev/null 2>&1
rm -f smoke-artifact.txt

printf '\n===================================\n'
if [ "$FAILURES" -eq 0 ]; then
  echo "SMOKE PASSED"
else
  echo "SMOKE FAILED: $FAILURES check(s)"
fi
printf '===================================\n'
exit "$FAILURES"
