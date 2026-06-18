package rpc

import "time"

const (
	MethodAutoTradeStatus        = "auto_trade.status"
	MethodTradeProposalsSnapshot = "trade_proposals.snapshot"
	MethodTradeProposalsRefresh  = "trade_proposals.refresh"
	MethodTradeProposalsPreview  = "trade_proposals.preview"
	MethodTradeProposalsSubmit   = "trade_proposals.submit"
	MethodTradeProposalsIgnore   = "trade_proposals.ignore"

	ProtectionPolicyFingerprintVersion = "protection-policy-fp-v1"

	ProtectionPolicyStatusActive   = "active"
	ProtectionPolicyStatusDefault  = "default"
	ProtectionPolicyStatusDrift    = "drift"
	ProtectionPolicyStatusError    = "error"
	ProtectionPolicyStatusDisabled = "disabled"

	TradeProposalSnapshotKind = "ibkr.trade_proposal_snapshot"
	// TradeProposalSnapshotSchemaVersion v2 adds account/mode scoping:
	// account_id is a concrete single account (never the "All" aggregate)
	// and account_mode records the paper/live session the proposals were
	// generated under. Adoption of a persisted snapshot at daemon startup
	// gates on the scope being concrete, not on this version string, so
	// v1 snapshots (which lack account_mode) fail closed automatically.
	TradeProposalSnapshotSchemaVersion = "trade-proposal-snapshot-v2"

	TradeProposalBucketThetaHygiene  = "theta_hygiene"
	TradeProposalBucketRiskReduction = "risk_reduction"
	TradeProposalBucketTrailingStop  = "trailing_stop"

	TradeProposalStateGenerated = "generated"
	TradeProposalStateBlocked   = "blocked"
)

type ProtectionPolicyStatus struct {
	Kind          string           `json:"kind,omitempty"`
	Status        string           `json:"status"`
	PolicyID      string           `json:"policy_id,omitempty"`
	PolicyVersion int              `json:"policy_version,omitempty"`
	Profile       string           `json:"profile,omitempty"`
	Fingerprint   Fingerprint      `json:"fingerprint,omitzero"`
	Source        string           `json:"source,omitempty"`
	Path          string           `json:"path,omitempty"`
	LoadedAt      time.Time        `json:"loaded_at,omitzero"`
	LastCheckedAt time.Time        `json:"last_checked_at,omitzero"`
	Message       string           `json:"message,omitempty"`
	Blockers      []TradingBlocker `json:"blockers,omitempty"`
}

type AutoTradeStatus struct {
	Kind             string                 `json:"kind,omitempty"`
	AsOf             time.Time              `json:"as_of,omitzero"`
	Trading          TradingStatus          `json:"trading"`
	ProposalsEnabled bool                   `json:"proposals_enabled"`
	Enabled          bool                   `json:"enabled"`
	AutoSubmit       bool                   `json:"auto_submit"`
	FastPathEnabled  bool                   `json:"fast_path_enabled"`
	HotReload        bool                   `json:"hot_reload"`
	ReloadInterval   string                 `json:"reload_interval,omitempty"`
	ProposalCadence  string                 `json:"proposal_cadence,omitempty"`
	Policy           ProtectionPolicyStatus `json:"policy"`
	Blocked          bool                   `json:"blocked"`
	Blockers         []TradingBlocker       `json:"blockers,omitempty"`
}

type TradeProposalSourceFingerprints struct {
	Account      *Fingerprint `json:"account,omitempty"`
	Positions    *Fingerprint `json:"positions,omitempty"`
	Regime       *Fingerprint `json:"regime,omitempty"`
	MarketEvents *Fingerprint `json:"market_events,omitempty"`
}

