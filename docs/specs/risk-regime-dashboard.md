# Risk Regime Dashboard — Build Spec

**Updated:** 2026-05-24 12:50 CEST — dashboard rows now expose compact
`as_of`, band, threshold, and provenance metadata; MOVE/rates-vol is removed
from the release surface until a verified licensed source exists.

A single-page daily-check dashboard for a retail trader to detect when market dynamics shift from stabilizing to amplifying. The goal is **evidence balance, not prediction.** Multiple indicators flashing together is the real signal; any one alone is noise.

-----

## What to Build

A single-page web dashboard (HTML or React) showing eight indicator rows,
grouped into six evidence clusters so related signals do not double-count:

- Current value and recent trend (sparkline, ~30 days)
- Compact freshness/as-of badge per row: `live`, `15m delayed`, `close D-1`,
  `2d old`, `cached 11:42`, `computing`, or `unavailable`
- Status light: **green** (normal), **yellow** (watch), **red** (warning),
  or unranked when the data path is missing
- One-line plain-English interpretation under each
- A composite regime label at the top based on cluster counts, plus a raw
  indicator-row count for transparency
- Data refresh: daily is sufficient. No need for real-time.

-----

## Indicator Rows and Evidence Clusters

Clusters:

- Equity volatility: VIX/VIX3M and VVIX
- Credit: HYG/SPY and official HY/IG OAS
- Funding: 90-day AA financial commercial paper minus 3-month T-bill
- FX carry: USD/JPY
- Dealer gamma: SPY+SPX zero-gamma
- Breadth: S&P 500 breadth

Within a cluster, use the worst ranked band (red > yellow > green). This keeps
equity-vol or credit from counting twice while still showing disagreement in
the row table.

All threshold sets below are **heuristics pending backtest**. The dashboard
reports a risk score / confidence / evidence balance, not a forecast
probability. A probability field should not be added until the calibration
framework in [regime-backtest-plan.md](regime-backtest-plan.md) has produced
out-of-sample reliability evidence.

### 1. VIX Term Structure

**What it is:** Ratio of VIX (30-day implied vol) to VIX3M (3-month implied vol). Tells you whether short-term fear is greater than longer-term fear.

**Source:** vixcentral.com (free, scrapeable) or CBOE direct.

**Thresholds:**

- Green: VIX/VIX3M < 0.92 (healthy contango)
- Yellow: 0.92–1.00 (flattening)
- Red: > 1.00 (backwardation — acute stress pricing)

**Observation window:** Watch for **sustained inversion over 2–3 days**, not single spikes. A one-day flip on a Fed day means nothing. A flip that holds for a week is the real signal.

-----

### 2. VVIX Vol-of-Vol

**What it is:** Cboe VVIX, the expected volatility of VIX. It is a convexity
demand / vol-of-vol stress input and can rise before VIX/VIX3M fully inverts.

**Source:** Cboe official VVIX daily time series.

**Thresholds (heuristic):**

- Green: < 90
- Yellow: 90–110
- Red: > 110

**Observation window:** Treat as confirmation or disagreement inside the
equity-vol cluster. Do not read it as a standalone forecast.

-----

### 3. HYG vs SPX Divergence

**What it is:** High-yield corporate bond ETF (HYG) compared to S&P 500 (SPY). Credit markets often crack before equities.

**Source:** Any broker, TradingView, Yahoo Finance.

**Metric:** Compute 20-day rolling correlation, and track whether HYG is above/below its 50-day moving average while SPY is near 52-week highs.

**Thresholds:**

- Green: HYG and SPY both trending up together
- Yellow: HYG breaks 50-day MA while SPY is not near highs, or the 52-week-high context is incomplete
- Red: HYG below 50-day MA while SPY is still within 3% of highs; use the row streak to distinguish an early one-session divergence from a sustained 5+ session credit downtrend

**Observation window:** Divergence over **2–4 weeks** is meaningful. Single-day moves are noise.

-----

### 4. Official HY/IG Credit Spreads

**What it is:** ICE BofA high-yield and investment-grade corporate
option-adjusted spreads. This is the official cash-credit companion to the
faster HYG/SPY ETF proxy.

**Source:** FRED/St. Louis Fed CSV endpoints for ICE BofA OAS series:
`BAMLH0A0HYM2` (HY OAS) and `BAMLC0A0CM` (IG OAS).

**Thresholds (heuristic, percentage points):**

- Green: HY OAS < 4.0 and not widening quickly
- Yellow: HY OAS 4.0–5.5, or HY OAS widening > 0.50 pp over ~20 observations
- Red: HY OAS > 5.5, or HY OAS widening > 1.00 pp over ~20 observations

**Observation window:** Daily close. The signal is slower but more official
than ETF price action.

-----

### 5. Funding Stress Spread

