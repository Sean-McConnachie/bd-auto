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
# Usage: scripts/smoke.sh [--isolated]
#
# --isolated builds a throwaway repo with its own beads database, copies the
# binary in, and runs this script inside it. That is what makes smoke usable at
# the moment it is most wanted: it refuses to start while a run is active,
# correctly, because its cleanup deletes .beads/auto — which means a worker
# changing bd-auto during a drain could not run it at all, and verifying that
# change meant hand-building a throwaway repo every time.
set -uo pipefail

ISOLATED=0
DRAIN=1
for arg in "$@"; do
  case $arg in
  --isolated) ISOLATED=1 ;;
  --no-drain) DRAIN=0 ;;
  -h | --help)
    sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
  *)
    echo "unknown argument: $arg" >&2
    exit 2
    ;;
  esac
done

SOURCE_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# --- isolation ----------------------------------------------------------------
#
# The fixture is built the way scripts/resume-vs-fresh.sh builds its own: git
# init, bd init with its own prefix, and beads' git hooks turned off. The hooks
# matter here for the same reason they do there — the post-checkout hook imports
# .beads/issues.jsonl back over the database, so creating a worktree reverts
# whatever bd-auto has written since the base commit.
if [ "$ISOLATED" = 1 ]; then
  FIXTURE=$(mktemp -d "${TMPDIR:-/tmp}/bd-auto-smoke.XXXXXX") || exit 1
  [ -x "$SOURCE_REPO/bin/bd-auto" ] || { echo "build first: make build"; exit 1; }
  (
    cd "$FIXTURE" || exit 1
    git init -q -b main .
    git config user.email "smoke@bd-auto.invalid"
    git config user.name "bd-auto smoke"
    mkdir -p bin scripts .claude-plugin skills/bd-auto
    cp "$SOURCE_REPO/bin/bd-auto" bin/bd-auto
    cp "$SOURCE_REPO/scripts/smoke.sh" scripts/smoke.sh
    # The plugin-surface checks assert on these, and a fixture without them
    # would report a failure about the fixture rather than about bd-auto.
    cp "$SOURCE_REPO/.claude-plugin/plugin.json" .claude-plugin/plugin.json
    cp "$SOURCE_REPO/skills/bd-auto/SKILL.md" skills/bd-auto/SKILL.md
    printf 'fixture\n' > README.md
    git add -A && git commit -qm "fixture: the binary and the script"
    bd init --prefix=smk >/dev/null 2>&1
    git config --unset core.hooksPath 2>/dev/null
    git add -A
    git commit -qm "fixture: beads" >/dev/null 2>&1
    true
  ) || { echo "could not build the fixture at $FIXTURE" >&2; exit 1; }
  echo "isolated: $FIXTURE"
  # Re-exec inside the fixture. --isolated is dropped so the child is an
  # ordinary run against a repo that happens to be disposable.
  ( cd "$FIXTURE" && bash scripts/smoke.sh $([ "$DRAIN" = 1 ] || echo --no-drain) )
  RC=$?
  [ -n "${KEEP:-}" ] || rm -rf "$FIXTURE"
  exit "$RC"
fi

REPO="$SOURCE_REPO"
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
  # A drain case that died between swapping the config and putting it back
  # would otherwise leave the repo running the smoke configuration.
  [ -f "$REPO/.beads-auto.yaml.smoke-bak" ] &&
    mv "$REPO/.beads-auto.yaml.smoke-bak" "$REPO/.beads-auto.yaml"
  rm -f "$REPO/.beads-auto-smoke.yaml" "$REPO/smoke-worker.sh" "$REPO"/drained-*.txt
  rm -rf "$REPO/.beads/auto"
  echo "  cleaned"
}
# Both preflight checks must run BEFORE the trap is armed: bailing out with
# cleanup installed would tear down state this script never created.
[ -x "$BD_AUTO" ] || { echo "build first: make build"; exit 1; }

# Cleanup calls `run stop` and deletes .beads/auto, and run state is shared with
# the main checkout even when this runs from a worktree. Against a live drain
# that silently destroys the run, so refuse rather than clobber.
#
# The poll view rather than the JSON, because it answers both halves in four
# lines: the status of a run somebody started, and the in-flight line, which a
# standalone `bd-auto issue run` also prints. A standalone run is not armed and
# does not report as active, but its worker is holding this same state.
POLL=$("$BD_AUTO" run status --context 2>/dev/null || true)
case $POLL in
*"bd-auto run: active"* | *"bd-auto run: paused"* | *"running: "*)
  echo "a bd-auto run is in progress; smoke would stop it and delete its state."
  echo "finish it, or 'bd-auto run stop', before running smoke."
  exit 1
  ;;
