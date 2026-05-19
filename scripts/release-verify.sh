#!/usr/bin/env bash
#
# release-verify.sh — run the freshly-built ibkr binary against a live
# IBKR Gateway and assert it can complete a deterministic smoke matrix.
#
# Wired into `make release` between the rebuild and the cross-compile,
# so a binary that cannot talk to the gateway never reaches GitHub
# Releases. Designed to be binding (non-zero exit aborts the release)
# and deterministic (same binary + same gateway state → same outcome).
#
# Determinism notes:
#   - Runs against an isolated daemon under /tmp; never touches the
#     user's running daemon or their canonical socket. Set up + torn
#     down per invocation.
#   - The matrix uses only commands that work regardless of market
#     hours: version (offline), status, account, positions, and a
#     SPY snapshot. Option-chain commands are deliberately excluded
#     because IBKR's secdef-farm is degraded pre-RTH and a chain
#     request can take ≥25 s on a cold cache — that's an off-hours
#     property of the gateway, not a regression we want to gate on.
#   - Each command has a tight wall-clock timeout so a wedged daemon
#     cannot hang the release.
#
# Usage:
#   scripts/release-verify.sh <bin-path> <expected-version>
#
# Example:
#   scripts/release-verify.sh bin/ibkr v0.15.1

set -euo pipefail

BIN="${1:?usage: release-verify.sh <bin/ibkr> <expected-version> (e.g. v0.15.1)}"
EXPECTED="${2:?expected version required, e.g. v0.15.1}"

if [[ ! -x "$BIN" ]]; then
    echo "release-verify: $BIN not executable" >&2
    exit 2
fi

# Tight per-command budgets. SPY snapshots typically return in <1s when
# the gateway is healthy; 15s is generous enough for a momentarily slow
# market-data farm without letting a wedged daemon hang the release.
PER_CMD_TIMEOUT="${IBKR_RELEASE_VERIFY_TIMEOUT:-15}"

# Isolated environment under /tmp (macOS Unix-socket path limit is 104
# bytes, so we keep the dir short). Using IBKR_SOCKET + IBKR_LOG mirrors
# what test/integration/lifecycle_test.go does for the same reason.
TMPDIR_BASE="${TMPDIR:-/tmp}"
SMOKE_DIR="$(mktemp -d "$TMPDIR_BASE/ibkr-release-verify-XXXXXX")"
SOCKET="$SMOKE_DIR/ibkr.sock"
LOG="$SMOKE_DIR/ibkr-daemon.log"
LOCK="$SMOKE_DIR/ibkr.lock"

export IBKR_SOCKET="$SOCKET"
export IBKR_LOG="$LOG"

cleanup() {
    local code=$?
    # Best-effort: SIGTERM the daemon that this run spawned. The lock
    # file holds its PID. Falls back to nothing if the file disappeared.
    if [[ -r "$LOCK" ]]; then
        local pid
        pid="$(tr -d '[:space:]' < "$LOCK" 2>/dev/null || true)"
        if [[ -n "$pid" && "$pid" -gt 0 ]] 2>/dev/null; then
            kill -TERM "$pid" 2>/dev/null || true
            # Wait up to 3s for graceful exit.
            for _ in $(seq 1 30); do
                if ! kill -0 "$pid" 2>/dev/null; then break; fi
                sleep 0.1
            done
            kill -KILL "$pid" 2>/dev/null || true
        fi
    fi
    # On non-zero exit, surface the daemon log tail so the failure mode
    # is obvious from CI output — otherwise the daemon's reason for
    # refusing connections is hidden in a tmp file the user can't see.
    if [[ $code -ne 0 && -r "$LOG" ]]; then
        echo ""
        echo "release-verify: daemon log tail ($LOG):" >&2
        tail -40 "$LOG" >&2 || true
    fi
    rm -rf "$SMOKE_DIR" 2>/dev/null || true
    return $code
}
trap cleanup EXIT INT TERM

echo "release-verify: smoke matrix against $BIN expecting $EXPECTED"
echo "release-verify: isolated daemon → $SOCKET"

