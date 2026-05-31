# Regime and Canary Backtest Runbook

**Updated:** 2026-05-31 19:24 CEST

This is the single umbrella for proving and tuning `ibkr regime` and
`ibkr canary`. Keep the work here. Do not add another experiment plan, tuning
plan, or backtest framework unless this runbook says to.

The goal is simple: prove that regime and canary produce useful stress
detection without overfitting to named events.

## Plain Definitions

- A panel is a JSONL table: one row per date, using only information that would
  have been known on that date.
- A target label is for scoring only. It must not feed the signal.
- `regime` answers: what is the broad market state?
- `canary` answers: given account, positions, and regime, should the monitor
  stay quiet, watch, act, rebalance, or block on data quality?
- Portfolio-only stress belongs to canary. Regime can keep those rows for
  context, but they are out-of-scope for market-regime precision and recall.

## Artifact Map

All backtest artifacts live in one of these places:

| Path | Purpose |
| --- | --- |
| `docs/specs/regime-backtest-plan.md` | This runbook: sequencing, gates, stop rules, and current backlog. |
| `docs/specs/risk-regime-dashboard.md` | Product contract for live `ibkr regime`; not a tuning backlog. |
| `scripts/backtest/` | Reproducible data-build and comparison scripts only. |
| `internal/cli/testdata/backtest_sources*.jsonl` | Source ledgers: URLs, checksums, gaps, and retrieval status. |
| `internal/cli/testdata/regime_pit_panel*.jsonl` | Point-in-time market rows consumed by `build-regime`. |
| `internal/cli/testdata/regime_backtest*.jsonl` | Compact regime replay rows consumed by `backtest regime`. |
| `internal/cli/testdata/canary_backtest*.jsonl` | Canary replay rows with account and position overlays. |

Do not hand-edit generated compact regime rows when the point-in-time panel is
the source of truth. Rebuild them.

## Command Sequence

Small smoke fixtures:

```bash
ibkr backtest regime --input internal/cli/testdata/regime_backtest_sample.jsonl
ibkr backtest canary --input internal/cli/testdata/canary_backtest_sample.jsonl
```

Curated sourced fixtures:

```bash
ibkr backtest regime --input internal/cli/testdata/regime_backtest_sourced_tuning.jsonl
ibkr backtest canary --input internal/cli/testdata/canary_backtest_sourced_tuning.jsonl
ibkr backtest regime --input internal/cli/testdata/regime_backtest_sourced_holdout.jsonl
ibkr backtest canary --input internal/cli/testdata/canary_backtest_sourced_holdout.jsonl
```

Point-in-time regime builder:

```bash
ibkr backtest build-regime --input internal/cli/testdata/regime_pit_panel_sample.jsonl \
  > /tmp/regime_backtest_rows.jsonl
ibkr backtest regime --input /tmp/regime_backtest_rows.jsonl
```

That is two passes only when starting from raw point-in-time market rows. If the
input is already compact regime JSONL, run `ibkr backtest regime` directly.

Tier 1 expanded panel:

```bash
python3 scripts/backtest/build-tier1-regime-panel.py --no-fetch
ibkr backtest build-regime --input internal/cli/testdata/regime_pit_panel_tier1.jsonl \
  > internal/cli/testdata/regime_backtest_tier1.jsonl
ibkr backtest regime --input internal/cli/testdata/regime_backtest_tier1.jsonl
python3 scripts/backtest/compare-tier1-vol-rules.py
```

## Data Tiers

Tier 0: smoke fixtures.

- Purpose: keep CLI contracts stable.
- Gate: tiny samples continue to run and render.

Tier 1: expanded volatility/calm/event panel.

- Sources: Cboe VIX/VIX3M/VVIX, Nasdaq ETF OHLC, FRED funding/FX/credit where
  available.
- Current artifact: `regime_pit_panel_tier1.jsonl`.
- Source ledger: `backtest_sources_tier1.jsonl`.
- Primary label: 5-session market stress.
- Secondary feature: 20-session drawdown for early-warning analysis.
- Known gap: gamma and breadth are explicitly unavailable.

Tier 2: confirmation proxy panel.

- Purpose: test whether noisy isolated red volatility can be confirmed or
  downgraded without losing major stress events.
- Allowed sources: reproducible public or IBKR/Nasdaq/FRED daily data.
- Candidate proxies: `RSP/SPY`, `IWM/SPY`, `QQQE/QQQ` or `QQQ/SPY`,
  `HYG/LQD`, `HYG/IEF`, `LQD/TLT`, `TLT/IEF`, `SHY/IEF`, and FRED rates/curve
  series.
