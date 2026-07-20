# Trading Rulebook

Updated: 2026-07-08 17:40 CEST
Status: senior-reviewed 2026-07-07 (verdict: build with amendments — all
amendments folded in below); shipped in v1.15.0. Rule 1's hedge-exemption
semantics were amended post-ship after the first live-market run
(2026-07-07); see the rule-1 semantics note. Rulebook v2 (2026-07-08, dual
senior review against a live confirmed-stress tape): two never-false-pass
fixes (rules 6/8 silent skips), regime-conditional thresholds for rules
3/4/12, rule 2 hedge-premium tier + rule 12 act tier (atomic pair), rule 7
short-put coverage, rule 1 provable lower bounds, new rules 13
(exit_discipline) and 14 (fx_exposure), stock-leg underlying join.

A daily, mechanical 14-rule checklist evaluated daemon-side against the live
book, surfaced as advisory breaches in CLI/MCP/SPA and as non-blocking causes
on order previews. Distilled from a discretionary-trader review of the
account on 2026-07-06; the behavioral target is "hardest trade first", not
"most comfortable trade first".

## Scope

- Goal: encode rules 1–14 (below) as a versioned policy evaluated by the
  daemon; advisory-only. The hardest-first ranking is a property of the
  output, not a rule row. Still out of scope after v2: rule 12(b)
  (regime-aware wing-sell preview cause), carried-forward greeks for
  off-session rule-12 evaluability (deferred with a documented morning
  consequence: hedge classification — rule 2's tier, rule 1/13 exemptions —
  needs a live delta, so those tiers only engage once greeks land intraday;
  pre-market the affected legs conservatively take the stricter non-hedge
  path), an fx_exposure act tier + preview cause (watch-only until the TOML
  policy loader ships), and the TOML loader itself.
- User-facing surfaces: `ibkr rules [--json]`, MCP `ibkr_rules`, SPA rules
  card on the overview beside the canary hero with in-place drill-in, and
  advisory `rule_*` warnings on `ibkr order preview`.
- Owner layers: rulebook policy (embedded default + operator TOML,
  protection-policy manager semantics), earnings state (daemon.db),
  manual earnings overrides + feature toggle (runtime platform settings),
  rule evaluation (daemon), rendering (CLI/MCP/SPA adapters, no policy
  duplication).
- Existing behavior: canary signals already cover margin cushion, gross/net
  exposure, and single-name exposure; proposals already run theta_hygiene
  and risk_reduction buckets. The rulebook does not replace these; it
  presents a fixed 14-rule daily contract on top of the same aggregation.

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

## The 14 rules

Inputs available today unless marked otherwise. "Exposure" for a name =
stock shares×spot + Σ(option delta×100×contracts×spot), from
`UnderlyingExposure`/`PositionGroup` aggregation (base currency).
Rules 3/4/12 thresholds are regime-conditional: calm / early_warning /
confirmed sets selected by the latched regime lifecycle stage (see the
regime-conditionality notes).

| # | Rule id | Check | Default threshold | Status when breached |
|---|---|---|---|---|
| 1 | `single_name_exposure` | per-name exposure / NLV; rule-12-sized short hedge exposure exempt (see notes); provable lower-bound breaches under greeks gaps (see notes) | > 40% | act; watch ≥ 30% |
| 2 | `option_line_premium` | single option line market value / NLV; rule-12-classified hedge legs use the hedge tier | > 5% (hedge tier > 15%) | watch; act > 10% (hedge tier act > 25%) |
| 3 | `cash_sell_only` | total cash / NLV | < −25 / 0 / +10% by regime | act ("sell-only mode") |
| 4 | `extrinsic_budget` | Σ long-option extrinsic / NLV, excluding rule-12-classified hedge legs (hedge extrinsic disclosed in notes) | > 10/15 · 7.5/12 · 5/10 by regime | watch; act at the higher bar |
| 5 | `expiry_runway` | long option DTE < 14 unless ≥70-delta ITM or hedge leg | < 14 DTE | watch; act < 7 DTE |
| 6 | `catalyst_coverage` | OTM long option expiring before the name's next earnings; legs with no underlying spot are named unknowns, never skipped | expiry < earnings | watch |
| 7 | `overwrite_earnings` | short option spanning earnings: short calls act; short puts watch, act on assignment notional (≥ 10% NLV line, ≥ 20% name) | see ET semantics below | act / watch |
| 8 | `earnings_size_freeze` | name ≤3 US sessions from earnings while rule 1 breached on it; greeks-gapped names stay unknown unless earnings provably outside the window | ≤3 sessions | act |
| 9 | `red_on_green` | stock-leg day-change ≤ −1.5% while SPY day-change ≥ +0.5% | intraday only | watch |
| 10 | `winner_trim` | stock-leg day-change ≥ +4% with per-name exposure ≥ 15% NLV | intraday only | watch |
| 11 | `green_day_action` | account daily P&L > 0 while any act-severity rule open | portfolio tape | info (nudge, never act) |
| 12 | `hedge_integrity` | hedge short-delta / gross long delta outside the regime band | 25–35 / 30–50 / 40–70% by regime | watch; act > 2× band top |
| 13 | `exit_discipline` | long option line unrealized loss / premium paid; rule-12-classified hedge legs exempt (decay is the cost of protection) | ≥ 40% | watch; act ≥ 60% |
| 14 | `fx_exposure` | Σ non-base-currency NLV / NLV | > 60% | watch only (see notes) |

