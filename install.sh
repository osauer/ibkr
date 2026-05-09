#!/usr/bin/env bash
# Install ibkr + ibkrd binaries and the Claude Code skill bundle.
#
# Usage:
#   ./install.sh                  # build + install binaries + skill, no settings merge
#   ./install.sh --merge-settings # also merge the skill's settings snippet
#                                 # into ~/.claude/settings.json
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

GOBIN_DIR="${GOBIN:-$(go env GOPATH)/bin}"
CLAUDE_DIR="${CLAUDE_DIR:-$HOME/.claude}"
SKILL_DIR="$CLAUDE_DIR/skills/ibkr"
SETTINGS_FILE="$CLAUDE_DIR/settings.json"
MERGE_SETTINGS=0

for arg in "$@"; do
  case "$arg" in
    --merge-settings) MERGE_SETTINGS=1 ;;
    --help|-h)
      grep -E '^#( |$)' "$0" | sed 's/^# \?//'
      exit 0
      ;;
    *)
      echo "unknown option: $arg" >&2
      exit 2
      ;;
  esac
done

echo "==> Building ibkr + ibkrd"
make -s build

echo "==> Installing binaries to $GOBIN_DIR"
install -d "$GOBIN_DIR"
install -m 0755 bin/ibkr "$GOBIN_DIR/ibkr"
install -m 0755 bin/ibkrd "$GOBIN_DIR/ibkrd"

echo "==> Installing skill to $SKILL_DIR"
install -d "$SKILL_DIR"
install -m 0644 skill/SKILL.md "$SKILL_DIR/SKILL.md"
install -m 0644 skill/schemas.md "$SKILL_DIR/schemas.md"

if [[ "$MERGE_SETTINGS" -eq 1 ]]; then
  if ! command -v jq >/dev/null 2>&1; then
    echo "  --merge-settings requires jq; install it and re-run." >&2
    exit 1
  fi
  echo "==> Merging settings into $SETTINGS_FILE"
  install -d "$CLAUDE_DIR"
  if [[ -f "$SETTINGS_FILE" ]]; then
    cp "$SETTINGS_FILE" "$SETTINGS_FILE.bak.$(date +%s)"
  else
    echo '{}' > "$SETTINGS_FILE"
  fi
  tmp=$(mktemp)
  jq -s '
    .[0] as $existing | .[1] as $skill |
    $existing
    * { permissions: (
          ($existing.permissions // {})
          + { allow: ((($existing.permissions.allow // []) + ($skill.permissions.allow // [])) | unique) ,
              deny:  ((($existing.permissions.deny  // []) + ($skill.permissions.deny  // [])) | unique) }
        ),
        hooks: ($existing.hooks // {} | . + { PreToolUse: (
          (($existing.hooks.PreToolUse // []) + ($skill.hooks.PreToolUse // [])) ) })
      }
  ' "$SETTINGS_FILE" settings/ibkr.settings.json > "$tmp"
  mv "$tmp" "$SETTINGS_FILE"
  echo "  merged; previous settings backed up to $SETTINGS_FILE.bak.*"
fi

echo
echo "Done."
echo
echo "Sanity checks:"
echo "  $GOBIN_DIR/ibkr version"
echo "  $GOBIN_DIR/ibkr status"
echo
if [[ "$MERGE_SETTINGS" -eq 0 ]]; then
  echo "Run with --merge-settings to also wire up Claude Code permissions and the"
  echo "PreToolUse hook (recommended). Alternatively, manually merge"
  echo "settings/ibkr.settings.json into $SETTINGS_FILE."
fi
