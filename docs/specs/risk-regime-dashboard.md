# Risk Regime Dashboard Contract

`ibkr regime` reports whether broad market conditions look normal, watch-level,
or stressed. It is an evidence-balance read, not a prediction, trading system,
or investment recommendation.

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

Missing, stale, computing, and degraded data must stay visible. A quiet reading
with missing critical inputs is not the same thing as a confirmed calm regime.

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
stocks. HYG/SPY is the faster market proxy. HY/IG OAS is the slower official
cash-credit read. Credit stress matters because equity rallies are less sturdy
when lenders are already demanding more compensation for risk.

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

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| CP 90-day AA financial minus 3-month T-bill | < 25 bp | 25-75 bp | > 75 bp |

### FX Carry

This cluster watches USD/JPY as a proxy for global carry-trade pressure. When
the yen strengthens quickly, leveraged risk trades can unwind at the same time.
That does not predict every selloff, but it is useful confirmation when other
clusters are also deteriorating.

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

| Row | Green | Yellow | Red |
| --- | --- | --- | --- |
| SPY+SPX zero-gamma | spot > 2% above zero-gamma | within +/-2% | spot below zero-gamma |

### Breadth

This cluster watches how many S&P 500 stocks are participating. A rally led by
many stocks is healthier than a rally carried by a few mega-caps. Weak breadth
near index highs warns that the headline index may be hiding fragility.

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
