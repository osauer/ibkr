package rpc

import (
	"time"

	"github.com/osauer/ibkr/internal/risk"
)

const (
	RiskPlanKind          = "ibkr.risk_plan"
	RiskPlanSchemaVersion = "risk-plan-v1"

	RiskPlanModeAuto        = "auto"
	RiskPlanModeDefend      = "defend"
	RiskPlanModeRebalance   = "rebalance"
	RiskPlanModeStage       = "stage"
	RiskPlanModeConfirmData = "confirm-data"
	RiskPlanModeDeploy      = "deploy"

	RiskPlanCandidatePreviewable   = "previewable"
	RiskPlanCandidateBlocked       = "blocked"
	RiskPlanCandidateInformational = "informational"

	RiskPlanExecutionAuthorityNone = "none"
	RiskPlanNextExpertReview       = "expert_review"
)

type RiskPlanResult struct {
	Kind                       string                   `json:"kind"`
	SchemaVersion              string                   `json:"schema_version"`
	PlanID                     string                   `json:"plan_id"`
	AsOf                       time.Time                `json:"as_of"`
	AccountID                  string                   `json:"account_id,omitempty"`
	BaseCurrency               string                   `json:"base_currency,omitempty"`
	RequestedMode              string                   `json:"requested_mode"`
	ResponseMode               string                   `json:"response_mode"`
	PolicyProfile              string                   `json:"policy_profile"`
	PolicyVersion              string                   `json:"policy_version"`
	PolicyFingerprint          Fingerprint              `json:"policy_fingerprint"`
	TriggerCanaryFingerprint   *Fingerprint             `json:"trigger_canary_fingerprint,omitempty"`
	RefreshedCanaryFingerprint Fingerprint              `json:"refreshed_canary_fingerprint"`
	SourceAsOf                 CanarySourceAsOf         `json:"source_as_of,omitzero"`
	SourceFingerprints         CanarySourceFingerprints `json:"source_fingerprints,omitzero"`
	Canary                     RiskPlanCanarySummary    `json:"canary"`
	RiskBefore                 RiskPlanRiskSnapshot     `json:"risk_before"`
	Candidates                 []RiskPlanCandidate      `json:"candidates"`
	Warnings                   []string                 `json:"warnings,omitempty"`
	BestPracticeChecks         []RiskPlanPracticeCheck  `json:"best_practice_checks,omitempty"`
	ExecutionAuthority         string                   `json:"execution_authority"`
	NextRequiredStep           string                   `json:"next_required_step"`
	NotExecution               string                   `json:"not_execution"`
}

type RiskPlanCanarySummary struct {
	Action             string                `json:"action,omitempty"`
	Direction          risk.SignalDirection  `json:"direction,omitempty"`
	Severity           risk.SignalSeverity   `json:"severity,omitempty"`
	PlannerModeHint    risk.PlannerMode      `json:"planner_mode_hint,omitempty"`
	PlannerReadiness   risk.PlannerReadiness `json:"planner_readiness,omitempty"`
	MarketConfirmation string                `json:"market_confirmation,omitempty"`
	PortfolioFit       string                `json:"portfolio_fit,omitempty"`
	InputHealth        string                `json:"input_health,omitempty"`
	Summary            string                `json:"summary,omitempty"`
	PrimaryDrivers     []risk.SignalID       `json:"primary_drivers,omitempty"`
	Signals            []risk.Signal         `json:"signals,omitempty"`
}

type RiskPlanRiskSnapshot struct {
	NetLiquidationBase    float64  `json:"net_liquidation_base,omitempty"`
	BaseCurrency          string   `json:"base_currency,omitempty"`
	MarginCushionPct      *float64 `json:"margin_cushion_pct,omitempty"`
	LookAheadCushionPct   *float64 `json:"look_ahead_cushion_pct,omitempty"`
	GrossExposurePctNLV   *float64 `json:"gross_exposure_pct_nlv,omitempty"`
	NetDeltaPctNLV        *float64 `json:"net_delta_pct_nlv,omitempty"`
	GrossDeltaPctNLV      *float64 `json:"gross_delta_pct_nlv,omitempty"`
	DailyPnLPctNLV        *float64 `json:"daily_pnl_pct_nlv,omitempty"`
	LargestExposure       string   `json:"largest_exposure,omitempty"`
	LargestExposurePctNLV *float64 `json:"largest_exposure_pct_nlv,omitempty"`
	LargestDeltaExposure  string   `json:"largest_delta_exposure,omitempty"`
	LargestDeltaPctNLV    *float64 `json:"largest_delta_pct_nlv,omitempty"`
	SPYHedgeOffsetPct     *float64 `json:"spy_hedge_offset_pct,omitempty"`
	OptionGreeks          string   `json:"option_greeks,omitempty"`
}

