# SPY dealer zero-gamma — pre-market compute slow-path analysis

**Date:** 2026-05-20 09:40 CEST
**Trigger:** live observation 08:15 ET — ~960 legs × 4 workers × ~5 s/leg ≈ 20 min wall-clock pre-market.
**Scope:** analysis only. No code changes proposed.

## TL;DR — highest-leverage changes

The 20-minute pre-market wall-clock is almost entirely `pollUntil` budget burn: every leg waits the full 5-second deadline for a model-computation tick the gateway will never send, then falls through to the BS-IV solve in microseconds. Two changes, in combination, plausibly take pre-market from 20 min → 3-5 min:

1. **Session-phase detection up front + shrunk per-leg budget (~10× on the slow path).** `snapshotUnderlyingForGamma` already returns `dataType`. When it comes back `"frozen"`, the gateway's model engine is off; the IV poll is pure waste. Skip Stage 2 entirely and shrink the OI gate to ~500-750 ms. Net per-leg budget drops from 5 s to ~0.5 s.
2. **Worker count bumped to ~12 under the fallback path (~3× additional).** The 4-worker cap is tuned for `reqContractDetails` throttle and concurrent model-tick load. Neither applies pre-market on a warm `optionContractCache` (which is now persistent across daemon restarts as of 2026-05-19's contract-cache commit `2fbd614`). The 100-slot rate limiter has 25× headroom over what 12 workers would consume.

Items 4-6 (Newton convergence, CPU micro-opt, BS-IV math swap) are dominated by the wall-clock waste in items 1-3 and aren't worth touching first.

---

## 1. Session-phase detection + budget tuning — **rank #1**

**Current.** [internal/daemon/gamma_zero_compute.go:278-304](internal/daemon/gamma_zero_compute.go:278) — both `pollMarketData(... d.OpenInt > 0)` and the model-tick `pollUntil(... iv > 0)` share **one** 5-second deadline (`deadline := time.Now().Add(5 * time.Second)` at line 279, both polls reference it). In pre-market, OI lands within a few hundred ms; IV never lands; the worker burns the residual ~4.5 s before falling through to Stage 2b's BS-IV solve. Per-leg wall-clock ≈ 5 s × 960 legs / 4 workers ≈ 1200 s.

**Signal already available.** `snapshotUnderlyingForGamma` ([gamma_zero_compute.go:728](internal/daemon/gamma_zero_compute.go:728)) returns `dataType` from `briefSnapshotFull` ([handlers.go:1950](internal/daemon/handlers.go:1950)). Values are `"live" | "frozen" | "delayed" | "delayed-frozen" | ""` (via `marketDataTypeName` at [handlers.go:1206](internal/daemon/handlers.go:1206)). Pre-market and weekends return `"frozen"` under the daemon's default `MarketDataType=2`. The compute already passes through `isAcceptableDataType` ([gamma_zero_compute.go:790](internal/daemon/gamma_zero_compute.go:790)), so the signal is at the right ABI point — it just isn't used to branch the fetcher.

**Proposed.** Capture `dataType` at line 438 in `computeGammaZeroSPX` and thread a `fallbackOnly bool` (or similar) through to the fetcher. When set:
- Drop the IV poll (Stage 2) entirely.
- Shrink the OI deadline to ~500 ms (tunable; see #6).
- Read `optPrevClose` directly and BS-solve.

**Expected speedup.** 5 s → 0.5 s per leg ≈ **10×** on the slow path alone. Pre-market 1200 s → ~120 s.

**Risk.** Two false-negative cases to think about:
- A `dataType == "live"` reading for SPY at, say, 04:15 ET (SPY extended hours start) where OPRA model ticks are still cold. The dataType branches on the *underlying* tick, not the option model engine, so they can diverge. Mitigation: keep Stage 2 with a *shorter* deadline (1-2 s) even on the "live" path, OR gate fallback-only on `dataType == "frozen"` alone (conservative — accept that the first 30 minutes of pre-market still runs the slow path).
- RTH session that throws `"frozen"` because the feed pauses mid-day (rare; usually a gateway reconnect): the compute degrades to BS-IV from prior close, which is the documented v0.26.0 fallback behaviour, so semantics are unchanged. Result envelope's `derivedCount` warning already discloses this.

**Recommendation.** Highest priority. The lever is the cleanest one in the codebase — no new gateway calls, no new dependencies, just a flag.

---

## 2. Bulk-fetch prior closes via FetchHistoricalDailyBars — **rank skip**

**Current.** Tick 9 (option-contract prev close) lands on subscribe automatically — see [connector.go:2168-2173](pkg/ibkr/connector.go:2168). It's already populated by the time Stage 2b reads it via `GetOptionPrevClose`. No additional fetch is happening.

**Proposed (per the brief).** Pre-fetch via `FetchHistoricalDailyBars` for all option contracts in one pass.

**Why it doesn't help.**
- `FetchHistoricalDailyBars` ([connector.go:2928](pkg/ibkr/connector.go:2928)) takes a `symbol string` and runs through `classifySymbol` — it's wired for STK/IND/CASH, not OPT contracts. Adapting it would mean a parallel HMDS path with per-contract `Contract` resolution.
- HMDS has its own concurrent-request cap (the rate-limiter's `historicalConcurrent` semaphore at [ratelimiter.go:394](pkg/ibkr/ratelimiter.go:394)) — fewer concurrent slots than streaming market-data.
- Tick 9 on the streaming subscription already arrives in hundreds of ms, free of charge. The wait isn't on the close — it's on the model tick that follows.

**Expected speedup.** Zero. We already have the prev close.

**Recommendation.** Skip. The brief's hypothesis ("eliminate the per-leg wait") is correct in shape but the wait isn't on prev close — it's on the IV model tick. Item #1 attacks that directly.

---

## 3. Worker count tuning under fallback path — **rank #2 (combine with #1)**

**Current.** `defaultWorkerCount = 4` ([gamma_zero_compute.go:31](internal/daemon/gamma_zero_compute.go:31)). Rationale in the file comment: matches the `reqContractDetails`-safe throttle for the chain fan-out (handlers.go:1628 area). Throttle defence is implemented via `throttleDetected` ([gamma_zero_compute.go:759](internal/daemon/gamma_zero_compute.go:759)) — 5 % `"contract details unavailable"` rate over a 50-leg sample triggers an abort.

**What gates parallelism actually.**
- Gateway's `marketDataSubs` cap is 100 ([ratelimiter.go:403](pkg/ibkr/ratelimiter.go:403)). With 4 workers we use 4 slots concurrently — 25× headroom.
- `reqContractDetails` throttle is the documented historical reason for 4 workers. But: as of commit `2fbd614` ("persist contract cache across daemon restarts"), `optionContractCache` ([connection.go:308](pkg/ibkr/connection.go:308)) survives restart. After a single warmed-cache day, `resolveOptionContract` ([connection.go:4098](pkg/ibkr/connection.go:4098)) returns from cache without a network call. The throttle concern collapses on warm cache.
- Stage 2b (the dominant path pre-market) doesn't pressure `reqContractDetails` at all — it pressures streaming-subscribe slot churn, which the 100-slot pool absorbs.

**Proposed.** When the fallback-only branch from #1 is active AND `optionContractCache` is hot (or simply: always, on the fallback path — let the throttle watchdog catch any surprise), raise workers to 8 or 12. Concretely: pass `params.WorkerCount = 12` from the normalizer when `fallbackOnly && cacheHot`.

**Expected speedup.** Linear in workers up to the gateway's per-second subscribe rate cap. 4 → 12 ≈ **3×**, modulated by whatever per-second cap the gateway enforces (the rate limiter doesn't currently track subscribes/sec, so this is the empirical question — but the rate-limit error counter in `recordRateLimitError` would surface it).

**Risk.** Two:
- Cold contract cache (first run after a restart, or after a release that didn't ship the cache file). The throttle watchdog at line 759 already catches this, but a 12-worker fan-out hitting throttle wastes more work than a 4-worker one. Mitigation: probe with `runtime`-scoped detection — start at 4, scale up if first 100 legs see zero throttle samples.
- Concurrent regime/chain calls competing for the same 100-slot pool. Pre-market the user typically isn't running chain fetches in parallel, but worth verifying via `marketDataSlots` instrumentation.

**Recommendation.** Combine with #1. After the 10× from #1 lands pre-market at ~120 s, this gets it to ~40 s — comfortably under the 5-minute target with margin.

---

## 4. Newton-Raphson convergence speed (Stefanica-Radoicic) — **rank skip**

**Current.** `bsImpliedVolatility` ([blackscholes.go:165](internal/daemon/blackscholes.go:165)) uses Brenner-Subrahmanyam initial guess with clamps ([blackscholes.go:225-230](internal/daemon/blackscholes.go:225)), 50 iters, 1e-5 tolerance. Empirically converges in 5-15 iterations for ATM-band strikes (the only strikes that survive `filterStrikesAroundSpot`'s ±10 % cut).

**Proposed (per the brief).** Replace with the Stefanica-Radoicic 2017 closed-form approximation (~10 ppm accuracy in a single algebraic eval).

**Quantification.** Per-leg solve cost: 5-15 Newton iterations × ~3 BS evaluations per iter (price + vega + arithmetic) × ~100 ns per `math.Exp` / `math.Erfc` ≈ **5-15 µs per leg**. Across 960 legs, the entire BS-IV phase costs **5-15 ms**. Even a 100% saving is invisible against the 1200-s wall-clock burn from item #1.

**Recommendation.** Skip until items #1 and #3 land. Revisit only if a future profile shows the BS phase rising above ~5 % of compute time, which would require items #1 and #3 to already have collapsed the wait-budget waste.

---

## 5. CPU-side micro-optimizations — **rank skip**

**Current.** Sweep ([gamma_zero_compute.go:915](internal/daemon/gamma_zero_compute.go:915)) is 60 spots × N legs × `bsGamma` (~100 ns each). At N=900: 5.4 ms total. The entire sweep is invisible against 1200-s budget burn.

**Proposed knobs from the brief:** vectorize `bsGamma` across the sweep, pre-compute `log(K)` and `√t`, lookup table for `normCDF` hot range.

**Quantification.** Sweep is 5.4 ms. `normCDF` isn't even on the sweep path (`bsGamma` uses `math.Exp` on the pdf, not `normCDF`). Pre-computing `log(K)` and `√t` per leg saves ~30 % of `bsGamma` cost → 1.6 ms saved across the sweep. Invisible.

**Recommendation.** Skip. Same logic as #4.

---

## 6. Skip-the-OI-gate optimization — **rank #3, modest**

**Current.** The OI poll ([gamma_zero_compute.go:280](internal/daemon/gamma_zero_compute.go:280)) waits up to 5 s for `d.OpenInt > 0`. OI ticks (27/28) come from OCC end-of-day data, not session activity — they should arrive within a few hundred ms post-subscribe regardless of session phase.

**Question raised by the brief.** Is the 5-s OI deadline fat?

**Analysis.** Two cases:
- Healthy gateway with warm contract cache: OI arrives within ~100-300 ms. A 750 ms deadline is generous; 5 s is fat.
- Cold-start gateway or unwarmed option contract: OI may still take longer because the underlying `reqContractDetails` is in flight before the subscribe completes. The 5 s here also covers the resolve round-trip.

**Proposed.** As part of item #1's fallback branch, drop the OI deadline to 750 ms when `dataType == "frozen"`. This is half of item #1's "shrunk budget" already — the components are coupled.

**Expected speedup.** Already counted in #1's 10×. Independently, on the slow path: shaves ~4 s off the worst-case leg where OI arrives normally but model tick doesn't. With item #1 in place, the marginal saving here is small.

**Risk.** Legitimately-slow OI on cold cache → false drop. Mitigation: per-job context still bounded by 5 s on the cold-cache first 50 legs (the throttle-watchdog window) — only after the watchdog signals cache-hot do we switch to 750 ms. This is per-job state plumbing; modest implementation cost.

**Recommendation.** Bundle with item #1 — it's the same code path. Don't ship as a standalone optimization.

---

## Ranking summary

| # | Change | Standalone speedup | Combined with #1 | Implementation cost |
|---|---|---|---|---|
| 1 | Session-phase detection + budget tuning | ~10× pre-market | — | low (one flag, one branch) |
| 3 | Workers 4→12 on fallback path | ~3× | additive (3×10 = 30×) | low (param threading) |
| 6 | Shrunk OI deadline | small (subsumed by #1) | subsumed | low (couples to #1) |
| 2 | Bulk prev-close fetch | 0× — wait isn't on prev close | — | high, no value |
| 4 | Closed-form IV approximation | invisible (saves ~10 ms / 1200 s) | invisible | medium |
| 5 | CPU sweep micro-opt | invisible (saves ~1 ms / 1200 s) | invisible | medium |

**Concrete target.** Land #1 and #3 together. Expected pre-market wall-clock: 20 min → 1-2 min on a warm contract cache; 20 min → 4-5 min on a cold cache (where the first 100-200 legs run the conservative 4-worker / 5-s budget before the watchdog upgrades). Both stay well under the 5-minute "feels-broken" threshold the user identified.

**What this leaves on the table.** The compute is fundamentally a "fetch 960 things from a 100-slot pool" workload. Past item #3 the only further leverage is reducing leg count (tighter `defaultStrikeWidthPct` than ±10 %, or fewer expirations than 6). That's a methodology change, not a performance change, and is out of scope for this analysis.
