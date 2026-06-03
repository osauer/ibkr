#!/usr/bin/env bash
#
# registry-publish-with-login.sh - publish MCP Registry metadata and recover
# from the common expired-JWT case with the publisher's interactive login flow.

set -euo pipefail

publisher="${1:?usage: registry-publish-with-login.sh <mcp-publisher> <server.json>}"
server_json="${2:?server.json path required}"
auto_login="${MCP_REGISTRY_AUTO_LOGIN:-1}"
login_method="${MCP_REGISTRY_LOGIN_METHOD:-github}"

if [[ ! -f "$server_json" ]]; then
    echo "registry-publish: missing $server_json" >&2
    exit 2
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

if "$publisher" publish "$server_json" >"$tmp" 2>&1; then
    cat "$tmp"
    exit 0
fi
status=$?
cat "$tmp" >&2

if [[ "$auto_login" != "1" ]]; then
    exit "$status"
fi
if ! grep -Eiq 'unauthorized|not logged in|login|jwt|token.*expired|expired.*token|invalid.*token|token.*invalid' "$tmp"; then
    exit "$status"
fi

cat >&2 <<EOF

registry-publish: MCP Registry auth appears expired.
registry-publish: starting '$(basename "$publisher") login $login_method'.

For GitHub device flow:
  1. Open the URL printed by mcp-publisher.
  2. Enter the printed device code.
  3. Authorize the registry publisher.
  4. Leave this terminal running; publish will retry automatically.

Set MCP_REGISTRY_AUTO_LOGIN=0 to disable this retry behavior.

EOF

"$publisher" login "$login_method"
"$publisher" publish "$server_json"
