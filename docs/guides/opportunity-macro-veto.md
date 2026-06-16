# Opportunity Macro-Veto Handbook

Updated: 2026-06-16 CEST

This guide explains the first lean strategy variant in the opportunity research
harness: `pullback_uptrend_rs63_macro_veto_v1`. It is diagnostic only. It does
not place orders, preview orders, or turn a passing row into investment advice.

## What It Tests

The plan starts from the existing pullback rule:

- price is above the 200-day moving average
- price is no more than 5% above the 50-day moving average
- 63-day relative strength versus the benchmark is positive
- existing freshness, session, quote-quality, and liquidity gates pass

The macro-veto variant then requires captured regime context. It allows
`normal` and `watch` regime tones, and blocks `stress`, `risk_off`,
`data_quality`, missing macro context, regime RPC errors, or unknown tones.

The question it answers is narrow: does avoiding confirmed broad-market stress
improve the pullback strategy's forward excess returns compared with the plain
pullback rule?

## Capture Candidates

Use `--include-regime` when creating new point-in-time rows. This stamps a
compact `regime.snapshot` read into each row's features before the feature
checksum is computed.

```sh
ibkr backtest capture-opportunity \
  --preset top-movers \
  --include-regime \
  --append research/opportunity-pit.jsonl \
  --json
```

For a hand-picked watchlist, bypass the scanner:

```sh
ibkr backtest capture-opportunity \
  --symbols NVDA,MSFT,AVGO \
  --include-regime \
  --append research/opportunity-pit.jsonl \
  --json
```

For protected holdout collection, pre-register the split:

```sh
ibkr backtest capture-opportunity \
  --symbols NVDA,MSFT,AVGO \
  --include-regime \
  --split holdout \
  --holdout-plan 2026q3-pullback-macro-veto \
  --append research/opportunity-pit.jsonl \
  --json
```

Do not edit feature fields after capture. The backtest gate verifies
`feature_provenance.checksum`, and macro context is part of that checksum when
present.

## Score After The Forward Window

Captured rows are intentionally unscored. After the configured forward window is
observable, export bars and score the PIT ledger.

```sh
ibkr backtest export-opportunity-bars \
  --symbols NVDA,MSFT,AVGO,QQQ \
  --bars research/opportunity-bars.jsonl \
  --bars-manifest research/opportunity-bars.manifest.json \
  --benchmark QQQ \
  --json

ibkr backtest score-opportunity \
  --input research/opportunity-pit.jsonl \
  --bars research/opportunity-bars.jsonl \
  --bars-manifest research/opportunity-bars.manifest.json \
  --target-policy net-excess-positive \
  > research/opportunity-scored.jsonl
```

## Compare The Strategies

Run the plain pullback and macro-veto plans side by side:

```sh
ibkr backtest research-opportunity \
  --input research/opportunity-scored.jsonl \
  --plan pullback_uptrend_rs63_v1,pullback_uptrend_rs63_macro_veto_v1
```

Use JSON when you want stable fields for notes or spreadsheets:

```sh
ibkr backtest research-opportunity \
  --input research/opportunity-scored.jsonl \
  --plan pullback_uptrend_rs63_v1,pullback_uptrend_rs63_macro_veto_v1 \
  --json
```

Prefer the macro-veto only if it improves holdout net excess return, hit rate,
and drawdown behavior without collapsing the number of fired samples. Tuning
lift alone is not enough.

## Reading Reason Tokens

Useful pass/fail tokens:

- `passed_pullback_uptrend_rs63_macro_veto_v1`: the strategy fired
- `macro_context_missing`: row was not captured with regime context
- `macro_context_error`: regime snapshot failed during capture
- `macro_data_quality_veto`: regime context was present but not trustworthy
- `macro_stress_veto`: broad-market stress blocked a new candidate
- `macro_risk_off_veto`: full risk-off context blocked a new candidate

Missing/error/data-quality macro reasons are treated as dirty context, not as
evidence that the strategy is bad. `macro_stress_veto` and
`macro_risk_off_veto` are feature filters: they are the intended conservative
behavior.

## Live Use Today

This build is a measurement tool, not an execution system. For live candidates,
capture rows with `--include-regime`, review the macro tone in the JSON, and
let the rows mature into scored evidence before trusting the strategy.

If several candidates appear on the same day, the current system does not yet
emit a ranked buy list for this plan. Keep ranking manual and simple: prefer
rows with clean live context, non-degraded macro context, stronger RS63,
higher dollar liquidity, and less extension above the 50-day moving average.
The next useful extension would be a read-only candidate scorer that applies
the same plan to captured rows and prints a shortlist, still without order
preview or broker writes.
