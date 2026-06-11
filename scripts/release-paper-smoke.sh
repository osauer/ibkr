#!/usr/bin/env bash
#
# release-paper-smoke.sh — binding release gate: run the daemon-observed
# paper order round-trip (`ibkr trading paper-smoke`: place 1-share
# far-off-market SPY LMT → broker ack → cancel → cancel confirm) against
# an isolated daemon pinned to the local *paper* TWS/Gateway session.
#
# Wired into `make release` at version bump (2026-06-10 decision): the
# order pipeline is verified automatically per release instead of by a
# human-certified runtime gate. The gate is BINDING — there is no SKIP:
#   - no paper session reachable  → release aborts (log the paper account
#     in first; a live session is never used for the smoke)
#   - non-DU account on the paper port → release aborts
#   - smoke result != passed      → release aborts
#
# Usage:
#   scripts/release-paper-smoke.sh <bin/ibkr>
#
# Environment hooks:
#   IBKR_TEST_HOST        — gateway host (default 127.0.0.1)
#   IBKR_PAPER_PORTS      — space-separated paper probe ports (default "4002 7497")
#   IBKR_SMOKE_CLIENT_ID  — client ID for the isolated daemon (default derived)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/lib-daemon-control.sh"

BIN="${1:?usage: release-paper-smoke.sh <bin/ibkr>}"
if [[ ! -x "$BIN" ]]; then
    echo "release-paper-smoke: $BIN not executable" >&2
    exit 2
fi

HOST="${IBKR_TEST_HOST:-127.0.0.1}"
# Paper ports ONLY (Gateway 4002, TWS 7497). The smoke transmits a real
# order; the live ports are deliberately not probe candidates.
read -r -a probe_ports <<<"${IBKR_PAPER_PORTS:-4002 7497}"

PORT=""
for port in "${probe_ports[@]}"; do
    if timeout 2 bash -c "exec 3<>/dev/tcp/${HOST}/${port}" 2>/dev/null; then
        PORT="$port"
        break
    fi
done
if [[ -z "$PORT" ]]; then
    echo "release-paper-smoke: FAIL — no paper TWS/Gateway reachable at ${HOST} ports ${probe_ports[*]}." >&2
    echo "  The release gate transmits a 1-share paper round-trip and refuses to run against live." >&2
    echo "  Log TWS (7497) or IB Gateway (4002) into the paper account, then re-run \`make release\`." >&2
    exit 1
fi
echo "release-paper-smoke: paper gateway present at ${HOST}:${PORT}"

CLIENT_ID="${IBKR_SMOKE_CLIENT_ID:-$((300 + ($$ % 600)))}"

TMPDIR_BASE="${TMPDIR:-/tmp}"
SMOKE_DIR="$(mktemp -d "$TMPDIR_BASE/ibkr-paper-smoke-XXXXXX")"
SOCKET="$SMOKE_DIR/ibkr.sock"
LOG="$SMOKE_DIR/ibkr-daemon.log"
LOCK="$SMOKE_DIR/ibkr.lock"
CONFIG="$SMOKE_DIR/config.toml"

export IBKR_SOCKET="$SOCKET"
export IBKR_LOG="$LOG"
export IBKR_CONFIG="$CONFIG"
# Isolated trading state: evidence, journal, and tokens must not touch the
# user's canonical daemon state.
export XDG_STATE_HOME="$SMOKE_DIR/state"
export XDG_CACHE_HOME="$SMOKE_DIR/cache"

cleanup() {
    local code=$?
    kill_daemon_from_lockfile "$LOCK"
    if [[ $code -ne 0 && -r "$LOG" ]]; then
        echo "" >&2
        echo "release-paper-smoke: daemon log tail ($LOG):" >&2
        tail -25 "$LOG" >&2 || true
    fi
    rm -rf "$SMOKE_DIR" 2>/dev/null || true
    return $code
}
trap cleanup EXIT INT TERM

# Phase 1 — data-only daemon pinned to the paper port: discover the
# concrete paper account (order gates need a pinned non-aggregate account).
cat > "$CONFIG" <<EOF
[gateway]
host = "$HOST"
port = $PORT
client_id = $CLIENT_ID
tls = false
EOF

# `account --json` reports the aggregate "All" summary on an unpinned
# daemon; `status --json` carries the concrete session account. The field
# stays empty until the gateway handshake completes, so poll like
# release-smoke.sh does for status.connected.
ACCOUNT=""
for _ in $(seq 1 100); do
    ACCOUNT="$(timeout 30 "$BIN" status --json | python3 -c 'import json,sys; print(json.load(sys.stdin).get("connected_account",""))')"
    [[ -n "$ACCOUNT" ]] && break
    sleep 0.25
done
if [[ -z "$ACCOUNT" ]]; then
    echo "release-paper-smoke: FAIL — could not resolve the connected account on ${HOST}:${PORT} after 25s" >&2
    exit 1
fi
case "$ACCOUNT" in
    DU*|du*) ;;
    *)
        echo "release-paper-smoke: FAIL — account '$ACCOUNT' on ${HOST}:${PORT} is not a paper (DU) account; refusing to transmit" >&2
        exit 1
        ;;
esac
echo "release-paper-smoke: paper account $ACCOUNT"

# Phase 2 — restart the isolated daemon with the trading gate pinned to
# that account, then run the smoke through the production order path.
kill_daemon_from_lockfile "$LOCK"
cat > "$CONFIG" <<EOF
[gateway]
host = "$HOST"
port = $PORT
client_id = $CLIENT_ID
account = "$ACCOUNT"
tls = false

[trading]
mode = "paper"
EOF

# Fresh autospawned daemon: wait for the gateway handshake before the
# smoke, or its reference-quote leg fails fast with gateway_unavailable.
CONNECTED=""
for _ in $(seq 1 100); do
    CONNECTED="$(timeout 30 "$BIN" status --json | python3 -c 'import json,sys; print(json.load(sys.stdin).get("connected",False))')"
    [[ "$CONNECTED" == "True" ]] && break
    sleep 0.25
done
if [[ "$CONNECTED" != "True" ]]; then
    echo "release-paper-smoke: FAIL — daemon did not connect to ${HOST}:${PORT} within 25s" >&2
    exit 1
fi

OUT="$(timeout 150 "$BIN" trading paper-smoke --json)" || {
    echo "release-paper-smoke: FAIL — paper-smoke command errored:" >&2
    echo "$OUT" >&2
    exit 1
}
RESULT="$(printf '%s' "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("result",""))')"
if [[ "$RESULT" != "passed" ]]; then
    echo "release-paper-smoke: FAIL — smoke result '$RESULT' (want passed):" >&2
    printf '%s\n' "$OUT" | python3 -m json.tool >&2 || printf '%s\n' "$OUT" >&2
    exit 1
fi
ORDER_REF="$(printf '%s' "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("order_ref",""))')"
echo "release-paper-smoke: PASS — order pipeline round-trip confirmed on $ACCOUNT (${ORDER_REF:-no ref})"
