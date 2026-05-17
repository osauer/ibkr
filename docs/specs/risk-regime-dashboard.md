# Risk Regime Dashboard — Build Spec

A single-page daily-check dashboard for a retail trader to detect when market dynamics shift from stabilizing to amplifying. The goal is **probability shifting, not prediction.** Multiple indicators flashing together is the real signal; any one alone is noise.

-----

## What to Build

A single-page web dashboard (HTML or React) showing five indicators with:

- Current value and recent trend (sparkline, ~30 days)
- Status light: **green** (normal), **yellow** (watch), **red** (warning)
- One-line plain-English interpretation under each
- A composite “regime score” at the top (count of red/yellow indicators)
- Data refresh: daily is sufficient. No need for real-time.

-----

## The Five Indicators

### 1. VIX Term Structure

**What it is:** Ratio of VIX (30-day implied vol) to VIX3M (3-month implied vol). Tells you whether short-term fear is greater than longer-term fear.

**Source:** vixcentral.com (free, scrapeable) or CBOE direct.

**Thresholds:**

- Green: VIX/VIX3M < 0.92 (healthy contango)
- Yellow: 0.92–1.00 (flattening)
- Red: > 1.00 (backwardation — acute stress pricing)

**Observation window:** Watch for **sustained inversion over 2–3 days**, not single spikes. A one-day flip on a Fed day means nothing. A flip that holds for a week is the real signal.

-----

### 2. HYG vs SPX Divergence

**What it is:** High-yield corporate bond ETF (HYG) compared to S&P 500 (SPY). Credit markets often crack before equities.

**Source:** Any broker, TradingView, Yahoo Finance.

**Metric:** Compute 20-day rolling correlation, and track whether HYG is above/below its 50-day moving average while SPY is near 52-week highs.

**Thresholds:**

- Green: HYG and SPY both trending up together
- Yellow: HYG breaks 50-day MA while SPY still within 3% of highs
- Red: HYG in clear downtrend (below 50-day for 5+ sessions) while SPY at/near highs

**Observation window:** Divergence over **2–4 weeks** is meaningful. Single-day moves are noise.

-----

### 3. USD/JPY

**What it is:** Dollar-Yen exchange rate. A proxy for global carry trade leverage — when yen rallies hard (USD/JPY falls), leveraged risk positions are being unwound globally.

**Source:** Any broker, Yahoo Finance (`JPY=X`).

**Thresholds:**

- Green: Stable or moving < 1% per week
- Yellow: Falling 1–2% in a week (yen strengthening)
- Red: Falling > 2% in 3 days, or > 3% in a week

**Observation window:** This can be **acute and fast** — August 2024 unwound in three sessions. Check daily. Speed of move matters more than absolute level.

-----

### 4. Dealer Zero-Gamma Level (SPX)

**What it is:** The S&P 500 price level where market-maker hedging flips from dampening volatility to amplifying it. Above the level, dealers buy dips and sell rips. Below it, they sell into selloffs and buy into rallies — the dangerous regime.

**Source:** SpotGamma free posts on X/Twitter, Menthor Q free content, or paid feeds (SpotGamma, Tier1Alpha).

**Note for builder:** This is the hardest data point to automate. Free version: scrape latest public post from designated accounts. Paid version: API integration.

**Thresholds:**

- Green: SPX > 2% above zero-gamma level
- Yellow: SPX within 2% of zero-gamma
- Red: SPX trading below zero-gamma level

**Observation window:** The **flip itself is the event.** Once SPX closes below zero-gamma, the regime has shifted; no waiting period needed.

-----

### 5. Market Breadth — % of SPX Stocks Above 50-Day MA

**What it is:** Tells you whether the rally is broad or carried by a few mega-caps. Narrow rallies are fragile.

**Source:** indexindicators.com, stockcharts.com (`$SPXA50R`), barchart.com — all free.

**Thresholds:**

- Green: > 55% and trending sideways/up
- Yellow: 40–55%, or falling while SPX still near highs
- Red: < 40% while SPX within 3% of 52-week highs (classic divergence)

**Observation window:** **2–4 weeks of declining breadth** while the index holds up is the textbook late-cycle warning. Day-to-day moves are noise.

-----

## Composite Logic

Display at the top of the dashboard:

|Reds|Yellows|Interpretation                                  |
|----|-------|------------------------------------------------|
|0   |0–2    |Normal regime                                   |
|0   |3–5    |Elevated alert — review positioning             |
|1–2 |any    |Watch closely, prep defensive moves             |
|3+  |any    |Regime shift likely — execute pre-committed plan|
|5   |—      |Full risk-off conditions                        |

**Critical:** the dashboard should not tell you what to do. It should tell you what conditions are. Action rules must be pre-committed by the user before the moment arrives.

-----

## Honest Caveats (display at footer)

- These indicators produce **false signals regularly.** Most “yellow” readings resolve back to green without anything happening.
- The cost of acting on every signal is real — you will underperform a buy-and-hold in calm markets.
- This dashboard is most useful as input to **rules you’ve decided in advance** (e.g., “I reduce equity exposure 30% when 3+ indicators are red for 2 consecutive sessions”).
- No combination of indicators caught August 2024 in advance with more than a few days of warning. Tails are tails.
- Free data sources lag. Don’t trade intraday off this.

-----

## Glossary

**Backwardation** — When the front-month future trades higher than later-dated futures. In VIX, this means traders are paying more for protection now than later — a stress signal.

**Breadth** — How many stocks in an index are participating in a move. Narrow breadth = a few stocks carrying the rally.

**Carry trade** — Borrowing in a low-yield currency (yen) to invest in higher-yielding assets elsewhere. Unwinds violently when funding currency strengthens.

**Contango** — Normal state for VIX futures: later months priced higher than near months. Means the market expects calm now, more uncertainty later.

**Dealer / Market Maker** — Firm that quotes both buy and sell prices for options and hedges by trading the underlying stock. Their hedging flow can stabilize or amplify markets depending on positioning.

**Delta** — How much an option’s price changes for a $1 move in the underlying stock. Used by dealers to size their stock hedge.

**Gamma** — How much an option’s delta changes as the underlying moves. Short-gamma dealers must trade *in the direction of the move* to stay hedged — buying as price rises, selling as it falls. This amplifies volatility.

**HYG** — iShares iBoxx High Yield Corporate Bond ETF. Most-watched proxy for high-yield credit health.

**Implied volatility (IV)** — The market’s expectation of future volatility, embedded in option prices. VIX is the SPX 30-day IV.

**Zero-gamma level** — The SPX price where aggregate dealer gamma flips sign. Above it: dealers dampen moves. Below it: dealers amplify moves.

-----

## Build Notes for the Builder Agent

- Prefer free data sources where possible; mark indicators that require paid API.
- Cache aggressively; data only needs to update once per trading day after close.
- Make thresholds **configurable** via a settings panel — the user may want to tune them.
- Mobile-readable layout; user will check this on phone.
- No login, no account, no telemetry. Local-only state.
- Optional: email/push alert if composite state changes color overnight.

-----

## Daemon API — `regime.snapshot` (v0.21.0+)

