# Risk-Regime Feature Review — Expert + Online Cross-Check

**Reviewed:** 2026-05-20 09:45 CEST · **Scope:** `ibkr regime` / `ibkr_regime` and the five
risk-regime indicators (VIX term structure, HYG-SPY divergence, USD/JPY, SPY dealer zero-gamma,
S&P 500 breadth). Methodology spec under review:
[docs/specs/risk-regime-dashboard.md](../specs/risk-regime-dashboard.md). Implementation:
[internal/daemon/regime.go](../../internal/daemon/regime.go),
[internal/daemon/gamma_zero_compute.go](../../internal/daemon/gamma_zero_compute.go),
[internal/breadth/](../../internal/breadth/), [internal/cli/regime.go](../../internal/cli/regime.go).

This is an analysis pass — no code changes were made.

---

## TL;DR — what's solid, what's not

**The math is right.** Black-Scholes gamma matches the textbook formula; the dealer-GEX sign
convention is the canonical Perfiliev recipe verbatim; VIX/VIX3M ratio, HYG-50dma SMA, and the
constituent-fanout breadth method are all methodologically equivalent to the published references.

**The biggest concrete weakness** is **not** in the math. It's in three softer places:

1. **Some of our threshold numbers are ours, not industry-standard** — and a couple are
   calibrated to a pre-2023 market that no longer exists (Mag-7 concentration; structurally lower
   VIX since the 0DTE explosion).
2. **The dealer-gamma "regime hint" framing is increasingly load-bearing.** An 8-year backtest
   (FlashAlpha, 2026) found that once you control for at-the-money implied vol, the standalone
   forecasting power of zero-gamma flips drops sharply. Our spec already calls it a "regime hint,
   not precise level" — that framing is exactly right; we just need to make sure the LLM-facing
   surface carries it loudly.
3. **The LLM-facing tool description (`ibkr_regime` in MCP) buries critical caveats** that the
   spec already worked hard to state — leading an agent consumer to over-interpret partial data
   or skip threshold derivation.

**Bottom line for dealer gamma specifically (the headline question):** the calculation is solid
and matches what every retail-accessible vendor does. The known holes (static sign convention,
end-of-day open interest, sticky implied vol across the sweep) are **shared by every public
implementation including the GitHub clones**. Closing them requires either paid data (OPRA tape
~hundreds/month for intraday open-interest inference) or a vol-surface model we'd need to build
and validate ourselves. The "good enough as a regime hint" framing is defensible; just keep it
honest in the user-facing surface.

---

## 1. Methodology check — is the math right?

Quick acronym primer used throughout (some defined on first use, gathered here for reference):

- **GEX**: Gamma Exposure — dollars of dealer hedging required per 1% move in the underlying.
- **OI**: Open Interest — number of option contracts outstanding (the OCC reports it end-of-day).
- **OPRA**: Options Price Reporting Authority — the U.S. tape that records every option trade
  in real time. Used by paid vendors to estimate intraday OI.
- **IV / ATM IV**: Implied Volatility / At-the-Money IV — the market's expectation of future
  volatility, embedded in option prices.
- **BS**: Black-Scholes, the standard option-pricing model.
- **0DTE**: Zero-Days-To-Expiration — options expiring on the same day they're traded. Grew from
  marginal in 2020 to ~59% of SPX volume in 2025.
- **DTE**: Days To Expiration.
- **Term structure**: the curve of implied volatilities across different expirations.

### 1.1 Dealer zero-gamma compute — methodologically canonical

Our recipe in [internal/daemon/gamma_zero_compute.go](../../internal/daemon/gamma_zero_compute.go)
matches Sergei Perfiliev's blog post ("How to Calculate Gamma Exposure (GEX) and Zero Gamma
Level", Feb 2022, [perfiliev.com](https://perfiliev.com/blog/how-to-calculate-gamma-exposure-and-zero-gamma-level/))
verbatim: same formula `Σ sign(right) × Γ × OI × 100 × spot² × 0.01`, same calls-long /
puts-short sign convention, same sweep approach. Harel Jacobson's public endorsement is
confirmed.

