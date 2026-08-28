#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$ROOT/scripts/screenshot-manifest.sh"

WORK=$(mktemp -d /tmp/bd-auto-shot-manifest-test.XXXXXX)
trap 'rm -rf "$WORK"' EXIT

fail() {
  echo "screenshot-manifest-test: $*" >&2
  exit 1
}

touch_set() {
  local dir=$1
  shift
  local name
  for name in "$@"; do
    : > "$dir/$name"
  done
}

OUT=$WORK/complete
mkdir -p "$OUT"
MANIFEST=$WORK/complete.manifest
touch_set "$OUT" \
  tui-01-current.ansi tui-01-current.png \
  ro-01-current.ansi ro-01-current.png \
  tui-88-stale.ansi tui-88-stale.png \
  ro-88-stale.ansi ro-88-stale.png \
  notes.png custom.ansi tui-not-managed.txt
screenshot_manifest_add "$MANIFEST" \
  tui-01-current.ansi tui-01-current.png \
  ro-01-current.ansi ro-01-current.png
screenshot_prune "$OUT" "$MANIFEST" true true

for name in tui-01-current.ansi tui-01-current.png ro-01-current.ansi ro-01-current.png notes.png custom.ansi tui-not-managed.txt; do
  [ -f "$OUT/$name" ] || fail "removed retained file $name"
done
for name in tui-88-stale.ansi tui-88-stale.png ro-88-stale.ansi ro-88-stale.png; do
  [ ! -e "$OUT/$name" ] || fail "left stale managed file $name"
done
[ "$(screenshot_manifest_count "$MANIFEST")" = 2 ] || fail "manifest count is not 2"

for completed in 'false true' 'true false'; do
  OUT=$WORK/incomplete-${completed// /-}
  mkdir -p "$OUT"
  MANIFEST=$OUT/manifest
  touch_set "$OUT" tui-99-stale.ansi tui-99-stale.png
  screenshot_manifest_add "$MANIFEST" tui-01-current.ansi tui-01-current.png
  read -r tui_done ro_done <<< "$completed"
  if screenshot_prune "$OUT" "$MANIFEST" "$tui_done" "$ro_done"; then
    fail "incomplete generation was accepted: $completed"
  fi
  [ -f "$OUT/tui-99-stale.ansi" ] && [ -f "$OUT/tui-99-stale.png" ] || \
    fail "incomplete generation pruned existing captures: $completed"
done

echo "screenshot-manifest-test: pass"
