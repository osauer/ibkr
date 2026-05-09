# `ibkr` JSON schemas

This document is the authoritative description of every `--json` output the
`ibkr` CLI emits. Field absence semantics matter:

- `null` for a `float64` field means the gateway did not deliver that tick.
- An empty array means the user genuinely has nothing to show, not a failure.
- Numbers are always `float64` unless explicitly marked `int64`.
- Times are RFC 3339 with timezone.

## account

`ibkr account --json`

```json
{
  "account_id": "U1234567",
  "profile": "live",
  "base_currency": "EUR",
  "net_liquidation": 248310.42,
  "buying_power": 992841.68,
  "available_funds": 124055.21,
  "excess_liquidity": 124055.21,
  "total_cash": 18422.30,
  "maintenance_margin": 2815.04,
  "initial_margin": 3520.55,
  "data_type": "live",
  "as_of": "2026-05-09T14:32:08+02:00"
}
```

Field meanings:
- `net_liquidation` — total account value in `base_currency`.
- `buying_power` — funds available for new positions.
- `available_funds` — cash net of margin requirements.
- `excess_liquidity` — buffer above maintenance margin.
- `data_type` — one of `live`, `delayed`, `frozen`, `delayed_frozen`.

## positions

`ibkr positions --json`

```json
{
  "data_type": "live",
  "as_of": "2026-05-09T14:32:09Z",
  "account_id": "U1234567",
  "stocks": [
    {
      "symbol": "NVDA",
      "sec_type": "STOCK",
      "exchange": "NASDAQ",
      "currency": "USD",
      "quantity": 120,
      "avg_cost": 412.18,
      "mark": 478.55,
      "market_value": 57426.00,
      "unrealized_pnl": 7964.40,
      "realized_pnl": 0
    }
  ],
  "options": [
    {
      "symbol": "AAPL",
      "sec_type": "OPTION",
      "currency": "USD",
      "quantity": 5,
      "avg_cost": 682.0,
      "mark": 940.0,
      "market_value": 4700.00,
      "unrealized_pnl": 1290.0,
      "realized_pnl": 0,
      "expiry": "20260619",
      "strike": 215.0,
      "right": "C"
    }
  ]
}
```

The `stocks` and `options` arrays are always present (possibly empty). For
options, the `symbol` is the underlying (e.g. `AAPL`), and `expiry` /
`strike` / `right` together identify the contract.

`avg_cost` for options is the per-contract premium; the gateway does NOT
multiply by 100. `market_value` and `unrealized_pnl` are already in account
currency and applied the multiplier.

The response also includes `by_underlying`: an array of groups (one per
underlying symbol) with the stock leg (`stock`, optional), the option legs
(`options`, may be empty), and the summed `group_market_value` /
`group_unrealized_pnl`. This is always populated; consumers that want a
flat view should ignore it.

```json
"by_underlying": [
  {
    "underlying": "AAPL",
    "stock": {"symbol": "AAPL", "quantity": 100, ...},
    "options": [{"symbol": "AAPL", "right": "C", "strike": 215, ...}],
    "group_market_value": 25400.0,
    "group_unrealized_pnl": 2838.0
  }
]
```

## quote

`ibkr quote AAPL --json` (single symbol) or `ibkr quote AAPL,MSFT,SPY --json`
(comma-separated → array).

```json
{
  "symbol": "AAPL",
  "contract": {
    "symbol": "AAPL",
    "sec_type": "STK",
    "exchange": "SMART",
    "currency": "USD"
  },
  "bid": 207.86,
  "ask": 207.88,
  "last": 207.87,
  "bid_size": 100,
  "ask_size": 200,
  "volume": 12400000,
  "iv": null,
  "iv_status": "unavailable",
  "data_type": "live",
  "as_of": "2026-05-09T14:32:11.421+02:00"
}
```

Field meanings:
- `bid`, `ask`, `last` — `null` means not delivered. Do not substitute.
- `bid_size`, `ask_size` — top-of-book size in shares (stocks/ETFs) or
  contracts (options). Omitted when the gateway didn't deliver tick 0/3.
