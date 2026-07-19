#!/usr/bin/env bash
#
# codex-implement.sh — run one headless Codex implementation task in a
# sibling worktree, capturing artifacts for the orchestrating session.
#
# The orchestrator (a Claude session or a human) owns the loop: it writes
# the brief, runs this script, reviews the diff against the brief, runs the
# repo gates, iterates with --resume, integrates by applying the reviewed
# patch in the primary tree, and finishes with --cleanup. Codex only
# implements. See .claude/skills/codex-delegate/SKILL.md for the full loop.
#
# Task lifecycle invariants (enforced, not conventions):
#   - A fresh task requires no leftover worktree or codex/NAME branch;
#     finish the previous task of that name with --cleanup first.
#   - The brief is materialized and validated before any git mutation: a
#     missing or empty brief strands no worktree, branch, or task dir.
#   - Iteration rounds (--resume) require the task worktree to still exist:
#     the thread's context refers to files on disk.
#   - diff.patch is the cumulative task delta against the recorded base
#     commit, so it stays correct even if Codex commits inside its worktree.
#   - --cleanup is idempotent, including when the worktree directory was
#     removed out-of-band: stale registrations are pruned before the branch
#     is deleted.
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
#   scripts/codex-implement.sh --task NAME [--read-only] [--effort LEVEL]
#                               [--budget-threshold N] [--force-budget]
#                               [--brief FILE]
#   scripts/codex-implement.sh --task NAME --resume THREAD_ID
#                               [--effort LEVEL] [--force-rounds]
#                               [--budget-threshold N] [--force-budget]
#                               [--brief FILE]
#   scripts/codex-implement.sh --task NAME --cleanup
#
#   The brief (task instructions) comes from --brief FILE, or stdin.
#
# Options:
#   --task NAME       Task slug. Worktree ../<repo>-codex-NAME on branch
#                     codex/NAME, created from local main.
#   --resume ID       Continue an earlier thread (review feedback loop).
#   --read-only       Analysis/review task: read-only sandbox, no worktree
#                     writes expected.
#   --effort LEVEL    Reasoning effort: low|medium|high|xhigh|max (default: high);
#                     ultra deliberately excluded (automatic task delegation).
#   --force-rounds    Allow a resume after three or more recorded task runs.
#   --budget-threshold N
#                     Refuse at this weekly usage percentage (default: 70).
#   --force-budget    Launch even when the weekly budget threshold is met.
#   --cleanup         Remove the task worktree and branch. Artifacts under
#                     .claude/codex-runs/NAME/ are kept as the audit trail.
#
# Exit codes:
#   2  Usage or launcher lifecycle error.
#   3  Weekly budget-gate refusal.
#   4  Resume round-cap refusal.
#   Other non-zero statuses are passed through from Codex.
#
# Artifacts land in .claude/codex-runs/NAME/<utc-stamp>/ in the primary
# repo: brief.md, events.jsonl, last-message.md, thread-id, diff.patch.
set -euo pipefail

usage() { sed -n '/^# Usage:/,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//;$d'; }
require_value() {
    if (( $# < 2 )) || [[ -z "$2" ]]; then
        echo "error: $1 needs a value" >&2
        usage >&2
        exit 2
    fi
}

TASK="" RESUME="" BRIEF="" SANDBOX="workspace-write" CLEANUP=0
EFFORT="high" FORCE_ROUNDS=0 BUDGET_THRESHOLD=70 FORCE_BUDGET=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --task)      require_value "$@"; TASK="$2"; shift 2 ;;
        --resume)    require_value "$@"; RESUME="$2"; shift 2 ;;
        --brief)     require_value "$@"; BRIEF="$2"; shift 2 ;;
        --read-only) SANDBOX="read-only"; shift ;;
        --effort)    require_value "$@"; EFFORT="$2"; shift 2 ;;
        --force-rounds) FORCE_ROUNDS=1; shift ;;
        --budget-threshold) require_value "$@"; BUDGET_THRESHOLD="$2"; shift 2 ;;
        --force-budget) FORCE_BUDGET=1; shift ;;
        --cleanup)   CLEANUP=1; shift ;;
        -h|--help)   usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
done

