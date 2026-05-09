---
name: ibkr
description: Query Interactive Brokers via the local `ibkr` CLI. Use when the user asks
  about their IBKR account, positions, P&L, market quotes, option chains, daily price
  history, or running a market scan. Read-only — never attempts to place orders.
allowed-tools: Bash(ibkr account*) Bash(ibkr positions*) Bash(ibkr quote*)
  Bash(ibkr chain*) Bash(ibkr history*) Bash(ibkr scan*) Bash(ibkr status*)
  Bash(ibkr version*)
---

## When to use

If the user asks about holdings, cash, buying power, P&L, a specific stock or ETF
quote, an option chain, daily history, or wants to scan the market, run the
relevant `ibkr` subcommand with `--json` and parse the output.

If the user asks anything that implies *placing* an order (buy, sell, cancel,
"close my position"), refuse and explain that `ibkr` is read-only in this
release. Do not invent or simulate trade execution.

## Output discipline

- Always run with `--json` when parsing programmatically, then present results
  as a clean Markdown table.
- Always surface the `data_type` field (`live` / `delayed` / `frozen`). If it
  isn't `live`, mention it in the answer so the user knows the prices may not
  reflect the current market.
- Never claim an order was placed. The CLI cannot trade in v1.
- Never fabricate Greeks or implied volatility. If the JSON returns
  `"iv": null` and `"iv_status": "unavailable"`, say so plainly.

## Commands

| Command | Purpose | Schema |
|---|---|---|
| `ibkr status` | Daemon + gateway health (run this first if anything fails) | [schemas.md#status](schemas.md#status) |
| `ibkr account` | Account summary (NLV, BP, cash, margin) | [schemas.md#account](schemas.md#account) |
| `ibkr positions` | Open positions; stocks and options separated | [schemas.md#positions](schemas.md#positions) |
| `ibkr quote SYM[,SYM…]` | Snapshot quotes for one or many symbols | [schemas.md#quote](schemas.md#quote) |
| `ibkr quote SYM YYMMDD C\|P STRIKE` | Single-option snapshot | [schemas.md#quote](schemas.md#quote) |
| `ibkr quote SYM --watch` | Streaming ticks (Ctrl-C to stop) | streaming frames per [schemas.md#frame](schemas.md#frame) |
| `ibkr chain SYM --expiry YYYY-MM-DD` | Option chain ATM ± width | [schemas.md#chain](schemas.md#chain) |
| `ibkr history SYM` | Daily OHLCV bars | [schemas.md#history](schemas.md#history) |
| `ibkr scan <preset>` | Run a configured scanner preset | [schemas.md#scan](schemas.md#scan) |
| `ibkr scan list` | Enumerate configured scanner presets | [schemas.md#scan-list](schemas.md#scan-list) |

Add `--json` to any command for parseable output. Flags can come after positional
symbols — the CLI hoists them automatically.

### Per-command flags

- `ibkr status [--json]`
- `ibkr account [--json]`
- `ibkr positions [--symbol SYM] [--type stk|opt] [--sort alpha|pnl|value] [--by underlying] [--json]`
  - `--by underlying` groups stock + option legs per underlying with group P&L totals; the JSON `by_underlying` array is always populated regardless of this flag.
- `ibkr quote SYM[,SYM…] [--timeout 5s] [--json]`
- `ibkr quote SYM --watch [--rate 250ms] [--json]` — only one symbol at a time
- `ibkr chain SYM --expiry YYYY-MM-DD [--width 5] [--side calls|puts|both] [--json]`
- `ibkr history SYM [--days 90] [--json]` — calendar lookback; daily bars only in v1.0
- `ibkr scan <preset> [--limit N] [--json]`
- `ibkr scan list [--json]`

## Errors

The CLI exits with code 1 on a daemon-side error. The error line on stderr has
a code prefix when applicable:

- `daemon_unavailable` → `ibkrd` could not start. The IB Gateway is probably not
  running, or the user has not configured a profile.
- `gateway_unavailable` → connection to IB Gateway lost. Reconnects happen
  automatically; advise the user to retry in a few seconds. The CLI also prints
  a hint pointing at `ibkr status`.
- `symbol_inactive` → IBKR has flagged the symbol as delisted or halted. Do
  *not* substitute a similar symbol; surface the error.
- `timeout` → the gateway didn't respond within the snapshot window. Suggest
  retrying with `--timeout 10s` (quote) or reducing `--days` (history).
- `bad_request` → wrong arguments or unknown preset. Show the user the usage
  hint emitted on stderr.
- `trading_disabled` → the user (or you) tried to call an order verb. v1
  cannot trade by design. Acknowledge and suggest using IBKR's TWS/web app
  instead.

## Worked examples

### Position summary
```
$ ibkr positions --json
{
  "data_type": "live",
  "as_of": "2026-05-09T14:32:09Z",
  "stocks": [
    {"symbol": "AAPL", "quantity": 100, "avg_cost": 192.40, "mark": 207.88,
     "unrealized_pnl": 1548.0, "realized_pnl": 0}
  ],
  "options": [
    {"symbol": "AAPL", "right": "C", "expiry": "20260619", "strike": 215,
     "quantity": 5, "avg_cost": 6.82, "mark": 9.40, "unrealized_pnl": 1290}
  ],
  "by_underlying": [
    {"underlying": "AAPL", "stock": {...}, "options": [...],
     "group_market_value": 25400.0, "group_unrealized_pnl": 2838.0}
  ]
}
```

Render to the user as two compact tables (stocks, options) with money formatted
as currency and totals. Always mention the `data_type` if it is not `live`. If
the user asks "what's my exposure to AAPL?" or "how am I doing per name?",
reach for the `by_underlying` grouping.

### Quote snapshot
```
$ ibkr quote AAPL --json
{ "symbol": "AAPL", "bid": 207.85, "ask": 207.88, "last": 207.86,
  "bid_size": 100, "ask_size": 200, "volume": 12400000,
  "iv": null, "iv_status": "unavailable", "data_type": "live", ... }
```

Present as: `AAPL — $207.86 (bid 207.85 × 100 / ask 207.88 × 200) · vol 12.4M · live`.
If `data_type` is not `live`, prepend a short warning. Sizes and volume can be
`null` (omitted) when the gateway didn't deliver them.

### Daily history
```
$ ibkr history AAPL --days 30 --json
{
  "symbol": "AAPL",
  "days": 30,
  "data_type": "live",
  "bars": [
    {"date": "2026-04-09", "open": 195.20, "high": 198.40, "low": 194.10, "close": 197.65, "volume": 51234100},
    ...
  ]
}
```

The bar count typically lags the requested calendar window because non-trading
days are skipped. Daily granularity only in v1.0.

### What about implied volatility?
The CLI never derives or estimates IV. If `iv_status` is `"unavailable"`, the
gateway didn't deliver tick 106 for that contract — most stock snapshots do
not include IV. Don't substitute historical vol or any proxy.
