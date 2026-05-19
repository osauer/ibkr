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
	// TimeoutMs bounds the wait for the first valid S5FI tick. Default
	// 5000 ms when zero. The default is generous because the INDEX
	// exchange feed can be slow to deliver the first tick on a fresh
	// subscription.
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

// BreadthDailyValue is one trailing daily close of the S5FI index. The
// units match the headline Value: percentage points in [0, 100].
type BreadthDailyValue struct {
	Date  string  `json:"date"`  // YYYY-MM-DD
	Value float64 `json:"value"` // % of SPX constituents above 50-day SMA
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

// BreadthSPXResult is the payload for MethodBreadthSPX. The headline
// Value is the current reading; History is the trailing series in
// oldest-first order for sparkline rendering. Threshold derivation
// (green/yellow/red) is intentionally left to the renderer — the spec
// itself says thresholds should be tunable, so the daemon stays out of
// that policy choice.
//
// The Source / Method strings name the data provenance and computation
// path so renderers can disclose how the number was derived. Method is
// a short token; longer methodology disclosure lives in the spec doc.
type BreadthSPXResult struct {
	// State classifies the engine pipeline at the moment this envelope
	// was assembled (cold / computing / ready / degraded). Consumers
	// should branch on this, not on (value==0 && history==[]) heuristics.
	// See BreadthState docs for semantics.
	State BreadthState `json:"state"`
	// Value is the current S5FI reading: percentage of S&P 500
	// constituents trading above their own 50-day simple moving
	// average. 0–100, with 50 the symmetric midpoint. Spec rule of
	// thumb: > 55 healthy, 40–55 watch, < 40 with SPX at highs is the
	// classic late-cycle divergence. Zero is meaningful only when
	// State == "ready" (impossible in practice — every observed market
	// regime puts at least one name above its 50DMA); a State other
	// than "ready" can carry value=0 as the "no data yet" sentinel.
	Value float64 `json:"value"`
	// History is the trailing daily series, oldest first. Length is
	// bounded by BreadthSPXParams.HistoryDays.
	History []BreadthDailyValue `json:"history"`
	// Source identifies the data provenance for the headline value.
	// Free-form; renderers display verbatim.
	Source string `json:"source"`
	// Method is a short token naming the computation path so renderers
	// can disclose methodology. v1 token: "s5fi-direct".
	Method string `json:"method"`
	// AsOf is the daemon's wall-clock when the result was assembled.
	AsOf time.Time `json:"as_of"`
	// SpotAt is the gateway-observation timestamp for the headline
	// Value, distinct from AsOf which covers history + headline.
	SpotAt time.Time `json:"spot_at"`
	// DataType reflects the gateway's feed state when Value was captured
	// — "live", "delayed", "frozen", "delayed-frozen", or "" when no
	// notice has arrived yet. Renderers use this to dim the headline.
	DataType string `json:"data_type,omitempty"`
}

// GammaZeroSPXStatus values are returned on GammaZeroSPXResult.Status and
// drive the dashboard generator's "render the number" vs "render a
// loading state" choice. The compute is heavy (several minutes against
// hundreds of option legs) and runs on a daemon-internal goroutine, so
// the wire shape always carries a state — the first caller of the day
// receives "computing" and a renderer hint; subsequent callers receive
// "ready" with a cached payload until the next NY trading session.
const (
	// GammaZeroStatusComputing — a background compute is in flight; the
	// EtaSeconds / Progress fields carry refresh hints. Callers who can
	// wait may set BreadthSPXParams.WaitMs > 0 on the request to block
	// up to that budget for the result.
	GammaZeroStatusComputing = "computing"
	// GammaZeroStatusReady — Result is populated and reflects the most
	// recent NY trading session's calculation.
	GammaZeroStatusReady = "ready"
	// GammaZeroStatusError — the last compute failed; Error carries the
	// classified reason. Callers retry by re-invoking the method.
	GammaZeroStatusError = "error"
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
// wall" view that's methodology-agnostic.
type StrikeConcentration struct {
	Strike float64 `json:"strike"`
	Expiry string  `json:"expiry"` // YYYY-MM-DD
	Right  string  `json:"right"`  // "C" | "P"
	AbsGEX float64 `json:"abs_gex"`
	OI     int64   `json:"open_interest"`
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
type GammaZeroComputed struct {
	// SpotUnderlying is the price of the underlying instrument
	// (currently SPY — see Source) at which the aggregation was
	// anchored. Field was renamed from SpotSPX when the compute moved
	// from SPX to the more liquid SPY chain (SPY has continuous
	// extended-hours quoting and a single trading class, which keeps
	// the compute robust off-hours).
	SpotUnderlying float64 `json:"spot_underlying"`
	// SpotAt is the gateway-observation timestamp for SpotUnderlying.
	// Distinct from AsOf which covers the whole computation.
	SpotAt time.Time `json:"spot_at"`

	// ZeroGamma is the dealer γ-zero level under the Perfiliev convention
	// (the spot where dealer net gamma crosses zero). nil when no
	// crossing exists within the sweep window — inspect GammaSign in
	// that case to learn whether the whole sweep is long-γ or short-γ.
	ZeroGamma *float64 `json:"zero_gamma"`
	// GapPct is (SpotUnderlying − ZeroGamma) / ZeroGamma × 100. nil iff
	// ZeroGamma is nil. Sign convention: positive = spot above γ-zero
	// (dampening regime); negative = below γ-zero (amplifying regime).
	GapPct *float64 `json:"gap_pct"`
	// GammaSign is "positive" or "negative" and is meaningful only when
	// ZeroGamma is nil — it tells the renderer which side of zero the
	// whole sweep landed on so the UI can say "all long-gamma" or "all
	// short-gamma in window."
	GammaSign string `json:"gamma_sign,omitempty"`
	// Profile is the full (spot, gex) sweep, oldest first. 60 points
	// over [0.85, 1.15] × SpotUnderlying. Renderers chart this as the
	// gamma-exposure curve and visually confirm the zero crossing.
	Profile []GammaProfilePoint `json:"profile"`

	// GammaTotalAbs is the sign-agnostic magnitude signal at
	// SpotUnderlying: Σ |Γ| × OI × 100 × SpotUnderlying² × 0.01. In
	// dollar gamma terms — the total notional dealer hedging flow for
	// a 1% underlying move, independent of any positioning assumption.
	// Larger = market is more sensitive to dealer rebalancing.
	GammaTotalAbs float64 `json:"gamma_total_abs"`
	// TopStrikes is the top-N strikes ranked by absolute gamma notional.
	// Concentration here is more reliable than the signed ZeroGamma in
	// regimes where the dealer-sign assumption may invert.
	TopStrikes []StrikeConcentration `json:"top_strikes"`

	// Expirations is the YYYY-MM-DD list of expirations actually
	// included in the aggregation (after 0DTE-post-settlement filtering
	// and SPXW/SPX merging).
	Expirations []string `json:"expirations"`
	// LegCount is the number of option legs that successfully delivered
	// OI + IV within the per-leg budget. Compare to the theoretical
	// max from Params to spot-check whether the gateway throttled the
	// run (e.g., 240 out of 480 means half the chain dropped — surface
	// to the renderer as a confidence flag).
	LegCount int `json:"leg_count"`
	// DerivedIVLegs counts how many of those legs used the BS-IV
	// Newton-Raphson fallback because the gateway never pushed a
	// model-computation tick. Pre-market this is typically equal to
	// LegCount (the model engine is idle); during regular hours it
	// should stay at 0. Renderers surface a "compute used N derived
	// IVs" disclosure so the prior-session-price anchor is visible
	// to a reader who's about to act on the γ-zero level.
	DerivedIVLegs int `json:"derived_iv_legs,omitempty"`
	// Warnings is a structured list of non-fatal conditions: e.g.,
	// "no_crossing_in_window", "spxw_partial_oi", "low_leg_coverage".
	// Empty when the computation was clean. Renderers surface these
	// as inline badges; the dashboard generator can fail loud or soft
	// based on which codes appear.
	Warnings []string `json:"warnings,omitempty"`

	// Params echoes the v1 calibration window so a renderer can show
	// "computed over 6 expirations within ATM ± 10%" without consulting
	// out-of-band documentation.
	Params GammaZeroParams `json:"params"`
	// Source identifies the data provenance for the headline numbers.
	Source string `json:"source"`
	// Method is a short stable token for the computation path. v1:
	// "perfiliev-bs-sweep-v1". Full methodology disclosure lives in
	// docs/specs/risk-regime-dashboard.md so renderers can deep-link.
	Method string `json:"method"`
	// AsOf is the daemon's wall-clock when the compute finished.
	AsOf time.Time `json:"as_of"`
	// DurationMS is honest about how long the compute took on the wall
	// clock; useful for tuning ExpiryCount / StrikeWidthPct.
	DurationMS int64 `json:"duration_ms"`
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
//     Method/Warnings document the assumptions)
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

// RegimeVIXTerm is Indicator 1: VIX/VIX3M ratio. Watch for sustained
// inversion (ratio > 1.0) over 2-3 sessions, not a single spike.
//
// FieldsMissing is an advisory list of pointer-typed fields above
// (e.g. "vix3m", "ratio") that did NOT land within the fetch budget
// even though the row's primary measurement succeeded. Absent when
// nothing is missing. Use it to dim a sub-cell without re-classifying
// the whole row as `error`.
type RegimeVIXTerm struct {
	Status        string   `json:"status"`
	VIX           *float64 `json:"vix"`
	VIX3M         *float64 `json:"vix3m"`
	Ratio         *float64 `json:"ratio"` // VIX / VIX3M
	DataType      string   `json:"data_type,omitempty"`
	Notes         string   `json:"notes"`
	ErrorMessage  string   `json:"error_message,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
	// Per-scalar provenance. Each *Quality is nil when the corresponding
	// value pointer is nil; otherwise the daemon populates it at the
	// fetch site so renderers can show "firm live", "frozen", or
	// "estimate · 18s" without re-deriving from DataType.
	VIXQuality   *Quality `json:"vix_quality,omitempty"`
	VIX3MQuality *Quality `json:"vix3m_quality,omitempty"`
}

// RegimeHYGSPYDivergence is Indicator 2: HYG vs SPY context. The
// daemon surfaces raw measurements; the consumer compares HYG's
// current to its 50-day SMA and SPY's current to its 52-week high.
//
// FieldsMissing carries optional sub-fields ("spy_52w_high",
// "hyg_50dma") that didn't land — both are best-effort and don't
// downgrade the row's primary status.
type RegimeHYGSPYDivergence struct {
	Status        string   `json:"status"`
	HYGPrice      *float64 `json:"hyg_price"`
	HYG50DMA      *float64 `json:"hyg_50dma"` // 50-day SMA of HYG daily close
	SPYPrice      *float64 `json:"spy_price"`
	SPY52WHigh    *float64 `json:"spy_52w_high"`
	HYGDataType   string   `json:"hyg_data_type,omitempty"`
	Notes         string   `json:"notes"`
	ErrorMessage  string   `json:"error_message,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
	// Per-scalar provenance. SPY52WHigh has two paths (live tick 165 vs
	// history fallback); the Quality field is what the renderer reads to
	// distinguish firm-tick from derived-fallback.
	HYGQuality        *Quality `json:"hyg_quality,omitempty"`
	HYG50DMAQuality   *Quality `json:"hyg_50dma_quality,omitempty"`
	SPYQuality        *Quality `json:"spy_quality,omitempty"`
	SPY52WHighQuality *Quality `json:"spy_52w_high_quality,omitempty"`
}

// RegimeUSDJPY is Indicator 3: USD/JPY exchange rate. Spec measures
// "weekly move" — daemon surfaces last and 7-trading-days-ago close so
// the consumer can compute the change. Source is FX-pair routing
// (CASH/IDEALPRO); routing arrives in a sibling commit.
type RegimeUSDJPY struct {
	Status        string   `json:"status"`
	Symbol        string   `json:"symbol"` // "USD.JPY" canonical form
	Last          *float64 `json:"last"`
	Close7DAgo    *float64 `json:"close_7d_ago"`      // close from 7 trading days ago
	WeeklyChange  *float64 `json:"weekly_change_pct"` // (last − close_7d_ago) / close_7d_ago × 100
	DataType      string   `json:"data_type,omitempty"`
	Notes         string   `json:"notes"`
	ErrorMessage  string   `json:"error_message,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
	// Per-scalar provenance. Last is firm-live (or firm-frozen);
	// Close7DAgo is always estimate-derived (MIDPOINT historical bar).
	LastQuality       *Quality `json:"last_quality,omitempty"`
	Close7DAgoQuality *Quality `json:"close_7d_ago_quality,omitempty"`
}

// RegimeGammaZero is Indicator 4: the existing gamma.zero_spx
// envelope embedded inline. Auto-kicked by regime.snapshot on first
// call of an NY trading day; subsequent calls return the cached
// result. Method token + warnings carry methodology disclosures.
type RegimeGammaZero struct {
	Status        string             `json:"status"`
	Envelope      GammaZeroSPXResult `json:"envelope"`
	Notes         string             `json:"notes"`
	FieldsMissing []string           `json:"fields_missing,omitempty"`
	// Per-scalar provenance for the two values the renderer prints:
	// ZeroGamma is proxy-modelled (carries the perfiliev-bs-sweep-v1
	// methodology); GammaTotalAbs is estimate-derived (sign-agnostic
	// notional summed over observed OI+IV).
	ZeroGammaQuality     *Quality `json:"zero_gamma_quality,omitempty"`
	GammaTotalAbsQuality *Quality `json:"gamma_total_abs_quality,omitempty"`
}

// RegimeBreadth is Indicator 5: the existing breadth.spx envelope
// embedded inline. In v1 IBKR doesn't carry the S5FI feed under any
// known ticker; this row currently returns Status="unavailable" with
// a notes pointer to the disposition decision in the spec doc.
type RegimeBreadth struct {
	Status        string           `json:"status"`
	Envelope      BreadthSPXResult `json:"envelope"`
	Notes         string           `json:"notes"`
	FieldsMissing []string         `json:"fields_missing,omitempty"`
	// Per-scalar provenance for the breadth percentage. In v1 IBKR
	// doesn't carry the S5FI feed under any retail subscription, so
	// this is typically nil (unavailable). When ranked, firm-live or
	// firm-frozen depending on the envelope's DataType.
	ValueQuality *Quality `json:"value_quality,omitempty"`
}

// RegimeSnapshotParams is the input for MethodRegimeSnapshot. Empty
// body means "fetch all five indicators with default parameters." A
// future caller could trim to a subset, but v1 always returns all
// rows — partial responses are surfaced via per-indicator Status.
type RegimeSnapshotParams struct{}

// RegimeSnapshotResult is the wire payload for the dashboard
// generator and the MCP natural-language interface. One JSON
// envelope, all five rows. Each row carries:
//   - raw measurements (no derived status colors)
//   - a `notes` field embedding the spec's threshold bands verbatim,
//     so a consumer can interpret without reading the spec doc
//   - a structured Status the renderer branches on for UI state
//
// Compatibility note for renderers: the daemon never returns nil for
// any indicator field — empty / unavailable indicators surface
// Status="unavailable" with populated Notes. Numerical fields are
// pointers so "not arrived yet" vs "exactly zero" stays
// distinguishable.
type RegimeSnapshotResult struct {
	AsOf             time.Time              `json:"as_of"`
	VIXTermStructure RegimeVIXTerm          `json:"vix_term_structure"`
	HYGSPYDivergence RegimeHYGSPYDivergence `json:"hyg_spy_divergence"`
	USDJPY           RegimeUSDJPY           `json:"usd_jpy"`
	GammaZero        RegimeGammaZero        `json:"gamma_zero"`
	Breadth          RegimeBreadth          `json:"breadth"`
	// SpecDoc points consumers (especially LLM-driven ones) at the
	// canonical methodology + threshold reference so they don't
	// hallucinate band edges. Same path on every response.
	SpecDoc string `json:"spec_doc"`
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
}
