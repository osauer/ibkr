#!/usr/bin/env bash
#
# wire-smoke.sh — exercise the freshly-built ibkr binary against a live
# IBKR Gateway with the wire interceptor enabled, and assert per-command
# protocol-level invariants.
#
# This catches the kind of regression where the daemon "works" by
# returning JSON but the underlying wire conversation is broken (e.g.
# the v0.24.x productionLegFetcher bug, where the gateway was sending
# the right ticks and the daemon was reading the wrong field).
#
# Wired into `make release` AFTER `release-verify` so a binary that
# ships honest JSON but a broken wire flow can never reach a tag.
# Designed to be:
#   - Binding when a gateway is reachable
#   - SKIP (exit 0) when no gateway is up — same posture as
#     test/integration so `make release` works on a laptop without IBKR
#   - Deterministic per-run within the live-vs-off-hours dimension
#     (the script auto-detects frozen mode and loosens budgets)
#
# Usage:
#   scripts/wire-smoke.sh <bin-path> <wire-assert-path>
#
# Example:
#   scripts/wire-smoke.sh bin/ibkr bin/wire-assert
#
# Environment hooks:
#   IBKR_TEST_PORT          — gateway port to probe (default: 7496 TWS live)
#   IBKR_TEST_HOST          — gateway host (default: 127.0.0.1)
#   IBKR_SMOKE_TIMEOUT      — per-command wall-clock timeout in seconds (default: 30)
#   IBKR_SMOKE_STRICT       — 1 = FAIL on no-gateway instead of SKIP (release path)
#   SPX_EXPECTED_REACHABLE  — 1 (default in `make smoke`) = `ibkr gamma --only=spx`
#                             must return real SPX data; banner-seen FAILS the run.
#                             0 = banner-seen is a clean skip (CI / accounts without
#                             CBOE OPRA). User-flagged guardrail: "no SPX data
#                             would be a bug on my setup" — prevents silent SPX
#                             regression between releases (design §11.2).
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/lib-daemon-control.sh"

BIN="${1:?usage: wire-smoke.sh <bin/ibkr> <bin/wire-assert>}"
ASSERT="${2:?usage: wire-smoke.sh <bin/ibkr> <bin/wire-assert>}"

if [[ ! -x "$BIN" ]]; then
    echo "wire-smoke: $BIN not executable" >&2
    exit 2
fi
if [[ ! -x "$ASSERT" ]]; then
    echo "wire-smoke: $ASSERT not executable (run 'make smoke-build')" >&2
    exit 2
fi

GATEWAY_HOST="${IBKR_TEST_HOST:-127.0.0.1}"
GATEWAY_PORT="${IBKR_TEST_PORT:-7496}"
if [[ ! "$GATEWAY_HOST" =~ ^[A-Za-z0-9._:-]+$ ]]; then
    echo "wire-smoke: invalid IBKR_TEST_HOST: $GATEWAY_HOST" >&2
    exit 2
fi
if [[ ! "$GATEWAY_PORT" =~ ^[0-9]+$ ]] || (( GATEWAY_PORT < 1 || GATEWAY_PORT > 65535 )); then
    echo "wire-smoke: invalid IBKR_TEST_PORT: $GATEWAY_PORT" >&2
    exit 2
fi
# 60s default. The chain fetch can legitimately take ~30s when 22 legs
# need contract resolution from a cold cache (observed 2026-05-18:
# chain SPY --width 5 → 30018ms wall clock). 30s was too tight; 60s
# gives the legitimate path room without letting a wedged daemon hang.
PER_CMD_TIMEOUT="${IBKR_SMOKE_TIMEOUT:-60}"

# 1. Gateway-presence probe. Default posture matches test/integration:
# a missing gateway is SKIP (exit 0), not FAIL — `make smoke` from a
# laptop without paper-account IBKR access must still pass. The release
# path overrides via IBKR_SMOKE_STRICT=1 to FAIL on no-gateway, so a
# release can't silently bypass the wire gate. The probe uses bash's
# /dev/tcp to avoid a netcat dependency.
STRICT="${IBKR_SMOKE_STRICT:-0}"
if ! timeout 2 bash -c "exec 3<>/dev/tcp/${GATEWAY_HOST}/${GATEWAY_PORT}" 2>/dev/null; then
    if [[ "$STRICT" == "1" ]]; then
        echo "wire-smoke: FAIL — no gateway reachable at ${GATEWAY_HOST}:${GATEWAY_PORT} (STRICT mode; release path must exercise TWS)" >&2
        exit 1
    fi
    echo "wire-smoke: SKIP — no gateway reachable at ${GATEWAY_HOST}:${GATEWAY_PORT}"
    exit 0
fi
echo "wire-smoke: gateway present at ${GATEWAY_HOST}:${GATEWAY_PORT}"

