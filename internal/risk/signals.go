package risk

type SignalDirection string

const (
	DirectionDefensive    SignalDirection = "defensive"
	DirectionConstructive SignalDirection = "constructive"
	DirectionRebalance    SignalDirection = "rebalance"
	DirectionMixed        SignalDirection = "mixed"
	DirectionDataQuality  SignalDirection = "data_quality"
)

type PortfolioPosture string

const (
	PortfolioPostureNeutral           PortfolioPosture = "neutral"
	PortfolioPostureThreat            PortfolioPosture = "threat"
	PortfolioPostureRebalance         PortfolioPosture = "rebalance"
	PortfolioPostureOpportunity       PortfolioPosture = "opportunity"
	PortfolioPostureThreatOpportunity PortfolioPosture = "threat_opportunity"
	PortfolioPostureConfirmData       PortfolioPosture = "confirm_data"
)

type SignalSeverity string

const (
	SeverityObserve SignalSeverity = "observe"
	SeverityWatch   SignalSeverity = "watch"
	SeverityAct     SignalSeverity = "act"
	SeverityUrgent  SignalSeverity = "urgent"
)

type PlannerMode string

const (
	PlannerModeNone        PlannerMode = "none"
	PlannerModeStage       PlannerMode = "stage"
	PlannerModeDefend      PlannerMode = "defend"
	PlannerModeRebalance   PlannerMode = "rebalance"
	PlannerModeDeploy      PlannerMode = "deploy"
	PlannerModeConfirmData PlannerMode = "confirm_data"
)

type PlannerReadiness string

const (
	PlannerReadinessNone     PlannerReadiness = "none"
	PlannerReadinessWatch    PlannerReadiness = "watch"
	PlannerReadinessPrestage PlannerReadiness = "prestage"
	PlannerReadinessReady    PlannerReadiness = "ready"
	PlannerReadinessBlocked  PlannerReadiness = "blocked"
)

type SignalID string

const (
	SignalMarginCushionLow       SignalID = "margin_cushion_low"
	SignalLookAheadCushionLow    SignalID = "lookahead_cushion_low"
	SignalMarketSelloffViolent   SignalID = "market_selloff_violent"
	SignalVolSpikeConfirmed      SignalID = "vol_spike_confirmed"
	SignalMarketRallyViolent     SignalID = "market_rally_violent"
	SignalVolCrushConfirmed      SignalID = "vol_crush_confirmed"
	SignalRegimeStressConfirmed  SignalID = "regime_stress_confirmed"
	SignalRegimeStressEarly      SignalID = "regime_stress_early"
	SignalFXCarryUnwind          SignalID = "fx_carry_unwind"
	SignalGammaRed               SignalID = "gamma_red"
	SignalGrossExposureHigh      SignalID = "gross_exposure_high"
	SignalNetDeltaHigh           SignalID = "net_delta_high"
	SignalGrossDeltaHigh         SignalID = "gross_delta_high"
	SignalSingleNameExposureHigh SignalID = "single_name_exposure_high"
	SignalSingleNameDeltaHigh    SignalID = "single_name_delta_high"
	SignalOptionGreeksDegraded   SignalID = "option_greeks_degraded"
	SignalShortConvexityHigh     SignalID = "short_convexity_high"
	SignalPortfolioPnLShock      SignalID = "portfolio_pnl_shock"
	SignalRiskDataDegraded       SignalID = "risk_data_degraded"
	SignalMarketDataStale        SignalID = "market_data_stale"
)

type Signal struct {
	ID               SignalID         `json:"id"`
	Direction        SignalDirection  `json:"direction"`
	Posture          PortfolioPosture `json:"posture,omitempty"`
	Severity         SignalSeverity   `json:"severity"`
	Subject          string           `json:"subject,omitempty"`
	Metric           string           `json:"metric,omitempty"`
	Observed         *float64         `json:"observed,omitempty"`
	Threshold        *float64         `json:"threshold,omitempty"`
	Target           *float64         `json:"target,omitempty"`
	Unit             string           `json:"unit,omitempty"`
	Evidence         string           `json:"evidence,omitempty"`
	Confidence       string           `json:"confidence,omitempty"`
	ConfidenceImpact string           `json:"confidence_impact,omitempty"`
	BlockedBy        []string         `json:"blocked_by,omitempty"`
}
