#!/usr/bin/env bash
#
# release-smoke.sh - run the release JSON contract checks and wire-level
# invariants against one isolated live-gateway daemon.
#
# This folds the release path's former `release-verify` + `smoke-only`
# sequence into a single daemon session. That keeps the same quality
# gates while avoiding the second daemon bounce, second TWS client-ID
# cooldown, and duplicate command matrix.
#
# Usage:
#   scripts/release-smoke.sh <bin-path> <expected-version> <wire-assert-path>

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/lib-daemon-control.sh"

BIN="${1:?usage: release-smoke.sh <bin/ibkr> <expected-version> <bin/wire-assert>}"
EXPECTED="${2:?expected version required, e.g. v0.15.1}"
ASSERT="${3:?wire-assert path required, e.g. bin/wire-assert}"

if [[ ! -x "$BIN" ]]; then
    echo "release-smoke: $BIN not executable" >&2
    exit 2
fi
if [[ ! -x "$ASSERT" ]]; then
    echo "release-smoke: $ASSERT not executable (run 'make smoke-build')" >&2
    exit 2
fi

GATEWAY_HOST="${IBKR_TEST_HOST:-127.0.0.1}"
GATEWAY_PORT="${IBKR_TEST_PORT:-7496}"
STRICT="${IBKR_SMOKE_STRICT:-0}"
JSON_TIMEOUT="${IBKR_RELEASE_VERIFY_TIMEOUT:-15}"
WIRE_TIMEOUT="${IBKR_SMOKE_TIMEOUT:-60}"

python_extract='
import json, sys
d = json.load(sys.stdin)
for k in sys.argv[1].split("."):
    if not isinstance(d, dict):
        print(""); sys.exit(0)
    v = d.get(k)
    if v is None:
        print(""); sys.exit(0)
    d = v
print(d)
'

python_haskey='
import json, sys
d = json.load(sys.stdin)
sys.exit(0 if sys.argv[1] in d else 1)
'

json_field() {
    local path="$1"
    local input="$2"
    printf '%s' "$input" | python3 -c "$python_extract" "$path"
}

json_has_key() {
    local key="$1"
    local input="$2"
    printf '%s' "$input" | python3 -c "$python_haskey" "$key"
}

run_cli() {
    local label="$1"
    local timeout_seconds="$2"
    shift 2
    if ! out="$(timeout "$timeout_seconds" "$BIN" "$@" 2>&1)"; then
        echo "release-smoke: FAIL [$label]: '$BIN $*' exited non-zero (or timed out at ${timeout_seconds}s)" >&2
        echo "$out" >&2
        exit 1
    fi
    printf '%s' "$out"
}

wire_offset() {
    if [[ -r "$WIRE_LOG" ]]; then
        wc -c < "$WIRE_LOG" | tr -d '[:space:]'
    else
        printf '0'
    fi
}

run_wire_cli() {
    local label="$1"
    local timeout_seconds="$2"
    shift 2
    LAST_WIRE_OFFSET="$(wire_offset)"
    LAST_CMD_OUTPUT="$(run_cli "$label" "$timeout_seconds" "$@")"
}

assert_wire() {
    local check="$1"
    local offset="$2"
    local envelope="${3:-}"
    local args=(--jsonl "$WIRE_LOG" --since-offset "$offset" --check "$check")
    if [[ "${LOOSE:-0}" -eq 1 ]]; then
        args+=(--loose)
    fi
    if [[ -n "$envelope" ]]; then
        args+=(--gamma-envelope-path "$envelope")
    fi
    if ! "$ASSERT" "${args[@]}"; then
        echo "" >&2
        echo "release-smoke: aborting on first wire assertion failure" >&2
        exit 1
    fi
}

echo "release-smoke: smoke matrix against $BIN expecting $EXPECTED"

# Version is offline and cheap; fail here before probing TWS so an
# accidentally dirty or unstamped binary is obvious even on a laptop
# without a gateway.
echo "  [1] version stamp..."
version_json="$(run_cli version "$JSON_TIMEOUT" version --json)"
actual_version="$(json_field version "$version_json")"
if [[ "$actual_version" != "$EXPECTED" ]]; then
    echo "release-smoke: FAIL: binary stamps version=$actual_version, expected=$EXPECTED" >&2
    echo "(the ldflags in 'make build' must agree with the release tag)" >&2
    exit 1
fi

