#!/usr/bin/env bash
# Break-glass for the implementation-lane hook: records a human-approved
# inline-edit waiver for one Claude session (48h max; the hook prunes older
# files). Deliberately NOT allowlisted in .claude/settings.json — the
# permission prompt this command triggers in an agent session IS the
# approval. Keep it that way.
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
