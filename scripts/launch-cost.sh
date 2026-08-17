#!/usr/bin/env bash
# What a drain costs the session that launches it.
#
# This is the project's headline claim, so it is a script rather than a number
# in a README: re-run it after touching SKILL.md or the poll view and see
# whether the claim still holds.
#
# It adds up everything that enters the launching session's context when the
# bd-auto skill runs a drain end to end:
#
#   skill body        loaded once, when the skill is invoked
#   launch call       the drain command; its output is redirected and never read
#   status polls      one `run status --context --wait 1h` per hour of run
#   final report      what the model writes from the last poll
#
# The claim being defended is that **none of these grows with the epic**. The
# skill body is fixed. The poll view is capped, and
# internal/cmds/run_test.go asserts that cap holds at 8, 400 and 4000 issues.
# The poll count follows the wall clock, not the issue count — which is why the
# wait window is an hour rather than the couple of minutes a naive poll loop
# would use.
#
# So the answer has two halves: a fixed cost, and a cost per hour of run. What
# the 2000-token budget buys is a number of hours, printed below.
#
# Usage: scripts/launch-cost.sh [hours]   (default 6)
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOURS="${1:-6}"
BUDGET=2000

# Bytes per token, in tenths so the arithmetic stays integer. Claude's tokenizer
# averages nearer 3.8 on English prose with markdown; 3.5 is used so this errs
# high rather than flattering itself. Nothing here needs an API key, because a
# measurement that needs one is a measurement nobody re-runs.
BPT=35

# Per-call overhead: the command string plus the tool-call framing round it.
# Deliberately generous.
LAUNCH_CALL=200
POLL_CALL=140

# The cap the poll view is held to, asserted by
# TestRenderContextIsBoundedByEpicSize. This is the worst case — both named
# lists saturated with long issue IDs — not the typical one. TYPICAL is what a
# run at concurrency 5 with nothing parked actually prints; the budget is
# checked against the worst case and the typical figure is reported beside it,
# because a claim that only holds on a good day is not a bound.
POLL_OUT=400
POLL_OUT_TYPICAL=220

# What the model writes at the end: what landed, what is parked, where the log
# is. A few sentences.
FINAL_REPORT=500

SKILL="$REPO/skills/bd-auto/SKILL.md"
[ -f "$SKILL" ] || {
  echo "missing $SKILL"
  exit 1
}
SKILL_BYTES=$(wc -c <"$SKILL" | tr -d ' ')

tok() { echo $(($1 * 10 / BPT)); }

PER_POLL=$((POLL_CALL + POLL_OUT))
FIXED=$((SKILL_BYTES + LAUNCH_CALL + FINAL_REPORT))
POLL_BYTES=$((PER_POLL * HOURS))
TOTAL=$((FIXED + POLL_BYTES))

printf '\nWhat one drain costs the session that launches it\n'
printf -- '-------------------------------------------------\n'
printf '%-34s %7s %8s\n' 'term' 'bytes' 'tokens'
printf '%-34s %7d %8d\n' 'skill body (SKILL.md)' "$SKILL_BYTES" "$(tok "$SKILL_BYTES")"
printf '%-34s %7d %8d\n' 'launch tool call' "$LAUNCH_CALL" "$(tok $LAUNCH_CALL)"
printf '%-34s %7d %8d\n' 'final report' "$FINAL_REPORT" "$(tok $FINAL_REPORT)"
printf '%-34s %7d %8d\n' '  fixed subtotal' "$FIXED" "$(tok "$FIXED")"
printf -- '-------------------------------------------------\n'
printf '%-34s %7d %8d\n' 'per hour of run (one poll)' "$PER_POLL" "$(tok $PER_POLL)"
printf '%-34s %7d %8d\n' "x $HOURS hour(s)" "$POLL_BYTES" "$(tok "$POLL_BYTES")"
printf -- '-------------------------------------------------\n'
printf '%-34s %7d %8d\n' 'total' "$TOTAL" "$(tok "$TOTAL")"

TOKENS=$(tok "$TOTAL")
SPARE=$(((BUDGET - $(tok "$FIXED")) * BPT / 10))
HEADROOM=$((SPARE / PER_POLL))
HEADROOM_TYPICAL=$((SPARE / (POLL_CALL + POLL_OUT_TYPICAL)))
printf '\nEvery term above is flat in the number of issues.\n'
printf 'At 3.5 bytes/token the %d-token budget buys %d hours of run at the poll\n' "$BUDGET" "$HEADROOM"
printf 'view'"'"'s worst case, and %d hours at what a typical run prints.\n' "$HEADROOM_TYPICAL"

if [ "$TOKENS" -lt "$BUDGET" ]; then
  printf 'PASS: %d tokens for a %d-hour drain, under %d\n\n' "$TOKENS" "$HOURS" "$BUDGET"
  exit 0
fi
printf 'FAIL: %d tokens for a %d-hour drain, over %d\n\n' "$TOKENS" "$HOURS" "$BUDGET"
exit 1
