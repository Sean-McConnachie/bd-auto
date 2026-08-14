#!/usr/bin/env bash
# End-to-end smoke test for bd-auto against a throwaway epic.
#
# Unit tests cover the pure logic. This exercises the parts that only fail for
# real: talking to bd, reading the DAG, driving the run state across processes,
# and the Stop hook's decisions. It creates its own issues and branches and
# deletes them again.
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

step "Stop hook refuses to stop while workers are in flight"
OUT=$(echo '{"hook_event_name":"Stop"}' | "$BD_AUTO" hook stop 2>&1)
RC=$?
[ "$RC" -eq 2 ] && pass "exit 2 (blocks the stop)" || fail "expected exit 2, got $RC"
check "names the in-flight issue" "$A" "$OUT"
check "tells the model what to do" 'Do not stop' "$OUT"

step "Stop hook stays silent for a subagent"
OUT=$(echo "{\"hook_event_name\":\"Stop\",\"agent_id\":\"x\"}" | "$BD_AUTO" hook stop 2>&1)
RC=$?
[ "$RC" -eq 0 ] && pass "does not block subagents" || fail "expected exit 0, got $RC"

step "PreToolUse binds a claiming agent to its issue"
echo "{\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Bash\",\"agent_id\":\"agent-7\",\"agent_type\":\"bd-worker\",\"tool_input\":{\"command\":\"bd update $A --claim\"}}" |
  "$BD_AUTO" hook pre-tool-use >/dev/null 2>&1
OUT=$(cat "$REPO/.beads/auto/run.json")
check "binding recorded" "\"agent-7\": \"$A\"" "$OUT"

step "PreToolUse blocks a worker from merging"
OUT=$(echo '{"hook_event_name":"PreToolUse","tool_name":"Bash","agent_id":"agent-7","agent_type":"bd-worker","tool_input":{"command":"git merge main"}}' |
  "$BD_AUTO" hook pre-tool-use 2>/dev/null)
check "denied" '"permissionDecision": "deny"' "$OUT"
check "explains why" 'bd-integrator' "$OUT"

step "PreToolUse lets the integrator merge"
OUT=$(echo '{"hook_event_name":"PreToolUse","tool_name":"Bash","agent_id":"agent-9","agent_type":"bd-integrator","tool_input":{"command":"git merge main"}}' |
  "$BD_AUTO" hook pre-tool-use 2>/dev/null)
if printf '%s' "$OUT" | grep -q 'deny'; then fail "integrator must not be blocked"; else pass "integrator allowed"; fi

step "SubagentStop sends back a worker whose issue is not closed"
# Keep the message on one line: a raw newline inside a JSON string is invalid
# JSON, and the hook fails open on unparseable input.
SUBAGENT_STOP_JSON="{\"hook_event_name\":\"SubagentStop\",\"agent_type\":\"bd-worker\",\"agent_id\":\"agent-7\",\"last_assistant_message\":\"all done. BD-AUTO: issue=$A branch=bd-auto/$A status=done\"}"
OUT=$(printf '%s' "$SUBAGENT_STOP_JSON" | "$BD_AUTO" hook subagent-stop 2>&1)
RC=$?
[ "$RC" -eq 2 ] && pass "exit 2 (worker sent back)" || fail "expected exit 2, got $RC"
check "explains the real state" 'is still' "$OUT"
check "names the issue" "$A" "$OUT"

step "SubagentStop demands the report footer when nothing identifies the issue"
OUT=$(printf '%s' '{"hook_event_name":"SubagentStop","agent_type":"bd-worker","agent_id":"unknown-agent","last_assistant_message":"I finished the work"}' |
  "$BD_AUTO" hook subagent-stop 2>&1)
RC=$?
[ "$RC" -eq 2 ] && pass "exit 2 (footer demanded)" || fail "expected exit 2, got $RC"
check "asks for the footer" 'BD-AUTO: issue=' "$OUT"

step "SubagentStop ignores agents that are not ours"
OUT=$(printf '%s' '{"hook_event_name":"SubagentStop","agent_type":"Explore","agent_id":"e1","last_assistant_message":"done"}' |
  "$BD_AUTO" hook subagent-stop 2>&1)
RC=$?
[ "$RC" -eq 0 ] && pass "leaves other agents alone" || fail "expected exit 0, got $RC"

step "worker done is refused while the issue is still open"
OUT=$("$BD_AUTO" worker done --issue "$A" 2>&1)
check "refuses to record an unclosed issue as done" 'not closed' "$OUT"

step "worker done accepted once the issue is really closed"
bd close "$A" -q >/dev/null 2>&1
OUT=$("$BD_AUTO" worker done --issue "$A" 2>/dev/null)
check "recorded" '"recorded": "done"' "$OUT"

step "SubagentStop now lets the worker finish"
OUT=$(printf '%s' "$SUBAGENT_STOP_JSON" | "$BD_AUTO" hook subagent-stop 2>&1)
RC=$?
[ "$RC" -eq 0 ] && pass "exit 0 once the issue is closed" || fail "expected exit 0, got $RC"

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

step "unparking an issue that is not parked is refused"
OUT=$("$BD_AUTO" run unpark --issue "$C" 2>&1)
check "refused" 'not parked' "$OUT"

step "merge-order reports branches in dependency order"
git switch -c "bd-auto/$C" -q 2>/dev/null
echo "smoke" >smoke-artifact.txt && git add smoke-artifact.txt &&
  git commit -qm "smoke: $C" >/dev/null 2>&1
git switch -q - 2>/dev/null
"$BD_AUTO" plan --dispatch >/dev/null 2>&1
OUT=$("$BD_AUTO" merge-order 2>/dev/null)
check "C's branch is mergeable" "bd-auto/$C" "$OUT"
check "commit counted" '"commits": 1' "$OUT"

step "run status --context rehydrates after a compaction"
OUT=$("$BD_AUTO" run status --context 2>/dev/null)
check "identifies the run" "$EPIC" "$OUT"
check "states the orchestrator role" 'You are the orchestrator' "$OUT"
check "lists parked work" "$B" "$OUT"

step "run stop disarms every hook"
"$BD_AUTO" run stop >/dev/null 2>&1
OUT=$(echo '{"hook_event_name":"Stop"}' | "$BD_AUTO" hook stop 2>&1)
RC=$?
[ "$RC" -eq 0 ] && pass "Stop hook is a no-op with no run" || fail "expected exit 0, got $RC"
[ -z "$OUT" ] && pass "and stays silent" || fail "should be silent, said: $OUT"

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