# Stop any pre-existing `ibkr daemon` process before spawning the smoke
# daemon. The script isolates the socket + log + lockfile under /tmp but
# the IBKR gateway only allows one connection per client ID, and both
# daemons read the same config (defaulting to ID 15). Without this step,
# the smoke daemon races the user's canonical daemon for the gateway slot
# and the second one loses with "code 326 / client id already in use" —
# which aborted the v0.16.0 release on first run. SIGTERM is enough for
# the canonical daemon to release its slot; SIGKILL handles stragglers.
# Survivors auto-spawn on the next CLI call, so the cost is one bounce.
stop_existing_daemons() {
    local pids
    pids="$(pgrep -f 'ibkr daemon' 2>/dev/null || true)"
    if [[ -z "$pids" ]]; then
        return 0
    fi
    echo "release-verify: stopping pre-existing daemon(s) so they don't race the smoke daemon for the gateway client-ID slot:"
    for pid in $pids; do
        local cmd
        cmd="$(ps -o command= -p "$pid" 2>/dev/null || echo '?')"
        echo "  pid=$pid cmd=$cmd"
    done
    for pid in $pids; do
        kill -TERM "$pid" 2>/dev/null || true
    done
    # Wait up to 5s for graceful exit before escalating.
    for _ in $(seq 1 50); do
        local remaining=""
        for pid in $pids; do
            if kill -0 "$pid" 2>/dev/null; then
                remaining="$remaining $pid"
            fi
        done
        if [[ -z "$remaining" ]]; then
            return 0
        fi
        sleep 0.1
    done
    for pid in $pids; do
        kill -KILL "$pid" 2>/dev/null || true
    done
}
stop_existing_daemons

# Helper: run a CLI command with a deadline; on failure, print the
# command + output before bubbling up.
run_cli() {
    local label="$1"
    shift
    if ! out="$(timeout "$PER_CMD_TIMEOUT" "$BIN" "$@" 2>&1)"; then
        echo "release-verify: FAIL [$label]: '$BIN $*' exited non-zero (or timed out at ${PER_CMD_TIMEOUT}s)" >&2
        echo "$out" >&2
        exit 1
    fi
    printf '%s' "$out"
}

# Helper: print the value at a dotted JSON path, or empty string when any
# segment is missing. JSON arrives on stdin; path is argv[1]. Booleans
# become Python's True/False (the caller compares against those exact
# tokens).
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

# Helper: exit 0 if the JSON (stdin) has the given top-level key,
# else exit 1. Used to assert presence/absence of fields.
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

# 1 — version stamp on the binary matches what we're shipping. Offline.
echo "  [1/7] version stamp..."
version_json="$(run_cli version version --json)"
actual_version="$(json_field version "$version_json")"
if [[ "$actual_version" != "$EXPECTED" ]]; then
    echo "release-verify: FAIL: binary stamps version=$actual_version, expected=$EXPECTED" >&2
    echo "(the ldflags in 'make build' must agree with the release tag)" >&2
    exit 1
fi

# 2 — status: daemon spawned, gateway connected, daemon_version matches.
# `status` autospawns the daemon at IBKR_SOCKET if one isn't running there.
echo "  [2/7] status (autospawn daemon at isolated socket)..."
status_json="$(run_cli status status --json)"
connected="$(json_field connected "$status_json")"
daemon_version="$(json_field daemon_version "$status_json")"
if [[ "$connected" != "True" ]]; then
    echo "release-verify: FAIL: gateway not reachable (status.connected=$connected)" >&2
    echo "$status_json" >&2
    exit 1
fi
if [[ "$daemon_version" != "$EXPECTED" ]]; then
    echo "release-verify: FAIL: daemon stamped version=$daemon_version, expected=$EXPECTED" >&2
    echo "(autospawn picked up an unexpected binary — check \$PATH and bin/ibkr)" >&2
    exit 1
