#!/usr/bin/env bash
#
# release-auth-preflight.sh - fail-fast auth checks before the release
# pipeline spends ~10 minutes on gates. What can actually be verified at
# minute 0:
#   - gh CLI auth (release-publish creates the GitHub Release page); goes
#     stale between releases and used to surface only at the last legs.
#   - The registry leg's preconditions. MCP Registry JWTs from the GitHub
#     device-code flow live only ~5 minutes (observed v2.1.0, 2026-07-18;
#     originally assumed hours), so a stored token is always dead by the
#     registry-publish leg and gating on residual validity — or refreshing
#     here — is meaningless. The real credential mint is the device-code
#     login that registry-publish-with-login.sh runs AT the publish leg;
#     this preflight verifies that backstop is armed (publisher binary
#     present, MCP_REGISTRY_AUTO_LOGIN not disabled) and reminds the
#     operator to be at a browser near the END of the pipeline (v2.0.0
#     stranded twice when nobody was).

set -euo pipefail

publisher="${1:?usage: release-auth-preflight.sh <mcp-publisher> [login-method]}"
login_method="${2:-github}"
auto_login="${MCP_REGISTRY_AUTO_LOGIN:-1}"

fail() { printf 'release-auth-preflight: %s\n' "$1" >&2; exit 1; }
note() { printf 'release-auth-preflight: %s\n' "$1"; }

command -v gh >/dev/null 2>&1 || fail "gh CLI not on PATH; brew install gh"
if ! gh auth status >/dev/null 2>&1; then
    fail "gh auth is invalid or expired — run 'gh auth login', then retry"
fi
note "gh auth OK"

command -v "$publisher" >/dev/null 2>&1 \
    || fail "mcp-publisher not found at '$publisher' — the registry-publish leg would strand"

if [ "$auto_login" != "1" ]; then
    fail "MCP_REGISTRY_AUTO_LOGIN=0: registry JWTs live only ~5 minutes, so without the publish-leg auto-login every release strands at registry-publish — drop the override"
fi

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

# Stored-token state is informational only: with ~5-minute JWTs no stored
# token survives to the registry leg, so nothing here gates the release.
if remaining="$(registry_jwt_remaining_minutes 2>/dev/null)"; then
    if [ "$remaining" -gt 0 ]; then
        note "stored registry JWT has ${remaining}m left — it will still be expired by the registry-publish leg"
    else
        note "stored registry JWT expired $((-remaining))m ago (normal: registry JWTs live ~5 minutes)"
    fi
else
    note "no readable registry JWT at $token_file (normal between releases)"
fi

note "REMINDER: registry-publish runs '$(basename "$publisher") login $login_method' near the END of the pipeline — be at a browser then; the device code expires in ~1 minute"
