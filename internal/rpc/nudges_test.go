package rpc

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

type nudgeSourceHealthTimestampTest struct {
	name             string
	mutateHealth     func(*NudgeSourceHealth)
	coverageFrom     time.Time
	wantMarshalError bool
	wantCleanEmpty   bool
}

func TestNudgesSnapshotSourceHealthTimestampMarshalValidation(t *testing.T) {
	for _, tt := range nudgeSourceHealthTimestampTests() {
		t.Run(tt.name, func(t *testing.T) {
			_, err := json.Marshal(nudgeSourceHealthTimestampTestSnapshot(tt))
			if got := err != nil; got != tt.wantMarshalError {
				t.Fatalf("json.Marshal() error = %v, want error = %v", err, tt.wantMarshalError)
			}
		})
	}
}

func TestNudgesSnapshotSourceHealthTimestampCleanEmptyValidation(t *testing.T) {
	for _, tt := range nudgeSourceHealthTimestampTests() {
		t.Run(tt.name, func(t *testing.T) {
			if got := nudgeSourceHealthTimestampTestSnapshot(tt).IsCleanEmpty(); got != tt.wantCleanEmpty {
				t.Fatalf("IsCleanEmpty() = %v, want %v", got, tt.wantCleanEmpty)
			}
		})
	}
}

func nudgeSourceHealthTimestampTests() []nudgeSourceHealthTimestampTest {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	future := asOf.Add(time.Nanosecond)
	earlier := asOf.Add(-time.Hour)
	return []nudgeSourceHealthTimestampTest{
		{
			name:             "future policy",
			mutateHealth:     func(health *NudgeSourceHealth) { health.Policy.AsOf = future },
			wantMarshalError: true,
		},
		{
			name:             "future reconciliation",
			mutateHealth:     func(health *NudgeSourceHealth) { health.Reconciliation.AsOf = future },
			wantMarshalError: true,
		},
		{
			name:             "future capital",
			mutateHealth:     func(health *NudgeSourceHealth) { health.Capital.AsOf = future },
			wantMarshalError: true,
		},
		{
			name:             "future pins",
			mutateHealth:     func(health *NudgeSourceHealth) { health.Pins.AsOf = future },
			wantMarshalError: true,
		},
		{
			name:             "future cadence",
			mutateHealth:     func(health *NudgeSourceHealth) { health.Cadence.AsOf = future },
			wantMarshalError: true,
		},
		{
			name:             "future confirmed flow",
			mutateHealth:     func(health *NudgeSourceHealth) { health.ConfirmedFlow.AsOf = future },
			wantMarshalError: true,
		},
		{
			name:           "timestamps equal snapshot",
			coverageFrom:   asOf,
			wantCleanEmpty: true,
		},
		{
			name: "valid earlier timestamps",
			mutateHealth: func(health *NudgeSourceHealth) {
				health.Policy.AsOf = earlier
				health.Reconciliation.AsOf = earlier
				health.Capital.AsOf = earlier
				health.Pins.AsOf = earlier
				health.Cadence.AsOf = earlier
				health.ConfirmedFlow.AsOf = earlier
			},
			coverageFrom:   earlier,
			wantCleanEmpty: true,
		},
	}
}

func nudgeSourceHealthTimestampTestSnapshot(tt nudgeSourceHealthTimestampTest) NudgesSnapshotResult {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	health := allReadyNudgeSourceHealth(asOf)
	if tt.mutateHealth != nil {
		tt.mutateHealth(&health)
	}
	coverageFrom := tt.coverageFrom
	if coverageFrom.IsZero() {
		coverageFrom = asOf.Add(-time.Hour)
	}
	return NudgesSnapshotResult{
		AsOf:         asOf,
		SourceHealth: health,
		ConfirmedFlowCoverage: &NudgeConfirmedFlowCoverage{
			CoverageFrom: coverageFrom,
		},
	}
}

type nudgeCoverageCoherenceTest struct {
	name             string
	coverage         *NudgeConfirmedFlowCoverage
	confirmedFlow    NudgeInputHealth
	wantMarshalError bool
	wantCleanEmpty   bool
}