`ibkr regime` / `ibkr_regime` / `regime.snapshot` returns all five
indicators in one JSON envelope. The daemon never derives
green/yellow/red status — the bands above are user-tunable per
this spec, so threshold derivation lives in the renderer (or in an
LLM consumer's reasoning when called via MCP). Each indicator row
on the response carries:

- raw measurement(s) — pointers so "not arrived yet" vs "exactly
  zero" stays distinguishable;
- a `status` field — `ok`, `stale` (gateway delivered a frozen or
  delayed tick), `computing` (gamma's background compute), `unavailable`
  (no data source), or `error`;
- a `notes` field — the spec's threshold bands embedded verbatim,
  so an LLM consumer doesn't need to consult this document.

A `spec_doc` field on the envelope points back here for deep-linking.

**Live-test result on 2026-05-17 (frozen weekend data)**:

```json
{
  "vix_term_structure": { "vix": 18.43, "vix3m": 21.36, "ratio": 0.863, "status": "stale" },
  "hyg_spy_divergence": { "hyg_price": 79.55, "spy_price": 737.34, "status": "stale" },
  "usd_jpy":            { "last": 158.7285, "status": "stale" },
  "gamma_zero":         { "status": "error", "envelope": { "error": "no SPX spot available" } },
  "breadth":            { "status": "unavailable" }
}
```

Read this as: weekend hours, gateway in frozen mode. VIX ratio 0.863
applied against the spec gives **green** (<0.92 is healthy contango).
Gamma errored because SPX is not delivering any tick over weekend
nights — expected; rerun during market hours. Breadth is unavailable
because IBKR doesn't carry the S5FI feed (see Indicator 5 below).

## Daemon methodology — what the IBKR daemon actually computes

This section documents how Indicators 4 (Dealer Zero-Gamma) and 5
(Market Breadth) are sourced from the IBKR gateway. Indicators 1–3
(VIX term structure, HYG/SPX divergence, USD/JPY) use the standard
quote/history endpoints; USD/JPY routes through native CASH/IDEALPRO
FX (added in v0.21.0).

### Indicator 5 — Market Breadth (`breadth.spx`, `ibkr_breadth`)

**Source.** S&P Dow Jones Indices publishes the `S5FI` index (% of
S&P 500 constituents above their 50-day SMA). The daemon reads it
directly from IBKR's `INDEX` exchange — no constituent fan-out, no
daemon-side SMA recomputation. S&P DJI does the math; we read the
result.

**Update cadence.** Whatever S&P DJI publishes. On retail entitlements
this is typically delayed; the daemon surfaces the gateway's feed state
in `data_type` so renderers can dim the headline when it isn't live.

**History.** A best-effort fetch of ~30 trailing daily bars for the
sparkline. The lookback is padded above the requested length to
compensate for non-trading-day shrinkage.

**Limitations.**

- Headline value reflects whatever timezone of update S&P pushes; the
  daemon doesn't recompute or interpolate.
- When the gateway's data type isn't live, the headline still
  surfaces — labelled — rather than failing. Use `data_type` to
  decide whether to trust an intraday read.

### Indicator 4 — Dealer Zero-Gamma (`gamma.zero_spx`, `ibkr_gamma`)

The daemon estimates the SPX price level at which aggregate
market-maker gamma exposure crosses zero. Above the level dealer
hedging is mean-reverting (dampens vol); below it, momentum-following
(amplifies vol). **Treat this as a regime hint, not a precise level.**

**Methodology token: `perfiliev-bs-sweep-v1`**

The compute follows the [Perfiliev recipe](https://perfiliev.com/blog/how-to-calculate-gamma-exposure-and-zero-gamma-level/),
endorsed by Harel Jacobson (BNP options) and the basis for several
public open-source GEX calculators:

1. **Snapshot SPX spot.** Refused if the gateway's data type is
   anything other than "live" or "" (no notice yet).
2. **Enumerate option chain.** All listed expirations and strikes via
   one `reqSecDefOptParams` round-trip. Both AM-settled SPX and
   PM-settled SPXW contracts arrive (the trading-class merge is
   automatic at this layer and is pinned by a regression test).
3. **Select 6 nearest expirations,** dropping any 0DTE that's already
   past the 16:15 ET conservative settlement cutoff.
4. **Filter strikes** to ATM ± 10 % per expiry.
5. **Fan-out per-leg subscriptions** at 4 concurrent (the documented
   safe gateway throttle). Each leg waits up to 5 s for OI and IV
   ticks; gateway-computed gamma (if delivered) is captured for the
   snapshot aggregation.
6. **Aggregate at spot:** `Σ sign(right) × Γ × OI × 100 × spot² ×
   0.01`. Sign convention is the 2018-era Perfiliev default:
   **calls long, puts short**.
7. **Sweep spot ∈ [0.85, 1.15] × snapshot_spot in 60 steps.** For
   each scenario spot, recompute Γ per leg via Black-Scholes with
   the leg's captured IV and DTE. Hold IV fixed across the sweep —
   a documented v1 limitation (see below).
8. **Linear interpolation** between the bracketing points of the
   first zero crossing gives the headline `zero_gamma` price.
9. **Magnitude signal:** alongside the signed value, the daemon
   returns `gamma_total_abs` and `top_strikes` ranked by `|Γ| × OI`.
   This signal is sign-agnostic — robust to the dealer-positioning
   assumption — and is the more reliable input when covered-call ETF
   flow or autocall barrier proximity is likely to invert the naive
   sign.

**Known limitations of the v1 methodology** — keep these visible in
the dashboard's footer or tooltip:

- **Dealer sign assumption (the biggest one).** "Long calls, short
  puts" was right in the 2010s when retail bought calls and
  institutions bought puts. Today the flows fight back:
  - Covered-call ETFs (JEPQ ~$37 B, QYLD ~$8 B, XYLD ~$3 B) sell
    index calls → dealers long calls (agrees with Perfiliev).
  - Autocallables (~$500 B+ outstanding) flip sign as spot
    approaches barriers — can swamp the baseline near specific
    events.
  - Retail 0DTE flow is mixed.
  Net: the convention is a defensible baseline on most days but
  can be wrong by sign near covered-call-ETF concentration or
  autocall barriers. That's why the daemon ships the magnitude
  signal alongside.

- **Sticky IV across the sweep.** Each leg's IV is captured once at
  snapshot spot and reused for every scenario spot. The skew is
  real: SPX puts ~10 % OTM have IV that's materially higher than
  ATM. Holding IV fixed biases `zero_gamma` upward by ~30–80 SPX
  points typical, more in stress. Renderers should round the
  displayed value to the nearest 25 SPX points to signal the
  precision the methodology can defensibly claim. Sticky-strike
  skew handling is on the backlog for v2.

- **End-of-day Open Interest.** IBKR delivers OI as of the previous
  trading day's close (OCC tape). [SpotGamma](https://spotgamma.com/spx-gamma-model-positioning-adjustments/)
  pays for intraday OI inference from OPRA print aggressor-tagging;
  that's not accessible from IBKR's API. The daemon's number is
  ≥ 24 hours stale on the OI side. Acceptable for daily-refresh
  regime detection; not acceptable for tick-rate decisions.

- **No proprietary positioning model.** This is intentional. Vendors
  (SpotGamma, Tier1Alpha) add proprietary OI/volume adjustments
  that we cannot replicate from IBKR alone. Cross-check against
  their public posts during the calibration ritual below.

- **AM vs PM-settled overlap at 3rd Fridays.** On the third Friday
  of each month both `SPX` (AM-settled) and `SPXW` (PM-settled)
  contracts exist with the same date. IBKR's contract resolver
  picks one — usually the higher-OI variant. Most expiries don't
  have this overlap; the corner case is documented and intentional
  for v1.

- **Reference precision.** `zero_gamma` is returned to two decimal
  places on the wire so the renderer can choose its own rounding.
  The compute's effective precision (after the limitations above)
  is ≈ ±25 SPX points.

### Calibration ritual (first 4 weeks after launch)

The methodology has known biases. The only honest way to know whether
those biases matter for *your* dashboard is to cross-check against a
known public reference. For the first 4 weeks after enabling
`gamma.zero_spx` in production:

1. **Each Friday after close**, fetch the daemon's `zero_gamma` and
   compare to SpotGamma's free Friday recap post on X/Twitter
   (typically published 4-5 PM ET).
2. **Log the delta:**
   - Sign agreement (both above spot? both below?). Sign
     disagreement is the loudest signal that the dealer-positioning
     assumption is wrong for the day.
   - Magnitude delta in SPX points. Expect ≤ 50 points typical for
     calm days; > 100 points on autocall-heavy weeks.
3. **After 4 weeks**, review the log:
   - If sign agreement is < 70 %, revisit the convention (file a
     v2 issue to parameterise).
   - If magnitude delta is consistently > 100 SPX points, widen the
     dashboard's "within 2 % of zero-gamma" yellow band to 3 %.
   - If both numbers look healthy, the calibration ritual ends.

This is a human ritual, not an automated check — the references it
compares against are publicly-posted opinions, not API feeds.

### Composite-score honesty

The dashboard composite (count of red / yellow indicators) does *not*
account for correlation between indicators. Empirically:

- Zero-gamma flips correlate ~0.6+ with VIX backwardation (Indicator 1).
- Breadth collapse correlates with HYG/SPX divergence (Indicator 2).

A "4-red" event may therefore reflect ~2.5 independent factors
shouted four ways. Renderers should consider showing a cluster
breakdown alongside the raw count.

### Deferred backlog

These are deliberately out of scope for v1; they are tracked items
for future versions:

- Sticky-strike or sticky-delta IV across the sweep (resolves
  the sticky-IV limitation above).
- Intraday OI inference from OPRA prints (would require a separate
  data subscription; out of IBKR scope).
- Parameterised dealer convention (current: hardcoded Perfiliev;
  v2 could expose both signs and surface the spread as a regime-
  uncertainty signal).
- Holiday-aware cache TTL (current: hardcoded NY date rollover at
  midnight; correct except on early-close days).
- MOVE / IG-HY spread as a 6th regime indicator.