if ! timeout 2 bash -c "exec 3<>/dev/tcp/${GATEWAY_HOST}/${GATEWAY_PORT}" 2>/dev/null; then
    if [[ "$STRICT" == "1" ]]; then
        echo "release-smoke: FAIL - no gateway reachable at ${GATEWAY_HOST}:${GATEWAY_PORT} (STRICT mode; release path must exercise TWS)" >&2
        exit 1
    fi
    echo "release-smoke: SKIP - no gateway reachable at ${GATEWAY_HOST}:${GATEWAY_PORT}"
    exit 0
fi
echo "release-smoke: gateway present at ${GATEWAY_HOST}:${GATEWAY_PORT}"

TMPDIR_BASE="${TMPDIR:-/tmp}"
SMOKE_DIR="$(mktemp -d "$TMPDIR_BASE/ibkr-release-smoke-XXXXXX")"
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
    if [[ $code -ne 0 ]]; then
        if [[ -r "$LOG" ]]; then
            echo ""
            echo "release-smoke: daemon log tail ($LOG):" >&2
            tail -40 "$LOG" >&2 || true
        fi
        if [[ -r "$WIRE_LOG" ]]; then
            echo ""
            echo "release-smoke: last 8 wire frames ($WIRE_LOG):" >&2
            tail -8 "$WIRE_LOG" >&2 || true
        fi
    fi
    rm -rf "$SMOKE_DIR" 2>/dev/null || true
    return $code
}
trap cleanup EXIT INT TERM

echo "release-smoke: isolated daemon -> $SOCKET"
echo "release-smoke: wire log -> $WIRE_LOG"

stop_existing_daemons release-smoke

echo "  [2] status (autospawn daemon at isolated socket)..."
boot_offset="$(wire_offset)"
status_json=""
connected=""
daemon_version=""
for attempt in $(seq 1 25); do
    status_json="$(run_cli status "$JSON_TIMEOUT" status --json)"
    connected="$(json_field connected "$status_json")"
    daemon_version="$(json_field daemon_version "$status_json")"
    if [[ "$connected" == "True" ]]; then
        break
    fi
    sleep 1
    if [[ "$attempt" -eq 25 ]]; then
        echo "release-smoke: FAIL: gateway not reachable after 25s of polling (status.connected=$connected)" >&2
        echo "$status_json" >&2
        exit 1
    fi
done
if [[ "$daemon_version" != "$EXPECTED" ]]; then
    echo "release-smoke: FAIL: daemon stamped version=$daemon_version, expected=$EXPECTED" >&2
    echo "(autospawn picked up an unexpected binary - check \$PATH and bin/ibkr)" >&2
    exit 1
fi
bg_check="$(printf '%s' "$status_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
tasks = d.get("background_tasks", None)
if tasks is None:
    print("background_tasks missing - must be emitted (even when empty)")
    sys.exit(0)
if not isinstance(tasks, list):
    print("background_tasks=" + repr(tasks) + ", want list")
    sys.exit(0)
for t in tasks:
    if not isinstance(t, dict) or not isinstance(t.get("name"), str):
        print("background_tasks entry malformed: " + repr(t))
        sys.exit(0)
print("ok (" + str(len(tasks)) + " task(s))")
')"
if [[ "$bg_check" != ok* ]]; then
    echo "release-smoke: FAIL: status.background_tasks: $bg_check" >&2
    echo "$status_json" >&2
    exit 1
fi
assert_wire status-handshake "$boot_offset"
echo "    status.background_tasks: $bg_check"

echo "  [3] account.summary..."
run_wire_cli account "$JSON_TIMEOUT" account --json
account_json="$LAST_CMD_OUTPUT"
account_id="$(json_field account_id "$account_json")"
if [[ -z "$account_id" ]]; then
    echo "release-smoke: FAIL: account_id missing from account.summary response" >&2
    echo "$account_json" >&2
    exit 1
fi
if json_has_key data_type "$account_json"; then
    echo "release-smoke: FAIL: account.summary leaked data_type field (v0.15 must omit it)" >&2
    echo "$account_json" >&2
    exit 1
fi
assert_wire account-summary "$LAST_WIRE_OFFSET"

echo "  [4] positions.list..."
positions_json="$(run_cli positions "$JSON_TIMEOUT" positions --json)"
positions_shape_ok="$(printf '%s' "$positions_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("ok" if isinstance(d.get("stocks"), list) and isinstance(d.get("options"), list) else "bad")
')"
if [[ "$positions_shape_ok" != "ok" ]]; then
    echo "release-smoke: FAIL: positions.list missing stocks/options arrays" >&2
    echo "$positions_json" >&2
    exit 1
