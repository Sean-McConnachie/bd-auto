#!/usr/bin/env bash

# Helpers for screenshot generators. This file is sourceable so pruning can be
# tested without tmux, Pillow, or the Go harness.

screenshot_manifest_add() { # manifest basename...
  local manifest=$1
  shift
  printf '%s\n' "$@" >> "$manifest"
}

screenshot_prune() { # output-dir manifest tui-complete ro-complete
  local out=$1 manifest=$2 tui_complete=$3 ro_complete=$4
  [ "$tui_complete" = true ] && [ "$ro_complete" = true ] || return 1
  [ -f "$manifest" ] || return 1

  local path name
  for path in "$out"/tui-*.ansi "$out"/tui-*.png "$out"/ro-*.ansi "$out"/ro-*.png; do
    [ -f "$path" ] || continue
    name=${path##*/}
    if ! grep -Fqx -- "$name" "$manifest"; then
      rm -f -- "$path"
    fi
  done
}

screenshot_manifest_count() { # manifest
  grep -Ec '\.png$' "$1" || true
}
