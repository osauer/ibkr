# Regime Backtest Plan

**Updated:** 2026-05-24 12:50 CEST

This plan defines the calibration work required before `ibkr regime` may expose
forecast probabilities. Until then, the dashboard should continue to report
evidence balance, risk score, confidence, bands, and scoped warnings only.

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

Targets must be evaluated from the timestamp at which the dashboard would have
been read; do not let later official revisions change the historical feature
row used for that read.

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

## Current Recommendation

Keep the present threshold labels marked `heuristic: true` and
`pending_backtest: true`. Do not add forecast probabilities. The next useful
implementation step is a point-in-time data builder that writes one compact
daily JSONL row matching the live `ibkr regime --json` shape, followed by a
separate notebook/script that evaluates the targets above.
