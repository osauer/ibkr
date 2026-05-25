package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/cli"
	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/rpc"
	"github.com/osauer/ibkr/internal/watchlist"
)

// Tool is the registered shape of an MCP tool exposed by `ibkr mcp`.
// JSONSchema is sent to the MCP client verbatim; Handler is invoked when
// the client issues tools/call with a matching name. Handlers receive the
// daemon connection and the raw JSON arguments (an empty object when the
// client omits arguments).
type Tool struct {
	Name        string
	Description string
	JSONSchema  json.RawMessage
	Handler     func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error)
}

// Tools is the canonical read-only inventory exposed over MCP. Order is the
// same as cli.Commands() to keep the parity test readable; the MCP client
// rebroadcasts whatever order we send.
var Tools = []Tool{
	{
		Name:        "ibkr_status",
		Description: "Daemon + gateway health snapshot: connection state, account, market-data type (live/frozen/delayed), server version, members-list source, last-error. Run this first when troubleshooting connectivity (\"why is data missing / stale / wrong-account?\"). NOT for portfolio state — use `ibkr_account` for cash/margin or `ibkr_positions` for what you own.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.HealthResult
			if err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_account",
		Description: "Account-level financials: net liquidation, buying power, cash, margin — all in base currency. Use when the question is about *the account as a whole* (\"how much cash?\", \"how much margin am I using?\", \"what's today's P&L?\"). Includes daily_pnl (start-of-trading-day to now), with daily_pnl_unrealized and daily_pnl_realized breakdown when the gateway provides them — these are distinct from session-running unrealized/realized totals. For multi-currency accounts, also returns currency_exposure: one row per non-base currency holding with net liquidation in that currency, gateway-reported exchange rate, and the base-currency conversion. Useful for attributing P&L between underlying moves and FX moves. NOT for per-position detail — use `ibkr_positions` to see what you actually own.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.AccountResult
			if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_positions",
		Description: "Open positions: stocks and options separated, plus a per-underlying grouping with summed P&L. Use when the question is about *what you own* (\"show me my positions\", \"what's my exposure to AAPL?\", \"how much delta do I have?\"). Each row carries unrealized_pnl (session-running) and daily_pnl (start-of-trading-day to now, from IBKR's reqPnLSingle stream). daily_pnl is null when the daemon hasn't yet pre-warmed that contract's subscription or the account isn't entitled; never zero-substituted. Option legs include per-leg Greeks (delta/gamma/theta/vega) when IBKR delivers the model-computation tick within budget. The `portfolio` block aggregates effective_delta (share-equivalents), dollar_delta, daily_theta, gamma, vega, plus fx_sensitivity_per_pct for accounts with non-base-currency holdings. Non-base positions also carry fx_rate and market_value_ccy. NOT for cash/margin totals (use `ibkr_account`) and NOT for live quotes on symbols you don't hold (use `ibkr_quote`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol": schemaString("filter to a single underlying symbol (case-insensitive)"),
			"type":   schemaEnum([]string{"stk", "opt"}, "filter to stock or option positions"),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.PositionsListParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.PositionsResult
			if err := conn.Call(ctx, rpc.MethodPositionsList, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_quote",
		Description: "Snapshot quotes for one or more equity / ETF symbols. Returns bid/ask/last, mark, sizes, volume, opportunistic IV when the gateway delivers tick 106 (stock/ETF IV is often null/unavailable), and `session_context` when the official market calendar explains stale/frozen/missing data. Use for *current price* questions on stocks/ETFs (\"what's SPY trading at?\"); off-hours/frozen snapshots may have `mark` or `prev_close` when bid/ask/last are absent. US symbols default to SMART/USD. For German/Xetra equities whose ticker collides with the US default route (for example MBG), set `market: \"de\"` or explicit `exchange`/`currency`. NOT for options (use `ibkr_chain` with an `expiry` argument), NOT for historical bars (use `ibkr_history`), NOT for the position you already hold (`ibkr_positions` already includes live marks).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbols":          json.RawMessage(`{"type":"array","items":{"type":"string"},"minItems":1,"description":"ticker symbols, e.g. [\"AAPL\",\"MSFT\"] or [\"MBG\"] with market=\"de\""}`),
			"market":           json.RawMessage(`{"type":"string","enum":["us","de"],"description":"optional stock routing shortcut; omit or use \"us\" for SMART/USD, use \"de\" for German/Xetra EUR equities via SMART with primary_exchange=IBIS"}`),
			"exchange":         schemaString("optional IBKR exchange/venue override for stocks, e.g. SMART or IBIS; omit unless the default market route fails"),
			"primary_exchange": schemaString("optional IBKR primary-exchange hint when routing a stock through SMART, e.g. NASDAQ or IBIS"),
			"currency":         schemaString("optional ISO currency override for stocks, e.g. USD or EUR"),
		}, []string{"symbols"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbols         []string `json:"symbols"`
				Market          string   `json:"market"`
				Exchange        string   `json:"exchange"`
				PrimaryExchange string   `json:"primary_exchange"`
				Currency        string   `json:"currency"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if len(in.Symbols) == 0 {
				return nil, fmt.Errorf("symbols is required and must be non-empty")
			}
			quotes := make([]rpc.Quote, 0, len(in.Symbols))
			for _, sym := range in.Symbols {
				params := rpc.QuoteSnapshotParams{Contract: rpc.ContractParams{
					Symbol:      strings.ToUpper(strings.TrimSpace(sym)),
					SecType:     "STK",
					Market:      strings.TrimSpace(in.Market),
					Exchange:    strings.ToUpper(strings.TrimSpace(in.Exchange)),
					PrimaryExch: strings.ToUpper(strings.TrimSpace(in.PrimaryExchange)),
					Currency:    strings.ToUpper(strings.TrimSpace(in.Currency)),
				}}
				var q rpc.Quote
				if err := conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
					return nil, fmt.Errorf("quote %s: %w", sym, err)
				}
				quotes = append(quotes, q)
			}
			if len(quotes) == 1 {
				return json.Marshal(quotes[0])
			}
			return json.Marshal(quotes)
		},
	},
	{
		Name:        "ibkr_watch",
		Description: "Read the user's local ibkr watchlist: symbols they explicitly saved with the CLI via `ibkr watch SYMBOL --add`. Use when the user asks \"what's on my watchlist?\"; set `include_quotes: true` when they want a decision-making monitor with current price/currency, change, previous close, day range, 52-week range, volume, average volume, data freshness, session context, and optional held-stock context. This MCP tool is read-only: it does NOT add, remove, clear, create IBKR/TWS watchlists, or place trades. For ad-hoc symbols that are not saved in the watchlist, use `ibkr_quote` instead.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"include_quotes":    json.RawMessage(`{"type":"boolean","description":"when true, return enriched quote rows for saved symbols instead of only the local symbol list; default false preserves offline/list-only behavior"}`),
			"include_positions": json.RawMessage(`{"type":"boolean","description":"when include_quotes is true, attach compact held-stock context where available; default true"}`),
			"timeout_ms":        json.RawMessage(`{"type":"integer","minimum":100,"description":"per-symbol quote timeout when include_quotes is true; default 5000 ms"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				IncludeQuotes    bool  `json:"include_quotes"`
				IncludePositions *bool `json:"include_positions"`
				TimeoutMs        int   `json:"timeout_ms"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			path, err := watchlist.DefaultPath()
			if err != nil {
				return nil, err
			}
			snap, err := watchlist.New(path).Snapshot()
			if err != nil {
				return nil, err
			}
			if !in.IncludeQuotes {
				return json.Marshal(snap)
			}
			if conn == nil {
				return nil, fmt.Errorf("include_quotes requires a daemon connection")
			}
			includePositions := true
			if in.IncludePositions != nil {
				includePositions = *in.IncludePositions
			}
			res, err := buildWatchlistQuoteResult(ctx, conn, snap, in.TimeoutMs, includePositions)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_calendar",
		Description: "Official market-session calendar for supported first-release markets: U.S. cash equities (`market: \"us\"` / `\"us-equity\"`), U.S. listed options regular sessions (`\"us-options\"`), and German Xetra cash equities (`\"de\"` / `\"de-xetra\"`). Use for questions like \"is the market open?\", \"when is the next session?\", \"is today a holiday or early close?\", \"why is this quote frozen at 1am ET?\", or risk-manager context before a long market holiday weekend. NOT for prices (use `ibkr_quote`), NOT for broad futures/FX/bonds/Eurex/crypto calendars, and NOT for per-contract SPX/VIX global-hours nuance — v1 is official exchange calendars only and returns `unknown` outside embedded coverage rather than guessing from weekdays.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"market": json.RawMessage(`{"type":"string","enum":["us","us-equity","us-options","de","de-xetra"],"description":"which official calendar to query: us/us-equity for U.S. stocks and ETFs, us-options for U.S. listed options regular sessions, de/de-xetra for Xetra cash equities"}`),
			"date":   schemaString("optional local market date YYYY-MM-DD; omit to use now"),
			"days":   json.RawMessage(`{"type":"integer","minimum":1,"maximum":400,"description":"number of calendar days to include in sessions (default 14, capped at 400)"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.MarketCalendarParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.MarketCalendarResult
			if err := conn.Call(ctx, rpc.MethodMarketCalendar, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_chain",
		Description: "Option chain — use whenever the user asks anything about options (\"AAPL puts\", \"this Friday's chain\", \"call wall on SPY\"). Two shapes: **omit `expiry`** to get the expiry list (each row carries ATM IV, DTE, and the 1-σ implied move `spot × IV × √(DTE/365)` — the desk-standard expected dollar move by expiration, used for earnings sizing and strike selection; daemon caches IV results, second call within ~60 s during RTH is instant); **provide `expiry`** (YYYY-MM-DD) for the ATM±`width` strike grid. Per-leg fields on the strike grid: bid/ask/last for calls and puts, IV when delivered, per-leg call/put delta when delivered, and **`call_oi` / `put_oi`** (option open interest, int64) sourced from IBKR ticks 27/28 on the existing per-leg subscription — `null` when the gateway didn't push the tick within the chain fill budget (common off-hours and for illiquid wings), never zero-substituted. `no_iv` returns the fast skeleton for the expiry list (DTE only). `all_expiries` lifts the default 12-expiry cap (nearest 12 normally — back-half LEAPS rarely on the decision path). NOT for stock-level quotes (use `ibkr_quote`), NOT for historical bars (use `ibkr_history`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol":       schemaString("underlying ticker"),
			"expiry":       schemaString("expiry date YYYY-MM-DD; omit to list available expiries"),
			"width":        json.RawMessage(`{"type":"integer","minimum":0,"description":"strikes ATM ± this count (default 5; 0 returns ATM only)"}`),
			"side":         schemaEnum([]string{"calls", "puts", "both"}, "filter strike legs (default both)"),
			"no_iv":        json.RawMessage(`{"type":"boolean","description":"when listing expiries, skip ATM IV (faster)"}`),
			"all_expiries": json.RawMessage(`{"type":"boolean","description":"when listing expiries, return every listed date (default: nearest 12 with IV)"}`),
		}, []string{"symbol"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbol      string `json:"symbol"`
				Expiry      string `json:"expiry"`
				Width       *int   `json:"width"`
				Side        string `json:"side"`
				NoIV        bool   `json:"no_iv"`
				AllExpiries bool   `json:"all_expiries"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Symbol == "" {
				return nil, fmt.Errorf("symbol is required")
			}
			if in.Expiry == "" {
				var res rpc.ChainExpiriesResult
				params := rpc.ChainExpiriesParams{
					Symbol:      strings.ToUpper(in.Symbol),
					WithIV:      !in.NoIV,
					AllExpiries: in.AllExpiries,
				}
				if err := conn.Call(ctx, rpc.MethodChainExpiries, params, &res); err != nil {
					return nil, err
				}
				return json.Marshal(res)
			}
			width := 5
			if in.Width != nil {
				width = *in.Width
			}
			if in.Side == "" {
				in.Side = "both"
			}
			var res rpc.ChainResult
			params := rpc.ChainFetchParams{Symbol: strings.ToUpper(in.Symbol), Expiry: in.Expiry, Width: width, Side: in.Side}
			if err := conn.Call(ctx, rpc.MethodChainFetch, params, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_history",
		Description: "Daily OHLCV bars for an equity / ETF symbol. Use for trend / moving-average / lookback questions (\"is AAPL above its 50-DMA?\", \"what's the 90-day range?\"). Non-trading days are skipped, so the row count is typically smaller than `days`. NOT for intraday bars (not exposed today), NOT for options (use `ibkr_chain`), NOT for the live current price (use `ibkr_quote`).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol": schemaString("equity / ETF ticker, case-insensitive (e.g. \"AAPL\", \"spy\")"),
			"days":   json.RawMessage(`{"type":"integer","minimum":1,"description":"calendar-day lookback (default 90); the returned row count is smaller because non-trading days are skipped"}`),
		}, []string{"symbol"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.HistoryDailyParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Symbol == "" {
				return nil, fmt.Errorf("symbol is required")
			}
			in.Symbol = strings.ToUpper(in.Symbol)
			var res rpc.HistoryDailyResult
			if err := conn.Call(ctx, rpc.MethodHistoryDaily, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_scan",
		Description: "Run a market scanner. Three call shapes: (1) preset by name — `{preset: \"top-movers\"}` — for the configured shortcuts; (2) ad-hoc — `{type: \"TOP_PERC_GAIN\", exchange: \"STK.US.MAJOR\"}` for US stocks or `{type: \"TOP_PERC_GAIN\", exchange: \"STK.EU.IBIS\", instrument: \"STOCK.EU\"}` for German/Xetra stocks — to compose a scan without writing to the user's config; (3) empty `{}` — enumerates the configured presets so the agent can pick one. For ad-hoc, call `ibkr_scan_params` first to discover the scanCode (`type`), locationCode (`exchange`), and instrument values this gateway accepts. Each row is enriched with last/prev_close/change/change_pct/volume/iv/week_52_high/week_52_low via per-row market-data subscriptions the daemon issues automatically (IBKR's scanner protocol returns only rank+symbol). Nil fields mean the gateway didn't deliver the corresponding tick within the enrichment window — common off-hours, and IV is nil for symbols without actively-traded options. Ad-hoc rows are capped at 50.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"preset":     schemaString("preset name from `ibkr_scan` with no args (e.g. \"top-movers\"); omit for ad-hoc or list mode"),
			"type":       schemaString("ad-hoc scanCode (e.g. \"TOP_PERC_GAIN\") — required with `exchange` when no `preset` is given"),
			"exchange":   schemaString("ad-hoc locationCode (e.g. \"STK.US.MAJOR\" or \"STK.EU.IBIS\") — required with `type` when no `preset` is given"),
			"instrument": schemaString("IBKR scanner instrument for ad-hoc scans; defaults to STK for US stocks, use STOCK.EU for European stock locations such as STK.EU.IBIS"),
			"limit":      json.RawMessage(`{"type":"integer","minimum":1,"description":"max rows; preset default when omitted; ad-hoc capped at 50"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.ScanRunParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Preset == "" && in.Type == "" && in.Exchange == "" {
				var res rpc.ScanListResult
				if err := conn.Call(ctx, rpc.MethodScanList, nil, &res); err != nil {
					return nil, err
				}
				return json.Marshal(res)
			}
			var res rpc.ScanResult
			if err := conn.Call(ctx, rpc.MethodScanRun, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_scan_params",
		Description: "Discover the scanner catalog this IBKR gateway supports: every scanCode (the `type` for ad-hoc `ibkr_scan`) and every locationCode (`exchange`), plus the instrument types each scanCode applies to. Use this before composing an ad-hoc scan — the catalog varies by gateway version, market-data permissions, and region. Pass `instrument: \"STK\"` to narrow scan_types to US stocks or `instrument: \"STOCK.EU\"` for European stocks; pass `include_raw_xml: true` only when you need a field not surfaced in the parsed result (the XML payload is ~200 KB).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"instrument":      schemaString("filter scan_types to those valid for this instrument (e.g. \"STK\", \"STOCK.EU\", \"OPT\", \"ETF\"); empty returns all"),
			"include_raw_xml": json.RawMessage(`{"type":"boolean","description":"include the gateway's raw XML payload (~200 KB); default false"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.ScanParamsParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.ScanParamsResult
			if err := conn.Call(ctx, rpc.MethodScanParams, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_breadth",
		Description: "S&P 500 market-breadth readings for the risk-regime dashboard. Use for questions about the *market's internals* — \"how many S&P names are above their 50-DMA?\", \"is this a narrow rally?\", \"what's the new-high/new-low spread?\". S&P 500 constituents only — NDX, RUT, sector-specific, or single-stock breadth are NOT supported. Returns three readings every call: `pct_above_50dma` (the percentage of S&P 500 constituents trading above their 50-day SMA — the tactical signal), `pct_above_200dma` (the slower companion that catches cyclical tops cleanly), and `new_highs_today`/`new_lows_today` (constituent counts of names making fresh 52-week highs/lows), plus the derived `net_new_highs_pct`. The classic narrow-rally pattern — SPX near highs with `net_new_highs_pct` near zero or negative — fires when a few mega-caps carry the index while the median name is rolling over. IBKR does not redistribute S&P DJI's S5FI or the equivalent breadth indices on retail subscriptions, so the daemon computes all three locally from the 500 constituent daily closes pulled via IBKR's historical-bar feed (method: `constituent-fanout-50/200dma-hl`). A once-daily refresh post-close (16:35 ET) slides each name's 200-bar window forward and updates a 252-bar rolling max/min; readers see a cached snapshot. **Cold start (no cache yet) takes ~60 min** — IBKR's historical-data pacing limit caps the fan-out at ~6 names/min sustained, so the response carries `state: \"computing\"` until the cache is built. Pulling 200 bars per constituent instead of 50 doesn't cost more requests; the pacing limit is per-request, not per-bar, so the cold-start budget is unchanged. After cold-start the cache persists across daemon restarts and every subsequent call is instant. Threshold derivation (green/yellow/red) is intentionally left to the consumer; the spec calls those bands user-tunable. Suggested bands: 50-DMA — `>55` green / `40-55` yellow / `<40` with SPX at highs red. 200-DMA — `>60` green / `40-60` yellow / `<40` red (calibrated to the post-Mag-7 era; StockCharts' 70/30 default fires red far too often).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"history_days": json.RawMessage(`{"type":"integer","minimum":1,"maximum":90,"description":"trailing daily-series length (default 30)"}`),
			"timeout_ms":   json.RawMessage(`{"type":"integer","minimum":100,"description":"per-snapshot wait budget when the engine has a fresh value but the wire envelope is still being assembled (default 5000 ms); does not affect the multi-minute cold-start fan-out"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.BreadthSPXParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			var res rpc.BreadthSPXResult
			if err := conn.Call(ctx, rpc.MethodBreadthSPX, in, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_gamma",
		Description: "Dealer-gamma market-structure snapshot for SPY, SPX, or the default SPY+SPX view. Use for questions like \"where is dealer gamma?\", \"did the signed profile find a zero-gamma crossing?\", \"is the modeled book long-gamma or short-gamma?\", or \"where are the largest gamma concentrations?\" NOT for portfolio Greeks (use `ibkr_positions`) and NOT for options chains/quotes (use `ibkr_chain` / `ibkr_quote`). The ready result leads with `summary`: `primary_statement`, `zero_gamma_status` (`crossing`, `none_in_window`, `mixed`, `mixed_degraded`, `unavailable`), `regime`, `confidence`, `not_advice`, and per-index summaries. In combined scope there is no top-level combined zero-gamma price because SPY and SPX use different price scales; read `summary.per_index.SPY` and `summary.per_index.SPX` for per-underlying spot, zero, swept range, regime, and GEX leg counts. `gamma_total_abs` and `top_strikes` are sign-agnostic concentration/magnitude diagnostics. `leg_count` means legs with non-zero OI-weighted GEX; `priced_leg_count` means legs that priced/fit IV but may not have usable OI. Non-fatal data-quality issues are in `warning_details` with `{code, scope, severity, message, impact, action}`; raw warning tokens are not part of the JSON contract. By default profile arrays are stripped to keep MCP responses compact; set `include_profiles: true` only when charting the sweep. First call of a NY session may return `status: \"computing\"` with progress/ETA; set `wait_ms` to wait. The signed zero-gamma convention is a regime hint, not advice or a trade level.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"wait_ms":          json.RawMessage(`{"type":"integer","minimum":0,"description":"block up to this many ms for the result; 0 (default) returns the current status immediately"}`),
			"force":            json.RawMessage(`{"type":"boolean","description":"diagnostics-only: ignore the cached result and start a fresh compute; default false"}`),
			"scope":            json.RawMessage(`{"type":"string","enum":["spy","spx","spy+spx"],"description":"which underlying(s) to compute: 'spy+spx' (combined, the default — falls back to SPY-only on SPX entitlement/data gap); 'spy' (SPY-only); 'spx' (SPX-only, errors out if SPX unreachable). Omit for the default combined view. Mirrors the CLI's --only flag."}`),
			"include_profiles": json.RawMessage(`{"type":"boolean","description":"include full sweep profile arrays for charting; default false keeps the response compact for agents and tooling"}`),
		}, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in rpc.GammaZeroSPXParams
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			// Normalise/validate scope at the MCP edge so a bad value
			// surfaces as a tool error rather than the daemon's wire
			// error envelope — clients distinguish the two.
			switch strings.ToLower(strings.TrimSpace(in.Scope)) {
			case "", rpc.GammaZeroScopeSPY, rpc.GammaZeroScopeSPX, rpc.GammaZeroScopeCombined:
				in.Scope = strings.ToLower(strings.TrimSpace(in.Scope))
			default:
				return nil, fmt.Errorf("scope must be one of 'spy', 'spx', 'spy+spx' (got %q)", in.Scope)
			}
			var res rpc.GammaZeroSPXResult
			if err := conn.Call(ctx, rpc.MethodGammaZeroSPX, in, &res); err != nil {
				return nil, err
			}
			if !in.IncludeProfiles {
				rpc.StripGammaProfiles(&res)
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_regime",
		Description: "Risk-regime snapshot — single-call, non-advisory answer for \"how does the market regime look today?\", \"is this a risk-on or risk-off tape?\", \"are we close to stress thresholds?\", or \"give me the daily-check dashboard.\" Use this when the user wants the market's current evidence balance across equity vol (VIX/VIX3M + VVIX), credit (HYG/SPY + official HY/IG OAS), funding stress (CP/T-bill spread), FX carry proxy, dealer gamma, and S&P 500 breadth. NOT for a trade recommendation or probability forecast. The compact MCP response leads with `summary` (`label`, cluster-level `evidence`, `indicator_evidence`, `punch_line`, `confidence`, `dominant_risks`, `not_advice`) and `composite` raw + cluster counts, then the eight indicator rows. Per-row fields include raw measurements, `status`, `band`, `band_reason`, `thresholds` (heuristic + pending_backtest), `as_of` (`label`, freshness/source/time/date), `streak`, and per-scalar `*_quality` provenance. `warning_details` gives scoped prose for unavailable/stale/computing rows with `{code, scope, severity, message, impact, action}`; do not parse opaque error strings when this field is present. MOVE/rates-vol is intentionally absent until a verified IBKR contract/source exists; do not infer it from ETFs or futures. Methodology prose is omitted from MCP for compactness; use `spec_doc` or CLI `ibkr regime --explain` for full threshold notes. Gamma embeds the compact `ibkr_gamma` envelope with profiles stripped: in combined scope use `envelope.result.summary`, `per_index.SPY`, `per_index.SPX`, `gamma_total_abs`, and `top_strikes`; the signed γ-zero is a regime hint, not a precise level. Expect gamma/breadth to be `computing` on cold starts and optional `fields_missing` values when a secondary scalar missed the fetch budget or an official daily file is temporarily unavailable.",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.RegimeSnapshotResult
			if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &res); err != nil {
				return nil, err
			}
			rpc.CompactRegimeSnapshot(&res)
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_size",
		Description: "Fixed-fractional position sizing pegged to live NLV. Pure math against the account snapshot — never proposes or executes an order. Pass an optional target to also get the R-multiple (reward:risk) and breakeven win rate.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol":   schemaString("ticker the trade plan applies to (for reporting only)"),
			"entry":    json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"planned entry price per share, quote currency"}`),
			"stop":     json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"planned stop price per share, quote currency"}`),
			"target":   json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"optional take-profit price; when set, response includes r (reward:risk multiple) and breakeven_win_rate"}`),
			"risk_pct": json.RawMessage(`{"type":"number","exclusiveMinimum":0,"maximum":100,"description":"percent of NLV to risk (default 1.0)"}`),
			"side":     schemaEnum([]string{"long", "short"}, "trade direction (default long)"),
			"lot":      json.RawMessage(`{"type":"integer","minimum":1,"description":"round shares down to this multiple (default 1; use 100 for one option contract's worth of stock)"}`),
			"fx":       json.RawMessage(`{"type":"number","exclusiveMinimum":0,"description":"quote-currency units per 1 base-currency unit (default 1.0 for same-currency trades)"}`),
		}, []string{"symbol", "entry", "stop"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbol  string  `json:"symbol"`
				Side    string  `json:"side"`
				Entry   float64 `json:"entry"`
				Stop    float64 `json:"stop"`
				Target  float64 `json:"target"`
				RiskPct float64 `json:"risk_pct"`
				Lot     int     `json:"lot"`
				FX      float64 `json:"fx"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Side == "" {
				in.Side = "long"
			}
			if in.RiskPct == 0 {
				in.RiskPct = 1.0
			}
			if in.Lot == 0 {
				in.Lot = 1
			}
			if in.FX == 0 {
				in.FX = 1.0
			}
			var acct rpc.AccountResult
			if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
				return nil, err
			}
			res, err := cli.ComputeSize(cli.SizeInput{
				Symbol:      in.Symbol,
				Side:        in.Side,
				Entry:       in.Entry,
				Stop:        in.Stop,
				Target:      in.Target,
				RiskPct:     in.RiskPct,
				Lot:         in.Lot,
				FX:          in.FX,
				NLV:         acct.NetLiquidation,
				BuyingPower: acct.BuyingPower,
				Currency:    acct.BaseCurrency,
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
}

func buildWatchlistQuoteResult(ctx context.Context, conn *dial.Conn, snap *watchlist.Snapshot, timeoutMs int, includePositions bool) (*rpc.WatchlistResult, error) {
	if timeoutMs <= 0 {
		timeoutMs = int((5 * time.Second).Milliseconds())
	}
	res := &rpc.WatchlistResult{
		Name:    snap.Name,
		Symbols: append([]string(nil), snap.Symbols...),
		Rows:    make([]rpc.WatchlistRow, 0, len(snap.Symbols)),
		AsOf:    time.Now(),
	}
	holdings := map[string]*rpc.WatchlistHolding{}
	if includePositions {
		var pos rpc.PositionsResult
		if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{Type: "stk"}, &pos); err == nil {
			for _, p := range pos.Stocks {
				holdings[strings.ToUpper(p.Symbol)] = &rpc.WatchlistHolding{
					Quantity:      p.Quantity,
					AvgCost:       p.AvgCost,
					Mark:          p.Mark,
					MarketValue:   p.MarketValue,
					UnrealizedPnL: p.UnrealizedPnL,
					DailyPnL:      p.DailyPnL,
					Exchange:      p.Exchange,
					Currency:      p.Currency,
				}
			}
		}
	}
	for _, sym := range snap.Symbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var q rpc.Quote
		params := rpc.QuoteSnapshotParams{
			Contract:  watchlistQuoteContract(sym, holdings[strings.ToUpper(sym)]),
			TimeoutMs: timeoutMs,
		}
		row := rpc.WatchlistRow{}
		if err := conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
			row.Quote = rpc.Quote{Symbol: sym}
			row.Error = err.Error()
		} else {
			row.Quote = q
		}
		if h, ok := holdings[strings.ToUpper(sym)]; ok {
			row.Holding = h
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

func watchlistQuoteContract(sym string, h *rpc.WatchlistHolding) rpc.ContractParams {
	c := rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: "USD"}
	if h == nil {
		return c
	}
	if h.Currency != "" {
		c.Currency = h.Currency
	}
	if h.Exchange != "" {
		c.Exchange = h.Exchange
	}
	return c
}

// ExcludedCLI is the set of cli.Commands() names that intentionally have no
// MCP tool counterpart. The parity test consults this so adding a new CLI
// command without an MCP tool fails the gate unless the exclusion is recorded.
//
// `quote` is intentionally absent from this map — it has both a snapshot
// MCP tool (ibkr_quote) and, for the `--watch` mode, the MCP resource
// template ibkr://quote/{symbol} gated by TestStreamingParity in
// resources_test.go.
var ExcludedCLI = map[string]string{
	"version": "info-only CLI verb; not useful as a tool call",
	"mcp":     "transport server mode; the MCP host starts this process, no LLM should call it as a tool",
	"daemon":  "local background service mode; autospawned by CLI/MCP clients and not an agent operation",
	"setup":   "local configuration verb (writes claude_desktop_config.json); not a daemon RPC, no LLM should ever call it",
	"update":  "binary-management verb (replaces the ibkr binary from GitHub releases); not a daemon RPC, must stay user-triggered for trust-boundary reasons",
}

func schemaObject(props map[string]json.RawMessage, required []string) json.RawMessage {
	// Minimal hand-built schema — avoids pulling a JSON Schema library and
	// keeps the wire payload exactly what MCP clients expect (a JSON object
	// with type:"object" and a properties map).
	buf := &strings.Builder{}
	buf.WriteString(`{"type":"object","properties":{`)
	first := true
	// Sorted iteration so the JSONSchema bytes are stable across builds —
	// MCP clients hash these for caching; non-deterministic property order
	// would invalidate caches unnecessarily.
	keys := sortedKeys(props)
	for _, k := range keys {
		if !first {
			buf.WriteString(",")
		}
		fmt.Fprintf(buf, "%q:%s", k, string(props[k]))
		first = false
	}
	buf.WriteString(`}`)
	if len(required) > 0 {
		b, _ := json.Marshal(required)
		fmt.Fprintf(buf, `,"required":%s`, string(b))
	}
	buf.WriteString(`}`)
	return json.RawMessage(buf.String())
}

func schemaString(description string) json.RawMessage {
	b, _ := json.Marshal(struct {
		Type        string `json:"type"`
		Description string `json:"description,omitempty"`
	}{Type: "string", Description: description})
	return json.RawMessage(b)
}

func schemaEnum(values []string, description string) json.RawMessage {
	b, _ := json.Marshal(struct {
		Type        string   `json:"type"`
		Enum        []string `json:"enum"`
		Description string   `json:"description,omitempty"`
	}{Type: "string", Enum: values, Description: description})
	return json.RawMessage(b)
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func unmarshalArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