**What it is:** 90-day AA financial commercial paper rate minus 3-month
Treasury bill secondary-market rate, an OFR-style U.S. funding spread.

**Source:** FRED/St. Louis Fed official Federal Reserve series
`RIFSPPFAAD90NB` and `DTB3`.

**Thresholds (heuristic, basis points):**

- Green: < 25 bp
- Yellow: 25–75 bp
- Red: > 75 bp

**Observation window:** Daily. This is a slow funding/liquidity check, not an
intraday funding-stress detector.

-----

### 6. USD/JPY

**What it is:** Dollar-Yen exchange rate. A proxy for global carry trade leverage — when yen rallies hard (USD/JPY falls), leveraged risk positions are being unwound globally.

**Source:** Any broker, Yahoo Finance (`JPY=X`).

**Thresholds:**

- Green: Stable or moving < 1% per week
- Yellow: Falling 1–2% in a week (yen strengthening)
- Red: Falling > 2% in 3 days, or > 3% in a week

**Observation window:** This can be **acute and fast** — August 2024 unwound in three sessions. Check daily. Speed of move matters more than absolute level.

-----

### 7. Dealer Zero-Gamma Level (SPY+SPX)

**What it is:** The S&P 500 price level where market-maker hedging flips from dampening volatility to amplifying it. Above the level, dealers buy dips and sell rips. Below it, they sell into selloffs and buy into rallies — the dangerous regime.

**Underlying:** The compute uses SPY (the S&P 500 ETF), not SPX (the index). SPY trades extended hours on SMART/ARCA with continuous market-maker quotes, has a single trading class, and IBKR pushes IV ticks for its options pre-market. SPX has no spot trading outside RTH and its option IV ticks aren't computed by the gateway pre-market, which made the SPX-based compute consistently fail to land a single leg off-hours. SPY dealer gamma tracks SPX dealer gamma closely (both are dominated by the same dealer-positioning regime) — the regime signal is unchanged, only the absolute level is SPY-scale (~SPX/10).

**Source:** Computed locally from IBKR's SPY option chain via the Perfiliev BS-sweep (`ibkr_gamma`, `gamma.zero_spx`).

**Thresholds:**

- Green: SPY > 2% above zero-gamma level
- Yellow: SPY within 2% of zero-gamma
- Red: SPY trading below zero-gamma level

**Observation window:** The **flip itself is the event.** Once SPY closes below zero-gamma, the regime has shifted; no waiting period needed.

-----

### 8. Market Breadth — % of SPX Stocks Above 50-Day MA

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

|Cluster reds|Cluster yellows|Interpretation            |
|------------|---------------|--------------------------|
|0           |0–2            |Normal regime             |
|0           |3+             |Elevated stress watch     |
|1–2         |any            |Stress signal present     |
|3+          |any            |Broad stress regime       |
|all ranked clusters red|—   |Full risk-off conditions  |

Show raw indicator evidence beside or below the cluster evidence. Example:
`Normal regime · 5 green clusters / 1 unranked cluster` followed by
`Indicators: 6 green / 2 unranked`.

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

`ibkr regime` / `ibkr_regime` / `regime.snapshot` returns all eight
indicator rows in one JSON envelope. The daemon derives default bands
only for persisted streaks and the compact composite; raw measurements
remain available so consumers can apply their own thresholds. Each
indicator row on the response carries:

- raw measurement(s) — pointers so "not arrived yet" vs "exactly
  zero" stays distinguishable;
- a `status` field — `ok`, `stale` (gateway delivered a frozen or
  delayed tick), `computing` (gamma's background compute), `unavailable`
  (no data source), or `error`;
- a `notes` field in `--explain` / expanded JSON — the spec's threshold
  bands embedded verbatim, so an LLM consumer doesn't need to consult this
  document.

A `spec_doc` field on the envelope points back here for deep-linking.

**Live-test result on 2026-05-17 (frozen weekend data)**:

Read this as the *normal* weekend response — several rows stale or
unavailable. Live-market behavior populates `ratio` + `last` fields
on the stale-during-weekend rows and surfaces gamma as `computing`
on the first call of the NY session.

```json
{
  "vix_term_structure": { "vix": 18.43, "vix3m": 21.36, "ratio": 0.863, "status": "stale" },
  "hyg_spy_divergence": { "hyg_price": 79.55, "spy_price": 737.34, "status": "stale" },
  "usd_jpy":            { "last": 158.7285, "status": "stale" },
  "gamma_zero":         { "status": "error", "envelope": { "error": "no SPX spot available" } },
  "breadth":            { "status": "ok", "envelope": { "state": "ready", "value": 61.8 } }
}
```

Read this as: weekend hours, gateway in frozen mode. VIX ratio 0.863
applied against the spec gives **green** (<0.92 is healthy contango).
Gamma errored because SPX is not delivering any tick over weekend
nights — expected; rerun during market hours. Breadth is served from
the daemon's persisted cache (last weekday's post-close refresh — see
Indicator 8 below for how the local engine computes the metric).

