# `ibkr` JSON schemas

Updated: 2026-05-25 13:40 CEST

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
  "base_currency": "EUR",
  "net_liquidation": 248310.42,
  "buying_power": 992841.68,
  "available_funds": 124055.21,
  "excess_liquidity": 124055.21,
  "total_cash": 18422.30,
  "maintenance_margin": 2815.04,
  "initial_margin": 3520.55,
  "currency_exposure": [
    {
      "currency": "USD",
      "net_liquidation_ccy": 92418.07,
      "cash_ccy": 12005.50,
      "stock_market_value_ccy": 80412.57,
      "option_market_value_ccy": 0,
      "unrealized_pnl_ccy": 1842.40,
      "realized_pnl_ccy": 0,
      "exchange_rate": 1.0823,
      "net_liquidation_base": 85398.92
    }
  ],
  "data_type": "live",
  "as_of": "2026-05-09T14:32:08+02:00"
}
```

Field meanings:
- `net_liquidation` — total account value in `base_currency`.
- `buying_power` — funds available for new positions.
- `available_funds` — cash net of margin requirements.
- `excess_liquidity` — buffer above maintenance margin.
- `currency_exposure[]` — one row per non-base currency the gateway
  reported via `$LEDGER:ALL`. Empty / omitted on a single-currency
  account or pre-handshake. Rows reconcile within ~0.5%:
  `net_liquidation_ccy × exchange_rate ≈ net_liquidation_base`.
  - `exchange_rate` is BASE per CCY (how many base-currency units 1
    unit of the named currency converts to — matches IBKR's `$LEDGER`
    semantics so the reconciliation works without inversion).
  - `*_ccy` fields are in the named currency; `net_liquidation_base`
    is in the account's `base_currency`. Zero fields are real zeros
    from the gateway (e.g. no options held in that currency), not
    "unavailable".
- `data_type` — one of `live`, `delayed`, `frozen`, `delayed-frozen`.

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
      "multiplier": 1,
      "avg_cost": 412.18,
      "mark": 478.55,
      "data_type": "live",
      "prev_close": 471.20,
      "day_change": 7.35,
      "day_change_pct": 1.56,
      "day_high": 481.10,
      "day_low": 472.08,
      "week_52_high": 502.66,
      "week_52_low": 302.25,
      "volume": 41762007,
      "avg_volume": 58900000,
      "price_at": "2026-05-09T14:31:58-04:00",
      "price_as_of": "As of: May 9 at 02:31:58 PM EDT",
      "market_value": 57426.00,
      "market_value_ccy": 57426.00,
      "fx_rate": 1.0823,
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
      "multiplier": 100,
      "avg_cost": 682.0,
      "mark": 9.40,
      "market_value": 4700.00,
      "unrealized_pnl": 1290.0,
      "realized_pnl": 0,
      "expiry": "20260619",
      "strike": 215.0,
      "right": "C",
      "delta": 0.42,
      "gamma": 0.018,
      "theta": -0.08,
      "vega": 0.42,
      "option_bid": 9.35,
      "option_ask": 9.45,
      "option_prev_close": 8.92,
      "iv": 0.284
    }
  ],
  "portfolio": {
    "effective_delta": 1847.0,
    "dollar_delta": 326584.5,
    "dollar_delta_currency": "USD",
    "daily_theta": -42.18,
    "daily_theta_currency": "USD",
    "gamma": 12.4,
    "vega": 1205.0,
    "greeks_coverage": 5,
    "greeks_total": 5,
    "fx_sensitivity_per_pct": -854.32,
    "fx_base_currency": "EUR"
  },
  "by_underlying": [
    {
      "underlying": "AAPL",
      "stock": { "...": "STOCK row, same shape as stocks[]" },
      "options": [ { "...": "OPTION row, same shape as options[]" } ],
      "group_market_value": 25400.0,
      "group_unrealized_pnl": 2838.0
    }
  ]
}
```

The `stocks`, `options`, and `by_underlying` arrays are always present
(possibly empty). For options, the `symbol` is the underlying (e.g. `AAPL`),
and `expiry` / `strike` / `right` together identify the contract.

### Field meanings

- `sec_type` — wire constants from `pkg/ibkr.AssetType`: `STOCK`,
  `OPTION`, `FUTURE`, `INDEX`. Compare against the full word, not a
  three-letter short form.
