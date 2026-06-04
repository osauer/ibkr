package orderreview

import (
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

const (
	SourceKindRiskPlan = "risk_plan"

	IntentMitigateRisk = "mitigate_risk"
)

type Set struct {
	ID                 string                       `json:"id"`
	Revision           string                       `json:"revision"`
	SourceKind         string                       `json:"source_kind"`
	Intent             string                       `json:"intent"`
	PlanID             string                       `json:"plan_id,omitempty"`
	CanaryFingerprint  string                       `json:"canary_fingerprint,omitempty"`
	SourceFingerprints rpc.CanarySourceFingerprints `json:"source_fingerprints,omitzero"`
	Rows               []Row                        `json:"rows"`
	Capabilities       rpc.TradingStatus            `json:"capabilities"`
	LatestPreview      *Preview                     `json:"latest_preview,omitempty"`
	CreatedAt          time.Time                    `json:"created_at"`
	UpdatedAt          time.Time                    `json:"updated_at"`
}

type Row struct {
	RowID            string             `json:"row_id"`
	CandidateID      string             `json:"candidate_id"`
	LegIndex         int                `json:"leg_index"`
	ProposedQuantity int                `json:"proposed_quantity"`
	EditableQuantity int                `json:"editable_quantity"`
	MaxQuantity      int                `json:"max_quantity"`
	Included         bool               `json:"included"`
	Action           string             `json:"action"`
	Contract         rpc.ContractParams `json:"contract"`
	OrderType        string             `json:"order_type"`
	LimitStrategy    string             `json:"limit_strategy"`
	LimitPrice       *float64           `json:"limit_price,omitempty"`
	TIF              string             `json:"tif"`
	OutsideRTH       bool               `json:"outside_rth"`
	Rationale        string             `json:"rationale,omitempty"`
	RiskImpact       RiskImpact         `json:"risk_impact"`
	Status           string             `json:"status,omitempty"`
	HeldQuantity     float64            `json:"held_quantity,omitempty"`
	PositionEffect   string             `json:"position_effect,omitempty"`
	Blockers         []string           `json:"blockers,omitempty"`
	Warnings         []string           `json:"warnings,omitempty"`
}

type RiskImpact struct {
	MarketValueBase     float64  `json:"market_value_base,omitempty"`
	DollarDeltaBase     *float64 `json:"dollar_delta_base,omitempty"`
	GrossExposurePctNLV *float64 `json:"gross_exposure_pct_nlv,omitempty"`
	NetDeltaPctNLV      *float64 `json:"net_delta_pct_nlv,omitempty"`
	GrossDeltaPctNLV    *float64 `json:"gross_delta_pct_nlv,omitempty"`
	RealizedPnLBase     *float64 `json:"realized_pnl_base,omitempty"`
}

type Preview struct {
	ID           string            `json:"id"`
	SetID        string            `json:"set_id"`
	SetRevision  string            `json:"set_revision"`
	Rows         []PreviewRow      `json:"rows"`
	SubmitReady  bool              `json:"submit_ready"`
	Blockers     []string          `json:"blockers,omitempty"`
	Capabilities rpc.TradingStatus `json:"capabilities"`
	AsOf         time.Time         `json:"as_of"`
}

type PreviewRow struct {
	RowID          string                  `json:"row_id"`
	Included       bool                    `json:"included"`
	Quantity       int                     `json:"quantity"`
	Draft          *rpc.OrderDraft         `json:"draft,omitempty"`
	Preview        *rpc.OrderPreviewResult `json:"preview,omitempty"`
	TokenMinted    bool                    `json:"token_minted"`
	SubmitEligible bool                    `json:"submit_eligible"`
	WhatIfStatus   string                  `json:"what_if_status,omitempty"`
	Blockers       []string                `json:"blockers,omitempty"`
	Warnings       []string                `json:"warnings,omitempty"`
	Failure        string                  `json:"failure,omitempty"`
}

type RebaseChange struct {
	RowID   string `json:"row_id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
