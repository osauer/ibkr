package rpc

import "time"

// MethodAlertCandidates exposes the daemon-authored, source-neutral alert
// candidate snapshot. The method is observational: it has no delivery target,
// acknowledgement, policy-change, or broker-write authority.
const MethodAlertCandidates = "alerts.candidates"

// MethodAlertShadowStatus exposes redacted calibration/coverage measurements.
// It cannot activate delivery and deliberately carries no candidate identity.
const MethodAlertShadowStatus = "alerts.shadow_status"

// AlertCandidatesParams is intentionally empty. Producers and their coverage
// universe are daemon-owned; callers cannot select sources or weaken evidence
// requirements through request parameters.
type AlertCandidatesParams struct{}

// AlertShadowStatusParams is intentionally empty because shadow scope and
// source coverage are daemon-owned.
type AlertShadowStatusParams struct{}

// AlertShadowStatusResult is the redacted, read-only commissioning view of the
// daemon alert registry. DeliveryActive is authoritative; measurements never
// imply permission to deliver.
type AlertShadowStatusResult struct {
	AsOf                  time.Time                 `json:"as_of,omitzero"`
	Authority             string                    `json:"authority"`
	DeliveryActive        bool                      `json:"delivery_active"`
	ExpectedSources       []AlertSource             `json:"expected_sources"`
	Evaluations           uint64                    `json:"evaluations"`
	RegistryApplyFailures uint64                    `json:"registry_apply_failures"`
	Equivocations         uint64                    `json:"equivocations"`
	LastErrorCode         string                    `json:"last_error_code,omitempty"`
	HumanPrecision        string                    `json:"human_precision"`
	HumanRecall           string                    `json:"human_recall"`
	Sources               []AlertShadowSourceStatus `json:"sources"`
}

// AlertShadowSourceStatus reports coverage and calibration health for one
// allowlisted source without exposing candidate or account identities.
type AlertShadowSourceStatus struct {
	Source       AlertSource             `json:"source"`
	Status       string                  `json:"status"`
	Reason       string                  `json:"reason"`
	InputAsOf    time.Time               `json:"input_as_of,omitzero"`
	ObservedAt   time.Time               `json:"observed_at,omitzero"`
	Covered      bool                    `json:"covered"`
	Active       int                     `json:"active_candidates"`
	Measurements AlertShadowMeasurements `json:"measurements"`
}

// AlertShadowMeasurements contains cumulative, redacted commissioning counts.
// Zero values mean no recorded observation, not successful coverage.
type AlertShadowMeasurements struct {
	Evaluations              uint64  `json:"evaluations"`
	CoveredEvaluations       uint64  `json:"covered_evaluations"`
	ActiveEvaluations        uint64  `json:"active_evaluations"`
	ActiveObservations       uint64  `json:"active_observations"`
	EpisodesOpened           uint64  `json:"episodes_opened"`
	EpisodesEscalated        uint64  `json:"episodes_escalated"`
	EpisodesRecovered        uint64  `json:"episodes_recovered"`
	EpisodesReopened         uint64  `json:"episodes_reopened"`
	DuplicateInputs          uint64  `json:"duplicate_inputs"`
	DuplicateCandidates      uint64  `json:"duplicate_candidates"`
	RepeatedActive           uint64  `json:"repeated_active_observations"`
	ActiveEvidenceChurn      uint64  `json:"active_evidence_revisions"`
	Equivocations            uint64  `json:"equivocations"`
	StaleSuppressions        uint64  `json:"stale_suppressions"`
	CoverageFailures         uint64  `json:"coverage_failures"`
	TimeToObserveSamples     uint64  `json:"time_to_observe_samples"`
	TimeToObserveTotalSecond float64 `json:"time_to_observe_total_seconds"`
	TimeToObserveMaxSecond   float64 `json:"time_to_observe_max_seconds"`
}