## Daemon methodology — what the IBKR daemon actually computes

This section documents how the daemon sources and labels the rows. VIX/VIX3M,
HYG/SPY, and USD/JPY use standard IBKR quote/history endpoints; VVIX,
official OAS, and funding spreads use official Cboe/FRED daily files. USD/JPY
routes through native CASH/IDEALPRO FX. MOVE/rates-vol is intentionally absent
until a verified IBKR contract or licensed official connector exists.

Every indicator row exposes compact metadata in default JSON/MCP:

- `status`: `ok`, `stale`, `computing`, `unavailable`, or `error`
- `band`: `green`, `yellow`, `red`, or `unranked`
- `band_reason`: one short threshold explanation for the current row
- `thresholds`: `{label, green, yellow, red, heuristic, pending_backtest}`
- `as_of`: `{label, freshness, source, time/date, age_seconds}` where
  `label` is the CLI column value

Text output prints `AS OF` between `VALUE` and `BAND`; JSON/MCP keeps the same
data under each row for agents.

### Indicator 8 — Market Breadth (`breadth.spx`, `ibkr_breadth`)

**Source.** S&P Dow Jones Indices publishes the `S5FI` (% above
50-day SMA) and `S5TH` (% above 200-day SMA) index family plus the
new-52w-highs/lows count. IBKR does not redistribute these on retail
subscriptions (verified via `reqContractDetails` — see
`pkg/ibkr/symbols.go`), so the daemon computes the equivalents
locally from the 500 constituent daily closes pulled via IBKR's
historical-bar feed (HMDS). Method token: `constituent-fanout-50/200dma-hl`.

**Three readings, one refresh.** The compute walks each constituent's
daily bars once and returns:

- `pct_above_50dma` — the tactical signal. Spec bands: >55 green /
  40-55 yellow / <40 with SPX within 3% of 52-week high red.
- `pct_above_200dma` — the slow companion that catches cyclical
  tops cleanly. Locked-plan bands: >60 green / 40-60 yellow / <40
  red (calibrated to the post-Mag-7 era; the StockCharts 70/30
  default fires red far too often in this concentration regime).
- `new_highs_today` / `new_lows_today` — constituent counts of names
  making fresh 252-bar highs/lows (≈ "52 weeks"). The derived
  `net_new_highs_pct = (highs − lows) / coverage × 100`. The
  narrow-rally pattern is SPX at/near highs with
  `net_new_highs_pct` near zero or negative — a small set of mega-
  caps carrying the index while the median name rolls over.
  September 2025 was a textbook example: SPX at ATH with only 4.6%
  of names at 52-week highs.

**Update cadence.** Once-daily refresh post-close at 16:35 ET. The
scheduler waits until both the regular session and the S&P DJI
publication window have settled, then slides each constituent's
200-bar window forward and updates the 252-bar rolling max/min
trackers. Readers see a cached snapshot, never a multi-minute
fan-out on the read path.

**Cold start.** ~60 minutes of wall-clock — unchanged from v1.
IBKR's historical-data pacing limit is per-request, not per-bar, so
pulling 200 bars per constituent instead of 50 doesn't cost more
requests. The cap at 60 requests per 10-min sliding window means
sustained throughput ≈ 6 names/min for the ~500-name fan-out. The
v2 cap on the per-constituent close window grew from 50 to 200
entries; v1 on-disk caches trigger a graceful rebuild because their
windows are too short to seed the 200-day reading honestly.

**History.** A best-effort fetch of trailing daily points carrying
all three readings — the renderer charts each as its own sparkline.

**Coverage safety.** If a refresh completes with fewer than the
engine's minimum coverage fraction (0.80 of the constituent set on
the 50-day reading), the new snapshot is rejected and the previous
good value continues to serve under `state: "degraded"`. The
200-day and new-highs/lows readings carry their own coverage
denominators (smaller, because more names need to clear the higher
history bar); a recent IPO contributes to the 50-DMA reading but
not yet to the 200-DMA or new-52w-highs count.

**Limitations.**

- Constituent list is snapshotted from the index membership file;
  S&P additions/removals between updates are not reflected until
  the next refresh of that file.
- SMA windows are computed on regular-session closes only — no
  pre/post-market adjustment.
- When the gateway's data type on a constituent's bar isn't live,
  the daemon still includes it; the headline `data_type` reflects
  the worst-case across the contributing bars.
- The rolling 252-bar max/min is exact once the engine has observed
  a full year of bars per constituent; before that, the count
  surfaces under `coverage_highs_lows` so a renderer can flag
  under-covered days.

