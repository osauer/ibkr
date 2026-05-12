// Package rpc defines the wire types and method names for the daemon's
// newline-delimited JSON-RPC protocol, plus the small typed payloads used
// by both the daemon (server side) and the CLI (client side).
//
// The wire envelope is intentionally tiny: one JSON object per line, an "id"
// to correlate requests with responses, an "ok" boolean, and either a
// "result" or "error" payload. Streaming responses share an id and emit
// "frame" objects until a closing {"end": true}.
package rpc

import (
	"encoding/json"
	"time"
)

// Method names. Keep stable; the CLI sends these as strings.
const (
	MethodAccountSummary = "account.summary"
	MethodPositionsList  = "positions.list"
	MethodQuoteSnapshot  = "quote.snapshot"
	MethodQuoteSubscribe = "quote.subscribe"
	MethodChainFetch     = "chain.fetch"
	MethodChainExpiries  = "chain.expiries"
	MethodScanRun        = "scan.run"
	MethodScanList       = "scan.list"
	MethodScanParams     = "scan.params"
	MethodHistoryDaily   = "history.daily"
	MethodStatusHealth   = "status.health"
	MethodCancel         = "cancel"
	MethodOrderPlace     = "order.place"  // refused in v1
	MethodOrderCancel    = "order.cancel" // refused in v1
)

// Error codes used in Error.Code. CLI maps these to user-facing messages.
const (
	CodeUnknownMethod      = "unknown_method"
	CodeBadRequest         = "bad_request"
	CodeDaemonUnavailable  = "daemon_unavailable"
	CodeGatewayUnavailable = "gateway_unavailable"
	CodeSymbolInactive     = "symbol_inactive"
	CodeTimeout            = "timeout"
	CodeTradingDisabled    = "trading_disabled"
	CodeInternal           = "internal"
)

// Frame-level error codes used in FrameError.Code. These are terminal: a
// frame carrying any of these is the last frame the consumer will receive
// on its subscription. Distinct from the request-envelope error codes
// because the wire shape (frame, not Error) and lifecycle (mid-stream
// vs synchronous) are different concerns.
const (
	FrameErrGatewayLost          = "gateway_lost"
	FrameErrEntitlementLost      = "entitlement_lost"
	FrameErrSubscriptionRejected = "subscription_rejected"
	FrameErrDaemonShutdown       = "daemon_shutdown"
)

