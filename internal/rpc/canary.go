package rpc

import (
	"time"

	"github.com/osauer/ibkr/v2/internal/marketcal"
	"github.com/osauer/ibkr/v2/internal/risk"
)

// CanaryInput is the pure state input shared by the CLI and MCP tool. It
// deliberately consumes existing daemon snapshots instead of adding a second
// risk-data path: account margin, portfolio exposure, and market regime stay
// single-source-of-truth.
type CanaryInput struct {
	Account      AccountResult
	Positions    PositionsResult
	Regime       RegimeSnapshotResult
	MarketEvents MarketEventsResult
	Now          time.Time
}

// CanaryResult is the compact scheduled-monitor payload. The canary is
// stateless: it combines current broad-market regime with the current portfolio
// shape, then emits a fresh action snapshot. Fingerprint is the canonical
// alert identity for monitors; SourceFingerprints records the classified
// upstream state the canary consumed.
type CanaryResult struct {
	AsOf               time.Time                `json:"as_of"`
	SourceAsOf         CanarySourceAsOf         `json:"source_as_of,omitzero"`
	Fingerprint        Fingerprint              `json:"fingerprint"`
	SourceFingerprints CanarySourceFingerprints `json:"source_fingerprints,omitzero"`
	SourceHealth       []SourceHealth           `json:"source_health,omitempty"`
	Policy             string                   `json:"policy,omitempty"`
	PolicyProfile      string                   `json:"policy_profile,omitempty"`
	PolicyVersion      string                   `json:"policy_version,omitempty"`
	PolicyFingerprint  Fingerprint              `json:"policy_fingerprint,omitzero"`
	Action             string                   `json:"action,omitempty"`
	MarketConfirmation string                   `json:"market_confirmation,omitempty"`
	PortfolioFit       string                   `json:"portfolio_fit,omitempty"`
	InputHealth        string                   `json:"input_health,omitempty"`
	Direction          risk.SignalDirection     `json:"direction,omitempty"`
	Severity           risk.SignalSeverity      `json:"severity"`
	PlannerModeHint    risk.PlannerMode         `json:"planner_mode_hint,omitempty"`
	PlannerReadiness   risk.PlannerReadiness    `json:"planner_readiness,omitempty"`
	Summary            string                   `json:"summary"`
	PrimaryDrivers     []risk.SignalID          `json:"primary_drivers,omitempty"`
	Signals            []risk.Signal            `json:"signals,omitempty"`
	Rows               []CanaryRow              `json:"rows"`
	Portfolio          CanaryPortfolioSummary   `json:"portfolio"`
	Market             CanaryMarketSummary      `json:"market"`
	MarketIndicators   []CanaryMarketIndicator  `json:"market_indicators,omitempty"`
	Warnings           []string                 `json:"warnings,omitempty"`
	NotExecution       string                   `json:"not_execution"`
}

type CanarySourceAsOf struct {
	Account      time.Time `json:"account,omitzero"`
	Positions    time.Time `json:"positions,omitzero"`
	Regime       time.Time `json:"regime,omitzero"`
	MarketEvents time.Time `json:"market_events,omitzero"`
}

type CanarySourceFingerprints struct {
	Account      *Fingerprint `json:"account,omitempty"`
	Positions    *Fingerprint `json:"positions,omitempty"`
	Regime       *Fingerprint `json:"regime,omitempty"`
	MarketEvents *Fingerprint `json:"market_events,omitempty"`
}

type CanaryRow struct {
	Title     string               `json:"title"`
	Direction risk.SignalDirection `json:"direction,omitempty"`
	Severity  risk.SignalSeverity  `json:"severity"`
	Guidance  string               `json:"guidance"`
	Evidence  string               `json:"evidence,omitempty"`
}

type CanaryMarketIndicator struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // green | amber | red | context | n/a
	AsOf    string `json:"as_of,omitempty"`
	Reading string `json:"reading,omitempty"`
	Comment string `json:"comment,omitempty"`
}

type CanaryPortfolioSummary struct {
	BaseCurrency         string                     `json:"base_currency,omitempty"`
	NetLiquidation       float64                    `json:"net_liquidation,omitempty"`
	CushionPct           *float64                   `json:"cushion_pct,omitempty"`
	LookAheadCushionPct  *float64                   `json:"look_ahead_cushion_pct,omitempty"`
	GrossExposurePctNLV  *float64                   `json:"gross_exposure_pct_nlv,omitempty"`
	NetDeltaPctNLV       *float64                   `json:"net_delta_pct_nlv,omitempty"`
	GrossDeltaPctNLV     *float64                   `json:"gross_delta_pct_nlv,omitempty"`
	LargestExposure      string                     `json:"largest_exposure,omitempty"`
	LargestExposurePct   *float64                   `json:"largest_exposure_pct_nlv,omitempty"`
	LargestDeltaExposure string                     `json:"largest_delta_exposure,omitempty"`
	LargestDeltaPctNLV   *float64                   `json:"largest_delta_pct_nlv,omitempty"`
	DailyPnLPct          *float64                   `json:"daily_pnl_pct,omitempty"`
	OptionGreeks         string                     `json:"option_greeks,omitempty"`
	ProtectionCoverage   *ProtectionCoverageSummary `json:"protection_coverage,omitempty"`
	HeldStress           []CanaryHeldStress         `json:"held_stress,omitempty"`
}

