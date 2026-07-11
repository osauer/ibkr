#!/usr/bin/env bash
#
# codex-implement.sh — run one headless Codex implementation task in a
# sibling worktree, capturing artifacts for the orchestrating session.
#
# The orchestrator (a Claude session or a human) owns the loop: it writes
# the brief, runs this script, reviews the diff against the brief, runs the
# repo gates, iterates with --resume, and integrates the result. Codex only
# implements. See .claude/skills/codex-delegate/SKILL.md for the full loop.
#
# Safety shape (do not weaken):
#   - The worktree is created from local main; the primary working tree and
#     its in-flight changes are never the delegate's workspace.
#   - workspace-write seatbelt scoped to the worktree plus the Go build
#     cache; other out-of-tree writes and direct network access are denied.
#   - Headless exec has no approver: sandbox escalations and execpolicy
#     prompt/forbidden decisions fail closed instead of asking.
#   - Broker writes are never delegated. Briefs keep ibkr usage read-only;
#     daemon agent-origin gating remains the binding boundary regardless.
#
# Usage:
#   scripts/codex-implement.sh --task NAME [options] [--brief FILE]
#   scripts/codex-implement.sh --task NAME --resume THREAD_ID [--brief FILE]
#
#   The brief (task instructions) comes from --brief FILE, or stdin.
#
# Options:
#   --task NAME       Task slug. Worktree ../<repo>-codex-NAME on branch
#                     codex/NAME, created from local main if missing.
#   --resume ID       Continue an earlier thread (review feedback loop).
#   --read-only       Analysis/review task: read-only sandbox, no worktree
#                     writes expected.
#   --model M         Override the model (default: user codex config).
#   --effort E        Override model_reasoning_effort for this run.
#
# Artifacts land in .claude/codex-runs/NAME/<utc-stamp>/ in the primary
# repo: brief.md, events.jsonl, last-message.md, thread-id, diff.patch.
set -euo pipefail

usage() { sed -n '/^# Usage:/,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//;$d'; }

TASK="" RESUME="" BRIEF="" MODEL="" EFFORT="" SANDBOX="workspace-write"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --task)      TASK="${2:?--task needs a name}"; shift 2 ;;
        --resume)    RESUME="${2:?--resume needs a thread id}"; shift 2 ;;
        --brief)     BRIEF="${2:?--brief needs a file}"; shift 2 ;;
        --model)     MODEL="${2:?--model needs a value}"; shift 2 ;;
        --effort)    EFFORT="${2:?--effort needs a value}"; shift 2 ;;
        --read-only) SANDBOX="read-only"; shift ;;
        -h|--help)   usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
done

[[ -n "$TASK" ]] || { echo "error: --task NAME is required" >&2; exit 2; }
[[ "$TASK" =~ ^[a-z0-9][a-z0-9-]*$ ]] || {
    echo "error: task name must be lowercase-kebab (got: $TASK)" >&2; exit 2
}

REPO="$(git rev-parse --show-toplevel)"
WORKTREE="$(dirname "$REPO")/$(basename "$REPO")-codex-$TASK"
BRANCH="codex/$TASK"

if [[ ! -d "$WORKTREE" ]]; then
    git -C "$REPO" worktree add -b "$BRANCH" "$WORKTREE" main
fi

RUN_DIR="$REPO/.claude/codex-runs/$TASK/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RUN_DIR"

if [[ -n "$BRIEF" ]]; then
    cp "$BRIEF" "$RUN_DIR/brief.md"
else
    cat > "$RUN_DIR/brief.md"
fi
[[ -s "$RUN_DIR/brief.md" ]] || { echo "error: empty brief" >&2; exit 2; }

# Fixed, non-negotiable execution shape. No approval or hook-trust bypass
# flags may be added here; headless denials failing closed is the design.
CMD=(codex exec --json
    -C "$WORKTREE"
    -s "$SANDBOX"
    -o "$RUN_DIR/last-message.md")
if command -v go >/dev/null 2>&1 && [[ "$SANDBOX" == "workspace-write" ]]; then
    CMD+=(--add-dir "$(go env GOCACHE)")
fi
[[ -n "$MODEL" ]] && CMD+=(-m "$MODEL")
[[ -n "$EFFORT" ]] && CMD+=(-c "model_reasoning_effort=$EFFORT")
if [[ -n "$RESUME" ]]; then
    CMD+=(resume "$RESUME" -)
fi

set +e
"${CMD[@]}" < "$RUN_DIR/brief.md" > "$RUN_DIR/events.jsonl" 2> "$RUN_DIR/stderr.log"
CODEX_EXIT=$?
set -e

THREAD="$RESUME"
if [[ -z "$THREAD" ]]; then
    THREAD="$(grep -o '"thread_id":"[^"]*"' "$RUN_DIR/events.jsonl" | head -1 | cut -d'"' -f4 || true)"
fi
printf '%s\n' "$THREAD" > "$RUN_DIR/thread-id"

# Full patch including untracked files, so the orchestrator reviews one
# artifact. intent-to-add only touches the delegate worktree's index.
git -C "$WORKTREE" add -A -N .
git -C "$WORKTREE" diff > "$RUN_DIR/diff.patch" || true

echo "=== codex-implement result"
echo "task:      $TASK"
echo "worktree:  $WORKTREE ($(git -C "$WORKTREE" branch --show-current))"
echo "thread:    ${THREAD:-unknown}"
echo "exit:      $CODEX_EXIT"
echo "artifacts: ${RUN_DIR#"$REPO"/}"
echo "--- diffstat"
git -C "$WORKTREE" diff --stat | tail -20
echo "--- last message"
cat "$RUN_DIR/last-message.md" 2>/dev/null || echo "(none)"
exit "$CODEX_EXIT"