- `multiplier` — 1 for stocks, 100 for standard equity options, sometimes
  higher for index/futures options. Always present (defaults to 1 when
  the gateway didn't supply one).
- `avg_cost` — **per-share** for stocks, **per-contract** (multiplier-
  inclusive) for options. To get the per-share premium on options divide
  by `multiplier`. The CLI does this automatically on the rendered AVG
  COST column; JSON output stays IBKR-faithful. `market_value` and
  `unrealized_pnl` are already in account-currency dollars with the
  multiplier applied.
- `prev_close`, `day_change`, `day_change_pct` — populated on STOCK rows
  via the daemon's prev-close prewarm. `null` when the gateway hasn't
  delivered tick 9 (rare on the happy path; usually pre-market).
- `day_high`, `day_low`, `week_52_high`, `week_52_low`, `volume`,
  `avg_volume`, `price_at`, `price_as_of`, `stale`, `stale_reason`,
  and `data_type` — stock-only quote context reused from the same
  market-data path as `ibkr quote` / `ibkr watch --quotes`. Nil fields
  mean the gateway did not deliver that tick within the short prewarm
  window. `price_as_of` is display-ready text; `price_at` is the typed
  timestamp.
- `market_value_ccy`, `fx_rate` — only set on non-base-currency positions.
  `market_value` remains in account base for back-compat; `market_value_ccy`
  is the contract-currency view, `fx_rate` the gateway-reported BASE/CCY
  conversion. Both nil/zero on same-currency books — no synthesis.
- `delta`, `gamma`, `theta`, `vega` — option-only, populated when the
  daemon captured a model-computation tick (msg 21 tickType 13) within
  budget. `null` = unavailable (illiquid leg, OOH model abstention, busy
  subscribe slot); never zero-substituted.
- `option_bid`, `option_ask`, `option_prev_close`, `iv` — option-only,
  populated from the per-leg market-data subscription the daemon already
  opens for Greeks. `iv` is a decimal fraction (0.284 = 28.4%).
- `portfolio` — daemon-computed aggregate block. Present when at least
  one option leg captured Greeks OR any non-base currency exposure has a
  known FX rate. Inner fields are nil when their inputs were unavailable
  — never zero-substituted.
  - `effective_delta` — sum of per-leg signed share-equivalents (stocks
    contribute signed quantity; options contribute
    delta × signed_qty × multiplier).
  - `dollar_delta` / `dollar_delta_currency` — share-equivalents
    multiplied by each leg's contract-currency spot. Currency named
    separately for client-side conversion.
  - `daily_theta` / `daily_theta_currency` — Σ (theta × signed_qty ×
    multiplier). IBKR reports theta as daily decay, so the sum is the
    daily P&L from time decay assuming everything else holds. The
    currency follows the same single-ccy-or-"MIX" convention as
    `dollar_delta_currency`: an ISO code when every contributing option
    leg agrees, "MIX" when the book mixes currencies (in which case
    the sum is genuinely undefined — render it without a single symbol).
  - `greeks_coverage` / `greeks_total` — count of option legs whose
    Greeks were captured / total option legs. Render partial-coverage
    explicitly to the user.
  - `fx_sensitivity_per_pct` — Σ (non-base market value in base) × 0.01;
    "how many base-currency units of P&L move per 1% FX shift". In
    `fx_base_currency`.
- `by_underlying[]` — groups stock leg (optional) + option legs by
  underlying. Always populated regardless of the `--by underlying` flag,
  which only affects the text view. `group_*` totals sum every leg in
  the group.

## watch

`ibkr watch --list --json`

```json
{
  "name": "default",
  "symbols": ["IBM", "SPY", "AAPL"],
  "as_of": "2026-05-25T10:13:00+02:00"
}
```

Field meanings:
- `name` — always `"default"` in this release; named lists are not exposed.
- `symbols` — locally stored symbols, normalized the same way as
  `ibkr quote`: comma-separated input is split, whitespace is trimmed,
  and symbols are uppercased. No IBKR lookup is performed when storing.
- `as_of` — local read time; this is not an IBKR market-data timestamp.

Human CLI mutations are `ibkr watch SYM --add`, `ibkr watch SYM --remove`,
and `ibkr watch --clear`. MCP exposes read-only watchlist list and
enriched quote views only.

`ibkr watch --quotes --json`

```json
{
  "name": "default",
  "symbols": ["AAPL", "MSFT"],
  "rows": [
    {
      "symbol": "AAPL",
      "contract": {
        "symbol": "AAPL",
        "sec_type": "STK",
        "currency": "USD"
      },
      "price": 190.12,
      "price_source": "last",
      "prev_close": 188.20,
      "change": 1.92,
      "change_pct": 1.02,
      "day_high": 191.30,
      "day_low": 187.55,
      "week_52_high": 199.62,
      "week_52_low": 164.08,
      "volume": 41762007,
      "avg_volume": 58900000,
      "data_type": "delayed",
      "price_at": "2026-05-22T16:01:02-04:00",
      "price_as_of": "Delayed: May 22 at 04:01:02 PM EDT",
      "stale": true,
      "stale_reason": "price timestamp is 20m old during market hours",
      "session_context": {
        "market": "us_equity",
        "label": "US equities",
        "date": "2026-05-25",
        "timezone": "America/New_York",
        "state": "holiday",
        "is_open": false,
        "reason": "Memorial Day",
        "next_open": "2026-05-26T09:30:00-04:00"
      },
      "holding": {
        "quantity": 25,
        "avg_cost": 176.50,
        "mark": 190.12,
        "market_value": 4753.00,
        "unrealized_pnl": 340.50,
        "daily_pnl": 48.00,
        "exchange": "NASDAQ",
        "currency": "USD"
      }
    }
  ],
  "as_of": "2026-05-25T16:15:00+02:00"
}
```

`rows[]` uses the same quote fields documented below, flattened onto each
watchlist row, plus optional `holding` context for saved symbols that are
currently held as stocks. When a holding is present, its `currency` and
`exchange` are also reused for the quote request so non-USD watched
positions route like `ibkr positions`. A per-row `error` string can appear
when one symbol fails while the rest of the watchlist succeeds.
`ibkr watch --watch` renders the same enriched rows repeatedly in text mode; `--watch`
and `--json` are mutually exclusive.

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
  "price": 207.87,
  "price_source": "last",
  "prev_close": 205.52,
  "change": 2.35,
  "change_pct": 1.14,
  "bid": 207.86,
  "ask": 207.88,
  "last": 207.87,
  "mark": 207.87,
  "day_high": 209.10,
  "day_low": 204.80,
  "week_52_high": 237.49,
  "week_52_low": 164.08,
  "bid_size": 100,
  "ask_size": 200,
  "volume": 12400000,
  "avg_volume": 58900000,
  "iv": null,
  "iv_status": "unavailable",
  "data_type": "frozen",
  "price_at": "2026-05-22T16:01:02-04:00",
  "price_as_of": "At close: May 22 at 04:01:02 PM EDT",
  "as_of": "2026-05-25T16:32:11.421+02:00",
  "session_context": {
    "market": "us_equity",
    "label": "US equities",
    "date": "2026-05-25",
    "timezone": "America/New_York",
    "state": "holiday",
    "is_open": false,
    "reason": "Memorial Day",
    "next_open": "2026-05-26T09:30:00-04:00",
    "next_close": "2026-05-26T16:00:00-04:00",
    "coverage_start": "2026-01-01",
    "coverage_end": "2028-12-31"
  }
}
```

Field meanings:
- `price` — the daemon's best display price. `price_source` names the
  input used: `last`, `mark`, `mid`, `bid`, `ask`, or `prev_close`.
  Use this for headline rendering; keep the source visible when it is not
  a live last trade.
- `prev_close`, `change`, `change_pct` — previous regular-session close
  and movement from `price`. Pointers/nulls preserve "not delivered"
  separately from an exactly flat day.
- `bid`, `ask`, `last` — `null` means not delivered. Do not substitute.
- `mark` — optional IBKR tick 37 mark/fair price. It is most useful
  off-hours or for instruments where bid/ask/last do not flow; render it
  as a fallback price, not as an actual last trade.
- `day_high`, `day_low`, `week_52_high`, `week_52_low` — range context
  from the market-data subscription. Nil means the gateway did not push
  the tick within the snapshot window.
- `bid_size`, `ask_size` — top-of-book size in shares (stocks/ETFs) or
  contracts (options). Omitted when the gateway didn't deliver tick 0/3.
- `volume` — cumulative day total. Omitted when the gateway didn't deliver
  tick 8.
- `avg_volume` — IBKR average volume tick from the Misc Stats bundle.
  Omitted when not delivered.
- `iv` / `iv_status` — populated only when IBKR sends tick 106
  (Option Implied Volatility). For a stock snapshot this is almost always
  `null` / `"unavailable"` — that's an honest signal, not an error.
- `data_type` — `live`, `delayed`, `frozen`, or `delayed-frozen`.
- `price_at` / `price_as_of` — timestamp for `price` and display-ready
  freshness text. Last trades use IBKR's last-timestamp tick when present;
  previous-close fallbacks use the prior official regular-session close.
- `stale` / `stale_reason` — present when the market is officially open
  but the best available price is only previous close or its timestamp is
  old enough to be misleading.
- `session_context` — optional official calendar explanation. Present
  when the snapshot is frozen/delayed/missing prices or the market is not
  in an ordinary open regular session. It names the supported market,
  current state (`regular`, `closed`, `holiday`, `early_close`, `unknown`),
  reason when known, session hours for trading days, next open/close when
  known, and embedded coverage. During ordinary live RTH with prices
  present, this field is omitted.

For the multi-symbol form, the response is a top-level JSON array of these
objects.

## calendar

`ibkr calendar --market us --date 2026-05-25 --next 3 --json`

Supported markets:

- `us` / `us-equity` — US cash equities.
- `us-options` — US listed options regular sessions.
- `de` / `de-xetra` — German Xetra cash equities.

Other markets and asset classes are only partly supported today. Futures,
FX, crypto, bonds, Eurex, Frankfurt floor trading, and per-class SPX/VIX
global-hours nuance are out of scope for v1.

```json
{
  "market": "us_equity",
  "label": "US equities",
  "timezone": "America/New_York",
  "as_of": "2026-05-25T11:44:00+02:00",
  "coverage_start": "2026-01-01",
  "coverage_end": "2028-12-31",
  "source": "official_exchange_calendar",
  "source_url": "https://www.nyse.com/markets/hours-calendars",
  "session": {
    "market": "us_equity",
    "label": "US equities",
    "date": "2026-05-25",
    "timezone": "America/New_York",
    "state": "holiday",
    "is_open": false,
    "reason": "Memorial Day",
    "next_open": "2026-05-26T09:30:00-04:00",
    "next_close": "2026-05-26T16:00:00-04:00",
    "source": "official_exchange_calendar",
    "source_url": "https://www.nyse.com/markets/hours-calendars",
    "coverage_start": "2026-01-01",
    "coverage_end": "2028-12-31",
    "notes": "Official NYSE/Nasdaq cash-equity holidays and early closes; other U.S. products may differ."
  },
  "sessions": [
    {
      "market": "us_equity",
      "label": "US equities",
      "date": "2026-05-25",
      "timezone": "America/New_York",
      "state": "holiday",
      "is_open": false,
      "reason": "Memorial Day",
      "next_open": "2026-05-26T09:30:00-04:00",
      "next_close": "2026-05-26T16:00:00-04:00"
    },
    {
      "market": "us_equity",
      "label": "US equities",
      "date": "2026-05-26",
      "timezone": "America/New_York",
      "state": "regular",
      "is_open": false,
      "open": "2026-05-26T09:30:00-04:00",
      "close": "2026-05-26T16:00:00-04:00"
    }
  ]
}
```

Field meanings:

- `market` — stable market id: `us_equity`, `us_options`, or `de_xetra`.
- `session` — the current/date session requested by `--date` or by now
  when omitted.
- `sessions[]` — forward calendar-day rows starting at the requested date
  or current market date. `--next` defaults to 14 and is capped at 400.
- `state` — `regular`, `closed`, `holiday`, `early_close`, or `unknown`.
  `unknown` means outside embedded official coverage; do not infer open
  from weekdays.
- `open` / `close` — present on trading days. Times are RFC 3339 in the
  market's timezone.
- `next_open` / `next_close` — present when the requested instant is closed
  and a next session is known within coverage.
- `coverage_start` / `coverage_end` — embedded official schedule coverage.
  Calendar updates arrive with binary releases in v1; there is no runtime
  network refresh.

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

The MCP server (`ibkr mcp`) exposes live stock and ETF quotes via the
`ibkr://quote/{symbol}` URI template, discoverable through
`resources/templates/list`:

