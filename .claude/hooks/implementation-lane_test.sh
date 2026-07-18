#!/usr/bin/env bash
# Table-driven behavior test for implementation-lane.sh.
#
# The hook is a workflow gate with two failure directions: false-allow lets
# a Claude session hand-edit code (defeating the Codex-only lane), and
# false-block breaks the higher-value lane (docs, briefs, config) or edits
# outside the checkout. Every row is one payload and the exit code the hook
# must produce, driven against a throwaway repo root.
set -u

hook_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
hook="${HOOK_UNDER_TEST:-$hook_dir/implementation-lane.sh}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
root="$work/repo"
mkdir -p "$root/.claude/state/inline-waivers"

pass=0
fail=0

payload() { # tool_name file_path session_id
  printf '{"tool_name":"%s","session_id":"%s","tool_input":{"file_path":"%s"}}' "$1" "$3" "$2"
}

run_case() { # name want_exit payload
  local name="$1" want="$2" body="$3" got=0
  printf '%s' "$body" | CLAUDE_PROJECT_DIR="$root" bash "$hook" >/dev/null 2>&1 || got=$?
  if [ "$got" -eq "$want" ]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL $name: want exit $want, got $got" >&2
  fi
}

run_case "go edit blocked" 2 "$(payload Edit "$root/internal/app/relay/worker.go" s1)"
run_case "new go file blocked" 2 "$(payload Write "$root/cmd/ibkr/new.go" s1)"
run_case "shell blocked" 2 "$(payload Edit "$root/scripts/codex-implement.sh" s1)"
run_case "hook self-edit blocked" 2 "$(payload Edit "$root/.claude/hooks/implementation-lane.sh" s1)"
run_case "makefile blocked" 2 "$(payload Edit "$root/Makefile" s1)"
run_case "spa js blocked" 2 "$(payload Edit "$root/web/app/regimerows.js" s1)"
run_case "spa html blocked" 2 "$(payload Write "$root/web/app/index.html" s1)"
run_case "markdown allowed" 0 "$(payload Edit "$root/docs/architecture.md" s1)"
run_case "toml allowed" 0 "$(payload Edit "$root/risk-policy.toml" s1)"
run_case "json allowed" 0 "$(payload Edit "$root/.claude/settings.json" s1)"
run_case "outside checkout allowed" 0 "$(payload Edit "$work/scratch/probe.go" s1)"
run_case "no file_path allowed" 0 '{"tool_name":"Edit","session_id":"s1","tool_input":{}}'

printf 'granted: test\nreason: test\n' >"$root/.claude/state/inline-waivers/s2"
run_case "waived session allowed" 0 "$(payload Edit "$root/internal/app/relay/worker.go" s2)"
run_case "other session still blocked" 2 "$(payload Edit "$root/internal/app/relay/worker.go" s3)"

# A block must teach the recovery path: the codex-delegate lane plus the
# waiver command with this session's id.
msg="$(printf '%s' "$(payload Edit "$root/cmd/ibkr/app.go" sess-42)" | CLAUDE_PROJECT_DIR="$root" bash "$hook" 2>&1 >/dev/null)" || true
ok=1
case "$msg" in *"waive-inline.sh sess-42"*) ;; *) ok=0 ;; esac
case "$msg" in *codex-delegate*) ;; *) ok=0 ;; esac
if [ "$ok" -eq 1 ]; then
  pass=$((pass + 1))
else
  fail=$((fail + 1))
  echo "FAIL block message: must name codex-delegate and waive-inline.sh with the session id" >&2
fi

if [ "$fail" -eq 0 ]; then
  echo "implementation-lane behavior: all $pass cases passed"
else
  echo "implementation-lane behavior: $fail case(s) failed" >&2
  exit 1
fi