Row status enum: `pass | info | watch | act | unknown | not_evaluated`.
`info` renders neutral; it exists so rule 11 never inflates severity. The
five non-pass states are load-bearing: **no input condition may ever
produce `pass` by absence of data.**

Semantics notes:

- Ranking (hardest-first; the number 13 was reassigned to exit_discipline
  in v2): estimated exposure impact descending where the rule has a natural
  impact (1, 2, 4, 5, 6, 7, 8, 10, 12, 13 = offending exposure, premium, or
  salvageable premium in base currency); rules 3, 9, 11, 14 rank by
  severity then rule number. Impact definition lives beside each rule in
  the policy file.
- Hedge legs (rules 1, 5, 12): long puts on a policy-owned index list (default
  `SPY, SPX, SPXW, QQQ, IWM`). Classification is disclosed per leg in the
  result; rule 5 lists which legs it exempted; while rule 12's band is
  breached high (over-hedged) the rule-5 hedge exemption is suppressed so a
  misclassified directional put resurfaces. Short index puts have positive
  delta and are never classified hedge.
- Rule 1 hedge exemption (post-ship amendments, 2026-07-07, from the first
  live-market run): a policy-hedge index name carrying net-short delta is
  the hedge, not concentration — rule 12 owns its sizing, and ranking it
  here buried the real offenders (40 SPY 710 puts ≈ $722k short
  delta-dollars, 234% of NLV, outranked every real name while rule 12
  already flagged the same position over-band; with the hedge noise gone,
  MSFT at 119% of NLV surfaced as the true top offender). The exemption
  covers only what rule 12 can actually size: legs passing the shared
  `rule12HedgeLeg` predicate (long put on a hedge-listed underlying with
  delta and underlying present), capped at the name's net-short exposure,
  disclosed in the row's Exempt list with a rule-12 pointer — never
  silently dropped. Residual short beyond the sized legs stays a
  concentration offender, and a hedge-symbol short with no sizeable legs
  (short stock, short calls) gets no Exempt row at all — it ranks as
  ordinary concentration. Long index exposure is always ordinary
  concentration. Unlike rule 5, this exemption is not suppressed while
  rule 12 is over-band: the oversized short is exactly what rule 12 flags,
  and double-counting it as concentration was the original failure mode.
