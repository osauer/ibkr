#!/usr/bin/env bash
# Implementation-lane gate (ibkr pilot): primary-tree code stays gated to
# headless Codex by default; agent worktrees allow Claude edits as the
# hard-cap fallback. This PreToolUse hook (matcher Edit|Write in
# .claude/settings.json) enforces that deterministically. The valve is a
# per-session waiver written by scripts/waive-inline.sh: the orchestrating
# session may self-grant it with a logged reason when its judgment says
# inline action is right (user decision 2026-07-19); spawned agents must
# never invoke it. Docs and config stay freely editable: only the code
# extensions below are gated, and only inside this checkout.
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

# Allow code edits in real Claude agent worktrees at the repository root. The
# strict generated-name shape plus a regular .git worktree marker keeps plain
# lookalike directories gated.
relative_path="${file_path#"$root"/}"
first_segment="${relative_path%%/*}"
case "/$relative_path/" in
  */../*) ;;
  *)
    if printf '%s\n' "$first_segment" | grep -Eq '^[a-z0-9]+(-[a-z0-9]+)+-[0-9a-f]{6}$' &&
      [ -f "$root/$first_segment/.git" ]; then
      exit 0
    fi
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
