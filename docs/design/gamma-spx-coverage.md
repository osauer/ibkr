# Design: SPX coverage for `ibkr gamma`

- Status: **draft v2, post-senior-review — do not implement yet, interview pending**
- Author: Claude, on behalf of @osauer
- Created: 2026-05-21 16:24 CEST
- Revised: 2026-05-21 16:50 CEST (round-1 senior-reviewer agent critique applied)
- Reviewers: senior-reviewer agent (round 1, done), user (interview after this draft)
- Code touchpoints anchored on commit `6651e5b` (v0.30.1)

### v2.1 change log (post-user-question, 2026-05-21 17:35 CEST)

User asked to verify contract-ID persistence and rolling daily refresh
parity with SPY. Verification found one new ship-blocker the design
didn't cover:

- **§4.4 added.** `optionContractKey` (`pkg/ibkr/connection.go:3791`)
  omits trading class. SPX vs SPXW on a third-Friday at the same strike
  collide in `optionContractCache` and in the persisted
  `contracts.json`. New §4.4 plumbs trading class through the cache key
  and bumps `contractStoreVersion` v2 → v3 with a backwards-read
  migration.
- Step 2 of the implementation order absorbs the cache-key fix — both
  schema changes (`FetchOptionExpiryStrikesClassed` + `optionContractKey`
  v3) land in the same commit since they share the SPX/SPXW identity
  hazard.
- Persistence layer otherwise verified working as designed: SPX
  underlying contract is already in
  `/Users/osauer/.cache/ibkr/contracts.json` (other code paths resolved
  it); the 60s save loop + NY-date GC at `contract_store.go:138`
  inherits cleanly for SPX OPT entries once the new compute populates
  them. No change needed in §5.

### v2 change log

Round-1 senior reviewer flagged four ship-blockers and several scope-creep
items. Applied:

- **Cut speculative tuning.** Single 5-min soft-TTL across all scopes (was a
  15-min split). Cache-key 2-tuple removed; `--only=*` routes through
  `force()`. Per-phase progress envelope dropped (reuses existing 0–100
  atomic).
- **Section 4 expanded.** Enumerated **every** hardcoded ET cutoff site
  with file:line. Documented the `FetchOptionExpiryStrikes` schema change
  — the wire actually carries `tradingClass` at `fields[4]` of
  `handleSecDefOptParam` (per `pkg/ibkr/connector_expiries.go:168`) but the
  current code explicitly drops it. SPX third-Friday has **two listed
  contracts** (SPX AM + SPXW PM) on the same date, which the
  `map[string][]float64` shape cannot represent.
- **Section 5.3 hardened.** Added the SPY/SPX rolling-correlation gate. If
  20-day correlation < 0.90, render per-index headlines as primary and
  badge combined as "decoupled."
- **Section 7 hardened.** Added the underlying-hold transition checklist
  item — `subs.Hold(SPY)` release MUST fire before `subs.Hold(SPX)` acquire,
  and SPX-phase errors must not leak SPY's hold.
- **Section 8 hardened.** Added partial-trading-class entitlement state
  (SPX class lands, SPXW returns 354, or vice versa).
- **Section 12 rewritten.** The original "grow existing fields in place"
  claim was wrong — `SpotUnderlying`, `ZeroGamma`, `Profile`, etc. are
  scalar / single-shape. Combined view requires a new `PerIndex` map and a
  `Scope` discriminator on the result. Top-level scalars now hold the
  **headline** (combined when both indices reachable, SPY-only when SPX
  is unavailable); this is a documented breaking semantic change for
  `--json` consumers that read top-level fields.
- **Per-underlying `MinLegCoverageFraction`.** SPX off-hours plausibly
  lands 5–15 % via the BS-IV fallback; the 0.2 SPY threshold would always
  abort. Per-underlying threshold added.
- **Interview question added** on the SPX coverage threshold (refuse below
  0.2, or accept 0.05-and-badged).

## 1. Background

`ibkr gamma` currently runs the dealer zero-gamma compute on **SPY only**. The
existing docstring on `computeGammaZeroSPX` is explicit about the trade-off:
SPY was chosen because it has continuous extended-hours quoting on SMART/ARCA
and a single trading class, so the compute stays robust off-hours. SPX (the
index) has no spot trading outside RTH, so off-hours runs land ~0 % usable legs.

A trader reviewer flagged the obvious cost: SPY is retail + ETF-arb dominated;
SPX is where institutional hedge books sit. SPY-only **misses 50–60 % of total
dealer-gamma notional** and the headline regime call can disagree with vendor
dashboards (SpotGamma, SqueezeMetrics) for scope reasons alone.

The goal of this design is to extend coverage to SPX while preserving the
robust-off-hours behaviour that made the SPY choice correct in the first place.

## 2. Goals and non-goals

### Goals

1. `ibkr gamma` (no flag) produces a combined SPY + SPX view by default when
   both underlyings are reachable and entitled.
2. `ibkr gamma --only=spy` reproduces **today's behaviour bit-for-bit** —
   regression target.
3. `ibkr gamma --only=spx` runs SPX in isolation.
4. Graceful fallback: if SPX is unreachable (no entitlement, gateway error),
   render SPY-only with a clear banner and exit 0 — SPY-only users must not
   be regressed.
5. Cache keys correctly partition SPY-only, SPX-only, and combined runs so
   stale cross-contamination is impossible.
6. The slot-leak class of bug (project memory `project_opt_subscription_slot_leak`)
   is audited before merging — doubling the fan-out without checking
   subscribe/observe/unsubscribe is exactly how that bug came back the first
   time.

### Non-goals

1. NDX, RUT, VIX — out of scope. The CLI flag surface (`--only`) is shaped to
   allow them later, but no implementation work for them in this change.
2. A new methodology. Same Perfiliev sign convention, same sticky-moneyness
   skew fit, same ±10 % strike width, same ±15 % sweep. We are widening
   coverage, not changing the model.
3. A live-tick streaming mode. The compute remains a session-cached batch
   job; only the cache key and underlying set change.
4. Tuning the per-leg or per-fan-out throttles below their current SPY values.
   Worker count stays at 4 (see §6 for why).

## 3. Methodology recap (so the SPX-specific notes have context)

Anchored on `internal/daemon/gamma_zero_compute.go` and
`internal/daemon/blackscholes.go`:

