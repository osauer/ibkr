# Trading Rulebook

Updated: 2026-07-07 08:41 CEST
Status: senior-reviewed 2026-07-07 (verdict: build with amendments — all
amendments folded in below); implementation may start.

A daily, mechanical 12-rule checklist evaluated daemon-side against the live
book, surfaced as advisory breaches in CLI/MCP/SPA and as non-blocking causes
on order previews. Distilled from a discretionary-trader review of the
account on 2026-07-06; the behavioral target is "hardest trade first", not
"most comfortable trade first".

## Scope

- Goal: encode rules 1–12 (below) as a versioned policy evaluated by the
  daemon; advisory-only in v1. Rule 13 (hardest-first) is the ranking of the
  output, not a rule row. Rule 14 (exit triggers), FX exposure, and rule
  12(b) (regime-aware wing-sell preview cause) are out of scope for v1.
- User-facing surfaces: `ibkr rules [--json]`, MCP `ibkr_rules`, SPA rules
  card on the overview beside the canary hero with in-place drill-in, and
  advisory `rule_*` warnings on `ibkr order preview`.
- Owner layers: rulebook policy (embedded default + operator TOML,
  protection-policy manager semantics), earnings cache (daemon XDG cache),
  manual earnings overrides + feature toggle (runtime platform settings),
  rule evaluation (daemon), rendering (CLI/MCP/SPA adapters, no policy
  duplication).
- Existing behavior: canary signals already cover margin cushion, gross/net
  exposure, and single-name exposure; proposals already run theta_hygiene
  and risk_reduction buckets. The rulebook does not replace these; it
  presents a fixed 12-rule daily contract on top of the same aggregation.

## Which verdict wins when

Three surfaces measure overlapping metrics with different bars, by design:
the **canary** is regime×portfolio alerting (compiled thresholds, push
alerts), **proposals** are executable protection orders (protection-policy
TOML), the **rulebook** is the operator's daily discipline checklist
(rulebook TOML). Same measurements, different questions. Containment so this
never drifts into contradiction:

- One aggregation: rule evaluation consumes the same
  `PositionsPortfolio`/`PositionGroup`/`UnderlyingExposure` values the canary
  reads; a Go test asserts observed values are identical across both
  consumers. Bars may differ; observations may not.
- The rulebook TOML ships with comments cross-referencing the sibling
  thresholds (canary single-name watch 35 compiled; protection
  risk-reduction target 25) and which surface owns which question.
- Every rendered breach shows observed value next to threshold, so two
  surfaces disagreeing on severity still visibly agree on the number.

## Decisions locked with the operator (2026-07-07)

1. Earnings dates: free web fetch (Nasdaq per-symbol endpoint) + manual
   override; unknown renders as `unknown`, never a false pass.
2. Enforcement: advisory + preview causes. No hard blocks in v1.
3. SPA: compact card on the overview + drill-in. No new tab.
4. Hook: read-only `ibkr orders` allowlisted explicitly (see Hook section).

## The 12 rules

Inputs available today unless marked otherwise. "Exposure" for a name =
stock shares×spot + Σ(option delta×100×contracts×spot), from
`UnderlyingExposure`/`PositionGroup` aggregation (base currency).

| # | Rule id | Check | Default threshold | Status when breached |
|---|---|---|---|---|
| 1 | `single_name_exposure` | per-name exposure / NLV | > 40% | act; watch ≥ 30% |
| 2 | `option_line_premium` | single option line market value / NLV | > 5% | watch; act > 10% |
| 3 | `cash_sell_only` | total cash / NLV | < −25% | act ("sell-only mode") |
| 4 | `extrinsic_budget` | Σ long-option extrinsic / NLV | > 10% | watch; act > 15% |
| 5 | `expiry_runway` | long option DTE < 14 unless ≥70-delta ITM or hedge leg | < 14 DTE | watch; act < 7 DTE |
| 6 | `catalyst_coverage` | OTM long option expiring before the name's next earnings | expiry < earnings | watch |
| 7 | `overwrite_earnings` | short call spanning earnings | see ET semantics below | act |
| 8 | `earnings_size_freeze` | name ≤3 US sessions from earnings while rule 1 breached on it | ≤3 sessions | act |
| 9 | `red_on_green` | stock-leg day-change ≤ −1.5% while SPY day-change ≥ +0.5% | intraday only | watch |
| 10 | `winner_trim` | stock-leg day-change ≥ +4% with per-name exposure ≥ 15% NLV | intraday only | watch |
| 11 | `green_day_action` | account daily P&L > 0 while any act-severity rule open | portfolio tape | info (nudge, never act) |
| 12 | `hedge_integrity` | hedge short-delta / gross long delta outside 25–35% band | band | watch |

