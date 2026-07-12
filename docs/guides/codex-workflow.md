# Codex Workflow

Updated: 2026-07-12 07:29 CEST

This page is a navigation aid, not a second copy of the repo rules. The
canonical instructions live in [`AGENTS.md`](../../AGENTS.md); use this guide to
find the supporting surfaces quickly.

## Canonical Sources

- [`AGENTS.md`](../../AGENTS.md)
- [`docs/templates/risk-policy-contract.md`](../templates/risk-policy-contract.md)
- [`docs/guides/trading-harness-development.md`](trading-harness-development.md)
- [`docs/templates/daemon-cli-trading-contract.md`](../templates/daemon-cli-trading-contract.md)
- [`docs/templates/spa-authority-matrix.md`](../templates/spa-authority-matrix.md)
- [`.codex/`](../../.codex)
- [`.agents/skills/ibkr-harness/SKILL.md`](../../.agents/skills/ibkr-harness/SKILL.md)

## What To Remember

- `make help` is the target inventory.
- `make check` is the static gate.
- `make smoke-fast` is the per-change live-gateway gate; the full
  `make smoke` wire matrix is binding for daemon/CLI/wire-path changes and
  releases. Both wait on `scripts/with-gateway-lock.sh` instead of racing
  other sessions for the gateway.
- Use read-only subagents for exploration and review, and keep writes in the
  main session.
- For broker-adjacent or SPA work, start from the canonical templates instead
  of re-deriving the contract here.

## How To Ask For Good Work

Give Codex four things: the outcome, the authority boundary, the evidence it
should use, and what makes the task done. State product constraints and safety
decisions; do not prescribe every search or tool call.

Useful prompt shapes:

- `Explain <surface> and trace its authority. Read-only; no edits.`
- `Review <diff/surface>. Findings first. Challenge the design and cite exact
  files/lines; do not implement.`
- `Implement <outcome>. Preserve <invariants>. Done means <behavior, targeted
  tests, and smoke tier>. Keep external writes out of scope.`
- `Use $ibkr-harness read-only to reconcile <question> against current status,
  rules, and data quality. Report insufficient data instead of guessing.`

For a broker write, the current user message must identify the transaction and
ask for the write explicitly. A request to analyze, monitor, prepare, preview,
or protect the portfolio does not authorize submission.

Avoid adding generic instructions such as “think step by step,” broad response
templates, or repeated brevity rules. Current reasoning models work best from a
short, direct outcome plus real constraints and acceptance evidence. See
[OpenAI model guidance](https://developers.openai.com/api/docs/guides/latest-model)
and [Codex AGENTS.md guidance](https://developers.openai.com/codex/guides/agents-md).

## Choosing The Working Shape

- Use one task for a coherent explore → implement → verify change.
- Ask for independent read-only subagents on repo-wide reviews, contract audits,
  or log/test analysis. Parallel writers are useful only after the batches are
  reviewed and non-overlapping.
- Start a fresh task or leave a compact handoff when the topic changes, after a
  long pause, or when moving from a large exploration into implementation.
- For risk-policy work, ask for a decision document and adversarial review
  before automation. A model can propose thresholds, but only the user can
  approve them as policy.

## Headless Delegation (Orchestrated)

When another session orchestrates (typically Claude planning and reviewing),
delegate one bounded implementation task per run:

```sh
scripts/codex-implement.sh --task <name> --brief <file>
```

The runner creates `../ibkr-codex-<name>` from local `main`, runs `codex exec`
under a workspace-write seatbelt with fail-closed approvals, and captures
`brief.md`, `events.jsonl`, `last-message.md`, `thread-id`, and `diff.patch`
under `.claude/codex-runs/<name>/`. The orchestrator reviews the diff against
the brief, runs the gates, iterates with `--resume <thread-id>`, integrates
by applying the reviewed patch in the primary tree, and finishes with
`--cleanup` (worktree and branch removal; the runner refuses fresh tasks
over leftovers, so skipped cleanup surfaces instead of littering). The full loop,
brief template, and sandbox facts live in
[`.claude/skills/codex-delegate/SKILL.md`](../../.claude/skills/codex-delegate/SKILL.md).
Broker writes, guardrail changes, and release work are never delegated.
`internal/agentconfig` gates the runner's fail-closed shape.

## Completion Evidence

For instruction/docs/config-only changes, run the targeted check and
`make check`. For Go or runtime behavior, run `make test` once (it already
includes `check`). Add the smoke tier required by root `AGENTS.md`. For rendered
SPA behavior, include browser/DOM evidence. Redact private account and order
data from the final message.
