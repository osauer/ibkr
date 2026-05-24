# TWS protocol coverage

Last reviewed: 2026-05-24 07:38 CEST

`pkg/ibkr` is a clean-room Go implementation of the TWS wire protocol. It is not a full replacement for every TWS API method; it covers the read-side surface that the `ibkr` binary, daemon, CLI, and MCP server need.

Order-writing methods exist only for wire-format completeness and downstream forks. Default builds return `pkg/ibkr.ErrTradingDisabled` before any order write reaches the socket; intentionally order-capable forks must rebuild with `-tags trading`. The shipped daemon, CLI, MCP server, and Claude plugin expose no order surface.

| Capability | Wire opcodes | Library entry point | Status |
|---|---|---|---|
| Account summary | `reqAccountSummary` (62), `accountSummary` (63), `acctValue` (6) | `Connector.RequestAccountSummary`, `GetAccountSummary` | ready |
| Positions + portfolio | `reqPositions` (61), `position` (61), `portfolioValue` (7), `$LEDGER:ALL` | `Connector.GetCachedPositions` | ready |
| Snapshot quote | `reqMktData` (1) snapshot=true, `tickPrice` (1), `tickSnapshotEnd` (57) | `Connector.FetchMarketSnapshot` | ready |
| Streaming quote | `reqMktData` (1) snapshot=false, `tickPrice` / `tickSize` / `tickGeneric` | `Connector.SubscribeMarketData`, `GetMarketData` | ready |
| Generic-tick set | gen-ticks 100, 101, 104, 106, 165 (option vol, OI, HV, IV, misc stats) | populated into `MarketData` automatically | ready |
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