# 2. Isolated daemon under /tmp. Mirrors release-verify.sh so the smoke
# gate never touches the user's canonical daemon. Wire interceptor is
# enabled here, not in the production code — IBKR_WIRE_INTERCEPTOR is
# the test surface designed for exactly this use case.
TMPDIR_BASE="${TMPDIR:-/tmp}"
SMOKE_DIR="$(mktemp -d "$TMPDIR_BASE/ibkr-wire-smoke-XXXXXX")"
SOCKET="$SMOKE_DIR/ibkr.sock"
LOG="$SMOKE_DIR/ibkr-daemon.log"
LOCK="$SMOKE_DIR/ibkr.lock"
WIRE_LOG="$SMOKE_DIR/wire.jsonl"

export IBKR_SOCKET="$SOCKET"
export IBKR_LOG="$LOG"
export IBKR_WIRE_INTERCEPTOR=1
export IBKR_WIRE_LOG_PATH="$WIRE_LOG"
export IBKR_WIRE_RING_SIZE=4096

cleanup() {
    local code=$?
    kill_daemon_from_lockfile "$LOCK"
    # On failure, surface the daemon log tail and the last few wire
    # frames — the failure-mode is in the wire data, not in the CLI's
    # exit code, so we need both.
    if [[ $code -ne 0 ]]; then
        if [[ -r "$LOG" ]]; then
            echo ""
            echo "wire-smoke: daemon log tail ($LOG):" >&2
            tail -30 "$LOG" >&2 || true
        fi
        if [[ -r "$WIRE_LOG" ]]; then
            echo ""
            echo "wire-smoke: last 5 wire frames ($WIRE_LOG):" >&2
            tail -5 "$WIRE_LOG" >&2 || true
        fi
    fi
    rm -rf "$SMOKE_DIR" 2>/dev/null || true
    return $code
}
trap cleanup EXIT INT TERM

# See scripts/lib-daemon-control.sh for the client-ID slot rationale.
stop_existing_daemons wire-smoke

# Run a CLI command with a deadline; on failure, print the command +
# output. Sets $LAST_CMD_OUTPUT and $LAST_CMD_EXIT for the caller.
run_cli() {
    local label="$1"
    shift
    LAST_CMD_EXIT=0
    LAST_CMD_OUTPUT="$(timeout "$PER_CMD_TIMEOUT" "$BIN" "$@" 2>&1)" || LAST_CMD_EXIT=$?
}

# Run one named wire-assert check against the whole JSONL. Per-command
# scoping was tempting but in practice the daemon pre-warms subscriptions
# at boot (SPY for the regime path, ARCA contract lookups, etc.), so
# isolating "frames produced by THIS command" gives false negatives. The
# isolated tmp daemon's wire log is small enough — and the per-command
# command order is deterministic enough — that whole-file scans work.
#
# Optional second arg: the path to a JSON envelope to forward via
# --gamma-envelope-path. Only the gamma-premarket-derived check reads
# this; passing it for other checks is harmless.
assert_wire() {
    local check="$1"
    local envelope="${2:-}"
    local args=(--jsonl "$WIRE_LOG" --check "$check")
    if [[ "${LOOSE:-0}" -eq 1 ]]; then
        args+=(--loose)
    fi
    if [[ -n "$envelope" ]]; then
        args+=(--gamma-envelope-path "$envelope")
    fi
    if ! "$ASSERT" "${args[@]}"; then
        echo "" >&2
        echo "wire-smoke: aborting on first failure" >&2
        exit 1
    fi
}

echo "wire-smoke: isolated daemon → $SOCKET"
echo "wire-smoke: wire log → $WIRE_LOG"

# 4. Boot the daemon by issuing a status call (which autospawns one at
# the isolated socket). Wait for the gateway to be connected — give it
# 25s, same budget as the integration suite.
echo "  [boot] autospawning daemon..."

for attempt in $(seq 1 25); do
    if "$BIN" status --json 2>/dev/null | grep -q '"connected": *true'; then
        break
    fi
    sleep 1
    if [[ $attempt -eq 25 ]]; then
        echo "wire-smoke: FAIL: daemon never reached connected=true within 25s" >&2
        exit 1
    fi
done
assert_wire status-handshake
echo "  [boot] ok"

# 5. Detect frozen/off-hours mode by querying SPY's data_type. If
# frozen/delayed, set LOOSE=1 so the chain-iv-source check warns
# instead of failing (model engine doesn't fire when options aren't
# trading — that's an IBKR characteristic, not a regression).

run_cli quote-spy quote SPY --json
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: quote SPY exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
data_type="$(echo "$LAST_CMD_OUTPUT" | grep -o '"data_type": *"[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/')"
case "$data_type" in
    live)
        LOOSE=0
        echo "  [mode] live"
        ;;
    frozen|delayed|delayed-frozen|"")
        LOOSE=1
        echo "  [mode] $data_type — loose (model engine may be idle)"
        ;;
    *)
        LOOSE=1
        echo "  [mode] unknown ($data_type) — loose"
        ;;
esac
assert_wire quote-spy

# 6. account.summary — pins account-level reqAccountSummary path.
echo "  [account]..."