- Rules 9/10 evaluate only during the US equity session
  (`marketcal.SessionAt`) and only from existing stock-leg quote enrichment
  (`DayChangePct`) plus one dedicated best-effort SPY snapshot quote per
  evaluation (`spyDayChangePct`, 2.5s budget; correction 2026-07-08 — the
  implementation never read the regime snapshot's SPY tape). **No standing
  market-data subscriptions** (100-slot budget). Option-only names:
  `not_evaluated` with reason `no_stock_leg_tape`. Off-session:
  `not_evaluated`.
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

Rulebook v2 semantics (2026-07-08 dual senior review — trading-semantics
and Go-implementation lenses, both "implement with amendments"):

- **Partial data may indict, never acquit.** Rule 1 computes a provable
  per-name minimum when material legs miss delta: known legs are already in
  `ExposureBase`; each delta-less leg contributes a signed interval (long
  call: intrinsic…notional, since delta·S ≥ C ≥ intrinsic; long put:
  −notional…0 — put intrinsic is NOT a bound on |delta·S| and is never
  used; shorts mirrored; missing underlying or FX ⇒ unbounded). A breach is
  asserted only when the interval minimum alone crosses the bar, rendered
  "≥ X%" with `observed_is_lower_bound`; anything short of provable stays
  `unknown`.
- **Regime conditionality (rules 3/4/12).** The daemon latches the regime
  lifecycle stage on every regime snapshot, buckets it (quiet/opportunity →
  calm; early_warning/stabilization → early_warning; confirmed_stress/
  panic/forced_defense → confirmed; data_quality holds the previous latch;
  unrecognized future stages take the MIDDLE bucket, never silently calm)
  and persists it as a versioned daemon.db state document so a restart
  mid-stress cannot reset thresholds to calm. Stage older than
  `regime_stage_max_age_minutes` (default 240) serves as *carried*: the
  rule evaluates under BOTH the carried set and the calm set and reports
  the worse verdict — stale regime data can hold or tighten, never relax,
  in either band direction (a stale "confirmed" hedge band is wider than
  calm and would otherwise acquit). A cold or stale latch kicks one async
  regime refresh (single-flight, 10-minute cooldown, never from the
  preview path). Row notes always disclose which set applied and why.
- **Rule 2 hedge tier + rule 12 act tier are an atomic pair.** Hedge-
  classified legs (`rule12HedgeLeg`) measure against 15/25% of NLV; the
  oversized-hedge act moves to rule 12 at >2× the band top. Shipping one
  without the other leaves an oversized hedge with no act anywhere. The
  rule-12 act evidence states explicitly: the flag is sizing honesty ("a
  directional short wearing a hedge's clothing"), not a directive to get
  long during stress. Unclassifiable legs (no live delta) take the normal
  tier — no relief without classification.
- **Rule 13 exit_discipline** fences long-option losses at −40/−60% of
  premium paid (cost basis = multiplier-inclusive AvgCost × |contracts| ×
  FX — never re-multiplied). Hedge legs are exempt: a decayed hedge's
  problem is lost protection, which rule 12's under-band arm catches as
  deltas shrink, not lost premium. Averaging down resets the basis and
  clears the fence — documented deliberately; the order-preview cause on
  adding to a flagged line is the guard.
- **Rule 14 fx_exposure is watch-only in v2.** At structurally high
  non-base exposure (this book: ~90% USD vs EUR base) a permanent act and
  an every-USD-order preview cause would be pure alarm fatigue — a warning
  with a 100% base rate trains the operator to ignore `rule_*` causes.
  Never-false-pass corroboration: an empty `CurrencyExposure` report only
  passes as "base-only" when the positions snapshot shows no non-base leg
  and no FX sensitivity; otherwise the row is `unknown` (`fx_unavailable`).
- **Stock-leg underlying join.** An option leg whose greeks tick carried no
  underlying spot borrows the same-name stock leg's account mark
  (`UnderlyingSource: stock_leg_mark`; quality-gated `Mark > 0 && !Stale`),
  making rules 4/6 evaluable pre-market for stock-backed names. Derived
  spots support OTM-ness and extrinsic; they never classify hedge legs —
  pairing a greeks-tick delta with a different-source spot is exactly the
  apples-and-oranges sizing the join exists to avoid.

## Input health (result-level gate)

`RulesResult.InputHealth` mirrors the canary's source-health pattern: one
entry per source (account, positions, regime_stage, earnings, tape) with
status/as_of/reason. When positions or account are pending, stale, or absent
— boot races included — every portfolio-dependent row is `unknown` with the
source reason. A cold daemon renders a column of `unknown`, never 14 green
rows. This is the acceptance criterion for the property test below.

## Architecture

```
internal/risk/rulebook.go         rule ids, typed inputs, Evaluate() (pure)
internal/risk/rulebook_policy.go  RulebookPolicy, regime sets, fingerprint
internal/risk/option_math.go      intrinsic/extrinsic/spread helpers hoisted
                                  from proposal_engine (shared, one copy)
internal/daemon/rulebook.go       input assembly, rules.snapshot handler,
                                  cached-eval provider for preview causes
internal/daemon/rulebook_regime_stage.go
                                  regime stage bucket/latch/persist/kick
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
- Persistence: daemon.db current state plus immutable earnings observations,
  shaped as `{version, entries: {SYM: {date, time_of_day, estimated,
  observed_at, source}}}`, fresh 24h, TTL 45d, 1/min throttled flush.
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
- Policy: embedded default `rulebook-v2` (Version 2; every threshold —
  including the three regime sets — is part of `FingerprintKey`, so a
  threshold outside the fingerprint is impossible without failing the
  fingerprint test). The optional operator TOML override
  (`~/.config/ibkr/policies/rulebook-policy.toml`, protection-policy
  manager semantics, 30s reload) is **planned, not shipped** — v1 doc
  described it aspirationally; corrected 2026-07-08. Until it lands,
  threshold changes are code changes.
- Evidence: rule-status transitions append as typed events to the daemon's
  sole live authority, `~/.local/state/ibkr/daemon.db`, so threshold
  calibration has data. The latched regime bucket is a versioned state
  document in the same database. Retired JSON/JSONL paths exist only as
  one-time cutover inputs and isolated test seams.
- Settings: `features.rulebook.enabled` (default true, runtime) gates
  evaluation + SPA card + preview causes; disabled leaves `ibkr rules`
  readable with `status: disabled` (stock_protection pattern).

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback |
|---|---|---|---|---|
| Rule thresholds | rulebook policy (embedded; TOML planned) × latched regime stage for rules 3/4/12 | `RulesResult.PolicyFingerprint` | all | embedded `rulebook-v2`; stage carried/never-seen ⇒ worse-of/calm with disclosure |
| Rule verdicts | daemon `rules.snapshot` | `RulesResult.Rules []RuleRow` | CLI/MCP/SPA render only | per-row `unknown`/`not_evaluated`, result-level InputHealth |
| Earnings dates | daemon earnings cache (fetch ∪ override) | `RulesResult.Earnings[]` | same | `unknown`; stale flagged; per-name source shown |
| Preview causes | daemon preview handler (cached eval ≤45s) | `Warnings[].Code = rule_*`, `Scope = rulebook` | order preview surfaces | absent when rules disabled/stale beyond TTL |
| Feature toggle + overrides | platform settings (runtime) | `features.rulebook.*` | settings surfaces + SPA | defaults on |

## SPA (authority-matrix row)

| UI concept | Label | Source | Snapshot path | Fixture/test | Stale/error | QA gate |
|---|---|---|---|---|---|---|
| Rules card | "Rules" | live snapshot sibling section | `snapshot.rules` | browser_script_ids_test + app-browser-smoke | worst 2–3 breaches as tone pills; `unknown` neutral; InputHealth degradation shows as card-level note; card hidden when disabled | `make app-check` + `make app-refresh-smoke` |

Card: `#canaryRulesCard` beside the canary hero (worst 2–3 breaches as
severity pills, ranked hardest-first) + `#canaryRulesToggle` expanding
`#canaryRulesDetailPanel` with the full 14-row `.detail-grid` (tone classes
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
  account absent, greeks stripped, earnings unknown, off-session, per-leg
  underlying stripped with healthy positions (the rule-6 live false pass of
  2026-07-08), FX report absent, cost bases missing — and assert
  `unknown`/`not_evaluated`, never `pass`.
- v2 unit coverage: regime-set selection incl. carried worse-of in both
  band directions and never-seen disclosure; rule 2 tier split incl.
  unclassifiable fallback; rule 7 short-put notional tiers incl. unknown-FX
  no-quiet-escalation; rule 8 gap propagation (unknown/near/provably-out);
  rule 1 lower-bound direction-awareness (long-put blocks the bound);
  exit-discipline fences + hedge exemption; fingerprint mutation coverage
  for every new policy field.
- Unit: `internal/risk` table tests per rule (pass/info/watch/act/unknown/
  not_evaluated, hedge classification incl. suppression, rule-1 hedge
  exemption incl. residual and no-sizeable-legs cases, ranking, greeks
  floors); hoisted option-math helpers keep proposal-engine tests green.
- Doc drift: `TestDesignDocDisclosesRule1HedgeExemption` (`internal/risk`)
  pins this file's rule-1 hedge-exemption wording to the shared predicate,
  so a semantics change fails the hermetic suite until this doc is updated
  (trading-paper-smoke drift-guard precedent).
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

- Revert files above; runtime state added: daemon.db earnings/settings/latch
  documents, earnings observations, and rule-transition events. A rollback may
  ignore the rulebook records, but must not delete or replace daemon.db.
- User-visible on rollback: rules card, `ibkr rules`, and advisory `rule_*`
  preview warnings disappear; no trading-path change either way.
