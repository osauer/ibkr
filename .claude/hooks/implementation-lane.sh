#!/usr/bin/env bash
# Implementation-lane gate (ibkr pilot): all code in this repo is implemented
# by headless Codex via scripts/codex-implement.sh; Claude sessions and their
# subagents plan, brief, review, and integrate but do not hand-edit code.
# This PreToolUse hook (matcher Edit|Write in .claude/settings.json) enforces
# that deterministically. The break-glass is a per-session waiver written by
# scripts/waive-inline.sh, which is deliberately not allowlisted so each use
# needs the user's permission click. Docs and config stay freely editable:
# only the code extensions below are gated, and only inside this checkout.
set -euo pipefail

payload="$(cat)"
root="${CLAUDE_PROJECT_DIR:-$PWD}"

code_ext_regex='\.(go|js|mjs|ts|tsx|sh|bash|html|css)$'

block() {
  printf '%s\n' "$1" >&2
  exit 2
}

if ! command -v jq >/dev/null 2>&1; then
  # Fail closed for the gated class only: without jq the payload cannot be
  # parsed, so block anything that looks like a code-file edit.
  if printf '%s' "$payload" | grep -Eq '"file_path"[[:space:]]*:[[:space:]]*"[^"]*\.(go|js|mjs|ts|tsx|sh|bash|html|css)"'; then
    block "implementation-lane hook: jq is required to inspect code-edit payloads. Install jq, or delegate the change via the codex-delegate skill."
  fi
  exit 0
fi

file_path="$(printf '%s' "$payload" | jq -r '.tool_input.file_path // empty')"
[ -n "$file_path" ] || exit 0

base="$(basename "$file_path")"
case "$base" in
  Makefile | *.mk) : ;;
  *)
    printf '%s' "$base" | grep -Eq "$code_ext_regex" || exit 0
    ;;
esac

# Gate only files inside this checkout; scratchpad and external files are free.
case "$file_path" in
  /*)
    case "$file_path" in
      "$root"/*) : ;;
      *) exit 0 ;;
    esac
    ;;
esac

session_id="$(printf '%s' "$payload" | jq -r '.session_id // "unknown-session"')"
waiver_dir="$root/.claude/state/inline-waivers"

if [ -d "$waiver_dir" ]; then
  find "$waiver_dir" -type f -mmin +2880 -delete 2>/dev/null || true
  [ -f "$waiver_dir/$session_id" ] && exit 0
fi

block "implementation-lane: code in this repo is implemented by Codex, not edited inline.
Blocked: $file_path
Default lane: write a brief and delegate via the codex-delegate skill (directly or through the coder agent); integrate the reviewed patch with git apply.
Break-glass (requires the user's permission click): scripts/waive-inline.sh $session_id \"<why inline>\" — then retry this edit."