```
ibkr://quote/{symbol}
```

`{symbol}` is the uppercase ticker.

Example:

```
ibkr://quote/AAPL
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
      "call_bid": 12.80, "call_ask": 13.05, "call_last": 12.90, "call_iv": null, "call_oi": 18420,
      "put_bid": 1.85, "put_ask": 1.92, "put_last": 1.88, "put_iv": null, "put_oi": 9215
    }
  ]
}
```

The `is_atm: true` row is the strike closest to spot. Greeks are populated
only when IBKR delivers them; per-leg quotes may be `null` when the option
contract cannot be resolved without conID hydration.

Per-leg fields:

- `call_oi`, `put_oi` — option open interest (int64), best-effort from
  tick types 27 (`callOpenInterest`) and 28 (`putOpenInterest`) on the
  same per-leg subscription that drives bid/ask/IV. `null` when the
  gateway didn't deliver the tick within the chain fill budget — common
  off-hours or for illiquid wing strikes. Never zero-substituted.

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

Daily granularity only; intraday bars are not implemented.

## scan

Three invocations share this result shape — preset, ad-hoc, and list-only differ in inputs.

`ibkr scan top-movers --json` (preset shorthand):

```json
{
  "preset": "top-movers",
  "type": "TOP_PERC_GAIN",
  "as_of": "2026-05-09T14:32:09Z",
  "rows": [
    {
      "rank": 1,
      "symbol": "NVDA",
      "currency": "USD",
      "last": 458.02,
      "prev_close": 434.50,
      "change": 23.52,
      "change_pct": 5.41,
      "volume": 12345678,
      "iv": 0.342,
      "week_52_high": 465.10,
      "week_52_low": 290.50,
      "comment": ""
    }
  ]
}
```