// Request is the envelope sent from CLI to daemon.
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the envelope sent from daemon to CLI.
type Response struct {
	ID     string          `json:"id"`
	Ok     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Frame  json.RawMessage `json:"frame,omitempty"`
	Stream bool            `json:"stream,omitempty"`
	End    bool            `json:"end,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is the structured error payload for a failed request.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error implements the error interface so callers can return *Error.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

// ContractParams names a tradeable instrument. SecType "STK" for stocks/ETFs;
// "OPT" for options (Expiry, Strike, Right required).
type ContractParams struct {
	Symbol   string  `json:"symbol"`
	SecType  string  `json:"sec_type,omitempty"` // STK | OPT
	Exchange string  `json:"exchange,omitempty"` // SMART
	Currency string  `json:"currency,omitempty"`
	Expiry   string  `json:"expiry,omitempty"` // YYYYMMDD
	Strike   float64 `json:"strike,omitempty"`
	Right    string  `json:"right,omitempty"` // C | P
}

// QuoteSnapshotParams is the input for MethodQuoteSnapshot.
type QuoteSnapshotParams struct {
	Contract  ContractParams `json:"contract"`
	TimeoutMs int            `json:"timeout_ms,omitempty"`
}

// QuoteSubscribeParams is the input for MethodQuoteSubscribe.
type QuoteSubscribeParams struct {
	Contract ContractParams `json:"contract"`
}

// PositionsListParams supports filters in future versions; v1 ignores fields.
type PositionsListParams struct {
	Symbol string `json:"symbol,omitempty"`
	Type   string `json:"type,omitempty"` // stk | opt
}

// ChainFetchParams selects strikes around the spot price for an expiry.
type ChainFetchParams struct {
	Symbol string `json:"symbol"`
	Expiry string `json:"expiry"` // YYYY-MM-DD
	Width  int    `json:"width"`  // ATM ± width
	Side   string `json:"side"`   // calls | puts | both
}

// ScanRunParams runs a scanner. Two modes:
//
//  1. Preset shorthand: set Preset to the name of a [scans.<name>] block
//     from config.toml (or one of the built-in defaults). Type/Exchange
//     are ignored.
//  2. Ad-hoc: leave Preset empty and set Type (scanCode) and Exchange
//     (locationCode) directly. Useful for agents that don't want to
//     persist a preset to the user's config file.
//
// Exactly one of Preset or Type is required. Limit is optional in both
// modes; <=0 falls back to the preset's configured Limit (mode 1) or
// the daemon's hard cap of 50 (mode 2).
type ScanRunParams struct {
	Preset   string `json:"preset,omitempty"`
	Type     string `json:"type,omitempty"`
	Exchange string `json:"exchange,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// ScanParamsParams requests the gateway's full scanner catalog. Instrument
// filters the ScanTypes list to those valid for the given instrument
// (e.g. "STK"); empty returns every type. IncludeRawXML attaches the raw
// XML payload to the response for callers that want to grep for fields
// not surfaced in the parsed struct (filter values, instrument flags,
// etc.). The XML is typically ~200 KB on a US Pro gateway.
type ScanParamsParams struct {
	Instrument    string `json:"instrument,omitempty"`
	IncludeRawXML bool   `json:"include_raw_xml,omitempty"`
}

// ScanParamsResult mirrors pkg/ibkr.ScannerParameters but stays in the
// rpc package so consumers (CLI, MCP) don't need to import pkg/ibkr.
// Code comments on the wire-level types live with the parser.
type ScanParamsResult struct {
	Instruments []ScanParamInstrument `json:"instruments"`
	Locations   []ScanParamLocation   `json:"locations"`
	ScanTypes   []ScanParamScanType   `json:"scan_types"`
	RawXML      string                `json:"raw_xml,omitempty"`
	AsOf        time.Time             `json:"as_of"`
}

// ScanParamInstrument is one row in ScanParamsResult.Instruments.
type ScanParamInstrument struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ScanParamLocation is one row in ScanParamsResult.Locations.
type ScanParamLocation struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
}

// ScanParamScanType is one row in ScanParamsResult.ScanTypes. Instruments
// is the list of instrument-type tokens this scan is valid for (e.g.
// ["STK", "ETF"]); empty means "all".
type ScanParamScanType struct {
	Code        string   `json:"code"`
	DisplayName string   `json:"display_name"`
	Instruments []string `json:"instruments,omitempty"`
}

// CancelParams cancels an in-flight stream by id.
type CancelParams struct {
	ID string `json:"id"`
}

// HistoryDailyParams requests N days of daily OHLCV bars for a symbol.
type HistoryDailyParams struct {
	Symbol string `json:"symbol"`
	Days   int    `json:"days,omitempty"` // default 90 when zero
}

// HistoryBar is one daily OHLCV row.
type HistoryBar struct {
	Date   string  `json:"date"` // YYYY-MM-DD
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume"`
}

// HistoryDailyResult wraps the daily bars for the CLI.
type HistoryDailyResult struct {
	Symbol   string       `json:"symbol"`
	Days     int          `json:"days"`
	DataType string       `json:"data_type"`
	Bars     []HistoryBar `json:"bars"`
	AsOf     time.Time    `json:"as_of"`
}

// Quote is the daemon's snapshot result.
//
// PrevClose / Change / ChangePct are non-nil only when the gateway has
// delivered both the previous regular-session close (tick 9) and a
// current Last (tick 4). Pre-market with no live ticks: Last is nil and
// so are Change / ChangePct, but PrevClose typically still arrives — the
// honest answer is "yesterday closed at X, no live print yet". No
// fabrication: never substitute mid-of-bid-ask for Last when computing
// Change.
type Quote struct {
	Symbol    string         `json:"symbol"`
	Contract  ContractParams `json:"contract"`
	Bid       *float64       `json:"bid"`
	Ask       *float64       `json:"ask"`
	Last      *float64       `json:"last"`
	PrevClose *float64       `json:"prev_close"`
	Change    *float64       `json:"change"`
	ChangePct *float64       `json:"change_pct"`
	BidSize   *int           `json:"bid_size,omitempty"`
	AskSize   *int           `json:"ask_size,omitempty"`
	Volume    *int64         `json:"volume,omitempty"`
	IV        *float64       `json:"iv"`
	IVStatus  string         `json:"iv_status"`
	DataType  string         `json:"data_type"`
	AsOf      time.Time      `json:"as_of"`
}

