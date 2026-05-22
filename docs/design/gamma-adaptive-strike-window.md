# Gamma adaptive strike window

**Status:** PR #1 (picker fix) implemented and gated by `make check && make smoke`. Remaining roadmap items deferred pending live measurement of PR #1's impact.
**Last update:** 2026-05-22 05:38 CEST
**Owner:** osauer
**Related code:** [gamma_zero_compute.go](../../internal/daemon/gamma_zero_compute.go), [gamma_skew.go](../../internal/daemon/gamma_skew.go), [gamma_zero_cache.go](../../internal/daemon/gamma_zero_cache.go), [gamma_handler.go](../../internal/daemon/gamma_handler.go), [cli/gamma.go](../../internal/cli/gamma.go).

## Problem

`ibkr gamma` enumerates strikes within ±10% of spot for every selected expiration. See [gamma_zero_compute.go:1108](../../internal/daemon/gamma_zero_compute.go#L1108) (`filterStrikesAroundSpot`) and the call at [gamma_zero_compute.go:533](../../internal/daemon/gamma_zero_compute.go#L533).

±10% is correct for 0DTE / front-week, where essentially nothing with material gamma lives beyond it. It is wrong for monthlies (30–45 DTE) and quarterlies (60–180 DTE), where institutional tail hedges sit at ±15-30% out and our compute misses them. The term-bucket γ-zero (DTE > 7, `gamma_zero_compute.go:756`) is therefore systematically biased toward fewer, nearer-money strikes than reality — enough to flip its sign call on certain days.

Reference: this surfaced when comparing `ibkr gamma` output to vendor numbers; vendors widen for longer tenors as standard practice.

## Proposal — adaptive window rule

Replace the single `defaultStrikeWidthPct = 0.10` constant with a DTE-keyed lookup, evaluated per expiry inside the job-building loop ([gamma_zero_compute.go:531-540](../../internal/daemon/gamma_zero_compute.go#L531)):

| DTE band | Window | Rationale |
|---|---|---|
| 0–7 d | ±10 % | Current default; nothing material beyond at front-week tenor. |
| 8–45 d | ±20 % | Monthly OPEX hedges; first standard-deviation band for typical SPX vol over a month. |
| 46–180 d | ±30 % | Quarterly institutional collars, tail puts (e.g. 25 % OTM SPY puts). |
| > 180 d | skip | Current behavior; LEAPS have negligible day-to-day dealer hedging impact. |

Boundaries derive from where dealer activity discontinuously changes character:

- **7 d** is already the `nearDTECutoffYears` boundary in code ([gamma_zero_compute.go:40](../../internal/daemon/gamma_zero_compute.go#L40)), the dividing line between the near/term gamma buckets surfaced on the CLI. Using the same number for the window rule keeps one DTE concept in the codebase, not two.
- **45 d** matches the canonical monthly OPEX horizon (3rd-Friday cycle averages 30 d but extends to 45 d on the long edge of a month). It is also where one-standard-deviation SPX moves over the tenor cross ±20 %.
- **180 d** is where the chain density drops sharply (SPY listings beyond 6 months are sparse with $5+ strike spacing). LEAPS are not currently included by `selectExpirations`, but the rule documents the explicit upper bound for the same reason — clarity.

## Skew-fit quality under wider windows

`fitSkewCurve` ([gamma_skew.go:73](../../internal/daemon/gamma_skew.go#L73)) is unweighted least-squares over a quadratic σ(m) = A + B·m + C·m² with m = ln(K/S). Doubling or tripling the moneyness range pulls fit mass into the wings where the parabolic model degrades — the ATM estimate (which dominates the GEX integral) gets noisier even though more raw IV samples are in play.

Three options considered:

1. **Weighted LS by ATM-proximity** — `w_i = 1 / (1 + |m_i|/h)` for some bandwidth `h` (≈ 0.05 = 5 % moneyness half-width).
2. **Piecewise / SVI fit** — proper option-pricing skew model.
3. **Keep unweighted, report degradation honestly** — split the existing single `median R²` line into `min/median/max` so a wing-heavy fit is visible.

**Recommendation: (1) + (3).** (1) is a minimal patch to `fitSkewCurve`'s normal equations (each row multiplied by w_i; the matrix solve is unchanged). (3) costs nothing — same per-expiry data already in `SkewFitQuality`. (2) is overkill for v1 of this change; SVI introduces non-convex solves and an additional dependency for marginal benefit.

The bandwidth `h` for the weighted-LS choice is the load-bearing constant: too small and we over-fit the near-ATM cluster (defeats the point of including wings); too large and the weighting does nothing. Starting at `h = 0.05`, validate empirically by re-running today's session and confirming `median R²` is no lower than today's 0.76 even with wider strike inputs.

If `min R²` after weighting falls below ~0.5 on any expiry, that expiry will fall back to sticky-IV via the existing `curve.ok = false` path. No new code path needed.

## Slot-management audit — required reading

A 2026-05-21 incident ([project_opt_subscription_slot_leak](../../../.claude/projects/-Users-osauer-dev-ibkr/memory/project_opt_subscription_slot_leak.md)) leaked one rate-limiter slot per OPT subscription that received only model-computation ticks (msg 21). Doubling the leg count would resurrect that bug class if it were still live.

**Status: fix is in place and load-bearing.** [connector.go:1596-1640](../../pkg/ibkr/connector.go#L1596) shows `UnsubscribeMarketData` calls `CancelMarketData` unconditionally on connected sessions with a non-zero `ReqID`, regardless of `sub.Observed`:

```go
// internal/daemon/../pkg/ibkr/connector.go:1628
if c.conn != nil && c.conn.IsConnected() && sub.ReqID != 0 {
    if err := c.conn.CancelMarketData(sub.ReqID); err != nil {
```

The comment block at lines 1612-1627 explicitly memorialises the prior bug and the reason the `&& sub.Observed` guard was removed: `handleOptionComputation` does not set `Observed`, only `handleTickPrice`/`handleTickSize` do, so OPT-only subscriptions stayed `Observed=false` and skipped the cancel under the old guard. `CancelMarketData` is the call site of `releaseMarketDataSlot` ([ratelimiter.go:432](../../pkg/ibkr/ratelimiter.go#L432)), so leakage was 1:1 with subscriptions.

**Recheck before merging:** grep the codebase for any *other* place that reads `sub.Observed` as a gate on cancel/cleanup. If none, the path is clean. The current grep returns four `sub.Observed = ...` writes ([connector.go:1541, 2318, 2698, 2751, 3525](../../pkg/ibkr/connector.go)) and two test reads ([connector_tick_validation_test.go:195, 348](../../pkg/ibkr/connector_tick_validation_test.go)). No other read-as-gate sites.

**Per-leg defer pattern is correct.** `productionLegFetcher` at [gamma_zero_compute.go:299](../../internal/daemon/gamma_zero_compute.go#L299) holds `defer func() { _ = c.UnsubscribeMarketData(key) }()` immediately after `SubscribeOption`. Slot-acquisition is paired with cancel; the only way to leak is via panic before the defer registers, which Go forbids (defer registration is synchronous with the statement).

**Mitigation: surface a slot-watermark gauge.** Add a high-water mark (max observed `len(c.subscriptions)`-style or `rateLimiter.marketDataSubs` count) to the gamma result envelope, sampled every ~5 s during fan-out. If a future regression reintroduces leakage, the next run's output will show the watermark climbing run-over-run instead of bouncing around a steady ceiling.

## Cache invalidation

The cache key is currently NY session date alone ([gamma_zero_cache.go:157](../../internal/daemon/gamma_zero_cache.go#L157), `nySessionKey`). No params are hashed in. Additionally, [gamma_handler.go:72](../../internal/daemon/gamma_handler.go#L72) constructs `rpc.GammaZeroParams{}` and ignores the request params entirely — today, every call inside a session resolves to the same compute regardless of what `params` the request carried.

If `--strike-window=N` becomes a per-call flag, two callers in the same session passing different windows would share one compute and the second caller would silently get the first caller's window. Three options:

1. **Hash params into the session key.** `sessionKey = "2026-05-21|width=10|expiries=6"`. Multiple compute slots per day, one per param set.
2. **Treat the window rule as daemon-config.** One rule per running daemon process; `--strike-window` is read at daemon start and ignored on per-call RPC. Restart the daemon to change it.
3. **Force-recompute when params differ from cached.** If the cached compute used a different window, fall through to a fresh compute (same as `Force=true`).

**Recommendation: (1).** It is the smallest change that does the right thing for every call site: regime dashboard, ad-hoc CLI, MCP. The cost is up to N compute slots per session in pathological cases, but in practice the regime dashboard uses the default and only an explicit `--strike-window` flip is going to produce a key variation. Implementation: change `nySessionKey` to accept the params struct and append a stable hash; cache map switches from `current/refresh` (two pointers) to a small map keyed by full string.

(2) is tempting for simplicity but breaks ad-hoc CLI ergonomics: the user cannot get a one-off `--strike-window=10` reproduction without restarting the daemon. (3) loses the singleflight guarantee across param changes — a caller switching windows mid-day cancels work the regime dashboard is depending on.

## Compute-budget mitigation

Current numbers (from [gamma_zero_compute.go:74-78](../../internal/daemon/gamma_zero_compute.go#L74)):
- 6 expirations × ~80 strikes × 2 sides ≈ 960 legs worst case.
- 4 workers × 1.5 s/leg average → 6 min worst case; typical 2-4 min on warm contract cache.
- `computeETA = 240` (4 min midpoint).

Adaptive estimate (SPY strike density is ~$1 spacing near ATM, ~$5 in wings — so widening does *not* linearly scale leg count):

| Profile | Current ±10% | Adaptive |
|---|---|---|
| 0DTE expiry | ~80 strikes | ~80 (unchanged) |
| 8-45 DTE expiry | ~80 strikes | ~120 strikes (~50 % more, not 100 %) |
| 46-180 DTE expiry | ~80 strikes | ~140 strikes (~75 % more) |
| 6 expiries typical | 960 legs | ≈ 1300-1400 legs |

Net: 35-50 % more legs. Wall-clock impact at 4 workers is roughly proportional: 2-4 min → 3-6 min. Crosses the operator-attention threshold but stays well under the per-RPC ceiling.

**Mitigation knobs available without architecture change:**

- **Adaptive strike spacing inside the window.** For 46-180 DTE expiries, keep every listed strike inside ±10 % but skip every other strike outside ±10 %. Reduces wing-cost by half. Requires a per-expiry stride parameter; small change to `filterStrikesAroundSpot`.
- **WorkerCount bump.** From 4 to 6. Comment at [gamma_zero_compute.go:30](../../internal/daemon/gamma_zero_compute.go#L30) explicitly warns this needs slot-acquisition retuning. Out of scope for this design.
- **Drop expiries 5-6.** The 6-expiry default already over-shoots SpotGamma's 4-expiry default ([gamma_zero_compute.go:19](../../internal/daemon/gamma_zero_compute.go#L19)). Reducing to 4 for the wide-window mode would balance budget without losing the structural signal.

**Recommendation: ship adaptive-window-only first, measure, then add strike-stride if the operator-attention threshold matters.** Defer the worker bump and the expiry-count change.

## CLI surface

Today: `ibkr gamma [--json] [--no-wait] [--force]` ([cli/gamma.go:12](../../internal/cli/gamma.go#L12)). Default behavior is implicit `--strike-window=10`.

Proposed additions:

- **`--strike-window=N`** (integer percent, e.g. `10`, `20`). Forces a fixed window across all expirations. Default unset → adaptive rule applies. The current behavior is reproducible as `--strike-window=10`.
- **No separate `--fast` flag.** It would be a synonym for `--strike-window=10`; one knob is clearer.
- **`--expiries=N`** (out of scope for this design; mention only because the adaptive rule may push for reducing the default — defer).

Plumbing path:
- `rpc.GammaZeroSPXParams` gains `StrikeWindowPct float64 \`json:"strike_window_pct,omitempty"\`` (0 = adaptive).
- `handleGammaZeroSPX` ([gamma_handler.go:72](../../internal/daemon/gamma_handler.go#L72)) reads it instead of building empty params, populates `rpc.GammaZeroParams.StrikeWidthPct` for non-adaptive runs and a new `AdaptiveWindow bool` field for the adaptive case.
- `cli/gamma.go` adds the flag, populates the RPC param.

## Output mockup

Current (today, ±10 % everywhere):

```
SPY Dealer Zero-Gamma

  SPY spot    594.32  (15:42:11 EDT)
  γ-zero      587.45  (spot +1.17 %)
  γ-zero near 591.20  (spot +0.53 % · 142 legs · DTE ≤ 7)
  γ-zero term 580.10  (spot +2.45 % · 818 legs · DTE > 7)
  |Γ|·OI sum  3.412e+09 (sign-agnostic magnitude)
  Leg count   960 across 6 expirations
  Skew model  sticky-moneyness-v1  (6 expiries fit, median R² 0.76)
  Method      perfiliev-bs-sweep-v2-stickymoneyness
  Source      computed from IBKR SPY option chain
  Compute     3m 31s
```

Proposed (adaptive, default):

```
SPY Dealer Zero-Gamma

  SPY spot    594.32  (15:42:11 EDT)
  γ-zero      585.20  (spot +1.55 %)
  γ-zero near 591.20  (spot +0.53 % · 142 legs · DTE ≤ 7 · ±10 %)
  γ-zero term 578.40  (spot +2.75 % · 1182 legs · DTE > 7 · ±20-30 %)
  |Γ|·OI sum  4.103e+09 (sign-agnostic magnitude)
  Leg count   1324 across 6 expirations
  Scope       adaptive · ±10 % (0-7d) / ±20 % (8-45d) / ±30 % (46-180d)
  Skew model  sticky-moneyness-v1-weighted  (6 expiries fit, R² min/med/max 0.61/0.78/0.91)
  Method      perfiliev-bs-sweep-v3-adaptive
  Source      computed from IBKR SPY option chain
  Compute     4m 47s
```

Changes from today:
- Per-bucket window appears in the `near` / `term` lines.
- New `Scope` line documents the active rule.
- `Skew model` line shows `min/med/max R²` instead of just `median`.
- `Method` token bumps to `v3-adaptive`.

`--strike-window=10` reproduces the current output bit-for-bit (apart from the `Method` token if we choose to leave it stamped as the new version; alternative: keep v2 when window is fixed).

## Test plan

1. **Bit-for-bit reproduction guard.** `ibkr gamma --strike-window=10` must produce numerically-identical `γ-zero`, `Profile`, `TopStrikes` to a captured pre-change run. Added as a CLI integration test using a recorded chain fixture.
2. **Skew-weighting unit test.** Synthetic legs with known σ(m) curve + symmetric m range; verify weighted fit recovers the same A/B/C as unweighted (degenerate equivalence on symmetric inputs), then asymmetric m range with noise in the wings — verify ATM σ estimate is closer to truth under weighting.
3. **Cache-key parameterisation.** Two simulated concurrent compute requests with different `StrikeWindowPct` must produce two distinct jobs, both observable via the cache. Existing singleflight test extended.
4. **Job-builder rule application.** Pure function `pickStrikeWidthForDTE(dte float64) float64` table-driven test against all four bands plus boundary values (exactly 7 d, 45 d, 180 d).
5. **Slot-watermark surfacing.** End-to-end on a recorded fan-out: verify the watermark gauge is non-zero and bounded by `len(jobs)`.
6. **Live smoke.** `make smoke` must continue to pass; the adaptive run will land more legs but the existing `MinLegCoverageFraction = 0.2` gate is unaffected.

## Implementation sequencing — DO NOT START YET

Order, smallest-first, each gated on the previous landing cleanly. Listed for discussion only; no code lands without explicit user direction after this design + senior review.

1. Add `--strike-window=N` flag plumbed through CLI → RPC → handler → compute. Behavior is unchanged (always uses the explicit value or default 10). Backwards-compat regression test passes.
2. Add `AdaptiveWindow` field on `GammaZeroParams`. Job-builder branches on `params.AdaptiveWindow` to call `pickStrikeWidthForDTE(dte)` instead of the single param value. Behind a daemon-level env flag (`IBKR_GAMMA_ADAPTIVE=1`) for the first release; default off.
3. Weighted skew fit. Lands independently; user-visible change is improved median R² in `--strike-window=10` mode (sanity check that the weighting works on the existing window before we apply it on a wider one).
4. Min/median/max R² reporting. Pure rendering change.
5. Cache-key parameterisation. Required before flipping default to adaptive.
6. Flip default to adaptive. Method token bumps to v3.

## Senior-reviewer iteration (2026-05-21)

Two rounds with a senior-quant reviewer reshaped the design materially. Recording the binding outcomes here; the body of the doc above still describes the *original* proposal verbatim so the deltas are auditable.

### Finding 1 — `selectExpirations` is the actual fix (BLOCKING)

The 46-180 DTE band is unreachable today. `selectExpirations` ([gamma_zero_compute.go:1052](../../internal/daemon/gamma_zero_compute.go#L1052)) sorts candidate expirations lexicographically and slices the first `count` (default 6). On 2026-05-21 the 6 picks are:

```
2026-05-22 Fri  DTE= 1
2026-05-25 Mon  DTE= 4
2026-05-27 Wed  DTE= 6
2026-05-29 Fri  DTE= 8
2026-06-01 Mon  DTE=11
2026-06-03 Wed  DTE=13
```

Max DTE = 13. The next monthly OPEX (2026-06-19) at DTE = 29 never enters the basket. SPY's M/W/F weekly cadence fills 6 slots in ~2 weeks every trading day — not sensitive to the third-Friday monthly's position.

**Implication:** widening strikes without fixing the picker just spends more compute on the same shallow basket. The 8-45 d band would activate marginally (1-2 of the 6 picks); the 46-180 d band would never activate. The headline value proposition ("capture institutional quarterly hedges") does not materialise.

**Decision:** ship the picker change as its own preceding PR. Concrete shape: replace the lexicographic pick with a slot-based selector — e.g. *2 nearest weeklies, next monthly OPEX, next quarterly, plus 2 fill slots*. Slot definitions and exact count are deferred to the picker-PR's own design.

### Finding 2 — multi-cross hazard (v2, not v1)

Wider strike inputs can make `findZeroCrossing` ([gamma_zero_cache.go:444](../../internal/daemon/gamma_zero_cache.go#L444)) miss double zero-crossings (wing-put basin + far-call peak both push positive while ATM stays negative). Current behaviour is single-bracket-first; the existing comment at gamma_zero_cache.go:464 documents this as deliberate.

**Decision:** v1 adds a cheap `multi_cross` warning whenever >1 bracket is observed (still returns the first); v2 builds the actual detector once we have empirical data on which cross to prefer. Designing the detector without examples is speculative.

### Finding 3 — sweep ±15 % vs strike ±30 % mismatch

`sweepProfile` ([gamma_zero_compute.go:1168](../../internal/daemon/gamma_zero_compute.go#L1168)) evaluates legs at scenario spots in `[0.85·spot, 1.15·spot]`. A strike at 0.70·spot is moneyness ≈ -0.19 at the sweep's lower edge — outside any fitted skew curve's `[mLo, mHi]`, so `IVAtMoneyness` clamps and the leg's gamma is near-zero at every sweep point. The leg costs fan-out budget for no zero-crossing impact.

**Decision:** filter strike inputs to `sweep_range + skew_margin` (concretely: `±(SweepRangePct + 0.05)` = ±20 % on the inputs as long as the sweep stays ±15 %). If the design later widens the sweep to track wider tenors, that flows through automatically. The widening-the-sweep alternative is in scope but deferred — sweep-range change has its own consequences for the `Profile` rendering.

### Finding 4 — `MinLegCoverageFraction` semantics drift

0.2 was chosen against a ~960-leg pool concentrated near ATM ([gamma_zero_compute.go:104](../../internal/daemon/gamma_zero_compute.go#L104)). At 1300+ legs with the extra 35-50 % being wing strikes, the gate at 20 % can pass while landing strictly fewer ATM legs than today.

**Decision:** redefine the gate against ATM-band sub-coverage (e.g. "≥ 30 % of legs within ±5 % moneyness landed"). Lands as part of the picker-PR or the adaptive-PR, whichever comes second; not a standalone change.

### Finding 5 — skew weighting is premature

The only empirical datum is `median R² = 0.76` on today's ±10 % window. Mechanically a parabola has *more* freedom to fit a smile-skew with wider data; R² often improves rather than degrades. Weighting unconditionally couples two changes (weighting + width) whose effects cannot be separated.

**Decision:** drop weighted-LS from v1. Ship unweighted fit + `min/med/max R²` reporting. Revisit weighting only if `min R²` drops below ~0.6 on any expiry across a week of live data.

### Finding 6 — cache-key parameterisation is phantom

The handler ([gamma_handler.go:72](../../internal/daemon/gamma_handler.go#L72)) hard-codes `rpc.GammaZeroParams{}` and ignores request params. The "two callers, different windows, same session" race is not reachable until `--strike-window=N` is plumbed AND keyed AND defaults vary.

**Decision:** `--strike-window=N` is permitted only in combination with `--force` (bypass cache, do not write back). All non-force calls use the daemon's single configured window value. Zero key changes needed. If future telemetry shows users routinely want non-default windows, plumb the key then.

### Bit-for-bit reproduction test, qualified

Adaptive-width + new method token + new R² rendering = the envelope changes shape. Carve-out: the bit-for-bit test asserts equivalence on `ZeroGamma`, `Profile`, `TopStrikes`, `GammaTotalAbs`, `LegCount` only — NOT the full envelope, NOT `Method`, NOT `SkewFitQuality`.

### Stop-loss for live smoke

If the adaptive default lands more than 1600 legs OR exceeds 6 min wall-clock on `make smoke`, revert and re-tune. Goes in the test plan as a hard threshold, not "we'll measure."

### Skip-every-other-strike: rejected

The compute-budget mitigation proposed sub-sampling far-OTM strikes. This silently changes the OI sum's interpretation — OI density was the signal, not the strike count. Either every listed strike or none.

### Slot-watermark in envelope: log-only

Slot leak fix is already load-bearing in `connector.go:1628` and the per-leg defer pattern in `productionLegFetcher` is correct. A watermark in the JSON envelope is observability theatre; surface in daemon logs only.

---

## Revised implementation roadmap

PRs land in order; each is independent and can ship/release on its own.

1. **Picker PR — `selectExpirations` slot policy.** Guarantee 1 monthly + 1 quarterly in the basket. No strike-width changes. Verify on live smoke that the basket reaches into 30-90 DTE territory. **This is the actual fix** for the under-coverage complaint; everything downstream is enrichment.
2. **Skew reporting PR — `min/med/max R²`.** Render-only change; no compute path touched. Lands the per-expiry visibility we need before changing the fit's inputs.
3. **Strike-input filter PR — match sweep range.** Add `sweep_range + 0.05` cap on strike enumeration. Trims dead-weight wing legs from any window setting. Should be a small win on the existing ±10 % too.
4. **Multi-cross warning PR.** Count brackets in `findZeroCrossing`; emit `multi_cross` warning when > 1. No behaviour change. Sets us up for a v2 detector with real data.
5. **`MinLegCoverageFraction` redefinition PR.** Switch from total-coverage to ATM-band sub-coverage. Lands once the picker change is in, since the new policy stresses ATM coverage differently.
6. **Adaptive-window PR.** ONLY after the above. Adds the DTE-keyed window rule, behind `IBKR_GAMMA_ADAPTIVE=1` env flag, default off. Includes the bit-for-bit reproduction guard and the leg-count stop-loss.
7. **`--strike-window=N` CLI flag PR.** Force-only (refused without `--force`). Diagnostics affordance.
8. **Default flip PR.** Adaptive on by default, method token bumps to `v3-adaptive`. Lands after one release cycle of #6 behind the env flag.

## Locked decisions (user interview, 2026-05-22)

1. **Scope:** PR #1 (picker fix) only. Everything from PR #2 onward is deferred — to be re-evaluated after PR #1 lands and we have live measurements of the new basket's behaviour.
2. **Picker shape:** 6-slot basket: `[front-week-1, front-week-2, EOW, next-monthly, next-quarterly, fill]`, where:
   - **front-week-1 / front-week-2:** the two nearest non-expired listed dates.
   - **EOW:** the next Friday (if not already a front-week slot — otherwise the slot becomes a second fill).
   - **next-monthly:** the next 3rd-Friday expiration after the EOW slot.
   - **next-quarterly:** the next 3rd-Friday of Mar / Jun / Sep / Dec after the monthly slot.
   - **fill:** the nearest unused candidate date, breaks ties toward shorter DTE.
   - Dedup is mandatory — if a chosen anchor (e.g. EOW) collides with a front-week pick, the slot rolls to the next candidate of its category.
3. **No CLI flag.** `--strike-window=N` is not added. Daemon-config governs the strike window; operators experiment via daemon env var if needed.
4. **Adaptive default flip (if/when we reach PR #6):** one release behind an env flag, then flip. Recorded for posterity; not in current scope.

## PR #1 — what landed

Replaced lexicographic-N picker with a slot-anchored picker in [gamma_zero_compute.go](../../internal/daemon/gamma_zero_compute.go#L1123). The slot order is fixed in the function body; anchored slots (EOW / monthly / quarterly) silently roll to the fill rule when no eligible candidate exists.

**Pure helpers added** (testable without a live gateway):

- `pickExpirationSlots(candidates, nyNow, count)` — the slot policy.
- `thisWeekFriday(nyNow)` — EOW anchor.
- `isThirdFridayDate(yyyy_mm_dd)` — monthly predicate.
- `isQuarterlyThirdFridayDate(yyyy_mm_dd)` — quarterly predicate (Mar/Jun/Sep/Dec).

**Tests added** in [gamma_zero_compute_test.go](../../internal/daemon/gamma_zero_compute_test.go):

- `TestPickExpirationSlots` — 9 sub-tests covering today (Fri 2026-05-22), Wed/Tue weekdays, monthly-collides-with-front-week, missing-quarterly fall-through, count=2 / count=4 / count=0, empty candidates.
- `TestThisWeekFriday` — 7 sub-tests across the weekday spectrum.
- `TestIsThirdFridayDate`, `TestIsQuarterlyThirdFridayDate` — predicate table tests.
- Existing `TestSelectExpirations` sub-tests updated: the monthly+quarterly anchors now land in baskets where the legacy lexicographic picker would have stopped at weeklies, so the expected "want" arrays changed accordingly. Filter behaviour (yesterday-excluded, today-post-cutoff-excluded, SPX vs SPXW settlement) is unchanged and the trading-class sub-tests still validate it.

**Out of scope (deferred):** strike-width changes, skew weighting, R² rendering, cache-key parameterisation, `--strike-window` CLI flag, multi-cross detection, ATM-band coverage gate.

## Residual question — empirical, not blocking

The new basket (4 weeklies + 1 monthly + 1 quarterly typically) stresses `MinLegCoverageFraction = 0.2` differently than the old all-weeklies basket. Monthly/quarterly chains are thinner outside ATM ($5 spacing vs $1) and have OI concentrated at round-number ATM strikes. Live smoke run is the deciding artifact:

- If `make smoke` passes and `ibkr gamma` produces `LegCount` × `MinLegCoverageFraction` ≥ landed legs, the gate is happy.
- If `gateway throttled` / `low leg coverage` errors appear after PR #1 lands, the gate likely needs to switch to ATM-band sub-coverage (reviewer finding #4).
