# Risk-plan orchestration

**Status:** Draft for later pickup.
**Created:** 2026-05-29 20:43 CEST
**Last update:** 2026-05-29 20:46 CEST
**Owner:** osauer
**Related:** [internal/cli/canary.go](../../internal/cli/canary.go), [docs/specs/risk-regime-dashboard.md](../specs/risk-regime-dashboard.md), [docs/reference/protocol.md](../reference/protocol.md)

## Decision

`canary` does not need to become more specific about concrete repositions for `risk-plan` to generate a plan. It needs to remain a risk-state classifier: margin state, exposure state, concentration state, regime state, confidence, source timestamps, and warnings.

`risk-plan` should be the first component that proposes concrete position changes. It should call the existing evidence tools internally, build candidate reductions or hedges, and hand only reviewed order drafts to the order-preview layer. Piping remains supported for audit and replay, but raw `canary` or `regime` JSON must not be accepted as order input.

For a production account, the LLM is an operator and explainer, not the authority for portfolio math or order construction. The agent may interview the user, invoke tools, rank and explain tool-produced candidates, and ask for approval. It must not hand-write orders, override stale-data gates, or convert prose recommendations into tickets.

## Why canary should not propose trades

`regime` is market evidence only. It knows the tape, not the portfolio.

`canary` knows the portfolio risk state, but not enough to safely name a trade. A concrete reposition requires exact open lots, option-leg availability, live bid/ask, spread quality, chain liquidity, margin impact, account currency, and closing/opening semantics. If `canary` emits "sell 50 BB calls", it silently becomes a planner, chain screener, liquidity gate, and execution preflight. That makes the canary harder to test and easier to misuse.

The canary's contract should stay: "given current account, positions, and regime evidence, what risk posture are we in?" It may say `WATCH`, `DELEVER`, or `LIQUIDATE`, and why. It should not say which contract to trade.

## Minimal actors

- User: chooses policy, hedge permissions, and final selection.
- Agent: interviews the user, explains tradeoffs, invokes CLI/MCP tools, and never bypasses preview or approval gates.
- Evidence tools: `account`, `positions`, `regime`, `canary`, `quote`, and `chain`; each remains narrow and independently useful.
- Risk planner: `risk-plan`; the only component that composes evidence into candidate repositions.
- Broker validator: `order-preview`; computes broker-side what-if, margin, cost, and rejection details without submitting.
- Execution layer: out of scope for this design; if added later, it must accept only approved preview artifacts.

## Production agent contract

The agent can reliably operate the workflow only if its powers are narrow:

- It may choose which command to invoke next.
- It may ask the user for policy inputs: reduction-only vs hedges, premium cap, target exposure, and whether covered option sells are allowed.
- It may summarize evidence and candidate tradeoffs.
- It may select candidates for preview only from IDs emitted by `risk-plan`.
- It must not fabricate order drafts, quantities, strikes, expiries, hashes, or preview results.
- It must stop on missing, stale, degraded, or low-confidence inputs unless the user explicitly chooses a replay or diagnostic mode.
- It must surface every hard gate failure instead of "working around" it with another instrument.

The deterministic tools own all stateful and numeric work:

- `canary` classifies risk state.
- `risk-plan` computes target breaches, candidate quantities, and candidate risk effects.
- `chain` and `quote` validate marketability.
- `order-preview` validates broker-side impact.
- A future submitter, if added, validates preview hash, expiry, user confirmation, and account guardrails.

In other words: the agent is allowed to operate the checklist; it is not allowed to become the checklist.

## Reduced orchestration flow

Default CLI flow:

```sh
ibkr risk-plan --json \
  | ibkr order-preview --input - --json
```

Interactive CLI flow:

```sh
ibkr risk-plan
ibkr order-preview --input risk-plan.json
```

Agent flow:

1. Agent asks the user for policy choices only when defaults are insufficient: reduction-only vs hedges, maximum hedge premium, maximum single-name exposure, and whether option sells are allowed.
2. Agent invokes `ibkr risk-plan --json` or the equivalent read-only MCP planner.
3. `risk-plan` refreshes account, positions, regime, and canary itself.
4. `risk-plan` builds candidate reductions from existing positions first.
5. If hedges are explicitly allowed, `risk-plan` queries chains/quotes and adds debit-only hedge candidates that pass liquidity gates.
6. Agent presents the ranked plan and asks the user which candidates to preview.
7. Agent invokes `order-preview` for the selected candidates.
8. Agent presents preview impact and stops. Submission is a separate future approval path.

Scheduled monitoring flow:

1. Scheduler or agent invokes `ibkr canary --json`.
2. If the decision is `HOLD`, log the artifact and stop.
3. If the decision is `WATCH`, generate a staged `risk-plan` but do not preview unless the user or policy asks for it.
4. If the decision is `DELEVER` or `LIQUIDATE`, generate a `risk-plan`, present the highest-priority candidates, and ask for preview approval.
5. If margin danger is immediate, `risk-plan` prioritizes closing existing exposure and raising liquidity before any hedge candidate.
6. If any source is stale, degraded, computing, or below required confidence, the agent reports the blocker and asks whether to wait, refresh, or run a diagnostic. It does not invent a substitute signal.

External piping from evidence tools remains a diagnostic path, not the canonical path:

```sh
ibkr canary --json \
  | ibkr risk-plan --from-canary - --refresh account,positions,regime --json
```

The refresh is load-bearing. It prevents a stale canary file from being combined with fresh chains and current account margin.

## Risk-plan responsibilities