type TradeProposalSnapshot struct {
	Kind               string                          `json:"kind"`
	SchemaVersion      string                          `json:"schema_version"`
	AsOf               time.Time                       `json:"as_of"`
	Revision           string                          `json:"revision"`
	AccountID          string                          `json:"account_id,omitempty"`
	AccountMode        string                          `json:"account_mode,omitempty"`
	PolicyID           string                          `json:"policy_id,omitempty"`
	PolicyVersion      int                             `json:"policy_version,omitempty"`
	PolicyFingerprint  Fingerprint                     `json:"policy_fingerprint,omitzero"`
	PolicyStatus       ProtectionPolicyStatus          `json:"policy_status"`
	AutoTrade          AutoTradeStatus                 `json:"auto_trade"`
	Trading            TradingStatus                   `json:"trading"`
	SourceFingerprints TradeProposalSourceFingerprints `json:"source_fingerprints,omitzero"`
	MarketEvents       *MarketEventsResult             `json:"market_events,omitempty"`
	Proposals          []TradeProposal                 `json:"proposals"`
	Counts             TradeProposalCounts             `json:"counts"`
	Blockers           []TradingBlocker                `json:"blockers,omitempty"`
	LoadedFromState    bool                            `json:"loaded_from_state,omitempty"`
}

type TradeProposalCounts struct {
	Total                       int     `json:"total"`
	Actionable                  int     `json:"actionable"`
	ThetaHygiene                int     `json:"theta_hygiene"`
	RiskReduction               int     `json:"risk_reduction"`
	TrailingStop                int     `json:"trailing_stop"`
	MarketFlags                 int     `json:"market_flags,omitempty"`
	ThetaPerDay                 float64 `json:"theta_per_day"`
	RiskReductionExcessNotional float64 `json:"risk_reduction_excess_notional,omitempty"`
	RiskReductionExcessCurrency string  `json:"risk_reduction_excess_currency,omitempty"`
}

type TradeProposal struct {
	Key                string                           `json:"key"`
	Revision           string                           `json:"revision"`
	State              string                           `json:"state"`
	Bucket             string                           `json:"bucket"`
	Rank               int                              `json:"rank"`
	Symbol             string                           `json:"symbol"`
	SecType            string                           `json:"sec_type"`
	Action             string                           `json:"action"`
	Quantity           int                              `json:"quantity"`
	MaxQuantity        int                              `json:"max_quantity"`
	PositionQuantity   float64                          `json:"position_quantity"`
	PositionEffect     string                           `json:"position_effect"`
	OrderType          string                           `json:"order_type"`
	Trail              *OrderTrailSpec                  `json:"trail,omitempty"`
	TrailSizing        *TradeProposalTrailSizing        `json:"trail_sizing,omitempty"`
	ExecutionSemantics *TradeProposalExecutionSemantics `json:"execution_semantics,omitempty"`
	StopRisk           *TradeProposalStopRisk           `json:"stop_risk,omitempty"`
	StopLadder         []TradeProposalStopLadderStep    `json:"stop_ladder,omitempty"`
	TriggerMethod      int                              `json:"trigger_method,omitempty"`
	TIF                string                           `json:"tif"`
	OutsideRTH         bool                             `json:"outside_rth"`
	Contract           ContractParams                   `json:"contract"`
	Reason             string                           `json:"reason"`
	Details            []string                         `json:"details,omitempty"`
	Score              float64                          `json:"score,omitempty"`
	ThetaPerDay        float64                          `json:"theta_per_day,omitempty"`
	Notional           float64                          `json:"notional,omitempty"`
	RiskExcessNotional float64                          `json:"risk_excess_notional,omitempty"`
	RiskExcessCurrency string                           `json:"risk_excess_currency,omitempty"`
	MarketValuePctNLV  *float64                         `json:"market_value_pct_nlv,omitempty"`
	MarketFlags        []MarketEventFlag                `json:"market_flags,omitempty"`
	LimitPrice         *float64                         `json:"limit_price,omitempty"`
	PolicyID           string                           `json:"policy_id,omitempty"`
	PolicyVersion      int                              `json:"policy_version,omitempty"`
	PolicyFingerprint  Fingerprint                      `json:"policy_fingerprint,omitzero"`
	SourceFingerprints TradeProposalSourceFingerprints  `json:"source_fingerprints,omitzero"`
	Blockers           []TradingBlocker                 `json:"blockers,omitempty"`
	CreatedAt          time.Time                        `json:"created_at,omitzero"`
}

