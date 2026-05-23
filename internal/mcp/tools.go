package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/osauer/ibkr/internal/cli"
	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/rpc"
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
		Description: "Daemon + gateway health snapshot: connection state, account, market-data type, server version. Run this first when troubleshooting.",
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
		Description: "Account summary: net liquidation, buying power, cash, margin — all in base currency. Includes daily_pnl (start-of-trading-day to now), with daily_pnl_unrealized and daily_pnl_realized breakdown when the gateway provides them — these are distinct from session-running unrealized/realized totals. For multi-currency accounts, also returns currency_exposure: one row per non-base currency holding with net liquidation in that currency, gateway-reported exchange rate, and the base-currency conversion. Useful for attributing P&L between underlying moves and FX moves.",
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
		Description: "Open positions: stocks and options separated, plus a per-underlying grouping with summed P&L. Each row carries unrealized_pnl (session-running) and daily_pnl (start-of-trading-day to now, from IBKR's reqPnLSingle stream). daily_pnl is null when the daemon hasn't yet pre-warmed that contract's subscription or the account isn't entitled; never zero-substituted. Option legs include per-leg Greeks (delta/gamma/theta/vega) when IBKR delivers the model-computation tick within budget. The `portfolio` block aggregates effective_delta (share-equivalents), dollar_delta, daily_theta, gamma, vega, plus fx_sensitivity_per_pct for accounts with non-base-currency holdings. Non-base positions also carry fx_rate and market_value_ccy.",
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
		Description: "Snapshot quotes for one or more equity / ETF symbols. Returns bid/ask/last, sizes, volume, and IV per symbol.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbols": json.RawMessage(`{"type":"array","items":{"type":"string"},"minItems":1,"description":"ticker symbols, e.g. [\"AAPL\",\"MSFT\"]"}`),
		}, []string{"symbols"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbols []string `json:"symbols"`
			}
			if err := unmarshalArgs(args, &in); err != nil {
				return nil, err
			}
			if len(in.Symbols) == 0 {
				return nil, fmt.Errorf("symbols is required and must be non-empty")
			}
			quotes := make([]rpc.Quote, 0, len(in.Symbols))
			for _, sym := range in.Symbols {
				params := rpc.QuoteSnapshotParams{Contract: rpc.ContractParams{Symbol: strings.ToUpper(strings.TrimSpace(sym)), SecType: "STK"}}
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
		Name:        "ibkr_chain",
		Description: "Option chain. Omit `expiry` to get the expiry list (with ATM IV, DTE, and 1-σ implied move per expiry by default — daemon caches IV results). The implied move is `spot × IV × √(DTE/365)`, the canonical expected dollar move by expiration. Provide `expiry` (YYYY-MM-DD) for the ATM±width strike grid. `no_iv` returns the fast skeleton (DTE only); `all_expiries` lifts the default 12-expiry cap.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol":       schemaString("underlying ticker"),
			"expiry":       schemaString("expiry date YYYY-MM-DD; omit to list available expiries"),
			"width":        json.RawMessage(`{"type":"integer","minimum":1,"description":"strikes ATM ± this count (default 5)"}`),
			"side":         schemaEnum([]string{"calls", "puts", "both"}, "filter strike legs (default both)"),
			"no_iv":        json.RawMessage(`{"type":"boolean","description":"when listing expiries, skip ATM IV (faster)"}`),
			"all_expiries": json.RawMessage(`{"type":"boolean","description":"when listing expiries, return every listed date (default: nearest 12 with IV)"}`),
		}, []string{"symbol"}),
		Handler: func(ctx context.Context, conn *dial.Conn, args json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Symbol      string `json:"symbol"`
				Expiry      string `json:"expiry"`
				Width       int    `json:"width"`
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
			if in.Width == 0 {
				in.Width = 5
			}
			if in.Side == "" {
				in.Side = "both"
			}
			var res rpc.ChainResult
			params := rpc.ChainFetchParams{Symbol: strings.ToUpper(in.Symbol), Expiry: in.Expiry, Width: in.Width, Side: in.Side}
			if err := conn.Call(ctx, rpc.MethodChainFetch, params, &res); err != nil {
				return nil, err
			}
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_history",
		Description: "Daily OHLCV bars for a symbol. Non-trading days are skipped, so the row count is typically smaller than `days`.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"symbol": schemaString("ticker symbol"),
			"days":   json.RawMessage(`{"type":"integer","minimum":1,"description":"calendar lookback (default 90)"}`),
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
		Description: "Run a market scanner. Three call shapes: (1) preset by name — `{preset: \"top-movers\"}` — for the configured shortcuts; (2) ad-hoc — `{type: \"TOP_PERC_GAIN\", exchange: \"STK.US.MAJOR\"}` — to compose a scan without writing to the user's config; (3) empty `{}` — enumerates the configured presets so the agent can pick one. For ad-hoc, call `ibkr_scan_params` first to discover the scanCode (`type`) and locationCode (`exchange`) values this gateway accepts. Each row is enriched with last/prev_close/change/change_pct/volume/iv/week_52_high/week_52_low via per-row market-data subscriptions the daemon issues automatically (IBKR's scanner protocol returns only rank+symbol). Nil fields mean the gateway didn't deliver the corresponding tick within the enrichment window — common off-hours, and IV is nil for symbols without actively-traded options. Ad-hoc rows are capped at 50.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"preset":   schemaString("preset name from `ibkr_scan` with no args (e.g. \"top-movers\"); omit for ad-hoc or list mode"),
			"type":     schemaString("ad-hoc scanCode (e.g. \"TOP_PERC_GAIN\") — required with `exchange` when no `preset` is given"),
			"exchange": schemaString("ad-hoc locationCode (e.g. \"STK.US.MAJOR\") — required with `type` when no `preset` is given"),
			"limit":    json.RawMessage(`{"type":"integer","minimum":1,"description":"max rows; preset default when omitted; ad-hoc capped at 50"}`),
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
		Description: "Discover the scanner catalog this IBKR gateway supports: every scanCode (the `type` for ad-hoc `ibkr_scan`) and every locationCode (`exchange`), plus the instrument types each scanCode applies to. Use this before composing an ad-hoc scan — the catalog varies by gateway version and by the user's market-data permissions. Pass `instrument: \"STK\"` to narrow scan_types to stocks; pass `include_raw_xml: true` only when you need a field not surfaced in the parsed result (the XML payload is ~200 KB).",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"instrument":      schemaString("filter scan_types to those valid for this instrument (e.g. \"STK\", \"OPT\", \"ETF\"); empty returns all"),
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
		Description: "S&P 500 market-breadth readings for the risk-regime dashboard. Returns three readings every call: `pct_above_50dma` (the percentage of S&P 500 constituents trading above their 50-day SMA — the tactical signal), `pct_above_200dma` (the slower companion that catches cyclical tops cleanly), and `new_highs_today`/`new_lows_today` (constituent counts of names making fresh 52-week highs/lows), plus the derived `net_new_highs_pct`. The classic narrow-rally pattern — SPX near highs with `net_new_highs_pct` near zero or negative — fires when a few mega-caps carry the index while the median name is rolling over. IBKR does not redistribute S&P DJI's S5FI or the equivalent breadth indices on retail subscriptions, so the daemon computes all three locally from the 500 constituent daily closes pulled via IBKR's historical-bar feed (method: `constituent-fanout-50/200dma-hl`). A once-daily refresh post-close (16:35 ET) slides each name's 200-bar window forward and updates a 252-bar rolling max/min; readers see a cached snapshot. **Cold start (no cache yet) takes ~60 min** — IBKR's historical-data pacing limit caps the fan-out at ~6 names/min sustained, so the response carries `state: \"computing\"` until the cache is built. Pulling 200 bars per constituent instead of 50 doesn't cost more requests; the pacing limit is per-request, not per-bar, so the cold-start budget is unchanged. After cold-start the cache persists across daemon restarts and every subsequent call is instant. Threshold derivation (green/yellow/red) is intentionally left to the consumer; the spec calls those bands user-tunable. Suggested bands: 50-DMA — `>55` green / `40-55` yellow / `<40` with SPX at highs red. 200-DMA — `>60` green / `40-60` yellow / `<40` red (calibrated to the post-Mag-7 era; StockCharts' 70/30 default fires red far too often).",
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
		Description: "Combined SPY+SPX dealer zero-gamma estimate (Indicator 4 of the risk-regime dashboard). Computed from IBKR's option chains using the Perfiliev convention (dealers long calls, short puts) over the 6 nearest non-0DTE-post-settlement expirations, ±10 % strike width. v2 methodology (`perfiliev-bs-sweep-v2-stickymoneyness`): the spot sweep reprices each leg's IV at the scenario-spot's moneyness via a per-expiry quadratic skew curve fitted at snapshot time — sticky-moneyness rather than sticky-IV — so the zero-gamma estimate is no longer biased upward by the steep put-side skew. The envelope breaks the headline into a near bucket (DTE ≤ 7 days; ~59% of 2025 SPX volume is 0DTE) and a term bucket (DTE > 7), surfaced as `zero_gamma_near` / `zero_gamma_term` alongside the combined value. **Default scope is combined** (`scope: \"spy+spx\"`): the daemon runs SPY (the S&P 500 ETF — continuous extended-hours quoting on SMART/ARCA) and SPX (the index, both SPX-class AM-settled monthlies and SPXW-class PM-settled weeklies — AM/PM settlement is honoured in the DTE filter) serially in one job and returns top-level headline fields plus a per-index breakdown under `per_index.SPY` and `per_index.SPX`, each a fully-formed single-underlying result envelope. The result's `scope` field discriminates `\"spy+spx\"` / `\"spy\"` / `\"spx\"`. **Regime agreement classifier** (combined scope only): the result envelope carries `regime_agreement` — one of `\"agree:long-gamma\"` / `\"agree:short-gamma\"` / `\"agree:flipping\"` / `\"disagree\"` / `\"\"` (no data) — derived directly from per-index sweep outcomes. `\"disagree\"` is the actionable signal (one index stabilising, the other amplifying — institutional/retail positioning divergence). No fused \"combined γ-zero spot X%\" number is exposed: the spot² scaling makes SPX dominate the dollar sum 75-80%, so a combined number would be SPX-only with epsilon SPY noise; consumers wanting one should derive it from `per_index`. **Entitlement-graceful fallback**: when the account lacks CBOE OPRA entitlements (354) or the SPX chain is unreachable (200, no-data, timeout), a combined-scope request drops back to SPY-only and `warnings` carries an `spx_unavailable:<reason>` token (reasons: `354` entitlement gap, `200` contract resolution rejected, `no_data` fan-out landed 0 legs, or a short failure token). Partial cases (one SPX trading class lands, the other 354s) surface in `partial_classes` (map keyed by the unreachable class, e.g. `{\"SPX\": \"354\"}`). Pass `scope: \"spy\"` for the bit-for-bit pre-coverage-arc SPY-only path (fastest, regression-target); `scope: \"spx\"` for SPX-only (errors out if SPX is unreachable rather than degrading). Returns two complementary signals on every ready result: a *signed* `zero_gamma` price level (regime hint, can invert near covered-call-ETF flow or autocall barriers) and a sign-agnostic `gamma_total_abs` / `top_strikes` view robust to the dealer-positioning assumption. In combined scope each `top_strikes` row carries an `underlying` field (`\"SPY\"` or `\"SPX\"`) so consumers can identify which book is driving the strike. Compute is heavy — the first call of an NY trading day kicks a multi-minute background job and returns `status: \"computing\"` with `eta_seconds`; subsequent callers within the same session receive `status: \"ready\"` instantly. Set `wait_ms` to block up to N milliseconds for the result (capped at the per-method deadline). `force: true` ignores the cached result and starts fresh. Honest caveat: this is a regime hint, not a precision level — for the full methodology see `docs/specs/risk-regime-dashboard.md` and `docs/design/gamma-spx-coverage.md`.",
		JSONSchema: schemaObject(map[string]json.RawMessage{
			"wait_ms": json.RawMessage(`{"type":"integer","minimum":0,"description":"block up to this many ms for the result; 0 (default) returns the current status immediately"}`),
			"force":   json.RawMessage(`{"type":"boolean","description":"diagnostics-only: ignore the cached result and start a fresh compute; default false"}`),
			"scope":   json.RawMessage(`{"type":"string","enum":["spy","spx","spy+spx"],"description":"which underlying(s) to compute: 'spy+spx' (combined, the default — falls back to SPY-only on SPX entitlement gap); 'spy' (SPY-only fast path, bit-for-bit pre-coverage-arc behaviour); 'spx' (SPX-only, errors out if SPX unreachable). Omit for the default combined view. Mirrors the CLI's --only flag."}`),
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
			return json.Marshal(res)
		},
	},
	{
		Name:        "ibkr_regime",
		Description: "Risk-regime snapshot: returns all five risk-regime dashboard indicators (VIX/VIX3M term structure, HYG vs SPY divergence, USD/JPY weekly move, **combined SPY+SPX dealer zero-gamma**, S&P 500 breadth) in one JSON envelope. The value isn't \"all five always populated\" — it's surfacing the *state* of each indicator in one round trip. Each row carries raw measurements plus a `notes` field embedding the spec's threshold bands verbatim; green / yellow / red derivation is the consumer's job. Each row also carries a `streak` field — `{band, sessions, since}` — counting how many consecutive trading sessions the indicator has been in its current band; useful for distinguishing day 1 of a stress event from day 5 (the spec's \"sustained 2-3 days, not single spikes\" language). The gamma row additionally surfaces `zero_gamma_near` (DTE ≤ 7) and `zero_gamma_term` (DTE > 7) alongside the combined-time-horizon headline plus a `horizon_agreement` field flagging `diverge` when near and term γ-zero land on opposite sides of spot; under the hood the row is sourced from the same combined SPY+SPX compute as `ibkr_gamma` (the result envelope's `per_index` breakdown is accessible via `ibkr_gamma` if the consumer wants the per-underlying split). The methodology token for the gamma compute is now `perfiliev-bs-sweep-v2-stickymoneyness`: the sweep reprices each leg's IV at the scenario-spot's moneyness via a per-expiry skew curve rather than holding it fixed at the snapshot value. Expect these failure modes: Indicator 4 (combined SPY+SPX dealer zero-gamma) returns status=\"computing\" with an `eta_seconds` on the first call of an NY trading day (multi-minute background compute); subsequent calls return status=\"ok\". Indicator 5 (SPX breadth) returns status=\"computing\" on the first call against a fresh daemon (the local 50-DMA engine fans out across 500 S&P constituents at ~6 names/min — see `ibkr_breadth` for the cold-start budget); subsequent calls run from the persisted cache. Indicators 1-3 (VIX term, HYG/SPY, USD/JPY) may carry a `fields_missing` array listing optional sub-fields (e.g. `hyg_50dma`, `weekly_change_pct`) that didn't land within the fetch budget — the row's primary measurement still landed, so treat fields_missing as a render hint, not an error. The result also carries `spec_doc` pointing at the canonical methodology reference. Useful for: \"how does the market regime look today?\", \"are we close to any of the spec's regime-shift thresholds?\", \"give me the daily-check dashboard.\"",
		JSONSchema:  schemaObject(nil, nil),
		Handler: func(ctx context.Context, conn *dial.Conn, _ json.RawMessage) (json.RawMessage, error) {
			var res rpc.RegimeSnapshotResult
			if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &res); err != nil {
				return nil, err
			}
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
	"setup":   "local configuration verb (writes claude_desktop_config.json); not a daemon RPC, no LLM should ever call it",
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
