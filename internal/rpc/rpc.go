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
	MethodBreadthSPX     = "breadth.spx"
	MethodGammaZeroSPX   = "gamma.zero_spx"
	MethodRegimeSnapshot = "regime.snapshot"
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

// MarketDataType values carried on Quote.DataType, Frame.DataType, and
// ChainResult.DataType. IBKR's tickMarketDataType message (58) maps
// gateway feed state into one of these strings; the CLI renders a badge
// based on the value. HealthResult.DataType remains on the wire shape
// (omitempty) for renderer-fallback compatibility but is no longer
// written by the daemon — `status` has no per-reqID data-type to honestly
// report (the same reasoning that retired the field for AccountResult /
// PositionsResult / HistoryDailyResult in v0.15.0).
//
// Empty string means "the gateway hasn't sent a notice yet" — typically a
// few hundred ms after a fresh subscription. Treated as live for
// rendering purposes (see IsLiveDataType).
const (
	MarketDataLive          = "live"
	MarketDataFrozen        = "frozen"
	MarketDataDelayed       = "delayed"
	MarketDataDelayedFrozen = "delayed-frozen"
)

// IsLiveDataType reports whether the gateway's per-reqID feed state is
// "live ticks", treating empty-string the same as live (no notice yet).
// Used by renderers to decide whether to dim a row or show a phase badge.
func IsLiveDataType(dt string) bool {
	return dt == "" || dt == MarketDataLive
}

// IsOptionRTH reports whether the given instant falls within U.S. listed-
// equity-option regular trading hours: weekdays 09:30–16:00 ET.
//
// Used in preference to IsLiveDataType for option-context renderers (chain,
// option quotes) because the underlying ETF can stay "live" via extended-
// hours quoting on SMART/ARCA while the option markets themselves are
// closed — IsLiveDataType won't fire in that window, but the chain has
// no bid/ask and IVs come from IBKR's model-computation engine off
// prior-session prices. The clock-gated check captures that state.
//
// Holidays are NOT modeled. The fall-through is "open" on those days; the
// existing model-tick → BS-IV fallback chain keeps results usable, and a
// missed disclosure on a holiday is preferable to mis-flagging a regular
// session. If holiday accuracy becomes load-bearing, route the gateway's
// TradingHours field through and check against it instead.
//
// Fail-open: if the America/New_York zone can't be loaded (e.g. tzdata
// missing in a minimal container), returns true so the banner stays
// suppressed rather than firing during RTH.
func IsOptionRTH(now time.Time) bool {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return true
	}
	t := now.In(ny)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	open := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, ny)
	closeT := time.Date(t.Year(), t.Month(), t.Day(), 16, 0, 0, 0, ny)
	return !t.Before(open) && t.Before(closeT)
}

// SessionClass classifies an instant by its U.S. equity-options session
// phase. Callers that need different cadence in pre vs RTH vs post vs
// closed (most prominently the gamma cache's session-aware soft-TTL)
// branch on this rather than re-deriving the boundaries themselves.
//
// Boundaries (America/New_York):
//   - Pre   : weekdays 04:00–09:30
//   - RTH   : weekdays 09:30–16:00
//   - Post  : weekdays 16:00–20:00
//   - Closed: everything else (overnight + weekends)
//
// Holidays are NOT modeled — same fall-through policy as IsOptionRTH.
type SessionClass int

const (
	SessionClosed SessionClass = iota
	SessionPre
	SessionRTH
	SessionPost
)

// String renders the session class for log lines and debug output.
// Not load-bearing on the wire (the gamma cache holds the enum value
// directly), but used in test failure messages and warning logs.
func (c SessionClass) String() string {
	switch c {
	case SessionPre:
		return "pre"
	case SessionRTH:
		return "rth"
	case SessionPost:
		return "post"
	default:
		return "closed"
	}
}

// ClassifySession returns the SessionClass containing now. Fail-safe:
// if America/New_York can't be loaded (minimal container, missing
// tzdata), returns SessionRTH — the broadest "treat as active" answer
// so refresh cadence isn't accidentally disabled under degraded zone
// data. Mirrors IsOptionRTH's fail-open policy.
func ClassifySession(now time.Time) SessionClass {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return SessionRTH
	}
	t := now.In(ny)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return SessionClosed
	}
	pre := time.Date(t.Year(), t.Month(), t.Day(), 4, 0, 0, 0, ny)
	open := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, ny)
	closeT := time.Date(t.Year(), t.Month(), t.Day(), 16, 0, 0, 0, ny)
	post := time.Date(t.Year(), t.Month(), t.Day(), 20, 0, 0, 0, ny)
	switch {
	case t.Before(pre):
		return SessionClosed
	case t.Before(open):
		return SessionPre
	case t.Before(closeT):
		return SessionRTH
	case t.Before(post):
		return SessionPost
	default:
		return SessionClosed
	}
}

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