`ibkr scan --type TOP_PERC_GAIN --exchange STK.NASDAQ --limit 25 --json` (ad-hoc): same row shape, `preset` is empty.

Row fields:

- `rank` — IBKR scanner ranking (0-indexed in the response, 1-indexed in the text renderer for readability).
- `symbol` — ticker.
- `currency` — ISO-4217 code for `last`/`prev_close`/`change`/`week_52_*`. Populated from the gateway's scannerData row (the contract currency comes back alongside symbol/exchange). Omitted by daemons older than v0.13.0; consumers should treat empty as "unknown" and fall back to `$`.
- `last`, `prev_close`, `change`, `change_pct`, `volume` — populated by a follow-up market-data subscribe the daemon issues per row. IBKR's scanner subscription itself returns *only* rank + symbol (by protocol design — the leaderboard is a separate service from market data), so the daemon enriches each row in parallel. Nil fields mean the gateway didn't deliver the corresponding tick within the per-row enrichment window — common off-hours, especially for IV.
- `iv` — underlying's averaged option implied volatility (from generic tick 106). Stored as a fraction: 0.234 = 23.4%. Present only when the symbol has actively-traded options *and* the gateway delivers the tick within the window.
- `week_52_high`, `week_52_low` — 52-week price range (from generic tick 165). Used to gauge where the current price sits within the year's extremes.
- `comment` — raw scanner-side comment field. Empty for most scan types; carries the IBKR-side metric only for a few specialty scans.

`type` always echoes the underlying `scanCode` so the caller can attribute rows even without `preset`. **The scanner ranks server-side; per-row data is fetched client-side.** This is by IBKR's design — the TWS Market Scanner GUI works the same way.

## scan-list

`ibkr scan list --json`

```json
{
  "presets": [
    { "name": "gappers", "type": "HIGH_OPEN_GAP", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "high-iv-rank", "type": "HIGH_OPT_IMP_VOLAT_OVER_HIST", "exchange": "STK.US", "limit": 20 },
    { "name": "most-active", "type": "MOST_ACTIVE", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "top-losers", "type": "TOP_PERC_LOSE", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "top-movers", "type": "TOP_PERC_GAIN", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "unusual-opt-vol", "type": "HOT_BY_OPT_VOLUME", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "unusual-vol", "type": "HOT_BY_VOLUME", "exchange": "STK.US.MAJOR", "limit": 20 }
  ]
}
```

User-defined `[scans.<name>]` blocks in `config.toml` replace the defaults entirely (no merge). Always run `scan list` if unsure what's configured.

## scan-params

`ibkr scan params --instrument STK --json` (catalog dump; use to discover valid `scanCode` and `locationCode` strings before composing an ad-hoc scan):

```json
{
  "instruments": [
    { "name": "US Stocks", "type": "STK" },
    { "name": "US Equity ETFs", "type": "ETF.EQ.US" }
  ],
  "locations": [
    { "code": "STK.US", "display_name": "US Stocks" },
    { "code": "STK.US.MAJOR", "display_name": "Listed/NASDAQ" },
    { "code": "STK.NASDAQ", "display_name": "NASDAQ" }
  ],
  "scan_types": [
    { "code": "TOP_PERC_GAIN", "display_name": "Top % Gainers", "instruments": ["STK", "ETF"] },
    { "code": "HIGH_OPT_IMP_VOLAT_OVER_HIST", "display_name": "High Option Imp Vol Over Historical", "instruments": ["STK"] }
  ],
  "as_of": "2026-05-12T06:45:00Z"
}
```

- `instruments` — instrument-group tokens. Use `instruments[].type` as the `--instrument` filter value (e.g. `STK`, `OPT`, `ETF`).
- `locations` — every `locationCode` the gateway accepts. Pass `code` as the ad-hoc scan's `--exchange`.
- `scan_types` — every `scanCode`. Pass `code` as the ad-hoc scan's `--type`. `scan_types[].instruments` lists which instrument-types the scan applies to (filter the list to scans valid for your target).
- Add `--raw` to attach the full XML in a `raw_xml` field (only when you need a less-common field like filter values or category tags).

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
  show the error to the user.
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
- `data_type` — `live`, `delayed`, `frozen`, or `delayed-frozen`. If a
  user asks about a quote and `data_type != "live"`, mention it.