The active GitHub implementations (`jensolson/SPX-Gamma-Exposure`,
`phammings/SPX500-Gamma-Exposure-Calculator`, `Matteo-Ferrara/gex-tracker`,
`VandersonTorres/gamma-exposure-indicator`) all replicate the same formula. None advances it.

The Black-Scholes math in [internal/daemon/blackscholes.go](../../internal/daemon/blackscholes.go)
is textbook: `γ = φ(d1) / (S·σ·√t)`, Newton-Raphson IV solver with Brenner-Subrahmanyam initial
guess and convergence guards (`[0.01, 5.0]` accept bounds, 50 iterations max), put-call parity
conversion before the solve. The vega-collapse and intrinsic-value guards are correct.

The one place the spec language drifts from reality: we describe **sticky-IV across the sweep**
as a "v1 limitation". It's not — Perfiliev's published code does exactly the same thing, and so
do all four open-source GitHub clones. It's the **canonical recipe**, not our deviation. The
limitation is real but is industry-standard, not implementation-specific.

### 1.2 VIX term structure — clean

The `vix / vix3m` ratio is dimensionally clean (both indices use the same generalised
variance-swap formula on the same 30-day vs 3-month expirations). The 1.00 contango/backwardation
boundary is methodologically natural and empirically supported (eco3min.fr: backwardation has
preceded ~100% of major drawdowns since 1990; Volatility Trading Strategies: 21 of 22
backwardation episodes 2004-2025 coincided with >5% drawdown within 30 days). The implementation
in [daemon/regime.go:330-391](../../internal/daemon/regime.go) is sound; the 8-second budget for
VIX3M (vs 5s for VIX) is a thoughtful accommodation for the thinner CBOE index.

The **0.92 yellow** threshold is **our calibration**, not an industry standard. No credible
source publishes a fixed flattening threshold; published references discuss percentiles instead.

### 1.3 HYG vs SPY — mostly clean, one structural concern

The 50-day SMA computation is correct. The 90-calendar-day lookback (`HYGLookbackDays = 90`)
generously covers 50 trading bars even on the holiday-clipped side of Memorial Day / Labor Day /
Thanksgiving — good defensive choice. SPY 52-week-high fallback from 252 daily bars when tick
165 doesn't land is sensible.

