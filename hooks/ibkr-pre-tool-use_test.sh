#!/usr/bin/env bash
# Table-driven behavior test for ibkr-pre-tool-use.sh.
#
# The hook is a broker guardrail: false-allow lets an agent reach a write
# path, false-block breaks read-only workflows (the v1.14.0 cache blocked
# plain `ibkr orders --json` for weeks). Every row here is one payload and
# the exit code the hook must produce. Write-path rows stub `ibkr trading
# status --json` via a fake ibkr on PATH; read-only rows must decide without
# invoking ibkr at all (the stub records invocations so we can assert that).
set -u

hook_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
hook="$hook_dir/ibkr-pre-tool-use.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

stub_dir="$work/bin"
mkdir -p "$stub_dir"
cat >"$stub_dir/ibkr" <<'STUB'
#!/usr/bin/env bash
echo "invoked $*" >>"$IBKR_STUB_LOG"
if [[ "$1 $2 ${3:-}" == "trading status --json" ]]; then
  cat "$IBKR_STUB_STATUS"
  exit 0
fi
exit 1
STUB
chmod +x "$stub_dir/ibkr"

live_ready="$work/live-ready.json"
cat >"$live_ready" <<'JSON'
{"mode":"live","blocked":false,"live_override":"ready","can_write":true,"account":"U0000000","gateway_port":7496,"write_blockers":[]}
JSON

live_frozen="$work/live-frozen.json"
cat >"$live_frozen" <<'JSON'
{"mode":"live","blocked":false,"live_override":"ready","can_write":false,"account":"U0000000","gateway_port":7496,"write_blockers":[{"code":"trading_frozen"}]}
JSON

mode_disabled="$work/disabled.json"
cat >"$mode_disabled" <<'JSON'
{"mode":"disabled","blocked":true,"live_override":"blocked","can_write":false,"write_blockers":[{"code":"trading_disabled"}]}
JSON

fails=0
run_case() {
  local name="$1" want="$2" status_file="$3" want_stub="$4" cmd="$5"
  local stub_log="$work/stub-$name.log"
  : >"$stub_log"
  printf '{"tool_input":{"command":%s}}' "$(printf '%s' "$cmd" | jq -Rs .)" |
    PATH="$stub_dir:$PATH" IBKR_STUB_LOG="$stub_log" IBKR_STUB_STATUS="$status_file" \
      bash "$hook" >/dev/null 2>"$work/stderr-$name"
  local got=$?
  if [[ "$got" != "$want" ]]; then
    echo "FAIL $name: exit $got, want $want (cmd: $cmd)" >&2
    sed 's/^/  stderr: /' "$work/stderr-$name" >&2
    fails=$((fails + 1))
    return
  fi
  local stub_calls
  stub_calls="$(wc -l <"$stub_log" | tr -d ' ')"
  if [[ "$want_stub" == "none" && "$stub_calls" != "0" ]]; then
    echo "FAIL $name: read-only path invoked ibkr ($stub_calls calls)" >&2
    fails=$((fails + 1))
    return
  fi
  if [[ "$want_stub" == "status" && "$stub_calls" == "0" ]]; then
    echo "FAIL $name: write path decided without consulting trading status" >&2
    fails=$((fails + 1))
    return
  fi
  echo "ok   $name"
}

# Read-only surfaces must pass without consulting the daemon.
run_case orders-bare 0 "$live_ready" none 'ibkr orders'
run_case orders-json 0 "$live_ready" none 'ibkr orders --json'
run_case orders-open 0 "$live_ready" none 'ibkr orders open --json'
run_case orders-history 0 "$live_ready" none 'ibkr orders history'
run_case orders-piped 0 "$live_ready" none 'ibkr orders --json | jq -c .orders'
run_case positions 0 "$live_ready" none 'ibkr positions --json'
run_case order-preview 0 "$live_ready" none 'ibkr order preview sell BB 20260821 C 12 100 --json'
run_case order-status 0 "$live_ready" none 'ibkr order status 42 --json'
run_case order-help 0 "$live_ready" none 'ibkr order --help'
run_case rules-future 0 "$live_ready" none 'ibkr rules --json'

# Human-only and destructive state writes stay blocked regardless of status.
run_case settings-set 2 "$live_ready" none 'ibkr settings set trading.freeze=true'
run_case watch-add 2 "$live_ready" none 'ibkr watch --add BB'
run_case daemon-wipe 2 "$live_ready" none 'ibkr daemon wipe'

# Broker writes consult trading status: route-ready allows, otherwise block.
run_case place-live-ready 0 "$live_ready" status 'ibkr order place --preview-token tok'
run_case place-disabled 2 "$mode_disabled" status 'ibkr order place --preview-token tok'
run_case cancel-frozen 0 "$live_frozen" status 'ibkr order cancel 42'
run_case place-frozen 2 "$live_frozen" status 'ibkr order place --preview-token tok'

# Shell composition around a write is blocked before any status lookup.
run_case compound-write 2 "$live_ready" none 'ibkr orders --json; ibkr order place --preview-token tok'
run_case subshell-write 2 "$live_ready" none 'ibkr order place --preview-token $(cat tok)'

if [[ "$fails" -gt 0 ]]; then
  echo "$fails hook behavior case(s) failed" >&2
  exit 1
fi
echo "hook behavior: all cases passed"