esac

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

# --- a real drain, with no model anywhere near it -----------------------------
#
# beads-auto-imp-tpk. Everything above decides something and stops; this is the
# only case that dispatches a worker, merges a branch and settles an epic. It
# runs under `provider: fake` with a command, so the work is a shell script and
# the engine cannot tell the difference — see internal/runner/fake/exec.go.
if [ "$DRAIN" = 1 ]; then
  step "a whole drain, under provider: fake"

  DRAIN_EPIC=$(bd create --title="smoke drain epic" --type=epic --label "$LABEL" --silent 2>/dev/null)
  if [ -z "$DRAIN_EPIC" ]; then
    fail "could not create the drain epic"
  else
    DRAIN_IDS=""
    for n in 1 2 3; do
      id=$(bd create --title="smoke drain issue $n" --parent="$DRAIN_EPIC" --label "$LABEL" \
        --description="Create drained-$n.txt, commit it, and close this issue." --silent 2>/dev/null)
      DRAIN_IDS="${DRAIN_IDS:+$DRAIN_IDS,}$id"
    done

    # The worker: write a file, commit it, close the issue. Exactly the three
    # things prompts/worker.md asks for, which is what makes the drain's verdict
    # about the engine rather than about the script.
    cat >"$REPO/smoke-worker.sh" <<'WORKER'
#!/usr/bin/env bash
set -eu
issue=$(git rev-parse --abbrev-ref HEAD | sed 's|^bd-auto/||')
printf 'drained by %s\n' "$issue" > "drained-$issue.txt"
git add -A
git commit -qm "$issue: smoke drain"
bd close "$issue" --reason="smoke drain" >/dev/null
WORKER
    chmod +x "$REPO/smoke-worker.sh"

    cat >"$REPO/.beads-auto-smoke.yaml" <<YAML
gate: []
pipeline:
  - stage: implement
runners:
  default:
    provider: fake
    extra_args: ["$REPO/smoke-worker.sh"]
concurrency: 2
autonomy: auto
retry: 0
discovered_work: triage
handoff:
  branch: false
  pr: false
YAML
    cp "$REPO/.beads-auto.yaml" "$REPO/.beads-auto.yaml.smoke-bak" 2>/dev/null
    cp "$REPO/.beads-auto-smoke.yaml" "$REPO/.beads-auto.yaml"

    OUT=$("$BD_AUTO" drain --issues "$DRAIN_IDS" --plain --no-preflight 2>&1)
    RC=$?
    [ "$RC" -eq 0 ] && pass "the drain exited 0" ||
      fail "the drain exited $RC: $(printf '%s' "$OUT" | tail -5)"

    for n in 1 2 3; do
      id=$(printf '%s' "$DRAIN_IDS" | cut -d, -f"$n")
      st=$(bd show "$id" --json 2>/dev/null | grep -o '"status": *"[^"]*"' | head -1 | cut -d'"' -f4)
      [ "$st" = "closed" ] && pass "$id closed" || fail "$id is $st, want closed"
      [ -f "$REPO/drained-$id.txt" ] && pass "$id's work merged into the checkout" ||
        fail "drained-$id.txt is not in the checkout: the branch did not merge"
    done

    check "the run reports every issue done" '"done"' "$OUT"

    # Restore before the epic assertions, so a failure below still leaves the
    # repo's own config in place.
    [ -f "$REPO/.beads-auto.yaml.smoke-bak" ] &&
      mv "$REPO/.beads-auto.yaml.smoke-bak" "$REPO/.beads-auto.yaml"
    rm -f "$REPO/.beads-auto-smoke.yaml" "$REPO/smoke-worker.sh"
    for n in 1 2 3; do
      id=$(printf '%s' "$DRAIN_IDS" | cut -d, -f"$n")
      rm -f "$REPO/drained-$id.txt"
    done
    git rm -q --cached --ignore-unmatch "drained-*.txt" >/dev/null 2>&1
  fi
fi

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
# The hook command is NOT gone, and asserting it was is what this used to do.
# beads-auto-imp-wz9.11 cut the hooks file and the agents; beads-auto-imp-nwu
# then made the command itself fail open, because a hook that errors takes the
# operator's whole Claude Code session with it. So the contract is the opposite
# of what was asserted here: it must exit 0 for every event, known or not.
for event in stop subagent-stop pre-tool-use not-an-event ""; do
  echo '{}' | "$BD_AUTO" hook $event >/dev/null 2>&1
  RC=$?
  [ "$RC" -eq 0 ] && pass "hook ${event:-<none>} exits 0" ||
    fail "hook ${event:-<none>} exited $RC: a hook that errors wedges the session"
done

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
