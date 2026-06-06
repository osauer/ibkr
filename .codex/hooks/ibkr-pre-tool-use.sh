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

if has_re '(^|[[:space:]/])ibkr[[:space:]]+(order[[:space:]]+(preview|status)|orders[[:space:]]+(open|status)|trading[[:space:]]+status)([[:space:]]|$)'; then
  exit 0
fi

if has_re '(^|[[:space:]/])ibkr[[:space:]]+(order|orders|trading|trade|trades|submit|place|cancel|close|modify|transmit)([[:space:]]|$)'; then
  block "Broker mutation verbs are blocked for Codex. Use preview/status reads only."
fi

exit 0
