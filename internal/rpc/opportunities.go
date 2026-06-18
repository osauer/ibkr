package rpc

import "time"

const (
	MethodOpportunitiesStatus          = "opportunities.status"
	MethodOpportunitiesSnapshot        = "opportunities.snapshot"
	MethodOpportunitiesRefresh         = "opportunities.refresh"
	MethodOpportunitiesPreviewExercise = "opportunities.preview_exercise"
	MethodOpportunitiesSubmitExercise  = "opportunities.submit_exercise"
	MethodOpportunitiesIgnore          = "opportunities.ignore"

	OpportunityPolicyFingerprintVersion = "opportunity-policy-fp-v1"

	OpportunityPolicyStatusActive   = "active"
	OpportunityPolicyStatusDefault  = "default"
	OpportunityPolicyStatusDrift    = "drift"
	OpportunityPolicyStatusError    = "error"
	OpportunityPolicyStatusDisabled = "disabled"

	OpportunitySnapshotKind          = "ibkr.opportunity_snapshot"
	OpportunitySnapshotSchemaVersion = "opportunity-snapshot-v1"
	OpportunityStatusKind            = "ibkr.opportunity_status"

	OpportunityBucketOptionExercise = "option_exercise"

	OpportunityStateGenerated = "generated"
	OpportunityStateBlocked   = "blocked"

	OpportunityActionExercise = "EXERCISE"

	ExerciseActionExercise = 1
	ExerciseActionLapse    = 2

	ExercisePositionEffectClose    = "close"
	ExercisePositionEffectReduce   = "reduce"
	ExercisePositionEffectOpen     = "open"
	ExercisePositionEffectIncrease = "increase"
	ExercisePositionEffectFlip     = "flip"
	ExercisePositionEffectUnknown  = "unknown"

	ExerciseRiskChangeClosed    = "closed"
	ExerciseRiskChangeReduced   = "reduced"
	ExerciseRiskChangeOpened    = "opened"
	ExerciseRiskChangeIncreased = "increased"
	ExerciseRiskChangeFlipped   = "flipped"
	ExerciseRiskChangeUnknown   = "unknown"
)

