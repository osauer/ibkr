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

Streaming frames emitted by `ibkr quote SYM --watch --json` and by the
MCP streaming-resource notification path. One JSON object per line / per
notification.

```json
{ "t": "2026-05-09T14:32:11.421Z", "bid": 207.86, "ask": 207.88, "last": 207.87,
  "bid_size": 100, "ask_size": 200, "data_type": "live" }
```

All price and size fields are optional and may be omitted between ticks.
Volume is intentionally not streamed — it is a slow, monotonically
increasing day total and clutters the tick feed.

### Terminal error frames

A frame with the `error` field populated is the **last** frame on its
subscription — the daemon will not send anything after. Price/size
fields are nil. The CLI watcher renders the error and exits cleanly;
MCP subscribers receive the error frame as a normal
`notifications/resources/updated` payload, after which the subscription
is removed daemon-side.

```json
{
  "t": "2026-05-09T14:32:14.802Z",
  "error": {
    "code": "gateway_lost",
    "message": "IB Gateway connection dropped during streaming subscription"
  }
}
```

`error.code` is one of:

- `gateway_lost` — IB Gateway connection dropped mid-stream.
- `entitlement_lost` — data-type slid below a viable level (e.g. `delayed` → no data).
- `subscription_rejected` — post-subscribe IBKR rejection (delisted, halted permanently).
- `daemon_shutdown` — daemon doing a clean exit (signal received).

Synchronous errors (symbol not found at subscribe time, gateway down at
subscribe time) ride the JSON-RPC error response on the subscribe call
itself, not the notification channel.

## MCP streaming resources

The MCP server (`ibkr mcp`) exposes live quotes via two URI templates,
discoverable through `resources/templates/list`:

```
ibkr://quote/{symbol}
ibkr://option/{symbol}/{expiry}/{right}/{strike}
```

- `{symbol}` is uppercase ticker.
- `{expiry}` is `YYMMDD`.
- `{right}` is `C` or `P` (uppercase or lowercase accepted).
- `{strike}` is the numeric strike (integer or decimal).

Examples:

```
ibkr://quote/AAPL
ibkr://option/AAPL/240119/C/195
ibkr://option/SPX/240119/P/4500.5
```