// Frame is a single streaming tick. DataType carries the gateway's
// per-reqID market-data-type notice (live / frozen / delayed /
// delayed-frozen) so the CLI can render a badge — important after
// hours, where frozen mode delivers a single snapshot and then goes
// silent. Empty string means "unknown" (the gateway hasn't sent the
// notice yet); the CLI treats that the same as "live" for rendering.
//
// Error is the terminal-error variant: when populated, the price/size
// fields are nil and this is the last frame the consumer will receive
// on the subscription. Codes are the FrameErr* constants. Backward-
// compatible because of omitempty — older consumers parsing tick frames
// see no Error field and continue to work.
type Frame struct {
	T        time.Time   `json:"t"`
	Bid      *float64    `json:"bid,omitempty"`
	Ask      *float64    `json:"ask,omitempty"`
	Last     *float64    `json:"last,omitempty"`
	BidSize  *int        `json:"bid_size,omitempty"`
	AskSize  *int        `json:"ask_size,omitempty"`
	DataType string      `json:"data_type,omitempty"`
	Error    *FrameError `json:"error,omitempty"`
}

// FrameError is the terminal error payload carried in Frame.Error. Code is
// one of the FrameErr* constants; Message is a single-sentence human
// description suitable for surfacing in CLI/MCP client output.
type FrameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PositionView is the wire shape of a single position returned to the CLI.
//
// DayChange / DayChangePct describe how far Mark sits from the underlying's
// previous regular-session close. Pointers so "no data" (pre-market with
// no tick 9 yet, options where we don't track contract-level prev close)
// is distinct from "exactly flat". The daemon caches prev close per
// underlying so the first call pre-warms and subsequent ones are
// instant — no fabrication.
//
// MarketValueCcy and FXRate carry the contract-currency view: MarketValue
// remains in account base currency for back-compat, but for a USD position
// in a EUR account MarketValueCcy is the USD figure and FXRate is the
// gateway-reported BASE/CCY conversion. Both nil/zero on same-currency
// books — no synthesis.
//
// Delta/Gamma/Theta/Vega populate on option positions when the daemon
// captured a valid model-computation tick within budget. nil = unavailable
// (illiquid leg, OOH model abstention, subscribe slot churn) — never zero-
// substituted.
type PositionView struct {
	Symbol         string   `json:"symbol"`
	SecType        string   `json:"sec_type"`
	Exchange       string   `json:"exchange,omitempty"`
	Currency       string   `json:"currency,omitempty"`
	Quantity       float64  `json:"quantity"`
	AvgCost        float64  `json:"avg_cost"`
	Mark           float64  `json:"mark"`
	PrevClose      *float64 `json:"prev_close,omitempty"`
	DayChange      *float64 `json:"day_change,omitempty"`
	DayChangePct   *float64 `json:"day_change_pct,omitempty"`
	MarketValue    float64  `json:"market_value"`
	MarketValueCcy *float64 `json:"market_value_ccy,omitempty"`
	FXRate         *float64 `json:"fx_rate,omitempty"`
	UnrealizedPnL  float64  `json:"unrealized_pnl"`
	RealizedPnL    float64  `json:"realized_pnl"`

	// Option-only fields (zero values when not applicable).
	Expiry string  `json:"expiry,omitempty"`
	Strike float64 `json:"strike,omitempty"`
	Right  string  `json:"right,omitempty"`

	Delta *float64 `json:"delta,omitempty"`
	Gamma *float64 `json:"gamma,omitempty"`
	Theta *float64 `json:"theta,omitempty"`
	Vega  *float64 `json:"vega,omitempty"`

	// Option-only contract-level fields populated from the per-leg
	// market-data subscription that captures Greeks (msg 21 tickType 13)
	// plus tick 1/2/9 for the option itself. Nil when the subscription
	// budget expired without delivering the tick — never zero-substituted.
	//
	// OptionBid / OptionAsk let callers detect a wide spread on illiquid
	// contracts where the mark is a midpoint that may not be tradable.
	// OptionPrevClose is the option contract's own prior settle (NOT the
	// underlying's PrevClose above); required for option-level daily P&L
	// without the underlying-vs-option confusion the agent-feedback flagged.
	// IV is the model-computation implied volatility for this leg.
	OptionBid       *float64 `json:"option_bid,omitempty"`
	OptionAsk       *float64 `json:"option_ask,omitempty"`
	OptionPrevClose *float64 `json:"option_prev_close,omitempty"`
	IV              *float64 `json:"iv,omitempty"`
}