type OpportunityPolicyStatus struct {
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

type OpportunityStatus struct {
	Kind           string                  `json:"kind,omitempty"`
	AsOf           time.Time               `json:"as_of,omitzero"`
	Enabled        bool                    `json:"enabled"`
	HotReload      bool                    `json:"hot_reload"`
	ReloadInterval string                  `json:"reload_interval,omitempty"`
	RefreshCadence string                  `json:"refresh_cadence,omitempty"`
	Policy         OpportunityPolicyStatus `json:"policy"`
	Trading        TradingStatus           `json:"trading"`
	Blocked        bool                    `json:"blocked"`
	Blockers       []TradingBlocker        `json:"blockers,omitempty"`
}

type OpportunitySourceFingerprints struct {
	Account   *Fingerprint `json:"account,omitempty"`
	Positions *Fingerprint `json:"positions,omitempty"`
}

type OpportunitySnapshot struct {
	Kind               string                        `json:"kind"`
	SchemaVersion      string                        `json:"schema_version"`
	AsOf               time.Time                     `json:"as_of"`
	Revision           string                        `json:"revision"`
	AccountID          string                        `json:"account_id,omitempty"`
	AccountMode        string                        `json:"account_mode,omitempty"`
	PolicyID           string                        `json:"policy_id,omitempty"`
	PolicyVersion      int                           `json:"policy_version,omitempty"`
	PolicyFingerprint  Fingerprint                   `json:"policy_fingerprint,omitzero"`
	PolicyStatus       OpportunityPolicyStatus       `json:"policy_status"`
	Status             OpportunityStatus             `json:"status"`
	Trading            TradingStatus                 `json:"trading"`
	SourceFingerprints OpportunitySourceFingerprints `json:"source_fingerprints,omitzero"`
	Opportunities      []Opportunity                 `json:"opportunities"`
	Counts             OpportunityCounts             `json:"counts"`
	Blockers           []TradingBlocker              `json:"blockers,omitempty"`
	LoadedFromState    bool                          `json:"loaded_from_state,omitempty"`
}

type OpportunityCounts struct {
	Total                int     `json:"total"`
	Actionable           int     `json:"actionable"`
	Blocked              int     `json:"blocked"`
	OptionExercise       int     `json:"option_exercise"`
	ExpectedGain         float64 `json:"expected_gain,omitempty"`
	ExpectedGainCurrency string  `json:"expected_gain_currency,omitempty"`
}

type Opportunity struct {
	Key                      string                        `json:"key"`
	Revision                 string                        `json:"revision"`
	State                    string                        `json:"state"`
	Bucket                   string                        `json:"bucket"`
	Rank                     int                           `json:"rank"`
	Symbol                   string                        `json:"symbol"`
	SecType                  string                        `json:"sec_type"`
	Action                   string                        `json:"action"`
	ExerciseAction           int                           `json:"exercise_action"`
	Quantity                 int                           `json:"quantity"`
	MaxQuantity              int                           `json:"max_quantity"`
	PositionQuantity         float64                       `json:"position_quantity"`
	PositionEffect           string                        `json:"position_effect"`
	UnderlyingQuantityBefore float64                       `json:"underlying_quantity_before"`
	UnderlyingQuantityAfter  float64                       `json:"underlying_quantity_after"`
	UnderlyingShareChange    float64                       `json:"underlying_share_change"`
	PostExerciseRisk         *OpportunityPostExerciseRisk  `json:"post_exercise_risk,omitempty"`
	Contract                 ContractParams                `json:"contract"`
	UnderlyingContract       ContractParams                `json:"underlying_contract"`
	ExpectedGain             float64                       `json:"expected_gain,omitempty"`
	ExpectedGainCurrency     string                        `json:"expected_gain_currency,omitempty"`
	IntrinsicValue           float64                       `json:"intrinsic_value,omitempty"`
	CloseValue               float64                       `json:"close_value,omitempty"`
	OptionBid                *float64                      `json:"option_bid,omitempty"`
	UnderlyingBid            *float64                      `json:"underlying_bid,omitempty"`
	UnderlyingAsk            *float64                      `json:"underlying_ask,omitempty"`
	Reason                   string                        `json:"reason"`
	Details                  []string                      `json:"details,omitempty"`
	Score                    float64                       `json:"score,omitempty"`
	PolicyID                 string                        `json:"policy_id,omitempty"`
	PolicyVersion            int                           `json:"policy_version,omitempty"`
	PolicyFingerprint        Fingerprint                   `json:"policy_fingerprint,omitzero"`
	SourceFingerprints       OpportunitySourceFingerprints `json:"source_fingerprints,omitzero"`
	Blockers                 []TradingBlocker              `json:"blockers,omitempty"`
	CreatedAt                time.Time                     `json:"created_at,omitzero"`
}

// OpportunityPostExerciseRisk is advisory context for what exercising a long
// option would do to the underlying stock/ETF exposure. It does not authorize
// or block submit; preview/submit gates remain daemon-owned and broker-gated.
type OpportunityPostExerciseRisk struct {
	Underlying                      string   `json:"underlying,omitempty"`
	BeforeQuantity                  float64  `json:"before_quantity"`
	AfterQuantity                   float64  `json:"after_quantity"`
	ShareChange                     float64  `json:"share_change"`
	PositionEffect                  string   `json:"position_effect,omitempty"`
	RiskChange                      string   `json:"risk_change,omitempty"`
	RiskOpened                      bool     `json:"risk_opened,omitempty"`
	RiskIncreased                   bool     `json:"risk_increased,omitempty"`
	RiskFlipped                     bool     `json:"risk_flipped,omitempty"`
	ProtectionReviewNeeded          bool     `json:"protection_review_needed"`
	ProtectionReviewReason          string   `json:"protection_review_reason,omitempty"`
	ProtectionCoverageState         string   `json:"protection_coverage_state,omitempty"`
	CurrentProtectedQuantity        float64  `json:"current_protected_quantity,omitempty"`
	CurrentUnprotectedQuantity      float64  `json:"current_unprotected_quantity,omitempty"`
	CurrentUnprotectedNotionalBase  *float64 `json:"current_unprotected_notional_base,omitempty"`
	UnprotectedNotionalBaseCurrency string   `json:"unprotected_notional_base_currency,omitempty"`
	WarningCodes                    []string `json:"warning_codes,omitempty"`
}

type OpportunitySnapshotParams struct {
	Show bool `json:"show,omitempty"`
}

type OpportunityRefreshParams struct {
	Show bool `json:"show,omitempty"`
}

type OpportunityExercisePreviewParams struct {
	Key       string `json:"key"`
	Revision  string `json:"revision"`
	Quantity  int    `json:"quantity,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	Origin    string `json:"origin,omitempty"`
}

type OpportunityExercisePreviewResult struct {
	Accepted              bool             `json:"accepted"`
	Opportunity           Opportunity      `json:"opportunity"`
	PreviewTokenID        string           `json:"preview_token_id,omitempty"`
	PreviewTokenExpiresAt time.Time        `json:"preview_token_expires_at,omitzero"`
	SubmitEligible        bool             `json:"submit_eligible"`
	Blockers              []TradingBlocker `json:"blockers,omitempty"`
	AsOf                  time.Time        `json:"as_of"`
}

type OpportunityExerciseSubmitParams struct {
	Key       string `json:"key"`
	Revision  string `json:"revision"`
	Quantity  int    `json:"quantity,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	Origin    string `json:"origin,omitempty"`
}

type OpportunityExerciseSubmitResult struct {
	Accepted       bool                              `json:"accepted"`
	Opportunity    Opportunity                       `json:"opportunity"`
	Preview        *OpportunityExercisePreviewResult `json:"preview,omitempty"`
	PreviewTokenID string                            `json:"preview_token_id,omitempty"`
	OrderRef       string                            `json:"order_ref,omitempty"`
	Blockers       []TradingBlocker                  `json:"blockers,omitempty"`
	Message        string                            `json:"message,omitempty"`
	AsOf           time.Time                         `json:"as_of"`
}

type OpportunityIgnoreParams struct {
	Key      string `json:"key"`
	Revision string `json:"revision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type OpportunityIgnoreResult struct {
	Accepted bool      `json:"accepted"`
	Key      string    `json:"key"`
	Revision string    `json:"revision,omitempty"`
	Message  string    `json:"message,omitempty"`
	AsOf     time.Time `json:"as_of"`
}