fi
# v0.27.4 wire contract: background_tasks is always emitted (possibly
# empty) so consumers can rely on `len() == 0` to mean "daemon idle".
# Absent or non-list here would break MCP/CLI consumers that read it
# without nil-checking. Each entry, when present, must have a string
# name. We don't assert on which tasks are running — that depends on
# whether the autospawned daemon is past postConnectSetup yet — only
# on the shape.
bg_check="$(printf '%s' "$status_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
tasks = d.get("background_tasks", None)
if tasks is None:
    print("background_tasks missing — must be emitted (even when empty)")
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
    echo "release-verify: FAIL: status.background_tasks: $bg_check" >&2
    echo "$status_json" >&2
    exit 1
fi

# 3 — account: pins the account-summary handler and the v0.15+ omitempty
# behaviour on data_type. Account financials are always available
# regardless of market hours.
echo "  [3/7] account.summary..."
account_json="$(run_cli account account --json)"
account_id="$(json_field account_id "$account_json")"
if [[ -z "$account_id" ]]; then
    echo "release-verify: FAIL: account_id missing from account.summary response" >&2
    echo "$account_json" >&2
    exit 1
fi
# data_type must be absent (v0.15 contract: account has no live/delayed
# dimension; the field is omitempty + emitted empty).
if printf '%s' "$account_json" | python3 -c "$python_haskey" data_type; then
    echo "release-verify: FAIL: account.summary leaked data_type field (v0.15 must omit it)" >&2
    echo "$account_json" >&2
    exit 1
fi

# 4 — positions: shape only. Empty positions array is valid; this gates
# the handler running, not the user holding stock.
echo "  [4/7] positions.list..."
positions_json="$(run_cli positions positions --json)"
positions_shape_ok="$(printf '%s' "$positions_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("ok" if isinstance(d.get("stocks"), list) and isinstance(d.get("options"), list) else "bad")
')"
if [[ "$positions_shape_ok" != "ok" ]]; then
    echo "release-verify: FAIL: positions.list missing stocks/options arrays" >&2
    echo "$positions_json" >&2
    exit 1
fi
if printf '%s' "$positions_json" | python3 -c "$python_haskey" data_type; then
    echo "release-verify: FAIL: positions.list leaked data_type field (v0.15 must omit it)" >&2
    echo "$positions_json" >&2
    exit 1
fi

# 5 — quote SPY: pins the quote.snapshot handler and the data_type
# plumbing v0.15 introduced. SPY is liquid enough to return ticks at
# any hour the gateway feed is alive. If no tick arrives within the
# timeout the snapshot returns empty bid/ask/last AND empty data_type
# — that's an acceptable degraded state; we only fail on a hard error
# or a non-canonical data_type value.
echo "  [5/7] quote SPY..."
quote_json="$(run_cli quote_SPY quote SPY --json)"
quote_check="$(printf '%s' "$quote_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
sym = d.get("symbol")
if sym != "SPY":
    print("symbol=" + repr(sym) + ", want SPY")
    sys.exit(0)
dt = d.get("data_type", "")
valid = {"", "live", "delayed", "frozen", "delayed-frozen"}
if dt not in valid:
    print("data_type=" + repr(dt) + ", want one of " + str(sorted(valid)))
    sys.exit(0)
print("ok")
')"
if [[ "$quote_check" != "ok" ]]; then
    echo "release-verify: FAIL: quote SPY: $quote_check" >&2
    echo "$quote_json" >&2
    exit 1
fi

