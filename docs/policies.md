# Policies

`ibkr` uses several policy-shaped controls, but they do not all have the same
owner or authority. The desk should start with one rule:

> A policy declares approved choices. Measurements report what is true now,
> enforcement decides what may happen, and reporting preserves what happened.
> A setting, warning, or dashboard label must not silently become policy.

## Policy System Overview

[![ibkr policy authority and execution lifecycle](diagrams/policy-authority.svg)](diagrams/policy-authority.svg)

[PNG fallback](diagrams/policy-authority.png) ·
[SVG source generator](diagrams/render-architecture.mjs) ·
[Tabler Icons license](diagrams/ICON-LICENSE.txt)

The daemon is the policy executor. Operator-authored TOML, embedded engine
defaults, daemon-owned runtime preferences, and code-owned invariants enter
through different authority paths. The daemon validates and fingerprints the
applicable policy, combines it with typed evidence, calls pure semantics in
`internal/risk`, persists the resulting state or event where required, and
publishes one typed result through RPC. CLI, MCP, app, and Canary render that
result; they do not reinterpret policy.

## Four Policy Classes

| Class | Authority | Default if absent | How it changes | What it can do |
|---|---|---|---|---|
| Personal risk constitution | `~/.config/ibkr/policies/risk-policy.toml` | None. Missing material values remain `unapproved`. | Edit the TOML and raise `policy_version`. | Defines capital, drawdown, reconciliation, exception, cadence, and sibling-policy approval choices. Current schema is advisory/shadow. |
| Advisory engine policy | Protection and opportunity policy TOML, or their embedded defaults | Conservative embedded policy. | Generate the current default, save a file, edit it, and raise `policy_version`. | Shapes protection proposals and option-exercise opportunity detection. It never submits an order by itself. |
| Runtime preference | Versioned platform-settings document in `daemon.db` | Code/config default, reported with provenance. | `ibkr settings set`, the Settings UI, or the typed API, subject to access rules. | Enables product features, records earnings overrides, and applies the human-owned trading freeze or experimental limits. It is not a second policy-file system. |
| Code-owned invariant or model | Versioned code and typed contracts | Always present in that binary. | Reviewed code/release change; guardrail changes require an explicit human decision. | Owns broker-write gates, validation, calculations, the rulebook/regime/Canary model, and constraints policy files are forbidden to weaken. |

Policy and semantic fingerprints also identify the compiled rulebook, Regime,
Canary, alert producers, and other models. Those identities make decisions
reproducible and allow the constitution to pin approved sibling versions. They
are not extra operator-editable TOML files unless this page or the
[configuration reference](reference/config.md) names one.

## Risk Constitution

The personal risk constitution is the desk's machine-readable approval record.
Its fixed path is `~/.config/ibkr/policies/risk-policy.toml`; it has no embedded
default and no path override. The schema lives in `internal/risk`, while the
operator owns the material values.

The main sections are:

| Section | Decision owned by the operator |
|---|---|
| `[capital]` | Account base currency, protected equity floor, declared risk capital, and evidence-age limits. Effective risk capital is the lesser of the declared amount and equity above the protected floor. |
| `[drawdown]` | Warning and block-consumption thresholds and the declared enforcement class. Schema v1 rejects `hard`; the implemented choices are `shadow` and `advisory`. |
| `[override]` | Maximum lifetime of one human-only, reasoned, expiring exception. The code owns origin gating and audit. |
| `[recon]` | Statement-flow match tolerances, date window, report age, and the same-day statement/runtime equity divergence allowed for clean-report auto-extension. |
| `[cadence]` | Advisory morning, end-of-day, weekly, and version-dependent nudge/monthly operating cadence. Missing material cadence remains explicit rather than receiving a model-chosen value. |
| `[inventory]` | The rulebook, protection, and Canary identities against which this constitution was approved. These are pins, not copies of sibling thresholds. |

Use [the checked-in template](../examples/risk-policy.toml) to see every field.
It intentionally leaves material numerical decisions commented out. `nil` or
an omitted key means unapproved, never zero and never an invitation for an
agent to choose a number.

The risk-policy manager checks the file on a short interval and applies this
version discipline:

1. A valid first policy or a higher `policy_version` becomes active.
2. Identical content at the active version remains active.
3. Changed content at the same or a lower version reports `drift`; the last
   good policy remains the executable context.
4. Invalid TOML, unknown keys, or failed validation report `error`; the last
   good policy remains visible and the error is not normalized away.
5. Removing a previously active file is not retirement. Status becomes
   `absent`, the last loaded policy remains active, and the operator must
   restore the file or publish a deliberate revision.

Inspect the applied constitution with:

```sh
ibkr policy show
ibkr policy show --explain
ibkr policy show --json
```

`--explain` is the operator reference for units, effective values, evidence
health, drawdown state, reconciliation, active overrides, cadence, sibling
pins, and the policy fingerprint. The write verbs under `ibkr policy` record
human governance actions; they are not configuration shortcuts and are
rejected for agent origins.

## Protection and Opportunity Policies

Protection and opportunity policies are bounded advisory-engine policies. They
have embedded defaults because the engines need a complete conservative
parameter set even when the operator has not customized them.

