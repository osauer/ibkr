#!/usr/bin/env bash
#
# release-auth-preflight.sh - fail-fast auth checks before the release
# pipeline spends ~10 minutes on gates. Two credentials go stale between
# releases and both used to surface only at the last pipeline legs:
#   - gh CLI auth (release-publish creates the GitHub Release page)
#   - MCP Registry JWT (registry-publish; expires within hours, and its
#     GitHub device-code recovery needs a human at a browser)
# Checking them first means the interactive device-code login happens at
# minute 0 with the operator present, not after tag+push when a strand
# leaves the registry leg to heal by hand (v2.0.0 stranded twice there).

set -euo pipefail

publisher="${1:?usage: release-auth-preflight.sh <mcp-publisher> [login-method]}"
login_method="${2:-github}"
min_valid_minutes="${REGISTRY_TOKEN_MIN_VALID_MINUTES:-30}"

fail() { printf 'release-auth-preflight: %s\n' "$1" >&2; exit 1; }
note() { printf 'release-auth-preflight: %s\n' "$1"; }

command -v gh >/dev/null 2>&1 || fail "gh CLI not on PATH; brew install gh"
if ! gh auth status >/dev/null 2>&1; then
    fail "gh auth is invalid or expired — run 'gh auth login', then retry"
fi
note "gh auth OK"

token_file="${XDG_CONFIG_HOME:-$HOME/.config}/mcp-publisher/token.json"

registry_jwt_remaining_minutes() {
    # Prints whole minutes of validity left on the stored registry JWT
    # (negative when expired); nonzero exit when missing or unreadable.
    python3 - "$token_file" <<'PY'
import base64, json, sys, time
try:
    with open(sys.argv[1]) as f:
        jwt = json.load(f)["token"]
    payload = jwt.split(".")[1]
    claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
    exp = int(claims["exp"])
except Exception as exc:
    print(f"registry token unreadable: {exc}", file=sys.stderr)
    sys.exit(1)
print(int((exp - time.time()) // 60))
PY
}

remaining="$(registry_jwt_remaining_minutes)" || remaining=""
if [ -n "$remaining" ] && [ "$remaining" -ge "$min_valid_minutes" ]; then
    note "registry JWT OK (${remaining}m left, need ${min_valid_minutes}m)"
    exit 0
fi

if [ -n "$remaining" ]; then
    note "registry JWT has ${remaining}m left (need ${min_valid_minutes}m) — refresh required"
else
    note "registry JWT missing or unreadable at $token_file — refresh required"
fi

if [ ! -t 0 ]; then
    fail "no TTY for the interactive '$login_method' device-code login — run 'make registry-login' first, then retry"
fi

"$publisher" login "$login_method"

remaining="$(registry_jwt_remaining_minutes)" || fail "login completed but the token at $token_file is still unreadable"
if [ "$remaining" -lt "$min_valid_minutes" ]; then
    fail "refreshed token already has only ${remaining}m validity (need ${min_valid_minutes}m)"
fi
note "registry JWT refreshed (${remaining}m left)"