func TestNudgesSnapshotConfirmedFlowCoverageMarshalCoherence(t *testing.T) {
	for _, tt := range nudgeCoverageCoherenceTests() {
		t.Run(tt.name, func(t *testing.T) {
			_, err := json.Marshal(nudgeCoverageTestSnapshot(tt))
			if got := err != nil; got != tt.wantMarshalError {
				t.Fatalf("json.Marshal() error = %v, want error = %v", err, tt.wantMarshalError)
			}
		})
	}
}

func TestNudgesSnapshotConfirmedFlowCoverageCleanEmptyCoherence(t *testing.T) {
	for _, tt := range nudgeCoverageCoherenceTests() {
		t.Run(tt.name, func(t *testing.T) {
			if got := nudgeCoverageTestSnapshot(tt).IsCleanEmpty(); got != tt.wantCleanEmpty {
				t.Fatalf("IsCleanEmpty() = %v, want %v", got, tt.wantCleanEmpty)
			}
		})
	}
}

func nudgeCoverageCoherenceTests() []nudgeCoverageCoherenceTest {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	coverageFrom := time.Date(2031, time.February, 2, 9, 30, 0, 0, time.UTC)
	ready := NudgeInputHealth{Status: NudgeInputStatusOK, AsOf: asOf}
	unreviewed := NudgeInputHealth{
		Status: NudgeInputStatusUnapproved,
		Reason: NudgeHealthReasonCutoverReviewRequired,
		AsOf:   asOf,
	}
	unavailable := NudgeInputHealth{
		Status: NudgeInputStatusUnavailable,
		Reason: NudgeHealthReasonCoverageUnavailable,
		AsOf:   asOf,
	}
	return []nudgeCoverageCoherenceTest{
		{
			name:             "missing coverage cannot be ready",
			confirmedFlow:    ready,
			wantMarshalError: true,
		},
		{
			name:          "missing coverage with unavailable source",
			confirmedFlow: unavailable,
		},
		{
			name: "valid unreviewed coverage",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom:              coverageFrom,
				PreCutoverFlowsUnreviewed: true,
			},
			confirmedFlow: unreviewed,
		},
		{
			name: "unreviewed coverage cannot be ready",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom:              coverageFrom,
				PreCutoverFlowsUnreviewed: true,
			},
			confirmedFlow:    ready,
			wantMarshalError: true,
		},
		{
			name: "unreviewed coverage cannot be unavailable",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom:              coverageFrom,
				PreCutoverFlowsUnreviewed: true,
			},
			confirmedFlow:    unavailable,
			wantMarshalError: true,
		},
		{
			name: "unreviewed coverage rejects normalized invalid health",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom:              coverageFrom,
				PreCutoverFlowsUnreviewed: true,
			},
			confirmedFlow: NudgeInputHealth{
				Status: NudgeInputStatusUnapproved,
				Reason: "SYNTHETIC_ARBITRARY_REASON",
				AsOf:   asOf,
			},
			wantMarshalError: true,
		},
		{
			name: "reviewed coverage cannot require cutover review",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom: coverageFrom,
			},
			confirmedFlow:    unreviewed,
			wantMarshalError: true,
		},
		{
			name: "valid reviewed coverage is clean",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom: coverageFrom,
			},
			confirmedFlow:  ready,
			wantCleanEmpty: true,
		},
		{
			name: "reviewed coverage may have truthful non-ready health",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom: coverageFrom,
			},
			confirmedFlow: unavailable,
		},
		{
			name:             "coverage timestamp is required",
			coverage:         &NudgeConfirmedFlowCoverage{},
			confirmedFlow:    ready,
			wantMarshalError: true,
		},
		{
			name: "coverage cannot be after snapshot",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom: asOf.Add(time.Nanosecond),
			},
			confirmedFlow:    ready,
			wantMarshalError: true,
		},
		{
			name: "coverage cannot be newer than source health",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom: coverageFrom,
			},
			confirmedFlow: NudgeInputHealth{
				Status: NudgeInputStatusOK,
				AsOf:   coverageFrom.Add(-time.Nanosecond),
			},
			wantMarshalError: true,
		},
		{
			name: "coverage may equal source health timestamp",
			coverage: &NudgeConfirmedFlowCoverage{
				CoverageFrom: coverageFrom,
			},
			confirmedFlow: NudgeInputHealth{
				Status: NudgeInputStatusOK,
				AsOf:   coverageFrom,
			},
			wantCleanEmpty: true,
		},
	}
}