# 6 — breadth: pins the v0.27.3 BreadthSPXResult.State contract. Three
# bugs in the v0.27.x cycle (bootstrap race, poison-cache, idle kill)
# all manifested as inconsistent state on this surface; a fresh
# autospawned daemon should reach a coherent state within a few seconds
# of postConnectSetup. The check asserts:
#   - state is one of the documented enum values
#   - if state == "ready", value MUST be > 0 (a successful finalise
#     against any real market regime puts >0% above their 50DMA;
#     v0.27.0's poison-cache produced value=0 with what would have been
#     state=ready — the exact regression this gates against)
#   - if state == "cold", value MUST be 0 (engine hasn't run; any
#     value here is a finalise-without-persist bug)
# We accept "cold" and "computing" as valid initial states because the
# cold-start fan-out takes ~60 min (IBKR pacing) — release-verify
# can't wait for "ready" without budgeting an hour, which would make
# every release a CI ordeal. The invariant we CAN check in seconds
# is "the wire state is internally consistent."
echo "  [6/7] breadth.spx state..."
breadth_json="$(run_cli breadth breadth --json)"
breadth_check="$(printf '%s' "$breadth_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
state = d.get("state")
value = d.get("value", 0)
valid_states = {"cold", "computing", "ready", "degraded"}
if state not in valid_states:
    print("state=" + repr(state) + ", want one of " + str(sorted(valid_states)))
    sys.exit(0)
if state == "ready" and value <= 0:
    print("state=ready but value=" + repr(value) + " — successful finalise must produce non-zero S5FI (regression of v0.27.0 poison-cache)")
    sys.exit(0)
if state == "cold" and value != 0:
    print("state=cold but value=" + repr(value) + " — engine that never ran cannot have a value")
    sys.exit(0)
print("ok (state=" + state + " value=" + repr(value) + ")")
')"
if [[ "$breadth_check" != ok* ]]; then
    echo "release-verify: FAIL: breadth.spx: $breadth_check" >&2
    echo "$breadth_json" >&2
    exit 1
fi
echo "    $breadth_check"

# 7 — regime call-sequence drop check. Pins the user-facing contract that
# motivated the test plan: a regime row that returned ok/stale on call N
# must not silently flip to error/unavailable on call N+1. Two calls 30 s
# apart sample two independent rounds of live ticks against the same
# gateway. If a row's status downgrades between them, something dropped
# between fetches and the release blocks.
#
# The check is one-directional: error→ok (recovery) and computing→ok
# (long-running task finishing) are both fine. Off-hours flake patterns
# (VIX3M, USD.JPY) typically error on BOTH calls and so are not flagged.
# Breadth has its own state-consistency check at step 6 and is not
# re-gated here.
#
# Status field across regime rows: vix_term_structure, hyg_spy_divergence,
# usd_jpy, gamma_zero each carry .status; breadth carries .state which is
# checked above.
echo "  [7/7] regime call-sequence (call N+1 doesn't lose what call N had)..."
# Wait for breadth's cold-start fan-out to be in flight before firing
# regime. The v0.27.5 production bug surfaced precisely under this
# condition: breadth fan-out + concurrent gamma compute saturated the
# gateway's HMDS slots, regime's history fetches blocked past the
# daemon ctx deadline, and the whole handler hung. A freshly-spawned
# smoke daemon enters this exact state on its own — postConnectSetup
# launches the bootstrap automatically — so the gate just has to wait
# for the state, not engineer it.
#
# Max 20 s wait; if breadth never enters refreshing (e.g. persisted
# snapshot is fresh enough to skip the bootstrap), proceed anyway —
# the call-sequence diff is still informative on a quiescent daemon.
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

# Regime fans out five fetchers; the slowest leg is a 20 s history budget,
# so the 15 s default for PER_CMD_TIMEOUT is too tight. Bump it for the
# two regime calls and restore afterwards.
saved_per_cmd_timeout="$PER_CMD_TIMEOUT"
PER_CMD_TIMEOUT=30
regime_json_1="$(run_cli regime_1 regime --json)"
sleep 30
regime_json_2="$(run_cli regime_2 regime --json)"
PER_CMD_TIMEOUT="$saved_per_cmd_timeout"
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
    echo "release-verify: FAIL: regime sequence: $regime_check" >&2
    echo "" >&2
    echo "call 1:" >&2
    echo "$regime_json_1" >&2
    echo "call 2:" >&2
    echo "$regime_json_2" >&2
    exit 1
fi
echo "    $regime_check"

echo ""
echo "release-verify: PASS — $BIN ($EXPECTED) talks to the gateway and ships honest JSON"
