# Risk-plan orchestration

**Status:** Draft implementation design.
**Created:** 2026-05-29 20:43 CEST
**Last update:** 2026-05-29 20:52 CEST
**Owner:** osauer
**Related:** [internal/cli/canary.go](../../internal/cli/canary.go), [docs/specs/risk-regime-dashboard.md](../specs/risk-regime-dashboard.md), [docs/reference/protocol.md](../reference/protocol.md)

## Summary

`risk-plan` is the proposed bridge between read-only portfolio risk evidence and a future approval-gated order workflow.

The overall process is:

1. Gather evidence from `account`, `positions`, `regime`, and `canary`.
2. Convert evidence into a typed `risk_plan.v1` artifact containing candidate reductions or optional hedges.
3. Preview selected candidates through broker-side order preview.
4. Present previewed impact to the user or agent.
5. Submit only through a future explicitly approved execution path that validates the preview hash, artifact freshness, account identity, and user confirmation.

`canary` and `regime` must not emit concrete trades. They are evidence surfaces. `risk-plan` is the first layer allowed to produce order drafts, and even those drafts are not executable until `order-preview` accepts them.

The current public CLI/MCP boundary remains read-only. Any order-submission path belongs behind a separate, explicit execution capability.

## Current Inputs

The project already has the evidence surfaces needed for a first implementation:

- `account`: account ID, base currency, net liquidation, cash, buying power, current margin cushion, look-ahead margin cushion, available funds, and margin context.
- `positions`: exact stock and option holdings, grouped exposure by underlying, base-currency market value, base-currency dollar delta, option greeks coverage, option bid/ask context, and warning details.
- `regime`: market-stress evidence across volatility, credit, funding, FX, dealer gamma, and breadth.
- `canary`: portfolio risk posture using account, positions, and regime evidence. It returns `HOLD`, `WATCH`, `DELEVER`, or `LIQUIDATE`, with confidence, source timestamps, portfolio summary, market summary, row-level actions, and warnings.
- `quote` and `chain`: marketability checks for candidate stock/ETF or option legs.
- `order-preview`: in-progress broker what-if layer. This design assumes it can validate draft orders without transmitting them.

## Design Decision

`canary` does not need concrete reposition instructions for `risk-plan` to work. It should remain a classifier and evidence summary. It may add machine-readable risk signals later, but those signals should still be facts such as "gross delta is above target", not trades such as "sell 2 contracts".

`risk-plan` should orchestrate the evidence tools internally. Piping remains useful for replay and debugging, but the canonical live path should refresh its own account, positions, regime, and canary snapshots so the plan is built from coherent state.

The LLM agent is an operator and explainer. Deterministic tools own source data, numeric calculations, candidate construction, preview validation, hashing, expiration, and hard stops.

## Why Canary And Regime Must Not Propose Trades

`regime` is market-context only. It can report that the tape is stressed, but it does not know the user's positions, account constraints, option liquidity, or margin impact.

`canary` is portfolio-aware, but it is still not an order planner. A concrete reposition requires:

- exact current holdings and quantities
- whether a proposed order is closing or opening exposure
- option bid/ask and spread quality
- chain liquidity and tradability
- greeks coverage for delta/gamma/theta/vega estimates
- account currency and FX conversion
- current and look-ahead margin impact
- broker-side rejection, commission, and what-if results

If `canary` emits specific trades, it silently becomes a planner, chain screener, liquidity checker, margin preflight, and order-drafting layer. That would make the risk alarm harder to test and easier for agents to misuse.

The safe contract is:

- `regime`: "What is the market backdrop?"
- `canary`: "What risk state is this portfolio in?"
- `risk-plan`: "What candidate changes would reduce the detected risks?"
- `order-preview`: "Would the broker accept these candidates, and what is the account impact?"
- execution layer: "Has the user explicitly approved this exact previewed action?"

## Actors And Authority

### User

The user owns risk policy and approval:

- whether to generate reductions only or allow hedges
- maximum hedge premium
- maximum single-name exposure
- maximum net and gross delta
- whether covered option sells are allowed
- which candidates to preview
- whether any future previewed order may be submitted

### Agent

The agent may:

- invoke CLI or MCP tools
- ask the user for missing policy choices
- summarize evidence
- explain candidate tradeoffs
- select candidates for preview only from IDs emitted by `risk-plan`
- present previewed broker impact

The agent must not:

- hand-write order drafts
- invent quantities, strikes, expiries, prices, or hashes
- convert `canary` prose into orders
- bypass stale-data, preview, or account guards
- submit orders without an explicit future submit command and confirmation flow

The agent operates the checklist. It is not the checklist.

### Deterministic Tools

The tools own the stateful and numeric work:

- `account` supplies hard account constraints.
- `positions` supplies the reduction surface.
- `regime` supplies market-stress context.
- `canary` classifies portfolio risk state.
- `quote` and `chain` validate marketability.
- `risk-plan` computes target breaches, candidate quantities, and candidate risk effects.
- `order-preview` validates broker-side impact.
- a future submitter validates preview hash, expiry, account identity, user confirmation, and kill switches.

## Live Workflow

### CLI

Default live planning:

```sh
ibkr risk-plan --json
```

Preview every candidate that is previewable:

```sh
ibkr risk-plan --json \
  | ibkr order-preview --input - --json
```

Preview only selected candidates:

```sh
ibkr risk-plan --json > risk-plan.json
ibkr order-preview --input risk-plan.json --select reduce-now-1,hedge-spy-1 --json
```

Diagnostic replay from saved evidence:

```sh
ibkr canary --json > canary.json
ibkr risk-plan --from-canary canary.json --refresh account,positions,regime --json
```

The `--refresh` behavior is load-bearing. A stale `canary` file must not be combined with fresh option chains and current margin as if all inputs came from one snapshot.

### MCP

The first MCP exposure should be read-only:

- `ibkr_risk_plan`: returns a `risk_plan.v1` artifact.
- `ibkr_order_preview`: only if preview is read-only and cannot transmit.

There should be no MCP order-submission tool in the first release of this workflow.

### Scheduled Monitoring

Scheduled monitoring may run `canary` frequently because it is a compact risk alarm. It should not preview or submit by itself.

Recommended policy:

1. Run `ibkr canary --json`.
2. If `decision=HOLD`, log and stop.
3. If `decision=WATCH`, optionally run `risk-plan` and stage candidates, but do not preview unless policy says so.
4. If `decision=DELEVER` or `decision=LIQUIDATE`, run `risk-plan`, present highest-priority candidates, and ask for preview approval.
5. If margin danger is immediate, `risk-plan` prioritizes closing existing exposure and raising liquidity before optional hedges.
6. If sources are stale, degraded, computing, delayed, or below required confidence, report the blocker and ask whether to wait, refresh, or run diagnostics.

## Risk-Plan Responsibilities

`risk-plan` owns policy composition and candidate generation:

- Fetch a coherent live snapshot from account, positions, regime, and canary.
- Hash each source payload.
- Enforce source freshness.
- Reject account-ID mismatches.
- Convert risk posture into target breaches: margin, gross exposure, net delta, gross delta, largest single-name market value, largest single-name delta, and option-greeks quality.
- Generate candidate reductions from existing positions first.
- Generate optional hedges only when the user or policy explicitly allows hedges.
- Query quotes/chains only for candidates that need executable market context.
- Reject candidates with stale quotes, missing bid/ask, untradable chains, excessive spreads, missing greeks, or ambiguous closing/opening semantics.
- Emit stable candidate IDs.
- Emit estimated before/after risk effects.
- Mark every candidate as preview-required.
- Set `expires_at` on the plan.

`risk-plan` does not submit orders.

## Candidate Ordering

Candidate priority should be deterministic and conservative:

1. Immediate margin safety: close exposure that improves current or look-ahead margin.
2. Concentration: reduce the largest single-name dollar-delta or market-value concentration.
3. Gross dollar delta: reduce offsetting option books that still carry large gross risk.
4. Net dollar delta: reduce broad directional exposure.
5. Optional hedges: add debit-only index hedges when reductions are insufficient or user policy prefers hedging.

Existing-position reductions should be preferred before opening new hedges. Closing known risk is easier to preview, easier to explain, and less likely to introduce new expiry, volatility, or liquidity risk.

## Account Role

`account` is the hard constraint source. `risk-plan` should read:

- account ID
- base currency
- net liquidation
- current cushion
- look-ahead cushion
- available funds
- excess liquidity
- buying power
- maintenance margin
- initial margin
- account type

If margin is dangerous, margin relief outranks every other objective. A hedge that costs premium or increases margin should not outrank closing existing exposure during margin stress.

## Positions Role

`positions` supplies the actual reduction surface:

- stock quantities
- option quantities
- option expiry, strike, right, and multiplier
- market value in account base currency
- per-underlying dollar delta in account base currency
- greeks and greeks coverage
- option bid/ask and mark warnings
- grouped exposure by underlying

Candidate reductions must be derived from current positions, not from canary text. Closing sides must be explicit:

- long stock: `SELL`
- short stock: `BUY_TO_COVER`
- long option: `SELL_TO_CLOSE`
- short option: `BUY_TO_CLOSE`

Opening hedges must be marked as opening and must require explicit hedge permission.

## Canary Contract

The current canary output is sufficient for a first `risk-plan` implementation because it already carries:

- source timestamps
- overall decision and confidence
- account cushion
- look-ahead cushion
- gross exposure
- net delta
- gross delta
- largest market concentration
- largest delta concentration
- market-regime summary
- warnings

No order-level fields belong in `canary`.

Optional future additions can make planning more deterministic:

```json
{
  "policy_version": "canary.v1",
  "plan_hint": "stage_reductions",
  "risk_signals": [
    {
      "code": "gross_delta_high",
      "severity": "watch",
      "value_pct_nlv": 391,
      "soft_target_pct_nlv": 250,
      "hard_target_pct_nlv": 150
    },
    {
      "code": "largest_delta_concentration",
      "underlying": "NOW",
      "severity": "watch",
      "value_pct_nlv": 118,
      "soft_target_pct_nlv": 75,
      "hard_target_pct_nlv": 35
    }
  ]
}
```

These are still risk facts, not trades. If `risk_signals` disagree with fresh account or positions data, `risk-plan` should prefer fresh account and positions data and report the mismatch.

## Risk Plan Artifact

The first implementation needs one planner artifact.

```json
{
  "kind": "risk_plan.v1",
  "generated_at": "2026-05-29T20:52:00+02:00",
  "expires_at": "2026-05-29T20:57:00+02:00",
  "account_id": "DU123456",
  "decision": "WATCH",
  "confidence": "medium-low",
  "source_as_of": {
    "account": "2026-05-29T18:45:10+02:00",
    "positions": "2026-05-29T20:45:10+02:00",
    "regime": "2026-05-29T20:45:18+02:00",
    "canary": "2026-05-29T20:45:18+02:00"
  },
  "source_hashes": {
    "account": "sha256:...",
    "positions": "sha256:...",
    "regime": "sha256:...",
    "canary": "sha256:..."
  },
  "policy": {
    "mode": "reductions_only",
    "allow_hedges": false,
    "allow_option_sells": "closing_only",
    "max_hedge_premium_pct_nlv": 0.75,
    "max_single_name_delta_pct_nlv": 75,
    "max_net_delta_pct_nlv": 150,
    "max_gross_delta_pct_nlv": 250
  },
  "breaches": [
    {
      "code": "gross_delta_high",
      "value_pct_nlv": 391,
      "target_pct_nlv": 250
    },
    {
      "code": "largest_delta_concentration",
      "underlying": "NOW",
      "value_pct_nlv": 118,
      "target_pct_nlv": 75
    }
  ],
  "candidates": [
    {
      "id": "reduce-now-1",
      "priority": 1,
      "intent": "reduce_single_name_delta",
      "scope": "existing_position",
      "rationale": "NOW is the largest dollar-delta concentration.",
      "estimated_effect": {
        "largest_delta_pct_nlv_before": 118,
        "largest_delta_pct_nlv_after": 75,
        "gross_delta_pct_nlv_before": 391,
        "gross_delta_pct_nlv_after": 348
      },
      "order_drafts": [
        {
          "side": "SELL_TO_CLOSE",
          "sec_type": "OPT",
          "symbol": "NOW",
          "expiry": "YYYY-MM-DD",
          "strike": 0,
          "right": "C",
          "quantity": 1,
          "order_type": "LMT",
          "limit_policy": "passive_mid"
        }
      ],
      "gates": [
        "existing_position_matched",
        "closing_only",
        "requires_preview",
        "requires_user_approval"
      ]
    }
  ],
  "blocked_reasons": [],
  "warnings": [
    "gamma cluster degraded; plan is based on portfolio exposure and available market evidence"
  ]
}
```

