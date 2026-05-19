---
name: ibkr
description: Query Interactive Brokers via the local `ibkr` CLI. Use when the user asks
  about their IBKR account, positions, P&L, market quotes, option chains, daily price
  history, running a market scan, sizing a planned trade by fixed-fractional risk, or
  checking the market's risk regime (S&P 500 breadth, SPY dealer zero-gamma, the
  five-indicator regime dashboard). Read-only — never attempts to place orders.
allowed-tools: Bash(ibkr account*) Bash(ibkr positions*) Bash(ibkr quote*)
  Bash(ibkr chain*) Bash(ibkr history*) Bash(ibkr scan*) Bash(ibkr size*)
  Bash(ibkr breadth*) Bash(ibkr gamma*) Bash(ibkr regime*)
  Bash(ibkr status*) Bash(ibkr version*)
---

## When to use

If the user asks about holdings, cash, buying power, P&L, a specific stock or ETF
quote, an option chain, daily history, or wants to scan the market, run the
relevant `ibkr` subcommand with `--json` and parse the output.

If the user asks about the *market environment* — "is the market risky today?",
"what's the regime?", "where's dealer gamma?", "how broad is the rally?" — reach
for `ibkr regime` (all five indicators in one call), `ibkr breadth` (S&P 500
stocks-above-50DMA), or `ibkr gamma` (SPY dealer zero-gamma). The threshold
bands are intentionally not green/yellow/red-coded on the wire; the consumer
applies the spec's tunable cuts.

If the user asks anything that implies *placing* an order (buy, sell, cancel,
"close my position"), refuse and explain that `ibkr` is read-only in this
release. Do not invent or simulate trade execution.

## Output discipline

- Always run with `--json` when parsing programmatically, then present results
  as a clean Markdown table.
- Always surface the `data_type` field (`live` / `delayed` / `frozen`). If it
  isn't `live`, mention it in the answer so the user knows the prices may not
  reflect the current market.
- Never claim an order was placed. The CLI cannot trade.
- Never fabricate Greeks or implied volatility. If the JSON returns
  `"iv": null` and `"iv_status": "unavailable"`, say so plainly. The same
  applies to `delta`/`gamma`/`theta`/`vega` on option positions and to
  every `*_ccy` / `fx_rate` field: nil = "the gateway didn't deliver
  this", never substitute zero or a derived value.

## Commands

