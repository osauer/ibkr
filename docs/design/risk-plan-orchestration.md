# Canary-driven risk response

**Status:** Draft implementation brief.
**Created:** 2026-05-29 20:43 CEST
**Last update:** 2026-05-30 08:18 CEST
**Owner:** osauer
**Related:** [internal/cli/canary.go](../../internal/cli/canary.go), [docs/specs/risk-regime-dashboard.md](../specs/risk-regime-dashboard.md), [docs/reference/protocol.md](../reference/protocol.md)

## Thesis

`canary` is the live money-saving sentinel. It watches for conditions that demand attention: violent downside markets, euphoric upside markets, margin danger, portfolio overexposure, data failure, and unusual opportunity.

`risk-plan` is the response planner. It does not decide that the world is dangerous by itself, it does not authorize execution, and it does not execute. It receives an alert context, refreshes live account and portfolio state, rebuilds the risk ledger, and proposes candidate actions for expert review.

No single input should dominate blindly. In monitor-triggered runs, the canary alert is the reason `risk-plan` wakes up. The source of truth for action is the refreshed portfolio ledger evaluated against the same policy that canary used to alert.

```text
live account + positions + regime
              |
              v
        shared risk policy
              |
      +-------+--------+
      |                |
      v                v
 canary alert      risk-plan
 sentinel          planner
      |                |
      +-------> plan -> expert review -> selected candidate
                                      |
                                      v
                         trading/order-entry preview
                                      |
                                      v
                         approval -> execution -> post-check
```

The handoff is deliberately split. Risk-plan can explain what might be done. An expert reviewer decides whether any candidate should proceed. The trading/order-entry subsystem independently validates any selected candidate before it can become an order.

## Shared Policy

Canary and risk-plan need shared criteria. Canary should not only say "something is wrong"; it should say which policy criteria fired, in machine-readable form. Risk-plan should not parse canary prose; it should re-evaluate those same criteria from fresh data.

The shared policy should cover both defense and opportunity:

| Area | Example signals | Planner implication |
| --- | --- | --- |
| Margin danger | `margin_cushion_low`, `lookahead_cushion_low` | Cut or liquidate risk first. |
| Violent downside | `market_selloff_violent`, `vol_spike_confirmed`, `regime_stress_confirmed` | Defend capital, reduce fragile exposure, consider hedges. |
| Violent upside or enthusiasm | `market_rally_violent`, `vol_crush_confirmed` | Check underinvestment while guarding against chase-risk; possibly deploy or rebalance. |
| Portfolio P&L shock | `portfolio_pnl_shock` | Protect liquidity or gains; a gain is not deployable by itself. |
| Single-title concentration | `single_name_exposure_high`, `single_name_delta_high` | Reduce the largest title risk before smaller issues. |
| Portfolio exposure | `gross_exposure_high`, `net_delta_high`, `gross_delta_high` | Defend, rebalance, or stage reductions before stress worsens. |
| Option quality and convexity | `option_greeks_degraded`, `short_convexity_high`, `gamma_red` | Block or prioritize option-specific actions. |
| Data quality | `risk_data_degraded`, `market_data_stale` | Block action or force human review. |

The exact thresholds and buckets are implementation details. The important design point is that the signal vocabulary and thresholds are shared, while candidate construction remains owned by risk-plan and order validation remains owned by trading/order-entry.

Current signal payloads carry:

- `id`: stable signal name from the shared vocabulary.
- `direction`: `defensive`, `constructive`, `mixed`, or `data_quality`.
- `severity`: `observe`, `watch`, `act`, or `urgent`.
- `subject`: optional symbol, cluster, or bucket.
- `metric`, `observed`, `threshold`, `target`, and `unit`.
- `evidence`: compact human-readable evidence.
- `confidence` and `confidence_impact`.
- `blocked_by`: missing or degraded inputs that prevent stronger action.

Targets must mean post-action targets. If a signal is only a watch-level early warning and the nearest action floor is below the current watch threshold, omit `target` rather than asking the planner to solve toward a weaker state.

## Canary Contract

Canary is a high-frequency alerting tool, not a planner.

It should emit:

- alert direction: defensive, constructive, mixed, or data-quality
- severity: observe, watch, act, or urgent
- planner mode hint: none, stage, defend, rebalance, deploy, or confirm-data
- planner readiness: none, watch, prestage, ready, or blocked
- compact human summary
- fired shared signal IDs
- observed values and thresholds for those signals
- split confidence: data confidence and signal confidence
- source timestamps and policy profile/version
- compact account, portfolio, and regime context
- explicit ambiguity and stale-data warnings

