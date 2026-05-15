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