// PositionsResult wraps the array so the daemon can attach metadata later.
// ByUnderlying groups stock + option legs per underlying — always populated
// so JSON consumers can rely on it. The CLI's `--by underlying` view renders
// from this; the default view keeps the flat Stocks/Options arrays.
//
// Portfolio is populated when at least one option leg has captured Greeks
// and/or any non-base currency holding has a known FX rate. Aggregates are
// computed daemon-side so JSON consumers and the CLI render the same
// numbers. Always-non-nil pointer; fields inside are nil when their inputs
// were unavailable — see PositionsPortfolio doc for the contract.
type PositionsResult struct {
	DataType     string              `json:"data_type"`
	AsOf         time.Time           `json:"as_of"`
	Stocks       []PositionView      `json:"stocks"`
	Options      []PositionView      `json:"options"`
	ByUnderlying []PositionGroup     `json:"by_underlying"`
	Portfolio    *PositionsPortfolio `json:"portfolio,omitempty"`
	AccountID    string              `json:"account_id,omitempty"`
}

// PositionsPortfolio is the daemon-side aggregator across all open legs.
//
// EffectiveDelta sums per-leg signed share-equivalent exposure:
//   - stocks contribute their signed quantity (long 100 sh => +100)
//   - options contribute delta × signed contract qty × multiplier
//
// DollarDelta multiplies each leg's share-equivalent exposure by the
// leg's contract-currency spot and sums; result is reported in
// DollarDeltaCurrency. For a mixed-currency book this is the dominant
// contract currency (e.g. USD when every option references a USD
// underlying); callers that need a single base-currency rollup combine
// it with the AccountResult.CurrencyExposure FX rate.
//
// DailyTheta is Σ (theta × signed contract qty × multiplier). IBKR
// already reports theta as daily decay, so the sum is the daily P&L
// from time decay assuming everything else holds (in contract
// currency).
//
// GreeksCoverage is the count of option legs whose Greeks were captured
// over the total — useful for the renderer to flag partial coverage
// when the model tick didn't arrive for some legs.
type PositionsPortfolio struct {
	EffectiveDelta      *float64 `json:"effective_delta,omitempty"`
	DollarDelta         *float64 `json:"dollar_delta,omitempty"`
	DollarDeltaCurrency string   `json:"dollar_delta_currency,omitempty"`
	DailyTheta          *float64 `json:"daily_theta,omitempty"`
	Gamma               *float64 `json:"gamma,omitempty"`
	Vega                *float64 `json:"vega,omitempty"`
	GreeksCoverage      int      `json:"greeks_coverage"`
	GreeksTotal         int      `json:"greeks_total"`

	// FXSensitivityPerPct estimates the change in base-currency P&L for a 1%
	// move in the non-base contract currency, holding everything else
	// constant. Computed as Σ (non-base market value in base) × 0.01.
	// Useful as a quick answer to "how much of my book is FX-exposed?".
	FXSensitivityPerPct *float64 `json:"fx_sensitivity_per_pct,omitempty"`
	FXBaseCurrency      string   `json:"fx_base_currency,omitempty"`
}

// PositionGroup aggregates the stock leg (if any) and option legs per
// underlying. GroupUnrealizedPnL/GroupMarketValue are sums across all legs.
type PositionGroup struct {
	Underlying         string         `json:"underlying"`
	Stock              *PositionView  `json:"stock,omitempty"`
	Options            []PositionView `json:"options"`
	GroupMarketValue   float64        `json:"group_market_value"`
	GroupUnrealizedPnL float64        `json:"group_unrealized_pnl"`
}