`risk-plan` owns policy and composition:

- Fetch a coherent snapshot from account, positions, regime, and canary.
- Enforce source freshness and reject incompatible timestamps unless the user passes an explicit replay flag.
- Prefer reducing existing exposure before opening new hedges.
- Convert canary findings into target breaches, for example gross delta, net delta, and largest single-name delta.
- Generate candidate intents with estimated risk effects.
- Query quotes/chains only for candidates that need executable market context.
- Reject candidates with stale quotes, missing bid/ask, untradable chains, excessive spreads, or missing greeks needed for the estimate.
- Mark every candidate as preview-required.

It does not submit orders.

It should also emit enough metadata for an agent to operate safely:

- `source_hashes` for account, positions, regime, and canary inputs.
- `policy` values used for thresholds, hedge permissions, spread limits, and premium caps.
- `blocked_reasons` when no candidate can be produced.
- `candidate_ids` that are stable within one artifact and unique across candidates.
- `expires_at` so an old plan cannot be previewed as if it were fresh.
- `requires_user_approval` on every candidate.

## Account and positions roles

`account` sets hard constraints:

- net liquidation
- base currency
- current cushion
- look-ahead cushion
- available funds
- buying power
- account type and margin context

If margin is dangerous, margin relief outranks concentration cleanup and hedge elegance.

`positions` supplies the actual reduction surface:

- exact stock and option quantities
- closing side (`SELL` long stock, `BUY_TO_CLOSE` short option, `SELL_TO_CLOSE` long option)
- per-underlying market value
- per-underlying dollar delta
- option greeks and greeks coverage
- option bid/ask, marks, and warnings
- grouped exposure by underlying

Candidate reductions should be derived from current positions, not from canary prose.

## Canary contract changes

No order-level fields belong in `canary`.

The current canary output is sufficient for a first `risk-plan` implementation because it already carries the core planner inputs: source timestamps, margin cushion, look-ahead cushion, gross exposure, net delta, gross delta, largest market concentration, largest delta concentration, market confidence, and warnings.

Small additions can make `risk-plan` simpler and more deterministic:

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

These are still risk facts and planner hints, not trades. `risk-plan` remains free to recompute the same facts from account and positions and should treat canary signals as an input consistency check.

If `risk_signals` disagree with fresh account or positions data, `risk-plan` should prefer the fresh account and positions data and report the mismatch as a warning. Canary is a classifier, not a source of executable truth.

## Reliability invariants

These invariants are required before this workflow is safe for a large account:

- Typed artifacts only: `order-preview` refuses raw `canary`, raw `regime`, and arbitrary agent-written JSON.
- Freshness gates: live planning requires all source timestamps inside configured tolerances.
- Preview gate: every candidate must pass broker preview before any future submission path can see it.
- Hash gate: preview and future submission artifacts carry the source plan hash.
- Expiry gate: stale plans and stale previews are rejected.
- Scope gate: closing orders must match an existing position; opening hedges must be explicitly marked as opening.
- Permission gate: hedges, covered option sells, and any future submit action require explicit user policy.
- Kill-switch: any gateway disconnect, account mismatch, delayed data, or margin-preview rejection blocks execution.
- Audit trail: every artifact includes generated time, source hashes, policy, candidates, rejections, and warnings.

## Minimal JSON artifacts

Only two artifacts are required for the first implementation.

`risk_plan.v1`:

```json
{
  "kind": "risk_plan.v1",
  "generated_at": "2026-05-29T20:43:00+02:00",
  "decision": "WATCH",
  "confidence": "medium-low",
  "source_as_of": {
    "account": "2026-05-29T17:45:10Z",
    "positions": "2026-05-29T19:45:10+02:00",
    "regime": "2026-05-29T19:45:18+02:00"
  },
  "targets": {
    "gross_delta_pct_nlv": 250,
    "net_delta_pct_nlv": 150,
    "single_name_delta_pct_nlv": 75
  },
  "candidates": [
    {
      "id": "reduce-now-1",
      "intent": "reduce_single_name_delta",
      "priority": 1,
      "scope": "existing_position",
      "rationale": "NOW is the largest dollar-delta concentration.",
      "estimated_effect": {
        "largest_delta_pct_nlv_after": 75
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
      "requires_preview": true
    }
  ]
}
```

`order_preview.v1`:

```json
{
  "kind": "order_preview.v1",
  "source_plan_hash": "sha256:...",
  "accepted_candidates": ["reduce-now-1"],
  "rejected_candidates": [],
  "margin_impact": {
    "current_excess_liquidity": 0,
    "post_trade_excess_liquidity": 0
  },
  "submit_allowed": false
}
```

## Defaults

First implementation defaults should be conservative:

- reductions only
- no naked short options
- no option-opening hedge unless `--allow-hedges` is set
- debit hedges only when enabled
- maximum hedge premium defaults to `0.75%` of NLV
- no candidate from stale option quotes or untradable chains
- no submission path

## Non-goals

- No automatic trading.
- No direct `canary | order-preview` path.
- No MCP order submission.
- No optimizer that tries to find a mathematically perfect portfolio.
- No tax-lot optimization in the first version.
- No naked short-option strategy generation.

## Open questions

- Should `risk-plan` default targets be user-configurable in config, flags, or both?
- Should candidate selection happen inside `risk-plan --select` or only in `order-preview --select`?
- Should hedge candidates use SPY only initially, or allow SPY/QQQ/SPX once SPX chain readiness is robust?
- Should `risk-plan` persist the last generated artifact for replay, or rely on explicit shell redirection?
