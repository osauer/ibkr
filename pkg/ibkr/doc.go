// Package ibkr is a clean-room Go implementation of the Interactive
// Brokers TWS wire protocol. It speaks length-prefixed null-delimited
// messages directly to an IB Gateway or TWS instance — no Python
// bridge, no IBKR jars, no third-party dependencies on InteractiveBrokers
// source code. The package is independent of the `ibkr` binary's CLI and
// MCP surfaces (which live in internal/) and can be used as a Go library
// against any TWS-speaking endpoint.
//
// # Protocol coverage
//
// What's plumbed today (v0.13):
//
//   - Account & positions: reqAccountSummary (62), reqAccountUpdates (6),
//     portfolio_value (msg 7), $LEDGER:ALL multi-currency exposure. Positions
//     come from the streaming portfolioValue subscription started by
//     [Connector.RequestAccountUpdates]; read the cache with
//     [Connector.GetCachedPositions]. No reqPositions round-trip on the read
//     path — it would clear the cache and lose mark/value/P&L.
//     Entry points: [Connector.RequestAccountSummary],
//     [Connector.RequestAccountUpdates], [Connector.GetCachedPositions].
//   - Market data, snapshot: reqMktData (1) with snapshot=true.
//     Entry point: [Connector.FetchMarketSnapshot].
//   - Market data, streaming: reqMktData (1) with snapshot=false; default
//     generic-tick list 100,101,104,106,165 (option volume, OI, HV,
//     averaged IV, Misc Stats 13/26/52w highs/lows).
//     Entry points: [Connector.SubscribeMarketData],
//     [Connector.EnsureMarketDataSubscription], [Connector.GetMarketData].
//   - Option chains: reqSecDefOptParams (78), tickOptionComputation (21).
//     Entry points: [Connector.FetchOptionExpiries],
//     [Connector.FetchOptionExpiryStrikes], [Connector.GetOptionGreeks],
//     [Connector.GetOptionIV].
//   - Historical bars: reqHistoricalData (20), daily granularity.
//     Entry point: [Connector.FetchHistoricalDailyBars].
//   - Market scanner: reqScannerSubscription (22),
//     reqScannerParameters (24). One-shot first-frame collection;
//     scanner subscriptions are cancelled after the first response.
//     Entry points: [Connector.RunScannerSubscription],
//     [Connector.RunScannerParameters].
//   - Contract resolution: reqContractData (9). Contract details
//     cached per symbol with phase-aware TTL.
//     Entry point: [Connector.FetchContractDetails].
//   - Market-data type: reqMarketDataType (59) — switch live / frozen /
//     delayed / delayed-frozen at runtime.
//     Entry point: [Connector.SetMarketDataType].
//   - Order placement: placeOrder (3), cancelOrder (4). Wire-implemented
//     but the bundled CLI / MCP / daemon refuse the verbs in v0.x — the
//     daemon's order-method handlers return ErrTradingDisabled
//     unconditionally (see internal/daemon/trading_disabled.go). Library
//     callers can drive the wire directly via [Connector.SubmitOrder] /
//     [Connector.CancelOrder].
//
// What's not implemented (would-need-plumbing if a use case appears):
//
//   - Real-time bars: reqRealTimeBars (50).
//   - Market depth (L2): reqMktDepth (10), reqMktDepthL2 (13).
//   - Fundamental data: reqFundamentalData (52).
//   - News bulletins: reqNewsBulletins (12), tickNews.
//   - Financial Advisor (FA) sub-account routing: reqFA (18).
//   - Implied-volatility / option-price calculators:
//     reqCalcImpliedVolatility (54), reqCalcOptionPrice (55).
//
// # Entry points
//
// The primary type is [Connector]. Construct via [NewConnector] with a
// [ConnectorConfig] and call [Connector.Start] to handshake the gateway.
// A connection pool holds the actual TCP socket; the Connector is a
// stateful facade with handler registration, request-ID allocation,
// subscription tracking, and per-symbol caches.
//
// # Read-only safety
//
// The TWS protocol intermingles read and write opcodes; this package
// exposes both because clean-room reimplementing the protocol means
// reimplementing the write side too. The bundled `ibkr` binary refuses
// every order verb at the daemon's RPC dispatch layer — see
// internal/daemon/trading_disabled.go — so the CLI / MCP / hook chain
// physically cannot send placeOrder / cancelOrder. Library callers
// driving Connector directly are on their own; if you want the same
// guarantee, do not call [Connector.SubmitOrder] / [Connector.CancelOrder].
//
// # Trademark
//
// "Interactive Brokers", "IBKR", "TWS", and "IB Gateway" are trademarks
// of Interactive Brokers Group, Inc. or its affiliates, used here
// nominatively to identify the protocol this package speaks. This
// package is not built, endorsed, or supported by Interactive Brokers.
package ibkr