// AccountResult is the wire shape of MethodAccountSummary.
//
// CurrencyExposure decomposes the portfolio by contract currency: one
// row per non-base currency the gateway reported via $LEDGER:ALL. Rows
// reconcile within ~0.5%: NetLiquidationCcy × ExchangeRate ≈ contribution
// to base NetLiquidation. Empty array on a same-currency account.
type AccountResult struct {
	AccountID         string             `json:"account_id"`
	BaseCurrency      string             `json:"base_currency"`
	NetLiquidation    float64            `json:"net_liquidation"`
	BuyingPower       float64            `json:"buying_power"`
	AvailableFunds    float64            `json:"available_funds"`
	ExcessLiquidity   float64            `json:"excess_liquidity"`
	TotalCash         float64            `json:"total_cash"`
	MaintenanceMargin float64            `json:"maintenance_margin"`
	InitialMargin     float64            `json:"initial_margin"`
	CurrencyExposure  []CurrencyExposure `json:"currency_exposure,omitempty"`
	DataType          string             `json:"data_type"`
	AsOf              time.Time          `json:"as_of"`
}

// CurrencyExposure is one row in AccountResult.CurrencyExposure.
// Values are reported in the named currency (the "Ccy" suffix); the
// ExchangeRate field is BASE per CCY (i.e. "how many base-currency
// units 1 unit of the named currency converts to" — matches IBKR's
// $LEDGER semantics so reconciliation works without inversion).
// Fields are populated only when the gateway delivered them; absent
// fields are 0, never fabricated.
type CurrencyExposure struct {
	Currency             string  `json:"currency"`
	NetLiquidationCcy    float64 `json:"net_liquidation_ccy"`
	CashCcy              float64 `json:"cash_ccy"`
	StockMarketValueCcy  float64 `json:"stock_market_value_ccy"`
	OptionMarketValueCcy float64 `json:"option_market_value_ccy"`
	UnrealizedPnLCcy     float64 `json:"unrealized_pnl_ccy"`
	RealizedPnLCcy       float64 `json:"realized_pnl_ccy"`
	ExchangeRate         float64 `json:"exchange_rate"`
	NetLiquidationBase   float64 `json:"net_liquidation_base"`
}

// ChainStrike is one strike row in a chain.
type ChainStrike struct {
	Strike float64 `json:"strike"`
	IsATM  bool    `json:"is_atm,omitempty"`

	CallBid   *float64 `json:"call_bid"`
	CallAsk   *float64 `json:"call_ask"`
	CallLast  *float64 `json:"call_last"`
	CallIV    *float64 `json:"call_iv"`
	CallDelta *float64 `json:"call_delta"`
	CallOI    *int64   `json:"call_oi"`

	PutBid   *float64 `json:"put_bid"`
	PutAsk   *float64 `json:"put_ask"`
	PutLast  *float64 `json:"put_last"`
	PutIV    *float64 `json:"put_iv"`
	PutDelta *float64 `json:"put_delta"`
	PutOI    *int64   `json:"put_oi"`
}

// ChainExpiriesParams is the input for MethodChainExpiries.
//
// WithIV asks the daemon to fetch ATM implied volatility per expiry. The
// daemon caches results, picks the ATM strike per expiry, and runs the
// per-expiry subscribes through a bounded worker pool — first call costs
// a few seconds for a typical name, subsequent calls within the cache
// TTL are instant.
//
// AllExpiries lifts the default cap (the 12 nearest expiries). Off by
// default because the back-half LEAPS are rarely consulted and pay the
// IV-fetch cost for no decision value.
//
// Empty Symbol → bad_request.
type ChainExpiriesParams struct {
	Symbol      string `json:"symbol"`
	WithIV      bool   `json:"with_iv,omitempty"`
	AllExpiries bool   `json:"all_expiries,omitempty"`
}

// ChainExpiry is one row in MethodChainExpiries' response. IV is nil when
// --with-iv wasn't requested or when the per-strike IV fetch timed out;
// IVStatus disambiguates ("ok" | "unavailable" | "timeout").
//
// DTE is the integer day count from "today (local)" to the expiry date,
// inclusive of the expiry day (so a same-day expiry has DTE=0, next-day
// has DTE=1). Surfaced separately from ImpliedMove so consumers can
// derive their own term-structure math.
//
// ImpliedMove is the 1-σ expected dollar move by expiration, computed as
// spot × IV × √(DTE/365). Populated only when IV and spot are both
// available; otherwise nil. The matching ImpliedMovePct is the same value
// expressed as a fraction of spot (so `0.042` means 4.2%).
type ChainExpiry struct {
	Date           string   `json:"date"` // YYYY-MM-DD
	DTE            int      `json:"dte,omitempty"`
	IV             *float64 `json:"iv,omitempty"`
	IVStatus       string   `json:"iv_status,omitempty"`
	ImpliedMove    *float64 `json:"implied_move,omitempty"`
	ImpliedMovePct *float64 `json:"implied_move_pct,omitempty"`
}