The structural concern: **comparing HYG and SPY on price (not total-return) systematically
over-fires.** HYG has a trailing distribution yield around 7.4%; SPY's is around 1.0%. Over a
2-4 week comparison window the dividend differential is 30-60 basis points — a meaningful drag
that biases the comparison. A "HYG breaks 50-day MA while SPY at highs" signal will trigger
more often than it should. The institutional alternative is the **ICE BofA US High Yield Master
II Option-Adjusted Spread** ([FRED BAMLH0A0HYM2](https://fred.stlouisfed.org/series/BAMLH0A0HYM2)),
which strips out duration and coupon noise — but it's a free FRED daily series, not on IBKR.

### 1.4 USD/JPY — clean

The dotted-pair classifier route to `CASH/IDEALPRO/USD/JPY` is the right path, and the
14-calendar-day lookback for the 7-trading-day-ago close has the right holiday slack. The
spec's "2% in 3 days OR 3% in a week" threshold would have caught the Aug-2024 carry unwind:
USD/JPY fell from ~150 to ~144 between Jul-31 and Aug-5 (~4% in 3 sessions). It also would
have flashed yellow during the Jul-11→Jul-31 walk-down phase that preceded the cliff.

### 1.5 Breadth — clean implementation, calibration question

The constituent-fanout-50dma method is mechanically equivalent to S&P DJI's `S5FI`. The local
recompute is necessary because IBKR doesn't redistribute `S5FI` on retail subscriptions (you've
verified this against `reqContractDetails`). The 80% coverage gate and the daily refresh after
16:35 ET are both sensible. The cold-start cost (~60 minutes due to IBKR's 60-req/10-min pacing
limit) is documented honestly and there's a `--foreground` workaround.

The calibration concern is covered in §2.4 below.

---

## 2. Ranked findings — what to consider changing

Five **high-impact**, six **medium**, four **low**. None are "the code is broken"; they're all
"the framing has drifted from current best practice, or our calibration was right in 2022 and
isn't anymore." Each finding ends with a suggested approach (no code).

---

### HIGH

#### H1. The MCP tool description buries the threshold-derivation contract.

**What's wrong.** The spec is explicit that the daemon does **not** derive green/yellow/red
status — that's the renderer's job, and for an LLM consumer that means the LLM's job. The CLI
already does this correctly. But the `ibkr_regime` description in
[internal/mcp/tools.go:267](../../internal/mcp/tools.go) mentions the requirement mid-paragraph
("green / yellow / red derivation is the consumer's job") rather than as a headline. An LLM
agent skimming the tool description could easily forget to apply thresholds and report raw
numbers without context.

**Why it matters.** This is the highest-leverage user-facing surface for the regime feature
(everything else routes through it from Claude Desktop / Claude Code / MCP clients). A subtle
description issue cascades into every consumer.

**Suggested approach.** Rewrite the opening sentence of the `ibkr_regime` description to lead
with the contract ("This tool returns raw measurements + per-row threshold notes; the consumer
must derive green/yellow/red status from the notes — the daemon intentionally does not"). Add
explicit "regime hint, not precise level" caveat for the gamma row in the description. Clarify
that `fields_missing` is a render hint, not an error condition.

---

#### H2. Dealer-gamma standalone signal is weak net of ATM IV.

**What's wrong.** FlashAlpha's 8-year SPY backtest (2026,
[flashalpha.com/articles/gex-dex-vex-chex-8-year-backtest-spy-vix-control](https://flashalpha.com/articles/gex-dex-vex-chex-8-year-backtest-spy-vix-control))
found that GEX has a real raw relationship with next-day realised volatility — but once at-the-money
implied vol is added to the control set, GEX's forecasting power **drops sharply**. In other
words, much of what zero-gamma is "predicting" is already in the VIX number sitting next to it.

**Why it matters.** Our composite counts a gamma red and a VIX red as two independent reds. The
spec already calls this out ("zero-gamma flips correlate ~0.6+ with VIX backwardation") but the
composite doesn't act on it. We're effectively double-counting one of the strongest correlated
pairs in the dashboard.

**Suggested approach.** Two-part. (1) Update the gamma row's `notes` field to include a one-line
caveat that the signal is largely subsumed by VIX/ATM IV when treated standalone — keeps the
honest framing the spec already establishes. (2) When we eventually add a composite-score wire
field (see L3 below), use **weighted reds**: half-weight indicators that are by construction
correlated (gamma ↔ VIX/VIX3M, breadth ↔ HYG/SPY) rather than treating each as orthogonal.

---

#### H3. Some thresholds are calibrated to a pre-Mag-7 market.

**What's wrong.** The breadth red trigger ("< 40% above 50-day MA while SPX within 3% of 52-week
high") fired as a routine event throughout 2024 and parts of 2025 — Capital Group's 2025 piece
shows 27-28% of S&P 500 names beat the index in 2023 and 2024, and the top-10 weight reached
~32% (Mag-7) per First Trust's 2024 review. **40% breadth at index highs became the new median,
not a warning.**

The VIX/VIX3M 1.00 line survives this critique (it's a methodologically natural cutoff at
contango→backwardation). The breadth 40% red line does not.

**Why it matters.** A regime dashboard that fires "red" all the time during normal conditions
loses its informational value — the consumer stops trusting it.

**Suggested approach.** Move the breadth red to `< 30% above 50-DMA while SPX within 3% of
highs`, **or** — preferable — change the trigger from a level to a **change**: "4-week decline
of ≥15 percentage points while SPX < 3% off highs". The change-based rule fits the public
backtests on divergence better and survives regime drift. Either way, the existing fixed-level
rule is now a yellow trigger, not red. Surface the spec change in `breadthNotes`.

---

#### H4. The HYG-vs-SPY comparison should use total return, not price.

**What's wrong.** HYG's ~7.4% distribution yield against SPY's ~1.0% creates a 30-60 basis-point
drag on any price-based comparison over a 2-4 week window. Our "HYG breaks 50-day MA while SPY
at highs" signal therefore fires more often than the underlying credit story warrants — HYG
needs to drop further than SPY does to be saying the same thing.

**Why it matters.** This biases one of the five indicators toward false positives in calm
markets, eroding the dashboard's signal-to-noise.

**Suggested approach.** Two options, roughly equal cost. (a) Switch the 50-DMA computation to a
**total-return** basis for HYG (recompute closes with reinvested distributions). (b) Add the
free FRED daily HY OAS series (`BAMLH0A0HYM2`) as the primary credit gauge and demote the
HYG-vs-SPY price line to a same-day proxy. (b) is the institutional approach (State Street,
Schwab, BIS all use OAS); (a) is fewer dependencies but uglier in the IBKR data path. **Recommend
(b)** if you're willing to take a FRED dependency.

---

#### H5. Some bias claims in the spec lack public citations.

**What's wrong.** The spec asserts: "sticky-IV across the sweep biases `zero_gamma` upward by
~30-80 SPX points typical, more in stress" and "effective precision ≈ ±25 SPX points". Both are
sensible-magnitude assertions, but **neither has a public source**. Practitioners don't publish
this calibration, and the open-source clones don't either.

**Why it matters.** If a sophisticated user (or a code reviewer) traces these numbers, they
won't find a reference. That's fine if we acknowledge it; less fine if it reads as established
fact.

**Suggested approach.** Soften the wording: "consistent with the reported sticky-strike →
sticky-delta rotation observed in stress (e.g., Hull-Daglish-Suo on the 1987-2008 SPX skew);
the magnitude bias on `zero_gamma` is not publicly benchmarked." Then either (a) leave it at
that, or (b) run an internal calibration: log our `zero_gamma` against SpotGamma's free
Friday-recap posts for 4 weeks (the spec already mandates this ritual) and publish the
observed delta distribution. The latter would be a genuinely novel contribution — no one has
published this.

---

### MEDIUM

#### M1. MOVE index is the strongest "indicator we don't have but should".

**Why.** MOVE (Treasury implied volatility) led VIX in the April-2025 hedge-fund basis-trade
stress (Schwab, 2025: "the bond market has a history of pricing macroeconomic and systemic risk
earlier and/or more accurately than the stock market" — MOVE spiked to ~172, VIX caught up
later). The Society of Actuaries' Sep-2025 asset-allocation paper formalises MOVE-alongside-VIX
as a regime input. MOVE is freely available and would be a clean ETF/index quote via IBKR
(BlackRock licenses it as ticker `^MOVE` / cross-market via several brokers).

**Suggested approach.** Add as a 6th indicator with a fixed yellow/red band based on its
historical distribution (MOVE > 130 has historically marked stress regimes). The data path is
identical to VIX/VIX3M.

#### M2. Add 200-day breadth ($SPXA200R) as a slow companion.

**Why.** Our 50-day breadth catches tactical (weeks-to-month) regime changes. 200-day breadth
caught the 1999 and 2021 cyclical tops cleanly. Cost: zero — we already pull every constituent's
daily closes for the 50-day SMA; computing the 200-day SMA in the same pass is free. Just need
~150 more days of history at cold-start time (a one-time cost; the steady-state refresh is the
same).

**Suggested approach.** Compute and expose `pct_above_50dma` and `pct_above_200dma` on the same
breadth envelope. Apply different bands per the public backtests (BPSPX-style: 70/30 for
200-day; current 55/40 — or revised — for 50-day). A single composite that combines fast and
slow breadth is more informative than either alone.

#### M3. Tighten USD/JPY yellow to "1% in 3 days".

**Why.** Our current "1-2% in a week" yellow caught the cliff (Jul-31 to Aug-5 2024, ~4% in 3
sessions = red) but **the walk-down phase from Jul-11 onward** was missed by yellow because the
weekly move only crossed 1-2% mid-period. Tightening the yellow to a 3-day window would catch
the pre-cliff drift.

**Suggested approach.** Change yellow trigger to "USD/JPY falls 1-2% in 3 days OR 2-3% in a
week". Keep red unchanged.

#### M4. Surface `composite_score` on the wire envelope.

**Why.** The CLI computes a verdict from the per-row tally; an LLM consumer must do the same
manually after parsing notes for each row. That's both expensive and error-prone. The work is
already done in [regime.go:353-430](../../internal/cli/regime.go) — promoting it to the wire
envelope is a structural simplification, not a feature add.

**Suggested approach.** Add a `composite` block to the envelope with `reds`, `yellows`,
`greens`, `ranked`, `unranked`, and `verdict` (text). Make the verdict text the same one the CLI
already produces. Mark it `derived` quality so consumers understand it's the daemon's
interpretation, not raw data.

#### M5. Sticky-IV / sign-convention should also surface on the gamma envelope's `notes`.

**Why.** The CLI's `--explain` mode renders these caveats. The MCP envelope does not. An LLM
client doesn't have an `--explain` flag — it gets what's on the wire.

**Suggested approach.** Append a one-line caveat to the gamma row's notes: "Calculation assumes
2018-era dealer positioning (long calls, short puts); covered-call ETF flow and autocallable
barriers can invert local sign — see `gamma_total_abs` and `top_strikes` for the sign-agnostic
view. Implied vol held fixed across the spot sweep (canonical sticky-IV recipe; biases zero
estimate ~30-80 SPX points)."

#### M6. SpotGamma's "Volatility Trigger" ≠ our `zero_gamma`.

**Why.** Users (especially LLM consumers reading SpotGamma posts) will conflate these. SpotGamma's
Vol Trigger is a heuristic level *above* zero where positive-gamma support concentrates; our
`zero_gamma` is the actual flip level. They're related but not equivalent.

**Suggested approach.** One-line clarification in the gamma row notes: "Comparable to
SpotGamma's 'Gamma Flip' level. Not equivalent to SpotGamma's 'Volatility Trigger', which is a
related-but-distinct support heuristic."

---

### LOW

#### L1. Default CLI omits thresholds; require `--explain` to see them.

A first-time CLI user sees five rows of numbers with no methodology unless they add `--explain`
([regime.go:199-204](../../internal/cli/regime.go)). The JSON path carries them. Minor;
documenting that `--explain` is the way to get methodology disclosure would be enough.

#### L2. Spec link not in default CLI render.

The `spec_doc` field is on the envelope but `--explain` is the only path to surface it. A small
footer line ("Methodology: docs/specs/risk-regime-dashboard.md") in default mode would close
this without bloating the output.

#### L3. Calibration ritual has no CLI surface.

The spec mandates a 4-week SpotGamma cross-check post-launch. There's no tool to log a snapshot
at a moment or replay prior snapshots. An `ibkr regime --log /path/to/cal.jsonl` flag that
appends today's snapshot to a JSONL file would close this and give us the H5 backtest data path
for free.

#### L4. Breadth history isn't bubbled through regime rows.

`ibkr breadth` returns a ~30-day history series; `ibkr regime` doesn't surface it on the breadth
row. The spec calls for a sparkline; not having the data on the regime envelope means a renderer
must call both endpoints. Not a correctness issue, just a UX seam.

---

## 3. What we do not have (but every public alternative does or doesn't)

Survey of public 2026 retail-grade regime dashboards (SpotGamma, All Star Charts, SentimenTrader,
StockCharts, Capital Group). What they include that we don't, ranked by ROI for us:

- **Sector breadth** (per-GICS-sector % above 50-day, 11 numbers): cheap drill-down from data
  we already pull. All Star Charts has it; we don't. **High ROI.**
- **Net new 52-week highs minus lows** of S&P 500 constituents: also free from our daily
  closes cache. Caught the September-2025 "4.6% of names at 52-week highs with SPX at all-time
  highs" textbook narrow-breadth read. **High ROI.**
- **20-day breadth** ($SPXA20R) — short-term oversold/bounce signal. Cheap, but noisier than
  50/200; lower ROI than the two above.
- **DEX (delta exposure)**: aggregate dealer share-equivalent hedge — gives directional bias.
  Computable from the same chain pull we already do for gamma. **Medium ROI** (FlashAlpha
  found incremental signal modest net of GEX/VIX).

Things they have that we don't, **lower ROI** by my read:
- **Goldman/BofA composite scores** (multi-input, percentile-ranked): institutional appeal but
  our 5-indicator dashboard already exposes the underlying ingredients.
- **CDX HY (synthetic credit)**: institutional standard but not available via IBKR.
- **Lowry Buying Power / Selling Pressure**: proprietary, would need a license.

What they get wrong that we got right:
- **Single-axis dashboards** (SpotGamma's vol-trigger framing) miss credit and FX regime shifts.
  Our five-indicator panel is the right shape.
- **Anchored thresholds without regime conditioning**: most free dashboards still use fixed
  cutoffs (e.g., 70/30 on breadth). Our spec at least conditions the breadth red on "SPX within
  3% of highs" — closer to a divergence-aware rule than the typical retail tool.

---

## 4. Prioritized fix list

Top three for immediate action, in order:

1. **H1**: Rewrite `ibkr_regime` MCP description — lead with the threshold-derivation contract,
   add gamma "regime hint" caveat, clarify `fields_missing` semantics. *One file, no behaviour
   change.*
2. **H3 + H4**: Recalibrate the breadth red threshold (or move to a change-based trigger) AND
   either fix HYG/SPY to total-return basis or add the FRED HY OAS series. *These two together
   cure the largest sources of false positives in calm markets.*
3. **M4**: Surface `composite_score` on the wire envelope. *Promotion of existing CLI logic to
   the wire; structural cleanup.*

After that: H2 + M5 (add the dealer-gamma caveats to the gamma row notes) and M1 (add MOVE).

H5 (sticky-IV bias calibration) is genuinely novel work — no public source has done it. It would
make a nice changelog item, but it's not blocking.

---

## 5. Sources

### Dealer gamma / GEX methodology
- Sergei Perfiliev, "How to Calculate Gamma Exposure (GEX) and Zero Gamma Level", Feb 2022:
  [perfiliev.com/blog/how-to-calculate-gamma-exposure-and-zero-gamma-level](https://perfiliev.com/blog/how-to-calculate-gamma-exposure-and-zero-gamma-level/)
- Harel Jacobson, LinkedIn endorsement of Perfiliev recipe, Feb 2022:
  [linkedin.com/posts/harel-jacobson-b3040b6_how-to-calculate-gamma-exposure-and-zero-activity-6895375618490646528-01cH](https://www.linkedin.com/posts/harel-jacobson-b3040b6_how-to-calculate-gamma-exposure-and-zero-activity-6895375618490646528-01cH)
- SpotGamma, "GEX Gamma Exposure Explained":
  [support.spotgamma.com/hc/en-us/articles/15214161607827](https://support.spotgamma.com/hc/en-us/articles/15214161607827-GEX-Gamma-Exposure-Explained-What-It-Is-and-How-SpotGamma-Uses-It)
- SpotGamma, "Volatility Trigger Zero Gamma Trading":
  [spotgamma.com/volatility-trigger-zero-gamma-trading](https://spotgamma.com/volatility-trigger-zero-gamma-trading/)
- SqueezeMetrics white paper (DDOI methodology):
  squeezemetrics.com/download/white_paper.pdf
- FlashAlpha, "GEX/DEX/VEX/CHEX 8-year Backtest", 2026:
  [flashalpha.com/articles/gex-dex-vex-chex-8-year-backtest-spy-vix-control](https://flashalpha.com/articles/gex-dex-vex-chex-8-year-backtest-spy-vix-control)
- Barbon & Buraschi, "Gamma Fragility", SSRN 3725454, Nov 2020 / Mar 2021:
  [abarbon.com/assets/Barbon_Buraschi_2021_Gamma_Fragility.pdf](https://www.abarbon.com/assets/Barbon_Buraschi_2021_Gamma_Fragility.pdf)
- Dim, Eraker & Vilkov, "0DTEs: Trading, Gamma Risk and Volatility Propagation", SSRN 2024:
  [papers.ssrn.com/sol3/Delivery.cfm/4692190.pdf](https://papers.ssrn.com/sol3/Delivery.cfm/4692190.pdf?abstractid=4692190)
- Cboe, "0DTE Index Options and Market Volatility" research, 2024:
  [cdn.cboe.com/resources/education/research_publications/gammasqueezes.pdf](https://cdn.cboe.com/resources/education/research_publications/gammasqueezes.pdf)
- MenthorQ, "Gamma & Dealer Hedging Guide":
  [menthorq.com/guide/gamma-and-dealer-hedging](https://menthorq.com/guide/gamma-and-dealer-hedging/)
- Risk.net, "Casualties of the smile", April 2008 (sticky-strike vs sticky-delta):
  [risk.net/sites/default/files/import_unmanaged/db.riskwaters.com/data/risknet/pdf/2008/026-027_Risk_0408.pdf](https://www.risk.net/sites/default/files/import_unmanaged/db.riskwaters.com/data/risknet/pdf/2008/026-027_Risk_0408.pdf)
- Cheddar Flow, "What is Gamma Exposure: An In-Depth Analysis for Traders", 2024:
  [cheddarflow.com/blog/what-is-gamma-exposure-an-in-depth-analysis-for-traders](https://www.cheddarflow.com/blog/what-is-gamma-exposure-an-in-depth-analysis-for-traders/)

### VIX, HYG, USD/JPY
- CBOE, VIX Term Structure: [cboe.com/tradable-products/vix/term-structure](https://www.cboe.com/tradable-products/vix/term-structure/)
- Macroption, "VIX3M Explained": [macroption.com/vix3m](https://www.macroption.com/vix3m/)
- Eco3min, "VIX backwardation, contango and term structure":
  [eco3min.fr/en/vix-backwardation-contango-volatility-term-structure](https://eco3min.fr/en/vix-backwardation-contango-volatility-term-structure/)
- Volatility Trading Strategies, "VIX9D:VIX:VIX3M:VIX6M:VIX1Y":
  [volatilitytradingstrategies.com/blog/vix9d-vix-vix3m-vix6m-vix1y-fast-medium-slow-long-crossovers](https://www.volatilitytradingstrategies.com/blog/vix9d-vix-vix3m-vix6m-vix1y-fast-medium-slow-long-crossovers)
- Northern Trust, Q4 2025 Options Commentary:
  [northerntrust.com/insights-research/asset-servicing/a-suite/insights-hub/options-quarterly-commentary-q4-2025](https://www.northerntrust.com/insights-research/asset-servicing/a-suite/insights-hub/options-quarterly-commentary-q4-2025)
- Schwab, "What's the MOVE Index", 2025:
  [schwab.com/learn/story/whats-move-index-and-why-it-might-matter](https://www.schwab.com/learn/story/whats-move-index-and-why-it-might-matter)
- Society of Actuaries, "Using Bond and Equity Volatility Indices", Sep 2025:
  [soa.org/sections/investment/investment-newsletter/2025/september/rr-2025-09-bitalvo](https://www.soa.org/sections/investment/investment-newsletter/2025/september/rr-2025-09-bitalvo/)
- SpotGamma, "VVIX Explained":
  [spotgamma.com/vvix-explained-what-the-volatility-index-tells-traders](https://spotgamma.com/vvix-explained-what-the-volatility-index-tells-traders/)
- FRED, ICE BofA US High Yield Master II OAS (BAMLH0A0HYM2):
  [fred.stlouisfed.org/series/BAMLH0A0HYM2](https://fred.stlouisfed.org/series/BAMLH0A0HYM2)
- State Street, "Credit spreads signal confidence and risk", Nov 24 2025:
  [ssga.com/us/en/institutional/insights/mind-on-the-market-24-november-2025](https://www.ssga.com/us/en/institutional/insights/mind-on-the-market-24-november-2025)
- Schwab, "Credit Spreads: Under the Radar but Influential":
  [schwab.com/learn/story/credit-spreads-under-radar-but-influential](https://www.schwab.com/learn/story/credit-spreads-under-radar-but-influential)
- BIS Bulletin No 90, "The market turbulence and carry trade unwind of Aug 2024", Oct 2024:
  [bis.org/publ/bisbull90.pdf](https://www.bis.org/publ/bisbull90.pdf)
- AMRO Analytical Note on Aug-2024 carry unwind, Dec 19 2024:
  [amro-asia.org/wp-content/uploads/2024/12/20241219-Analytical_Note_Carry_Trade.pdf](https://amro-asia.org/wp-content/uploads/2024/12/20241219-Analytical_Note_Carry_Trade.pdf)
- Wellington Management, "Yen Carry Trade Unwind":
  [wellington.com/en/insights/the-yen-carry-trade-unwind](https://www.wellington.com/en/insights/the-yen-carry-trade-unwind)

### Breadth + composite frameworks
- StockCharts ChartSchool, "Percent Above Moving Average":
  [chartschool.stockcharts.com/table-of-contents/market-indicators/percent-above-moving-average](https://chartschool.stockcharts.com/table-of-contents/market-indicators/percent-above-moving-average)
- StockCharts ChartSchool, "Bullish Percent Index":
  [chartschool.stockcharts.com/table-of-contents/market-indicators/bullish-percent-index-bpi](https://chartschool.stockcharts.com/table-of-contents/market-indicators/bullish-percent-index-bpi)
- Cedric Thompson on StockCharts, "Can Market Breadth Help Identify S&P 500 Turning Points?", Apr 24 2026:
  [articles.stockcharts.com/article/can-market-breadth-help-identify-s-p-500-turning-points](https://articles.stockcharts.com/article/can-market-breadth-help-identify-s-p-500-turning-points/)
- Capital Group, "Fresh Breadth: Market Concentration in 3 Charts", 2025:
  [capitalgroup.com/institutional/insights/articles/fresh-breadth-market-concentration-3-charts](https://www.capitalgroup.com/institutional/insights/articles/fresh-breadth-market-concentration-3-charts.html)
- First Trust, "The S&P 500 in 2024: A Market Driven Once Again by the Mag 7":
  [ftportfolios.com/Commentary/EconomicResearch/2025/1/8/the-sp-500-index-in-2024-a-market-driven-once-again-by-the-mag-7](https://www.ftportfolios.com/Commentary/EconomicResearch/2025/1/8/the-sp-500-index-in-2024-a-market-driven-once-again-by-the-mag-7)
- Hollo, Kremer, Lo Duca (ECB CISS), ECB working paper 1426, Mar 2012:
  [ecb.europa.eu/pub/pdf/scpwps/ecbwp1426.pdf](https://www.ecb.europa.eu/pub/pdf/scpwps/ecbwp1426.pdf)
- Chicago Fed, NFCI About:
  [chicagofed.org/research/data/nfci/about](https://www.chicagofed.org/research/data/nfci/about)
- Goldman Sachs Research, "Bear Repair: The Bumpy Road to Recovery" (Bull/Bear indicator):
  [goldmansachs.com/intelligence/pages/bear-repair-the-bumpy-road-to-recovery.html](https://www.goldmansachs.com/intelligence/pages/bear-repair-the-bumpy-road-to-recovery.html)
- Thesis Rationale, "Market Breadth: The Indicator Most Investors Ignore", May 19 2026:
  [thesisrationale.substack.com/p/market-breadth-the-indicator-most](https://thesisrationale.substack.com/p/market-breadth-the-indicator-most)
- ScienceDirect, "Market broadening and future volatility", 2025:
  [sciencedirect.com/science/article/pii/S1062940825000099](https://www.sciencedirect.com/science/article/pii/S1062940825000099)
- All Star Charts, "The Best Of Our New Breadth Chartbook":
  [allstarcharts.com/breadth-chart-update](https://allstarcharts.com/breadth-chart-update/)
