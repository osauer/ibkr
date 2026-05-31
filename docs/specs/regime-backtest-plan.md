# Regime and Canary Backtest Plan

**Updated:** 2026-05-31 11:34 CEST

This plan defines the calibration work required before `ibkr regime` or
`ibkr canary` may expose forecast probabilities. Until then, regime should
continue to report evidence balance, risk score, confidence, bands, and scoped
warnings only; canary should continue to report monitor state, planner
readiness, and data-quality blocks without pretending to forecast markets.

The minimal implementation path is two-stage:

1. Backtest regime as a market-state classifier.
2. Replay canary over point-in-time regime rows plus synthetic portfolio
   overlays.

The stages are related but not interchangeable. Regime answers "what is the
market state?" Canary answers "given this portfolio and market state, should a
scheduled monitor stay quiet, watch, act, or block on data quality?"

## Historical Inputs

Build a daily panel with one row per U.S. trading day, using only data that
would have been observable at that day's decision time.

- VIX and VIX3M closes, plus any available intraday/frozen labels if the study
  evaluates intraday reads.
- Cboe VVIX official close and publication availability date.
- HYG and SPY daily OHLC, HYG 50-DMA, SPY 52-week high, and HYG/SPY divergence
  streak state.
- FRED/ICE BofA HY OAS and IG OAS vintages, including release dates and any
  source revisions.
- FRED CP 90-day AA financial and 3-month T-bill vintages, including release
  dates and revisions.
- USD/JPY daily midpoint/close, 7-trading-day change, and missing-market days.
- Dealer gamma snapshots, with method version, input option universe, data
  coverage, warning details, and cache timestamp.
- SPX breadth snapshots, with constituent universe as of that day, coverage,
  50/200-DMA percentages, new-high/new-low counts, and cache timestamp.
- Candidate future input: ICE BofA MOVE official daily or intraday series from
  a licensed point-in-time source. Keep it outside the production feature set
  until the live sourcing question is solved; never fill it with ETF/futures
  proxies.

## Canary Replay Inputs

Canary backtests should consume the same live contracts the monitor uses:

- `account`: `rpc.AccountResult`, including current and look-ahead margin
  cushion, net liquidation, gross exposure, and daily P&L when available.
- `positions`: `rpc.PositionsResult`, especially `portfolio.exposure_base`,
  dollar delta, gross dollar delta, option greeks coverage, and gamma.
- `regime`: `rpc.RegimeSnapshotResult`, compacted to the fields visible in
  `ibkr regime --json`; row bands, statuses, warning details, data quality, and
  composite cluster counts must be present or derived consistently from row
  bands.
- `target`: labelled forward stress window for evaluation, not for model input.

The first committed harnesses are intentionally small:

```bash
ibkr backtest canary --input internal/cli/testdata/canary_backtest_sample.jsonl
ibkr backtest regime --input internal/cli/testdata/regime_backtest_sample.jsonl
```

Each JSONL line is one point-in-time observation. The runner calls the pure
`ComputeCanary` function for canary rows and scores compact
`rpc.RegimeSnapshotResult` rows directly for regime rows, so neither harness
requires TWS, the daemon, or market-data entitlements.

## Target Stress Definitions

Test several targets rather than tune to one story:

- Forward SPX drawdown: max close-to-close drawdown over 5, 10, and 20 trading
  days exceeding 3%, 5%, and 8%.
- Volatility shock: VIX close or intraday high crossing 25/30, or VIX 5-day
  change exceeding a fixed percentile of its trailing distribution.
- Credit stress: HY OAS widening by at least 50 bp or 100 bp over the next 20
  observations.
- Cross-asset stress: any two of SPX drawdown, VIX shock, HY OAS widening, or
  USD/JPY yen-strengthening shock occur inside the target window. MOVE can be
  evaluated as a candidate target only after a licensed point-in-time source is
  available.
- Portfolio stress: margin-danger windows, single-name squeeze exposure,
  option-greeks/negative-gamma fragility, and concentration shocks that may
  happen while broad SPY/VIX regime remains calm.

Targets must be evaluated from the timestamp at which the dashboard would have
been read; do not let later official revisions change the historical feature
row used for that read.

## Market Behavior Clusters

Report every metric by market-behavior cluster so the canary is not calibrated
to one decade's dominant microstructure:

- 2016-early 2018 low-vol / short-vol carry and the February 2018 Volmageddon
  break.
- Q4 2018 Fed-tightening / liquidity shock.
- March-April 2020 COVID crash and fast policy rebound.
- 2020-2021 retail, Reddit, meme-stock, short-squeeze, and options-flow
  participation. These can be single-name portfolio events with calm SPY/VIX.
- 2022 inflation/rates bear market, where equity and bond stress can persist
  without a COVID-style volatility profile.
- 2023 banking/funding stress and later soft-landing rally.
- 2023-2026 AI mega-cap concentration and narrow breadth.
- August 2024 yen carry unwind, where the stress impulse was fast and then
  stabilized quickly.
- 2025-2026 elevated valuation, crowded leverage, and AI sentiment risk.

## Walk-Forward Calibration

Use expanding or rolling walk-forward windows:

- Fit thresholds/weights on an initial window, for example 2006-2015.
- Validate on the next block, then roll forward yearly or quarterly.
- Freeze all threshold decisions before scoring the next out-of-sample block.
- Report discrimination (AUROC/PR), calibration (Brier/reliability only if a
  probability model is actually fitted), false-alarm rate, missed-stress rate,
  and average lead time.
- Compare simple baselines: VIX-only, VIX/VIX3M-only, equal-weight row count,
  cluster worst-band count, and any proposed cluster weighting.

## Look-Ahead Controls

- Use point-in-time FRED vintages; when unavailable, lag official series by a
  conservative publication delay and label that assumption.
- Use only constituent membership known on the date for breadth.
- Recompute rolling windows from data available up to the read timestamp.
- Treat gamma as absent before the method existed unless historical option
  chains, OI, IV, and the exact method version can be reconstructed.
- Keep source outages and entitlement gaps as `unavailable`; do not forward-fill
  a missing critical row into a fake green/yellow/red reading.

## Missing Rows and Revisions

Score each historical row with the same contract as live JSON:

- `status`
- `band`
- `as_of`
- `source`
- `warning_details`
- row-level confidence/freshness

Evaluate both the predictive signal and the coverage signal. A model that
looks good only after dropping unavailable gamma/breadth/official-file days is
not a valid live dashboard model.

## Cluster Weight Evaluation

Test cluster logic separately from row thresholds:

- Worst-band cluster tally, current production behavior.
- Equal-weight rows, to show the over-counting baseline.
- Equal-weight clusters.
- Learned cluster weights with shrinkage and strict walk-forward validation.
- Red-dominance rule: any red cluster forces at least a "Stress signal present"
  label, regardless of green row count.

Promote a learned weight only if it improves out-of-sample utility without
making stale/unavailable critical rows look safer than they are.

## Regime Scoring

Score regime before canary tuning:

- Watch recall: any `Elevated stress watch`, `Stress signal present`, or worse
  before labelled market stress.
- Red-cluster stress precision and recall: any red cluster with sufficient
  ranked coverage before labelled market stress.
- False-alarm rate on labelled non-stress market windows.
- Miss rate on labelled market-stress windows.
- Average lead time when the labelled window carries `days_to_stress`.
- Coverage and data-quality watches: insufficient ranked clusters, stale rows,
  computing rows, unavailable rows, degraded gamma, and warning details.

Rows with `target.scope` outside broad market or cross-asset stress, for
example single-name meme squeezes or portfolio-only concentration shocks, are
reported as out-of-scope for regime precision/recall. They still belong in the
panel so post-2020 retail/crowding behavior is visible, but they should be
owned by canary/portfolio logic rather than counted as regime misses.

## Canary Scoring

Score canary separately from regime:

- Severity distribution: observe / watch / act / urgent.
- Defensive watch recall: any defensive or mixed canary state at `watch` or
  above against labelled stress windows.
- Defensive act precision/recall: `act` and `urgent` are severity filters, not
  the only success gate.
- False defensive watches and false defensive acts on non-stress windows.
- Data-quality watches and blocked planner states, tracked separately from
  false positives.
- Primary drivers and rows that caused the alert.
- Average lead time when the labelled window carries `days_to_stress`.

Use synthetic portfolio overlays to isolate policy behavior:

- 1x diversified SPY/QQQ style exposure.
- Levered broad beta.
- Mag7 / AI mega-cap concentration.
- Meme / retail squeeze concentration.
- Negative-gamma or options-heavy book.
- Diversified low-beta book.
- EU/FX-exposed book.

Backtest output should be read as "how the monitor behaves," not as "how the
market is predicted."

Opportunity posture is not scored in the first regime pass. Opportunity needs a
separate target definition, for example forward rebound or volatility crush
conditional on clean risk budget and deployable portfolio capacity. Until that
exists, the harness should log constructive/opportunity posture but not tune
against it.

## Weaknesses To Watch

This pass should surface these issues but not fix them:

- Regime thresholds remain heuristic and pending backtest.
- Hand-built regime fixtures can lie unless composite cluster counts match row
  bands; the canary reads both.
- Historical gamma is fragile because method versions, OI/IV availability,
  0DTE growth, covered-call/autocall flow, and sign conventions changed.
- Breadth can mean different things in broad bear markets versus narrow
  mega-cap rallies.
- Meme/retail squeezes may bypass broad-market regime and only show up through
  portfolio concentration.
- Fingerprints are alert identities, not historical datasets; raw classified
  rows must be stored separately.

## Documentation Cleanup

Keep this document as the canonical source for both regime and canary backtest
methodology. Later, retire or collapse duplicate canary/regime prose in
agentic-use guides, MCP marketing pages, and older design docs into short
links here. Generated `.html` companions remain generated artifacts, not source
documents.

## Current Recommendation

Keep the present threshold labels marked `heuristic: true` and
`pending_backtest: true`. Do not add forecast probabilities.

The next useful implementation step after the minimal canary and regime replay
harnesses is a point-in-time data builder that writes one compact daily JSONL
row matching the live `ibkr regime --json` shape, followed by a strict
walk-forward scorer for the targets above.