// SecType values carried on PositionView.SecType. The daemon maps IBKR's
// raw three-letter SecType codes ("STK", "OPT") onto the canonical wire
// values below in positionSecType — full words, not the short forms IBKR
// accepts on ContractParams (a different path; see the doc-comment there).
//
// Compare against these constants in renderers and filters rather than
// literal strings — the v0.12.4 "OPT" vs "OPTION" drift was the
// canonical "two callers, two literals" failure, prevented by a single
// source of truth.
const (
	SecTypeStock  = "STOCK"
	SecTypeOption = "OPTION"
	SecTypeFuture = "FUTURE"
	SecTypeIndex  = "INDEX"
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

// ContractParams names a tradeable instrument on the REQUEST side.
//
// Asymmetry to watch for: SecType here uses the IBKR API's three-letter
// short form ("STK", "OPT", "FUT", "IND") because that's what the gateway
// accepts in reqMktData / reqContractDetails. The RESPONSE side
// (PositionView.SecType) uses the full word ("STOCK", "OPTION", ...) —
// see the SecType* constants above. The two shapes flow on different
// paths and the gateway uses different vocabularies at each end; this
// type uses the request vocabulary.
//
// SecType "STK" for stocks/ETFs; "OPT" for options (Expiry, Strike,
// Right required).
type ContractParams struct {
	Symbol   string  `json:"symbol"`
	SecType  string  `json:"sec_type,omitempty"` // STK | OPT | FUT | IND (request-side; see asymmetry note)
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

// PositionsListParams filters the positions response. Both fields are
// honoured by the daemon (`internal/daemon/handlers.go::handlePositionsList`).
// Symbol matches the underlying (or the synthetic option key); empty returns
// every position. Type narrows to stocks ("stk") or options ("opt"); empty
// returns both. Filters are applied before the FX / Greeks decoration, so a
// narrowed query is also faster.
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

// HistoryDailyResult wraps the daily bars for the CLI. Historical daily
// bars are gateway-stored data with no live/delayed dimension; DataType
// is therefore unused on this response and kept only as a reserved field
// (omitempty) for shape parity with the streaming surfaces.
type HistoryDailyResult struct {
	Symbol   string       `json:"symbol"`
	Days     int          `json:"days"`
	DataType string       `json:"data_type,omitempty"`
	Bars     []HistoryBar `json:"bars"`
	AsOf     time.Time    `json:"as_of"`
}

// BreadthSPXParams is the input for MethodBreadthSPX. All fields are
// optional with sensible defaults — the dashboard generator calls this
// with empty params for the canonical view.
type BreadthSPXParams struct {
	// HistoryDays bounds the trailing daily series. Default 30 when
	// zero or negative; capped at 90 to keep the wire payload bounded.
	HistoryDays int `json:"history_days,omitempty"`
	// TimeoutMs bounds the wait when the engine has a fresh value but
	// the wire envelope is still being assembled. Default 5000 ms when
	// zero. Does not affect the multi-minute cold-start fan-out; that
	// path returns immediately with State="computing".
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

// BreadthDailyValue is one trailing daily breadth reading. The two
// SMA readings (50-day and 200-day) plus the constituent counts for
// new 52-week highs/lows are carried per session so a renderer can
// chart all four series in one history call. Units: percentages in
// [0, 100] for the SMA readings; raw counts for the highs/lows.
type BreadthDailyValue struct {
	Date           string  `json:"date"` // YYYY-MM-DD
	PctAbove50DMA  float64 `json:"pct_above_50dma"`
	PctAbove200DMA float64 `json:"pct_above_200dma,omitempty"`
	NewHighs       int     `json:"new_highs,omitempty"`
	NewLows        int     `json:"new_lows,omitempty"`
}

// BreadthState classifies the engine's compute-pipeline state at the
// moment a result envelope was assembled. Distinct from a generic
// "status" because the consumer's branching logic depends on which
// state the engine is in, not just whether the value is present:
//
//   - cold: no snapshot has ever been computed AND no refresh is in
//     flight. The engine exists but hasn't been kicked yet. Treat as
//     "indicator not yet available" — typically only seen during the
//     ~few-second window between daemon start and postConnectSetup
//     launching the scheduler.
//   - computing: a refresh is in flight. The Value/History fields may
//     reflect a prior snapshot (warm refresh) or be empty (cold-start
//     bootstrap). Renderers show a loading state.
//   - ready: a snapshot exists, no refresh in flight. Value/History
//     are authoritative.
//   - degraded: a snapshot exists but its coverage is below the
//     engine's threshold (e.g. partial fan-out completed). Value is
//     present but should be rendered with a warning — the underlying
//     constituent coverage is insufficient.
//
// Codified on the wire (rather than left as a side-channel via
// engine.IsRefreshing) so every consumer reads the same state without
// remembering to call a sibling method. Pre-v0.27.3 this gap caused
// fetchRegimeBreadth to mis-classify a poisoned Coverage=0 snapshot
// as "ok" because the side-channel check only fired on (value==0 AND
// history empty), and the poisoned envelope had history len 1.
type BreadthState string

const (
	BreadthStateCold      BreadthState = "cold"
	BreadthStateComputing BreadthState = "computing"
	BreadthStateReady     BreadthState = "ready"
	BreadthStateDegraded  BreadthState = "degraded"
)

// BreadthSPXResult is the payload for MethodBreadthSPX. The two
// SMA percentages plus the new-highs/lows count are surfaced as
// separate fields so consumers can read each independently;
// History carries the trailing daily series for sparkline
// rendering. Threshold derivation (green / yellow / red) is left to
// the renderer — the spec itself says thresholds should be tunable,
// so the daemon stays out of that policy choice.
//
// The Source / Method strings name the data provenance and
// computation path so renderers can disclose how the number was
// derived. Method is a short token; longer methodology disclosure
// lives in the spec doc.
type BreadthSPXResult struct {
	// State classifies the engine pipeline at the moment this envelope
	// was assembled (cold / computing / ready / degraded). Consumers
	// should branch on this, not on (value==0 && history==[]) heuristics.
	// See BreadthState docs for semantics.
	State BreadthState `json:"state"`
	// PctAbove50DMA is the current fast-window reading: percentage of
	// S&P 500 constituents trading above their own 50-day SMA. 0-100;
	// 50 is the symmetric midpoint. Spec rule of thumb: > 55 healthy,
	// 40-55 watch, < 40 with SPX at highs is the classic late-cycle
	// divergence. Zero is meaningful only when State == "ready" (a
	// State other than "ready" can carry the field at 0 as the "no
	// data yet" sentinel).
	PctAbove50DMA float64 `json:"pct_above_50dma"`
	// PctAbove200DMA is the slow-window reading: percentage above the
	// 200-day SMA. Caught the 1999 and 2021 cyclical tops cleanly.
	// Bands per locked plan: below 40% = red / 40-60% = yellow / above
	// 60% = green (calibrated to the post-Mag-7 era).
	PctAbove200DMA float64 `json:"pct_above_200dma"`
	// NewHighsToday is the count of constituents whose latest close
	// strictly exceeded their trailing 252-bar max (~1 year of
	// trading sessions ≈ "52-week high"). Names with < 252 sessions
	// of cached history are skipped.
	NewHighsToday int `json:"new_highs_today"`
	// NewLowsToday is the symmetric count for new 252-bar lows.
	NewLowsToday int `json:"new_lows_today"`
	// NetNewHighsPct is (NewHighsToday - NewLowsToday) / coverage × 100
	// where coverage is the count of names with enough history to
	// contribute. The classic "narrow rally" pattern is SPX near
	// highs with NetNewHighsPct near zero or negative — a small
	// number of mega-caps carrying the index while the median name
	// is rolling over.
	NetNewHighsPct float64 `json:"net_new_highs_pct"`
	// History is the trailing daily series, oldest first. Length is
	// bounded by BreadthSPXParams.HistoryDays. Each point carries
	// both SMA readings plus the new-highs/lows counts.
	History []BreadthDailyValue `json:"history,omitempty"`
	// Source identifies the data provenance for the headline value.
	// Free-form; renderers display verbatim.
	Source string `json:"source"`
	// Method is a short token naming the computation path so renderers
	// can disclose methodology. v2 token: "constituent-fanout-50/200dma-hl"
	// (50-DMA + 200-DMA + new highs/lows over 252-bar rolling
	// max/min — all computed locally from constituent daily closes
	// pulled via IBKR's historical-bar feed, since IBKR doesn't
	// redistribute the underlying S&P DJI / NYSE breadth indices on
	// retail subscriptions).
	Method string `json:"method"`
	// AsOf is the daemon's wall-clock when the result was assembled.
	AsOf time.Time `json:"as_of"`
	// SpotAt is the gateway-observation timestamp for the headline,
	// distinct from AsOf which covers history + headline.
	SpotAt time.Time `json:"spot_at,omitzero"`
	// DataType reflects the gateway's feed state when the headline
	// was captured — "live", "delayed", "frozen", "delayed-frozen",
	// or "" when no notice has arrived yet. Renderers use this to
	// dim the headline.
	DataType string `json:"data_type,omitempty"`
}

// GammaZeroSPXStatus values are returned on GammaZeroSPXResult.Status and
// drive the dashboard generator's "render the number" vs "render a
// loading state" choice. The compute is heavy (several minutes against
// hundreds of option legs) and runs on a daemon-internal goroutine, so
// the wire shape always carries a state — the first caller of the day
// receives "computing" and a renderer hint; subsequent callers receive
// "ready" with a cached payload until the next NY trading session.
//
// The four states mirror BreadthState's cold/computing/ready/error
// semantics so consumers can branch on Status uniformly across the two
// state-machine engines.
const (
	// GammaZeroStatusCold — no compute has been kicked this NY trading
	// session AND none is in flight. The daemon hasn't been asked to
	// produce a value yet. Distinct from Computing so a consumer can
	// tell "first caller of the day must kick or wait" from "wait,
	// this'll resolve on its own." Pre-v0.27.9 the cache conflated
	// these two states under Computing — same side-channel-inference
	// pattern the v0.27.3 breadth State enum retired.
	GammaZeroStatusCold = "cold"
	// GammaZeroStatusComputing — a background compute is in flight; the
	// EtaSeconds / Progress fields carry refresh hints. Callers who can
	// wait may set GammaZeroSPXParams.WaitMs > 0 on the request to block
	// up to that budget for the result.
	GammaZeroStatusComputing = "computing"
	// GammaZeroStatusReady — Result is populated and reflects the most
	// recent NY trading session's calculation.
	GammaZeroStatusReady = "ready"
	// GammaZeroStatusError — the last compute failed; Error carries the
	// classified reason. Callers retry by re-invoking the method.
	GammaZeroStatusError = "error"
)

// Scope values for GammaZeroSPXParams.Scope. Discriminator for the
// SPY+SPX coverage arc: today's SPY-only path is "spy"; SPX-only and
// combined arrive in the same coverage arc. Empty Scope defaults to
// "spy+spx" (combined when both reachable, SPY-only otherwise) once
// step 7+8 land; until then it falls back to "spy".
const (
	GammaZeroScopeSPY      = "spy"
	GammaZeroScopeSPX      = "spx"
	GammaZeroScopeCombined = "spy+spx"
)

// GammaZeroSPXParams is the input for MethodGammaZeroSPX. All fields are
// optional; defaults match the v1 calibration window documented in
// docs/specs/risk-regime-dashboard.md.
type GammaZeroSPXParams struct {
	// WaitMs is the maximum time the daemon blocks on an in-flight or
	// just-kicked-off compute before returning the current state. 0
	// (the default) means "return immediately with whatever state we
	// have." A non-zero value is capped daemon-side to keep the RPC
	// under the per-method deadline.
	WaitMs int `json:"wait_ms,omitempty"`
	// Force, when true, ignores a cached result for the current session
	// and kicks a fresh compute. Useful for diagnostics; the dashboard
	// generator should leave this off and let the daily cache handle
	// freshness.
	Force bool `json:"force,omitempty"`
	// Scope selects which underlying(s) to compute. One of GammaZeroScopeSPY
	// ("spy"), GammaZeroScopeSPX ("spx"), or GammaZeroScopeCombined ("spy+spx").
	// Empty defaults to "spy+spx" with SPX-skipped fallback.
	Scope string `json:"scope,omitempty"`
	// IncludeProfiles asks clients/renderers to retain the full profile
	// arrays in JSON/MCP responses. The daemon compute always produces
	// profiles where meaningful; thin adapters may strip them by default
	// to keep agent/tool payloads compact.
	IncludeProfiles bool `json:"include_profiles,omitempty"`
}

// GammaZeroParams echoes the v1 calibration window back to the caller so
// renderers can show "computed over N expirations within ±X%." Future
// versions can add fields here without breaking the result shape — every
// renderer-relevant tuning parameter lives on this echo.
type GammaZeroParams struct {
	// ExpiryCount is the number of nearest non-0DTE-post-settlement
	// expirations included in the aggregation.
	ExpiryCount int `json:"expiry_count"`
	// StrikeWidthPct is the half-width of the strike grid around spot,
	// expressed as a fraction (0.10 = ATM ± 10 %).
	StrikeWidthPct float64 `json:"strike_width_pct"`
	// SweepRangePct is the half-range of the spot sweep used to find
	// the zero-crossing (0.15 = [0.85, 1.15] × spot).
	SweepRangePct float64 `json:"sweep_range_pct"`
	// WorkerCount is the per-leg fan-out concurrency. 4 matches the
	// documented safe gateway throttle; bumping it requires retuning
	// AcquireMarketDataSlot.
	WorkerCount int `json:"worker_count"`
}

// GammaProfilePoint is one (spot, dealer_gex) sample from the sweep.
// GEX is signed under the Perfiliev convention (call gamma positive,
// put gamma negative); a sign flip across two adjacent points is what
// the renderer reads as a "zero crossing."
type GammaProfilePoint struct {
	Spot float64 `json:"spot"`
	GEX  float64 `json:"gex"`
}

// StrikeConcentration is one row of the "where dealer hedging
// concentrates" table — the top strikes ranked by sign-agnostic gamma
// notional |Γ| × OI × multiplier × spot² × 0.01. This is the more
// robust signal in regimes where the Perfiliev dealer-sign assumption
// can invert (covered-call ETF flow, autocall barrier proximity); the
// renderer can present it alongside ZeroGamma as a "call wall / put
// wall" view that's sign-convention agnostic.
type StrikeConcentration struct {
	// Underlying identifies which index this strike belongs to —
	// "SPY" or "SPX" today. Populated by single-underlying computes
	// with that compute's sym, and carried through the combined-scope
	// merge so the renderer can label per-row in the top-strikes
	// table without re-deriving from the trading class. Empty for
	// pre-v0.31 result envelopes; renderers should treat empty as
	// "the only underlying in scope" for back-compat.
	Underlying string `json:"underlying,omitempty"`
	// TradingClass is the listed class on the contract — "SPY",
	// "SPX" (AM-settled monthly), "SPXW" (PM-settled weekly).
	// Distinct from Underlying for SPX which lists both classes.
	// Empty in single-class results that don't need disambiguation.
	TradingClass string  `json:"trading_class,omitempty"`
	Strike       float64 `json:"strike"`
	Expiry       string  `json:"expiry"` // YYYY-MM-DD
	Right        string  `json:"right"`  // "C" | "P"
	AbsGEX       float64 `json:"abs_gex"`
	OI           int64   `json:"open_interest"`
}

// GammaLegDiagnosticCounts splits the priced-leg funnel into the
// conditions required for dealer GEX contribution. A leg can price and
// still fail to contribute when open interest is missing/zero, gamma is
// degenerate, or the resulting OI-weighted absolute GEX is zero.
type GammaLegDiagnosticCounts struct {
	PricedLegs        int `json:"priced_legs"`
	OpenInterestLegs  int `json:"oi_positive_legs"`
	GammaPositiveLegs int `json:"gamma_positive_legs"`
	AbsGEXLegs        int `json:"abs_gex_positive_legs"`
}

// GammaLegDiagnostics carries the leg-quality funnel for the whole
// result plus splits that identify whether the drop happened at the
// underlying or trading-class level (for example SPX vs SPXW).
type GammaLegDiagnostics struct {
	Total          GammaLegDiagnosticCounts            `json:"total"`
	ByUnderlying   map[string]GammaLegDiagnosticCounts `json:"by_underlying,omitempty"`
	ByTradingClass map[string]GammaLegDiagnosticCounts `json:"by_trading_class,omitempty"`
}

// GammaWarningDetail is the human/agent-facing warning surface. The
// daemon may use compact codes internally, but the wire carries this
// scoped explanation so renderers do not have to decode raw tokens.
type GammaWarningDetail struct {
	// Code is the stable warning token, without lossy prose parsing.
	// Examples: "throttled", "0dte_no_legs",
	// "spx_unavailable:354", "oi_missing".
	Code string `json:"code"`
	// Scope names the affected slice: "SPY", "SPX", "SPY+SPX", or a
	// narrower trading class / expiry when the condition is that local.
	Scope string `json:"scope,omitempty"`
	// Severity is one of "info", "data_quality", or "methodology".
	// Renderers can show data_quality prominently and tuck info under
	// an expanded view.
	Severity string `json:"severity,omitempty"`
	// Message is a short user-facing explanation of the condition.
	Message string `json:"message"`
	// Impact explains how to read the gamma result in light of the
	// warning. Empty when the message is self-contained.
	Impact string `json:"impact,omitempty"`
	// Action is an optional non-advisory operational next step, such as
	// retrying during RTH or suppressing a known SPX entitlement banner.
	Action string `json:"action,omitempty"`
}

// GammaIndexSummary is a compact interpretation of one per-underlying
// gamma compute. It gives agents and text renderers the answer they
// usually need without walking profile arrays or knowing the raw
// sign-convention details.
type GammaIndexSummary struct {
	Underlying      string   `json:"underlying,omitempty"`
	SpotUnderlying  float64  `json:"spot_underlying,omitempty"`
	ZeroGamma       *float64 `json:"zero_gamma,omitempty"`
	ZeroGammaStatus string   `json:"zero_gamma_status,omitempty"`
	Regime          string   `json:"regime,omitempty"`
	SweepLowAbs     float64  `json:"sweep_low_abs,omitempty"`
	SweepHighAbs    float64  `json:"sweep_high_abs,omitempty"`
	LegCount        int      `json:"leg_count,omitempty"`
	PricedLegCount  int      `json:"priced_leg_count,omitempty"`
	GammaTotalAbs   float64  `json:"gamma_total_abs,omitempty"`
	Confidence      string   `json:"confidence,omitempty"`
	Interpretation  string   `json:"interpretation,omitempty"`
}

// GammaZeroSummary is the compact, non-advisory readout of a gamma
// result. The raw fields remain canonical for charting and backtests;
// Summary is for humans and agents answering "which gamma zero, if
// any, did we identify?"
type GammaZeroSummary struct {
	PrimaryStatement string                       `json:"primary_statement,omitempty"`
	ZeroGammaStatus  string                       `json:"zero_gamma_status,omitempty"`
	Regime           string                       `json:"regime,omitempty"`
	Confidence       string                       `json:"confidence,omitempty"`
	NotAdvice        string                       `json:"not_advice,omitempty"`
	PerIndex         map[string]GammaIndexSummary `json:"per_index,omitempty"`
}

// SkewFitInfo is the per-expiry diagnostic for the sticky-moneyness
// skew curve fitted at snapshot time. Populated only when SkewModel
// reports a fitted model (e.g. "sticky-moneyness-v1"); a renderer can
// surface it as "skew fit: 12 pts · R² 0.94 · m ∈ [-0.12, +0.09]" so
// the reader can audit how well the curve actually fit the observed
// IVs before acting on the zero-gamma level it implies.
//
// Range is the moneyness window m = ln(K/S) the curve was fitted over;
// scenario spots that push a leg's moneyness outside the window are
// clamped to the boundary during the sweep.
type SkewFitInfo struct {
	Points   int        `json:"points"`
	RSquared float64    `json:"r_squared"`
	Range    [2]float64 `json:"range"`
}

// GammaZeroComputed is the actual zero-gamma payload — populated when
// GammaZeroSPXResult.Status is GammaZeroStatusReady. Kept as a separate
// struct so the envelope (Status / Eta / Progress / Result pointer)
// can evolve independently of the computation payload.
//
// Sign convention: ZeroGamma assumes the 2018-era "dealers long calls,
// short puts" Perfiliev convention. This is a defensible baseline at
// the SPX index level but can invert near autocallable barriers and
// when covered-call ETF flow dominates. Treat ZeroGamma as a regime
// hint, not a precise level; consult TopStrikes (sign-agnostic) for
// the more robust positioning view. See docs/specs/risk-regime-dashboard.md
// for the full methodology disclosure.
//
// Combined-scope semantics:
//
// When Scope == "spy+spx", there is intentionally no top-level
// ZeroGamma, GapPct, GammaSign, SpotUnderlying, or horizon bucket.
// SPY and SPX live on different price scales, so consumers must read
// per_index.SPY and per_index.SPX for price-level regime detail. The
// combined top level is limited to scale-safe diagnostics: summary,
// regime agreement, sign-agnostic magnitude, top strikes, counts,
// warnings, method/source, citations, timestamps, and the per-index map.
type GammaZeroComputed struct {
	// SpotUnderlying is the price of the underlying instrument
	// (currently SPY — see Source) at which the aggregation was
	// anchored. Field was renamed from SpotSPX when the compute moved
	// from SPX to the more liquid SPY chain (SPY has continuous
	// extended-hours quoting and a single trading class, which keeps
	// the compute robust off-hours).
	SpotUnderlying float64 `json:"spot_underlying,omitempty"`
	// SpotAt is the gateway-observation timestamp for SpotUnderlying.
	// Distinct from AsOf which covers the whole computation.
	SpotAt time.Time `json:"spot_at,omitzero"`

	// ZeroGamma is the dealer γ-zero level under the Perfiliev convention
	// (the spot where dealer net gamma crosses zero). nil when no
	// crossing exists within the sweep window — inspect GammaSign in
	// that case to learn whether the whole sweep is long-γ or short-γ.
	ZeroGamma *float64 `json:"zero_gamma,omitempty"`
	// GapPct is (SpotUnderlying − ZeroGamma) / ZeroGamma × 100. nil iff
	// ZeroGamma is nil. Sign convention: positive = spot above γ-zero
	// (dampening regime); negative = below γ-zero (amplifying regime).
	GapPct *float64 `json:"gap_pct,omitempty"`
	// GammaSign is "positive" or "negative" and is meaningful only when
	// ZeroGamma is nil — it tells the renderer which side of zero the
	// whole sweep landed on so the UI can say "all long-gamma" or "all
	// short-gamma in window."
	GammaSign string `json:"gamma_sign,omitempty"`
	// Profile is the full (spot, gex) sweep, oldest first. 60 points
	// over [0.85, 1.15] × SpotUnderlying. Renderers chart this as the
	// gamma-exposure curve and visually confirm the zero crossing.
	Profile []GammaProfilePoint `json:"profile,omitempty"`

	// GammaTotalAbs is the sign-agnostic magnitude signal at
	// SpotUnderlying: Σ |Γ| × OI × 100 × SpotUnderlying² × 0.01. In
	// dollar gamma terms — the total notional dealer hedging flow for
	// a 1% underlying move, independent of any positioning assumption.
	// Larger = market is more sensitive to dealer rebalancing.
	GammaTotalAbs float64 `json:"gamma_total_abs"`
	// GammaTotalAbsConvention names the sign-handling for GammaTotalAbs
	// so downstream renderers can label it without re-deriving
	// methodology. Today's value is "sign-agnostic" — every leg's
	// magnitude |Γ|·OI·100·spot²·0.01 is summed unconditionally. This
	// is the convention-free read the M2 methodology refresh promotes
	// to co-primary alongside the signed γ-zero level.
	GammaTotalAbsConvention string `json:"gamma_total_abs_convention,omitempty"`
	// TopStrikes is the top-N strikes ranked by absolute gamma notional.
	// Concentration here is more reliable than the signed ZeroGamma in
	// regimes where the dealer-sign assumption may invert.
	TopStrikes []StrikeConcentration `json:"top_strikes"`
	// TopConcentrationPct is TopStrikes[0].AbsGEX / GammaTotalAbs × 100 —
	// what share of the sign-agnostic |Γ|·OI sum is parked at the single
	// largest strike. Renderers surface it as a one-line "this strike
	// dominates" cue alongside the table. Zero when TopStrikes is empty
	// or GammaTotalAbs is zero.
	TopConcentrationPct float64 `json:"top_concentration_pct,omitempty"`

	// SweepLowAbs / SweepHighAbs are the absolute spot bounds of the
	// sweep window in dollars: SpotUnderlying × (1 ± Params.SweepRangePct).
	// Surfaced for renderers that want to print "γ-zero outside swept
	// range $A.AA–$B.BB" without re-deriving the multiplication.
	SweepLowAbs  float64 `json:"sweep_low_abs,omitempty"`
	SweepHighAbs float64 `json:"sweep_high_abs,omitempty"`

	// Expirations is the YYYY-MM-DD list of expirations actually
	// included in the aggregation (after 0DTE-post-settlement filtering
	// and SPXW/SPX merging).
	Expirations []string `json:"expirations"`
	// LegCount is the number of option legs that contributed non-zero
	// open-interest-weighted gamma exposure to the profile. It excludes
	// priced legs whose IV landed but whose OI was missing/zero, because
	// those legs cannot move dealer GEX.
	LegCount int `json:"leg_count"`
	// PricedLegCount is the number of option legs that delivered IV (or
	// a BS-IV fallback) and were usable for skew fitting. It can exceed
	// LegCount when IBKR supplied prices/IV but not open interest.
	PricedLegCount int `json:"priced_leg_count,omitempty"`
	// DerivedIVLegs counts how many priced legs used the BS-IV
	// Newton-Raphson fallback because the gateway never pushed a
	// model-computation tick. Pre-market this is often equal to
	// PricedLegCount (the model engine is idle); during regular hours it
	// should stay at 0. Renderers surface a "compute used N derived
	// IVs" disclosure so readers can tell those IVs came from option
	// quote/close inversion rather than live model ticks.
	DerivedIVLegs int `json:"derived_iv_legs,omitempty"`
	// LegDiagnostics explains how priced legs flowed through the
	// GEX-contribution funnel. It is especially useful when a forced
	// off-hours run prices legs but every row has missing/zero OI.
	LegDiagnostics *GammaLegDiagnostics `json:"leg_diagnostics,omitempty"`
	// Warnings is the daemon-internal list of non-fatal condition codes:
	// "no_crossing_in_window", "spxw_partial_oi", "throttled",
	// "all_iv_derived". Empty when the computation was clean. It is not
	// serialized; wire consumers read WarningDetails instead.
	// Runs whose leg coverage falls below the MinLegCoverageFraction
	// persist threshold are surfaced as Status="error" with no
	// Result, not as a warning — see gamma_zero_compute's coverage
	// gate (mirror of breadth's MinCoverageFraction=0.80 pattern).
	Warnings []string `json:"-"`
	// WarningDetails is the serialized warning surface: scoped,
	// user-facing explanations plus optional impact/action text.
	WarningDetails []GammaWarningDetail `json:"warning_details,omitempty"`
	// Summary is a compact interpretation of the result. It is designed
	// for CLI/MCP consumers that need to answer "what did the model
	// identify?" before drilling into profile arrays.
	Summary *GammaZeroSummary `json:"summary,omitempty"`

	// ZeroGamma0DTE / Profile0DTE / GammaSign0DTE / LegCount0DTE are the
	// same headline triple computed over legs with DTE == 0 only —
	// same-day expiries before their settlement cutoff. Captures the
	// short-fuse flow that Cboe's 2025 data shows is ~59% of SPX
	// volume, isolated from the longer-dated weeklies and monthlies.
	// Nil when no 0DTE legs fell in the bucket (e.g. mid-week after
	// Monday's daily settled, before the next daily lists) OR when
	// the sweep over those legs had no crossing inside the ±10% band.
	// GammaSign0DTE="no_data" plus a "0dte_no_legs" warning communicate
	// the empty-bucket case.
	ZeroGamma0DTE *float64            `json:"zero_gamma_0dte,omitempty"`
	Profile0DTE   []GammaProfilePoint `json:"profile_0dte,omitempty"`
	GammaSign0DTE string              `json:"gamma_sign_0dte,omitempty"`
	LegCount0DTE  int                 `json:"leg_count_0dte,omitempty"`

	// ZeroGamma1to7 / Profile1to7 / GammaSign1to7 / LegCount1to7 are the
	// matching triple for legs with 0 < DTE ≤ 7 days — overnight
	// through one calendar week. Captures end-of-week dynamics
	// (weeklies, EOW Friday flow) without commingling with the 0DTE
	// term that swamps the bucket on a third Friday.
	ZeroGamma1to7 *float64            `json:"zero_gamma_1to7,omitempty"`
	Profile1to7   []GammaProfilePoint `json:"profile_1to7,omitempty"`
	GammaSign1to7 string              `json:"gamma_sign_1to7,omitempty"`
	LegCount1to7  int                 `json:"leg_count_1to7,omitempty"`

	// ZeroGammaTerm / ProfileTerm / GammaSignTerm / LegCountTerm are the
	// matching triple for legs with DTE > 7 days — monthly OPEX and
	// quarterly horizons. Slower-moving than the two near buckets;
	// dominated by collar/structured-product positioning rather than
	// dealer-flow speed.
	ZeroGammaTerm *float64            `json:"zero_gamma_term,omitempty"`
	ProfileTerm   []GammaProfilePoint `json:"profile_term,omitempty"`
	GammaSignTerm string              `json:"gamma_sign_term,omitempty"`
	LegCountTerm  int                 `json:"leg_count_term,omitempty"`

	// MethodologyCitations is the short bibliography backing the
	// methodology disclosure. Each entry is a single line of the form
	// "Author (Year) — short claim". Surfaced on the result envelope so
	// renderers can show the citations alongside the headline numbers
	// without the user having to consult out-of-band documentation.
	MethodologyCitations []string `json:"methodology_citations,omitempty"`

	// SkewModel names the IV model used during the sweep. v2 cutover:
	// "sticky-moneyness-v1" means a quadratic skew curve in
	// log-moneyness was fitted per expiry and σ was looked up at each
	// (scenario spot, strike) pair. Empty when the curve fell back to
	// sticky-IV everywhere (degenerate fits across every expiry); a
	// per-expiry fallback shows up in warning_details.
	SkewModel string `json:"skew_model,omitempty"`
	// SkewFitQuality is one SkewFitInfo per expiry that fitted a curve
	// successfully. Keyed by compact YYYYMMDD. Renderers can show fit
	// quality alongside the headline so the reader can audit how
	// confident the underlying skew curve is.
	SkewFitQuality map[string]SkewFitInfo `json:"skew_fit_quality,omitempty"`

	// Params echoes the v1 calibration window so a renderer can show
	// "computed over 6 expirations within ATM ± 10%" without consulting
	// out-of-band documentation.
	Params GammaZeroParams `json:"params"`
	// Source identifies the data provenance for the headline numbers.
	Source string `json:"source"`
	// Method is a short stable token for the computation path. v3:
	// "bs-gamma-profile-v3-stickymoneyness-0dte-split". The v3 bump
	// signals two semantic changes from v2:
	//   - horizon split is now 0DTE / 1-7 / >7 (was ≤7 / >7), because
	//     Cboe 2025 data shows 0DTE = ~59% of SPX volume and lumping
	//     it with weeklies muddies the signal.
	//   - the per-leg snapshot gamma is BS-recomputed from captured IV
	//     rather than read from the gateway's optional Greeks tick;
	//     fixes a v2 race where IV-but-no-Greeks legs contributed 0 to
	//     GammaTotalAbs.
	// "perfiliev" is dropped from the token because Perfiliev's
	// published method used sticky-IV; the sticky-moneyness refinement
	// is citable to Derman / Daglish-Hull-Suo (see MethodologyCitations).
	// Full disclosure lives in docs/specs/risk-regime-dashboard.md so
	// renderers can deep-link.
	Method string `json:"method"`
	// AsOf is the daemon's wall-clock when the compute finished.
	AsOf time.Time `json:"as_of"`
	// DurationMS is honest about how long the compute took on the wall
	// clock; useful for tuning ExpiryCount / StrikeWidthPct.
	DurationMS int64 `json:"duration_ms"`

	// Scope is the discriminator for combined-vs-single-underlying
	// payloads:
	//   "spy"     — SPY-only; PerIndex is nil
	//   "spx"     — SPX-only (--only=spx); PerIndex is nil
	//   "spy+spx" — combined; price-level fields stay under PerIndex
	//               because there is no meaningful combined price scale.
	// Empty is treated as Scope="spy" by renderers.
	Scope string `json:"scope,omitempty"`

	// PerIndex carries the per-underlying detail when Scope="spy+spx".
	// Nil for single-underlying scopes. Keys are uppercased symbols
	// ("SPY", "SPX"). Each entry is a self-contained GammaZeroComputed
	// with its own Scope ("spy" or "spx"), so a renderer can recurse on
	// the per-index slice and reuse the single-underlying formatting.
	//
	// Pointer values rather than struct values so the field can be
	// nil-checked rather than length-tested in renderers, and so the
	// recursive type doesn't bloat the SPY-only path's payload.
	PerIndex map[string]*GammaZeroComputed `json:"per_index,omitempty"`

	// PartialClasses surfaces per-trading-class entitlement gaps when
	// one class of an underlying lands but the other 354s. Keyed by
	// the unreachable trading class (e.g. {"SPX": "354"} when SPX-class
	// AM-monthlies return "not subscribed" but SPXW-class weeklies
	// land). Empty when both classes land cleanly OR when neither
	// lands (the latter surfaces as Status="error" upstream).
	PartialClasses map[string]string `json:"partial_classes,omitempty"`

	// RegimeAgreement classifies whether the SPY and SPX dealer-gamma
	// regimes agree, populated only on Scope="spy+spx" runs. One of:
	//
	//   "agree:long-gamma"  — both indices' sweeps stay positive (dealer
	//                         long-γ across the ±15% window, stabilizing).
	//   "agree:short-gamma" — both stay negative (short-γ, amplifying).
	//   "agree:transition-gamma" — both are within ±2% of their γ-zero
	//                         crossings. The per-index ZeroGamma levels
	//                         carry the precise prices.
	//   "disagree"          — one index is long-γ, the other short-γ
	//                         (or transition while the other isn't).
	//                         The actionable signal: institutional SPX
	//                         book and retail/ETF SPY book are positioned
	//                         opposite, which the regime-call use case
	//                         cares about more than any combined number.
	//   ""                  — at least one bucket has no data; can't
	//                         classify. Renderers fall back to per-index.
	//
	// Replaces the earlier DecoupledCorr field, which gated on 20-day
	// price correlation. Price correlation stays > 0.99 essentially
	// always; that gate never fired and missed the actual case worth
	// flagging — gamma regimes that decouple while prices stay tightly
	// correlated.
	RegimeAgreement string `json:"regime_agreement,omitempty"`
}

// GammaZeroSPXResult is the envelope returned by MethodGammaZeroSPX.
// Always carries a Status; Result is populated when Status is "ready".
// The split (envelope vs computed payload) keeps the wire stable while
// the compute pipeline can evolve — adding fields to GammaZeroComputed
// doesn't churn the polling contract.
type GammaZeroSPXResult struct {
	// Status is one of GammaZeroStatusComputing / Ready / Error.
	Status string `json:"status"`
	// StartedAt is when the currently-relevant compute kicked off — for
	// "computing", that's the in-flight job; for "ready", it's the
	// compute that produced Result. Nil if no compute has ever started.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// EtaSeconds is an initial estimate of the total wall-clock the
	// compute will need from kickoff. Used by renderers to show a
	// progress meter or set a polling cadence. 0 when Status != computing.
	EtaSeconds int `json:"eta_seconds,omitempty"`
	// Progress is a 0-100 hint, best-effort. 0 when Status != computing.
	Progress int `json:"progress,omitempty"`
	// Result is populated when Status == "ready".
	Result *GammaZeroComputed `json:"result,omitempty"`
	// Error is populated when Status == "error".
	Error string `json:"error,omitempty"`
	// ColdReasonCode / ColdReason / ColdAction are populated when
	// Status == "cold" and the daemon knows why no result can be
	// served. This distinguishes a true first-run cold cache from a
	// persisted snapshot that existed but was rejected by schema,
	// methodology, or data-quality gates.
	ColdReasonCode string `json:"cold_reason_code,omitempty"`
	ColdReason     string `json:"cold_reason,omitempty"`
	ColdAction     string `json:"cold_action,omitempty"`
	// RetryOfErrorAt + RetryOfErrorSummary are non-nil/non-empty only
	// when Status == "computing" AND the in-flight compute was kicked
	// because the previous attempt failed past gammaErrorRetryTTL. The
	// renderer surfaces them as "computing · retry of <summary> at
	// HH:MM:SS" so the user sees the prior failure context — without
	// this, the dashboard silently switched from "error" to "computing"
	// and the user had to grep the daemon log to understand why.
	RetryOfErrorAt      *time.Time `json:"retry_of_error_at,omitempty"`
	RetryOfErrorSummary string     `json:"retry_of_error_summary,omitempty"`
}

// StripGammaProfiles removes chart-sized sweep arrays from a gamma result
// while preserving headline levels, summaries, warning details, counts, and
// top strikes. CLI JSON and MCP use this by default so agents do not receive
// tens of kilobytes of points unless they explicitly ask for profiles.
func StripGammaProfiles(r *GammaZeroSPXResult) {
	if r == nil || r.Result == nil {
		return
	}
	stripGammaComputedProfiles(r.Result)
}

func StripRegimeGammaProfiles(r *RegimeSnapshotResult) {
	if r == nil {
		return
	}
	StripGammaProfiles(&r.GammaZero.Envelope)
}

// CompactRegimeSnapshot removes methodology prose and chart/history payloads
// from a regime response while preserving the current measurements, summary,
// composite counts, streaks, quality provenance, scoped warnings, and gamma
// headline diagnostics. CLI --json and MCP use this default shape so agent
// consumers get the decision surface without multi-kilobyte notes blocks.
func CompactRegimeSnapshot(r *RegimeSnapshotResult) {
	if r == nil {
		return
	}
	StripRegimeGammaProfiles(r)
	r.VIXTermStructure.Notes = ""
	r.VolOfVol.Notes = ""
	r.HYGSPYDivergence.Notes = ""
	r.CreditSpreads.Notes = ""
	r.FundingStress.Notes = ""
	r.USDJPY.Notes = ""
	r.GammaZero.Notes = ""
	r.Breadth.Notes = ""
	r.Breadth.Envelope.History = nil
}

func stripGammaComputedProfiles(c *GammaZeroComputed) {
	if c == nil {
		return
	}
	c.Profile = nil
	c.Profile0DTE = nil
	c.Profile1to7 = nil
	c.ProfileTerm = nil
	for _, sub := range c.PerIndex {
		stripGammaComputedProfiles(sub)
	}
}

// RegimeIndicatorStatus is the high-level availability/freshness state
// for one row of the regime snapshot. Renderers branch on it; the
// daemon never derives green/yellow/red status from raw values (the
// spec calls those thresholds user-tunable). Specific values:
//
//   - "ok"          — the indicator carries a real, fresh measurement
//   - "stale"       — measurement returned but the gateway labeled it
//     delayed/frozen; renderer should dim
//   - "computing"   — heavy compute is in-flight (gamma's first call
//     of the day); poll again to see the result
//   - "unavailable" — IBKR doesn't carry the feed on this account; the
//     `notes` field explains why and what to do
//   - "error"       — fetch failed; `error_message` carries the reason
const (
	RegimeStatusOK          = "ok"
	RegimeStatusStale       = "stale"
	RegimeStatusComputing   = "computing"
	RegimeStatusUnavailable = "unavailable"
	RegimeStatusError       = "error"
)

// Quality is the provenance + freshness envelope for one scalar regime
// field. Attached as a sibling pointer to each value pointer; nil
// Quality means "no provenance recorded" (legacy/migration only). The
// envelope is purely additive to the row-level Status/DataType —
// renderers prefer Quality when present.
//
// FreshnessClass values:
//   - "live"     — gateway live tick observed at AsOf
//   - "frozen"   — gateway frozen/delayed tick (typically last
//     regular-session close) at AsOf
//   - "derived"  — computed from historical bars (e.g.
//     max(High) over 252 daily bars)
//   - "modelled" — computed from a model with documented caveats (the
//     gamma sweep's perfiliev-bs-sweep-v1)
//
// Confidence values:
//   - "firm"     — direct gateway measurement
//   - "estimate" — derived from historical bars
//   - "proxy"    — modelled with methodology disclosure (caller's
//     Method/warning_details document the assumptions)
type Quality struct {
	AsOf           time.Time `json:"as_of"`
	FreshnessClass string    `json:"freshness_class"`
	Confidence     string    `json:"confidence"`
	// Source is a one-line human-readable provenance description, e.g.
	// "VIX tick", "SPY 252d max(High) fallback", "perfiliev-bs-sweep-v1".
	// Surfaced by --explain; renderers can ignore it for the compact
	// in-row annotation.
	Source string `json:"source,omitempty"`
}

const (
	FreshnessLive     = "live"
	FreshnessFrozen   = "frozen"
	FreshnessDerived  = "derived"
	FreshnessModelled = "modelled"

	ConfidenceFirm     = "firm"
	ConfidenceEstimate = "estimate"
	ConfidenceProxy    = "proxy"
)

// StreakInfo tells a consumer how many consecutive trading sessions
// an indicator has been in its current band. Closes the wire-shape
// gap with the spec's repeated "sustained 2-3 days, not single
// spikes" language — a single snapshot can't distinguish day 1 of a
// stress regime from day 5, but the streak counter makes that
// difference visible inline ("yellow · day 3").
//
// Band classification IS done daemon-side for streak purposes (a small
// violation of the "daemon doesn't derive bands" principle the spec
// states for the wire surface — but necessary for streak persistence).
// The bands used here mirror the spec's default cutoffs verbatim; a
// renderer that wants to apply a different threshold can still ignore
// Band and compute its own coloring from the row's raw measurement.
//
// Since is the YYYY-MM-DD NY-tz session key for when the current
// streak began. Sessions ≥ 1; the first session in a band is day 1.
// Indicator unavailable/computing/error states freeze the counter
// rather than reset it — a stale data point shouldn't end a streak.
type StreakInfo struct {
	Band     string `json:"band"`
	Sessions int    `json:"sessions"`
	Since    string `json:"since"`
}

// RegimeIndicatorMeta is the compact interpretation/provenance layer shared
// by every regime row. The fields are embedded into each indicator's JSON so
// agents do not have to derive bands, thresholds, or freshness from prose.
type RegimeIndicatorMeta struct {
	Band       string             `json:"band,omitempty"`
	BandReason string             `json:"band_reason,omitempty"`
	Thresholds *RegimeThresholds  `json:"thresholds,omitempty"`
	AsOf       *RegimeAsOfSummary `json:"as_of,omitempty"`
}

// RegimeThresholds names the heuristic threshold set used to classify an
// indicator. The string bands are intentionally compact and heterogeneous:
// each row has different units, so a label plus per-band text is friendlier
// than forcing everything into one numeric schema.
type RegimeThresholds struct {
	Label           string `json:"label,omitempty"`
	Green           string `json:"green,omitempty"`
	Yellow          string `json:"yellow,omitempty"`
	Red             string `json:"red,omitempty"`
	Heuristic       bool   `json:"heuristic,omitempty"`
	PendingBacktest bool   `json:"pending_backtest,omitempty"`
}

// RegimeAsOfSummary is the row-level freshness badge rendered in the CLI and
// exposed in JSON/MCP. Label is the user-facing compact form ("live",
// "15m delayed", "close D-1", "cached 11:42", "2d old", "unavailable").
// Time is present when a real timestamp exists; Date is present for official
// daily files whose observation date is more meaningful than midnight UTC.
type RegimeAsOfSummary struct {
	Label      string    `json:"label"`
	Time       time.Time `json:"time,omitzero"`
	Date       string    `json:"date,omitempty"`
	Freshness  string    `json:"freshness,omitempty"`
	Source     string    `json:"source,omitempty"`
	AgeSeconds int64     `json:"age_seconds,omitempty"`
}

// RegimeVIXTerm is Indicator 1: VIX/VIX3M ratio. Watch for sustained
// inversion (ratio > 1.0) over 2-3 sessions, not a single spike.
//
// FieldsMissing is an advisory list of pointer-typed fields above
// (e.g. "vix3m", "ratio") that did NOT land within the fetch budget
// even though the row's primary measurement succeeded. Absent when
// nothing is missing. Use it to dim a sub-cell without re-classifying
// the whole row as `error`.
type RegimeVIXTerm struct {
	RegimeIndicatorMeta
	Status        string   `json:"status"`
	VIX           *float64 `json:"vix"`
	VIX3M         *float64 `json:"vix3m"`
	Ratio         *float64 `json:"ratio"` // VIX / VIX3M
	DataType      string   `json:"data_type,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	ErrorMessage  string   `json:"error_message,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
	// VIX previous regular-session close (tick 9) and the day's percent
	// change. Populated when the same subscribe that delivered VIX also
	// surfaced tick 9 — pre-open this is typically the only useful daily
	// anchor since VIX itself doesn't trade. Both fields nil when the
	// gateway delivered no close tick within the budget.
	VIXPrevClose *float64 `json:"vix_prev_close,omitempty"`
	VIXChangePct *float64 `json:"vix_change_pct,omitempty"` // (vix − prev_close) / prev_close × 100
	// Per-scalar provenance. Each *Quality is nil when the corresponding
	// value pointer is nil; otherwise the daemon populates it at the
	// fetch site so renderers can show "firm live", "frozen", or
	// "estimate · 18s" without re-deriving from DataType.
	VIXQuality   *Quality `json:"vix_quality,omitempty"`
	VIX3MQuality *Quality `json:"vix3m_quality,omitempty"`
	// Streak counts how many consecutive sessions this row's value has
	// been in its current band. Persisted across daemon restarts in
	// $XDG_CACHE_HOME/ibkr/regime-streaks.json. Nil when the band can't
	// be determined (computing / unavailable / error) — the streak
	// freezes rather than resets.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeHYGSPYDivergence is Indicator 2: HYG vs SPY context. The
// daemon surfaces raw measurements; the consumer compares HYG's
// current to its 50-day SMA and SPY's current to its 52-week high.
//
// FieldsMissing carries optional sub-fields ("spy_52w_high",
// "hyg_50dma") that didn't land — both are best-effort and don't
// downgrade the row's primary status.
type RegimeHYGSPYDivergence struct {
	RegimeIndicatorMeta
	Status     string   `json:"status"`
	HYGPrice   *float64 `json:"hyg_price"`
	HYG50DMA   *float64 `json:"hyg_50dma"` // 50-day SMA of HYG daily close
	SPYPrice   *float64 `json:"spy_price"`
	SPY52WHigh *float64 `json:"spy_52w_high"`
	// SPY previous regular-session close (tick 9) plus the day's dollar
	// and percent change. Populated when the SPY subscribe also surfaced
	// tick 9 (the gateway emits it automatically alongside the price
	// triple). All three nil when no close tick landed in the budget.
	SPYPrevClose  *float64 `json:"spy_prev_close,omitempty"`
	SPYChange     *float64 `json:"spy_change,omitempty"`     // last − prev_close (dollars)
	SPYChangePct  *float64 `json:"spy_change_pct,omitempty"` // (last − prev_close) / prev_close × 100
	HYGDataType   string   `json:"hyg_data_type,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	ErrorMessage  string   `json:"error_message,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
	// Per-scalar provenance. SPY52WHigh has two paths (live tick 165 vs
	// history fallback); the Quality field is what the renderer reads to
	// distinguish firm-tick from derived-fallback.
	HYGQuality        *Quality `json:"hyg_quality,omitempty"`
	HYG50DMAQuality   *Quality `json:"hyg_50dma_quality,omitempty"`
	SPYQuality        *Quality `json:"spy_quality,omitempty"`
	SPY52WHighQuality *Quality `json:"spy_52w_high_quality,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	// RegimeVIXTerm.Streak for the semantics.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeVolOfVol is the VVIX vol-of-vol row. It uses Cboe's official
// daily VVIX time series rather than a retail-gateway quote, because
// VVIX is itself an index calculation and end-of-day source quality is
// better than pretending there is a continuously tradable instrument.
type RegimeVolOfVol struct {
	RegimeIndicatorMeta
	Status       string      `json:"status"`
	Symbol       string      `json:"symbol,omitempty"` // "VVIX"
	Last         *float64    `json:"last"`
	Change20D    *float64    `json:"change_20d_pct,omitempty"` // (last − t-20) / t-20 × 100
	AsOfDate     string      `json:"as_of_date,omitempty"`     // YYYY-MM-DD observation date
	Source       string      `json:"source,omitempty"`
	Notes        string      `json:"notes,omitempty"`
	ErrorMessage string      `json:"error_message,omitempty"`
	ValueQuality *Quality    `json:"value_quality,omitempty"`
	Streak       *StreakInfo `json:"streak,omitempty"`
}

// RegimeCreditSpreads is the official cash-credit companion to the HYG
// ETF proxy. Values are ICE BofA option-adjusted spread series retrieved
// via FRED/St. Louis Fed. Units are percentage points; e.g. 4.25 means
// 425 bp.
type RegimeCreditSpreads struct {
	RegimeIndicatorMeta
	Status        string      `json:"status"`
	HYOAS         *float64    `json:"hy_oas"`
	IGOAS         *float64    `json:"ig_oas"`
	HYIGSpread    *float64    `json:"hy_ig_spread,omitempty"`
	HY20DChange   *float64    `json:"hy_oas_20d_change,omitempty"` // percentage points
	AsOfDate      string      `json:"as_of_date,omitempty"`
	Source        string      `json:"source,omitempty"`
	Notes         string      `json:"notes,omitempty"`
	ErrorMessage  string      `json:"error_message,omitempty"`
	FieldsMissing []string    `json:"fields_missing,omitempty"`
	HYOASQuality  *Quality    `json:"hy_oas_quality,omitempty"`
	IGOASQuality  *Quality    `json:"ig_oas_quality,omitempty"`
	SpreadQuality *Quality    `json:"spread_quality,omitempty"`
	Streak        *StreakInfo `json:"streak,omitempty"`
}

// RegimeFundingStress is the OFR-style U.S. funding spread row:
// 90-day AA financial commercial paper rate minus 3-month Treasury bill
// rate, both from official Federal Reserve/FRED series. Units are basis
// points.
type RegimeFundingStress struct {
	RegimeIndicatorMeta
	Status         string      `json:"status"`
	CP3M           *float64    `json:"cp_3m_rate"`
	TBill3M        *float64    `json:"tbill_3m_rate"`
	SpreadBps      *float64    `json:"spread_bps"`
	AsOfDate       string      `json:"as_of_date,omitempty"`
	Source         string      `json:"source,omitempty"`
	Notes          string      `json:"notes,omitempty"`
	ErrorMessage   string      `json:"error_message,omitempty"`
	FieldsMissing  []string    `json:"fields_missing,omitempty"`
	CP3MQuality    *Quality    `json:"cp_3m_quality,omitempty"`
	TBill3MQuality *Quality    `json:"tbill_3m_quality,omitempty"`
	SpreadQuality  *Quality    `json:"spread_quality,omitempty"`
	Streak         *StreakInfo `json:"streak,omitempty"`
}

// RegimeUSDJPY is the FX-carry stress row: USD/JPY exchange rate. Spec measures
// "weekly move" — daemon surfaces last and 7-trading-days-ago close so
// the consumer can compute the change. Source is FX-pair routing
// (CASH/IDEALPRO); routing arrives in a sibling commit.
type RegimeUSDJPY struct {
	RegimeIndicatorMeta
	Status        string   `json:"status"`
	Symbol        string   `json:"symbol"` // "USD.JPY" canonical form
	Last          *float64 `json:"last"`
	Close7DAgo    *float64 `json:"close_7d_ago"`      // close from 7 trading days ago
	WeeklyChange  *float64 `json:"weekly_change_pct"` // (last − close_7d_ago) / close_7d_ago × 100
	DataType      string   `json:"data_type,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	ErrorMessage  string   `json:"error_message,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
	// Per-scalar provenance. Last is firm-live (or firm-frozen);
	// Close7DAgo is always estimate-derived (MIDPOINT historical bar).
	LastQuality       *Quality `json:"last_quality,omitempty"`
	Close7DAgoQuality *Quality `json:"close_7d_ago_quality,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	// RegimeVIXTerm.Streak for the semantics.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeGammaZero is the existing gamma.zero_spx
// envelope embedded inline. Auto-kicked by regime.snapshot on first
// call of an NY trading day; subsequent calls return the cached
// result. Method token + warning_details carry methodology disclosures.
type RegimeGammaZero struct {
	RegimeIndicatorMeta
	Status        string             `json:"status"`
	Envelope      GammaZeroSPXResult `json:"envelope"`
	Notes         string             `json:"notes,omitempty"`
	FieldsMissing []string           `json:"fields_missing,omitempty"`
	// Per-scalar provenance for the two values the renderer prints:
	// ZeroGamma is proxy-modelled (carries the perfiliev-bs-sweep
	// methodology); GammaTotalAbs is estimate-derived (sign-agnostic
	// notional summed over observed OI+IV).
	ZeroGammaQuality     *Quality `json:"zero_gamma_quality,omitempty"`
	GammaTotalAbsQuality *Quality `json:"gamma_total_abs_quality,omitempty"`
	// HorizonAgreement names how a single-underlying envelope's three
	// horizon-bucketed γ-zero readings (0DTE, 1-7, term) relate. Empty
	// for combined SPY+SPX results, where horizon buckets live under
	// per_index.SPY / per_index.SPX. One of:
	//
	//   - "all_long"             every usable bucket is long-γ
	//   - "all_short"            every usable bucket is short-γ
	//   - "all_transition"       every usable bucket is within ±2% of
	//                            its γ-zero
	//   - "diverge:0dte_vs_term"  0DTE and term buckets disagree
	//                            (highest-information case — short-fuse
	//                            flow disagrees with monthly positioning)
	//   - "diverge:partial"       other mixed cases (1-7 alone disagrees,
	//                            only two usable buckets disagree, etc.)
	//   - "0dte_only" / "1to7_only" / "term_only" — only one bucket is
	//                            usable
	//   - ""                     no bucket has a usable signal
	//
	// The renderer annotates the row whenever the value starts with
	// "diverge:" or ends in "_only" — those are the cases where the
	// combined headline doesn't tell the full story.
	HorizonAgreement string `json:"horizon_agreement,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	// RegimeVIXTerm.Streak for the semantics.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeBreadth is Indicator 5: the existing breadth.spx envelope
// embedded inline. The daemon's local 50-DMA engine computes S5FI from
// constituent daily closes (see BreadthSPXResult.Method). On the first
// call against a fresh daemon the cold-start fan-out runs (~60 min,
// IBKR-paced), so this row typically surfaces Status="computing" with
// a notes pointer; subsequent calls return Status="ok" from the
// persisted cache.
type RegimeBreadth struct {
	RegimeIndicatorMeta
	Status        string           `json:"status"`
	Envelope      BreadthSPXResult `json:"envelope"`
	Notes         string           `json:"notes,omitempty"`
	FieldsMissing []string         `json:"fields_missing,omitempty"`
	// PctAbove50DMA / PctAbove200DMA / NewHighsToday / NewLowsToday /
	// NetNewHighsPct are surfaced directly on the regime row so a
	// consumer doesn't have to dig into Envelope for the four-number
	// breadth view that informs the band. Echoed from Envelope; same
	// values, same units.
	PctAbove50DMA  float64 `json:"pct_above_50dma,omitempty"`
	PctAbove200DMA float64 `json:"pct_above_200dma,omitempty"`
	NewHighsToday  int     `json:"new_highs_today,omitempty"`
	NewLowsToday   int     `json:"new_lows_today,omitempty"`
	NetNewHighsPct float64 `json:"net_new_highs_pct,omitempty"`
	// Per-scalar provenance for the breadth percentage. firm-live or
	// firm-frozen when ranked, depending on the envelope's DataType;
	// nil during cold start or when the engine refused to persist
	// because constituent coverage fell below the safety threshold.
	ValueQuality *Quality `json:"value_quality,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	// RegimeVIXTerm.Streak for the semantics.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeSnapshotParams is the input for MethodRegimeSnapshot. Empty
// body means "fetch all regime indicators with default parameters." A
// future caller could trim to a subset, but v1 always returns all
// rows — partial responses are surfaced via per-indicator Status.
type RegimeSnapshotParams struct{}

// RegimeSnapshotResult is the wire payload for the dashboard
// generator and the MCP natural-language interface. One JSON
// envelope, all rows. Each row carries:
//   - raw measurements plus compact band/as-of metadata for agents
//   - a `notes` field embedding the full methodology prose for
//     explain-mode consumers
//   - a structured Status the renderer branches on for UI state
//
// Compatibility note for renderers: the daemon never returns nil for
// any indicator field — empty / unavailable indicators surface
// Status="unavailable" with populated Notes. Numerical fields are
// pointers so "not arrived yet" vs "exactly zero" stays
// distinguishable.
type RegimeSnapshotResult struct {
	AsOf             time.Time              `json:"as_of"`
	Summary          RegimeSummary          `json:"summary"`
	VIXTermStructure RegimeVIXTerm          `json:"vix_term_structure"`
	VolOfVol         RegimeVolOfVol         `json:"vol_of_vol"`
	HYGSPYDivergence RegimeHYGSPYDivergence `json:"hyg_spy_divergence"`
	CreditSpreads    RegimeCreditSpreads    `json:"credit_spreads"`
	FundingStress    RegimeFundingStress    `json:"funding_stress"`
	USDJPY           RegimeUSDJPY           `json:"usd_jpy"`
	GammaZero        RegimeGammaZero        `json:"gamma_zero"`
	Breadth          RegimeBreadth          `json:"breadth"`
	// Composite carries the daemon-side rollup the CLI shows above the
	// indicator rows (verdict + ranked/unranked counts), so MCP consumers
	// don't have to recompute it from per-row Status fields. Populated on
	// every response.
	Composite RegimeComposite `json:"composite"`
	// WarningDetails carries structured, row-scoped data-quality issues
	// that affected this snapshot but did not make the whole RPC fail.
	// Agent surfaces should prefer these over parsing ErrorMessage strings:
	// each warning states what happened, what it changes in the composite,
	// and the next useful action.
	WarningDetails []RegimeWarning `json:"warning_details,omitempty"`
	// SpecDoc points consumers (especially LLM-driven ones) at the
	// canonical methodology + threshold reference so they don't
	// hallucinate band edges. Same path on every response.
	SpecDoc string `json:"spec_doc"`
}

// RegimeSummary is the compact, agent-first reading of a regime snapshot.
// It deliberately avoids probabilities or trade instructions: the fields
// describe the evidence balance, coverage, and current condition only.
type RegimeSummary struct {
	Label             string   `json:"label"`
	Evidence          string   `json:"evidence"` // cluster-level balance
	IndicatorEvidence string   `json:"indicator_evidence,omitempty"`
	PunchLine         string   `json:"punch_line"`
	Confidence        string   `json:"confidence"`
	DominantRisks     []string `json:"dominant_risks,omitempty"`
	NotAdvice         string   `json:"not_advice,omitempty"`
}

// RegimeWarning is a structured data-quality or availability issue scoped
// to one regime indicator. Severity is "info", "warning", or "error" from
// the point of view of interpreting the snapshot, not the RPC transport.
type RegimeWarning struct {
	Code     string `json:"code"`
	Scope    string `json:"scope"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Impact   string `json:"impact"`
	Action   string `json:"action"`
}

// RegimeComposite is the daemon-side rollup of the regime rows.
// Verdict mirrors the CLI's text rendering verbatim ("Normal regime",
// "Stress signal present", etc.) so consumers can show the same
// non-advisory headline without re-implementing the band logic. The raw
// row counts are exposed alongside cluster counts so related signals
// (e.g. VIX term structure + VVIX, HYG proxy + cash credit spreads) do
// not double-count as independent macro confirmations.
//
// RankedCount + UnrankedCount sum to the indicator count; cluster counts
// sum to the cluster count. Verdict is based on clusters, not raw rows.
type RegimeComposite struct {
	Verdict              string `json:"verdict"`
	GreenCount           int    `json:"green_count"`
	YellowCount          int    `json:"yellow_count"`
	RedCount             int    `json:"red_count"`
	RankedCount          int    `json:"ranked_count"`
	UnrankedCount        int    `json:"unranked_count"`
	ClusterGreenCount    int    `json:"cluster_green_count"`
	ClusterYellowCount   int    `json:"cluster_yellow_count"`
	ClusterRedCount      int    `json:"cluster_red_count"`
	ClusterRankedCount   int    `json:"cluster_ranked_count"`
	ClusterUnrankedCount int    `json:"cluster_unranked_count"`
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
//
// Unit conventions:
//   - ChangePct is in PERCENT units (0.70 means 0.70 %, not 70 %). The
//     CLI renderer appends a trailing % without multiplying.
//   - IV is a DECIMAL FRACTION (0.247 means 24.7 %). The CLI renderer
//     multiplies by 100 before printing. Same convention across every
//     IV-bearing field in this package (chain expiries, chain strikes,
//     scan rows, position rows).
type Quote struct {
	Symbol    string         `json:"symbol"`
	Contract  ContractParams `json:"contract"`
	Bid       *float64       `json:"bid"`
	Ask       *float64       `json:"ask"`
	Last      *float64       `json:"last"`
	Mark      *float64       `json:"mark,omitempty"`
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
	Symbol   string  `json:"symbol"`
	SecType  string  `json:"sec_type"`
	Exchange string  `json:"exchange,omitempty"`
	Currency string  `json:"currency,omitempty"`
	Quantity float64 `json:"quantity"`
	// Multiplier is the contract multiplier — 1 for stocks, 100 for standard
	// equity options, sometimes higher for index options. Needed by JSON
	// consumers to convert between per-share Mark and per-contract AvgCost
	// on options (IBKR's averageCost is multiplier-inclusive on OPT).
	Multiplier int      `json:"multiplier"`
	AvgCost    float64  `json:"avg_cost"`
	Mark       float64  `json:"mark"`
	PrevClose  *float64 `json:"prev_close,omitempty"`
	// DayChange is per-share for stocks (Mark − stock prev close); for
	// options it stays nil because we don't track contract-level prev
	// close on the underlying-grouped path. DayChangePct is the same
	// ratio expressed as a percent. DayChangeMoney is the *position*-level
	// dollar impact: quantity × DayChange for stocks; quantity × multiplier
	// × (Mark − OptionPrevClose) for options when OptionPrevClose is
	// populated. nil when any input is missing — never fabricated.
	DayChange      *float64 `json:"day_change,omitempty"`
	DayChangePct   *float64 `json:"day_change_pct,omitempty"`
	DayChangeMoney *float64 `json:"day_change_money,omitempty"`
	MarketValue    float64  `json:"market_value"`
	MarketValueCcy *float64 `json:"market_value_ccy,omitempty"`
	FXRate         *float64 `json:"fx_rate,omitempty"`
	UnrealizedPnL  float64  `json:"unrealized_pnl"`
	RealizedPnL    float64  `json:"realized_pnl"`

	// DailyPnL is the start-of-trading-day to now P&L for this single
	// contract, sourced from IBKR's reqPnLSingle stream (TWS msg 95).
	// Distinct from UnrealizedPnL above, which is session-running.
	// nil means "no subscription yet" (daemon hasn't pre-warmed this
	// conId), "no frame received yet", "no entitlement", or "DBL_MAX
	// sentinel". Never zero-substituted. For options, the daily figure
	// can swing dramatically on small underlying moves; consumers
	// rendering a per-leg value should pair it with the option's
	// effective delta to interpret responsibly.
	DailyPnL *float64 `json:"daily_pnl,omitempty"`

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

	// Underlying is the model-computation underlying spot IBKR sent alongside
	// the Greeks (msg 21 tickType 13). The portfolio aggregator pairs delta
	// with this spot to compute dollar delta, so the dollar figure stays
	// consistent with the delta it was modelled against — pairing today's
	// delta with the underlying's prior close gives an apples-and-oranges
	// answer that lies by the size of any overnight gap. nil when the per-
	// leg Greeks tick didn't carry a spot (illiquid leg, model abstention)
	// — never zero-substituted.
	Underlying *float64 `json:"underlying,omitempty"`
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
	// DataType reflects the per-position mark-price feed when the daemon
	// can summarise it; left empty (omitted) when positions arrive purely
	// from the portfolio update stream without per-symbol feed state.
	DataType     string              `json:"data_type,omitempty"`
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
// from time decay assuming everything else holds. The value is in
// DailyThetaCurrency, computed with the same single-ccy-or-"MIX"
// convention as DollarDeltaCurrency: a single ISO code when every
// contributing option leg agrees, "MIX" when not. Renderers should
// surface "MIX" rather than picking a symbol — the sum is genuinely
// undefined in mixed-currency books.
//
// GreeksCoverage is the count of option legs whose Greeks were captured
// over the total — useful for the renderer to flag partial coverage
// when the model tick didn't arrive for some legs.
type PositionsPortfolio struct {
	EffectiveDelta      *float64 `json:"effective_delta,omitempty"`
	DollarDelta         *float64 `json:"dollar_delta,omitempty"`
	DollarDeltaCurrency string   `json:"dollar_delta_currency,omitempty"`
	DailyTheta          *float64 `json:"daily_theta,omitempty"`
	DailyThetaCurrency  string   `json:"daily_theta_currency,omitempty"`
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
//
// UnrealizedPnL / RealizedPnL are the gateway-reported base-currency
// session totals. Cushion is ExcessLiquidity / NetLiquidation as
// reported by the gateway (not derived locally) — a ratio, unitless.
// AccountType is one of IBKR's account-type strings ("INDIVIDUAL",
// "IB-MARGIN", "REG-T-MARGIN", "PORTFOLIO", "CASH", …); empty when the
// gateway didn't deliver it (older server versions or non-margin accounts).
// LookAhead* fields project the post-overnight-margin-cycle state — useful
// to spot "fine now, blown by tonight" cases on portfolio-margin books.
// All scalar fields are zero (not nil) on absence — the renderer treats
// zero as "show em-dash" for non-money fields like Cushion to avoid
// fabricating signal.
//
// DailyPnL / DailyPnLUnrealized / DailyPnLRealized are populated from
// the gateway's reqPnL stream (TWS msg 94): start-of-trading-day to now.
// Distinct from the session-running UnrealizedPnL / RealizedPnL above.
// All three are *float64 — nil means "no data yet" (pre-handshake,
// before the first stream frame), "no entitlement" (the gateway doesn't
// emit PnL for unentitled accounts), or "DBL_MAX sentinel" (gateway
// hasn't computed the slice). Never zero-substituted. DailyPnLUnrealized
// / DailyPnLRealized stay nil on older server versions that emit only
// the bare dailyPnL field.
type AccountResult struct {
	AccountID            string             `json:"account_id"`
	AccountType          string             `json:"account_type,omitempty"`
	BaseCurrency         string             `json:"base_currency"`
	NetLiquidation       float64            `json:"net_liquidation"`
	BuyingPower          float64            `json:"buying_power"`
	AvailableFunds       float64            `json:"available_funds"`
	ExcessLiquidity      float64            `json:"excess_liquidity"`
	TotalCash            float64            `json:"total_cash"`
	MaintenanceMargin    float64            `json:"maintenance_margin"`
	InitialMargin        float64            `json:"initial_margin"`
	GrossPositionValue   float64            `json:"gross_position_value"`
	UnrealizedPnL        float64            `json:"unrealized_pnl"`
	RealizedPnL          float64            `json:"realized_pnl"`
	Cushion              float64            `json:"cushion"`
	LookAheadInitMargin  float64            `json:"look_ahead_init_margin"`
	LookAheadMaintMargin float64            `json:"look_ahead_maint_margin"`
	LookAheadAvailable   float64            `json:"look_ahead_available_funds"`
	LookAheadExcess      float64            `json:"look_ahead_excess_liquidity"`
	DailyPnL             *float64           `json:"daily_pnl,omitempty"`
	DailyPnLUnrealized   *float64           `json:"daily_pnl_unrealized,omitempty"`
	DailyPnLRealized     *float64           `json:"daily_pnl_realized,omitempty"`
	CurrencyExposure     []CurrencyExposure `json:"currency_exposure,omitempty"`
	// DataType is reserved for account-feed state; the account-summary
	// path is gateway-direct with no live/delayed dimension and the field
	// is currently left empty (omitted). Kept for shape parity with the
	// market-data surfaces.
	DataType string    `json:"data_type,omitempty"`
	AsOf     time.Time `json:"as_of"`
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
//
// Currency is the ISO-4217 code for Last / PrevClose / Change / Week52*
// — needed so non-US ad-hoc scans (e.g. --exchange STK.EU.IBIS) render
// with the right symbol instead of a hardcoded $. Empty string means
// "the daemon couldn't resolve a currency for this row"; renderers
// should fall back to $ in that case for back-compat with old daemons.
//
// Unit conventions follow Quote: ChangePct is in PERCENT units (5.41
// means 5.41 %), IV is a DECIMAL FRACTION (0.342 means 34.2 %).
type ScanRow struct {
	Rank       int      `json:"rank"`
	Symbol     string   `json:"symbol"`
	Currency   string   `json:"currency,omitempty"`
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

// BackgroundTaskStatus names a daemon-internal long-running task that
// is currently executing. Used by `ibkr status` to surface activity
// that would otherwise be invisible — a fresh autospawned daemon
// mid-bootstrap looks identical to an idle one from outside. The
// surface deliberately carries no state enum: presence in the
// HealthResult.BackgroundTasks list IS the state ("this task is
// running right now"). Tasks that are idle/ready/cold are omitted
// entirely, keeping the wire payload bounded and the user-facing
// rendering compact.
//
// Current task names:
//   - "breadth-spx" — the SPX 50-DMA breadth engine is running a
//     refresh (cold-start bootstrap or daily post-close refresh).
//   - "gamma-zero" — the SPX zero-gamma compute is fanning out
//     across option legs.
//
// Renderers should treat unknown names as informational rather than
// errors; new background tasks added in future versions are
// forward-compatible by design.
type BackgroundTaskStatus struct {
	// Name is a stable token identifying the task. Stable across
	// daemon versions; one of the documented values above.
	Name string `json:"name"`
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
	// BackgroundTasks lists daemon-internal long-running computes that
	// are running RIGHT NOW. Empty when nothing's running. Always
	// present on the wire (never omitted) so consumers can rely on
	// `len(result.background_tasks) == 0` to mean "idle" without
	// inferring from absence.
	BackgroundTasks []BackgroundTaskStatus `json:"background_tasks"`
	// Members carries the runtime SPX-membership state: source
	// (cache vs embedded), count, as-of timestamp, refresh health.
	// Populated unconditionally — even when the daemon falls back
	// to the embedded list, the user needs to see WHICH list it's
	// using so silent parser rot / disabled-refresh shows up in
	// `ibkr status`. Zero-value Source means the daemon doesn't
	// know yet (engine construction failed); the CLI hides the row
	// in that case.
	Members MembersHealth `json:"members"`
}

// MembersHealth is the wire shape for the SPX-members surface
// rendered in `ibkr status`. Distinct from BreadthSPXResult: that
// carries the COMPUTED breadth value; this carries metadata about
// the constituent LIST.
//
// Source is "cache" when the daemon loaded the runtime-refreshed
// file, "embedded" when it fell back to the binary's compiled-in
// baseline. AsOf is the date the active list was generated.
// RefreshState is one of the spx.RefreshState constants ("healthy",
// "network_failed", "parse_failed", "disabled (config)", "disabled
// (env)"). Healthy is the steady-state; renderer omits the
// `refresh:` segment when healthy.
type MembersHealth struct {
	Source       string    `json:"source"`
	AsOf         time.Time `json:"as_of"`
	Count        int       `json:"count"`
	RefreshState string    `json:"refresh_state"`
}
