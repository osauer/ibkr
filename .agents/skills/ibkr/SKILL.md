---
name: ibkr
description: Use the local IBKR project tooling to answer account, position, P&L, quote, option-chain, scanner, calendar, history, technical, regime, canary, market-event, protection-proposal, settings/freeze, order-preview, and order-status questions. Prefer read-only MCP tools when available or `ibkr ... --json` when using the CLI. This skill is read-only and never runs broker writes; live agent-origin broker writes are blocked daemon-side.
---

Updated: 2026-06-11 19:32 CEST

## Contract

Use this skill when the user asks about their IBKR account, positions, exposure,
daily P&L, watchlist, quotes, calendars, option chains, daily history, scanners,
technical screens, fixed-fractional sizing, broad-market regime, dealer gamma,
market breadth, portfolio canary posture, held-name market events, protection
proposals, runtime settings/freeze state, order preview, or order status.

Prefer MCP tools for read-only snapshots when the `ibkr` MCP server is available.
Use the CLI with `--json` when the MCP surface is not available or when a project
workflow explicitly needs CLI output. Parse JSON before answering.

The MCP surface remains read-oriented for agents. Paper-account broker writes
are open to agents through the gated CLI flow; live agent-origin broker writes
are hard-blocked daemon-side. Never attempt live broker writes, live-trading
enablement, or destructive purge execution from an agent session. Order preview
can mint a local token; `token_minted` is not the same as `submit_eligible`.

## Output Discipline

- Always preserve and report `data_type` (`live`, `delayed`, `frozen`) when it
  matters to a decision.
- If quotes expose `stale`, `stale_reason`, `price_as_of`, or
  `session_context`, surface freshness and calendar context plainly.
- Nil JSON values mean unavailable, not zero. This matters for IV, Greeks, FX,
  open interest, money fields, and data-quality diagnostics.
- Render decision-making market/account data as compact Markdown tables or short
  summaries with the key freshness and quality fields included.
- Use `ibkr status --json` first when daemon/gateway access fails.

## Canonical References

This skill is the Codex-native wrapper. To avoid drift, detailed command
semantics and response schemas remain in the existing project references:

- [command catalog](../../../skills/ibkr/SKILL.md)
- [response schemas](../../../skills/ibkr/schemas.md)

Open those files when a command shape, flag, or field-level contract matters.

## Project Workflow

Read the root AGENTS.md before editing. For daemon/CLI/MCP/trading semantic
changes, use `docs/templates/daemon-cli-trading-contract.md`. For Canary SPA
changes, use `docs/templates/spa-authority-matrix.md`.

After daemon or CLI edits, the project requires:

```sh
make install
ibkr restart --timeout 15s
ibkr status --json
```

Then run a command that exercises the changed behavior and include that output
in the completion message. `make smoke` is the live gateway gate; a skip means
the live artifact was not exercised and must be reported as such.
