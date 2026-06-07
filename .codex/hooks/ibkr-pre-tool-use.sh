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
  block "jq is required so the project hook can inspect Codex tool payloads before broker-adjacent commands run."
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

if [[ -z "$command_line" ]] || ! has_re '(^|[[:space:]/])ibkr([[:space:]]|$)'; then
  exit 0
fi

if [[ "$command_line" == *$'\n'* || "$command_line" =~ [\;\&\|\<\>\`] || "$command_line" =~ \$\( ]]; then
  block "Run broker-adjacent ibkr commands directly, without shell composition, pipes, redirection, command substitution, or chained commands."
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+watch([[:space:]]|$)' &&
  has_re '(^|[[:space:]])--(add|remove|clear)(=|[[:space:]]|$)'; then
  block "Agents should not mutate the user's local watchlist. Ask the user to make that local preference change manually."
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+purge([[:space:]]|$)'; then
  if has_re '(^|[[:space:]])--execute(=|[[:space:]]|$)'; then
    block "Destructive purge execution requires a human-run command outside the Codex hook path."
  fi

  if has_re '(^|[[:space:]/])ibkr[[:space:]]+purge[[:space:]]+(dry-run|status|monitor|restore|--help|-h|help)([[:space:]]|$)' ||
    has_re '(^|[[:space:]])--dry-run([[:space:]]|$)' ||
    has_re '(^|[[:space:]])(--help|-h)([[:space:]]|$)'; then
    exit 0
  fi

  block "Unqualified 'ibkr purge' is destructive-adjacent. Use a read-only dry-run/status/monitor form first."
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+daemon[[:space:]]+(purge|reset|wipe)([[:space:]]|$)'; then
  block "Daemon destructive maintenance must be run by the user, not by Codex."
fi

require_paper_write_gate() {
  local capability="$1"
  local status
  local mode
  local account
  local endpoint
  local blocked
  local can_do
  local blockers

  if ! status="$(timeout 10s ibkr trading status --json 2>/dev/null)"; then
    block "Broker write command blocked: could not read ibkr trading status. Paper writes require a ready daemon-reported paper gate."
  fi

  mode="$(printf '%s' "$status" | jq -r '.mode // ""')"
  account="$(printf '%s' "$status" | jq -r '.account // ""')"
  endpoint="$(printf '%s' "$status" | jq -r '.endpoint // ""')"
  blocked="$(printf '%s' "$status" | jq -r '.blocked // false')"
  can_do="$(printf '%s' "$status" | jq -r --arg capability "$capability" '.[$capability] // false')"

  if [[ "$mode" == "disabled" ]]; then
    return 0
  fi
  if [[ "$mode" != "paper" ]]; then
    block "Broker write command blocked: Codex may only use paper writes; non-disabled ${mode:-unknown} routes stay blocked by the Codex hook. endpoint=${endpoint:-unknown} account=${account:-unknown}."
  fi
  if [[ "$account" != DU* ]]; then
    block "Broker write command blocked: paper writes require a DU paper account, got account=${account:-unknown}."
  fi
  if [[ "$endpoint" != *":4002" && "$endpoint" != *":7497" ]]; then
    block "Broker write command blocked: paper writes require a paper endpoint (4002/7497), got endpoint=${endpoint:-unknown}."
  fi
  if [[ "$blocked" != "false" ]]; then
    blockers="$(printf '%s' "$status" | jq -r '[.blockers[]?.code] | join(", ")')"
    block "Broker write command blocked: trading.status is blocked (${blockers:-no blocker code reported})."
  fi
  if [[ "$can_do" != "true" ]]; then
    block "Broker write command blocked: trading.status ${capability}=false for paper route endpoint=${endpoint:-unknown} account=${account:-unknown}."
  fi
}

if has_re '(^|[[:space:]/])ibkr[[:space:]]+(order[[:space:]]+(preview|status)|orders[[:space:]]+(open|status)|trading[[:space:]]+status)([[:space:]]|$)'; then
  exit 0
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+proposals[[:space:]]+submit([[:space:]]|$)'; then
  require_paper_write_gate "can_write"
  exit 0
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+order[[:space:]]+(place|submit|execute)([[:space:]]|$)' ||
  has_re '(^|[[:space:]/])ibkr[[:space:]]+(submit|place|transmit)([[:space:]]|$)'; then
  require_paper_write_gate "can_write"
  exit 0
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+order[[:space:]]+modify([[:space:]]|$)' ||
  has_re '(^|[[:space:]/])ibkr[[:space:]]+modify([[:space:]]|$)'; then
  require_paper_write_gate "can_write"
  exit 0
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+order[[:space:]]+(cancel|close)([[:space:]]|$)' ||
  has_re '(^|[[:space:]/])ibkr[[:space:]]+(cancel|close)([[:space:]]|$)'; then
  require_paper_write_gate "can_write"
  exit 0
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+(order|orders|trading|trade|trades|submit|place|cancel|close|modify|transmit)([[:space:]]|$)'; then
  block "Broker mutation verbs are blocked unless the daemon reports a ready paper trading gate. Live and unknown routes are always blocked for Codex."
fi

exit 0