type RiskPlanCandidate struct {
	ID                   string                 `json:"id"`
	Rank                 int                    `json:"rank"`
	Status               string                 `json:"status"`
	Intent               string                 `json:"intent"`
	Subject              string                 `json:"subject"`
	Reason               string                 `json:"reason"`
	PolicySignalIDs      []risk.SignalID        `json:"policy_signal_ids,omitempty"`
	Legs                 []RiskPlanCandidateLeg `json:"legs"`
	EstimatedRiskAfter   RiskPlanRiskSnapshot   `json:"estimated_risk_after"`
	EstimatedReduction   RiskPlanReduction      `json:"estimated_reduction"`
	EstimatedTradingCost RiskPlanTradingCost    `json:"estimated_trading_cost"`
	BlockedBy            []string               `json:"blocked_by,omitempty"`
	Warnings             []string               `json:"warnings,omitempty"`
	PreviewCommand       string                 `json:"preview_command,omitempty"`
	References           []string               `json:"references,omitempty"`
}

type RiskPlanCandidateLeg struct {
	Action                  string         `json:"action"`
	Contract                ContractParams `json:"contract"`
	Quantity                int            `json:"quantity"`
	HeldQuantity            float64        `json:"held_quantity"`
	PositionEffect          string         `json:"position_effect"`
	OrderType               string         `json:"order_type"`
	TIF                     string         `json:"tif"`
	OutsideRTH              bool           `json:"outside_rth"`
	LimitStrategy           string         `json:"limit_strategy"`
	EstimatedLimitPrice     *float64       `json:"estimated_limit_price,omitempty"`
	Bid                     *float64       `json:"bid,omitempty"`
	Ask                     *float64       `json:"ask,omitempty"`
	Mid                     *float64       `json:"mid,omitempty"`
	Spread                  *float64       `json:"spread,omitempty"`
	SpreadPctOfMid          *float64       `json:"spread_pct_of_mid,omitempty"`
	DTE                     *int           `json:"dte,omitempty"`
	Moneyness               *float64       `json:"moneyness,omitempty"`
	Delta                   *float64       `json:"delta,omitempty"`
	Gamma                   *float64       `json:"gamma,omitempty"`
	Theta                   *float64       `json:"theta,omitempty"`
	Vega                    *float64       `json:"vega,omitempty"`
	MarketValueBase         float64        `json:"market_value_base,omitempty"`
	DollarDeltaBase         *float64       `json:"dollar_delta_base,omitempty"`
	RealizedPnLEstimateBase *float64       `json:"realized_pnl_estimate_base,omitempty"`
	Warnings                []string       `json:"warnings,omitempty"`
}

type RiskPlanReduction struct {
	MarketValueBase     float64  `json:"market_value_base,omitempty"`
	GrossExposurePctNLV *float64 `json:"gross_exposure_pct_nlv,omitempty"`
	NetDeltaPctNLV      *float64 `json:"net_delta_pct_nlv,omitempty"`
	GrossDeltaPctNLV    *float64 `json:"gross_delta_pct_nlv,omitempty"`
	RealizedPnLBase     *float64 `json:"realized_pnl_base,omitempty"`
}

type RiskPlanTradingCost struct {
	EstimatedSlippageBase *float64 `json:"estimated_slippage_base,omitempty"`
	MaxSpreadRule         string   `json:"max_spread_rule,omitempty"`
	WhatIfRequired        bool     `json:"what_if_required"`
}

type RiskPlanPracticeCheck struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Reference string `json:"reference,omitempty"`
	Note      string `json:"note,omitempty"`
}

type RiskPlanOrderPreviewResult struct {
	Kind              string                    `json:"kind"`
	SchemaVersion     string                    `json:"schema_version"`
	AsOf              time.Time                 `json:"as_of"`
	PlanID            string                    `json:"plan_id"`
	CandidateID       string                    `json:"candidate_id"`
	PolicyFingerprint Fingerprint               `json:"policy_fingerprint"`
	SourceValidation  string                    `json:"source_validation"`
	Previews          []RiskPlanOrderLegPreview `json:"previews"`
	TokenMinted       bool                      `json:"token_minted"`
	SubmitEligible    bool                      `json:"submit_eligible"`
	Executable        bool                      `json:"executable"`
	WhatIf            OrderWhatIfResult         `json:"what_if"`
	Blockers          []string                  `json:"blockers,omitempty"`
	NotExecution      string                    `json:"not_execution"`
}

type RiskPlanOrderLegPreview struct {
	CandidateLeg RiskPlanCandidateLeg `json:"candidate_leg"`
	Draft        OrderDraft           `json:"draft"`
}
