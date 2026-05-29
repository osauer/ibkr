# Risk-plan orchestration

**Status:** Draft implementation brief.
**Created:** 2026-05-29 20:43 CEST
**Last update:** 2026-05-29 21:01 CEST
**Owner:** osauer
**Related:** [internal/cli/canary.go](../../internal/cli/canary.go), [docs/specs/risk-regime-dashboard.md](../specs/risk-regime-dashboard.md), [docs/reference/protocol.md](../reference/protocol.md)

## Purpose

`risk-plan` is the bridge between portfolio risk evidence and a future approval-gated order workflow.

The system should work like this:

1. Read current account, positions, regime, and canary evidence.
2. Produce a short list of candidate risk reductions or optional hedges.
3. Preview selected candidates through the broker what-if/order-preview path.
4. Show the previewed impact to the user or agent.
5. Submit only through a separate future execution command with explicit user confirmation.

The important boundary: `canary` and `regime` are evidence tools. They must not emit concrete trades. `risk-plan` is the first layer allowed to turn evidence into repositioning candidates, and those candidates still require preview before any execution path may see them.

## Small Tool Boundaries

The existing tools stay small:

- `account`: margin, cash, current cushion, and look-ahead margin.
- `positions`: exact holdings, grouped exposure, option legs, greeks, and bid/ask warnings.
- `regime`: market-stress evidence.
- `canary`: portfolio risk state.
- `chain`: option liquidity and candidate option-leg context.
- `quote`: executable-ish stock/ETF reference prices.
- `risk-plan`: policy engine that consumes the evidence tools and proposes repositioning candidates.
- `order-preview`: broker what-if, margin, commission, and rejection validator.

`risk-plan` does not replace the small tools. It is the only layer allowed to compose their outputs into a repositioning proposal.

## Why Canary And Regime Do Not Propose Trades

`regime` only knows the market backdrop. It does not know account constraints, held positions, option liquidity, or margin impact.

`canary` is portfolio-aware, but it is still a risk alarm, not an order planner. A concrete trade needs exact holdings, closing/opening semantics, current bid/ask quality, chain liquidity, greeks coverage, FX conversion, margin impact, and broker preview. If `canary` starts saying "sell 2 NOW calls", it becomes a planner, chain screener, and pre-trade validator in disguise.

The clean contract is:

- `regime`: "What is the market backdrop?"
- `canary`: "What risk state is this portfolio in?"
- `risk-plan`: "What candidate changes would reduce the detected risks?"
- `order-preview`: "Would the broker accept those candidates and what is the account impact?"
- future execution: "Has the user approved this exact previewed action?"

## Agent Contract

The LLM agent operates the workflow. It does not become the workflow.

The agent may:

- ask the user for missing policy choices
- invoke CLI or MCP tools
- summarize evidence and tradeoffs
- select candidates for preview only from IDs emitted by `risk-plan`
- present previewed broker impact

The agent must not:

- hand-write order drafts
- invent quantities, strikes, expiries, prices, hashes, or preview results
- convert `canary` prose into orders
- bypass stale-data, account, preview, or approval gates
- submit orders without a future explicit submit command and confirmation flow

Deterministic code owns calculations, freshness checks, candidate construction, preview validation, hashes, expiration, and hard stops.

## Core Flow

Default live CLI path:

```sh
ibkr risk-plan --json
ibkr order-preview --input risk-plan.json --select reduce-now-1 --json
```

Pipeline path:

```sh
ibkr risk-plan --json \
  | ibkr order-preview --input - --select reduce-now-1 --json
```

The live path should let `risk-plan` refresh its own inputs. Piping stale evidence into fresh chains and margin state is unsafe. Replay/debug modes can be added later, after the live path is stable.

MCP should start read-only:

- `ibkr_risk_plan`: returns candidate plans.
- `ibkr_order_preview`: acceptable only if it cannot transmit orders.
- No MCP order-submission tool in the first release.

## Risk-Plan MVP

First implementation should be intentionally narrow:

1. Fetch fresh `account`, `positions`, `regime`, and `canary`.
2. Check source freshness and account identity.
3. Identify target breaches from current data:
   - margin cushion and look-ahead cushion
   - gross exposure
   - net dollar delta
   - gross dollar delta
   - largest single-name market exposure
   - largest single-name dollar-delta exposure
   - missing or stale option greeks
4. Generate existing-position reductions only.
5. Estimate before/after risk effects.
6. Emit stable candidate IDs.
7. Require order preview for every candidate.

Optional hedges come after the reduction-only MVP works. Hedges should be opt-in, debit-only at first, premium-capped, and rejected when chains are stale, wide, or untradable.