Row status enum: `pass | info | watch | act | unknown | not_evaluated`.
`info` renders neutral; it exists so rule 11 never inflates severity. The
five non-pass states are load-bearing: **no input condition may ever
produce `pass` by absence of data.**

Semantics notes:

- Ranking (rule 13): estimated exposure impact descending where the rule has
  a natural impact (1, 2, 4, 5, 6, 7, 8, 10, 12 = offending exposure or
  premium in base currency); rules 3, 9, 11 rank by severity then rule
  number. Impact definition lives beside each rule in the policy file.
- Hedge legs (rules 5, 12): long puts on a policy-owned index list (default
  `SPY, SPX, SPXW, QQQ, IWM`). Classification is disclosed per leg in the
  result; rule 5 lists which legs it exempted; while rule 12's band is
  breached high (over-hedged) the rule-5 hedge exemption is suppressed so a
  misclassified directional put resurfaces. Short index puts have positive
  delta and are never classified hedge.
- Rules 9/10 evaluate only during the US equity session
  (`marketcal.SessionAt`) and only from existing stock-leg quote enrichment
  (`DayChangePct`) plus the regime snapshot's SPY tape. **No new market-data
  subscriptions** (100-slot budget). Option-only names: `not_evaluated`
  with reason `no_stock_leg_tape`. Off-session: `not_evaluated`.
- Rules 6/7/8 report `unknown` when the earnings date is unknown or stale
  (staleness threshold in policy). Estimated dates (Nasdaq flags these)
  evaluate normally but evidence discloses `estimated`; manual overrides are
  authoritative. Comparisons are computed in ET with `time_of_day`: AMC
  earnings on an option's expiry day do NOT breach rule 7 (the option dies
  before the gap); BMO earnings count the prior session as last runway day;
  unknown `time_of_day` is conservative (flag, with ambiguity disclosed).
  Rule 8 counts US sessions via `marketcal`, not calendar days.
- Greeks gaps (rules 1, 4, 12): a leg with material notional (≥ policy
  floor, default 1% NLV) missing delta makes that name's rule-1/12 row
  `unknown` naming the leg; rule 4 goes `unknown` when uncomputable
  extrinsic exceeds the same floor. Mirrors the proposal engine's
  `extrinsic_uncomputable` rigor — never a silent skip.
- Every rule row carries: id, title, status, observed, threshold, evidence,
  per-name offenders (worst first), exempted/unknown legs where relevant,
  and data-quality notes.

## Input health (result-level gate)

`RulesResult.InputHealth` mirrors the canary's source-health pattern: one
entry per source (positions, account, regime, earnings, session) with
status/as_of/reason. When positions or account are pending, stale, or absent
— boot races included — every portfolio-dependent row is `unknown` with the
source reason. A cold daemon renders a column of `unknown`, never 12 green
rows. This is the acceptance criterion for the property test below.

## Architecture

```
internal/risk/rulebook.go         rule ids, typed inputs, Evaluate() (pure)
internal/risk/rulebook_policy.go  RulebookPolicy, defaults, fingerprint
internal/risk/option_math.go      intrinsic/extrinsic/spread helpers hoisted
                                  from proposal_engine (shared, one copy)
internal/daemon/rulebook.go       input assembly, rules.snapshot handler,
                                  cached-eval provider for preview causes
internal/daemon/earnings_cache.go async fetch + LKG cache (fx_cache mirror)
internal/rpc/rulebook.go          MethodRulesSnapshot, RulesResult, RuleRow
internal/cli/rules.go             `ibkr rules` renderer
internal/mcp/tools.go             ibkr_rules tool
internal/app/live/service.go      snapshot.rules sibling section (SSE)
web/app/*                         rules card + drill-in
```

