# IBKR Portfolio Canary — Scheduled MCP Workflow

Updated: 2026-05-29 12:44 CEST

You are running a high-precision portfolio stress canary for a US-equity/options-heavy IBKR portfolio with some EU exposure. Use the read-only `ibkr_canary` MCP tool exactly once. Do not call order, execution, preview, modification, cancellation, or broker-submission tools.

Return a compact canary report in this shape:

```text
Portfolio Canary · <as_of>

Stage      [<overall stage>]
Confidence <confidence>
Action     <overall action>

| Title | Decision | Action |
|---|---|---|
| ... |

Warnings
- ...
```

Table columns:

| Title | Decision | Action |
|---|---|---|

Rules:

- The top summary is required. It must use `ibkr_canary.decision`, `ibkr_canary.confidence`, and `ibkr_canary.action`.
- Preserve the tool's row order and wording.
- Include every row returned by `ibkr_canary.rows`.
- Display decisions as user-facing stages: `HOLD` becomes `Go`; `WATCH` becomes `Watch`; `DE-LEVER` becomes `De-lever`; `LIQUIDATE` becomes `Liquidate`.
- Highlight the overall stage with brackets, e.g. `[Watch]`.
- Keep each row's `action` text exactly as returned by the tool.
- Include `Warnings` only when `ibkr_canary.warnings` is non-empty. Keep each warning as a bullet and preserve the tool wording.
- Do not add narrative before or after the report.
- If `decision` is `DE-LEVER` or `LIQUIDATE`, keep the concrete action text exactly aligned with the tool result.
- Treat `WATCH` ambiguity rows as real warnings: do not rewrite them as safe, but do not escalate them beyond the tool's decision.
- If `ibkr_canary` is unavailable, return the same report shape with `Stage` = `[Watch]`, `Confidence` = `low`, and one table row with `Title` = `Canary unavailable`, `Decision` = `Watch`, and `Action` = `Restart or update the MCP host so it exposes ibkr_canary; do not approximate this workflow with separate tools.`
