# IBKR Portfolio Canary — Scheduled MCP Workflow

Updated: 2026-06-03 07:06 CEST

You are running a high-precision stateless portfolio canary for a US-equity/options-heavy IBKR portfolio with some EU exposure. Use the read-only `ibkr_canary` MCP tool exactly once. Do not call order, execution, preview, modification, cancellation, or broker-submission tools.

Return a compact canary report in this shape:

```text
Portfolio Canary · <as_of>

Action      <action> · <market_confirmation> market · <portfolio_fit> portfolio fit
Guidance    <summary>
Next step   <planner_mode_hint> / <planner_readiness>

Why this fired
  Market weather   <market_confirmation> — <market evidence from market / rows>
  Portfolio shape  <portfolio_fit> — <portfolio evidence from portfolio / rows>
  Combined read    <one sentence explaining why action is or is not executable>

Input health
  Overall          <input_health>
  Sources          <source_health summary>

Warnings
- ...

Alert ID <fingerprint.version> <fingerprint.key>
```

Rules:

- The top summary is required. It must use `ibkr_canary.action`, `ibkr_canary.market_confirmation`, `ibkr_canary.portfolio_fit`, `ibkr_canary.input_health`, `ibkr_canary.planner_mode_hint`, `ibkr_canary.planner_readiness`, and `ibkr_canary.summary`.
- Preserve and display `ibkr_canary.fingerprint` exactly. This is the monitor dedupe key.
- Preserve `ibkr_canary.source_fingerprints.account`, `ibkr_canary.source_fingerprints.positions`, and `ibkr_canary.source_fingerprints.regime` when handing the result to another workflow or alert destination.
- Display `source_health[]` compactly and treat stale/degraded/partial statuses as readiness evidence.
- Use `ibkr_canary.rows` as supporting evidence, but do not print every row by default. Include only rows that explain the top action or an input-health blocker.
- Include `Warnings` only when `ibkr_canary.warnings` is non-empty. Keep each warning as a bullet and preserve the tool wording.
- Do not add narrative before or after the report.
- Do not convert account-only margin/P&L facts into a canary DEFEND action. DEFEND requires top-level `market_confirmation=confirmed`, vulnerable `portfolio_fit`, and clean enough `input_health`.
- Treat input-health rows as real blockers or limitations: do not rewrite them as safe, but do not escalate them beyond the tool's top-level `action`.
- If `ibkr_canary` is unavailable, return the same report shape with `Action = confirm_inputs`, `input_health = failed`, and guidance to restart or update the MCP host so it exposes `ibkr_canary`; do not approximate this workflow with separate tools.
