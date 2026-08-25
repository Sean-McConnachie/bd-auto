#!/usr/bin/env bash
# Photograph the wave table in every state it has.
#
# internal/tui/screenshot_test.go drives the real view over synthetic drain
# events and stops at each scene. This runs it on a real terminal — a tmux pane
# of a fixed size — presses that scene's keys, captures the pane, and turns the
# capture into a PNG.
#
#   scripts/tui-shots.sh [output-dir]
#
# It spawns no models and touches no repository state. The .ansi capture is
# kept next to each .png: it is the exact bytes the view wrote, and it diffs.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=${1:-$ROOT/docs/screenshots/tui}
WORK=$(mktemp -d /tmp/bd-auto-shots.XXXXXX)
BIN=$WORK/tui.test
SESSION=bd-auto-shots-$$
COLS=118
ROWS=44
NARROW=64
SHORT=20

command -v tmux >/dev/null || { echo "tui-shots: tmux is required" >&2; exit 1; }
python3 -c 'import PIL' 2>/dev/null || { echo "tui-shots: python3 with Pillow is required" >&2; exit 1; }

cleanup() {
  tmux kill-session -t "$SESSION" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

mkdir -p "$OUT"
echo "tui-shots: building the harness"
(cd "$ROOT" && go test -c -o "$BIN" ./internal/tui)

# keys_for prints what to press for a scene, one instruction per line:
#
#   key:<name>   a named key, as tmux spells it — Down, Escape
#   lit:<text>   literal characters, spaces and all
#
# The two are separate because `tmux send-keys -l` takes its arguments
# unquoted and joins them with nothing between, so a sentence sent that way
# arrives with every space missing.
keys_for() {
  case "$1" in
    selection)          printf 'key:Down\nkey:Down\n' ;;
    transcript)         printf 'key:Enter\n' ;;
    transcript-top)     printf 'lit:g\n' ;;
    transcript-empty)   printf 'key:Escape\nkey:Down\nkey:Down\nkey:Enter\n' ;;
    # esc closes the transcript and the cursor is where it was left, which is
    # the row this then kills.
    killing)            printf 'key:Escape\nkey:Up\nkey:Up\nlit:k\n' ;;
    question-choice)    printf 'key:Down\n' ;;
    question-typing)    printf 'lit:t\nlit:an array, but every value a string\n' ;;
    question-queued)    printf 'key:Escape\n' ;;
    question-swallowed) printf 'lit:k\n' ;;
    question-answered)  printf 'lit:1\n' ;;
    question-declined)  printf 'lit:s\n' ;;
    stages)             printf 'key:Escape\n' ;;
    stopping)           printf 'lit:q\n' ;;
    readonly-refused)   printf 'lit:k\n' ;;
    readonly-dismissed) printf 'key:Escape\n' ;;
    *)                  : ;;
  esac
}

# press sends one instruction from keys_for.
press() {
  case "$1" in
    key:*) tmux send-keys -t "$SESSION:0.0" "${1#key:}" ;;
    lit:*) tmux send-keys -t "$SESSION:0.0" -l "${1#lit:}" ;;
    *)     echo "tui-shots: unreadable key instruction: $1" >&2; return 1 ;;
  esac
}

# resize_for resizes the window before a scene that is about its size. Every
# one of these is a real SIGWINCH reaching a real program.
resize_for() {
  case "$1" in
    narrow)          tmux resize-window -t "$SESSION" -x "$NARROW" -y "$ROWS" ;;
    wide)            tmux resize-window -t "$SESSION" -x "$COLS" -y "$ROWS" ;;
    question-narrow) tmux resize-window -t "$SESSION" -x "$NARROW" -y "$ROWS" ;;
    question-short)  tmux resize-window -t "$SESSION" -x "$NARROW" -y "$SHORT" ;;
    # The scene after the two above, which is where the window goes back.
    question-choice) tmux resize-window -t "$SESSION" -x "$COLS" -y "$ROWS" ;;
    *)               : ;;
  esac
}

capture() { # capture <file>
  tmux capture-pane -p -e -t "$SESSION:0.0" > "$1"
}

run_test() { # run_test <test-name> <prefix>
  local tname=$1
  local prefix=$2
  local dir="$WORK/$prefix"
  mkdir -p "$dir"
  echo "tui-shots: $tname"

  tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS" \
    "env BD_AUTO_SHOTS='$dir' TERM=xterm-256color COLORTERM=truecolor '$BIN' -test.run '^${tname}$'; \
     printf 'harness exited %s\\n' \$?; sleep 600"
  tmux set-option -t "$SESSION" -q status off 2>/dev/null || true

  local n=0 waited ready name
  while :; do
    n=$((n + 1))
    ready=""
    for ((waited = 0; waited < 1200; waited++)); do
      ready=$(compgen -G "$dir/ready-$(printf '%02d' "$n")-*" || true)
      [ -n "$ready" ] && break
      # The harness is finished when the process is gone and no scene is up.
      if ! tmux list-panes -t "$SESSION" -F '#{pane_pid}' >/dev/null 2>&1; then break; fi
      sleep 0.1
    done
    [ -z "$ready" ] && break

    name=${ready##*/ready-}
    name=${name#*-}
    resize_for "$name"
    while IFS= read -r line; do
      [ -z "$line" ] && continue
      press "$line"
      sleep 0.25
    done < <(keys_for "$name")
    sleep 0.7

    local stem
    stem=$(printf '%s-%02d-%s' "$prefix" "$n" "$name")
    capture "$OUT/$stem.ansi"
    python3 "$ROOT/scripts/ansi2png.py" "$OUT/$stem.ansi" "$OUT/$stem.png" \
      --title "bd-auto — $name"
    echo "  $stem"
    touch "$dir/go-$(printf '%02d' "$n")"
  done

  # Give the harness a moment to report, then read its verdict out of the pane.
  sleep 1.5
  capture "$WORK/$prefix-exit.ansi"
  if ! grep -qE 'harness exited 0|^ok ' "$WORK/$prefix-exit.ansi"; then
    echo "tui-shots: $tname did not pass:" >&2
    sed -e 's/\x1b\[[0-9;]*m//g' "$WORK/$prefix-exit.ansi" | tail -20 >&2
    tmux kill-session -t "$SESSION" 2>/dev/null || true
    return 1
  fi
  tmux kill-session -t "$SESSION" 2>/dev/null || true
}

run_test TestScreenshots tui
run_test TestScreenshotsReadOnly ro

echo "tui-shots: $(ls "$OUT"/*.png | wc -l) screenshots in $OUT"