### Indicator 4 — Dealer Zero-Gamma (`gamma.zero_spx`, `ibkr_gamma`)

The daemon estimates the SPX price level at which aggregate
market-maker gamma exposure crosses zero. Above the level dealer
hedging is mean-reverting (dampens vol); below it, momentum-following
(amplifies vol). **Treat this as a regime hint, not a precise level.**

**Methodology token: `perfiliev-bs-sweep-v2-stickymoneyness`**

(Renamed from `perfiliev-bs-sweep-v1` — see the "Methodology v2 additions"
subsection at the end of this section for the changes.)

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

#### Methodology v2 additions (2026-05-20)

The v1 → v2 cutover replaces the sticky-IV recipe with sticky-moneyness
and adds two complementary outputs that broaden the regime view without
changing the headline contract.

- **Sticky-moneyness sweep.** The sweep now fits a quadratic skew
  curve in log-moneyness per expiry at snapshot time
  (`σ(m) = A + B·m + C·m²`, with `m = ln(K/S)`) and looks up σ at each
  scenario-spot's moneyness during the sweep instead of holding the
  captured snapshot IV fixed. Calls and puts are pooled (put-call
  parity makes them lie on the same surface). The fit clamps
  evaluations to the observed moneyness range — extrapolating a
  parabola outside the fitted window would imply IVs the data doesn't
  support. Curves that fail to fit (< 3 IV samples, degenerate solve)
  fall back to sticky-IV for that expiry only; surface as
  `skew_fallback:YYYYMMDD` warnings on the envelope. The expected
  effect: `zero_gamma` shifts ~30-80 SPX points relative to v1 and
  tracks SpotGamma's posted numbers materially better. **Revert
  criterion:** if four-week sign-agreement vs SpotGamma's Friday
  recap drops below the v1 baseline, revert to the prior recipe.

- **0DTE / weekly / term split.** The sweep now produces separate
  per-index γ-zero readings for 0DTE (`DTE == 0`), 1-7 DTE, and term
  (`DTE > 7`) buckets. 0DTE, end-of-week, and monthly-OPEX dynamics
  behave differently; aggregating them hides the highest-information
  case where the readings disagree. The regime row's
  `horizon_agreement` field names the relation for single-underlying
  gamma envelopes; in the default SPY+SPX view the horizon buckets live
  under each `per_index` result because there is no combined price
  scale. Wire fields: `zero_gamma_0dte`, `zero_gamma_1to7`,
  `zero_gamma_term`, the matching `profile_*` and `gamma_sign_*`
  fields, and `leg_count_*`. Buckets with zero usable GEX legs surface
  as nil plus scoped `warning_details`.

- **Per-indicator streak counter.** Every regime row now carries a
  `streak: {band, sessions, since}` field counting how many
  consecutive NY trading sessions the indicator has been in its
  current band. Daemon-classified using the spec's default thresholds
  for streak purposes — a slight violation of the "daemon doesn't
  derive bands" posture, accepted because streak persistence requires
  a stable daemon-side classification. Renderers with custom
  thresholds read the raw value cell and ignore the streak's
  classification. Persisted at
  `$XDG_CACHE_HOME/ibkr/regime-streaks.json` across daemon restarts;
  computing/unavailable/error states freeze the counter rather than
  reset it (a stale data point shouldn't end a streak).

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

**Tooling.** `ibkr regime --log <path>` appends one JSON line to a
file each time it's invoked: `{timestamp, regime}` with the full
envelope inline. Run from cron (e.g. weekday 16:40 ET) for the four
weeks. We suggest filenames like `regime-v1.jsonl` so a future
schema change can use a new filename rather than break the parser.
Plain JSONL — `jq` and pandas both read it; analyse the
`gamma_zero.envelope.result.zero_gamma` field against
SpotGamma's posted value.

### Composite-score honesty

The dashboard composite is cluster-aware and uses the worst ranked band within
each cluster, so a red equity-vol or credit sub-signal cannot be hidden by a
larger number of green rows. The raw indicator counts remain visible for
transparency.

Stale ranked rows still contribute a band, but they lower summary confidence
and appear in `warning_details`; unavailable/error/computing rows stay
unranked, lower coverage, and affect the punch line. This is especially
important for daily official files and cached gamma/breadth snapshots: a
Friday close on Sunday is not the same freshness as a live SPY tick.

The dashboard composite still does *not* claim statistical independence across
clusters. Empirically:

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
- Re-add ICE BofA MOVE/rates-vol only after a verified IBKR contract or
  licensed official data connector exists; do not proxy it with ETFs or
  futures.
- Backtest-derived calibration for threshold sets and cluster weights
  (see `docs/specs/regime-backtest-plan.md`).
