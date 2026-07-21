# TWS protocol coverage

Last reviewed: 2026-07-21 CEST

`pkg/ibkr` is a clean-room Go implementation of the TWS wire protocol. It is not a full replacement for every TWS API method; it covers the broker reads and narrow order-wire operations used by the `ibkr` binary and daemon.

Unrestricted `SubmitOrder`/`CancelOrder`, `PlaceOrder`/`CancelOrder`, and `ExerciseOptions` methods are present in every build, but their position-changing wire paths are enabled only in trading-capable builds; default builds return `pkg/ibkr.ErrTradingDisabled` before writing such a frame. The narrower `SubmitPaperOrder`, `PlacePaperOrder`, and `CancelPaperOrder` methods are present in both builds and validate a concrete paper account plus matching connection coordinates. That library-level evidence is not submit authority. Broker WhatIf previews are also available in both builds, but they do not create working orders and never grant submit authority. The daemon adds trading status, pinned account/mode, preview-token, freeze, journal, broker, and origin checks; the MCP server remains preview/read-oriented and exposes no place/modify/cancel tools.

## Semantic fingerprints

Decision surfaces that are useful to monitors expose a semantic `fingerprint` object:

```json
{"version": "canary-fp-v2", "key": "sha256:..."}
```

The key is a SHA-256 hash of classified state, not a hash of the full JSON response. It deliberately excludes timestamps, raw prices, exact observed values inside the same threshold bucket, methodology prose, row guidance text, and rendering order. Monitors should use the key for dedupe, recovery, and repeat-alert TTL policy instead of recomputing their own hash.

`regime.snapshot` emits `regime-fp-v2` from indicator bands/statuses,
composite counts, lifecycle stage/severity/readiness buckets, warning
codes/scopes/severities, high-level data quality, source-health buckets, and
gamma/breadth semantic state. V2 adds the allowlisted failure code/stage when a
source carries typed failure evidence.

`market_events.snapshot` emits `market-events-fp-v2` from requested symbols,
semantic flag categories/statuses/severities/roles/sources, and source-health
buckets. It excludes timestamps, exact rates/share counts inside the same flag
classification, source prose, and rendering order. V2 includes typed failure
code/stage so a materially different source failure changes monitor identity.

`canary` emits `canary-fp-v2` from policy, action, market confirmation,
portfolio fit, input health, direction, severity, planner mode/readiness,
primary drivers, signal semantics including held-underlying stress buckets,
classified market state, source-health buckets, row titles/states, and
`source_fingerprints.account`,
`source_fingerprints.positions`, `source_fingerprints.regime`, and
`source_fingerprints.market_events`. V2 includes typed source failure
code/stage. The separate `established_alert_projection` intentionally retains
its frozen `canary-fp-v1` compatibility identity for delivery continuity.

`rules.snapshot` emits `rulebook-fp-v3` — a policy-identity fingerprint, not
a classified-state hash: the key is a SHA-256 of a JSON projection of the
active rulebook policy (rule set, thresholds, regime-conditional sets), so it
changes when the policy changes, not when verdicts move. The rules-decisions
journal records the same `policy_fingerprint` with every transition, naming
the policy identity that produced each journaled verdict.

Regime also exposes a nested `lifecycle.fingerprint` for consumers that dedupe
by broad-market lifecycle transition rather than by the full regime snapshot.
Canary is stateless and does not expose a canary lifecycle fingerprint. Source
fingerprints and source-health entries use semantic buckets only; timestamps,
tiny raw-value movement inside a bucket, and prose changes must not churn the
hash.

## Market events

`market_events.snapshot` returns `MarketEventsResult` for explicit symbols or,
when symbols are omitted, held stock/ETF underlyings from the positions snapshot.
The result contains `flags[]`, `by_symbol`, `source_health[]`,
`warning_details[]`, `fingerprint`, and `not_execution`.

`MarketEventFlag.id` is one of `borrow_inventory_tight`,
`borrow_fee_extreme`, `reg_sho_threshold`, `luld_pause`, or
`halt_regulatory_or_news`. Event `status` uses event terms such as `active` and
`recent`; source freshness uses `source_health[].status` values such as `ok`,
`stale`, `unknown`, or `degraded`.

`source_health[]` may also carry durable `refresh_state`, `next_attempt`, and
redacted typed `last_failure` fields (`code`, `stage`, `failed_at`,
`retryable`). Consumers must respect backoff/not-due state and use these typed
fields instead of parsing provider or transport prose from `notes`.

