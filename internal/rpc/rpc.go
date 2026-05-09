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

// ScanRunParams runs a configured scanner preset.
type ScanRunParams struct {
	Preset string `json:"preset"`
	Limit  int    `json:"limit,omitempty"`
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
type Quote struct {
	Symbol   string         `json:"symbol"`
	Contract ContractParams `json:"contract"`
	Bid      *float64       `json:"bid"`
	Ask      *float64       `json:"ask"`
	Last     *float64       `json:"last"`
	BidSize  *int           `json:"bid_size,omitempty"`
	AskSize  *int           `json:"ask_size,omitempty"`
	Volume   *int64         `json:"volume,omitempty"`
	IV       *float64       `json:"iv"`
	IVStatus string         `json:"iv_status"`
	DataType string         `json:"data_type"`
	AsOf     time.Time      `json:"as_of"`
}

// Frame is a single streaming tick.
type Frame struct {
	T       time.Time `json:"t"`
	Bid     *float64  `json:"bid,omitempty"`
	Ask     *float64  `json:"ask,omitempty"`
	Last    *float64  `json:"last,omitempty"`
	BidSize *int      `json:"bid_size,omitempty"`
	AskSize *int      `json:"ask_size,omitempty"`
}

// PositionView is the wire shape of a single position returned to the CLI.
type PositionView struct {
	Symbol        string  `json:"symbol"`
	SecType       string  `json:"sec_type"`
	Exchange      string  `json:"exchange,omitempty"`
	Currency      string  `json:"currency,omitempty"`
	Quantity      float64 `json:"quantity"`
	AvgCost       float64 `json:"avg_cost"`
	Mark          float64 `json:"mark"`
	MarketValue   float64 `json:"market_value"`
	UnrealizedPnL float64 `json:"unrealized_pnl"`
	RealizedPnL   float64 `json:"realized_pnl"`

	// Option-only fields (zero values when not applicable).
	Expiry string  `json:"expiry,omitempty"`
	Strike float64 `json:"strike,omitempty"`
	Right  string  `json:"right,omitempty"`
}

// PositionsResult wraps the array so the daemon can attach metadata later.
// ByUnderlying groups stock + option legs per underlying — always populated
// so JSON consumers can rely on it. The CLI's `--by underlying` view renders
// from this; the default view keeps the flat Stocks/Options arrays.
type PositionsResult struct {
	DataType     string          `json:"data_type"`
	AsOf         time.Time       `json:"as_of"`
	Stocks       []PositionView  `json:"stocks"`
	Options      []PositionView  `json:"options"`
	ByUnderlying []PositionGroup `json:"by_underlying"`
	AccountID    string          `json:"account_id,omitempty"`
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
type AccountResult struct {
	AccountID         string    `json:"account_id"`
	Profile           string    `json:"profile"`
	BaseCurrency      string    `json:"base_currency"`
	NetLiquidation    float64   `json:"net_liquidation"`
	BuyingPower       float64   `json:"buying_power"`
	AvailableFunds    float64   `json:"available_funds"`
	ExcessLiquidity   float64   `json:"excess_liquidity"`
	TotalCash         float64   `json:"total_cash"`
	MaintenanceMargin float64   `json:"maintenance_margin"`
	InitialMargin     float64   `json:"initial_margin"`
	DataType          string    `json:"data_type"`
	AsOf              time.Time `json:"as_of"`
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

// ChainExpiriesParams is the input for MethodChainExpiries. WithIV asks the
// daemon to fetch ATM IV per expiry — slow (one subscribe cycle per row), so
// it's opt-in. Empty Symbol → bad_request.
type ChainExpiriesParams struct {
	Symbol string `json:"symbol"`
	WithIV bool   `json:"with_iv,omitempty"`
}

// ChainExpiry is one row in MethodChainExpiries' response. IV is nil when
// --with-iv wasn't requested or when the per-strike IV fetch timed out;
// IVStatus disambiguates ("ok" | "unavailable" | "timeout").
type ChainExpiry struct {
	Date     string   `json:"date"` // YYYY-MM-DD
	IV       *float64 `json:"iv,omitempty"`
	IVStatus string   `json:"iv_status,omitempty"`
}

// ChainExpiriesResult is MethodChainExpiries' payload. Expiries are sorted
// ascending and deduped across exchanges by the daemon.
type ChainExpiriesResult struct {
	Symbol   string        `json:"symbol"`
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

// ScanRow is one row of a scanner result.
type ScanRow struct {
	Rank    int     `json:"rank"`
	Symbol  string  `json:"symbol"`
	Last    float64 `json:"last,omitempty"`
	Change  float64 `json:"change,omitempty"`
	Pct     float64 `json:"pct,omitempty"`
	Volume  int64   `json:"volume,omitempty"`
	Comment string  `json:"comment,omitempty"`
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
type HealthResult struct {
	DaemonVersion string    `json:"daemon_version"`
	DaemonStarted time.Time `json:"daemon_started"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	Profile       string    `json:"profile"`
	Account       string    `json:"account,omitempty"`
	GatewayHost   string    `json:"gateway_host"`
	GatewayPort   int       `json:"gateway_port"`
	GatewayTLS    bool      `json:"gateway_tls"`
	NegotiatedTLS bool      `json:"negotiated_tls"`
	ClientID      int       `json:"client_id"`
	Connected     bool      `json:"connected"`
	DataType      string    `json:"data_type,omitempty"`
	ServerVersion int       `json:"server_version,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}