The example uses placeholder strike and expiry values. Real implementation must populate them from `positions`, never from canary prose or agent text.

## Order Preview Artifact

`order-preview` consumes `risk_plan.v1` and a set of selected candidate IDs. It returns a preview artifact.

```json
{
  "kind": "order_preview.v1",
  "generated_at": "2026-05-29T20:53:00+02:00",
  "expires_at": "2026-05-29T20:58:00+02:00",
  "account_id": "DU123456",
  "source_plan_hash": "sha256:...",
  "selected_candidates": ["reduce-now-1"],
  "accepted_candidates": ["reduce-now-1"],
  "rejected_candidates": [],
  "margin_impact": {
    "current_excess_liquidity": 0,
    "post_trade_excess_liquidity": 0,
    "current_look_ahead_excess_liquidity": 0,
    "post_trade_look_ahead_excess_liquidity": 0
  },
  "cost_impact": {
    "estimated_commission": 0,
    "estimated_slippage": 0
  },
  "submit_allowed": false,
  "warnings": []
}
```

`submit_allowed=false` is intentional for the first preview implementation. A future order-capable build can add an approval artifact, but preview should remain non-submitting.

## Future Execution Connection

If order submission is added, it should not accept raw order JSON. It should accept only an approved preview artifact:

```sh
ibkr order-submit --input order-preview.json --confirm "SUBMIT reduce-now-1"
```

The submitter must validate:

- trading-capable build or runtime permission is enabled
- account ID matches current gateway account
- preview hash matches the selected plan
- preview has not expired
- plan has not expired
- market data is not delayed unless policy explicitly allows delayed data
- selected candidates were accepted by preview
- order drafts still match current positions for closing orders
- current margin is not worse than preview assumptions beyond tolerance
- user confirmation string names the exact candidate IDs

Any mismatch blocks submission.

## Reliability Invariants

These invariants are required before this workflow is safe for a large account:

- Typed artifacts only: `order-preview` refuses raw `canary`, raw `regime`, and arbitrary agent-written JSON.
- Freshness gates: live planning requires all source timestamps inside configured tolerances.
- Preview gate: every candidate must pass broker preview before any future submission path can see it.
- Hash gate: preview and future submission artifacts carry the source plan hash.
- Expiry gate: stale plans and stale previews are rejected.
- Scope gate: closing orders must match an existing position; opening hedges must be explicitly marked as opening.
- Permission gate: hedges, covered option sells, and any future submit action require explicit user policy.
- Kill switch: any gateway disconnect, account mismatch, delayed data, or margin-preview rejection blocks execution.
- Audit trail: every artifact includes generated time, source hashes, policy, candidates, rejections, and warnings.

## Defaults

First implementation defaults:

- reductions only
- no naked short options
- no opening option trade unless `--allow-hedges` is set
- debit hedges only when enabled
- maximum hedge premium defaults to `0.75%` of net liquidation
- no candidate from stale option quotes or untradable chains
- no submission path

## Command Surface

Minimal CLI:

