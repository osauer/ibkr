---
name: codex-delegate
description: Delegate a bounded implementation task to headless Codex (gpt-5.6-sol) in a sibling worktree while this session keeps planning, review, judgement, and integration. Use when asked to delegate/hand off coding to Codex, to implement via Codex, or to run reviewed independent fix batches in parallel. Never for broker writes, guardrail changes, or releases.
---

Updated: 2026-07-12 09:26 CEST

# Codex delegation loop

Division of labor is fixed: this session plans, specs, reviews, judges, and
integrates. Codex implements inside an isolated sibling worktree created from
local `main`. The primary working tree and its in-flight changes are never the
delegate's workspace.

## When to delegate

Good: bounded, well-specified implementation — a reviewed fix batch, a
mechanical refactor, test scaffolding, a feature slice whose contract is
already decided. Independent tasks may run as parallel delegations under
distinct task names.

Exploration and review delegation stays with native read-only Claude
subagents (root `AGENTS.md`); they are lower-friction and need no runner.
This lane exists for one thing: a second model family implementing. Use a
codex `--read-only` run only when a second-model opinion is explicitly
wanted.

Never delegate: broker writes or anything requiring live-order interaction;
trading guardrail or freeze/settings changes; risk-policy threshold decisions;
release targets; `.codex`/hook/rule edits. Policy decisions return to the
user, per root `AGENTS.md`.

## The loop

1. **Brief** — write the task brief to a file (scratchpad is fine). Give it
   the four things from `docs/guides/codex-workflow.md`: outcome, authority
   boundary, evidence to use, and what done means. State invariants to
   preserve and the required test evidence. State that ibkr usage is
   read-only and new dependencies are out of scope unless the brief says
   otherwise. Do not prescribe every step.

2. **Run** —

   ```sh
   scripts/codex-implement.sh --task <name> --brief <file>
   ```

   Task names are lowercase-kebab; the worktree is `../ibkr-codex-<name>` on
   branch `codex/<name>`. The runner enforces the lifecycle: a fresh task
   refuses to start over a leftover worktree or branch (finish it with
   `--cleanup` first), `--resume` refuses to run once the worktree is
   gone, and a missing or empty brief is rejected before any git state is
   created. Long tasks: generous Bash timeout or background the call.
   Artifacts land in `.claude/codex-runs/<name>/<stamp>/`: `brief.md`,
   `events.jsonl`, `last-message.md`, `thread-id`, `diff.patch` —
   `diff.patch` is the cumulative task delta against the recorded base
   commit, correct even if Codex commits inside its worktree.

3. **Review** — senior review, in this session. Read `diff.patch` fully and
   judge it against the brief. Trust the diff, not the report; the last
   message is a claim, the diff is the evidence. Check invariants, layer
   boundaries (daemon/risk/rpc own policy; adapters must not re-create it),
   idiom (`make check` enforces modern Go), and scope creep.

4. **Gate** — run the repo gates against the worktree:

   ```sh
   git -C ../ibkr-codex-<name> status --porcelain   # scope check
   make -C ../ibkr-codex-<name> test                # binding for Go changes
   ```

   Gateway-touching smokes serialize via `scripts/with-gateway-lock.sh` and
   normally run from the primary tree after integration, not per-worktree.

5. **Iterate** — feed precise review findings back to the same thread:

   ```sh
   scripts/codex-implement.sh --task <name> --resume $(cat .claude/codex-runs/<name>/<stamp>/thread-id) --brief <feedback-file>
   ```

   The thread keeps its prior context, and the worktree must still exist —
   never clean up between rounds. Two or three rounds is normal; if it is
   not converging, stop, take over in the main session, and say so.

6. **Integrate** — once accepted, land it from the primary tree. Prefer
   applying the reviewed patch to the primary tree over merging the branch
   (matches the no-branch working style):

   ```sh
   git apply --3way .claude/codex-runs/<name>/<stamp>/diff.patch
   ```

   Re-run `make test` in the primary tree. Commit only when the user asks.

7. **Clean up** — after the patch is applied and the primary-tree gates are
   green (never between iteration rounds):

   ```sh
   scripts/codex-implement.sh --task <name> --cleanup
   ```

   This removes the worktree and branch, and is idempotent — it recovers
   even when the worktree directory was removed out-of-band. Artifacts
   stay under `.claude/codex-runs/<name>/` as the gitignored audit trail;
   prune them once the work is committed. An abandoned task ends the same
   way — run `--cleanup` and nothing ever touched the primary tree.

## Execution model and safety facts

- Headless runs have no approver: sandbox escalations and execpolicy
  prompt/forbidden decisions fail closed. Do not add bypass flags to the
  runner; that shape is the design.
- The seatbelt allows writes only to the worktree, tmp, and the Go build
  cache (`--add-dir "$(go env GOCACHE)"` — without it `go build` fails on
  `~/Library/Caches/go-build`). Direct network access is denied, so module
  downloads fail: adding dependencies is a main-session decision.
- Sibling worktrees inherit Codex project trust from `~/dev`, so repo
  `AGENTS.md`, skills, and `.codex/config.toml` load. Hook trust is pinned
  to the primary repo path and does not follow worktrees — treat hooks as
  absent there. Daemon agent-origin gating is the binding broker boundary
  either way; briefs keep ibkr usage read-only.
- Codex reaches live read-only data through the `ibkr` MCP server (spawned
  outside the sandbox); that surface cannot submit orders.
- Model and effort come from the user's Codex config (`gpt-5.6-sol`;
  requires codex-cli ≥ 0.144). Per-run overrides are deliberately not
  exposed; the user's config is the single knob.
- Launching write-mode headless Codex is itself a gated action in Claude
  sessions: expect a permission prompt per run in interactive sessions. In
  autonomous sessions it is denied unless the user has allowlisted
  `Bash(scripts/codex-implement.sh *)` in `.claude/settings.json` — that
  grant is the user's to make, not the agent's.
