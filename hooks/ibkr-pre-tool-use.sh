#!/usr/bin/env bash
set -euo pipefail

payload="$(cat)"
command_line="unknown"

block() {
  printf 'ibkr safety hook blocked command: %s\n\n%s\n' "$command_line" "$1" >&2
  exit 2
}

has_re() {
  printf '%s' "$command_line" | grep -Eq "$1"
}

if ! command -v jq >/dev/null 2>&1; then
  block "jq is required so the project hook can inspect agent tool payloads before broker-adjacent commands run."
fi

command_line="$(
  printf '%s' "$payload" | jq -r '
    def as_command:
      if type == "array" then map(tostring) | join(" ")
      elif type == "string" then .
      else ""
      end;

    [
      (.tool_input.command? | as_command),
      (.tool.input.command? | as_command),
      (.input.command? | as_command),
      (.arguments.command? | as_command),
      (.params.command? | as_command),
      (.command? | as_command),
      (.tool_input.args? | as_command),
      (.args? | as_command),
      (.argv? | as_command)
    ]
    | map(select(length > 0))
    | .[0] // ""
  ' 2>/dev/null || true
)"

command_line="${command_line#"${command_line%%[![:space:]]*}"}"
command_line="${command_line%"${command_line##*[![:space:]]}"}"
# Normalize trivial quoting ('ibkr' / "ibkr") so quoted invocations cannot
# slip past the verb matching below.
command_line="${command_line//\'/}"
command_line="${command_line//\"/}"

if [[ -z "$command_line" ]] || ! has_re '(^|[[:space:]/])ibkr([[:space:]]|$)'; then
  exit 0
fi

trading_status_json() {
  ibkr trading status --json 2>/dev/null
}

paper_writes_ready() {
  local status
  status="$(trading_status_json)" || return 1
  printf '%s' "$status" | jq -e '
    (.mode == "paper")
    and (.can_write == true)
    and ((.blocked // false) == false)
    and (
      (.gateway_port == 4002)
      or (.gateway_port == 7497)
      or ((.account // "" | ascii_upcase | startswith("DU")))
    )
    and ((.live_override // "blocked") != "ready")
  ' >/dev/null
}

paper_write_status_summary() {
  local status
  if ! status="$(trading_status_json)"; then
    printf 'trading.status unavailable'
    return
  fi
  printf '%s' "$status" | jq -r '
    "mode=\(.mode // "unknown") account=\(.account // "unknown") endpoint=\(.endpoint // "unknown") can_write=\(.can_write // false) blocked=\(.blocked // false) live_override=\(.live_override // "unknown")"
  ' 2>/dev/null || printf 'trading.status unreadable'
}

allow_only_paper_write_or_block() {
  # Broker-write invocations must be a single plain command: composition can
  # smuggle a second write past the verb matching above. Read-only ibkr
  # commands keep their pipes and redirects.
  if [[ "$command_line" == *$'\n'* || "$command_line" =~ [\;\&\|\<\>\`] || "$command_line" =~ \$\( ]]; then
    block "Run broker-write ibkr commands directly, without shell composition, pipes, redirection, command substitution, or chained commands."
  fi
  if paper_writes_ready; then
    exit 0
  fi
  block "Paper broker writes are allowed only when trading.status is paper/write-ready on a paper-looking account or endpoint. Live, disabled, blocked, and unknown trading states remain blocked. Current: $(paper_write_status_summary)"
}

purge_read_only_command() {
  if has_re '(^|[[:space:]/])ibkr[[:space:]]+purge[[:space:]]+(dry-run|status|monitor|--help|-h|help)([[:space:]]|$)' ||
    has_re '(^|[[:space:]])--dry-run([[:space:]]|$)' ||
    has_re '(^|[[:space:]])(--help|-h)([[:space:]]|$)'; then
    return 0
  fi
  if has_re '(^|[[:space:]/])ibkr[[:space:]]+purge[[:space:]]+restore([[:space:]]|$)' &&
    ! has_re '(^|[[:space:]])--execute(=|[[:space:]]|$)'; then
    return 0
  fi
  return 1
}

if has_re '(^|[[:space:]/])ibkr[[:space:]]+watch([[:space:]]|$)' &&
  has_re '(^|[[:space:]])--(add|remove|clear)(=|[[:space:]]|$)'; then
  block "Agents should not mutate the user's local watchlist. Ask the user to make that local preference change manually."
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+purge([[:space:]]|$)'; then
  if purge_read_only_command; then
    exit 0
  fi
  allow_only_paper_write_or_block
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+daemon[[:space:]]+(purge|reset|wipe)([[:space:]]|$)'; then
  block "Daemon destructive maintenance must be run by the user, not by an agent session."
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+(order|orders|trading|trade|trades|proposals)([[:space:]]|$)' &&
  has_re '(^|[[:space:]])(--help|-h|help)([[:space:]]|$)'; then
  exit 0
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+(order[[:space:]]+(preview|status)|orders[[:space:]]+(open|status)|trading[[:space:]]+status)([[:space:]]|$)'; then
  exit 0
fi

# Paper-smoke mints the last live-trading precondition; the daemon refuses
# agent origins regardless, this just fails the attempt earlier and clearer.
if has_re '(^|[[:space:]/])ibkr[[:space:]]+trading[[:space:]]+paper-smoke([[:space:]]|$)'; then
  block "Paper-smoke produces the last live-trading precondition and must be run by the user from an interactive terminal, not from an agent session."
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+proposals[[:space:]]+submit([[:space:]]|$)'; then
  allow_only_paper_write_or_block
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+order[[:space:]]+(place|submit|execute|modify|cancel|close)([[:space:]]|$)' ||
  has_re '(^|[[:space:]/])ibkr[[:space:]]+(submit|place|transmit|modify|cancel|close)([[:space:]]|$)'; then
  allow_only_paper_write_or_block
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+(order|orders|trading|trade|trades|submit|place|cancel|close|modify|transmit)([[:space:]]|$)'; then
  allow_only_paper_write_or_block
fi

exit 0