`resources/read` returns the current snapshot ([quote](#quote) shape)
in a single text content block. `resources/subscribe` returns `{}` and
then streams coalesced [frames](#frame) via `notifications/resources/updated`
notifications, with the frame JSON embedded in `params.contents`:

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/resources/updated",
  "params": {
    "uri": "ibkr://quote/AAPL",
    "contents": [{
      "uri": "ibkr://quote/AAPL",
      "mimeType": "application/json",
      "text": "{ \"t\": \"...\", \"bid\": 207.86, ... }"
    }]
  }
}
```

Unsubscribe explicitly via `resources/unsubscribe`, or close the MCP
server's stdio — the subscription drops either way and the daemon's
refcount on the IBKR market-data line decrements.

Multiple concurrent subscribers (CLI `--watch`, MCP subscribers, snapshot
polls) on the same symbol share **one** IBKR market-data line via the
daemon's fan-out manager. The line is released when the last subscriber
goes away.

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

## chain-expiries

`ibkr chain AAPL --json` (no `--expiry` → expiry listing with ATM IV
for the nearest 12 expiries by default; daemon caches results).
`ibkr chain AAPL --no-iv --json` returns the fast skeleton (date list only).
`ibkr chain AAPL --all-expiries --json` fetches IV for every listed date.

```json
{
  "symbol": "AAPL",
  "spot": 207.42,
  "as_of": "2026-05-09T14:32:11Z",
  "expiries": [
    {"date": "2026-05-16", "dte": 7, "iv": 0.312, "iv_status": "ok", "implied_move": 9.04, "implied_move_pct": 0.0436},
    {"date": "2026-05-23", "dte": 14, "iv": 0.298, "iv_status": "ok", "implied_move": 12.21, "implied_move_pct": 0.0589},
    {"date": "2026-06-19", "dte": 41, "iv": 0.284, "iv_status": "ok", "implied_move": 19.85, "implied_move_pct": 0.0957},
    {"date": "2026-07-17", "dte": 69, "iv": null, "iv_status": "timeout"},
    {"date": "2026-12-18", "dte": 223}
  ]
}
```

Field meanings:
- `spot` — underlying mid the daemon used to pick the per-expiry ATM
  strike and compute `implied_move`. Zero when the spot probe failed
  or `--no-iv` was passed.
- `expiries[].date` — ISO date `YYYY-MM-DD`. Sorted ascending, deduped
  across exchanges (SMART, AMEX, CBOE, …) so each expiry appears once.
- `expiries[].dte` — calendar days from today (local) to the expiry.
  Same-day expiries have `dte` = 0.
- `expiries[].iv` — decimal (e.g. `0.284` = 28.4%) or `null`. Present
  when the daemon fetched IV for that expiry (default: nearest 12;
  `--all-expiries` extends).
- `expiries[].iv_status` — `ok`, `timeout`, or `unavailable`. Set
  when IV was fetched (or attempted); absent on bare rows beyond the
  default cap. Surface non-`ok` rows clearly; do not substitute a proxy.
- `expiries[].implied_move` — the 1-σ expected dollar move by
  expiration, computed `spot × IV × √(DTE/365)`. The desk-standard
  "expected move" used to size event trades and pick option strikes.
  Populated only when spot and IV are both known; `null` otherwise.
- `expiries[].implied_move_pct` — `implied_move / spot` as a fraction
  (e.g. `0.0436` means 4.36% expected move by expiry).

Empty `expiries` means the symbol has no listed options (typical for
ETFs without an option program). Surface this honestly rather than
fabricating expiries or falling back to the existing chain command.

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

Healthy / connected:

```json
{
  "daemon_version": "v0.4.2",
  "uptime_seconds": 14400,
  "account": "U1234567",
  "gateway_host": "127.0.0.1",
  "gateway_port": 4001,
  "gateway_tls": false,
  "negotiated_tls": false,
  "port_origin": "discovered",
  "client_id": 15,
  "connected": true,
  "data_type": "live"
}
```

Degraded (TCP-reachable but gateway never completed the handshake, or the
user closed Gateway / opened TWS on a different port):

```json
{
  "daemon_version": "v0.4.2",
  "gateway_host": "127.0.0.1",
  "gateway_port": 4001,
  "port_origin": "discovered",
  "connected": false,
  "last_error": "gateway not responding to TWS handshake within 12s"
}
```

Action-relevant fields:

- `connected` — `true` only when the TWS handshake completed. `false` =
  surface the error to the user.
- `last_error` — populated when the daemon attempted a connection and it
  failed. Empty during the in-flight initial handshake (the daemon may
  still be waiting); populated once the daemon gives up.
- `port_origin` — `"pinned"` (user wrote it in config), `"discovered"`
  (probe found the listener), or `"default"`. Tells you whether `ibkr
  status` is showing the gateway the user *intended* (pinned) or one the
  daemon picked on its own (discovered).
- `gateway_tls` vs `negotiated_tls` — disagreement means the daemon's TLS
  fallback fired (config said plain, server demanded TLS, AUTO mode let it
  upgrade). Surface this when troubleshooting handshake errors.
- `data_type` — `live`, `delayed`, `frozen`, or `delayed_frozen`. If a
  user asks about a quote and `data_type != "live"`, mention it.

A full set of additional metadata fields (`alternates`, `tls_origin`,
`server_version`, `daemon_started`) is also returned but rarely
actionable; surface them only when the user is debugging discovery.

## size

`ibkr size --symbol SYM --entry F --stop F [--target F] [--risk-pct 1.0] [--side long|short] [--lot 1] [--fx 1.0] --json`

```json
{
  "symbol": "AAPL",
  "side": "long",
  "entry": 207.50,
  "stop": 202.50,
  "target": 217.50,
  "risk_pct": 1.0,
  "lot": 1,
  "fx": 1.0,
  "nlv": 248310.42,
  "base_currency": "EUR",
  "risk_base": 2483.10,
  "risk_quote": 2483.10,
  "per_share_risk": 5.0,
  "shares": 496,
  "notional": 102920.0,
  "max_loss": 2480.0,
  "r": 2.0,
  "reward_quote": 4960.0,
  "reward_base": 4960.0,
  "breakeven_win_rate": 0.3333,
  "status": "ok"
}
```

- `nlv` is read live from `account.summary` and is in `base_currency`.
- `risk_base = nlv * risk_pct / 100` (base currency).
- `risk_quote = risk_base * fx` (the trade's quote currency). Pass `--fx` when
  the account base differs from the symbol's quote currency; default `1.0` is
  correct for same-currency trades.
- `per_share_risk = |entry - stop|`, in quote currency.
- `shares = floor(risk_quote / per_share_risk / lot) * lot`.
- `notional` and `max_loss` are in quote currency.
- `target`, `r`, `reward_quote`, `reward_base`, `breakeven_win_rate` —
  populated **only** when `--target` is supplied. Long trades require
  `target > entry`; short trades require `target < entry`.
  - `r = |target - entry| / per_share_risk` — the reward-to-risk
    multiple. The standard discretionary threshold is `r >= 2`.
  - `reward_quote = shares * |target - entry|` (quote currency); 
    `reward_base = reward_quote / fx`.
  - `breakeven_win_rate = 1 / (1 + r)` — the strategy's break-even
    hit rate at this R. Surface as a percentage when explaining to
    a user (e.g. `r = 2.0` → 33.3% breakeven).
- `status` is one of:
  - `ok` — sized within buying power.
  - `tight_risk` — `shares == 0` because the budget can't cover one lot at the
    chosen per-share risk. Suggest widening the stop, raising `--risk-pct`, or
    lowering `--lot`.
  - `exceeds_buying_power` — `notional > buying_power * fx`. Suggest trimming
    `--risk-pct` or revisiting the entry.

The CLI never derives entry, stop, or target from market data — they're the
user's trade plan. The CLI also performs no order action; this is math
against the live account snapshot.