- `volume` — cumulative day total. Omitted when the gateway didn't deliver
  tick 8.
- `iv` / `iv_status` — populated only when IBKR sends tick 106
  (Option Implied Volatility). For a stock snapshot this is almost always
  `null` / `"unavailable"` — that's an honest signal, not an error.
- `data_type` — `live`, `delayed`, `frozen`, or `delayed_frozen`.

For the multi-symbol form, the response is a top-level JSON array of these
objects.

## frame

Streaming frames emitted by `ibkr quote SYM --watch --json`. One JSON object
per line; the stream ends on Ctrl-C.

```json
{ "t": "2026-05-09T14:32:11.421Z", "bid": 207.86, "ask": 207.88, "last": 207.87,
  "bid_size": 100, "ask_size": 200 }
```

All price and size fields are optional and may be omitted between ticks.
Volume is intentionally not streamed — it is a slow, monotonically
increasing day total and clutters the tick feed.

## chain

`ibkr chain AAPL --expiry 2026-06-19 --width 5 --json`

```json
{
  "symbol": "AAPL",
  "spot": 207.88,
  "expiry": "2026-06-19",
  "dte": 41,
  "data_type": "live",
  "as_of": "2026-05-09T14:32:11Z",
  "strikes": [
    {
      "strike": 200.0,
      "is_atm": false,
      "call_bid": 12.80, "call_ask": 13.05, "call_last": 12.90, "call_iv": null,
      "put_bid": 1.85, "put_ask": 1.92, "put_last": 1.88, "put_iv": null
    }
  ]
}
```

The `is_atm: true` row is the strike closest to spot. Greeks are populated
only when IBKR delivers them; per-leg quotes may be `null` when the option
contract cannot be resolved without conID hydration (a v1 limitation; v1.1
adds full chain pricing).

## history

`ibkr history AAPL --days 90 --json`

```json
{
  "symbol": "AAPL",
  "days": 90,
  "data_type": "live",
  "as_of": "2026-05-09T14:32:11Z",
  "bars": [
    {"date": "2026-02-09", "open": 188.30, "high": 189.95, "low": 187.10, "close": 189.40, "volume": 48230100},
    {"date": "2026-02-10", "open": 189.50, "high": 191.20, "low": 188.85, "close": 190.95, "volume": 51012400}
  ]
}
```

Field meanings:
- `days` — calendar lookback requested. The actual number of bars returned
  is typically smaller (non-trading days are skipped).
- `bars[].date` — ISO date `YYYY-MM-DD`. Bars are ordered oldest → newest.
- `volume` — daily total share/contract volume.

Daily granularity only in v1.0; intraday bars are v1.1.

## scan

`ibkr scan top-movers --json`

```json
{
  "preset": "top-movers",
  "type": "TOP_PERC_GAIN",
  "as_of": "2026-05-09T14:32:09Z",
  "rows": [
    { "rank": 1, "symbol": "ABCD", "comment": "" }
  ]
}
```

## scan-list

`ibkr scan list --json`

```json
{
  "presets": [
    { "name": "high-iv", "type": "HIGH_OPT_IMP_VOLAT", "exchange": "STK.US", "limit": 20 },
    { "name": "most-active", "type": "MOST_ACTIVE", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "top-movers", "type": "TOP_PERC_GAIN", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "unusual-vol", "type": "HOT_BY_VOLUME", "exchange": "STK.US.MAJOR", "limit": 20 }
  ]
}
```

## status

`ibkr status --json`

```json
{
  "daemon_version": "v1.0.0",
  "daemon_started": "2026-05-09T08:44:00Z",
  "uptime_seconds": 14400,
  "profile": "live",
  "account": "U1234567",
  "gateway_host": "127.0.0.1",
  "gateway_port": 4001,
  "gateway_tls": false,
  "client_id": 15,
  "connected": true,
  "data_type": "live",
  "server_version": 203
}
```

`connected: false` means `ibkrd` is up but the IB Gateway connection is
broken; the daemon reconnects automatically.