// TradeProposalTrailSizing is the daemon-owned explanation for a protective
// trail. Percent fields use human units (10.0 means 10%), matching the
// protection policy TOML and OrderTrailSpec's broker percent convention.
type TradeProposalTrailSizing struct {
	Method            string    `json:"method,omitempty"`
	Version           string    `json:"version,omitempty"`
	DataQuality       string    `json:"data_quality,omitempty"`
	SelectedBy        string    `json:"selected_by,omitempty"`
	Fallback          bool      `json:"fallback,omitempty"`
	Capped            bool      `json:"capped,omitempty"`
	ReferencePrice    *float64  `json:"reference_price,omitempty"`
	ReferenceSource   string    `json:"reference_source,omitempty"`
	ReferenceAsOf     time.Time `json:"reference_as_of,omitzero"`
	PolicyMinPct      float64   `json:"policy_min_pct,omitempty"`
	PolicyDefaultPct  float64   `json:"policy_default_pct,omitempty"`
	PolicyFallbackPct float64   `json:"policy_fallback_pct,omitempty"`
	PolicyMaxPct      float64   `json:"policy_max_pct,omitempty"`
	ChosenPct         float64   `json:"chosen_pct,omitempty"`
	ChosenAmount      *float64  `json:"chosen_amount,omitempty"`
	InitialStopPrice  *float64  `json:"initial_stop_price,omitempty"`
	ATR14             *float64  `json:"atr_14,omitempty"`
	ATRPct            *float64  `json:"atr_pct,omitempty"`
	ATRMultiplier     *float64  `json:"atr_multiplier,omitempty"`
	ATRCandidatePct   *float64  `json:"atr_candidate_pct,omitempty"`
	SpreadPct         *float64  `json:"spread_pct,omitempty"`
	SpreadMultiplier  *float64  `json:"spread_multiplier,omitempty"`
	SpreadFloorPct    *float64  `json:"spread_floor_pct,omitempty"`
	MissingReasons    []string  `json:"missing_reasons,omitempty"`
	AsOf              time.Time `json:"as_of,omitzero"`
}

// TradeProposalExecutionSemantics explains how a protective stop is expected
// to behave at the broker. It is disclosure only; broker WhatIf/order status
// remains authoritative for placement and lifecycle state.
type TradeProposalExecutionSemantics struct {
	ReferenceSide      string    `json:"reference_side,omitempty"`
	ReferencePrice     *float64  `json:"reference_price,omitempty"`
	ReferenceAsOf      time.Time `json:"reference_as_of,omitzero"`
	TriggerMethod      int       `json:"trigger_method,omitempty"`
	TriggerMethodLabel string    `json:"trigger_method_label,omitempty"`
	TriggerSource      string    `json:"trigger_source,omitempty"`
	TriggerEffect      string    `json:"trigger_effect,omitempty"`
	PriceGuarantee     string    `json:"price_guarantee,omitempty"`
}

// TradeProposalStopRisk estimates the near-stop account impact from the
// proposal's current reference price. It is not a fill guarantee and must not
// be treated as a broker promise: stop orders can gap or slip.
type TradeProposalStopRisk struct {
	ReferencePrice      *float64                  `json:"reference_price,omitempty"`
	StopPrice           *float64                  `json:"stop_price,omitempty"`
	Distance            *float64                  `json:"distance,omitempty"`
	DistancePct         *float64                  `json:"distance_pct,omitempty"`
	Quantity            int                       `json:"quantity,omitempty"`
	Multiplier          int                       `json:"multiplier,omitempty"`
	EstimatedLoss       *float64                  `json:"estimated_loss_ccy,omitempty"`
	Currency            string                    `json:"currency,omitempty"`
	EstimatedLossBase   *float64                  `json:"estimated_loss_base,omitempty"`
	BaseCurrency        string                    `json:"base_currency,omitempty"`
	EstimatedLossPctNLV *float64                  `json:"estimated_loss_pct_nlv,omitempty"`
	GapScenario         *TradeProposalStopRiskGap `json:"gap_scenario,omitempty"`
	WarningCodes        []string                  `json:"warning_codes,omitempty"`
}

type TradeProposalStopRiskGap struct {
	Label                 string   `json:"label,omitempty"`
	GapPct                float64  `json:"gap_pct,omitempty"`
	AssumedExecutionPrice *float64 `json:"assumed_execution_price,omitempty"`
	EstimatedLoss         *float64 `json:"estimated_loss_ccy,omitempty"`
	EstimatedLossBase     *float64 `json:"estimated_loss_base,omitempty"`
	EstimatedLossPctNLV   *float64 `json:"estimated_loss_pct_nlv,omitempty"`
}

