# Archived: the Codex delegation lane

Status: **retired 2026-07-19** (operator decision; Codex weekly window
exhausted while Claude capacity is plentiful). This document is the single
surviving reference. Everything else about the lane was removed from the
active instruction surfaces; git history retains the full implementation.

## What it was

From 2026-07-11 (policy) / 2026-07-18 (deterministic enforcement) to
2026-07-19, all code implementation in this repo was delegated to headless
Codex (`gpt-5.6-sol`) running in sibling git worktrees, while the
orchestrating Claude session kept planning, briefs, diff review, gates, and
integration. A `PreToolUse` hook blocked inline code edits (`.go`, `.js`,
`.sh`, `.html`, `.css`, `Makefile`, …) by Claude sessions and their
subagents; docs and config stayed direct. Guard rails grew in layers:

- `scripts/codex-implement.sh` — the delegation runner: worktree lifecycle
  (`../ibkr-codex-<task>` on `codex/<task>` branches), fail-closed sandbox
  pins, `--disable chronicle` (no re-delegation), per-run model/effort/
  service-tier pins, resume/cleanup, artifacts under `.claude/codex-runs/`.
- Budget guards: weekly-window gauge printed per run, launch gate at ≥70%
  (`--force-budget` to override), max two resume rounds (`--force-rounds`),
  `--effort low|medium|high|xhigh|max` (default `high`, `ultra` rejected).
- `.claude/hooks/implementation-lane.sh` (+ `_test.sh`) — the deterministic
  gate, registered in `.claude/settings.json`, tested by
  `make hook-behavior-check`.
- Oversteer valve: `scripts/waive-inline.sh <session-id> "<reason>"` wrote a
  48h session waiver under `.claude/state/inline-waivers/`; human-approved,
  never self-invoked by spawned agents (later relaxed to let the
  orchestrating session self-grant with a logged, announced reason).
- Hard-cap fallback: at gauge 100% or a Codex rate-limit refusal,
  implementation fell back to a Claude `implementer` agent in an isolated
  agent worktree (`.claude/agents/implementer.md`), same brief/review/gates.
- `.claude/agents/coder.md` — the delegation driver agent;
  `.claude/skills/codex-delegate/SKILL.md` — the full playbook
  (Brief → Run → Review → Gate → Iterate → Integrate → Clean up);
  `docs/guides/codex-workflow.md` — the navigation page.

Never delegated: broker writes, guardrail/freeze changes, releases,
`.claude`/hook configuration.

## Complete manifest

Deleted files (restorable from git history):

- `scripts/codex-implement.sh`
- `scripts/waive-inline.sh`
- `docs/guides/codex-workflow.md`
- `.claude/hooks/implementation-lane.sh`
- `.claude/hooks/implementation-lane_test.sh`
- `.claude/agents/coder.md`
- `.claude/agents/implementer.md`
- `.claude/skills/codex-delegate/SKILL.md`
- `internal/agentconfig/delegation_lifecycle_test.go`

Edits made at retirement (restore = revert these hunks):

- `.claude/settings.json`: removed allowlist entries
  `Bash(scripts/codex-implement.sh *)`, `Bash(scripts/waive-inline.sh *)`
  and the whole `hooks.PreToolUse` block registering the lane hook.
- `Makefile`: `hook-behavior-check` lost the
  `bash .claude/hooks/implementation-lane_test.sh` step;
  `agent-config-check` lost the three lane scripts from its `bash -n` list.
- `internal/agentconfig/config_test.go`: removed
  `TestDelegationRunnerStaysFailClosed`.
- `internal/cli/skill_drift_test.go`: removed
  `docs/guides/codex-workflow.md` and
  `.claude/skills/codex-delegate/SKILL.md` from the policy-phrase path list.
- `AGENTS.md`: removed the `codex-workflow.md` pointer, the lane bullets in
  "Work mode and delegation" (delegation mandate, oversteer clause,
  model/effort routing, hard-cap fallback, Codex-ready criteria,
  parallel-cluster flow), and the delegated-worktree offline-gates sentence
  in "Verification and evidence".
- `.gitignore`: removed the `.claude/codex-runs/` and `.claude/state/`
  ignore lines.
- `.claude/skills/release/SKILL.md`, `docs/guides/agent-session-hygiene.md`,
  `docs/design/risk-governance-nudges.md`,
  `docs/design/operator-ergonomics.md`, `.claude_memory.md`: lane
  references reworded to lane-neutral phrasing.

Kept deliberately (NOT part of the lane): the `.codex/` directory
(interactive Codex CLI trust config, broker-safety hook shim, execpolicy
rules, read-only reviewer roles), the `~/.codex` skill-install Makefile
targets, `internal/cli/origin.go` agent-origin markers, and the remaining
`internal/agentconfig` tests covering `.codex/`.

Transient state at retirement (not in git): ~15 run-artifact dirs under
`.claude/codex-runs/`, five active waivers under
`.claude/state/inline-waivers/`, and nine sibling worktrees
(`../ibkr-codex-*` on `codex/*` branches). Diffs and patches were backed up
locally before removal (see the retirement session report), then worktrees,
branches, and state were deleted.

## Reactivation runbook

The retirement spans two commits: the nine file deletions were swept into a
concurrent session's commit, and the policy/wiring edits (plus this file)
landed in the retirement commit proper. Both are self-locatable:

1. Find the deletion commit:
   `git log --diff-filter=D --format='%H %s' -1 -- scripts/codex-implement.sh`
   and the edits commit (the one that added this file):
   `git log --diff-filter=A --format='%H' -- docs/archive/codex-delegation-lane.md`
2. Restore the deleted files from the deletion commit's parent:
   `git checkout <deletion-sha>^ -- scripts/codex-implement.sh scripts/waive-inline.sh docs/guides/codex-workflow.md .claude/hooks .claude/agents .claude/skills/codex-delegate internal/agentconfig/delegation_lifecycle_test.go`
3. Revert the retirement edits in the surviving files (diff the edits
   commit for the exact hunks):
   `git show <sha> -- .claude/settings.json Makefile internal/agentconfig/config_test.go internal/cli/skill_drift_test.go AGENTS.md .gitignore`
   and re-apply in reverse (`git show <sha> -- <paths> | git apply -R`),
   then re-add the AGENTS.md policy bullets and doc references the same way.
   If a file has drifted too far for a clean reverse-apply (likeliest:
   `config_test.go`, `Makefile`), copy the removed hunks by hand from
   `git show <sha>^:<path>` instead of force-reverting.
4. Requirements: `codex` CLI ≥ 0.144 on PATH with an active subscription;
   `.codex/` project config trusted in a fresh Codex session (it was never
   removed).
5. Verify: `make agent-config-check` (includes `hook-behavior-check`) and a
   dry `scripts/codex-implement.sh --task probe --brief <file>` against a
   trivial brief; confirm the hook blocks an inline `.go` edit and that
   `scripts/waive-inline.sh` lifts it for the session.
6. Reinstate the policy text in `AGENTS.md` ("Work mode and delegation")
   and, if desired, the memory-digest entries that were scrubbed at
   retirement (working-style item, routing ladder, budget-guard facts) —
   the retirement commit's diff of `.claude_memory.md` has the wording.
