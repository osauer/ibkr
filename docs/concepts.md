# Concepts

What the three load-bearing indicators measure, in enough depth to read the output without mis-acting on it. Engineering rationale (methodology choices, fitting curves, regime classifiers) lives in [`docs/design/`](./design/); this page is the user's mental model.

---

## Regime

The five-indicator risk-regime dashboard summarises *the market's current posture* in one snapshot, designed for a 30-second daily check rather than a continuous monitor. Each indicator measures a different stress channel; together they distinguish "ordinary chop" from "regime shift in progress."

The five:

1. **VIX term structure** (VIX vs VIX3M). Backwardation — short-dated vol pricing above 3-month vol — is the stress fingerprint. The deeper and more sustained the inversion, the bigger the dislocation.
2. **HYG vs SPY divergence**. High-yield credit (HYG) leads equity selloffs on the way down; a HYG breakdown while SPY is still near highs is the classic late-cycle pre-tell.
3. **USD/JPY weekly move**. JPY funding-pair unwinds are a recurring stress amplifier (Aug 2024, Dec 2018, Jan 2016). A >3% week is a Tier-1 signal.
4. **Dealer zero-gamma** (SPY + SPX combined). Whether the dealer book stabilises or amplifies day-over-day moves. See the [Gamma](#gamma) section.
5. **S&P 500 breadth**. Whether the index's strength is broad or carried by a handful of mega-caps. See the [Breadth](#breadth) section.

Each row carries raw measurements plus a `notes` field embedding the spec's threshold bands verbatim — green / yellow / red derivation is intentionally left to the consumer because every trader has a different risk tolerance. Each row also carries a `streak` field counting consecutive sessions in the current band; a Day-1 stress event reads differently from a Day-5 one.

Two failure modes worth flagging on the wire:

- Indicators 4 (gamma) and 5 (breadth) are heavy computes. On the first call of an NY trading day, indicator 4 returns `status: "computing"` with an ETA; on a fresh daemon, indicator 5 returns `state: "computing"` while the constituent fan-out runs (~60 min cold).
- Indicators 1–3 may carry a `fields_missing` array for optional sub-fields that didn't land within the fetch budget. The primary measurement still landed; treat `fields_missing` as a render hint, not an error.

The full methodology spec is at [`docs/specs/risk-regime-dashboard.md`](./specs/risk-regime-dashboard.md). Use it when calibrating your own threshold bands — the spec's suggestions are starting points, not gospel.

---

## Gamma

Dealer zero-gamma is the spot price at which the aggregate options-dealer book switches from amplifying market moves (short-gamma, below zero) to stabilising them (long-gamma, above zero). It's a regime hint, not a precision level — but the qualitative state matters enormously for short-horizon risk.

`ibkr_gamma` and the regime row's indicator 4 both compute from IBKR's option chains using the Perfiliev convention (dealers long calls, short puts), summed across the 6 nearest non-0DTE-post-settlement expirations at ±10% strike width. Two key methodology choices:

1. **Sticky-moneyness skew** (`bs-gamma-profile-v3-stickymoneyness-0dte-split`). The spot sweep reprices each leg's IV at the scenario-spot's *moneyness* via a per-expiry quadratic skew curve fitted at snapshot time — sticky-moneyness rather than sticky-IV. Without this, the put-side skew biases zero-gamma estimates upward by 5–10%.

2. **Combined SPY+SPX with regime-agreement classifier**. SPY (continuous ETF, retail flow) and SPX (index options, institutional flow) often agree on regime direction, but the actionable signal is **disagreement** — one book stabilising while the other amplifies. The classifier surfaces `"agree:long-gamma"`, `"agree:short-gamma"`, `"agree:transition-gamma"`, or `"disagree"` directly so consumers don't have to derive it. A crossing is long/transition/short based on spot's distance from the identified γ-zero, not merely the existence of a crossing.

Two complementary outputs on every result:

- **Signed zero-gamma**: the price level itself, plus a `gamma_sign` ("positive"/"negative") describing the dealer book's posture at current spot. The Perfiliev sign convention assumes the standard "dealers long calls, short puts" book; in regimes dominated by covered-call ETF flow or autocall hedging the sign can invert. Treat as a regime hint, not gospel.
- **Sign-agnostic magnitude**: `gamma_total_abs` (sum of |Γ|·OI in notional terms) and `top_strikes` (the largest concentrations regardless of sign). Sign-convention agnostic — useful when the signed reading is suspect.

Compute timing: the first call of an NY trading day kicks a multi-minute background job; later callers within the same session see `status: "ready"` instantly. The cache persists across daemon restarts.

Full methodology at [`docs/specs/risk-regime-dashboard.md`](./specs/risk-regime-dashboard.md) and [`docs/design/gamma-spx-coverage.md`](./design/gamma-spx-coverage.md).

---

## Breadth

S&P 500 breadth answers a question the index level alone can't: *is this rally broad or narrow?* Two readings carry the load:

- **% above 50-DMA** — the tactical signal. >55 historically marks healthy uptrends; <40 with SPX at highs is the classic narrow-rally warning sign.
- **% above 200-DMA** — the cyclical companion. Tops cleanly when the median name rolls over, even when the index is still being held up by mega-caps.

The daemon also surfaces 52-week new-highs / new-lows counts and the derived `net_new_highs_pct`. The "SPX near highs with net_new_highs_pct near zero or negative" pattern is the most reliable narrow-rally fingerprint.

IBKR doesn't redistribute S&P DJI's official breadth indices on retail subscriptions, so the daemon computes all three locally from the 500 constituent daily closes pulled via IBKR's historical-bar feed (methodology token: `constituent-fanout-50/200dma-hl`). A once-daily post-close refresh (16:35 ET) slides each name's window forward.

**Cold-start budget**: the first request against a fresh daemon takes ~60 minutes — IBKR's historical-data pacing caps the constituent fan-out at ~6 names/min sustained. The response carries `state: "computing"` until done; after cold-start, the cache persists across daemon restarts and every subsequent call is instant.

The constituent list itself is also refreshed runtime — see [Updating](./guides/updating.md#updating-the-sp-500-list--automatic) for the cadence and pinning options. Threshold derivation is left to the consumer; suggestions are in the spec.

S&P 500 only today — NDX, RUT, sector-specific, single-stock breadth are not supported.