1. Snapshot underlying spot (refuse on stale, refuse on missing).
2. Enumerate listed expirations via `FetchOptionExpiryStrikes` and pick the
   nearest N non-0DTE-post-settlement.
3. Per expiry, filter strikes to ±10 % of spot, ATM-outward.
4. Bulk prewarm option contracts per expiry via `PrewarmOptionChain` with the
   underlying's trading class (load-bearing — see §4.2).
5. Filter jobs to only those whose `(symbol, expiry, strike, right)` is
   actually cached after prewarm.
6. Fan out across 4 workers. Each worker subscribes one OPT leg, polls for the
   gateway's model-computation tick, falls back to Black-Scholes IV from the
   leg's bid/ask mid or prior-session close.
7. Aggregate dealer GEX at snapshot spot:
   `GEX = Σ sign(right) × Γ_leg × OI_leg × 100 × spot² × 0.01`
   under the Perfiliev convention (dealers long calls, short puts).
8. Build sweep profile across [0.85, 1.15] × spot in 60 points; per scenario
   spot, recompute σ at the leg's strike/scenario-spot moneyness via the
   per-expiry quadratic skew curve.
9. Find zero crossing via linear interpolation on the swept profile.
10. Surface the top N legs by |Γ × OI| at snapshot spot as the regime-robust
    positioning view.

The multiplier `100` in step 7 is **per-contract**, not per-share — same for
SPY and SPX. The dollar-gamma units come out correctly for both indices as
long as we plug in each index's own spot in `spot²`. This is **the** load-
bearing claim of the combined-aggregation rule in §5.3.

## 4. Contract enumeration for SPX

### 4.1 Symbol → contract mapping

The infrastructure already exists. From the codebase map:

| Helper | Path | Behaviour for SPX |
|---|---|---|
| `classifySymbol` | `pkg/ibkr/symbols.go:94` | Returns `("IND", "CBOE", "USD", "CBOE")` |
| `contractDisplayHints` | `pkg/ibkr/symbols.go:178` | Returns `("SPX", "SPX")` for LocalSymbol & TradingClass |
| `prepareContract` | `pkg/ibkr/connector.go:826` | Already handles IND uniformly |
| `secDefOptParams` (chain enum) | `pkg/ibkr/connector_expiries.go:75` | Uses `classifySymbol`; works for IND |

No new contract-construction code is needed for the underlying. The plumbing is
generic.

### 4.2 SPX vs SPXW trading classes

This is the load-bearing detail. SPX options have **two trading classes**:

- **`SPX`** — standard monthly options, **AM-settled**. Settlement = third
  Friday 09:30 ET (open). Cash-settled against the SET (Special Opening
  Quotation) of the index.
- **`SPXW`** — weeklies + dailies + EOM, **PM-settled**. Settlement = 16:00 ET
  (close). Includes the 0/1/2/4 DTE flow that's the dominant volume segment.

The two classes have separate strike grids and separate IV surfaces. They are
quoted from different order books even when their expiries collide (a Friday
with both a SPX monthly and an SPXW Friday weekly — the September quarterly).

`PrewarmOptionChain(ctx, sym, expirations, tradingClass, timeout)` takes a
**single** trading-class argument. The existing comment at
`gamma_zero_compute.go:557-561` explicitly calls this out for SPY:

> TradingClass="SPY" is load-bearing: omitting it interleaves SPYW
> weeklies (different trading class) and cache entries can shadow each
> other; … For SPX the equivalent would be running this twice with
> "SPX" and "SPXW" trading classes.

**Decision:** For SPX, call `PrewarmOptionChain` twice — once with
`tradingClass="SPX"`, once with `"SPXW"`. The two cached supersets get merged
at the job-filter step. Each leg carries its trading class through to fan-out
so it can be priced under the correct settlement convention.

This needs a new field on `legData`/`legSpec`:

```go
type legSpec struct {
    expiryYMD     string
    strike        float64
    right         string
    tradingClass  string   // NEW: "SPX" | "SPXW" | "" (SPY default)
}
```

`SubscribeOption` already takes a trading class through its contract resolver;
we plumb the field through `productionLegFetcher`.

### 4.3 AM vs PM settlement → DTE and expiry filter

#### 4.3.1 Hardcoded-ET-cutoff audit

Every site where 16:00 / 16:15 ET appears in production daemon code:

| Site | Current value | SPX implication |
|---|---|---|
| `internal/daemon/gamma_zero_compute.go:1074` (`selectExpirations`) | 16:15 ET past-cutoff | Wrong for SPX-class AM expiries on 3rd-Friday |
| `internal/daemon/gamma_zero_compute.go:1148` (`dteYears`) | 16:00 ET as expiration instant | Wrong for SPX-class AM expiries (settle 09:30 ET) |
| `internal/daemon/expiry_iv_cache.go:80-82` (`expiryIVTTL`) | `[570, 960)` minutes window | This is the **IV-cache TTL** RTH window, not an expiry instant. No change — the SPX option chain still uses the same RTH IV-refresh cadence. |
| `internal/daemon/expiry_iv_cache_test.go:70` | 4 PM ET test | Test fixture for `expiryIVTTL`; no change. |
| `internal/rpc/rpc.go:104` (`IsOptionRTH`) | 09:30–16:00 ET RTH gate | Used for renderer dimming; no change (RTH is the same for SPY-class and SPXW-class; SPX-class is also during these hours). |

The **only two production lines that change** are
`gamma_zero_compute.go:1074` and `:1148`. The IV-cache window
(`expiry_iv_cache.go:80`) and the RTH gate (`rpc.go:104`) stay as-is.

#### 4.3.2 Per-trading-class settlement table

`selectExpirations` currently uses a 16:15 ET cutoff:

```go
settlementCutoff := time.Date(..., 16, 15, 0, 0, loc)
pastCutoff := nyNow.After(settlementCutoff)
```

For SPX-class (AM-settled), this is **wrong**: an SPX monthly expires at
**09:30 ET**, not 16:00. At 15:00 ET on a third-Friday expiry, the SPX
monthly is already cash-settled.

`dteYears` has the symmetric problem: it uses 16:00 ET as the expiration-
instant. For SPX-class monthlies the right instant is 09:30 ET.

**Decision:** trading-class-aware cutoffs:

