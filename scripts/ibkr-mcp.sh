#!/usr/bin/env bash
set -euo pipefail

candidate_paths=()

if [[ -n "${IBKR_BIN:-}" ]]; then
	candidate_paths+=("$IBKR_BIN")
fi

if [[ -n "${CLAUDE_PLUGIN_ROOT:-}" ]]; then
	candidate_paths+=("$CLAUDE_PLUGIN_ROOT/bin/ibkr")
fi

if command -v ibkr >/dev/null 2>&1; then
	candidate_paths+=("$(command -v ibkr)")
fi

candidate_paths+=(
	"$HOME/.local/bin/ibkr"
	"/opt/homebrew/bin/ibkr"
	"/usr/local/bin/ibkr"
)

for candidate in "${candidate_paths[@]}"; do
	if [[ -x "$candidate" ]]; then
		exec "$candidate" mcp
	fi
done

cat >&2 <<'EOF'
ibkr Claude Code plugin could not find an executable ibkr binary.

Install the CLI first, then restart Claude Code:
  curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh

For local development from a checkout:
  make install

You can also set IBKR_BIN=/absolute/path/to/ibkr before starting Claude Code.
EOF

exit 127
