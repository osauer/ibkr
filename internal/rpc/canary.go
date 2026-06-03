package rpc

import (
	"time"

	"github.com/osauer/ibkr/internal/risk"
)

// CanaryInput is the pure state input shared by the CLI and MCP tool. It
// deliberately consumes existing daemon snapshots instead of adding a second
// risk-data path: account margin, portfolio exposure, and market regime stay
// single-source-of-truth.
type CanaryInput struct {
	Account   AccountResult
	Positions PositionsResult
	Regime    RegimeSnapshotResult
	Now       time.Time
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
	Account   time.Time `json:"account,omitzero"`
	Positions time.Time `json:"positions,omitzero"`
	Regime    time.Time `json:"regime,omitzero"`
}

type CanarySourceFingerprints struct {
	Account   *Fingerprint `json:"account,omitempty"`
	Positions *Fingerprint `json:"positions,omitempty"`
	Regime    *Fingerprint `json:"regime,omitempty"`
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
	BaseCurrency         string   `json:"base_currency,omitempty"`
	NetLiquidation       float64  `json:"net_liquidation,omitempty"`
	CushionPct           *float64 `json:"cushion_pct,omitempty"`
	LookAheadCushionPct  *float64 `json:"look_ahead_cushion_pct,omitempty"`
	GrossExposurePctNLV  *float64 `json:"gross_exposure_pct_nlv,omitempty"`
	NetDeltaPctNLV       *float64 `json:"net_delta_pct_nlv,omitempty"`
	GrossDeltaPctNLV     *float64 `json:"gross_delta_pct_nlv,omitempty"`
	LargestExposure      string   `json:"largest_exposure,omitempty"`
	LargestExposurePct   *float64 `json:"largest_exposure_pct_nlv,omitempty"`
	LargestDeltaExposure string   `json:"largest_delta_exposure,omitempty"`
	LargestDeltaPctNLV   *float64 `json:"largest_delta_pct_nlv,omitempty"`
	DailyPnLPct          *float64 `json:"daily_pnl_pct,omitempty"`
	OptionGreeks         string   `json:"option_greeks,omitempty"`
}

type CanaryMarketSummary struct {
	RegimeVerdict              string   `json:"regime_verdict,omitempty"`
	RedClusters                int      `json:"red_clusters"`
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
}