func nudgeCoverageTestSnapshot(tt nudgeCoverageCoherenceTest) NudgesSnapshotResult {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	health := allReadyNudgeSourceHealth(asOf)
	health.ConfirmedFlow = tt.confirmedFlow
	return NudgesSnapshotResult{
		AsOf:                  asOf,
		SourceHealth:          health,
		ConfirmedFlowCoverage: tt.coverage,
	}
}

func TestNudgesSnapshotConfirmedFlowCoverageRoundTrip(t *testing.T) {
	coverageFrom := time.Date(2031, time.February, 2, 9, 30, 0, 0, time.UTC)
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	health := allReadyNudgeSourceHealth(asOf)
	health.ConfirmedFlow = NudgeInputHealth{
		Status: NudgeInputStatusUnapproved,
		Reason: NudgeHealthReasonCutoverReviewRequired,
		AsOf:   asOf,
	}
	result := NudgesSnapshotResult{
		AsOf:         asOf,
		SourceHealth: health,
		ConfirmedFlowCoverage: &NudgeConfirmedFlowCoverage{
			CoverageFrom:              coverageFrom,
			PreCutoverFlowsUnreviewed: true,
		},
	}

	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded NudgesSnapshotResult
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.ConfirmedFlowCoverage == nil {
		t.Fatal("confirmed_flow_coverage is absent after round trip")
	}
	if !decoded.ConfirmedFlowCoverage.CoverageFrom.Equal(coverageFrom) {
		t.Fatalf("coverage_from = %v, want %v", decoded.ConfirmedFlowCoverage.CoverageFrom, coverageFrom)
	}
	if !decoded.ConfirmedFlowCoverage.PreCutoverFlowsUnreviewed {
		t.Fatal("pre_cutover_flows_unreviewed = false, want true")
	}
}