```text
ibkr risk-plan [--json]
               [--allow-hedges]
               [--max-hedge-premium-pct-nlv N]
               [--max-single-name-delta-pct-nlv N]
               [--max-net-delta-pct-nlv N]
               [--max-gross-delta-pct-nlv N]
               [--from-canary PATH|-]
               [--refresh account,positions,regime]

ibkr order-preview --input PATH|-
                   [--select candidate-id[,candidate-id]]
                   [--json]
```

Future order-capable CLI:

```text
ibkr order-submit --input PATH
                  --confirm "SUBMIT candidate-id[,candidate-id]"
```

## Naming: Canary Versus Risk

The current `canary` name is defensible but not perfect.

Pros of keeping `canary`:

- It accurately describes an early-warning alarm rather than an execution engine.
- It avoids over-promising comprehensive portfolio risk management.
- It is already implemented in CLI, MCP docs, examples, tests, generated references, and likely screenshots.
- It distinguishes the compact scheduled monitor from the broader future `risk-plan` planner.
- It reduces churn while order preview and risk-plan are still being designed.

Cons of keeping `canary`:

- It is metaphorical; users and LLM tool routers may not immediately know it means portfolio risk alarm.
- It does not group naturally with `risk-plan`.
- It can sound less institutional than `risk`.
- Users may ask for "risk" and not discover `canary` unless docs and tool descriptions bridge the wording.

Pros of renaming to `risk`:

- Clearer top-level concept for users: `ibkr risk`, `ibkr risk plan`, `ibkr risk check`.
- Better match for agent prompts like "check portfolio risk".
- More extensible if the product becomes a risk workflow suite.

Cons of renaming to `risk`:

- `risk` is too broad for the current canary behavior. It may imply VaR, stress scenarios, factor exposure, tax, margin simulation, and execution advice.
- It risks conflating three different layers: risk alarm, risk planner, and order preview.
- It would require CLI aliasing or breaking changes.
- It would churn generated MCP docs, public docs, examples, screenshots, plugin descriptions, tests, preview fixtures, and any external user automation.
- A full rename would need deprecation messaging, compatibility aliases, docs redirects, and screenshot regeneration.

Recommendation:

- Keep `canary` as the stable command and MCP tool for the compact alarm.
- Introduce `risk-plan` as the planner.
- Optionally add a future alias such as `ibkr risk check` that renders the same payload as `ibkr canary`, while keeping `ibkr canary` indefinitely.
- Avoid a full docs/screenshots rename until there is a broader `risk` namespace with at least `risk check` and `risk plan`.

This gives clearer future UX without breaking the existing early-warning surface.

## Implementation Sequence

1. Add `risk_plan.v1` and `order_preview.v1` RPC/CLI types.
2. Add deterministic policy defaults and freshness thresholds.
3. Implement `ibkr risk-plan --json` using fresh internal calls to account, positions, regime, and canary.
4. Generate existing-position reduction candidates only.
5. Add chain/quote gates for option candidates.
6. Add optional debit hedge generation behind `--allow-hedges`.
7. Wire `ibkr order-preview --input` to accept only `risk_plan.v1`.
8. Add MCP `ibkr_risk_plan` only after CLI behavior is stable.
9. Add docs and examples for agent operation.
10. Defer any order-submission work until preview artifacts, hash gates, expiry gates, account gates, and user-confirmation gates are implemented.

## Non-goals

- No automatic trading.
- No direct `canary | order-preview` path.
- No MCP order submission.
- No optimizer that tries to find a mathematically perfect portfolio.
- No tax-lot optimization in the first version.
- No naked short-option strategy generation.

## Open Questions

- Should default risk targets be configured by flags, config file, or both?
- Should `risk-plan` persist the last generated artifact by default, or rely on explicit shell redirection?
- Should `risk-plan --select` exist, or should selection live only in `order-preview --select`?
- Should hedge candidates initially support SPY only, or SPY and QQQ?
- Should SPX hedge candidates wait until SPX chain readiness is consistently robust?
