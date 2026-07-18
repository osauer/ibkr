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
	NudgeHealthReasonNone                = ""
	NudgeHealthReasonPolicyUnapproved    = "policy_unapproved"
	NudgeHealthReasonCadenceUnapproved   = "cadence_unapproved"
	NudgeHealthReasonEvidenceStale       = "evidence_stale"
	NudgeHealthReasonSourceUnavailable   = "source_unavailable"
	NudgeHealthReasonEvaluationError     = "evaluation_error"
	NudgeHealthReasonCoverageUnavailable = "coverage_unavailable"
	NudgeHealthReasonInvalid             = "invalid_health"
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
	health.Policy = normalizeNudgeInputHealth(health.Policy)
	health.Reconciliation = normalizeNudgeInputHealth(health.Reconciliation)
	health.Capital = normalizeNudgeInputHealth(health.Capital)
	health.Pins = normalizeNudgeInputHealth(health.Pins)
	health.Cadence = normalizeNudgeInputHealth(health.Cadence)
	health.ConfirmedFlow = normalizeNudgeInputHealth(health.ConfirmedFlow)
	health.Aggregate = aggregateNormalizedNudgeSourceHealth(health, candidateCount)
	return health
}

func normalizeNudgeInputHealth(health NudgeInputHealth) NudgeInputHealth {
	validPair := false
	if !health.AsOf.IsZero() {
		switch health.Status {
		case NudgeInputStatusOK:
			validPair = health.Reason == NudgeHealthReasonNone
		case NudgeInputStatusUnapproved:
			validPair = health.Reason == NudgeHealthReasonPolicyUnapproved || health.Reason == NudgeHealthReasonCadenceUnapproved
		case NudgeInputStatusStale:
			validPair = health.Reason == NudgeHealthReasonEvidenceStale
		case NudgeInputStatusUnavailable:
			validPair = health.Reason == NudgeHealthReasonSourceUnavailable || health.Reason == NudgeHealthReasonCoverageUnavailable
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
	AsOf         time.Time         `json:"as_of"`
	Candidates   []NudgeCandidate  `json:"candidates"`
	SourceHealth NudgeSourceHealth `json:"source_health"`
}

func (result NudgesSnapshotResult) MarshalJSON() ([]byte, error) {
	if result.AsOf.IsZero() {
		return nil, errors.New("nudge snapshot is missing as_of")
	}
	candidates := make([]NudgeCandidate, len(result.Candidates))
	for i, candidate := range result.Candidates {
		canonical, err := canonicalizeRPCNudgeCandidate(candidate)
		if err != nil {
			return nil, fmt.Errorf("invalid nudge candidate at index %d: %w", i, err)
		}
		candidates[i] = canonical
	}
	normalizedHealth := NormalizeNudgeSourceHealth(result.SourceHealth, len(candidates))
	wire := struct {
		AsOf         time.Time             `json:"as_of"`
		Candidates   []NudgeCandidate      `json:"candidates"`
		SourceHealth nudgeSourceHealthWire `json:"source_health"`
	}{
		AsOf: result.AsOf, Candidates: candidates, SourceHealth: nudgeSourceHealthWire(normalizedHealth),
	}
	return json.Marshal(wire)
}

func (result NudgesSnapshotResult) IsCleanEmpty() bool {
	if result.AsOf.IsZero() {
		return false
	}
	normalized := NormalizeNudgeSourceHealth(result.SourceHealth, len(result.Candidates))
	return len(result.Candidates) == 0 && normalized.Aggregate == NudgeAggregateReady
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