A full set of additional metadata fields (`alternates`, `tls_origin`,
`server_version`, `daemon_started`) is also returned but rarely
actionable; show them only when the user is debugging discovery.

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

## breadth

`ibkr breadth --json` — S&P 500 stocks-above-50DMA reading. The daemon
computes the S&P DJI S5FI metric locally from constituent daily closes
(IBKR doesn't redistribute the index on retail subscriptions).

```json
{
  "state": "ready",
  "value": 62.4,
  "history": [
    {"date": "2026-04-09", "value": 58.2},
    {"date": "2026-04-10", "value": 59.6}
  ],
  "source": "Computed from S&P-500 constituent daily bars (IBKR HMDS)",
  "method": "constituent-fanout-50/200dma-hl",
  "as_of": "2026-05-09T20:35:01Z",
  "data_type": "live"
}
```

Field meanings:

- `state` — `"cold"` | `"computing"` | `"ready"` | `"degraded"`. Branch on
  this, not on `value == 0`. `cold` means the engine hasn't been kicked
  yet (rare; brief window at daemon start). `computing` means a refresh
  is in flight — `value` is `0` and `history` is empty during the
  first-ever build, which takes ~60 min due to IBKR's historical-data
  pacing limit. `ready` means the value is real. `degraded` means the
  engine refused to persist because constituent coverage dropped below
  the safety threshold (the previous good value still serves).
- `value` — percentage of S&P 500 constituents trading above their own
  50-day SMA. Range `[0, 100]`. Zero is meaningful only when `state ==
  "ready"`, which is impossible in practice — interpret `value: 0` on
  any other state as "no data yet."
- `history` — trailing daily series, oldest first. Length capped by
  `--days` (default 30, max 90). Empty during cold start.
- `source`, `method` — provenance strings the renderer can display
  verbatim. Method token: `constituent-fanout-50/200dma-hl`.
- `data_type` — gateway feed state (`live` / `delayed` / `frozen` /
  `delayed-frozen`) when the headline was captured. Omitted when no
  feed notice has arrived yet.

Spec rule of thumb (apply on the consumer side, not derived on the wire):
`> 55` healthy participation; `40–55` watch; `< 40` with SPX within 3% of
its 52-week high is the classic late-cycle divergence.

## gamma

`ibkr gamma --json` — dealer-gamma market-structure snapshot for SPY, SPX,
or the default SPY+SPX view. The result is heavy (multi-minute fan-out
across hundreds of legs); the first caller of an NY trading day kicks a
background job, subsequent callers within the session receive the cached
result instantly.

**MCP params** (`ibkr_gamma`):
- `scope` — `"spy" | "spx" | "spy+spx"`. Default `"spy+spx"`. CLI alias is `--only`.
- `wait_ms` — integer ms to block on an in-flight compute. Default 0.
- `force` — boolean; diagnostics-only — ignore cached result. Default false.
- `include_profiles` — boolean; default false. Include full sweep profile
  arrays only when charting.

**CLI-only flags** (no MCP equivalent — text-mode rendering controls):
- `--explain` — extra methodology, per-bucket horizon breakdown, scaling caveat. JSON unchanged.
- `--no-wait` — CLI sugar for `wait_ms: 0`.
- `--json` — switch the CLI from text to JSON output.
- `--profiles` — with `--json`, include full sweep profile arrays. Default
  JSON strips them so agents and shell tooling get compact payloads.

Computing (first call of the day):

```json
{
  "status": "computing",
  "started_at": "2026-05-09T13:30:14Z",
  "eta_seconds": 180,
  "progress": 22
}
```

Cold (closed market with unusable persisted cache):

```json
{
  "status": "cold",
  "cold_reason_code": "persisted_cache_rejected",
  "cold_reason": "persisted gamma cache for spy+spx was rejected: per_index[SPX]: zero-gamma invalid result: 890 GEX legs but zero gamma_total_abs/profile/top_strikes",
  "cold_action": "Run `ibkr gamma --force` for a diagnostic off-hours recompute, or call again during the next U.S. equity-options session."
}
```

Ready (combined scope, subsequent calls):

```json
{
  "status": "ready",
  "started_at": "2026-05-09T13:30:14Z",
  "result": {
    "scope": "spy+spx",
    "summary": {
      "primary_statement": "Zero-gamma: SPY $581.40; SPX none in $4615.50-$6244.50 (long-gamma). No combined zero is computed across SPY/SPX price scales.",
      "zero_gamma_status": "mixed",
      "regime": "mixed",
      "confidence": "estimate",
      "not_advice": "Market-structure context only; not a trade recommendation.",
      "per_index": {
        "SPY": {"underlying": "SPY", "spot_underlying": 583.21,
                "zero_gamma": 581.40, "zero_gamma_status": "crossing",
                "regime": "transition_gamma", "leg_count": 1208,
                "priced_leg_count": 1280},
        "SPX": {"underlying": "SPX", "spot_underlying": 5430.0,
                "zero_gamma_status": "none_in_window",
                "regime": "long_gamma", "leg_count": 1994,
                "priced_leg_count": 2150}
      }
    },
    "gamma_total_abs": 6.0e9,
    "gamma_total_abs_convention": "sign-agnostic",
    "regime_agreement": "disagree",
    "top_strikes": [
      {"underlying": "SPX", "trading_class": "SPXW", "strike": 5400.0,
       "expiry": "2026-06-19", "right": "C",
       "abs_gex": 7.0e9, "open_interest": 12450}
    ],
    "per_index": {
      "SPY": {"scope": "spy", "spot_underlying": 583.21, "zero_gamma": 581.40, "...": "..."},
      "SPX": {"scope": "spx", "spot_underlying": 5430.0, "gamma_sign": "positive", "...": "..."}
    },
    "expirations": ["2026-05-16", "2026-05-23", "2026-05-30",
                    "2026-06-06", "2026-06-13", "2026-06-19"],
    "leg_count": 3202,
    "priced_leg_count": 3430,
    "derived_iv_legs": 0,
    "leg_diagnostics": {
      "total": {"priced_legs": 3430, "oi_positive_legs": 3202,
                "gamma_positive_legs": 3430,
                "abs_gex_positive_legs": 3202},
      "by_underlying": {
        "SPY": {"priced_legs": 1280, "oi_positive_legs": 1208,
                "gamma_positive_legs": 1280,
                "abs_gex_positive_legs": 1208},
        "SPX": {"priced_legs": 2150, "oi_positive_legs": 1994,
                "gamma_positive_legs": 2150,
                "abs_gex_positive_legs": 1994}
      },
      "by_trading_class": {
        "SPY": {"priced_legs": 1280, "oi_positive_legs": 1208,
                "gamma_positive_legs": 1280,
                "abs_gex_positive_legs": 1208},
        "SPXW": {"priced_legs": 1900, "oi_positive_legs": 1750,
                 "gamma_positive_legs": 1900,
                 "abs_gex_positive_legs": 1750}
      }
    },
    "warning_details": [],
    "methodology_citations": [
      "Perfiliev (2022) — BS-sweep baseline",
      "Derman / Daglish-Hull-Suo — sticky-moneyness skew dynamics"
    ],
    "params": {"expiry_count": 6, "strike_width_pct": 0.10,
               "sweep_range_pct": 0.15, "worker_count": 4},
    "source": "computed from IBKR SPY+SPX option chains",
    "method": "bs-gamma-profile-v3-stickymoneyness-0dte-split",
    "as_of": "2026-05-09T13:32:54Z",
    "duration_ms": 158420
  }
}
```

Field meanings:

- `status` — `"cold"` | `"computing"` | `"ready"` | `"error"`. The CLI
  blocks on the compute by default; pass `--no-wait` for the polling
  shape. `cold` means no compute has been kicked this NY trading session
  and none is in flight (first caller will kick); `computing` means a job
  is in flight (use `eta_seconds` / `progress` for the renderer);
  `ready` means `result` is populated; `error` means the last compute
  failed and `error` carries the classified reason.
- `cold_reason_code` / `cold_reason` / `cold_action` — present only on
  `status: "cold"` when the daemon knows why no value is serveable (for
  example, a persisted cache existed but failed data-quality validation).
- `result.scope` — `"spy"` | `"spx"` | `"spy+spx"`. Discriminator for
  combined vs single-underlying envelopes.
- `result.summary` — agent-preferred readout. Start here. It tells you
  which zero-gamma crossing, if any, was identified; whether the signed
  profile stayed long-/short-gamma through the swept range; confidence;
  and the non-advisory caveat.
- In combined scope, **there is no top-level combined zero-gamma price**.
  SPY and SPX use different price scales, so consume
  `result.summary.per_index.SPY` / `.SPX` (or `result.per_index`) for
  per-underlying spot, zero-gamma, swept range, and regime.
- `result.leg_count` — legs with non-zero OI-weighted GEX. This is the
  count that matters for dealer-gamma magnitude and profile.
- `result.priced_leg_count` — legs that priced / fit IV. This can exceed
  `leg_count` when IBKR supplied IV but not open interest; those legs help
  skew fitting but do not contribute to GEX.
- `result.gamma_total_abs` — sign-agnostic magnitude signal:
  `Σ |Γ| × OI × 100 × spot² × 0.01`, summed across both indices on
  combined scope. SPX dominates ~75–80% of the sum because of the S²
  scaling. **More robust than `zero_gamma` when the dealer-sign
  assumption may invert** (covered-call ETF flow, autocall barrier
  proximity). `gamma_total_abs_convention` names the sign-handling
  ("sign-agnostic" today).
- `result.regime_agreement` — on combined scope, one of
  `"agree:long-gamma"` / `"agree:short-gamma"` /
  `"agree:transition-gamma"` / `"disagree"` / `""` (no data).
  `"disagree"` means SPY and SPX modeled regimes differ; show the
  per-index details instead of forcing a single headline.
- `result.per_index` — populated only on combined scope. Each entry
  (`"SPY"`, `"SPX"`) is a fully-formed single-underlying
  `GammaZeroComputed` so renderers can recurse for per-underlying
  detail. Profiles are stripped from default CLI JSON and MCP responses;
  pass `--profiles` / `include_profiles: true` when charting.
- `result.top_strikes` — top-N strikes by absolute gamma notional,
  merged across both indices on combined scope (sorted by `abs_gex`
  descending; SPX rows dominate by structure). Each row carries
  `underlying` (`"SPY"`/`"SPX"`) so the renderer can label per-row.
- `result.derived_iv_legs` — legs whose IV fell back to the
  Newton-Raphson BS-inversion path because the gateway never pushed a
  model-computation tick. Compare to `priced_leg_count`.
- `result.leg_diagnostics` — leg-quality funnel for the current result:
  priced legs, legs with positive open interest, legs with positive
  Black-Scholes gamma at the snapshot spot, and legs with non-zero
  OI-weighted absolute GEX. Splits are provided by underlying and by
  trading class (`SPX` vs `SPXW`) so off-hours failures can identify
  whether pricing, OI, or gamma contribution disappeared.
- `result.warning_details` — non-fatal data-quality/methodology issues
  as scoped prose: `{code, scope, severity, message, impact, action}`.
  Do not look for raw warning tokens in JSON.
- `result.methodology_citations` — short bibliography backing the
  methodology disclosure. Surface verbatim in `--explain`.

**Scaling caveat:** SPY contributes ~1/100 of SPX dollar-gamma per
equivalent leg (S² scaling). Combined `gamma_total_abs` sums the books,
but zero-gamma levels stay per-index.

**Treat the number as a regime hint, not a precise level.** Full
methodology lives in `docs/specs/risk-regime-dashboard.md`.

## regime

`ibkr regime --json` — single-call risk-regime dashboard: all eight
indicator rows in one compact JSON envelope. The default JSON/MCP shape
leads with `summary`, `composite`, and `warning_details`, then raw
measurements, streaks, and quality provenance. Long methodology `notes`
and breadth history are omitted by default; use `ibkr regime --json
--explain` when a JSON consumer explicitly needs the spec prose.

**MCP params** (`ibkr_regime`): none — the envelope always carries
all eight indicator rows.

**CLI flags**:
- `--explain` — show per-row streak markers, quality blocks, methodology disclosures; with `--json`, include full notes/history.
- `--watch` / `--rate` — auto-poll in place.
- `--log PATH` — append each snapshot to a JSONL trace file.

```json
{
  "as_of": "2026-05-09T14:32:09Z",
  "summary": {
    "label": "Normal regime",
    "evidence": "4 green clusters / 1 yellow cluster / 1 unranked cluster",
    "indicator_evidence": "6 green / 1 yellow / 1 unranked",
    "punch_line": "volatility term structure, vol-of-vol, cash credit spreads, funding spread, dealer gamma, and breadth are constructive; ETF credit proxy is mixed; FX carry proxy is unavailable.",
    "confidence": "medium",
    "not_advice": "Regime read only; not investment advice or a trade recommendation."
  },
  "vix_term_structure": {
    "status": "ok",
    "band": "green",
    "band_reason": "<0.92 contango",
    "thresholds": {"label": "vix_term_structure_v1", "green": "VIX/VIX3M < 0.92",
                   "yellow": "0.92 <= VIX/VIX3M < 1.00", "red": "VIX/VIX3M >= 1.00",
                   "heuristic": true, "pending_backtest": true},
    "as_of": {"label": "live", "freshness": "live", "source": "Cboe VIX and VIX3M via IBKR index market data",
              "time": "2026-05-09T14:32:09Z"},
    "vix": 14.82,
    "vix3m": 16.41,
    "ratio": 0.903,
    "data_type": "live",
    "vix_prev_close": 15.04,
    "vix_change_pct": -1.46,
    "vix_quality": {"as_of": "2026-05-09T14:32:09Z", "freshness_class": "live",
                    "confidence": "firm", "source": "VIX tick"},
    "vix3m_quality": {"as_of": "2026-05-09T14:32:09Z", "freshness_class": "frozen",
                      "confidence": "firm", "source": "VIX3M tick (thin CBOE; off-hours typically frozen)"},
    "streak": {"band": "green", "sessions": 4, "since": "2026-05-06"}
  },
  "vol_of_vol": {
    "status": "ok",
    "band": "green",
    "band_reason": "<90 vol-of-vol",
    "thresholds": {"label": "vvix_daily_v1", "green": "VVIX < 90",
                   "yellow": "90 <= VVIX < 110", "red": "VVIX >= 110",
                   "heuristic": true, "pending_backtest": true},
    "as_of": {"label": "close D-1", "freshness": "daily_close",
              "source": "Cboe official VVIX daily close", "date": "2026-05-08"},
    "symbol": "VVIX",
    "last": 82.4,
    "change_20d_pct": -3.1,
    "as_of_date": "2026-05-08",
    "source": "Cboe official VVIX daily time series",
    "value_quality": {"as_of": "2026-05-08T00:00:00Z", "freshness_class": "derived",
                      "confidence": "firm", "source": "Cboe VVIX daily close"},
    "streak": {"band": "green", "sessions": 3, "since": "2026-05-07"}
  },
  "hyg_spy_divergence": {
    "status": "ok",
    "hyg_price": 78.42,
    "hyg_50dma": 78.10,
    "spy_price": 583.21,
    "spy_52w_high": 605.78,
    "spy_prev_close": 581.94,
    "spy_change": 1.27,
    "spy_change_pct": 0.218,
    "hyg_data_type": "live",
    "hyg_quality": {"as_of": "2026-05-09T14:32:09Z", "freshness_class": "live",
                    "confidence": "firm", "source": "HYG tick (ARCA)"},
    "streak": {"band": "green", "sessions": 12, "since": "2026-04-28"}
  },
  "credit_spreads": {
    "status": "ok",
    "hy_oas": 3.62,
    "ig_oas": 1.05,
    "hy_ig_spread": 2.57,
    "hy_oas_20d_change": 0.08,
    "as_of_date": "2026-05-08",
    "source": "FRED/St. Louis Fed official ICE BofA OAS CSV",
    "hy_oas_quality": {"as_of": "2026-05-08T00:00:00Z", "freshness_class": "derived",
                       "confidence": "firm", "source": "FRED BAMLH0A0HYM2 HY OAS"},
    "spread_quality": {"as_of": "2026-05-08T00:00:00Z", "freshness_class": "derived",
                       "confidence": "firm", "source": "HY OAS minus IG OAS"},
    "streak": {"band": "green", "sessions": 4, "since": "2026-05-06"}
  },
  "funding_stress": {
    "status": "ok",
    "cp_3m_rate": 5.34,
    "tbill_3m_rate": 5.20,
    "spread_bps": 14.0,
    "as_of_date": "2026-05-08",
    "source": "FRED/St. Louis Fed official Federal Reserve CP and T-bill series",
    "spread_quality": {"as_of": "2026-05-08T00:00:00Z", "freshness_class": "derived",
                       "confidence": "firm", "source": "90-day AA financial CP minus 3-month T-bill"},
    "streak": {"band": "green", "sessions": 8, "since": "2026-04-30"}
  },
  "usd_jpy": {
    "status": "unavailable",
    "symbol": "USD.JPY",
    "error_message": "USD.JPY: gateway delivered no FX tick (check IDEALPRO entitlement)"
  },
  "gamma_zero": {
    "status": "ok",
    "envelope": {"status": "ready", "result": {"...": "see gamma schema"}},
    "zero_gamma_quality": {"as_of": "2026-05-09T13:32:54Z", "freshness_class": "modelled",
                           "confidence": "proxy",
                           "source": "bs-gamma-profile-v3-stickymoneyness-0dte-split"},
    "streak": {"band": "green", "sessions": 7, "since": "2026-04-30"}
  },
  "breadth": {
    "status": "ok",
    "envelope": {"state": "ready", "pct_above_50dma": 62.4, "...": "see breadth schema"},
    "pct_above_50dma": 62.4,
    "pct_above_200dma": 71.0,
    "value_quality": {"as_of": "2026-05-09T13:00:00Z", "freshness_class": "derived",
                      "confidence": "estimate", "source": "constituent-fanout-50/200dma-hl"},
    "streak": {"band": "green", "sessions": 31, "since": "2026-04-08"}
  },
  "composite": {
    "verdict": "Normal regime",
    "green_count": 6,
    "yellow_count": 1,
    "red_count": 0,
    "ranked_count": 7,
    "unranked_count": 1,
    "cluster_green_count": 4,
    "cluster_yellow_count": 1,
    "cluster_red_count": 0,
    "cluster_ranked_count": 5,
    "cluster_unranked_count": 1
  },
  "warning_details": [],
  "spec_doc": "docs/specs/risk-regime-dashboard.md"
}
```

Field meanings:

- `summary` is the agent-preferred readout. Start here: `label` is the
  non-advisory regime label, `evidence` is the cluster traffic-light
  balance, `indicator_evidence` is the raw row balance, `punch_line`
  explains the current read in one sentence, and `confidence` reflects
  evidence coverage rather than forecast certainty.
- Each indicator row carries a `status` field:
  `"ok"` (real fresh measurement) | `"stale"` (gateway labeled it
  delayed/frozen) | `"computing"` (heavy compute in flight; poll
  again) | `"unavailable"` (feed not entitled on this account; see
  `warning_details`) | `"error"` (`error_message` carries the reason).
- Each indicator row also carries compact agent metadata:
  `band` (`green` / `yellow` / `red` / `unranked`), `band_reason`,
  `thresholds` (`label`, per-band text, `heuristic`, `pending_backtest`),
  and `as_of` (`label`, `freshness`, `source`, optional `time` / `date`,
  optional `age_seconds`). Use `as_of.label` for the table freshness badge.
- Numerical fields are pointer-typed: `null` = "didn't arrive in the
  fetch budget," never zero-substituted.
- `fields_missing` lists advisory sub-fields (e.g. `spy_52w_high`,
  `hyg_50dma`, `series_date_mismatch`) that didn't land or aligned
  imperfectly — the row's primary measurement still landed, so dim those
  sub-cells without re-classifying the whole row as `error`.
- `warning_details` carries scoped `{message, impact, action}` prose for
  stale, computing, unavailable, and error rows. Prefer this over parsing
  row-level error strings.
- `notes` are omitted from default JSON/MCP for compactness. Use
  `--json --explain` or CLI `--explain` for full methodology prose.
- `composite` is the daemon-side rollup matching what the CLI prints
  above its indicator table. Raw row counts (`green_count`,
  `yellow_count`, `red_count`, `ranked_count`, `unranked_count`) sit
  beside cluster counts (`cluster_*`) so equity-vol or credit sub-signals
  do not double-count as independent macro confirmations. `verdict` is one of
  "Normal regime", "Elevated stress watch", "Stress signal present",
  "Broad stress regime", "Full risk-off conditions", "Insufficient
  signal — too few indicators ranked", "No ranked indicators — see
  rows below for state". Renderers showing their own band coloring
  can ignore this and re-tally from per-row `status` + measurements.
- Each row's `streak: {band, sessions, since}` counts consecutive NY
  trading sessions in the current band. Nil on computing / unavailable /
  error rows (streak freezes rather than resets). The CLI shows
  this inline ("yellow · day 3"); MCP consumers can render the same.
- Each row's `*_quality` objects (`vix_quality`, `hyg_quality`,
  `last_quality`, `zero_gamma_quality`, `value_quality`, etc.) carry
  per-scalar provenance: `freshness_class` (`live` / `frozen` /
  `derived` / `modelled`), `confidence` (`firm` / `estimate` / `proxy`),
  `as_of`, and a `source` description (e.g. `"VIX tick"`, `"SPY 252d
  max(High) fallback"`, `"perfiliev-bs-sweep-v1"`). The CLI's
  `--explain` view consumes these directly.
- `gamma_zero.envelope` and `breadth.envelope` carry the full
  [gamma](#gamma) / [breadth](#breadth) result shapes; consumers that
  already know those schemas can re-use the same renderers.
- `spec_doc` always points at the canonical methodology reference.
  Surface as a deep link when explaining a band edge to the user.

Use `ibkr regime --explain` to get the spec's per-indicator threshold
prose printed alongside each row, or `ibkr regime --json --explain` for
the full JSON methodology payload.
