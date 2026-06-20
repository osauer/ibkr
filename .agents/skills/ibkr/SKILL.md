---
name: ibkr
description: Use the local IBKR project tooling to answer account, position, P&L, quote, option-chain, scanner, calendar, history, technical, regime, canary, market-event, protection-proposal, opportunity, offline opportunity backtest/research diagnostics, settings/freeze, order-preview, order-status, and order-history questions. Prefer read-only MCP tools when available or `ibkr ... --json` when using the CLI. This skill is read/preview-first by default; explicit broker-write requests must use the gated CLI path and report the returned artifact.
---

Updated: 2026-06-20 00:00 CEST

## Contract

Use this skill when the user asks about their IBKR account, positions, exposure,
daily P&L, watchlist, quotes, calendars, option chains, daily history, scanners,
technical screens, fixed-fractional sizing, broad-market regime, dealer gamma,
market breadth, portfolio canary posture, held-name market events, protection
proposals, option-exercise opportunities, runtime settings/freeze state, order
preview, order status, or order history.

Prefer MCP tools for read-only snapshots when the `ibkr` MCP server is available.
Use the CLI with `--json` when the MCP surface is not available or when a project
workflow explicitly needs CLI output. Parse JSON before answering.

Use `ibkr opportunities status --json`, `ibkr opportunities list --json`, or
`ibkr opportunities refresh --json` only for read-only opportunity discovery.
`ibkr opportunities preview`, `ibkr opportunities exercise`, and
`ibkr opportunities ignore` are outside this read-only skill contract.

`ibkr backtest research-opportunity ...` is an offline/local research harness,
not a daemon opportunity feed and not a broker-action surface. Use it only when
the user explicitly asks to inspect scored opportunity research/backtest files.
Treat `not_advice`, `evidence.status`, `evidence.reasons`, feature diagnostics,
and reason diagnostics as the answer; never translate a passing/strong
diagnostic into alpha proof, a live trade recommendation, or an order
preview/place request.

Use `ibkr orders history --json` only as recent local order-journal evidence for
the current account/mode. It is not an IBKR Activity Statement, Flex
query/export, trade confirmation, commission ledger, closed-position ledger, or
broker-grade historical audit. Prefer `ibkr orders open` for current working
orders and `ibkr order status ID` for one order's full local audit trail.

The MCP surface remains read-oriented for agents. Explicit broker writes,
including live writes, are allowed only through the gated CLI flow: trading
status must be write-ready, preview tokens and broker checks must pass, and the
CLI JSON result is the artifact. Never attempt live-trading enablement, settings
freeze changes, or destructive daemon maintenance from an agent session. Order
preview can mint a local token; `token_minted` is not the same as
`submit_eligible`.

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
- For opportunity backtest/research output, say "diagnostic only" unless the
  JSON explicitly clears the evidence gate; even `promising_diagnostic` is not
  alpha proof without locked walk-forward/live paper evidence.
- Surface `diagnostics.features[]` and `diagnostics.reasons[]` as diagnostics,
  not prescriptions.

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

After daemon or CLI edits, refresh the installed daemon and capture artifacts:

```sh
make restart-daemon
ibkr status --json
```

Then run a command that exercises the changed behavior and include that output
in the completion message. `make smoke-fast` is the default per-change live
gateway gate; full `make smoke` is binding for daemon/CLI/wire-path changes and
release work. A skip means the live artifact was not exercised and must be
reported as such.