It must not emit order drafts, trade quantities, strikes, expiries, selected candidate IDs, previewable intents, preview tokens, nonces, or order authorizations. A canary alert answers: "Do we need to pay attention now, and why?"

The legacy `GO` / `WATCH` / `DE-LEVER` / `LIQUIDATE` ladder is no longer part of the machine contract. It was readable, but too trade-shaped for a sentinel. Human UX should instead show:

```text
Risk state  Defensive / Watch
Next step   Stage risk-plan
Guidance    Freeze new risk and stage a risk plan; wait for confirmation before major action.
Drivers     margin_cushion_low, net_delta_high, gamma_red
```

## Risk-Plan Contract

Risk-plan answers: "Given the alert and current portfolio, which candidate responses should an expert review?"

Every live run should:

1. Resolve response mode from the user request or canary alert: defend, rebalance, deploy, stage, or confirm-data.
2. Refresh account, positions, regime, and canary.
3. Rebuild the portfolio risk ledger.
4. Re-evaluate shared policy criteria from fresh data.
5. Load pending previews, locally tracked open orders, and recent candidate history.
6. Generate candidate actions that address the largest active risk or opportunity.
7. Mark unsupported, stale, duplicate, or policy-blocked candidates as blocked.
8. Emit a typed plan artifact whose mandatory next step is expert review.

Response modes:

- `defend`: reduce, hedge, or liquidate risk under stress or margin danger.
- `rebalance`: bring the portfolio back inside accepted policy bounds.
- `deploy`: use available risk budget when constructive conditions and portfolio constraints allow it.
- `stage`: prepare candidates without implying immediate execution.
- `confirm-data`: refresh or verify degraded inputs before planning major action.

The MVP can implement defensive reductions first, but the design should not bake in "risk-plan only sells." A live sentinel should also support disciplined opportunity response when the policy says risk budget can be used.

Risk-plan must not choose or authorize the execution channel. Its artifact is advisory until an expert reviewer accepts, rejects, edits, or selects candidate IDs for preview.

## Process Flow

### Monitor-triggered response

1. A scheduler runs canary, for example every 10 minutes.
2. Canary emits alert direction, severity, and fired shared signals.
3. Monitor policy decides whether to call risk-plan.
4. Risk-plan refreshes live state and rechecks the same policy criteria.
5. Risk-plan loads pending previews, tracked open orders, and recent plan candidates for duplicate suppression.
6. If the alert resolved, the plan says no action is currently required.
7. If action pressure remains, risk-plan emits candidate responses for expert review.
8. The expert reviewer rejects, edits, or selects candidate IDs.
9. Order preview validates selected candidate IDs against the current trading gate and order-entry capability.
10. Execution, when implemented, is owned by trading/order-entry and requires explicit approval plus fresh validation.
11. Canary runs again after action to confirm the portfolio state.

### User-requested response

The user can call risk-plan directly without waiting for canary:

- "Bring risk back inside limits" maps to `rebalance`.
- "What should I cut if this gets worse?" maps to `stage`.
- "Can I put cash to work under this regime?" maps to `deploy`.
- "Reduce harm now" maps to `defend`.

Risk-plan still uses the same shared policy, refreshed ledger, duplicate suppression, and expert-review boundary.

## Expert Review Boundary

Expert review is the mandatory next step after planning.

The reviewer owns judgment, not transport. They decide whether the plan is coherent, whether the policy and source data are trustworthy, whether any candidate should be edited or rejected, and which candidate IDs may proceed to preview.

The reviewer does not bypass trading/order-entry gates. A selected candidate is only an instruction to ask for preview. It is not an order, approval, nonce, or authorization to submit.

The plan artifact should make this explicit:

- `execution_authority`: always `none`
- `next_required_step`: `expert_review`
- no `execution_owner`
- no submission channel
- no preview token
- no human nonce
- no order authorization

## Trading and Preview Boundary

Trading/order-entry independently validates selected candidates. It must not trust risk-plan as an authority.

Preview should verify at least:

- local trading gate and blockers
- account match
- policy profile and policy version match
- source timestamps and hashes are still acceptable
- selected candidate ID exists in the artifact and is not blocked
- current gateway endpoint, account, and client ID match the trading configuration
- current order-entry capability supports the candidate
- max notional, max contracts, short/flip rules, and option-opening rules
- broker `WhatIf` result before any submit-eligible token
- token binding, expiry, and confirmation gates before execution

Current order-preview capability is intentionally narrow: equities and ETFs only, `LMT` only, `DAY` only, default `patient-limit`, and `outside_rth=false`. Until order-entry grows and tests a broader capability, risk-plan may still describe unsupported risk responses, but it must mark them `blocked` or `informational`; they must not be previewable.