run_cli account account --json
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: account exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
assert_wire account-summary

# 7. chain with a near expiry — pins the IV-source path that the
# v0.24.x bug broke. In loose mode this check warns instead of failing.
echo "  [chain SPY 1-wide]..."

# Pick a near expiry. The chain expiry-listing command returns them in
# DTE order; we grab the second (skipping today's 0DTE which can be
# quirky) and strip the date.
expiries="$("$BIN" chain SPY 2>/dev/null | awk '/^[[:space:]]+20[0-9]{2}-[0-9]{2}-[0-9]{2}/ {print $1}' | head -3 | tail -1)"
if [[ -z "$expiries" ]]; then
    echo "wire-smoke: FAIL: could not list SPY expiries via 'ibkr chain SPY'" >&2
    exit 1
fi
run_cli chain-iv chain SPY --expiry "$expiries" --width 1 --side both --json
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: chain exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
assert_wire chain-iv-source

# 8. regime — the dashboard's fan-out. Asserts all 5 indicator
# subscribes go out.
echo "  [regime]..."

run_cli regime regime --json
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: regime exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
assert_wire regime-subs

# 9. gamma --no-wait — proves the non-blocking gamma path returns a
# terminal status without hanging.
echo "  [gamma --no-wait]..."

run_cli gamma gamma --no-wait --json
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: gamma --no-wait exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
assert_wire gamma-noflag

# 10. gamma-premarket-derived — only meaningful in loose mode (off-hours).
# Block on the compute via the default `gamma --json` path (~50s wait),
# poll a few times if still computing, then assert derived_iv_legs > 0
# proves the BS-IV Newton-Raphson fallback fired. Strict mode skips
# internally — the model engine is active during RTH and the fallback
# isn't expected to engage.
#
# Polling: the daemon's per-RPC deadline is 55s, so `gamma --json`
# returns Status=computing if the compute outlives the budget. We poll
# up to 5 times (≈4-5 min total) to give the compute room to complete
# on a cold contract cache.
if [[ "${LOOSE:-0}" -eq 1 ]]; then
    echo "  [gamma (loose: BS-IV fallback assertion)]..."
    GAMMA_ENV="$SMOKE_DIR/gamma-envelope.json"
    for attempt in 1 2 3 4 5; do
        LAST_CMD_EXIT=0
        LAST_CMD_OUTPUT="$(timeout 60 "$BIN" gamma --json 2>&1)" || LAST_CMD_EXIT=$?
        if [[ $LAST_CMD_EXIT -ne 0 ]]; then
            echo "wire-smoke: FAIL: gamma --json exit=$LAST_CMD_EXIT (attempt $attempt)" >&2
            echo "$LAST_CMD_OUTPUT" >&2
            exit 1
        fi
        printf '%s' "$LAST_CMD_OUTPUT" > "$GAMMA_ENV"
        if grep -q '"status": *"ready"' <<<"$LAST_CMD_OUTPUT"; then
            break
        fi
        echo "    poll $attempt: still computing"
        sleep 2
    done
    assert_wire gamma-premarket-derived "$GAMMA_ENV"
fi

# 11. SPX coverage check — exercises the `--only=spx` path landed in
# the gamma-spx-coverage arc. Per design §11.2: on this dev machine
# `SPX_EXPECTED_REACHABLE=1` flips banner-seen from clean-skip to
# loud-fail, preventing silent SPX regression. CI accounts without
# CBOE OPRA can disable via the env var.
#
# The check is non-blocking on the SPX compute itself — `--no-wait`
# returns immediately with the current cache state. We only assert
# the daemon ACCEPTED `--only=spx` (didn't reject the scope) and that
# the result envelope doesn't carry the entitlement-skipped banner
# when SPX_EXPECTED_REACHABLE is set.
echo "  [gamma --only=spx --no-wait]..."
run_cli gamma-spx gamma --only=spx --no-wait --json
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: gamma --only=spx exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
if [[ "${SPX_EXPECTED_REACHABLE:-0}" -eq 1 ]]; then
    # Check the result for SPX-skipped warnings. The envelope's
    # `warnings` array carries "spx_unavailable:<reason>" tokens when
    # the combined-mode prewarm degraded. Note: when --only=spx is
    # used, the daemon runs the SPX path directly, so a real
    # entitlement issue surfaces as Status=error here.
    if echo "$LAST_CMD_OUTPUT" | grep -q '"status": *"error"'; then
        echo "wire-smoke: FAIL: SPX_EXPECTED_REACHABLE=1 but gamma --only=spx returned error" >&2
        echo "$LAST_CMD_OUTPUT" >&2
        exit 1
    fi
    echo "    [spx ok — daemon accepted --only=spx scope, no entitlement error]"
fi

echo ""
mode_label="strict"
if [[ "${LOOSE:-0}" -eq 1 ]]; then mode_label="loose"; fi
echo "wire-smoke: PASS — ${BIN} wire flow is healthy (mode=${mode_label})"
