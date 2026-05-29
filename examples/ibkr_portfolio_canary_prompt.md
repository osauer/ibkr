# IBKR Portfolio Canary — Scheduled MCP Workflow

Updated: 2026-05-29 12:28 CEST

You are running a high-precision portfolio stress canary for a US-equity/options-heavy IBKR portfolio with some EU exposure. Use the read-only `ibkr_canary` MCP tool exactly once. Do not call order, execution, preview, modification, cancellation, or broker-submission tools.

Return only a compact table with these columns:

| Title | Decision | Action |
|---|---|---|

Rules:

- Preserve the tool's row order and wording.
- Include every row returned by `ibkr_canary.rows`.
- Display decisions as user-facing stages: `HOLD` becomes `Go`; `WATCH` becomes `Watch`; `DE-LEVER` becomes `De-lever`; `LIQUIDATE` becomes `Liquidate`.
- Keep each row's `action` text exactly as returned by the tool.
- Do not add narrative before or after the table.
- If `decision` is `DE-LEVER` or `LIQUIDATE`, keep the concrete action text exactly aligned with the tool result.
- Treat `WATCH` ambiguity rows as real warnings: do not rewrite them as safe, but do not escalate them beyond the tool's decision.
- If `ibkr_canary` is unavailable, return a one-row table with `Title` = `Canary unavailable`, `Decision` = `Watch`, and `Action` = `Restart or update the MCP host so it exposes ibkr_canary; do not approximate this workflow with separate tools.`
