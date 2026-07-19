#!/usr/bin/env bash
# Oversteer valve for the implementation-lane hook: records an inline-edit
# waiver for one Claude session (48h max; the hook prunes older files).
# Contract (user decision 2026-07-19): Codex stays the default coding lane,
# but the ORCHESTRATING session may self-grant this waiver when its judgment
# says inline action is right (Codex window hard-capped, urgent fix, broken
# delegation path). Every use must carry a concrete reason and be announced
# in the session — the waiver file is the audit record, silence is a
# violation. The script is allowlisted in .claude/settings.json for exactly
# this purpose; delegated/spawned agents must still never invoke it.
set -euo pipefail

if [ $# -lt 2 ] || [ -z "$1" ] || [ -z "$2" ]; then
  echo "usage: scripts/waive-inline.sh <session-id> \"<reason>\"" >&2
  exit 1
fi

session_id="$1"
shift
reason="$*"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
waiver_dir="$root/.claude/state/inline-waivers"
mkdir -p "$waiver_dir"
printf 'granted: %s\nreason: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$reason" >"$waiver_dir/$session_id"
echo "inline-edit waiver recorded for session $session_id: $reason"
