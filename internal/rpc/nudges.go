package rpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
)

const MethodNudgesSnapshot = "nudges.snapshot"

const (
	NudgeKindReconcileDue       = risk.NudgeKindReconcileDue
	NudgeKindReconcileException = risk.NudgeKindReconcileException
	NudgeKindShadowWouldBlock   = risk.NudgeKindShadowWouldBlock
	NudgeKindDrawdownLatched    = risk.NudgeKindDrawdownLatched
	NudgeKindPolicyDrift        = risk.NudgeKindPolicyDrift
	NudgeKindConfirmedFlow      = risk.NudgeKindConfirmedFlow
	NudgeKindMonthlyPulse       = risk.NudgeKindMonthlyPulse

	NudgeStateDueSoon  = risk.NudgeStateDueSoon
	NudgeStateOverdue  = risk.NudgeStateOverdue
	NudgeStateOpen     = risk.NudgeStateOpen
	NudgeStateObserved = risk.NudgeStateObserved
	NudgeStateDue      = risk.NudgeStateDue

	NudgeSeverityWatch = risk.NudgeSeverityWatch
	NudgeSeverityAct   = risk.NudgeSeverityAct

	NudgeDestinationMonitor = risk.NudgeDestinationMonitor
	NudgeDestinationAlerts  = risk.NudgeDestinationAlerts
)

const (
	NudgeInputStatusOK          = "ok"
	NudgeInputStatusUnapproved  = "unapproved"
	NudgeInputStatusStale       = "stale"
	NudgeInputStatusUnavailable = "unavailable"
	NudgeInputStatusError       = "error"

	NudgeAggregateReady      = "ready"
	NudgeAggregateSuppressed = "suppressed"
	NudgeAggregateDegraded   = "degraded"
)

// Nudge source-health reasons are allowlisted tokens. Raw errors, paths,
// upstream fingerprints, and broker text do not belong on this contract.
const (
	NudgeHealthReasonNone                  = ""
	NudgeHealthReasonPolicyUnapproved      = "policy_unapproved"
	NudgeHealthReasonCadenceUnapproved     = "cadence_unapproved"
	NudgeHealthReasonEvidenceStale         = "evidence_stale"
	NudgeHealthReasonSourceUnavailable     = "source_unavailable"
	NudgeHealthReasonEvaluationError       = "evaluation_error"
	NudgeHealthReasonCoverageUnavailable   = "coverage_unavailable"
	NudgeHealthReasonCutoverReviewRequired = "cutover_review_required"
	NudgeHealthReasonInvalid               = "invalid_health"
)

// NudgesSnapshotParams is empty because nudges.snapshot is a
// gateway-independent, side-effect-free read.
type NudgesSnapshotParams struct{}