// ChainExpiriesResult is MethodChainExpiries' payload. Expiries are sorted
// ascending and deduped across exchanges by the daemon.
//
// Spot is the underlying mid the daemon used to pick the per-expiry ATM
// strike and to compute ImpliedMove. Zero when the spot probe failed or
// WithIV wasn't requested.
type ChainExpiriesResult struct {
	Symbol   string        `json:"symbol"`
	Spot     float64       `json:"spot,omitempty"`
	Expiries []ChainExpiry `json:"expiries"`
	AsOf     time.Time     `json:"as_of"`
}

// ChainResult is MethodChainFetch's payload.
type ChainResult struct {
	Symbol   string        `json:"symbol"`
	Spot     float64       `json:"spot"`
	Expiry   string        `json:"expiry"`
	DTE      int           `json:"dte"`
	DataType string        `json:"data_type"`
	Strikes  []ChainStrike `json:"strikes"`
	AsOf     time.Time     `json:"as_of"`
}

// ScanRow is one row of a scanner result. The IBKR scanner subscription
// only returns rank+symbol+three-mostly-empty-comment-fields per row, so
// every numeric field below is populated by the daemon via a follow-up
// snapshot subscribe on the symbol. Pointers (not scalars) so consumers
// can distinguish "the gateway didn't deliver this tick within the
// enrichment window" from "the value is genuinely zero" — the no-fabrication
// invariant. Comment carries the raw scanner-side text when non-empty
// (rare; most scan types leave it blank).
type ScanRow struct {
	Rank       int      `json:"rank"`
	Symbol     string   `json:"symbol"`
	Last       *float64 `json:"last,omitempty"`
	PrevClose  *float64 `json:"prev_close,omitempty"`
	Change     *float64 `json:"change,omitempty"`
	ChangePct  *float64 `json:"change_pct,omitempty"`
	Volume     *int64   `json:"volume,omitempty"`
	IV         *float64 `json:"iv,omitempty"`
	Week52High *float64 `json:"week_52_high,omitempty"`
	Week52Low  *float64 `json:"week_52_low,omitempty"`
	Comment    string   `json:"comment,omitempty"`
}

// ScanResult wraps the rows.
type ScanResult struct {
	Preset string    `json:"preset"`
	Type   string    `json:"type"`
	Rows   []ScanRow `json:"rows"`
	AsOf   time.Time `json:"as_of"`
}

// ScanListResult enumerates configured presets.
type ScanListResult struct {
	Presets []ScanPresetSummary `json:"presets"`
}

// ScanPresetSummary describes a single preset entry in scan list.
type ScanPresetSummary struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Exchange string `json:"exchange"`
	Limit    int    `json:"limit"`
}

// HealthResult is the response to MethodStatusHealth.
//
// PortOrigin / TLSOrigin record how the daemon arrived at the values
// shown — "pinned" (user wrote them in config), "discovered" (probe found
// the gateway), or "default" (built-in fallback). Alternates lists other
// ports that responded during discovery but lost the first-hit race.
// Empty alternates is the common case (single gateway up).
type HealthResult struct {
	DaemonVersion string    `json:"daemon_version"`
	DaemonStarted time.Time `json:"daemon_started"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	Account       string    `json:"account,omitempty"`
	GatewayHost   string    `json:"gateway_host"`
	GatewayPort   int       `json:"gateway_port"`
	GatewayTLS    bool      `json:"gateway_tls"`
	NegotiatedTLS bool      `json:"negotiated_tls"`
	PortOrigin    string    `json:"port_origin"`
	TLSOrigin     string    `json:"tls_origin"`
	Alternates    []int     `json:"alternates,omitempty"`
	ClientID      int       `json:"client_id"`
	Connected     bool      `json:"connected"`
	DataType      string    `json:"data_type,omitempty"`
	ServerVersion int       `json:"server_version,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}