Broker `WhatIf`, preview tokens, single-use confirmation, and actual order execution belong to trading/order-entry. Risk-plan cannot mint preview tokens, mark a token executable, or authorize broker writes.

## Monitor Idempotency

Monitor-triggered plans can repeat frequently. Repeating the same candidate every 10 minutes is a trading risk, even if every individual candidate looks valid.

Before emitting candidates, risk-plan should account for:

- pending previews that have not expired
- locally tracked open orders
- recent candidate IDs and their source hashes
- materially changed account, position, regime, or policy inputs
- stale data and partially failed source refreshes

Candidate IDs should be stable for the same plan source and intent. If the same alert snapshot produces the same response, risk-plan should report the existing pending candidate instead of creating a fresh previewable candidate. If the source snapshot changed materially, the artifact should say why a new candidate supersedes the old one.

## MCP Boundary

MCP risk-plan tools may produce plan artifacts. They may ask the trading
subsystem for candidate validation or preview diagnostics only after expert
selection, and only through a non-submitting surface that does not mint a
submit-capable token. Until such a diagnostic-only preview mode exists,
risk-plan MCP tools must not call order preview directly.

They must not:

- submit, modify, or cancel orders
- mint preview tokens
- mint or request human nonces
- present a risk-plan candidate as approved
- call MCP order-write tools on behalf of the user
- bypass expert review

If future MCP order-write tools exist, they remain under the trading/order-entry policy: explicit trading config, matching preview token, out-of-band human nonce, fresh validation, and audited journal state. Risk-plan MCP tools do not provide that authority.

## Candidate Priority

Default priority should be simple:

1. Prevent margin or liquidity harm.
2. Address the largest active breach or opportunity.
3. Prefer simple changes to complex ones.
4. Prefer closing or resizing known exposure before opening new exposure.
5. Prefer candidates the current order-entry capability can preview.
6. Require expert review before any preview.
7. Require preview for every selected candidate before any execution path.

For defensive runs, the largest active issue may be margin, single-title concentration, short convexity, or market beta. For deployment runs, it may be unused risk budget, missing target exposure, or a constructive market regime. The policy decides which opportunities are valid; risk-plan should not chase enthusiasm without constraints.

## Artifact Boundary

Risk-plan emits a typed artifact. Expert review consumes that artifact first. Order preview may later consume the same artifact plus selected candidate IDs. Raw canary output and agent-authored orders are not preview inputs.

The artifact should carry:

- kind, plan ID, account ID, policy profile, policy version, and response mode
- source timestamps and hashes for account, positions, quotes, regime, canary, and policy
- alert context and fired shared signals
- refreshed risk-ledger summary
- pending preview, open order, and recent candidate references used for duplicate suppression
- candidate IDs, rationale, draft intent, and capability status
- estimated before/after effects
- warnings, blocked reasons, and expiration
- `execution_authority: none`
- `next_required_step: expert_review`

The artifact should not carry:

- execution owner
- submission channel
- preview tokens
- human nonces
- broker order IDs
- order authorizations

## Implementation Direction

Build the shared policy and signal vocabulary first, then wire canary and risk-plan to it.

Suggested order:

1. Define shared signal IDs, severity, direction, and policy profiles.
2. Add machine-readable fired signals, planner mode hint, and planner readiness to canary.
3. Build a reusable risk ledger from account, positions, and regime.
4. Add monitor duplicate-suppression inputs: pending previews, open orders, and recent candidates.
5. Implement defensive risk-plan candidates for existing positions.
6. Add typed plan artifacts with expert-review-required semantics.
7. Add order-preview consumption only for candidates supported by current trading capability.
8. Extend exposure axes for asset class and market buckets before currency buckets.
9. Add deploy/stage modes once defensive planning is trustworthy.

## Non-Goals

- No scheduler inside risk-plan.
- No automatic trading.
- No execution-channel decision inside risk-plan.
- No order-level fields in canary.
- No direct canary-to-preview path.
- No preview tokens, nonces, or order authorization from risk-plan.
- No MCP order submission.
- No optimizer, tax-lot engine, or naked short-option strategy generation in the first version.

## Open Questions

- What exact thresholds should the first asset-class and market buckets use?
- What exact clean-risk-budget criteria make a constructive signal deployable rather than merely euphoric?
- Should monitor trigger context be flags, a small artifact, or both?
- How long should recent candidate history be retained for duplicate suppression?
- What evidence should let an expert reviewer mark a stale plan safe to rerun instead of creating a new candidate?