fi
if json_has_key data_type "$positions_json"; then
    echo "release-smoke: FAIL: positions.list leaked data_type field (v0.15 must omit it)" >&2
    echo "$positions_json" >&2
    exit 1
fi

echo "  [5] quote SPY..."
run_wire_cli quote_SPY "$JSON_TIMEOUT" quote SPY --json
quote_json="$LAST_CMD_OUTPUT"
quote_check="$(printf '%s' "$quote_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
sym = d.get("symbol")
if sym != "SPY":
    print("symbol=" + repr(sym) + ", want SPY")
    sys.exit(0)
dt = d.get("data_type", "")
valid = {"live", "delayed", "frozen", "delayed-frozen"}
if dt not in valid:
    print("data_type=" + repr(dt) + ", want one of " + str(sorted(valid)))
    sys.exit(0)
if not any(d.get(k) is not None for k in ("bid", "ask", "last", "mark")):
    print("quote has no bid/ask/last/mark price; refusing all-empty successful snapshot")
    sys.exit(0)
vol = d.get("volume")
if vol is not None and vol > 1_000_000_000:
    print("volume=" + repr(vol) + " exceeds 1B shares; likely IBKR Decimal size not normalised")
    sys.exit(0)
print("ok")
')"
if [[ "$quote_check" != "ok" ]]; then
    echo "release-smoke: FAIL: quote SPY: $quote_check" >&2
    echo "$quote_json" >&2
    exit 1
fi
data_type="$(json_field data_type "$quote_json")"
case "$data_type" in
    live)
        LOOSE=0
        echo "    mode: live"
        ;;
    frozen|delayed|delayed-frozen|"")
        LOOSE=1
        echo "    mode: $data_type - loose (model engine may be idle)"
        ;;
    *)
        LOOSE=1
        echo "    mode: unknown ($data_type) - loose"
        ;;
esac
assert_wire quote-spy "$LAST_WIRE_OFFSET"

echo "  [6] breadth.spx state..."
breadth_json="$(run_cli breadth "$JSON_TIMEOUT" breadth --json)"
breadth_check="$(printf '%s' "$breadth_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
state = d.get("state")
value = d.get("pct_above_50dma", 0)
valid_states = {"cold", "computing", "ready", "degraded"}
if state not in valid_states:
    print("state=" + repr(state) + ", want one of " + str(sorted(valid_states)))
    sys.exit(0)
if state == "ready" and value <= 0:
    print("state=ready but pct_above_50dma=" + repr(value) + " - successful finalise must produce non-zero S5FI")
    sys.exit(0)
if state == "cold" and value != 0:
    print("state=cold but pct_above_50dma=" + repr(value) + " - engine that never ran cannot have a value")
    sys.exit(0)
print("ok (state=" + state + " pct_above_50dma=" + repr(value) + ")")
')"
if [[ "$breadth_check" != ok* ]]; then
    echo "release-smoke: FAIL: breadth.spx: $breadth_check" >&2
    echo "$breadth_json" >&2
    exit 1
fi
echo "    $breadth_check"

echo "  [7] regime call-sequence (two scoped rounds, no downgrade)..."
echo "    waiting up to 20s for breadth mid-fan-out (reproduces v0.27.5 contention)..."
saw_breadth=""
for _ in $(seq 1 20); do
    status_check="$(timeout 5 "$BIN" status --json 2>/dev/null || true)"
    if printf '%s' "$status_check" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
    tasks = d.get("background_tasks") or []
    for t in tasks:
        if isinstance(t, dict) and t.get("name") == "breadth-spx":
            sys.exit(0)
    sys.exit(1)
except Exception:
    sys.exit(1)
' 2>/dev/null; then
        saw_breadth="yes"
        break
    fi
    sleep 1
done
if [[ -n "$saw_breadth" ]]; then
    echo "    breadth fan-out detected; letting it warm up for 8s before regime"
    sleep 8
else
    echo "    breadth not refreshing within window (snapshot fresh or bootstrap skipped); proceeding"
fi

run_wire_cli regime_1 30 regime --json
regime_json_1="$LAST_CMD_OUTPUT"
assert_wire regime-subs "$LAST_WIRE_OFFSET"
run_wire_cli regime_2 30 regime --json
regime_json_2="$LAST_CMD_OUTPUT"
assert_wire regime-subs "$LAST_WIRE_OFFSET"