- **Placement (review finding 1):** canary is computed adapter-side
  (`internal/cli`), so rules do NOT attach to `CanaryResult`. Rules are a
  daemon RPC (`rules.snapshot`) and a **sibling section** of the app live
  snapshot (`Rules *rpc.RulesResult` beside `Canary`), riding the existing
  SSE event. `BuildCanaryFingerprint`, push-alert dedupe, and the
  `ibkr_canary` payload are untouched.
- Evaluation is stateless and on-demand; the pure core lives in
  `internal/risk` with table tests. No new scheduler for evaluation.
- **Earnings fetches are strictly off the snapshot path.** `rules.snapshot`
  only observes cache staleness and kicks an async refresher: bounded
  concurrency (≤4), ~3s per-fetch budget, per-symbol failure memory with
  retry-after (borrow-fee pattern), LKG served meanwhile, rows `unknown`/
  `stale` until data lands. Endpoint spike (2026-07-07, empirical):
  `Go-http-client/1.1` is reset at connection level by api.nasdaq.com
  (exit 000 in 0.17s); a browser-identifying UA with `Accept:
  application/json` returns 200 in ~2–3s. The fetcher therefore sends a
  browser-style UA — a deliberate, documented choice, not the spx-fetcher
  convention. Strict parser against recorded fixtures; any ambiguity →
  `unknown` + source degradation, never a guessed date. Symbol mapping is an
  explicit tested function (IBKR `BRK B` → Nasdaq `BRK.B`); non-US listings
  are out of Nasdaq coverage and stay `unknown` unless overridden — for this
  book that means EUR names; the drill-in shows per-name source
  (`fetched | estimated | override | unknown`) via `RulesResult.Earnings[]`.
- Cache: `~/.cache/ibkr/earnings-dates.json`
  `{version, entries: {SYM: {date, time_of_day, estimated, observed_at,
  source}}}`, fresh 24h, TTL 45d, 1/min throttled flush, atomic rename.
  Manual override `features.rulebook.earnings_overrides` (map sym →
  YYYY-MM-DD or YYYY-MM-DDTamc/bmo, `null` clears) wins over fetch;
  platform-settings contract (access/source/reason) applies.
- **Preview causes:** the preview handler consults a short-TTL cached last
  evaluation (45s, with `rules_as_of` echoed in the warning detail) — never
  a fresh assembly per preview. When the drafted order would worsen a
  currently breached rule (increase the breached metric; reduce/close never
  warns), it appends `DataWarning{Code: "rule_<id>", Severity: <the rule's
  own watch|act>, Scope: "rulebook"}`. No ninth severity word;
  `submit_eligible` is never affected.
- Policy: embedded default `rulebook-v1` + optional
  `~/.config/ibkr/policies/rulebook-policy.toml` using the
  protection-policy manager semantics **exactly** (same drift/version/
  fingerprint behavior, 30s reload) — no third dialect.
- Journal: rule-status transitions append to
  `~/.local/share/ibkr/trading-state/rules-decisions.jsonl`
  (regime-decisions precedent) so week-one threshold calibration has data.
- Settings: `features.rulebook.enabled` (default true, runtime) gates
  evaluation + SPA card + preview causes; disabled leaves `ibkr rules`
  readable with `status: disabled` (stock_protection pattern).

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback |
|---|---|---|---|---|
| Rule thresholds | rulebook policy (embedded + TOML) | `RulesResult.PolicyFingerprint` | all | embedded `rulebook-v1` |
| Rule verdicts | daemon `rules.snapshot` | `RulesResult.Rules []RuleRow` | CLI/MCP/SPA render only | per-row `unknown`/`not_evaluated`, result-level InputHealth |
| Earnings dates | daemon earnings cache (fetch ∪ override) | `RulesResult.Earnings[]` | same | `unknown`; stale flagged; per-name source shown |
| Preview causes | daemon preview handler (cached eval ≤45s) | `Warnings[].Code = rule_*`, `Scope = rulebook` | order preview surfaces | absent when rules disabled/stale beyond TTL |
| Feature toggle + overrides | platform settings (runtime) | `features.rulebook.*` | settings surfaces + SPA | defaults on |

## SPA (authority-matrix row)