Borrow inventory comes from IBKR shortable-share market data. Borrow fee comes
from IBKR short-stock availability fee-rate evidence and emits
`borrow_fee_extreme` only at or above 50% annualized. Reg SHO V1 uses Nasdaq's
threshold list, so non-Nasdaq listing-exchange absence remains outside coverage.
LULD and halt context comes from Nasdaq trade-halt evidence.

Flags are context and safety gates, not execution authority. Active halt/LULD
can block proposal preview/submit; borrow flags modify existing short
buy-to-cover reductions only; `reg_sho_threshold` is regulatory context unless
paired with an existing reduce/cover proposal.

| Capability | Wire opcodes | Library entry point | Status |
|---|---|---|---|
| Account summary | `reqAccountSummary` (62), `accountSummary` (63), `acctValue` (6) | `Connector.RequestAccountSummary`, `GetAccountSummary` | ready |
| Positions + portfolio | `reqPositions` (61), `position` (61), `portfolioValue` (7), `$LEDGER:ALL` | `Connector.CachedPositions` | ready |
| Snapshot quote | `reqMktData` (1) snapshot=true, `tickPrice` (1), `tickSnapshotEnd` (57) | `Connector.FetchMarketSnapshot` | ready |
| Streaming quote | `reqMktData` (1) snapshot=false, `tickPrice` / `tickSize` / `tickGeneric` / `tickString` | `Connector.SubscribeMarketData`, `MarketDataSnapshot` | ready |
| Generic-tick set | gen-ticks 100, 101, 104, 106, 165 (option vol, OI, HV, IV, misc stats including range / average volume) | populated into `MarketData` automatically | ready |
| Contract resolution | `reqContractData` (9), `contractData` (10) | `Connector.FetchContractDetails` | ready |
| Option chains | `reqSecDefOptParams` (78), `tickOptionComputation` (21) | `Connector.FetchOptionExpiries`, `FetchOptionExpiryStrikes`, `OptionGreeks`, `OptionIV` | ready |
| Daily historical bars | `reqHistoricalData` (20), `historicalData` (17) | `Connector.FetchHistoricalDailyBars` | ready |
| Market scanner | `reqScannerSubscription` (22), `reqScannerParameters` (24) | `Connector.RunScannerSubscription`, `RunScannerParameters` | ready |
| Market-data type switch | `reqMarketDataType` (59), `marketDataType` (58) | `Connector.SetMarketDataType` | ready |
| Unrestricted order placement / cancel | `placeOrder` (3), `cancelOrder` (4) | `Connector.SubmitOrder`, `CancelOrder`; `Connection.PlaceOrder`, `CancelOrder` | disabled by default (`ErrTradingDisabled`); `-tags trading` only |
| Paper-gated order placement / cancel | `placeOrder` (3), `cancelOrder` (4) | `Connector.SubmitPaperOrder`, `CancelPaperOrder`; `Connection.PlacePaperOrder`, `CancelPaperOrder` | all builds; validates paper account and connection coordinates, but does not replace application authorization |
| Broker WhatIf preview | `placeOrder` (3) with WhatIf | `Connector.PreviewOrderWhatIf`, `Connection.PreviewOrderWhatIf` | all builds; broker evaluation that does not create a working order, never submit authority |
| Option exercise / lapse | `exerciseOptions` (21) | `Connector.ExerciseOptions`, `Connection.ExerciseOptions` | disabled by default (`ErrTradingDisabled`); `-tags trading` only; position-changing when accepted |
| API open-order snapshot | `reqAllOpenOrders` (5) | `Connector.SnapshotOpenOrders`, `Connection.RequestAllOpenOrders` | all builds; covers API-created orders across clients, not manual TWS orders by itself |
| Real-time bars | `reqRealTimeBars` (50) | - | not implemented |
| Market depth (L2) | `reqMktDepth` (10), `reqMktDepthL2` (13) | - | not implemented |
| Fundamental data | `reqFundamentalData` (52) | - | not implemented |
| Wall Street Horizon earnings dates | `reqWshMetaData` (100), `reqWshEventData` (102) | `Connector.FetchWSHEarnings` | ready; read-only, subscription-gated |
| News bulletins | `reqNewsBulletins` (12) | - | not implemented |
| Financial Advisor (FA) | `reqFA` (18) | - | not implemented |
| IV / option-price calculators | `reqCalcImpliedVolatility` (54), `reqCalcOptionPrice` (55) | - | not implemented |

Tests exercise handshake and parser behavior across IB Gateway server versions 100 through 203. Runtime connections reject versions below 124 and negotiate the highest supported protocol version with newer gateways.