func TestNudgesSnapshotConfirmedFlowCoverageMayBeAbsent(t *testing.T) {
	wire, err := json.Marshal(NudgesSnapshotResult{
		AsOf: time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(wire), "confirmed_flow_coverage") {
		t.Fatalf("unestablished coverage should be absent: %s", wire)
	}
}

func TestNudgeConfirmedFlowCoverageHasNoPrivateExtensionPath(t *testing.T) {
	typeOfCoverage := reflect.TypeFor[NudgeConfirmedFlowCoverage]()
	if typeOfCoverage.NumField() != 2 {
		t.Fatalf("NudgeConfirmedFlowCoverage has %d fields, want exactly 2", typeOfCoverage.NumField())
	}

	const hostileSentinel = "SYNTHETIC_PRIVATE_SENTINEL"
	const hostileJSON = `{
		"coverage_from":"2031-02-02T09:30:00Z",
		"pre_cutover_flows_unreviewed":true,
		"report_id":"SYNTHETIC_PRIVATE_SENTINEL",
		"note":"SYNTHETIC_PRIVATE_SENTINEL",
		"arbitrary_private_data":"SYNTHETIC_PRIVATE_SENTINEL"
	}`
	var coverage NudgeConfirmedFlowCoverage
	if err := json.Unmarshal([]byte(hostileJSON), &coverage); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	health := allReadyNudgeSourceHealth(asOf)
	health.ConfirmedFlow = NudgeInputHealth{
		Status: NudgeInputStatusUnapproved,
		Reason: NudgeHealthReasonCutoverReviewRequired,
		AsOf:   asOf,
	}
	wire, err := json.Marshal(NudgesSnapshotResult{
		AsOf:                  asOf,
		SourceHealth:          health,
		ConfirmedFlowCoverage: &coverage,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(wire), hostileSentinel) {
		t.Fatalf("private sentinel leaked onto typed wire: %s", wire)
	}
}

func TestNudgeCutoverReviewRequiredHealthRemainsNonReady(t *testing.T) {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	health := allReadyNudgeSourceHealth(asOf)
	health.ConfirmedFlow = NudgeInputHealth{
		Status: NudgeInputStatusUnapproved,
		Reason: NudgeHealthReasonCutoverReviewRequired,
		AsOf:   asOf,
	}

	normalized := NormalizeNudgeSourceHealth(health, 0)
	if normalized.ConfirmedFlow != health.ConfirmedFlow {
		t.Fatalf("confirmed-flow health = %+v, want valid pair preserved as %+v", normalized.ConfirmedFlow, health.ConfirmedFlow)
	}
	if normalized.Aggregate != NudgeAggregateSuppressed {
		t.Fatalf("aggregate = %q, want %q", normalized.Aggregate, NudgeAggregateSuppressed)
	}
}

func TestNudgeCutoverReviewRequiredHealthRejectsArbitraryReason(t *testing.T) {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	health := allReadyNudgeSourceHealth(asOf)
	health.ConfirmedFlow = NudgeInputHealth{
		Status: NudgeInputStatusUnapproved,
		Reason: "SYNTHETIC_ARBITRARY_REASON",
		AsOf:   asOf,
	}

	normalized := NormalizeNudgeSourceHealth(health, 1)
	if normalized.ConfirmedFlow.Status != NudgeInputStatusError || normalized.ConfirmedFlow.Reason != NudgeHealthReasonInvalid {
		t.Fatalf("confirmed-flow health = %+v, want fail-closed invalid health", normalized.ConfirmedFlow)
	}
	if normalized.Aggregate == NudgeAggregateReady {
		t.Fatalf("aggregate = %q, want non-ready", normalized.Aggregate)
	}
}

func TestNudgeCutoverReviewRequiredHealthRejectsWrongSource(t *testing.T) {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	cutoverReview := NudgeInputHealth{
		Status: NudgeInputStatusUnapproved,
		Reason: NudgeHealthReasonCutoverReviewRequired,
		AsOf:   asOf,
	}
	tests := []struct {
		name string
		set  func(*NudgeSourceHealth)
		get  func(NudgeSourceHealth) NudgeInputHealth
	}{
		{
			name: "policy",
			set:  func(health *NudgeSourceHealth) { health.Policy = cutoverReview },
			get:  func(health NudgeSourceHealth) NudgeInputHealth { return health.Policy },
		},
		{
			name: "reconciliation",
			set:  func(health *NudgeSourceHealth) { health.Reconciliation = cutoverReview },
			get:  func(health NudgeSourceHealth) NudgeInputHealth { return health.Reconciliation },
		},
		{
			name: "capital",
			set:  func(health *NudgeSourceHealth) { health.Capital = cutoverReview },
			get:  func(health NudgeSourceHealth) NudgeInputHealth { return health.Capital },
		},
		{
			name: "pins",
			set:  func(health *NudgeSourceHealth) { health.Pins = cutoverReview },
			get:  func(health NudgeSourceHealth) NudgeInputHealth { return health.Pins },
		},
		{
			name: "cadence",
			set:  func(health *NudgeSourceHealth) { health.Cadence = cutoverReview },
			get:  func(health NudgeSourceHealth) NudgeInputHealth { return health.Cadence },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := allReadyNudgeSourceHealth(asOf)
			tt.set(&health)
			normalized := NormalizeNudgeSourceHealth(health, 0)
			got := tt.get(normalized)
			if got.Status != NudgeInputStatusError || got.Reason != NudgeHealthReasonInvalid {
				t.Fatalf("normalized health = %+v, want fail-closed invalid health", got)
			}
			if normalized.Aggregate == NudgeAggregateReady {
				t.Fatalf("aggregate = %q, want non-ready", normalized.Aggregate)
			}
		})
	}
}

func allReadyNudgeSourceHealth(asOf time.Time) NudgeSourceHealth {
	ready := NudgeInputHealth{Status: NudgeInputStatusOK, AsOf: asOf}
	return NudgeSourceHealth{
		Policy:         ready,
		Reconciliation: ready,
		Capital:        ready,
		Pins:           ready,
		Cadence:        ready,
		ConfirmedFlow:  ready,
	}
}