| Trading class | Settlement style | `dteYears` instant | `selectExpirations` past-cutoff |
|---|---|---|---|
| `SPX` | AM (3rd-Friday monthly only) | day-of-expiry 09:30 ET | nyNow ≥ 09:30 ET on expiry day |
| `SPXW` | PM (weeklies, dailies, EOM, 3rd-Friday weekly) | day-of-expiry 16:00 ET | nyNow ≥ 16:00 ET on expiry day |
| `SPY` / default | PM | day-of-expiry 16:00 ET | nyNow ≥ 16:00 ET on expiry day |

Backwards-compat: when a leg has no trading class tag (SPY-only path), the
helpers fall back to the existing 16:00 ET behaviour bit-for-bit.

#### 4.3.3 `FetchOptionExpiryStrikes` schema change (ship-blocker)

Today: `Connector.FetchOptionExpiryStrikes(symbol) → map[string][]float64`
keyed by `"YYYY-MM-DD"`. The current implementation explicitly drops
`tradingClass` (`pkg/ibkr/connector_expiries.go:168`: *"fields[2] = exchange
(we keep it implicit — dedupe across all exchanges)"* — same logic discards
`fields[4] = tradingClass`).

For SPX this is **structurally wrong**: a third-Friday in SPX has both
an AM-settled SPX contract AND a PM-settled SPXW contract listed on the
same date. They are two distinct cash-settled options. A `map[string][]
float64` keyed by date alone **silently collapses them.**

**Decision: parallel-map approach** rather than changing the existing
function shape:

```go
// New helper alongside FetchOptionExpiryStrikes — keeps the existing
// return shape for SPY-style callers and the chain command that doesn't
// care about trading class.
type ExpiryClassedStrikes struct {
    // Strikes is the merged set across trading classes for this date.
    Strikes []float64
    // TradingClasses lists the class(es) that listed this expiry. For
    // most expiries len == 1; for SPX-class 3rd-Fridays len == 2
    // (["SPX", "SPXW"]).
    TradingClasses []string
}

// FetchOptionExpiryStrikesClassed is the SPX-aware variant. For each
// expiry date, returns the strikes AND the trading classes that listed
// that date. Implementation reads fields[4] (tradingClass) from each
// msg-75 frame in handleSecDefOptParam and stores it per (exchange,
// class, expiry) before deduping.
func (c *Connector) FetchOptionExpiryStrikesClassed(
    symbol string,
    timeout time.Duration,
) (map[string]ExpiryClassedStrikes, error)
```

`handleSecDefOptParam` (`pkg/ibkr/connector_expiries.go:172`) gets minor
surgery: keep `fields[4] = tradingClass` in the per-frame state instead of
discarding it. The dedupe at snapshot time becomes
`map[date]map[class]struct{Strikes}` flattened to the public return shape.

Existing `FetchOptionExpiryStrikes` callers (the chain command at
`internal/daemon/chain.go:232`) are unchanged — they keep getting the
flattened `map[string][]float64`. Only the gamma compute uses the new
classed helper.

Smoke target for the schema change: add a unit test mocking two msg-75
frames for SPX (one with tradingClass="SPX", one with "SPXW") on a
third-Friday date, assert the snapshot returns both classes for that date.

### 4.4 Spot snapshot for SPX

SPX is index-only — no bid/ask/last off-hours. `snapshotUnderlyingForGamma`
already falls through to MarkPrice (tick 37) and Close (tick 9), so the
mechanism works. The "stale data" check accepts `"live"` and `"frozen"` —
SPX off-hours will mark as `"frozen"`, which is correct and exactly what
the existing comment describes:

> For SPX this is typically yesterday's regular-session close.

So no change needed in the spot-snapshot helper. The renderer will use the
SPX-specific spot for the SPX sweep and the SPY-specific spot for the SPY
sweep; they are independent.

### 4.4 Option-cache key collision: SPX vs SPXW (ship-blocker)

Found post-interview while verifying user's question about contract-ID
persistence (`/Users/osauer/.cache/ibkr/contracts.json` inspection
2026-05-21 17:30 CEST).

**Current key format** (`pkg/ibkr/connection.go:3791`):

```go
func optionContractKey(symbol, expiry string, strike float64, right string) string {
    return strings.ToUpper(symbol) + "|" + expiry + "|" +
           strconv.FormatFloat(strike, 'f', 6, 64) + "|" +
           strings.ToUpper(right)
}
// SPY|20260521|500.000000|C
```

Trading class is NOT in the key. SPX and SPXW both list contracts on the
same third-Friday date with the same strikes — they are two distinct
contracts (different ConIDs, different settlement: SPX AM at 09:30, SPXW
PM at 16:00, different cash-settlement reference prices). With the
current key format both would map to e.g. `SPX|20260619|5400.000000|C`
and the second one written silently overwrites the first.

Symmetric to §4.3.3 (which fixes the expiry-list side by adding a classed
fetch helper), but on the contract-cache side. Both fixes must land in
the same arc.

**Decision:** include trading class in the cache key, with a
schema-version bump to `contracts.json`.

```go
// New key format: symbol|tradingClass|expiry|strike|right
// Empty tradingClass (SPY-only legacy path) renders as: SPY||20260521|500.000000|C
// Distinct from explicit SPY:                            SPY|SPY|20260521|500.000000|C
// Both round-trip cleanly; the empty-class shape preserves byte-for-byte
// backwards-compat at the optional-class read path.
func optionContractKey(symbol, tradingClass, expiry string, strike float64, right string) string {
    return strings.ToUpper(symbol) + "|" +
           strings.ToUpper(tradingClass) + "|" +
           expiry + "|" +
           strconv.FormatFloat(strike, 'f', 6, 64) + "|" +
           strings.ToUpper(right)
}
```

**Schema migration (`contractStoreVersion: v2 → v3`):**

- `LoadOptions()` (`pkg/ibkr/contract_store.go:119`) detects `f.Version
  == 2` (pre-trading-class) and migrates entries on read: every key
  `S|E|K|R` is normalised to `S||E|K|R` (empty class). Migrated entries
  remain readable; the old SPY-only callers continue to function
  unchanged because the connector's lookup path uses the same empty-class
  key when no class is supplied.
- `Save()` always writes v3 going forward.
- A future v4 (when we drop the v2-read migration) is out of scope here.

