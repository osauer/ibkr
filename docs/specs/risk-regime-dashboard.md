# Risk Regime Dashboard Contract

**Updated:** 2026-06-01 06:01 CEST

`ibkr regime` reports the broad-market stress lifecycle: `quiet`,
`early_warning`, `confirmed_stress`, `panic`, `stabilization`, `opportunity`,
or `data_quality`. It is an evidence-balance read, not a prediction, trading
system, portfolio planner, or investment recommendation.

Use it to answer one question: are several independent market-risk indicators
confirming each other, or is the market still broadly calm?

Canary may consume this output, but canary owns account and portfolio action. A
portfolio concentration problem can be real even while the broad market regime
is calm.

## Output Shape

Each row should show:

- current value;
- band: `green`, `yellow`, `red`, or unranked;
- status: `ok`, `stale`, `computing`, `unavailable`, or `error`;
- source and as-of information;
- a short band reason;
- the threshold set used for that row.

The top-level envelope should also show:

- `lifecycle`: stage, severity, readiness, timing, confidence, evidence,
  confirmed sources, unconfirmed sources, a semantic lifecycle fingerprint, and
  an explicit no-execution statement;
- `source_health`: per-cluster `as_of`, status, age/freshness, confidence, and
  fingerprint-stability semantics;
- `fingerprint`: semantic identity for the classified broad-market state.

Missing, stale, computing, and degraded data must stay visible. A quiet reading
with missing critical inputs is not the same thing as a confirmed calm regime.

## Indicator Sources

Each row must identify the concrete data source and actual symbol or series
behind the reading. The live dashboard uses these sources; historical replays
may substitute point-in-time equivalents, but the row meaning should stay the
same.

| Row | Actual symbols or series | Live source |
| --- | --- | --- |
| VIX/VIX3M | `VIX` and `VIX3M`, Cboe equity-volatility indexes | IBKR index market data for Cboe VIX and VIX3M; backtests use Cboe official historical CSVs. |
| VVIX | `VVIX`, Cboe's VIX-of-VIX index | Cboe official daily VVIX time series. |
| HYG/SPY | `HYG`, a high-yield corporate bond ETF, and `SPY`, an S&P 500 ETF | IBKR HYG/SPY quotes plus HMDS daily bars; SPY 52-week high uses IBKR Misc Stats tick 165 when available and daily-bar fallback otherwise. Backtests use Nasdaq public ETF history. |
| HY OAS | FRED `BAMLH0A0HYM2` for high-yield OAS and `BAMLC0A0CM` for investment-grade corporate OAS | FRED/St. Louis Fed CSVs for ICE BofA option-adjusted spread series. |
| CP 90-day AA financial minus 3-month T-bill | FRED `RIFSPPFAAD90NB` and `DTB3` | FRED/St. Louis Fed daily Federal Reserve rate series. |
| USD/JPY weekly change | `USD.JPY`, routed as IBKR `CASH` on `IDEALPRO` with currency `JPY` | IBKR FX tick plus HMDS midpoint history for the seven-trading-day comparison; Tier 1 historical replay uses FRED `DEXJPUS`. |
| SPY+SPX zero-gamma | `SPY` ETF options and `SPX` index options | IBKR option chains, open interest, option quotes/model-computation ticks, and the daemon's SPY+SPX gamma cache. |
| S&P 500 breadth | Current S&P 500 constituent stock tickers; there is no single breadth symbol used live | Local daemon compute from IBKR HMDS constituent daily bars and the generated S&P 500 membership list. |

## Clusters

A cluster is a group of related indicators. The composite regime counts
clusters, not raw rows, so one market theme cannot vote twice.

Within each cluster, the worst ranked row wins: red beats yellow, yellow beats
green. Unavailable, computing, and error rows are unranked.

### Equity Volatility

This cluster watches option-market fear. VIX/VIX3M asks whether near-term fear
is priced above longer-term fear. VVIX asks whether traders are paying up for
large volatility moves. When both worsen, equity stress is usually becoming
more urgent.

VIX is Cboe's 30-day implied-volatility index for the S&P 500. VIX3M is the
same idea over roughly three months, and VVIX measures how volatile VIX itself
is expected to be.

VIX/VIX3M backwardation is stress-level evidence by itself. An isolated VVIX
red between 110 and 120 is noisier: the VVIX row remains red and visible, but
the equity-volatility cluster counts as yellow unless VVIX is at least 120, VIX
is up at least 20% on the day, SPY is down at least 1% on the day, or another
independent cluster is red. This keeps volatility warnings visible without
letting a standalone vol-of-vol pop dominate the broad-market read.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| VIX/VIX3M | < 0.92 | 0.92-1.00 | > 1.00 |
| VVIX | < 90 | 90-110 | > 110 |

### Credit

This cluster watches whether corporate credit is weakening before or alongside
stocks. HYG is an ETF holding high-yield corporate bonds, meaning lower-rated
company debt that behaves more like risk assets than Treasuries. SPY is the
large S&P 500 ETF used here as the stock-market comparison.

HYG/SPY is the faster market proxy. HY/IG OAS is the slower official cash-credit
read: it compares high-yield and investment-grade corporate bond spreads, where
OAS means the extra yield investors demand over Treasuries after adjusting for
bond options. Credit stress matters because equity rallies are less sturdy when
lenders are already demanding more compensation for risk.

