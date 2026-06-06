# Codex Workflow

Updated: 2026-06-06 07:19 CEST

This page is a navigation aid, not a second copy of the repo rules. The
canonical instructions live in [`AGENTS.md`](../../AGENTS.md); use this guide to
find the supporting surfaces quickly.

## Canonical Sources

- [`AGENTS.md`](../../AGENTS.md)
- [`docs/templates/daemon-cli-trading-contract.md`](../templates/daemon-cli-trading-contract.md)
- [`docs/templates/spa-authority-matrix.md`](../templates/spa-authority-matrix.md)
- [`.codex/`](../../.codex)
- [`.agents/skills/ibkr/SKILL.md`](../../.agents/skills/ibkr/SKILL.md)

## What To Remember

- `make help` is the target inventory.
- `make check` is the static gate.
- `make smoke` is the live gateway proof when it matters.
- Use read-only subagents for exploration and review, and keep writes in the
  main session.
- For broker-adjacent or SPA work, start from the canonical templates instead
  of re-deriving the contract here.