// CanaryHeldStress is a bounded, positions-only explanation of stress inside
// material held underlyings. It deliberately avoids option-chain fan-out; all
// fields come from the existing positions/account snapshot.
type CanaryHeldStress struct {
	Underlying            string            `json:"underlying"`
	MaterialReasons       []string          `json:"material_reasons,omitempty"`
	MarketValuePctNLV     *float64          `json:"market_value_pct_nlv,omitempty"`
	DeltaPctNLV           *float64          `json:"delta_pct_nlv,omitempty"`
	DailyPnLPctNLV        *float64          `json:"daily_pnl_pct_nlv,omitempty"`
	NearExpiryDeltaPctNLV *float64          `json:"near_expiry_delta_pct_nlv,omitempty"`
	NearExpiryGamma       *float64          `json:"near_expiry_gamma,omitempty"`
	NearExpiryMinDTE      *int              `json:"near_expiry_min_dte,omitempty"`
	LiquidityFlags        []string          `json:"liquidity_flags,omitempty"`
	MarketFlags           []MarketEventFlag `json:"market_flags,omitempty"`
	SignalIDs             []risk.SignalID   `json:"signal_ids,omitempty"`
}

type CanaryMarketSummary struct {
	RegimeVerdict string        `json:"regime_verdict,omitempty"`
	RegimePosture RegimePosture `json:"regime_posture,omitzero"`
	RedClusters   int           `json:"red_clusters"`
	// EligibleRedClusters counts reds that passed the confirmation gates
	// (depth + persistence + freshness) — the only reds canary's
	// act/urgent-grade decisions key on. RedClusters keeps the visible
	// (confirmed-band) reds for watch-grade evidence; the difference is
	// disclosed in UnconfirmedRedClusterNames.
	EligibleRedClusters        int      `json:"eligible_red_clusters"`
	EligibleRedClusterNames    []string `json:"eligible_red_cluster_names,omitempty"`
	YellowClusters             int      `json:"yellow_clusters"`
	RankedClusters             int      `json:"ranked_clusters"`
	UnrankedClusters           int      `json:"unranked_clusters"`
	RedClusterNames            []string `json:"red_cluster_names,omitempty"`
	YellowClusterNames         []string `json:"yellow_cluster_names,omitempty"`
	UnconfirmedRedClusterNames []string `json:"unconfirmed_red_cluster_names,omitempty"`
	AmbiguousClusters          []string `json:"ambiguous_clusters,omitempty"`
	PartialClusters            []string `json:"partial_clusters,omitempty"`
	ComputingClusters          []string `json:"computing_clusters,omitempty"`
	DegradedClusters           []string `json:"degraded_clusters,omitempty"`
	StaleClusters              []string `json:"stale_clusters,omitempty"`
	SPYPrice                   *float64 `json:"spy_price,omitempty"`
	SPYChangePct               *float64 `json:"spy_change_pct,omitempty"`
	VIX                        *float64 `json:"vix,omitempty"`
	VIXChangePct               *float64 `json:"vix_change_pct,omitempty"`
	// TapeSessionState classifies the official US cash-equity calendar date
	// the canary ran on. On a closed date (weekend/holiday) the direct
	// SPY/VIX day-change prints are frozen last-session values — the anchors
	// can even reset independently while closed — so they carry evidence but
	// cannot confirm severity. Empty means outside embedded calendar
	// coverage: severity behaves as before (fail-open).
	TapeSessionState  string     `json:"tape_session_state,omitempty"`
	TapeSessionReason string     `json:"tape_session_reason,omitempty"`
	TapeNextOpen      *time.Time `json:"tape_next_open,omitempty"`
}

// TapeSessionState values shared by CanaryMarketSummary and
// RegimeSnapshotResult. Trading dates keep full direct-tape severity at any
// hour (pre/post/overnight moves are live prints the tape-shock row exists to
// catch); closed dates demote frozen tape shocks to observe and bar them from
// entering or holding tape-driven lifecycle stages until the next open
// re-evaluates them from live prints.
const (
	TapeSessionTradingDate = "trading_date"
	TapeSessionClosedDate  = "closed_date"
)

// TapeSessionFor classifies the official US cash-equity calendar date at now
// for direct-tape severity — the single policy copy behind both the canary
// tape row and the regime lifecycle tape terms. Trading dates (regular and
// early-close) keep full severity at any hour: pre/post/overnight prints are
// live (VIX prints overnight on weekdays). Closed dates (weekend/holiday)
// freeze the SPY/VIX day-change anchors at last-session values — which can
// even reset independently while closed — so frozen shocks carry evidence but
// confirm nothing until the next open. Outside embedded calendar coverage the
// state stays empty and consumers fail open to full severity.
func TapeSessionFor(now time.Time) (state, reason string, nextOpen *time.Time) {
	sess, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now)
	if err != nil {
		return "", "", nil
	}
	switch sess.State {
	case marketcal.StateClosed, marketcal.StateHoliday:
		return TapeSessionClosedDate, sess.Reason, sess.NextOpen
	case marketcal.StateRegular, marketcal.StateEarlyClose:
		return TapeSessionTradingDate, "", nil
	default:
		return "", "", nil
	}
}