| UI concept | Label | Source | Snapshot path | Fixture/test | Stale/error | QA gate |
|---|---|---|---|---|---|---|
| Rules card | "Rules" | live snapshot sibling section | `snapshot.rules` | browser_script_ids_test + app-browser-smoke | worst 2–3 breaches as tone pills; `unknown` neutral; InputHealth degradation shows as card-level note; card hidden when disabled | `make app-check` + `make app-refresh-smoke` |

Card: `#canaryRulesCard` beside the canary hero (worst 2–3 breaches as
severity pills, ranked per rule 13) + `#canaryRulesToggle` expanding
`#canaryRulesDetailPanel` with the full 12-row `.detail-grid` (tone classes
`risk|warn|ok|neutral`; `info` and `unknown` render neutral). Each breach
card shows observed vs threshold. Money strings arrive daemon-rendered with
real currency (the compat test bans a `"USD"` literal in app.js). Read-only.

Sequencing (review finding 14): the in-flight SPA polish working set
(app.js/index.html/styles.css/app_compat_test.go/app-browser-smoke.mjs/
service.go) is committed **before** rules-card work begins; new ids land in
`browser_script_ids_test.go` in the same change.

## Safety invariants (unchanged)

- Advisory only: no rulebook state may alter `submit_eligible`, blockers,
  freeze, pins, tokens, or any gated broker-write path.
- Nil means unavailable; unknown earnings ≠ pass; off-session tape rules are
  `not_evaluated`; absent inputs are `unknown`. Never false pass.
- `as_of`, InputHealth, and policy fingerprint ride every result.
- MCP description states when to invoke (daily review, "what should I fix
  today") and when not (`ibkr_canary` for regime×portfolio alerting,
  `ibkr_proposals` for executable protection orders).

## Hook (re-scoped after review finding 2)

At HEAD the repo hook already allowed all read-only `orders` forms; the
operator's block came from the stale deployed plugin cache (1.14.0, old
broad matcher). Landed 2026-07-07: bare `ibkr orders` moved from
fall-through-allow to the explicit read-only allowlist (intent now
documented in the matcher), plus `hooks/ibkr-pre-tool-use_test.sh` — a
19-case table-driven behavior test wired into `make check` via
`hook-behavior-check`, covering false-block (read paths) and false-allow
(compound/subshell writes, frozen place) directions. Remaining step is
human: plugin-cache redeploy at next plugin release.

## Verification

- **Never-false-pass property test** (acceptance for the safety invariant):
  for every rule, nil each input dimension — positions empty/pending,
  account absent, greeks stripped, earnings unknown, off-session — and
  assert `unknown`/`not_evaluated`, never `pass`.
- Unit: `internal/risk` table tests per rule (pass/info/watch/act/unknown/
  not_evaluated, hedge classification incl. suppression, ranking, greeks
  floors); hoisted option-math helpers keep proposal-engine tests green.
- Earnings: recorded-fixture parse tests (normal, estimated, malformed,
  missing-date, human-format dates); symbol-normalization tests (BRK.B,
  EUR names → unsupported); ET/DST table tests (BMO Monday after Friday
  expiry, AMC on expiry day, DST boundary week); cache
  load/save/TTL/override-precedence; refresher backoff + failure memory.
- Aggregation-consistency drift test: canary concentration and rule 1 read
  identical exposure values (review finding 6a).
- Preview causes: worsen logic incl. reduce/close exemption; TTL staleness;
  severity vocabulary unchanged (no new words).
- Contract: CLI JSON golden for `ibkr rules --json`; MCP parity test;
  `make docs-regen` for MCP + config references.
- SPA: browser_script_ids_test additions; app-browser-smoke exercises card,
  drill-in, and InputHealth-degraded rendering.
- Live gate: full `make smoke` + before/after artifacts per the
  daemon-cli-trading-contract template: `ibkr status --json`, `ibkr rules
  --json` against the live book, `ibkr order preview … --json` showing a
  `rule_*` warning, SPA rendered-flow screenshot.

## Rollback

- Revert files above; runtime state added: `earnings-dates.json` cache,
  `features.rulebook.*` settings keys, `rules-decisions.jsonl` journal (all
  safe to orphan).
- User-visible on rollback: rules card, `ibkr rules`, and advisory `rule_*`
  preview warnings disappear; no trading-path change either way.
