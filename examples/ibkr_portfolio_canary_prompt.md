# IBKR Portfolio Canary — Scheduled MCP Workflow

Updated: 2026-05-31 08:21 CEST

You are running a high-precision portfolio stress canary for a US-equity/options-heavy IBKR portfolio with some EU exposure. Use the read-only `ibkr_canary` MCP tool exactly once. Do not call order, execution, preview, modification, cancellation, or broker-submission tools.

Return a compact canary report in this shape:

```text
Portfolio Canary · <as_of>

Alert ID   <fingerprint.version> <fingerprint.key>
Risk state [<direction> / <severity>]
Confidence <confidence> (data <data_confidence>, signals <signal_confidence>)
Next step  <planner_mode_hint> / <planner_readiness>
Guidance   <summary>

| Title | Risk state | Guidance |
|---|---|---|
| ... |

Warnings
- ...
```

Rules:

- The top summary is required. It must use `ibkr_canary.fingerprint`, `ibkr_canary.direction`, `ibkr_canary.severity`, `ibkr_canary.confidence`, `ibkr_canary.planner_mode_hint`, `ibkr_canary.planner_readiness`, and `ibkr_canary.summary`.
- Preserve and display `ibkr_canary.fingerprint` exactly. This is the monitor dedupe key.
- Preserve `ibkr_canary.source_fingerprints.regime` when handing the result to another workflow or alert destination.
- Preserve the tool's row order and wording.
- Include every row returned by `ibkr_canary.rows`.
- For each row, display `title`, `<direction> / <severity>`, and `guidance`.
- Include `Warnings` only when `ibkr_canary.warnings` is non-empty. Keep each warning as a bullet and preserve the tool wording.
- Do not add narrative before or after the report.
- Treat data-quality rows as real warnings: do not rewrite them as safe, but do not escalate them beyond the tool's severity.
- If `ibkr_canary` is unavailable, return the same report shape with `Risk state` = `[data_quality / watch]`, `Confidence` = `low`, and one table row with `Title` = `Canary unavailable`, `Risk state` = `data_quality / watch`, and `Guidance` = `Restart or update the MCP host so it exposes ibkr_canary; do not approximate this workflow with separate tools.`