// NudgeCandidate is intentionally lockscreen-safe. Its title and body are
// daemon-authored enum templates; Fingerprint is an opaque semantic identity.
type NudgeCandidate struct {
	Fingerprint string    `json:"fingerprint"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	Severity    string    `json:"severity"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	OccurredAt  time.Time `json:"occurred_at,omitzero"`
	DueAt       time.Time `json:"due_at,omitzero"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	Destination string    `json:"destination"`
}

type NudgeInputHealth struct {
	Status string    `json:"status"` // ok | unapproved | stale | unavailable | error
	Reason string    `json:"reason,omitempty"`
	AsOf   time.Time `json:"as_of,omitzero"`
}

// NudgeSourceHealth is separate from app polling/relay health. Fixed fields
// prevent generic notes, raw fingerprints, or unknown source names from
// widening the wire contract.
type NudgeSourceHealth struct {
	Aggregate      string           `json:"aggregate"` // ready | suppressed | degraded
	Policy         NudgeInputHealth `json:"policy"`
	Reconciliation NudgeInputHealth `json:"reconciliation"`
	Capital        NudgeInputHealth `json:"capital"`
	Pins           NudgeInputHealth `json:"pins"`
	Cadence        NudgeInputHealth `json:"cadence"`
	ConfirmedFlow  NudgeInputHealth `json:"confirmed_flow"`
}

type nudgeHealthSource uint8

const (
	nudgeHealthSourcePolicy nudgeHealthSource = iota
	nudgeHealthSourceReconciliation
	nudgeHealthSourceCapital
	nudgeHealthSourcePins
	nudgeHealthSourceCadence
	nudgeHealthSourceConfirmedFlow
)

// AggregateNudgeSourceHealth makes an empty result reassuring only when every
// relevant authority is ready. Partial coverage with safe candidates is
// degraded; the same coverage gap with no candidates is suppressed.
func AggregateNudgeSourceHealth(health NudgeSourceHealth, candidateCount int) string {
	return NormalizeNudgeSourceHealth(health, candidateCount).Aggregate
}

// NormalizeNudgeSourceHealth is the mandatory wire boundary. It removes raw
// or incoherent status/reason values, preserves missing timestamps as missing,
// and derives Aggregate rather than trusting caller-provided state.
func NormalizeNudgeSourceHealth(health NudgeSourceHealth, candidateCount int) NudgeSourceHealth {
	health.Policy = normalizeNudgeInputHealth(health.Policy, nudgeHealthSourcePolicy)
	health.Reconciliation = normalizeNudgeInputHealth(health.Reconciliation, nudgeHealthSourceReconciliation)
	health.Capital = normalizeNudgeInputHealth(health.Capital, nudgeHealthSourceCapital)
	health.Pins = normalizeNudgeInputHealth(health.Pins, nudgeHealthSourcePins)
	health.Cadence = normalizeNudgeInputHealth(health.Cadence, nudgeHealthSourceCadence)
	health.ConfirmedFlow = normalizeNudgeInputHealth(health.ConfirmedFlow, nudgeHealthSourceConfirmedFlow)
	health.Aggregate = aggregateNormalizedNudgeSourceHealth(health, candidateCount)
	return health
}

func normalizeNudgeInputHealth(health NudgeInputHealth, source nudgeHealthSource) NudgeInputHealth {
	validPair := false
	if !health.AsOf.IsZero() {
		switch health.Status {
		case NudgeInputStatusOK:
			validPair = health.Reason == NudgeHealthReasonNone
		case NudgeInputStatusUnapproved:
			validPair = health.Reason == NudgeHealthReasonPolicyUnapproved ||
				health.Reason == NudgeHealthReasonCadenceUnapproved ||
				(source == nudgeHealthSourceConfirmedFlow && health.Reason == NudgeHealthReasonCutoverReviewRequired)
		case NudgeInputStatusStale:
			validPair = health.Reason == NudgeHealthReasonEvidenceStale
		case NudgeInputStatusUnavailable:
			validPair = health.Reason == NudgeHealthReasonSourceUnavailable ||
				health.Reason == NudgeHealthReasonCoverageUnavailable
		case NudgeInputStatusError:
			validPair = health.Reason == NudgeHealthReasonEvaluationError || health.Reason == NudgeHealthReasonInvalid
		}
	}
	if !validPair {
		health.Status = NudgeInputStatusError
		health.Reason = NudgeHealthReasonInvalid
	}
	return health
}

func aggregateNormalizedNudgeSourceHealth(health NudgeSourceHealth, candidateCount int) string {
	statuses := [...]string{
		health.Policy.Status,
		health.Reconciliation.Status,
		health.Capital.Status,
		health.Pins.Status,
		health.Cadence.Status,
		health.ConfirmedFlow.Status,
	}
	allReady := true
	for _, status := range statuses {
		if status != NudgeInputStatusOK {
			allReady = false
			break
		}
	}
	if allReady {
		return NudgeAggregateReady
	}
	if candidateCount <= 0 {
		return NudgeAggregateSuppressed
	}
	return NudgeAggregateDegraded
}

type nudgeSourceHealthWire NudgeSourceHealth

// MarshalJSON prevents standalone source-health values from carrying a false
// ready aggregate. Without result candidate context, partial health is
// conservatively suppressed.
func (health NudgeSourceHealth) MarshalJSON() ([]byte, error) {
	normalized := NormalizeNudgeSourceHealth(health, 0)
	return json.Marshal(nudgeSourceHealthWire(normalized))
}

type NudgesSnapshotResult struct {
	AsOf                  time.Time                   `json:"as_of"`
	Candidates            []NudgeCandidate            `json:"candidates"`
	SourceHealth          NudgeSourceHealth           `json:"source_health"`
	ConfirmedFlowCoverage *NudgeConfirmedFlowCoverage `json:"confirmed_flow_coverage,omitempty"`
}

// NudgeConfirmedFlowCoverage discloses only the redacted cutover boundary and
// whether flows before that boundary still require review.
type NudgeConfirmedFlowCoverage struct {
	CoverageFrom              time.Time `json:"coverage_from"`
	PreCutoverFlowsUnreviewed bool      `json:"pre_cutover_flows_unreviewed"`
}

func (result NudgesSnapshotResult) MarshalJSON() ([]byte, error) {
	normalizedHealth := NormalizeNudgeSourceHealth(result.SourceHealth, len(result.Candidates))
	if err := validateNudgeSnapshotConfirmedFlowCoherence(result.AsOf, result.ConfirmedFlowCoverage, normalizedHealth.ConfirmedFlow); err != nil {
		return nil, err
	}
	if err := validateNudgeSnapshotSourceHealthTimestamps(result.AsOf, normalizedHealth); err != nil {
		return nil, err
	}
	candidates := make([]NudgeCandidate, len(result.Candidates))
	for i, candidate := range result.Candidates {
		canonical, err := canonicalizeRPCNudgeCandidate(candidate)
		if err != nil {
			return nil, fmt.Errorf("invalid nudge candidate at index %d: %w", i, err)
		}
		candidates[i] = canonical
	}
	wire := struct {
		AsOf                  time.Time                   `json:"as_of"`
		Candidates            []NudgeCandidate            `json:"candidates"`
		SourceHealth          nudgeSourceHealthWire       `json:"source_health"`
		ConfirmedFlowCoverage *NudgeConfirmedFlowCoverage `json:"confirmed_flow_coverage,omitempty"`
	}{
		AsOf:                  result.AsOf,
		Candidates:            candidates,
		SourceHealth:          nudgeSourceHealthWire(normalizedHealth),
		ConfirmedFlowCoverage: result.ConfirmedFlowCoverage,
	}
	return json.Marshal(wire)
}

func (result NudgesSnapshotResult) IsCleanEmpty() bool {
	normalized := NormalizeNudgeSourceHealth(result.SourceHealth, len(result.Candidates))
	if err := validateNudgeSnapshotConfirmedFlowCoherence(result.AsOf, result.ConfirmedFlowCoverage, normalized.ConfirmedFlow); err != nil {
		return false
	}
	if err := validateNudgeSnapshotSourceHealthTimestamps(result.AsOf, normalized); err != nil {
		return false
	}
	return len(result.Candidates) == 0 && normalized.Aggregate == NudgeAggregateReady
}

func validateNudgeSnapshotSourceHealthTimestamps(asOf time.Time, health NudgeSourceHealth) error {
	inputs := [...]struct {
		name string
		asOf time.Time
	}{
		{name: "policy", asOf: health.Policy.AsOf},
		{name: "reconciliation", asOf: health.Reconciliation.AsOf},
		{name: "capital", asOf: health.Capital.AsOf},
		{name: "pins", asOf: health.Pins.AsOf},
		{name: "cadence", asOf: health.Cadence.AsOf},
		{name: "confirmed_flow", asOf: health.ConfirmedFlow.AsOf},
	}
	for _, input := range inputs {
		if !input.asOf.IsZero() && input.asOf.After(asOf) {
			return fmt.Errorf("nudge snapshot %s source health is after as_of", input.name)
		}
	}
	return nil
}

func validateNudgeSnapshotConfirmedFlowCoherence(
	asOf time.Time,
	coverage *NudgeConfirmedFlowCoverage,
	confirmedFlowHealth NudgeInputHealth,
) error {
	if asOf.IsZero() {
		return errors.New("nudge snapshot is missing as_of")
	}
	if coverage == nil {
		if confirmedFlowHealth.Status == NudgeInputStatusOK {
			return errors.New("nudge snapshot has ready confirmed-flow health without coverage")
		}
		return nil
	}
	if coverage.CoverageFrom.IsZero() {
		return errors.New("nudge snapshot confirmed-flow coverage is missing coverage_from")
	}
	if coverage.CoverageFrom.After(asOf) {
		return errors.New("nudge snapshot confirmed-flow coverage is after as_of")
	}
	if !confirmedFlowHealth.AsOf.IsZero() && coverage.CoverageFrom.After(confirmedFlowHealth.AsOf) {
		return errors.New("nudge snapshot confirmed-flow coverage is newer than source health")
	}
	if coverage.PreCutoverFlowsUnreviewed {
		if confirmedFlowHealth.Status != NudgeInputStatusUnapproved || confirmedFlowHealth.Reason != NudgeHealthReasonCutoverReviewRequired {
			return errors.New("nudge snapshot unreviewed confirmed-flow coverage has incoherent source health")
		}
		return nil
	}
	if confirmedFlowHealth.Reason == NudgeHealthReasonCutoverReviewRequired {
		return errors.New("nudge snapshot reviewed confirmed-flow coverage still requires cutover review")
	}
	return nil
}

func canonicalizeRPCNudgeCandidate(candidate NudgeCandidate) (NudgeCandidate, error) {
	canonical, err := risk.CanonicalizeNudgeCandidate(risk.NudgeCandidate{
		Fingerprint: candidate.Fingerprint,
		Kind:        candidate.Kind,
		State:       candidate.State,
		Severity:    candidate.Severity,
		Title:       candidate.Title,
		Body:        candidate.Body,
		OccurredAt:  candidate.OccurredAt,
		DueAt:       candidate.DueAt,
		ExpiresAt:   candidate.ExpiresAt,
		Destination: candidate.Destination,
	})
	if err != nil {
		return NudgeCandidate{}, err
	}
	return NudgeCandidate{
		Fingerprint: canonical.Fingerprint,
		Kind:        canonical.Kind,
		State:       canonical.State,
		Severity:    canonical.Severity,
		Title:       canonical.Title,
		Body:        canonical.Body,
		OccurredAt:  canonical.OccurredAt,
		DueAt:       canonical.DueAt,
		ExpiresAt:   canonical.ExpiresAt,
		Destination: canonical.Destination,
	}, nil
}
