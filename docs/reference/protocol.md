# TWS protocol coverage

Last reviewed: 2026-05-31 21:23 CEST

`pkg/ibkr` is a clean-room Go implementation of the TWS wire protocol. It is not a full replacement for every TWS API method; it covers the read-side calls that the `ibkr` binary, daemon, CLI, and MCP server need.

Order-writing methods exist only for wire-format completeness and downstream forks. Default builds return `pkg/ibkr.ErrTradingDisabled` before any order write reaches the socket; intentionally order-capable forks must rebuild with `-tags trading`. The shipped daemon, CLI, MCP server, and Claude plugin expose no order interface.

## Semantic fingerprints

Decision surfaces that are useful to monitors expose a semantic `fingerprint` object:

```json
{"version": "canary-fp-v1", "key": "sha256:..."}
```

The key is a SHA-256 hash of classified state, not a hash of the full JSON response. It deliberately excludes timestamps, raw prices, exact observed values inside the same threshold bucket, methodology prose, row guidance text, and rendering order. Monitors should use the key for dedupe, recovery, and repeat-alert TTL policy instead of recomputing their own hash.

`regime.snapshot` emits `regime-fp-v1` from indicator bands/statuses,
composite counts, lifecycle stage/severity/readiness buckets, warning
codes/scopes/severities, high-level data quality, source-health buckets, and
gamma/breadth semantic state.

`canary` emits `canary-fp-v1` from policy, action, market confirmation,
portfolio fit, input health, direction, severity, planner mode/readiness,
primary drivers, signal semantics, classified market state, source-health
buckets, row titles/states, and `source_fingerprints.account`,
`source_fingerprints.positions`, and `source_fingerprints.regime`.

Regime also exposes a nested `lifecycle.fingerprint` for consumers that dedupe
by broad-market lifecycle transition rather than by the full regime snapshot.
Canary is stateless and does not expose a canary lifecycle fingerprint. Source
fingerprints and source-health entries use semantic buckets only; timestamps,
tiny raw-value movement inside a bucket, and prose changes must not churn the
hash.

| Capability | Wire opcodes | Library entry point | Status |
|---|---|---|---|
| Account summary | `reqAccountSummary` (62), `accountSummary` (63), `acctValue` (6) | `Connector.RequestAccountSummary`, `GetAccountSummary` | ready |
| Positions + portfolio | `reqPositions` (61), `position` (61), `portfolioValue` (7), `$LEDGER:ALL` | `Connector.GetCachedPositions` | ready |
| Snapshot quote | `reqMktData` (1) snapshot=true, `tickPrice` (1), `tickSnapshotEnd` (57) | `Connector.FetchMarketSnapshot` | ready |
| Streaming quote | `reqMktData` (1) snapshot=false, `tickPrice` / `tickSize` / `tickGeneric` / `tickString` | `Connector.SubscribeMarketData`, `GetMarketData` | ready |
| Generic-tick set | gen-ticks 100, 101, 104, 106, 165 (option vol, OI, HV, IV, misc stats including range / average volume) | populated into `MarketData` automatically | ready |
| Contract resolution | `reqContractData` (9), `contractData` (10) | `Connector.FetchContractDetails` | ready |
| Option chains | `reqSecDefOptParams` (78), `tickOptionComputation` (21) | `Connector.FetchOptionExpiries`, `FetchOptionExpiryStrikes`, `GetOptionGreeks`, `GetOptionIV` | ready |
| Daily historical bars | `reqHistoricalData` (20), `historicalData` (17) | `Connector.FetchHistoricalDailyBars` | ready |
| Market scanner | `reqScannerSubscription` (22), `reqScannerParameters` (24) | `Connector.RunScannerSubscription`, `RunScannerParameters` | ready |
| Market-data type switch | `reqMarketDataType` (59), `marketDataType` (58) | `Connector.SetMarketDataType` | ready |
| Order placement / cancel | `placeOrder` (3), `cancelOrder` (4) | `Connector.SubmitOrder`, `CancelOrder` | disabled by default (`ErrTradingDisabled`); `-tags trading` only |
| Real-time bars | `reqRealTimeBars` (50) | - | not implemented |
| Market depth (L2) | `reqMktDepth` (10), `reqMktDepthL2` (13) | - | not implemented |
| Fundamental data | `reqFundamentalData` (52) | - | not implemented |
| News bulletins | `reqNewsBulletins` (12) | - | not implemented |
| Financial Advisor (FA) | `reqFA` (18) | - | not implemented |
| IV / option-price calculators | `reqCalcImpliedVolatility` (54), `reqCalcOptionPrice` (55) | - | not implemented |

Tested against IB Gateway server versions 100 through 203. Handshake auto-negotiates the highest protocol version the gateway and library agree on.