[[ -n "$TASK" ]] || { echo "error: --task NAME is required" >&2; exit 2; }
[[ "$TASK" =~ ^[a-z0-9][a-z0-9-]*$ ]] || {
    echo "error: task name must be lowercase-kebab (got: $TASK)" >&2; exit 2
}
case "$EFFORT" in
    low|medium|high|xhigh|max) ;;
    ultra) echo "error: --effort ultra enables automatic task delegation, which the delegation lane forbids (AGENTS.md: delegated runs never re-delegate)" >&2; exit 2 ;;
    *) echo "error: --effort must be low, medium, high, xhigh, or max (got: $EFFORT)" >&2; usage >&2; exit 2 ;;
esac
if [[ ! "$BUDGET_THRESHOLD" =~ ^[0-9]+$ ]] ||
    (( ${#BUDGET_THRESHOLD} > 3 )) || (( 10#$BUDGET_THRESHOLD > 100 )); then
    echo "error: --budget-threshold must be an integer from 0 to 100 (got: $BUDGET_THRESHOLD)" >&2
    usage >&2
    exit 2
fi
BUDGET_THRESHOLD=$((10#$BUDGET_THRESHOLD))

REPO="$(git rev-parse --show-toplevel)"
WORKTREE="$(dirname "$REPO")/$(basename "$REPO")-codex-$TASK"
BRANCH="codex/$TASK"
TASK_DIR="$REPO/.claude/codex-runs/$TASK"

if [[ "$CLEANUP" == 1 ]]; then
    [[ -d "$WORKTREE" ]] && git -C "$REPO" worktree remove --force "$WORKTREE"
    # Prune before deleting the branch: if the worktree directory was
    # removed out-of-band, the stale registration makes `git branch -D`
    # fail with "used by worktree" — pruning first keeps cleanup
    # idempotent in exactly the recovery case.
    git -C "$REPO" worktree prune
    if git -C "$REPO" show-ref --verify --quiet "refs/heads/$BRANCH"; then
        git -C "$REPO" branch -D "$BRANCH"
    fi
    echo "cleaned up: $WORKTREE and branch $BRANCH"
    [[ -d "$TASK_DIR" ]] && echo "artifacts kept: ${TASK_DIR#"$REPO"/} (prune once the work is committed)"
    exit 0
fi

if [[ -d "$WORKTREE" && -z "$RESUME" ]]; then
    echo "error: $WORKTREE already exists; iterate with --resume THREAD_ID, or finish that task with --cleanup" >&2
    exit 2
fi
if [[ ! -d "$WORKTREE" ]]; then
    if [[ -n "$RESUME" ]]; then
        echo "error: --resume needs the task worktree at $WORKTREE, which is gone; the thread's file state was cleaned up — start a fresh task" >&2
        exit 2
    fi
    if git -C "$REPO" show-ref --verify --quiet "refs/heads/$BRANCH"; then
        echo "error: stale branch $BRANCH exists without a worktree; run --cleanup first" >&2
        exit 2
    fi
fi

# Materialize and validate the brief before any git mutation: a missing
# or empty brief must strand no worktree, branch, or task directory. The
# stdin path buffers here for the same reason.
BRIEF_TMP="$(mktemp)"
trap 'rm -f "$BRIEF_TMP"' EXIT
if [[ -n "$BRIEF" ]]; then
    cp "$BRIEF" "$BRIEF_TMP"
else
    cat > "$BRIEF_TMP"
fi
[[ -s "$BRIEF_TMP" ]] || { echo "error: empty brief" >&2; exit 2; }

# The weekly gauge is advisory when telemetry is absent or malformed: a
# bounded tail keeps large rollouts cheap to inspect, and every failing
# pipeline is guarded so set -euo pipefail cannot brick the lane.
ROLLOUT=""
if [[ -n "${HOME:-}" ]]; then
    ROLLOUT="$(ls -t "$HOME"/.codex/sessions/*/*/*/rollout-*.jsonl 2>/dev/null | head -n 1 || true)"
fi
BUDGET_USED=""
if [[ -n "$ROLLOUT" ]]; then
    BUDGET_USED="$(
        tail -c 300000 "$ROLLOUT" 2>/dev/null |
            jq -Rrs '[splits("\n") | fromjson? | select(.payload.type == "token_count")] | last | .payload.rate_limits.primary.used_percent | numbers' 2>/dev/null ||
            true
    )"
fi
if [[ -n "$BUDGET_USED" ]]; then
    echo "codex budget: ${BUDGET_USED}% of weekly window used"
else
    echo "codex budget: unknown"
fi

if [[ -n "$RESUME" && "$FORCE_ROUNDS" == 0 ]]; then
    ROUND_COUNT=0
    if [[ -d "$TASK_DIR" ]]; then
        for RUN_PATH in "$TASK_DIR"/*; do
            [[ -d "$RUN_PATH" ]] || continue
            RUN_NAME="${RUN_PATH##*/}"
            if [[ "$RUN_NAME" == [0-9]*T[0-9]*Z ]]; then
                ROUND_COUNT=$((ROUND_COUNT + 1))
            fi
        done
    fi
    if (( ROUND_COUNT >= 3 )); then
        echo "error: task $TASK already has $ROUND_COUNT recorded runs; start a fresh task with a distilled delta-brief (current diff + remaining findings) instead of paying the grown thread context again, or pass --force-rounds" >&2
        exit 4
    fi
fi

if [[ -n "$BUDGET_USED" && "$FORCE_BUDGET" == 0 ]] &&
    jq -en --argjson used "$BUDGET_USED" --argjson threshold "$BUDGET_THRESHOLD" \
        '$used >= $threshold' >/dev/null 2>&1; then
    echo "error: weekly Codex budget is ${BUDGET_USED}% used (threshold ${BUDGET_THRESHOLD}%); defer non-urgent work or pass --force-budget" >&2
    exit 3
fi

if [[ ! -d "$WORKTREE" ]]; then
    git -C "$REPO" worktree add -b "$BRANCH" "$WORKTREE" main
    mkdir -p "$TASK_DIR"
    git -C "$REPO" rev-parse main > "$TASK_DIR/base-sha"
fi

RUN_DIR="$TASK_DIR/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RUN_DIR"
mv "$BRIEF_TMP" "$RUN_DIR/brief.md"

# Fixed, non-negotiable execution shape. No approval or hook-trust bypass
# flags may be added here; headless denials failing closed is the design.
# Model, effort, and speed are pinned per-run (user decision 2026-07-18):
# the ChatGPT desktop app rewrites ~/.codex/config.toml mid-session, so
# delegation behavior must not inherit that drift.
# The app-managed config also enables multi-agent features; pin chronicle
# off so delegated runs cannot re-delegate, as required by AGENTS.md.
CMD=(codex exec --json
    --disable chronicle
    -C "$WORKTREE"
    -s "$SANDBOX"
    -c 'model="gpt-5.6-sol"'
    -c "model_reasoning_effort=\"$EFFORT\""
    -c 'service_tier="priority"'
    -o "$RUN_DIR/last-message.md")
if command -v go >/dev/null 2>&1 && [[ "$SANDBOX" == "workspace-write" ]]; then
    CMD+=(--add-dir "$(go env GOCACHE)")
fi
if [[ -n "$RESUME" ]]; then
    CMD+=(resume "$RESUME" -)
fi

set +e
"${CMD[@]}" < "$RUN_DIR/brief.md" > "$RUN_DIR/events.jsonl" 2> "$RUN_DIR/stderr.log"
CODEX_EXIT=$?
set -e

THREAD="$RESUME"
if [[ -z "$THREAD" ]]; then
    THREAD="$(jq -r 'select(.type == "thread.started") | .thread_id' "$RUN_DIR/events.jsonl" 2>/dev/null | head -1)"
fi
printf '%s\n' "$THREAD" > "$RUN_DIR/thread-id"
[[ -n "$THREAD" ]] || echo "warning: no thread id in events.jsonl; --resume is unavailable for this run" >&2

# Cumulative task delta against the recorded base: covers committed, staged,
# unstaged, and (via add -A) untracked work in one apply-able patch.
BASE="$(cat "$TASK_DIR/base-sha" 2>/dev/null || git -C "$WORKTREE" merge-base main HEAD)"
git -C "$WORKTREE" add -A .
git -C "$WORKTREE" diff --binary "$BASE" > "$RUN_DIR/diff.patch" || true

echo "=== codex-implement result"
echo "task:      $TASK"
echo "worktree:  $WORKTREE ($(git -C "$WORKTREE" branch --show-current))"
echo "thread:    ${THREAD:-unknown}"
echo "exit:      $CODEX_EXIT"
echo "artifacts: ${RUN_DIR#"$REPO"/}"
echo "--- diffstat (vs task base)"
git -C "$WORKTREE" diff --stat "$BASE" | tail -20
echo "--- last message"
cat "$RUN_DIR/last-message.md" 2>/dev/null || echo "(none)"
exit "$CODEX_EXIT"