## Candidate Ordering

Candidate priority should be deterministic:

1. Immediate margin relief.
2. Largest single-name concentration.
3. Gross dollar-delta reduction.
4. Net dollar-delta reduction.
5. Optional debit hedges.

Existing-position reductions should rank before new hedges. Closing known exposure is easier to preview, explain, and audit.

## Plan Artifact

Avoid over-specifying the JSON shape before implementation. For pickup, the plan only needs to carry these concepts:

- account ID
- generation time and expiration time
- source timestamps and source hashes
- policy settings used for the run
- detected risk breaches
- candidate IDs
- candidate rationale
- draft order intent for each candidate
- estimated risk effect before and after
- warnings and blocked reasons

Do not use raw `canary` or `regime` JSON as an order-preview input. `order-preview` should accept only a typed risk-plan artifact.

### Versioning

Do not bake `.v1` into command names, headings, or every sentence in the design doc. It adds noise and makes the design look more final than it is.

Because plans may be piped, stored, hashed, or accepted by MCP, the artifact should still carry a small identity field. Use versioning inside the artifact, not in the user-facing workflow:

```json
{ "kind": "risk_plan.v1" }
{ "kind": "order_preview.v1" }
```

The exact Go/RPC structs are implementation-owned. This document describes required semantics, not a frozen JSON schema.

## Order Preview And Future Execution

`order-preview` consumes a risk plan and selected candidate IDs. It validates broker impact without submitting:

- order validity
- broker rejection details
- current and post-trade margin
- current and post-trade look-ahead margin
- estimated commission
- relevant warnings

A future submit command should accept only a previewed artifact, not raw orders. It must validate:

- trading-capable build or runtime permission
- current account ID
- preview source hash
- plan and preview freshness
- candidate IDs named in the confirmation string
- current positions still match closing orders
- market data and margin state have not materially drifted

Any mismatch blocks submission.

## Canary Changes

No order-level fields belong in `canary`.

The current canary output is enough for the reduction-only MVP because it already reports:

- source timestamps
- decision and confidence
- cushion and look-ahead cushion
- gross exposure
- net delta and gross delta
- largest market and delta concentrations
- market-regime summary
- warnings

Optional future improvement: add machine-readable `risk_signals` and a `plan_hint`, but keep them as facts, not trades. Example signal names:

- `gross_delta_high`
- `net_delta_high`
- `largest_delta_concentration`
- `margin_cushion_low`
- `look_ahead_margin_cushion_low`
- `market_data_degraded`

`risk-plan` should still recompute from fresh account and positions data and treat canary signals as a consistency check.

## Naming: Canary Versus Risk

Keep `canary` for now.

Reasons to keep it:

- It describes an early-warning alarm, not a full risk system.
- It avoids implying VaR, factor models, stress scenarios, or execution advice.
- It is already present in CLI, MCP docs, examples, generated references, tests, and likely screenshots.
- It cleanly separates the compact monitor from the future `risk-plan` planner.

Reasons to dislike it:

- It is metaphorical.
- Users may search for "risk" and miss it.
- It does not naturally group with `risk-plan`.

Recommendation:

- Keep `ibkr canary` as the stable command.
- Add `risk-plan` as the planner.
- Later, if a broader risk namespace exists, add `ibkr risk check` as an alias for `ibkr canary`.
- Avoid a full rename until docs, examples, screenshots, plugin descriptions, tests, and external automation can be migrated deliberately.

## Implementation Sequence

1. Add `risk-plan` CLI with reduction-only candidates.
2. Add source freshness, account identity, and expiration checks.
3. Add a minimal typed artifact with `kind`, stable candidate IDs, and estimated before/after risk effects.
4. Wire `order-preview --input` to accept only risk-plan artifacts.
5. Add quote/chain gates for option candidate marketability.
6. Add optional debit hedge generation behind `--allow-hedges`.
7. Add read-only MCP `ibkr_risk_plan` after CLI behavior is stable.
8. Defer all order-submission work until preview, hash, expiry, account, and confirmation gates exist.

## Non-goals

- No automatic trading.
- No direct `canary | order-preview` path.
- No MCP order submission.
- No optimizer for a mathematically perfect portfolio.
- No tax-lot optimization in the first version.
- No naked short-option strategy generation.

## Open Questions

- Should default risk targets live in flags, config, or both?
- Should `risk-plan` persist the last generated plan by default?
- Should candidate selection live in `risk-plan` or only in `order-preview`?
- Should the first hedge implementation support SPY only, or SPY and QQQ?