Print, review, and customize them as follows:

```sh
mkdir -p ~/.config/ibkr/policies
ibkr policy default protection > ~/.config/ibkr/policies/protection-policy.toml
ibkr policy default opportunity > ~/.config/ibkr/policies/opportunity-policy.toml
```

The protection path defaults to
`~/.config/ibkr/policies/protection-policy.toml` and can be changed with
`[auto_trade].policy_file`. The opportunity path defaults to
`~/.config/ibkr/policies/opportunity-policy.toml` and can be changed with
`[opportunities].policy_file`. Their complete generated key tables live in the
[configuration reference](reference/config.md).

For both files:

- `kind`, `schema_version`, `policy_id`, and `policy_version` identify the
  contract; `profile` is a descriptive label.
- Unknown keys and invalid combinations fail validation.
- Every material edit needs a higher `policy_version`; same-version changes
  report drift rather than silently changing behavior.
- The current fingerprint travels with snapshots and previews so a proposal or
  opportunity can be tied to the policy that produced it.
- `authority.auto_submit` must remain false. A proposal, opportunity, or
  preview is evidence, not broker-write authority.

Read the loaded identities and blockers on the proposal and opportunity status
surfaces after editing. A restart is not normally required when hot reload is
enabled, but an operator may use the normal restart path when commissioning a
new version.

## Runtime Settings Are Not Policy Files

Platform settings are live preferences stored as one compare-and-swap document
in `daemon.db`. They can reveal a config or build default, but they do not copy
TOML into SQLite and they cannot create a new enforcement rule.

```sh
ibkr settings show
ibkr settings show --json
ibkr settings set <key>=<value>
ibkr settings set <key>=null
```

`null` removes the runtime override and exposes the underlying default or TOML
value. Every typed field reports whether it is writable, its source, and any
read-only reason. `trading.freeze` and trading-limit changes are human-only.
No setting can bypass gateway/account/client pins, preview-token checks,
WhatIf/eligibility, journal health, daemon authorization, or origin gating.
See [Platform Settings](design/platform-settings.md) for the exact ownership
contract.

## From Policy to Decision

Every policy-backed decision should preserve five separate facts:

1. **Identity:** kind, schema version, policy ID/version, and semantic
   fingerprint.
2. **Evidence:** typed account, position, market, statement, calendar, or
   broker input with provenance, freshness, and completeness.
3. **Evaluation:** pure semantics that return pass, watch, breach, unknown, or
   unapproved without doing I/O.
4. **Enforcement:** advisory, shadow, pre-trade gate, or post-trade exception,
   including the risk-reducing and unavailable-data posture.
5. **Reporting:** the durable event or state revision and the typed result
   rendered to the operator.

Missing, stale, partial, or contradictory required evidence is not a weak
warning. It is an explicit unknown or data-quality condition. Likewise, a
policy can be valid but incomplete: `unapproved` is a useful state and must not
be converted into an implicit default.

## Commissioning and Work in Progress

Code availability, policy approval, activation, and reliable delivery are
separate milestones. The constitution's statement-backed reconciliation and
clean-report automation are implemented. Later cadence and governance-nudge
fields may be present in the binary while the installed desk policy is still
on an earlier approved version. Unified alert producers may also be collecting
shadow evidence while delivery remains inactive.

Do not infer that a control pages, blocks, or owns a workflow merely because
its schema or evaluator exists. Treat the installed typed status and
fingerprint as current truth, and keep any explicit `shadow`, `unapproved`, or
`delivery_active=false` boundary intact. The detailed commissioning records
are [Risk Governance Nudges](design/risk-governance-nudges.md) and
[Alerts and Regime Production](design/alert-regime-production.md).

## Operator Change Procedure

For a policy-file change:

1. Read the current typed status and record the active identity/fingerprint.
2. Edit the authoritative file, changing only approved values.
3. Raise `policy_version`; never reuse a version for different content.
4. Wait for reload or use the normal operator restart path.
5. Re-read the typed status and confirm `active`, the new version/fingerprint,
   expected effective values, and no drift/error/blocker.
6. Exercise the affected read or preview surface. For a new enforcement rule,
   retain shadow/replay evidence before any separately approved promotion.

For a runtime-setting change, read `access` and `source`, update one allowlisted
key, then re-read the settings result. For a code-owned guardrail or policy
schema change, use the policy and daemon contract templates and treat the
change as a reviewed release change, not an operator configuration edit.

## Reference Map

- [Configuration Reference](reference/config.md): every config, protection
  policy, opportunity policy, runtime-setting, and environment key.
- [Risk Constitution Design](design/risk-policy.md): capital and reconciliation
  authority, safety invariants, and phase history.
- [Trading Rulebook](design/trading-rulebook.md): compiled discipline checks
  and policy identity.
- [Trading Harness Development](guides/trading-harness-development.md): how to
  design, shadow, promote, and reconcile a new policy-backed control.
- [Architecture](architecture.md): daemon, RPC, adapter, and persistence
  ownership.
- [Database](database.md): where policy state, events, and typed projections
  are stored without making SQLite the policy-authoring surface.