**Connector-side touchpoints** (audit checklist for implementation):

| Site | Today | Change |
|---|---|---|
| `optionContractKey` def | `connection.go:3791` | Add `tradingClass` argument; preserve empty-class shape |
| `IsOptionContractCached` API | `contract_store.go:352` | Add trading-class arg with empty default; SPY callers unchanged |
| Cache insert sites | `connection.go:2386, 4200`; cache emit at `contract_store.go:387` | Pass `contract.TradingClass` from the resolved contract |
| `SnapshotOptionContracts` | `contract_store.go:337` | No change — keys are written through unchanged |
| `SeedOptionContracts` | `contract_store.go:366` | No change — keys read through unchanged after v2-migration normalisation |

**Implementation order impact:** this lands as part of **step 2** (the
schema work) — alongside `FetchOptionExpiryStrikesClassed`. Both are
load-bearing for steps 6 and 7. The step-2 commit message must mention
both schema fixes.

**Test:** add a unit test mocking two SPX option-contract resolutions on
the same expiry+strike+right but different trading classes (SPX and
SPXW), assert both round-trip through cache without overwriting each
other. Pin the v2-read migration with a fixture loading a v2 file and
asserting normalisation to v3 empty-class shape.

### 4.5 Multiplier and notional

Both SPY and SPX use multiplier=100. The SPX index ≈ 10 × SPY, so
`spot² × 0.01` is ≈ 100× larger for SPX. **This is correct** — it reflects
that one SPX contract has 10× the dollar gamma of an SPY contract at the
same Greeks. The aggregator must NOT normalise — the raw dollar-GEX is the
honest quantity to combine.

## 5. Architecture

### 5.1 Where to parameterise