- Label these as proxies. They are not official S&P 500 breadth, official MOVE,
  or reconstructed gamma.
- `LQD/TLT` is context-only for now because it mixes credit spread, duration,
  and rates effects. Do not use it as an active confirmation input.

## Current Findings

- Curated sourced regime holdout still has good recall and only a few false
  positives.
- Curated canary holdout catches labelled stress at watch level.
- Tier 1 exposes the broader problem: current `any red cluster` stress signals
  catch stress rows but fire too often in non-stress volatility regimes.
- A pure confirmation rule cuts false alarms but gives up too much recall.
- Therefore the next tuning target is narrow: isolated red volatility.
- Tier 2 source access is usable for a bounded proxy pass. The current build
  fetched all required Nasdaq ETF histories and recorded checksums. The first
  14 rows have unavailable proxy windows because the 20-session lookback is not
  mature yet.
- Tier 2 stress-label scoring moves the current holdout baseline out of the
  10.8% Tier 1 forward-label noise zone: current `any red cluster` is 34.2%
  precision and 69.1% recall on the 2024+ observable-stress target.
- The best tested Tier 2 confirmation rule improves holdout stress precision to
  45.1% and cuts false alarms from 17.2% to 9.2%, but recall falls to 58.2%.
  This is a promising candidate, not yet a production rule.

## Next Pass

Run this sequence and stop at the first failed gate:

1. Validate Tier 2 proxy sources and record them in the source ledger.
2. Build a Tier 2 point-in-time panel by extending Tier 1 with confirmation
   proxy features.
3. Split labels into:
   - `watch`: early warning / elevated risk.
   - `stress`: observable market damage or strongly confirmed broad stress.
4. Compare exactly three stress-signal rules:
   - current: any red cluster is stress.
   - confirmation-only: isolated red volatility is not stress.
   - severity split: isolated red volatility is watch unless severity or
     independent confirmation is strong enough.
5. Tune only the severity split, and only on the tuning split.
6. Score holdout once the tuning behavior is stable.

Current candidate to continue evaluating:

- Keep red-cluster watch behavior visible.
- Count a red-cluster stress signal only when there is current tape damage,
  severe volatility, or an active Tier 2 proxy group confirming stress.
- Do not apply this directly to live `ibkr regime` until the live equivalent is
  explicit. Tier 2 proxy groups are backtest features unless promoted into the
  live contract.

## Data Gates

Tier 2 data is green only if all are true:

- Every proxy has a reproducible source, retrieval status, and checksum.
- Missing data stays unavailable; no fabricated green/yellow/red values.
- The source ledger names every source gap plainly.
- Gamma remains excluded unless a method-stamped point-in-time source exists.
- Official S&P 500 breadth and MOVE are excluded unless a clean licensed or
  public source is proven.
- The point-in-time panel can rebuild the compact replay file deterministically.

If these fail, do not tune. Fix data or stop.

## Tuning Gates

A tuning change is allowed only if all are true:

- Watch recall remains high on major broad-market stress events.
- Stress precision materially beats the Tier 1 holdout baseline of 10.8%.
- Stress recall does not collapse on holdout.
- Major events are not hidden: Volmageddon, COVID, 2022 bear-market stress,
  yen carry unwind, and tariff shock remain visible at least at watch level.
- Calm/rally controls get quieter or the remaining false alarms are explainable.
- Portfolio-only stress is evaluated by canary, not counted as regime failure.
- Data-quality warnings are separate from stress false positives.

If Tier 2 cannot materially improve precision without destroying recall, stop
tuning and revisit the product definition. Do not add more indicators just to
force convergence.

## Verification Gates

Before calling a pass done:

```bash
go test ./...
make check
make smoke
```

After CLI or daemon changes, also install and smoke the actual binary:

```bash
pkill -f 'ibkr daemon' || true
make install
ibkr status
ibkr backtest build-regime --input internal/cli/testdata/regime_pit_panel_sample.jsonl \
  > /tmp/ibkr-build-regime-smoke.jsonl
ibkr backtest regime --input /tmp/ibkr-build-regime-smoke.jsonl
```

## Not Doing

- No forecast probabilities.
- No learned cluster weights.
- No automated experiment store.
- No combined `backtest loop` command.
- No gamma reconstruction from current or later data.
- No official S&P 500 breadth without point-in-time constituent coverage.
- No MOVE/rates-vol input without a clean source.