| Command | Purpose | Schema |
|---|---|---|
| `ibkr status` | Daemon + gateway health (run this first if anything fails) | [schemas.md#status](schemas.md#status) |
| `ibkr account` | Account summary (NLV, BP, cash, margin, daily P&L); add `--watch` for in-place refresh | [schemas.md#account](schemas.md#account) |
| `ibkr positions` | Open positions (stocks + options) with per-position daily P&L; add `--watch` for in-place refresh | [schemas.md#positions](schemas.md#positions) |
| `ibkr quote SYM[,SYM…]` | Snapshot quotes for one or many symbols | [schemas.md#quote](schemas.md#quote) |
| `ibkr quote SYM YYMMDD C\|P STRIKE` | Single-option snapshot | [schemas.md#quote](schemas.md#quote) |
| `ibkr quote SYM --watch` | Streaming ticks (Ctrl-C to stop) | streaming frames per [schemas.md#frame](schemas.md#frame) |
| `ibkr chain SYM` | List available option expiries for the underlying | [schemas.md#chain-expiries](schemas.md#chain-expiries) |
| `ibkr chain SYM --expiry YYYY-MM-DD` | Option chain ATM ± width for that expiry | [schemas.md#chain](schemas.md#chain) |
| `ibkr history SYM` | Daily OHLCV bars | [schemas.md#history](schemas.md#history) |
| `ibkr scan <preset>` | Run a configured scanner preset | [schemas.md#scan](schemas.md#scan) |
| `ibkr scan list` | Enumerate configured scanner presets | [schemas.md#scan-list](schemas.md#scan-list) |
| `ibkr scan --type SCANCODE --exchange LOCATIONCODE` | Ad-hoc scan without writing a preset to config | [schemas.md#scan](schemas.md#scan) |
| `ibkr scan params [--instrument STK]` | Dump the gateway's scanCode / locationCode catalog | [schemas.md#scan-params](schemas.md#scan-params) |
| `ibkr size --symbol SYM --entry F --stop F` | Fixed-fractional position sizing pegged to live NLV | [schemas.md#size](schemas.md#size) |
| `ibkr breadth` | S&P 500 stocks-above-50DMA reading (the S5FI metric, computed locally) | [schemas.md#breadth](schemas.md#breadth) |
| `ibkr gamma` | SPY dealer zero-gamma estimate (heavy compute; first call per NY trading day kicks a background job) | [schemas.md#gamma](schemas.md#gamma) |
| `ibkr regime` | Risk-regime snapshot: all five indicators (VIX term, HYG/SPY, USD/JPY, SPY gamma, SPX breadth) in one call | [schemas.md#regime](schemas.md#regime) |
| `ibkr version` | Print version, commit, build date, binary path | — |

Add `--json` to any command for parseable output. Flags can come after positional
symbols — the CLI hoists them automatically.

### Per-command flags

- `ibkr status [--json]`
- `ibkr account [--watch [--rate 1s]] [--json]` — `--watch` re-polls on the rate (default 1s) and redraws in place on a TTY; appends snapshots separated by a dim rule when piped. `--watch` and `--json` are mutually exclusive.
- `ibkr positions [--symbol SYM] [--type stk|opt] [--sort alpha|pnl|value] [--by underlying] [--watch [--rate 1s]] [--json]`
  - `--by underlying` groups stock + option legs per underlying with group P&L totals; the JSON `by_underlying` array is always populated regardless of this flag.
  - `--watch` re-polls on the rate (default 1s); same TTY/pipe behaviour as `account --watch`. Mutually exclusive with `--json`.
- `ibkr quote SYM[,SYM…] [--timeout 5s] [--json]`
- `ibkr quote SYM --watch [--rate 250ms] [--json]` — only one symbol at a time
- `ibkr chain SYM [--no-iv] [--all-expiries] [--json]` — list expiries for the underlying. Per-expiry ATM implied volatility is included **by default** (daemon caches results; second call within ~60 s during RTH is instant), along with `dte` (calendar days to expiration) and `implied_move` / `implied_move_pct` (the 1-σ expected dollar move by expiration, computed `spot × IV × √(DTE/365)`). Top-level `spot` carries the underlying mid the daemon used. `--no-iv` skips the IV fetch (and implied move) when only the date list is needed. `--all-expiries` lifts the default 12-expiry cap (the nearest 12 are picked since the back-half LEAPS are rarely on the decision path). Use this first when the user asks "what expiries are available for X?", "which expiry has the highest IV?", or "what move is the market pricing into earnings?".
- `ibkr chain SYM --expiry YYYY-MM-DD [--width 5] [--side calls|puts|both] [--json]` — full chain table for one expiry. Pick an expiry from the listing above when the user doesn't specify one.
- `ibkr history SYM [--days 90] [--json]` — calendar lookback; daily bars only
- `ibkr scan <preset> [--limit N] [--json]` — built-in presets: `top-movers`, `top-losers`, `most-active`, `unusual-vol`, `gappers`, `high-iv-rank` (IV elevated vs. its own history), `unusual-opt-vol` (hot options flow). User-defined presets may also exist; run `ibkr scan list` first when unsure. **Each row carries enriched data:** `last`, `prev_close`, `change`, `change_pct`, `volume`, `iv` (underlying's averaged option IV, as a fraction — 0.234 = 23.4%), `week_52_high`, `week_52_low`. These are populated by per-row market-data subscriptions the daemon issues automatically (IBKR's scanner subscription itself only returns rank + symbol). Nil fields = gateway didn't deliver that tick within the enrichment window; common off-hours, and `iv` is nil for symbols without actively-traded options. Don't fabricate values — surface "unavailable" honestly when a field is nil. **Off-hours behaviour:** scans that depend on the current session (`gappers`, `top-movers`, `top-losers`, `high-iv-rank`, `unusual-opt-vol`) often time out or return cold-start errors before market open. If the user sees `scanner subsystem did not respond...`, retry once before reporting it as broken — the TWS scanner farm warms lazily and a second attempt frequently succeeds within a few seconds. `most-active` and `unusual-vol` rank against tape and tend to stay warm.
- `ibkr scan list [--json]`
- `ibkr scan --type SCANCODE --exchange LOCATIONCODE [--limit N] [--json]` — **ad-hoc scan, agent-preferred.** Use this when the user asks for a screen that doesn't match any existing preset (e.g. "show me losers on NASDAQ only", "find unusual put activity"). Avoids writing to the user's `config.toml`. Rows are capped at 50. The two magic strings (`scanCode` and `locationCode`) come from the gateway catalog — call `ibkr scan params` first to discover them rather than guessing. **Non-US exchanges:** each row carries `currency` (e.g. `EUR` for `STK.EU.IBIS`, `HKD` for `STK.HK`); render prices with the row's symbol, not a hardcoded `$`.
- `ibkr scan params [--instrument STK] [--raw] [--json]` — gateway scanner catalog. Returns three lists: `instruments` (e.g. STK, OPT, ETF.EQ.US), `locations` (e.g. STK.US.MAJOR, STK.NASDAQ, STK.HK), and `scan_types` (every `scanCode` with display name and the instrument types it's valid for). The catalog varies by gateway version and user permissions — never assume a `scanCode` exists without checking. `--instrument STK` narrows to stock scans. `--raw` adds the full XML (~200 KB–2 MB); skip unless you need a field not in the parsed result.
- `ibkr version [--json]` — print version, commit, build date, binary path; `--json` returns the same data as a structured object (use this when you need to verify the user is running a supported release).
- `ibkr breadth [--days 30] [--json]` — S&P 500 stocks-above-50DMA reading. The daemon computes the S&P DJI S5FI metric locally from 500 constituent daily closes (IBKR doesn't redistribute the index on retail subscriptions). Returns a headline `value` (0–100), a trailing daily series, and a `state` field — branch on `state`, not on `value == 0`. **Cold start (no cache yet) returns `state: "computing"` with `value: 0` and takes ~60 min** because IBKR's historical-data pacing limit caps the fan-out at ~6 names/min sustained; once the cache is built the result is instant on every subsequent call and persists across daemon restarts. Don't hammer the endpoint waiting for the cold start to finish — poll at minute-scale or fall back to telling the user "the breadth engine is still warming; check back in ~an hour." Spec note: > 55 healthy, 40–55 watch, < 40 with SPX at highs is the classic late-cycle divergence — surface the raw number plus the spec band; never color-code on the wire.
- `ibkr gamma [--no-wait] [--force] [--json]` — SPY dealer zero-gamma estimate (Perfiliev convention, BS gamma summed across the 6 nearest non-0DTE expirations within ATM ±10%). SPY (the ETF) is used rather than SPX (the index) because it has continuous extended-hours quoting; the regime signal tracks SPX dealer gamma closely, and the absolute level is SPY-scale (~SPX/10). Compute is heavy — multi-minute fan-out across hundreds of legs. The first caller of an NY trading day kicks a background job and gets `status: "computing"` with an `eta_seconds`; subsequent callers within the session receive `status: "ready"` instantly. By default the CLI blocks until ready; pass `--no-wait` for the polling shape. Two complementary signals on every ready result: the *signed* `zero_gamma` (regime hint that can invert near covered-call-ETF flow or autocall barriers) and a sign-agnostic `gamma_total_abs` + `top_strikes` view that's robust to the dealer-positioning assumption. **Treat the number as a regime hint, not a precise level** — surface methodology caveats when the user is about to act on it.
- `ibkr regime [--explain] [--watch [--rate 5m]] [--json]` — single-call risk-regime dashboard: all five indicators (VIX/VIX3M term structure, HYG vs SPY divergence, USD/JPY weekly move, SPY dealer zero-gamma, SPX breadth) in one JSON envelope. Each row carries raw measurements plus a `notes` field embedding the spec's threshold bands verbatim; green/yellow/red derivation is intentionally on the consumer side. Expect these per-indicator failure modes on the first call against a fresh daemon: Indicator 4 (gamma) returns `status: "computing"` with `eta_seconds` (multi-minute background compute); Indicator 5 (breadth) does the same while the 50-DMA engine bootstraps (~60 min — see breadth bullet). Indicators 1–3 may carry a `fields_missing` array listing advisory sub-fields (e.g. `spy_52w_high`, `hyg_50dma`) that didn't land in the fetch budget — the row's primary measurement still landed, so treat `fields_missing` as a render hint, not an error. Use `--explain` to print the spec's threshold prose alongside each row when the user asks "what does this band mean?". `--watch` re-polls every 5 minutes by default (regime moves on minute-to-hour scales).
- `ibkr size --symbol SYM --entry F --stop F [--target F] [--risk-pct 1.0] [--side long|short] [--lot 1] [--fx 1.0] [--json]` — fixed-fractional sizing. Reads NLV from `account.summary` so `risk_pct` is pegged to the live account. `--fx` converts the base-currency risk budget into the trade's quote currency (e.g. `--fx 1.085` for a USD trade against an EUR account); default `1.0` is correct for same-currency trades. `--lot` rounds shares down (use `100` for one option contract's worth of stock). `--target` is optional: when set, the response also carries `r` (reward-to-risk multiple = `|target − entry| / |entry − stop|`; the standard "is this trade worth taking" filter, ≥ 2R typical), `reward_quote`, `reward_base`, and `breakeven_win_rate` (= `1 / (1 + R)`). Output `status` is `ok` | `tight_risk` (budget < per-share risk × lot — widen the stop or raise risk-pct) | `exceeds_buying_power`. The CLI never derives entry/stop/target from quotes — those are the user's trade plan; if the user asks "and what about the current price?" run `ibkr quote SYM --json` separately.

## Errors

The CLI exits with code 1 on a daemon-side error. The error line on stderr has
a code prefix when applicable:

- `daemon_unavailable` → the daemon could not start (the daemon is the
  long-running half of the same `ibkr` binary, autospawned on first call).
  The IB Gateway is probably not running, or the host/port pinned in config
  is wrong. Suggest `ibkr status` and pointing at `~/.local/state/ibkr/ibkr-daemon.log`.
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

For `breadth`, `gamma`, and `regime`, the JSON carries a per-row `state` /
`status` field rather than an error code — the CLI exits 0 because the
daemon successfully returned a typed envelope. Don't treat these as errors:

- `state: "computing"` (breadth) / `status: "computing"` (gamma, regime
  rows) → a background compute is in flight. Tell the user when to check
  back (gamma: a few minutes; breadth cold start: ~an hour) and don't
  hammer the endpoint. The result will land on a subsequent call.
- `state: "ready"` (breadth) / `status: "ready"` (gamma) /
  `status: "ok"` (regime rows) → the value is real.
- `state: "cold"` / `status: "unavailable"` → the indicator can't run on
  this account or this gateway right now. Surface the row's `notes` field
  verbatim; never substitute a proxy. For regime rows, `error_message`
  carries the specific reason when set.
- `state: "degraded"` (breadth only) → the engine refused to persist
  because constituent coverage fell below the safety threshold. The
  previous good value still serves; surface the degraded state honestly.

## Worked examples

### Position summary
```
$ ibkr positions --json
{
  "data_type": "live",
  "as_of": "2026-05-09T14:32:09Z",
  "stocks": [
    {"symbol": "AAPL", "sec_type": "STOCK", "multiplier": 1,
     "quantity": 100, "avg_cost": 192.40, "mark": 207.88,
     "unrealized_pnl": 1548.0, "realized_pnl": 0}
  ],
  "options": [
    {"symbol": "AAPL", "sec_type": "OPTION", "multiplier": 100,
     "right": "C", "expiry": "20260619", "strike": 215,
     "quantity": 5, "avg_cost": 682.0, "mark": 9.40, "unrealized_pnl": 1290}
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

`avg_cost` is **per-share** for stocks but **per-contract** for options
(the gateway sends it multiplier-inclusive). To compare to `mark` (which
is always per-share) divide by `multiplier`: a $6.82 premium call comes
off the wire as `avg_cost: 682.0` with `multiplier: 100`. The CLI's text
renderer does this division on the AVG COST column; if you're parsing
JSON yourself, do it too. `market_value` and `unrealized_pnl` already
have the multiplier applied — don't double-multiply.

Option rows carry per-leg `delta`/`gamma`/`theta`/`vega` when the gateway
delivered a model-computation tick within budget. The `portfolio` block sums
these into share-equivalent `effective_delta`, `dollar_delta` (in
`dollar_delta_currency` — typically USD for an option book), `daily_theta`
(IBKR reports theta as daily decay), `gamma`, `vega`, and tracks
`greeks_coverage` / `greeks_total` so you can flag partial coverage. When the
user asks "what's my net delta?" or "how much theta am I bleeding per day?",
read the `portfolio` block directly — never sum the legs yourself.

For multi-currency accounts, non-base positions carry `fx_rate`
(base-per-CCY) and `market_value_ccy` (in the contract currency).
`portfolio.fx_sensitivity_per_pct` answers "how much €P&L moves on a 1%
USD/EUR change?" — Σ (non-base NetLiq in CCY × FX × 0.01). It's exposure
× notional, not historical attribution; see SKILL note on `iv_status`
for the same nil-vs-zero discipline.

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
days are skipped. Daily granularity only.

### Option expiries
```
$ ibkr chain AAPL --json
{
  "symbol": "AAPL",
  "as_of": "2026-05-09T14:32:09Z",
  "expiries": [
    {"date": "2026-05-16"},
    {"date": "2026-05-23"},
    {"date": "2026-06-19"}
  ]
}
```

Use this when the user asks "what expiries are available for X?" or "when does the next AAPL option expire?". Render as a short bulleted list. By default each row carries `iv` (decimal, e.g. `0.284` for 28.4%) and `iv_status` (`ok`, `timeout`, `unavailable`) for the nearest 12 expiries; render IV as a percentage and mention any non-`ok` status. Pass `--all-expiries` to fetch IV for every listed date, or `--no-iv` to skip IV entirely. Empty `expiries` means the symbol has no listed options — surface this rather than fabricating expiries.

### Position sizing
```
$ ibkr size --symbol AAPL --entry 207.50 --stop 202.50 --risk-pct 1 --json
{
  "symbol": "AAPL", "side": "long", "entry": 207.50, "stop": 202.50,
  "risk_pct": 1.0, "lot": 1, "fx": 1.0,
  "nlv": 248310.42, "base_currency": "EUR",
  "risk_base": 2483.10, "risk_quote": 2483.10,
  "per_share_risk": 5.0,
  "shares": 496, "notional": 102920.0, "max_loss": 2480.0,
  "status": "ok"
}
```

Render as a short summary: `Risk 1% of NLV (€2,483) on AAPL 207.50 entry / 202.50 stop → 496 shares (notional €102,920, max loss €2,480).` Always quote the `status` field — `tight_risk` means shares=0 (suggest widening the stop or raising `--risk-pct`), `exceeds_buying_power` means notional > BP (suggest trimming `--risk-pct`). When the user's account base differs from the symbol's quote currency, ask them for the FX rate or pass `--fx` explicitly; never invent one.

### What about implied volatility?
The CLI never derives or estimates IV. If `iv_status` is `"unavailable"`, the
gateway didn't deliver tick 106 for that contract — most stock snapshots do
not include IV. Don't substitute historical vol or any proxy.