The compute today is `computeGammaZeroSPX(ctx, c, params, fetch, now, prog, log)`.
The name is a misnomer (it's SPY-side) but more importantly the symbol is
**hard-coded inside the function** at `const sym = "SPY"`.

**Decision:** Rename `computeGammaZeroSPX` → `computeGammaZeroFor(underlying)`
and lift `sym` to a function parameter. The renaming clears up the SPY/SPX
confusion in the docstring. Add the trading-class set as another parameter:

```go
// computeGammaZeroFor computes the zero-gamma profile for one underlying.
// tradingClasses is the set passed to PrewarmOptionChain; multi-class
// underlyings (SPX) pass {"SPX", "SPXW"}, single-class (SPY) passes {""}
// to take the gateway default.
func computeGammaZeroFor(
    ctx context.Context,
    c *ibkrlib.Connector,
    underlying string,
    tradingClasses []string,
    params rpc.GammaZeroParams,
    fetch legFetcher,
    now func() time.Time,
    progress *atomic.Int32,
    logger gammaLogger,
) (*rpc.GammaZeroComputed, error)
```

The handler decides which underlyings to run. The compute itself stays
single-underlying — one fan-out, one result envelope. The combined view is
formed at a higher layer (see §5.3).

### 5.2 Fan-out: serial across underlyings (v1)

Two options:

| Option | Wall-clock | Slot pressure | Complexity |
|---|---|---|---|
| **A. Serial** (SPY then SPX) | ~7 min (3:30 × 2) | Same as today | Low |
| **B. Parallel** (SPY‖SPX, share 4-worker pool) | ~5 min | Same as today | Medium |
| **C. Parallel + double workers** (4 SPY + 4 SPX) | ~3:30 | 8 concurrent OPT subs | High (slot retuning) |

**Decision: Option A for v1.** Reasoning:

- Worker count 4 is documented as load-bearing for the gateway throttle
  (`gamma_zero_compute.go:25-31`). The compute comment says: "Bumping it
  requires retuning AcquireMarketDataSlot and is a deliberate follow-up,
  not a v1 knob." Doubling concurrent OPT subscriptions is exactly that.
- Option B (parallel, shared pool) adds a shared job-queue layer across two
  underlyings — significant complexity for ~30 % wall-clock win.
- Serial keeps the existing per-leg fan-out unchanged; the only new code is
  the orchestration loop at the handler level. The slot-leak audit (§7.1)
  is correspondingly simpler.
- 7 minutes is past the user's stated UX threshold. The mitigations:
  - `--only=spy` for users who want the fast 3:30 path (alias of today).
  - SPX has a longer soft-TTL (see §5.5) — 15 min instead of 5 min — so
    intraday recalcs don't always re-pay the cost.
  - Per-index progress and ETA on the "computing" envelope, so the renderer
    can show "computing SPY (45 %) … then SPX" instead of one long bar.

If wall-clock becomes a recurring complaint we can move to Option B in v2
without breaking the wire shape.

### 5.3 Aggregation: combined headline

The combined sweep is **percent-of-spot relative to each index's own spot**,
then signed dollar-GEX is summed.

For each sweep index `i ∈ [0, 60)`:

- SPY scenario spot: `spot_spy × (1 - 0.15 + 0.30 × i / 59)`
- SPX scenario spot: `spot_spx × (1 - 0.15 + 0.30 × i / 59)`
- Combined GEX: `gex_spy[i] + gex_spx[i]` (both in dollars; raw, no normalisation)

This is the load-bearing rule the user called out in the brief.

The combined zero-gamma is the spot-percent at which the combined sweep crosses
zero. It is **not** a price level — it's a `%` move shared across both indices,
assuming they move together.

Per-index headlines stay as price levels:

- SPY γ-zero: 535.50 (spot +0.88 %)
- SPX γ-zero: 5380.00 (spot +0.93 %)

The combined γ-zero is rendered as: `combined γ-zero: spot −1.20 %`. The %
gap is the headline, with the regime sign label (long-γ / short-γ /
flipping).

#### 5.3.1 Decorrelation gate (post-round-1 addition)

The combined headline assumes SPY and SPX move together (≈ 0.98 daily
correlation in typical regimes). This breaks in:

- Futures-led tape with SPX-future basis dislocation.
- Midmorning ETF arbitrage gaps.
- Single-print SPX prints off open (SPX's print frequency is much lower
  than SPY's continuous tape).

In a decorrelated regime, a single combined-γ-zero "spot −1.20 %" is
hallucinatorily precise — it represents two divergent flips averaged.

**Decision:** compute SPY/SPX 20-session daily-close rolling correlation
at compute time using the existing `internal/daemon/breadth_fetcher.go`
`FetchHistoricalDailyBars` path (same primitive breadth uses; no new
gateway calls). If `corr < 0.90`:

- Surface a `decoupled` warning in the result envelope.
- Renderer **promotes the per-index headlines to the primary view** and
  badges the combined γ-zero as `combined γ-zero (decoupled): spot
  −1.20 % · use per-index above as primary`.
- The combined number is still computed and shown for reference — but the
  reader's eye is drawn to the per-index pair.

The 20-session window matches the breadth engine's existing daily-bar
fetch scope; no new contract subscriptions. `corr ≥ 0.90` keeps today's
implicit assumption explicit and tested.

Computational cost: 20 daily closes × 2 symbols = 40 bars. Sub-second on
warm cache; budgeted in §10.

### 5.4 Near / term split

The existing code partitions by DTE (≤ 7 d / > 7 d). The combined view does the
same partition per-index, then sums:

- `near` combined = `near_spy_gex[i] + near_spx_gex[i]` for each i
- `term` combined = `term_spy_gex[i] + term_spx_gex[i]` for each i

No new methodology — the DTE cutoff is the same 7 d for both indices, since
0DTE/end-of-week flow vs OPEX-monthly contrast is identical across the two.

### 5.5 Cache shape

**Decision (round-1 revision): keep the existing `nySessionKey`-only key.**

The earlier draft proposed a `(session, scope)` 2-tuple. The senior
reviewer pushed back: three coexisting scopes per day adds promotion /
discard logic for marginal benefit when the fan-out itself — not the
cache — is what's rate-limited.

Concrete shape:

- The canonical cache entry is **combined-when-reachable**: if SPX is
  entitled, the cached `current` holds the combined result with both
  `PerIndex` halves populated; if SPX is not reachable, the cached
  `current` is SPY-only with `Warnings: ["spx_skipped:354"]` and
  `Scope = "spy"`.
- `--only=spy` and `--only=spx` bypass cache via `force()` (today's
  documented diagnostics path). Their result is **not** stored — the
  next default call goes back through the canonical path.
- A new `Scope` string field on `rpc.GammaZeroComputed` lets the
  renderer disambiguate which mode produced the served value.

**Soft-TTL: single 5-min value across all scopes.** The earlier draft
asserted a 15-min TTL for SPX based on "slower-rebalancing books"; no
codebase measurement supports that. Defer until intraday SPX dealer
positioning has been measured.

### 5.6 Background-refresh interaction with serial fan-out

The soft-TTL refresh stays a single background goroutine. For the
combined canonical scope, that one job runs SPY then SPX serially. On
success the cached `current` promotes; on either underlying's failure
the prior value is retained, matching today's discard-on-error
semantics.

**Progress reporting stays at the existing 0–100 atomic.** The earlier
draft added per-phase progress (`computing SPY → SPX queued`); the senior
reviewer cut it as scope-creep. Serial fan-out trivially maps to the
existing counter: 0–50 SPY, 50–100 SPX. No envelope change needed.

## 6. CLI flag surface

```
ibkr gamma [--only=spy|spx] [--json] [--no-wait] [--force]
```

- **No flag (default):** combined SPY+SPX; falls back to SPY-only with a
  banner if SPX is unreachable.
- **`--only=spy`:** SPY-only, unchanged from today. Bit-for-bit regression
  target.
- **`--only=spx`:** SPX-only. Errors out (exit 1) if SPX is unreachable.

`--include=` is **not** added in v1 (premature for NDX/RUT/VIX).
`--fast` is **not** added (`--only=spy` is the same thing).

The shipped `ibkr gamma` MCP tool description (`internal/mcp/tools.go:248`)
gets a parallel update.

## 7. Slot-leak audit checklist

Per project memory `project_opt_subscription_slot_leak`: the bug was
`handleOptionComputation` not setting `sub.Observed`, so
`UnsubscribeMarketData` skipped `CancelMarketData` and leaked rate-limiter
slots. The fix lifted that `&& sub.Observed` guard.

Before merging, verify for **SPX-class options** specifically:

1. **Observed flag flips on tick arrival.** `pkg/ibkr/connector.go:2318`
   sets `sub.Observed = true` on the first tick. Confirm this is hit for an
   SPX option subscription by adding a one-off integration test:
   subscribe to one SPX leg, wait for first tick, assert Observed.
2. **`UnsubscribeMarketData` issues `CancelMarketData` for SPX.** Same code
   path as SPY; mechanical, but worth a unit-level assert that the cancel
   request was emitted on the wire (mock connector).
3. **Slot accounting balances.** After a 50-leg SPX fan-out, the rate
   limiter's market-data semaphore should return to the baseline acquired
   count (underlying hold + 0 leftover legs). Check via the existing
   `ibkr status` market-data-slots field, or a daemon-side diagnostic.
4. **Terminal-error paths don't double-release.** SPX is likely to throw 354
   ("not subscribed") if entitlement is missing. Confirm `releaseMarketDataSlot`
   is idempotent (it is, per `pkg/ibkr/connection.go:2076-2089`) and that
   both the connector's terminal-error handler AND the deferred
   `UnsubscribeMarketData` call don't both decrement the semaphore.
5. **Two-pass prewarm doesn't leak.** `PrewarmOptionChain` is called twice
   for SPX (once per trading class). The contract-details path doesn't hold
   market-data slots, but a sanity check on `c.GetActiveSubscriptionCount`
   before and after the two-pass prewarm protects against drift.
6. **Underlying-hold transition between SPY and SPX phases.** Today
   `gamma_handler.go:74-79` holds SPY for the whole compute. Serial-phase
   fan-out introduces a transition: SPY hold must release at SPY-phase
   end, SPX hold must acquire at SPX-phase start, and an SPX-phase error
   must NOT leak the SPY hold. Concrete checks:
   - Wrap each phase in its own `subs.Hold` / `defer release()` so the
     hold's lifetime is structurally tied to the phase function's stack
     frame — no manual release-on-error paths.
   - Add a daemon-side `subs.HeldSymbols()` accessor and assert it
     transitions `{SPY} → {} → {SPX} → {}` across a full combined run.
   - Drive an SPX-phase panic in a unit test and verify the SPY hold
     released cleanly.

The audit is **a precondition to merging**, not just a follow-up. The agent
implementing this will run all six checks and paste the results in the PR /
commit summary.

## 8. Entitlement-graceful degradation

### 8.1 Detection

SPX entitlement gap surfaces as one of:

- Code 354 on the SPX underlying subscription (most common — "Requested
  market data is not subscribed").
- Code 200 on SPX option contract resolution (rarer; "no security
  definition" when the OPRA chain is restricted).
- The fan-out lands zero usable legs within the early-abort window
  (`earlyAbortAfter = 30 s`).

### 8.2 Behaviour by scope

| Scope | Both SPX classes land | SPX skipped (no SPX legs at all) | **Partial: one SPX class lands, other 354s** |
|---|---|---|---|
| `--only=spy` | (n/a) | (n/a) — SPY-only path | (n/a) |
| `--only=spx` | run | exit 1 with banner: `SPX entitlement missing or unreachable: <reason>. Re-run with --only=spy or add CBOE OPRA subscription in IBKR.` | run with a partial banner: `SPX partial — SPXW entitled, SPX class returned 354. Combined SPX view uses SPXW only; AM-settled monthlies missing.` Exit 0. |
| default (combined) | run combined | SPY-only with a top-of-output banner: `SPX skipped — entitlement missing (354). Showing SPY only.` exit 0 | combined with a per-index sub-banner under the SPX row: `(partial — SPXW only)`. exit 0. |

#### 8.2.1 Partial-trading-class detection (post-round-1 addition)

The reviewer flagged that IBKR's CBOE OPRA entitlement can be granted
**per-trading-class**, not per-underlying. A run that lands SPXW legs but
0 SPX-class legs is a real intermediate state. Detection signal: after
the two-pass prewarm completes, count cached contracts per trading
class. If one class is non-empty AND the other is empty + threw 354 on
prewarm, mark as partial in the result envelope:

```go
// rpc.GammaZeroComputed addition
PartialClasses map[string]string  // e.g. {"SPX": "354"} when SPX-class 354s but SPXW lands
```

The renderer reads this map to decide the sub-banner; the JSON envelope
exposes it for programmatic consumers.

### 8.3 Smoke-test posture

`make smoke` already skips cleanly if no gateway is reachable. The SPX path
mirrors that for entitlement:

- If the default-scope smoke run sees SPX skipped with the banner, the smoke
  test passes (banner is expected on accounts without CBOE OPRA).
- If `--only=spx` returns exit 1 with the entitlement banner, the smoke run
  reports it as a skip, not a failure.

This requires a small addition to the smoke harness — a new
`SkipReason: "spx_entitlement_missing"` that maps to the exit-1 banner
content.

## 9. Output mockup

### 9.1 Default (combined, both reachable)

```
Dealer Zero-Gamma (SPY + SPX)

  SPY spot     540.20    (15:33:00 EDT)
  SPX spot     5430.10   (15:33:00 EDT)
  Computed     15:35:42 EDT · 2m 38s ago

  Combined γ-zero    spot −1.20 %    (regime: long-γ, stabilizing)
  Per-index:
    SPY γ-zero       535.50    (spot +0.88 %, long-γ)
      near           538.00    (spot +0.41 % · 320 legs · DTE ≤ 7)
      term           532.10    (spot +1.52 % · 380 legs · DTE > 7)
    SPX γ-zero       5380.00   (spot +0.93 %, long-γ)
      near           5410.00   (spot +0.37 % · 280 legs · DTE ≤ 7)
      term           5330.00   (spot +1.88 % · 340 legs · DTE > 7)

  |Γ|·OI sum    2.31e+10  (SPY 4.23e+09 · SPX 1.89e+10)
  Leg count    1380       (SPY 700 · SPX 680)
  Method       perfiliev-bs-sweep-v2-stickymoneyness
  Skew model   sticky-moneyness-v1  (12 expiries fit, median R² 0.91)
  Compute      4m 12s

  Top strikes by |Γ|·OI (regime-robust positioning signal):
    INDEX  EXPIRY       STRIKE   RIGHT       |GEX|         OI
    SPX    2026-06-20   5400     C           7.21e+09      18000
    SPX    2026-06-20   5300     P           5.13e+09      22000
    SPX    2026-05-30   5450     C           3.92e+09      14500
    SPY    2026-06-20   540      C           1.21e+09      82000
    ...

  Scope: SPX + SPY · ±10% strikes · 6 expirations per index

  Disclosure: the signed γ-zero assumes the 2018 "dealers long calls,
  short puts" convention. In regimes dominated by covered-call ETFs or
  autocall hedging the sign can invert; treat as a regime hint, not a
  level. The magnitude signal above is methodology-agnostic.
```

### 9.2 Combined with SPX skipped (entitlement gap)

```
Dealer Zero-Gamma (SPY + SPX)

  ⚠ SPX skipped — entitlement missing (354). Showing SPY only.
    Re-run with --only=spy to suppress this banner, or add CBOE OPRA
    subscription in IBKR.

  SPY spot     540.20    (15:33:00 EDT)
  Computed     15:35:42 EDT · 2m 38s ago

  γ-zero       535.00    (spot +0.97 %)
  γ-zero near  538.50    (spot +0.32 % · 320 legs · DTE ≤ 7)
  γ-zero term  532.00    (spot +1.54 % · 380 legs · DTE > 7)
  |Γ|·OI sum   4.23e+09
  Leg count    700 across 6 expirations
  ...

  Scope: SPY · ±10% strikes · 6 expirations
```

### 9.3 `--only=spy` (regression target)

Identical to today's output — no banner, no SPX rows. Header reads
`SPY Dealer Zero-Gamma` exactly as today.

## 10. Compute budget and UX

| Scope | Typical wall-clock | First-call ETA stamp |
|---|---|---|
| `--only=spy` | 3m 30s | 240 s |
| `--only=spx` | 3m 30s – 4m | 240 s |
| combined | 6m 30s – 7m 30s | 480 s |

The combined run crosses the user's 5-min anxiety threshold. Mitigations:

- **Per-index progress.** Compute envelope adds `phase: "spy" | "spx"` and
  per-phase progress. Renderer shows `computing SPY (62 %) → SPX queued`.
- **15-min soft TTL on SPX**, 5-min on SPY (see §5.5).
- **The `--only=spy` fast path** for trader-style rapid polling.
- **Background refresh** keeps a stale-but-served value live — the user only
  pays full wall-clock on the first call of an NY session.

If wall-clock complaints recur, the v2 plan is Option B (parallel shared
pool), which gets us back to ~5 min combined.

## 11. Test plan

### 11.1 Unit

- `selectExpirations` with mixed SPX/SPXW expiries on a third-Friday at
  10:00 ET → SPX-class third-Friday dropped (AM-settled), SPXW Friday
  weekly retained.
- `dteYears` with `tradingClass="SPX"` on a third-Friday → returns 0 at
  09:31 ET.
- Combined sweep aggregation: given two synthetic per-index profiles with
  known crossing points, assert the combined profile crosses at the
  correctly-weighted point.
- Skew fit on synthetic SPX put-skew (steeper than SPY): assert the curve
  is well-conditioned with R² > 0.85.

### 11.2 Integration / smoke

`make smoke` is the project's done-means anchor. The smoke harness must:

- Run `ibkr gamma --only=spy` first; require output matches the today's
  smoke baseline byte-for-byte (excepting the timestamp and computed-ago).
- Run `ibkr gamma --only=spx`. Behaviour depends on
  `SPX_EXPECTED_REACHABLE`:
  - If `=true` (default for **this** repo's `make smoke` target — the dev
    account IS entitled), banner-seen FAILS the smoke run. Real SPX data
    must come back: assert non-empty top strikes and a finite combined
    sweep.
  - If unset / `=false` (CI machines, accounts without CBOE OPRA), banner-
    seen is recorded as `skip("spx_entitlement_missing")` and exits 0.
- Run `ibkr gamma` (combined). With `SPX_EXPECTED_REACHABLE=true`, assert
  the full combined output with `SPY + SPX` header. Without it, accept
  either (a) combined output OR (b) the entitlement-skip banner.

The `SPX_EXPECTED_REACHABLE=true` default in `make smoke` is the
user-flagged guardrail: *"no SPX data would be a bug on my setup."* It
prevents the entitlement-graceful banner from silently masking a
regression on this dev account between releases.

### 11.3 Slot-leak audit checklist

See §7 — runs as part of the audit, output pasted in the merge commit.

## 12. Migration & risk

### 12.1 Wire-shape change (revised; **breaking semantic** for `--json`)

The earlier draft claimed top-level scalars (`SpotUnderlying`,
`ZeroGamma`, `Profile`, etc.) could be grown in place. The senior
reviewer correctly pushed back: those fields are scalar / single-shape.
A combined view has two spots, two flip points, two per-index sweeps.
You cannot grow them in place without breaking type compatibility.

**Decision: new `PerIndex` map + `Scope` discriminator.**

```go
type GammaZeroComputed struct {
    // === Headline (combined-when-both, SPY-only when SPX unavailable) ===
    Scope          string   // "spy" | "spx" | "spy+spx"  -- discriminator
    SpotUnderlying float64  // SCOPE-DEPENDENT: SPY spot when Scope="spy" or "spy+spx", SPX spot when Scope="spx"
    ZeroGamma      *float64 // SCOPE-DEPENDENT headline (see below)
    GapPct         *float64 // SCOPE-DEPENDENT
    // ... existing fields unchanged ...

    // === New: per-index breakdown (always populated when Scope != "spy") ===
    PerIndex map[string]*GammaZeroComputed `json:"per_index,omitempty"`
    // PerIndex["SPY"] and PerIndex["SPX"] each carry a fully-formed
    // GammaZeroComputed for that single index (Scope inside is "spy" or "spx").
    // PerIndex is nil for Scope="spy" (today's bit-for-bit behaviour).

    // === New: scope diagnostics ===
    PartialClasses map[string]string `json:"partial_classes,omitempty"`
    DecoupledCorr  *float64          `json:"decoupled_corr,omitempty"` // SPY/SPX 20d corr, nil when not computed
}
```

For `Scope="spy"` (the `--only=spy` regression target), `PerIndex` is
**nil** and the top-level scalars are exactly today's SPY values. This
preserves the bit-for-bit guarantee.

For `Scope="spy+spx"`:

- Top-level scalars hold the **combined headline**: `SpotUnderlying` is
  the SPY spot (kept as the renderer's anchor for the spot-% gap;
  documented), `ZeroGamma` is the combined γ-zero rendered as a
  spot-percent (we may need to keep `ZeroGamma` as a price level and add
  a new `CombinedGapPct *float64` — final shape decided in
  interview-q-5).
- `PerIndex["SPY"]` and `PerIndex["SPX"]` carry the per-index detail
  rows the renderer prints.

**Breaking-change disclosure:** JSON consumers that read top-level
`zero_gamma` from `ibkr gamma --json` (no flag → default combined
scope) will receive the combined value instead of the SPY-only value
they got before. Mitigations:

- Document the change in CHANGELOG `### What's new` (per project memory
  `release_notes_structure`).
- Add a `Scope` field they can branch on — old consumers that don't
  branch will see a combined value, which is the new intended semantics.
- Provide `ibkr gamma --only=spy --json` as the migration path for
  consumers wanting today's exact SPY-only shape.

### 12.2 Coverage threshold per underlying

`MinLegCoverageFraction = 0.2` was tuned for SPY's bursty RTH delivery
(`gamma_zero_compute.go:96-113`). SPX off-hours has no model-tick push
and depends entirely on the BS-IV fallback; realistic coverage is
plausibly 5–15 %, which would always fail the 0.2 gate.

**Decision:** per-underlying threshold:

```go
const (
    MinLegCoverageFractionSPY = 0.2
    MinLegCoverageFractionSPX = 0.05  // off-hours friendly
)
```

The SPX threshold is intentionally permissive **off-hours**. During RTH,
SPX option ticks DO push (the model-tick path works during regular
hours; the off-hours failure mode is the documented edge case). A future
v2 can make this RTH-aware. For v1, keep it simple: 0.05 always for SPX,
0.2 always for SPY.

Trade-off: an SPX RTH run that lands only 6 % of legs will now succeed
and serve a thin result, where SPY at the same coverage would refuse.
This is honest: SPX has fewer liquid strikes at the same ±10 % width,
and a sparse-but-real run is more useful than no run. Surfaced as a
`partial_coverage: 6 %` warning so the reader can dim the row.

### 12.3 Other risks

- **`computeGammaZeroSPX` → `computeGammaZeroFor`** is a rename across one
  package (`internal/daemon`). No CLI-level callers outside the package.
- **Cache shape change:** none. The earlier 2-tuple key proposal is
  withdrawn (§5.5). Existing in-memory cache is invalidated on daemon
  restart as usual; no persistence concern.
- **Risk: SPX off-hours unusable.** Mitigated by §12.2 (per-underlying
  coverage threshold) + §8.2.1 (partial-class detection) + the existing
  BS-IV fallback path.
- **Risk: doubling prewarm calls trips a gateway-side throttle we
  haven't seen with SPY-only.** `PrewarmOptionChain` is contract-details,
  not market-data; it has its own throttle envelope. Worth a load-test
  in the audit phase: time the two-pass SPX prewarm in isolation and
  confirm it lands under 30 s with no contract-details timeouts.
- **Risk: SPY-SPX decorrelation falsely triggers the gate during quiet
  intraday churn.** The 0.90 threshold is a defensible round number;
  empirical SPY/SPX 20-day daily-close correlation in the last 5 years
  has been ≥ 0.97 in calm regimes and dropped to ~0.85 during fast
  decoupling events (2020-03, 2024-08-05). 0.90 catches the events;
  false-positive rate should be near zero. If it triggers spuriously in
  prod, raise to 0.85 in a follow-up.

## 13. Locked decisions (post-interview, 2026-05-21 17:05 CEST)

The user answered the round-1 interview with recommendations accepted on
all 8 questions, plus one user-supplied caveat baked into the test plan:

1. **Default mode with SPX unreachable:** SPY-only + banner, exit 0.
   **User caveat:** *"we need to test this — no SPX data would be a bug
   on my setup."* See §11.2 SPX-expected-reachable smoke flag.
2. **Fan-out architecture:** Serial SPY-then-SPX (~7 min). Worker count
   stays at 4.
3. **JSON wire shape for combined headline:** Keep `zero_gamma` as a
   price level (SPY's when combined; the only-scope's otherwise); add a
   new `combined_gap_pct` field for the spot-percent headline. JSON
   consumers reading `zero_gamma` continue to see a price.
4. **SPX coverage threshold:** `MinLegCoverageFractionSPX = 0.05` with
   `partial_coverage: NN%` warning when below 0.20. SPY stays at 0.20.
5. **Smoke posture on this account:** banner-seen FAILS the smoke run
   when `SPX_EXPECTED_REACHABLE=true` is set in the dev env.
   `make smoke` exports this flag by default in this repo's smoke
   target; CI / other accounts can disable it.
6. **Top-strikes ordering:** Single combined-sorted list with an
   `INDEX` column. SPX rows dominate the top — that's the honest view.
7. **Decorrelation gate:** 0.90 20-day SPY/SPX correlation triggers the
   `decoupled` badge and promotes per-index headlines to primary.
8. **Implementation cadence:** Progressive to main, one commit at a time,
   each gated on `make check && make smoke`. Per
   `no_branches_single_dev`.

## 14. Implementation order (small commits)

Once the interview answers are in:

1. **Rename + parameterise.** `computeGammaZeroSPX` → `computeGammaZeroFor`
   with no behaviour change. SPY-only smoke unchanged. (1 commit.)
2. **Schema: classed expiry helper + cache-key trading class.** Two
   schema fixes in one commit because they share the same hazard
   (trading-class-less identity collapses SPX/SPXW):
   - Add `FetchOptionExpiryStrikesClassed` alongside the existing helper
     (§4.3.3). Keep `tradingClass` from `fields[4]` in
     `handleSecDefOptParam`. Unit test the SPX third-Friday two-class
     case.
   - Change `optionContractKey` to include trading class (§4.4). Bump
     `contractStoreVersion` v2 → v3 with backwards-read migration. Add a
     v2-fixture test asserting normalisation. Unit test SPX vs SPXW
     contract round-trip without collision.
   (1 commit; touches `pkg/ibkr`.)
3. **Plumb trading class through legs.** New optional `tradingClass` field
   on `legSpec` / `legData` / `productionLegFetcher`. SPY-only path passes
   empty string (today's default). (1 commit.)
4. **AM/PM settlement-aware helpers.** Refactor `selectExpirations` and
   `dteYears` to take a trading class (§4.3.2). Backwards-compat: empty
   class falls back to 16:00 ET. Unit tests for SPX 09:30 cutoff. (1
   commit.)
5. **Wire shape: `Scope`, `PerIndex`, `PartialClasses`, `DecoupledCorr`
   fields on `GammaZeroComputed`.** No combined logic yet — fields exist,
   default zero values. SPY-only path sets `Scope="spy"` and leaves the
   new fields nil. (1 commit.)
6. **SPX compute happy path.** New `internal/daemon/spx_chain.go` with the
   two-pass prewarm. Handler orchestration: SPY-then-SPX serial,
   underlying-hold transition with checklist-item-6 guarantees (§7.1).
   `--only=spx` flag wired. (1 commit.)
7. **Combined aggregation + decorrelation gate.** Per-spot sweep
   aggregation (§5.3), 20-day SPY/SPX correlation fetch + gate (§5.3.1).
   Combined headline + per-index rows on the renderer. (1 commit.)
8. **Entitlement-graceful degradation.** Detection of SPX-skipped and
   partial-class states (§8.2.1). Banner rendering. (1 commit.)
9. **Slot-leak audit + smoke updates.** All six audit checks per §7
   executed; results pasted in this commit message. Smoke harness updated
   for `spx_entitlement_missing` skip reason. (1 commit, audit results
   inline.)

Each step gates on `make check && make smoke` before the next is opened.
The senior reviewer's verdict treated steps 2, 4, 5, and the §12.2
threshold as **ship-blockers** — confirm those four land cleanly before
opening step 6.

## 15. Out of scope (deferred backlog)

- NDX, RUT, VIX coverage. CLI flag surface is shaped to allow them; not
  implemented.
- Parallel-shared-pool fan-out (Option B in §5.2). Defer until measured
  need.
- Sticky-strike skew during the sweep. Today is sticky-moneyness; the v1
  limitation around the sweep edge is documented and unchanged.
- Live-tick streaming mode for the regime call. Out of scope.