regime_check="$(python3 -c '
import json, sys
a = json.loads(sys.argv[1])
b = json.loads(sys.argv[2])
rows = ["vix_term_structure", "hyg_spy_divergence", "usd_jpy", "gamma_zero"]
drops = []
for r in rows:
    s1 = a.get(r, {}).get("status", "")
    s2 = b.get(r, {}).get("status", "")
    if s1 in ("ok", "stale") and s2 in ("error", "unavailable"):
        drops.append(r + ": " + s1 + " -> " + s2)
if drops:
    print("DROP " + "; ".join(drops))
    sys.exit(0)
print("ok (no rows downgraded between calls)")
' "$regime_json_1" "$regime_json_2")"
if [[ "$regime_check" != ok* ]]; then
    echo "release-smoke: FAIL: regime sequence: $regime_check" >&2
    echo "" >&2
    echo "call 1:" >&2
    echo "$regime_json_1" >&2
    echo "call 2:" >&2
    echo "$regime_json_2" >&2
    exit 1
fi
echo "    $regime_check"

shape_check="$(python3 -c '
import json, sys
findings = []
for label, payload in (("call 1", sys.argv[1]), ("call 2", sys.argv[2])):
    d = json.loads(payload)
    for r in ("vix_term_structure", "hyg_spy_divergence", "usd_jpy"):
        msg = d.get(r, {}).get("error_message", "") or ""
        if "fan-out exceeded handler deadline" in msg:
            findings.append(label + " " + r + ": orchestrator safety net triggered; message=" + repr(msg))
if findings:
    print("FALLBACK " + " | ".join(findings))
    sys.exit(0)
print("ok (no row hit the orchestrator-deadline fallback on either call)")
' "$regime_json_1" "$regime_json_2")"
if [[ "$shape_check" != ok* ]]; then
    echo "release-smoke: FAIL: regime shape: $shape_check" >&2
    echo "" >&2
    echo "call 1:" >&2
    echo "$regime_json_1" >&2
    echo "call 2:" >&2
    echo "$regime_json_2" >&2
    exit 1
fi
echo "    $shape_check"

echo "  [8] chain SPY 1-wide..."
expiries="$("$BIN" chain SPY 2>/dev/null | awk '/^[[:space:]]+20[0-9]{2}-[0-9]{2}-[0-9]{2}/ {print $1}' | head -3 | tail -1)"
if [[ -z "$expiries" ]]; then
    echo "release-smoke: FAIL: could not list SPY expiries via 'ibkr chain SPY'" >&2
    exit 1
fi
run_wire_cli chain-iv "$WIRE_TIMEOUT" chain SPY --expiry "$expiries" --width 1 --side both --json
assert_wire chain-iv-source "$LAST_WIRE_OFFSET"

echo "  [9] gamma --no-wait..."
run_wire_cli gamma "$WIRE_TIMEOUT" gamma --no-wait --json
assert_wire gamma-noflag "$LAST_WIRE_OFFSET"

if [[ "${LOOSE:-0}" -eq 1 ]]; then
    echo "  [10] gamma (loose: BS-IV fallback assertion)..."
    GAMMA_ENV="$SMOKE_DIR/gamma-envelope.json"
    for attempt in 1 2 3 4 5; do
        run_wire_cli gamma_wait 60 gamma --json
        printf '%s' "$LAST_CMD_OUTPUT" > "$GAMMA_ENV"
        if grep -q '"status": *"ready"' <<<"$LAST_CMD_OUTPUT"; then
            break
        fi
        echo "    poll $attempt: still computing"
        sleep 2
    done
    assert_wire gamma-premarket-derived "$LAST_WIRE_OFFSET" "$GAMMA_ENV"
fi

echo "  [11] gamma --only=spx --no-wait..."
gamma_spx_json="$(run_cli gamma_spx "$WIRE_TIMEOUT" gamma --only=spx --no-wait --json)"
if [[ "${SPX_EXPECTED_REACHABLE:-0}" -eq 1 ]]; then
    if grep -q '"status": *"error"' <<<"$gamma_spx_json"; then
        echo "release-smoke: FAIL: SPX_EXPECTED_REACHABLE=1 but gamma --only=spx returned error" >&2
        echo "$gamma_spx_json" >&2
        exit 1
    fi
    echo "    spx ok - daemon accepted --only=spx scope, no entitlement error"
fi

echo ""
mode_label="strict"
if [[ "${LOOSE:-0}" -eq 1 ]]; then mode_label="loose"; fi
echo "release-smoke: PASS - $BIN ($EXPECTED) JSON + wire flow is healthy (mode=${mode_label})"