type TradeProposalStopLadderStep struct {
	Label               string   `json:"label"`
	Kind                string   `json:"kind,omitempty"`
	Percent             *float64 `json:"percent,omitempty"`
	StopPrice           *float64 `json:"stop_price,omitempty"`
	EstimatedLoss       *float64 `json:"estimated_loss_ccy,omitempty"`
	EstimatedLossBase   *float64 `json:"estimated_loss_base,omitempty"`
	EstimatedLossPctNLV *float64 `json:"estimated_loss_pct_nlv,omitempty"`
	ReferencePrice      *float64 `json:"reference_price,omitempty"`
}

type TradeProposalSnapshotParams struct {
	Show bool `json:"show,omitempty"`
}

type TradeProposalRefreshParams struct {
	Show bool `json:"show,omitempty"`
}

type TradeProposalPreviewParams struct {
	Key       string `json:"key"`
	Revision  string `json:"revision"`
	Quantity  int    `json:"quantity,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	FastPath  bool   `json:"fast_path,omitempty"`
}

type TradeProposalPreviewResult struct {
	Accepted              bool                       `json:"accepted"`
	Proposal              TradeProposal              `json:"proposal"`
	PreviewTokenID        string                     `json:"preview_token_id,omitempty"`
	PreviewTokenExpiresAt time.Time                  `json:"preview_token_expires_at,omitzero"`
	SubmitEligible        bool                       `json:"submit_eligible"`
	Preview               *TradeProposalOrderPreview `json:"preview,omitempty"`
	Blockers              []TradingBlocker           `json:"blockers,omitempty"`
	AsOf                  time.Time                  `json:"as_of"`
}

type TradeProposalOrderPreview struct {
	PreviewTokenID        string                           `json:"preview_token_id,omitempty"`
	PreviewTokenScope     string                           `json:"preview_token_scope,omitempty"`
	PreviewTokenExpiresAt time.Time                        `json:"preview_token_expires_at,omitzero"`
	TokenMinted           bool                             `json:"token_minted"`
	SubmitEligible        bool                             `json:"submit_eligible"`
	Mode                  string                           `json:"mode"`
	Account               string                           `json:"account"`
	Endpoint              string                           `json:"endpoint"`
	ClientID              int                              `json:"client_id"`
	Draft                 OrderDraft                       `json:"draft"`
	Quote                 OrderQuoteSnapshot               `json:"quote"`
	Position              OrderPositionImpact              `json:"position"`
	ExecutionSemantics    *TradeProposalExecutionSemantics `json:"execution_semantics,omitempty"`
	StopRisk              *TradeProposalStopRisk           `json:"stop_risk,omitempty"`
	Notional              float64                          `json:"notional"`
	MaxNotional           float64                          `json:"max_notional,omitempty"`
	WhatIf                OrderWhatIfResult                `json:"what_if"`
	Warnings              []DataWarning                    `json:"warnings,omitempty"`
	AsOf                  time.Time                        `json:"as_of"`
}

type TradeProposalSubmitParams struct {
	Key       string `json:"key"`
	Revision  string `json:"revision"`
	Quantity  int    `json:"quantity,omitempty"`
	FastPath  bool   `json:"fast_path,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	Origin    string `json:"origin,omitempty"`
}

type TradeProposalSubmitResult struct {
	Accepted       bool                       `json:"accepted"`
	Proposal       TradeProposal              `json:"proposal"`
	Preview        *TradeProposalOrderPreview `json:"preview,omitempty"`
	Place          *OrderPlaceResult          `json:"place,omitempty"`
	PreviewTokenID string                     `json:"preview_token_id,omitempty"`
	OrderRef       string                     `json:"order_ref,omitempty"`
	Blockers       []TradingBlocker           `json:"blockers,omitempty"`
	Message        string                     `json:"message,omitempty"`
	AsOf           time.Time                  `json:"as_of"`
}

type TradeProposalIgnoreParams struct {
	Key      string `json:"key"`
	Revision string `json:"revision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type TradeProposalIgnoreResult struct {
	Accepted bool      `json:"accepted"`
	Key      string    `json:"key"`
	Revision string    `json:"revision,omitempty"`
	Message  string    `json:"message,omitempty"`
	AsOf     time.Time `json:"as_of"`
}
