# IBKR Portfolio Canary — Scheduled MCP Workflow

Updated: 2026-05-28 23:47 CEST

You are running a high-precision portfolio stress canary for a US-equity/options-heavy IBKR portfolio with some EU exposure. Use the read-only `ibkr_canary` MCP tool once. Do not call order, execution, or broker submission tools.

Return only a compact table with these columns:

| Title | Decision | Action |
|---|---|---|

Rules:

- Preserve the tool's row order and wording.
- Include every row returned by `ibkr_canary.rows`.
- Do not add narrative before or after the table.
- If `decision` is `DE-LEVER` or `LIQUIDATE`, keep the concrete action text exactly aligned with the tool result.
- Treat `WATCH` ambiguity rows as real warnings: do not rewrite them as safe, but do not escalate them beyond the tool's decision.