HYG/SPY can still show a red row by itself. For the cluster count, that
single proxy red is treated as a yellow watch unless cash credit is also red or
another independent cluster is red. The row stays visible; it just does not get
to call broad stress alone.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| HYG/SPY | HYG healthy | HYG below 50-DMA | HYG weak while SPY is near highs |
| HY OAS | < 4.0 and not widening | 4.0-5.5 or widening > 0.50 pp | > 5.5 or widening > 1.00 pp |

### Funding

This cluster watches whether short-term money markets are becoming stressed.
The spread between 90-day AA financial commercial paper and 3-month T-bills is
a simple check on whether financial borrowers are paying unusually high short-
term funding costs.

Commercial paper is short-term company borrowing; T-bills are short-term U.S.
Treasury borrowing. A wider spread means financial firms are paying noticeably
more than the government to borrow for a similar short horizon.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| CP 90-day AA financial minus 3-month T-bill | < 25 bp | 25-75 bp | > 75 bp |

### FX Carry

This cluster watches USD/JPY as a proxy for global carry-trade pressure. When
the yen strengthens quickly, leveraged risk trades can unwind at the same time.
That does not predict every selloff, but it is useful confirmation when other
clusters are also deteriorating.

USD/JPY is quoted as yen per U.S. dollar. A falling USD/JPY means the yen is
strengthening, which is the direction that can pressure yen-funded carry trades.

USD/JPY can still show a red row by itself. For the cluster count, an isolated
FX red is treated as a yellow watch until another independent cluster confirms
stress. Canary may still act on a fast carry unwind when direct SPY/VIX tape or
breadth confirms the move.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| USD/JPY weekly change | yen move < 1% | yen strengthens 1-2% | yen strengthens > 2% |

### Dealer Gamma

This cluster watches whether dealer hedging is more likely to dampen or amplify
index moves. Above zero-gamma, hedging flows are usually more stabilizing.
Below zero-gamma, hedging can chase the market lower or higher and make moves
sharper. Treat this as a regime hint, not a precise tradable level.

SPY is the exchange-traded S&P 500 ETF, while SPX is the S&P 500 index itself.
Their option books trade separately, so the dashboard reads both and combines
the per-index gamma regimes.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| SPY+SPX zero-gamma | spot > 2% above zero-gamma | within +/-2% | spot below zero-gamma |

### Breadth

This cluster watches how many S&P 500 stocks are participating. A rally led by
many stocks is healthier than a rally carried by a few mega-caps. Weak breadth
near index highs warns that the headline index may be hiding fragility.

There is no single live IBKR symbol for this row. The daemon computes it from
daily bars for the individual S&P 500 member stocks and caches the post-close
result.

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| S&P 500 breadth | > 55% above 50-DMA | 40-55%, or weakening near highs | < 40%, especially while SPX is near highs |

## Composite Logic

| Cluster state | Regime label |
| --- | --- |
| 0 red and 0-2 yellow | Normal regime |
| 0 red and 3+ yellow | Elevated stress watch |
| 1-2 red | Stress signal present |
| 3+ red | Broad stress regime |
| all ranked clusters red | Full risk-off conditions |

The output may also show raw indicator counts for transparency. Cluster counts
are the primary signal because related rows, such as VIX and VVIX, are not
fully independent votes.

Lifecycle is a second layer on top of the row and cluster evidence:

| Lifecycle stage | Broad-market meaning |
| --- | --- |
| `quiet` | Enough data is ranked and no material stress or recovery/opportunity evidence is present. |
| `early_warning` | Weak, isolated, or forward-looking evidence is visible, but independent confirmation is not yet present. |
| `confirmed_stress` | At least two independent stress clusters, or one severe cluster plus confirming SPY/VIX tape, are active. |
| `panic` | Stress is broad or tape is severe enough that the regime should be treated as acute. |
| `stabilization` | Stress evidence is easing, but this is not yet a deployable opportunity by itself. |
| `opportunity` | Constructive tape and low stress evidence are present; this is broad-market context only, not a trade instruction. |
| `data_quality` | Missing, stale, computing, or degraded inputs prevent a confident lifecycle read. |

The lifecycle layer must keep unconfirmed red evidence visible without letting a
single fragile proxy dominate the trigger. `readiness` should be `blocked` or
degraded when critical source health is stale, partial, computing, or degraded.

An unconfirmed HYG/SPY-only red or USD/JPY-only red remains visible in the row
details, but it is counted as yellow at the cluster level. This keeps fast
proxies useful without letting one fragile proxy dominate the broad-market
read.

The expanded Tier 1 backtest shows that isolated red equity-volatility clusters
are also the main source of repeated false alarms. They should not be deleted:
major stress often starts in volatility before credit, funding, or FX confirms.
The live rule therefore keeps VIX/VIX3M inversion as stress, but downgrades an
isolated moderate VVIX-only red to yellow unless the already-visible SPY/VIX
tape or another cluster confirms it.

## Method Notes

Breadth is computed locally from S&P 500 constituent daily bars because the
retail IBKR feed does not provide the official S&P breadth series directly. The
daemon caches the post-close result; reads should not trigger a 500-name fanout.

Dealer gamma is a best-effort SPY+SPX zero-gamma estimate from IBKR option
chain data. Historical backtests should exclude gamma unless the row has a
trusted point-in-time gamma snapshot with method, source, coverage, and
timestamp.

MOVE/rates-vol is outside the live surface until a verified IBKR contract or
licensed official connector exists. Do not proxy it with ETFs or futures.

## Backtesting

The active backtest sequence, tuning gates, and source-data backlog live in
[Regime and Canary Backtest Runbook](regime-backtest-plan.md). Keep this file
as the product contract for `ibkr regime`; do not use it as a second tuning
backlog.
