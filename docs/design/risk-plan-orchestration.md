# Canary-driven risk response

**Status:** Draft implementation brief.
**Created:** 2026-05-29 20:43 CEST
**Last update:** 2026-05-29 22:27 CEST
**Owner:** osauer
**Related:** [internal/cli/canary.go](../../internal/cli/canary.go), [docs/specs/risk-regime-dashboard.md](../specs/risk-regime-dashboard.md), [docs/reference/protocol.md](../reference/protocol.md)

## Thesis

`canary` is the live money-saving sentinel. It watches for conditions that demand attention: violent downside markets, euphoric upside markets, margin danger, portfolio overexposure, data failure, and unusual opportunity.

`risk-plan` is the response planner. It does not decide that the world is dangerous by itself, and it does not execute. It receives an alert context, refreshes live account and portfolio state, rebuilds the risk ledger, and proposes candidate actions.

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
      +-------> plan -> preview -> approval -> execution -> post-check
```

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

The exact thresholds and buckets are implementation details. The important design point is that the signal vocabulary and thresholds are shared, while order construction remains owned by risk-plan and preview.

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

It must not emit order drafts, trade quantities, strikes, expiries, or previewable intents. A canary alert answers: "Do we need to pay attention now, and why?"

The legacy `GO` / `WATCH` / `DE-LEVER` / `LIQUIDATE` ladder is no longer part of the machine contract. It was readable, but too trade-shaped for a sentinel. Human UX should instead show:

```text
Risk state  Defensive / Watch
Next step   Stage risk-plan
Guidance    Freeze new risk and stage a risk plan; wait for confirmation before major action.
Drivers     margin_cushion_low, net_delta_high, gamma_red
```

## Risk-Plan Contract

Risk-plan answers: "Given the alert and current portfolio, what candidate response should be previewed?"

Every live run should:

1. Resolve response mode from the user request or canary alert: defend, rebalance, deploy, stage, or confirm-data.
2. Refresh account, positions, regime, and canary.
3. Rebuild the portfolio risk ledger.
4. Re-evaluate shared policy criteria from fresh data.
5. Generate candidate actions that address the largest active risk or opportunity.
6. Emit a typed plan artifact for preview.

Response modes:

- `defend`: reduce, hedge, or liquidate risk under stress or margin danger.
- `rebalance`: bring the portfolio back inside accepted policy bounds.
- `deploy`: use available risk budget when constructive conditions and portfolio constraints allow it.
- `stage`: prepare candidates without implying immediate execution.
- `confirm-data`: refresh or verify degraded inputs before planning major action.

The MVP can implement defensive reductions first, but the design should not bake in "risk-plan only sells." A live sentinel should also support disciplined opportunity response when the policy says risk budget can be used.

## Process Flow

### Monitor-triggered response

1. A scheduler runs canary, for example every 10 minutes.
2. Canary emits alert direction, severity, and fired shared signals.
3. Monitor policy decides whether to call risk-plan.
4. Risk-plan refreshes live state and rechecks the same policy criteria.
5. If the alert resolved, the plan says no action is currently required.
6. If action pressure remains, risk-plan emits candidate responses.
7. Order preview validates selected candidate IDs.
8. Execution, when implemented, requires explicit approval and fresh validation.
9. Canary runs again after action to confirm the portfolio state.

### User-requested response

The user can call risk-plan directly without waiting for canary:

- "Bring risk back inside limits" maps to `rebalance`.
- "What should I cut if this gets worse?" maps to `stage`.
- "Can I put cash to work under this regime?" maps to `deploy`.
- "Reduce harm now" maps to `defend`.

Risk-plan still uses the same shared policy and refreshed ledger.

## Candidate Priority

Default priority should be simple:

1. Prevent margin or liquidity harm.
2. Address the largest active breach or opportunity.
3. Prefer simple changes to complex ones.
4. Prefer closing or resizing known exposure before opening new exposure.
5. Require preview for every candidate.

For defensive runs, the largest active issue may be margin, single-title concentration, short convexity, or market beta. For deployment runs, it may be unused risk budget, missing target exposure, or a constructive market regime. The policy decides which opportunities are valid; risk-plan should not chase enthusiasm without constraints.

## Artifact Boundary

Risk-plan emits a typed artifact. Order preview consumes that artifact and selected candidate IDs. Raw canary output and agent-authored orders are not preview inputs.

The artifact should carry:

- kind, account ID, policy profile, and response mode
- source timestamps and hashes
- alert context and fired shared signals
- refreshed risk-ledger summary
- candidate IDs, rationale, and draft intent
- estimated before/after effects
- warnings, blocked reasons, and expiration

## Implementation Direction

Build the shared policy and signal vocabulary first, then wire canary and risk-plan to it.

Suggested order:

1. Define shared signal IDs, severity, direction, and policy profiles.
2. Add machine-readable fired signals, planner mode hint, and planner readiness to canary.
3. Build a reusable risk ledger from account, positions, and regime.
4. Implement defensive risk-plan candidates for existing positions.
5. Add typed plan artifacts and order-preview consumption.
6. Extend exposure axes for asset class and market buckets before currency buckets.
7. Add deploy/stage modes once defensive planning is trustworthy.

## Non-Goals

- No scheduler inside risk-plan.
- No automatic trading.
- No order-level fields in canary.
- No direct canary-to-preview path.
- No MCP order submission.
- No optimizer, tax-lot engine, or naked short-option strategy generation in the first version.

## Open Questions

- What exact thresholds should the first asset-class and market buckets use?
- What exact clean-risk-budget criteria make a constructive signal deployable rather than merely euphoric?
- Should monitor trigger context be flags, a small artifact, or both?
