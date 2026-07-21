package rpc

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNudgesSnapshotParamsExactEmptyObject(t *testing.T) {
	if got := reflect.TypeFor[NudgesSnapshotParams]().NumField(); got != 0 {
		t.Fatalf("NudgesSnapshotParams has %d fields, want empty contract", got)
	}
	raw, err := json.Marshal(NudgesSnapshotParams{})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(raw) != "{}" {
		t.Fatalf("json.Marshal() = %s, want {}", raw)
	}

	for _, valid := range []string{"{}", " { } ", "\n{\n}\t"} {
		var params NudgesSnapshotParams
		if err := json.Unmarshal([]byte(valid), &params); err != nil {
			t.Fatalf("valid empty object %q rejected: %v", valid, err)
		}
		roundTrip, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("round-trip marshal for %q: %v", valid, err)
		}
		if string(roundTrip) != "{}" {
			t.Fatalf("round-trip for %q = %s, want {}", valid, roundTrip)
		}
	}
}

func TestNudgesSnapshotParamsRejectsHostileShapes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"unknown key", `{"unknown":1}`},
		{"case variant key", `{"Unknown":1}`},
		{"duplicate keys", `{"unknown":1,"unknown":2}`},
		{"null", `null`},
		{"array", `[]`},
		{"boolean", `true`},
		{"number", `0`},
		{"string", `""`},
		{"trailing object", `{} {}`},
		{"trailing scalar", `{} true`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params NudgesSnapshotParams
			if err := json.Unmarshal([]byte(tt.raw), &params); err == nil {
				t.Fatalf("hostile snapshot params accepted: %s", tt.raw)
			}
		})
	}
}

func TestNudgesCutoverReviewContractWireShape(t *testing.T) {
	if MethodNudgesCutoverReview != "nudges.cutover_review" {
		t.Fatalf("method = %q, want stable cutover-review method", MethodNudgesCutoverReview)
	}

	paramsType := reflect.TypeFor[NudgesCutoverReviewParams]()
	if paramsType.NumField() != 2 {
		t.Fatalf("NudgesCutoverReviewParams has %d fields, want exactly 2", paramsType.NumField())
	}
	params := NudgesCutoverReviewParams{
		Origin:   NudgeCutoverReviewOriginPairedDevice,
		Evidence: NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("json.Marshal(params) error = %v", err)
	}
	const wantParams = `{"origin":"paired_device","evidence":"paired_device_foreground_render_review"}`
	if string(paramsJSON) != wantParams {
		t.Fatalf("params JSON = %s, want %s", paramsJSON, wantParams)
	}

	reviewedAt := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	coverageFrom := reviewedAt.Add(-24 * time.Hour)
	resultType := reflect.TypeFor[NudgesCutoverReviewResult]()
	if resultType.NumField() != 5 {
		t.Fatalf("NudgesCutoverReviewResult has %d fields, want exactly 5", resultType.NumField())
	}
	result := NudgesCutoverReviewResult{
		OK:              true,
		AlreadyReviewed: true,
		ReviewedAt:      reviewedAt,
		CoverageFrom:    coverageFrom,
		Evidence:        NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal(result) error = %v", err)
	}
	const wantResult = `{"ok":true,"already_reviewed":true,"reviewed_at":"2031-02-03T12:00:00Z","coverage_from":"2031-02-02T12:00:00Z","evidence":"paired_device_foreground_render_review"}`
	if string(resultJSON) != wantResult {
		t.Fatalf("result JSON = %s, want %s", resultJSON, wantResult)
	}

	var decoded NudgesCutoverReviewResult
	if err := json.Unmarshal(resultJSON, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(result) error = %v", err)
	}
	if !reflect.DeepEqual(decoded, result) {
		t.Fatalf("round-trip result = %#v, want %#v", decoded, result)
	}
}

func TestNudgesCutoverReviewRejectsUnknownFieldsAndEnums(t *testing.T) {
	const validOrigin = `"origin":"paired_device"`
	const validEvidence = `"evidence":"paired_device_foreground_render_review"`
	for _, hostile := range []string{
		`"report_id":"PRIVATE"`,
		`"row_id":"PRIVATE"`,
		`"reviewed_at":"2031-02-03T12:00:00Z"`,
		`"note":"PRIVATE"`,
		`"path":"/private/report"`,
		`"fingerprint":"PRIVATE"`,
		`"payload":{"account_id":"PRIVATE"}`,
	} {
		t.Run(hostile, func(t *testing.T) {
			var params NudgesCutoverReviewParams
			raw := []byte("{" + validOrigin + "," + validEvidence + "," + hostile + "}")
			if err := json.Unmarshal(raw, &params); err == nil {
				t.Fatalf("hostile unknown field was accepted: %s", raw)
			}
		})
	}

	for _, params := range []NudgesCutoverReviewParams{
		{Origin: "cli", Evidence: NudgeCutoverReviewEvidencePairedDeviceForegroundRender},
		{Origin: NudgeCutoverReviewOriginPairedDevice, Evidence: "monthly_completion"},
	} {
		if raw, err := json.Marshal(params); err == nil {
			t.Fatalf("invalid params serialized: %s", raw)
		}
	}

	validResult := `{"ok":true,"already_reviewed":false,"reviewed_at":"2031-02-03T12:00:00Z","coverage_from":"2031-02-02T12:00:00Z","evidence":"paired_device_foreground_render_review"}`
	for _, hostile := range []string{
		`"account_id":"PRIVATE"`,
		`"report_id":"PRIVATE"`,
		`"monthly_completed":true`,
		`"broker_authorized":true`,
	} {
		raw := []byte(strings.TrimSuffix(validResult, "}") + "," + hostile + "}")
		var result NudgesCutoverReviewResult
		if err := json.Unmarshal(raw, &result); err == nil {
			t.Fatalf("hostile result field was accepted: %s", raw)
		}
	}
}

func TestNudgesCutoverReviewParamsExactObjectDecode(t *testing.T) {
	const evidence = "paired_device_foreground_render_review"
	valid := []byte(`{"evidence":"` + evidence + `","origin":"paired_device"}`)
	var decoded NudgesCutoverReviewParams
	if err := json.Unmarshal(valid, &decoded); err != nil {
		t.Fatalf("valid reordered params rejected: %v", err)
	}

	tests := []struct {
		name string
		raw  string
	}{
		{"duplicate origin", `{"origin":"paired_device","origin":"paired_device","evidence":"` + evidence + `"}`},
		{"duplicate evidence", `{"origin":"paired_device","evidence":"` + evidence + `","evidence":"` + evidence + `"}`},
		{"case variant origin", `{"Origin":"paired_device","evidence":"` + evidence + `"}`},
		{"case variant evidence", `{"origin":"paired_device","EVIDENCE":"` + evidence + `"}`},
		{"unknown key", `{"origin":"paired_device","evidence":"` + evidence + `","note":"private"}`},
		{"missing origin", `{"evidence":"` + evidence + `"}`},
		{"missing evidence", `{"origin":"paired_device"}`},
		{"null origin", `{"origin":null,"evidence":"` + evidence + `"}`},
		{"null evidence", `{"origin":"paired_device","evidence":null}`},
		{"trailing object", `{"origin":"paired_device","evidence":"` + evidence + `"} {}`},
		{"top-level null", `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params NudgesCutoverReviewParams
			if err := json.Unmarshal([]byte(tt.raw), &params); err == nil {
				t.Fatalf("invalid exact params object accepted: %s", tt.raw)
			}
		})
	}
}

func TestNudgesCutoverReviewResultExactObjectDecode(t *testing.T) {
	const valid = `{"ok":true,"already_reviewed":false,"reviewed_at":"2031-02-03T12:00:00Z","coverage_from":"2031-02-02T12:00:00Z","evidence":"paired_device_foreground_render_review"}`
	tests := []struct {
		name string
		raw  string
	}{
		{"duplicate ok", strings.Replace(valid, `"ok":true`, `"ok":true,"ok":true`, 1)},
		{"duplicate evidence", strings.Replace(valid, `"evidence":"paired_device_foreground_render_review"`, `"evidence":"paired_device_foreground_render_review","evidence":"paired_device_foreground_render_review"`, 1)},
		{"case variant", strings.Replace(valid, `"already_reviewed"`, `"Already_Reviewed"`, 1)},
		{"unknown key", strings.TrimSuffix(valid, "}") + `,"report_id":"private"}`},
		{"missing ok", `{"already_reviewed":false,"reviewed_at":"2031-02-03T12:00:00Z","coverage_from":"2031-02-02T12:00:00Z","evidence":"paired_device_foreground_render_review"}`},
		{"missing already_reviewed", `{"ok":true,"reviewed_at":"2031-02-03T12:00:00Z","coverage_from":"2031-02-02T12:00:00Z","evidence":"paired_device_foreground_render_review"}`},
		{"missing reviewed_at", `{"ok":true,"already_reviewed":false,"coverage_from":"2031-02-02T12:00:00Z","evidence":"paired_device_foreground_render_review"}`},
		{"missing coverage_from", `{"ok":true,"already_reviewed":false,"reviewed_at":"2031-02-03T12:00:00Z","evidence":"paired_device_foreground_render_review"}`},
		{"missing evidence", `{"ok":true,"already_reviewed":false,"reviewed_at":"2031-02-03T12:00:00Z","coverage_from":"2031-02-02T12:00:00Z"}`},
		{"null ok", strings.Replace(valid, `"ok":true`, `"ok":null`, 1)},
		{"null already_reviewed", strings.Replace(valid, `"already_reviewed":false`, `"already_reviewed":null`, 1)},
		{"null reviewed_at", strings.Replace(valid, `"reviewed_at":"2031-02-03T12:00:00Z"`, `"reviewed_at":null`, 1)},
		{"null coverage_from", strings.Replace(valid, `"coverage_from":"2031-02-02T12:00:00Z"`, `"coverage_from":null`, 1)},
		{"null evidence", strings.Replace(valid, `"evidence":"paired_device_foreground_render_review"`, `"evidence":null`, 1)},
		{"trailing value", valid + ` true`},
		{"unsuccessful evidence", strings.Replace(valid, `"ok":true`, `"ok":false`, 1)},
		{"top-level null", `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result NudgesCutoverReviewResult
			if err := json.Unmarshal([]byte(tt.raw), &result); err == nil {
				t.Fatalf("invalid exact result object accepted: %s", tt.raw)
			}
		})
	}
}

func TestNudgesCutoverReviewTimestampAndEvidenceValidation(t *testing.T) {
	reviewedAt := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	valid := NudgesCutoverReviewResult{
		OK: true, ReviewedAt: reviewedAt, CoverageFrom: reviewedAt.Add(-time.Hour),
		Evidence: NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	}
	tests := []struct {
		name   string
		result NudgesCutoverReviewResult
	}{
		{"ok false", func() NudgesCutoverReviewResult { r := valid; r.OK = false; return r }()},
		{"zero reviewed_at", func() NudgesCutoverReviewResult { r := valid; r.ReviewedAt = time.Time{}; return r }()},
		{"zero coverage_from", func() NudgesCutoverReviewResult { r := valid; r.CoverageFrom = time.Time{}; return r }()},
		{"coverage after review", func() NudgesCutoverReviewResult {
			r := valid
			r.CoverageFrom = reviewedAt.Add(time.Nanosecond)
			return r
		}()},
		{"unknown evidence", func() NudgesCutoverReviewResult { r := valid; r.Evidence = "human_attestation"; return r }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if raw, err := json.Marshal(tt.result); err == nil {
				t.Fatalf("invalid result serialized: %s", raw)
			}
		})
	}
}

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

func TestNudgesSnapshotContextCorrelationValidation(t *testing.T) {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	shadow := testNudgeContextCandidate(NudgeKindShadowWouldBlock, NudgeStateObserved, "a", asOf)
	drawdown := testNudgeContextCandidate(NudgeKindDrawdownLatched, NudgeStateOpen, "b", asOf)
	policy := testNudgeContextCandidate(NudgeKindPolicyDrift, NudgeStateOpen, "c", asOf)

	tests := []struct {
		name             string
		candidates       []NudgeCandidate
		context          *NudgeSnapshotContext
		wantMarshalError bool
		wantCleanEmpty   bool
	}{
		{name: "clean empty", wantCleanEmpty: true},
		{name: "empty context rejected", context: &NudgeSnapshotContext{}, wantMarshalError: true},
		{name: "unrelated candidate needs no context", candidates: []NudgeCandidate{policy}},
		{name: "valid shadow", candidates: []NudgeCandidate{shadow}, context: &NudgeSnapshotContext{Shadow: &NudgeShadowSummary{Count: 1}}},
		{name: "valid accumulated shadow", candidates: []NudgeCandidate{shadow}, context: &NudgeSnapshotContext{Shadow: &NudgeShadowSummary{Count: 7}}},
		{name: "valid drawdown unavailable", candidates: []NudgeCandidate{drawdown}, context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock}}},
		{name: "valid drawdown zero", candidates: []NudgeCandidate{drawdown}, context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(0.0)}}},
		{name: "valid drawdown positive", candidates: []NudgeCandidate{drawdown}, context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(31.25)}}},
		{name: "valid combined", candidates: []NudgeCandidate{shadow, drawdown}, context: &NudgeSnapshotContext{Shadow: &NudgeShadowSummary{Count: 2}, Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(42.0)}}},
		{name: "missing shadow summary", candidates: []NudgeCandidate{shadow}, wantMarshalError: true},
		{name: "orphan shadow summary", context: &NudgeSnapshotContext{Shadow: &NudgeShadowSummary{Count: 1}}, wantMarshalError: true},
		{name: "zero shadow count", candidates: []NudgeCandidate{shadow}, context: &NudgeSnapshotContext{Shadow: &NudgeShadowSummary{}}, wantMarshalError: true},
		{name: "negative shadow count", candidates: []NudgeCandidate{shadow}, context: &NudgeSnapshotContext{Shadow: &NudgeShadowSummary{Count: -1}}, wantMarshalError: true},
		{name: "duplicate shadow candidates", candidates: []NudgeCandidate{shadow, testNudgeContextCandidate(NudgeKindShadowWouldBlock, NudgeStateObserved, "d", asOf)}, context: &NudgeSnapshotContext{Shadow: &NudgeShadowSummary{Count: 2}}, wantMarshalError: true},
		{name: "missing drawdown summary", candidates: []NudgeCandidate{drawdown}, wantMarshalError: true},
		{name: "orphan drawdown summary", context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(30.0)}}, wantMarshalError: true},
		{name: "duplicate drawdown candidates", candidates: []NudgeCandidate{drawdown, testNudgeContextCandidate(NudgeKindDrawdownLatched, NudgeStateOpen, "e", asOf)}, context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(30.0)}}, wantMarshalError: true},
		{name: "unknown drawdown tier", candidates: []NudgeCandidate{drawdown}, context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: "warn", ConsumedPct: new(30.0)}}, wantMarshalError: true},
		{name: "negative drawdown percentage", candidates: []NudgeCandidate{drawdown}, context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(-0.01)}}, wantMarshalError: true},
		{name: "NaN drawdown percentage", candidates: []NudgeCandidate{drawdown}, context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(math.NaN())}}, wantMarshalError: true},
		{name: "positive infinite drawdown percentage", candidates: []NudgeCandidate{drawdown}, context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(math.Inf(1))}}, wantMarshalError: true},
		{name: "negative infinite drawdown percentage", candidates: []NudgeCandidate{drawdown}, context: &NudgeSnapshotContext{Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(math.Inf(-1))}}, wantMarshalError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := testNudgeContextSnapshot(asOf, tt.candidates, tt.context)
			_, err := json.Marshal(result)
			if got := err != nil; got != tt.wantMarshalError {
				t.Fatalf("json.Marshal() error = %v, want error = %v", err, tt.wantMarshalError)
			}
			if got := result.IsCleanEmpty(); got != tt.wantCleanEmpty {
				t.Fatalf("IsCleanEmpty() = %v, want %v", got, tt.wantCleanEmpty)
			}
		})
	}
}

func TestNudgesSnapshotCandidateTimeCoherence(t *testing.T) {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	before := asOf.Add(-time.Hour)
	after := asOf.Add(time.Hour)
	candidate := func(kind, state, fingerprintDigit string, occurredAt, dueAt time.Time) NudgeCandidate {
		value := testNudgeContextCandidate(kind, state, fingerprintDigit, occurredAt)
		value.DueAt = dueAt
		return value
	}
	tests := []struct {
		name      string
		candidate NudgeCandidate
		wantValid bool
	}{
		{"occurrence equal snapshot", candidate(NudgeKindPolicyDrift, NudgeStateOpen, "a", asOf, time.Time{}), true},
		{"future occurrence", candidate(NudgeKindPolicyDrift, NudgeStateOpen, "b", asOf.Add(time.Nanosecond), time.Time{}), false},
		{"due soon future deadline", candidate(NudgeKindReconcileDue, NudgeStateDueSoon, "c", before, after), true},
		{"due soon deadline equal snapshot", candidate(NudgeKindReconcileDue, NudgeStateDueSoon, "d", before, asOf), true},
		{"due soon past deadline", candidate(NudgeKindReconcileDue, NudgeStateDueSoon, "e", before.Add(-time.Hour), before), false},
		{"overdue past deadline", candidate(NudgeKindReconcileDue, NudgeStateOverdue, "f", before, before), true},
		{"overdue deadline equal snapshot", candidate(NudgeKindReconcileDue, NudgeStateOverdue, "1", asOf, asOf), true},
		{"overdue future deadline", candidate(NudgeKindReconcileDue, NudgeStateOverdue, "2", after, after), false},
		{"monthly past deadline", candidate(NudgeKindMonthlyPulse, NudgeStateDue, "3", before, before), true},
		{"monthly deadline equal snapshot", candidate(NudgeKindMonthlyPulse, NudgeStateDue, "4", asOf, asOf), true},
		{"monthly future deadline", candidate(NudgeKindMonthlyPulse, NudgeStateDue, "5", after, after), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := testNudgeContextSnapshot(asOf, []NudgeCandidate{tt.candidate}, nil)
			_, err := json.Marshal(result)
			if got := err == nil; got != tt.wantValid {
				t.Fatalf("json.Marshal() error = %v, want valid = %v", err, tt.wantValid)
			}
			if result.IsCleanEmpty() {
				t.Fatal("snapshot with candidate reported clean empty")
			}
		})
	}
}

func TestNudgesSnapshotDrawdownUnavailableIsJSONNull(t *testing.T) {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	drawdown := testNudgeContextCandidate(NudgeKindDrawdownLatched, NudgeStateOpen, "a", asOf)
	result := testNudgeContextSnapshot(asOf, []NudgeCandidate{drawdown}, &NudgeSnapshotContext{
		Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock},
	})
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !strings.Contains(string(raw), `"drawdown":{"tier":"block","consumed_pct":null}`) {
		t.Fatalf("unavailable consumed_pct was not explicit JSON null: %s", raw)
	}
	var decoded NudgesSnapshotResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.Context == nil || decoded.Context.Drawdown == nil || decoded.Context.Drawdown.ConsumedPct != nil {
		t.Fatalf("unavailable consumed_pct did not remain nil: %#v", decoded.Context)
	}
}

func TestNudgesSnapshotContextExactJSONShape(t *testing.T) {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	result := testNudgeContextSnapshot(asOf, []NudgeCandidate{
		testNudgeContextCandidate(NudgeKindShadowWouldBlock, NudgeStateObserved, "a", asOf),
		testNudgeContextCandidate(NudgeKindDrawdownLatched, NudgeStateOpen, "b", asOf),
	}, &NudgeSnapshotContext{
		Shadow:   &NudgeShadowSummary{Count: 3},
		Drawdown: &NudgeDrawdownSummary{Tier: NudgeDrawdownTierBlock, ConsumedPct: new(37.5)},
	})

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("json.Unmarshal(snapshot) error = %v", err)
	}
	contextFields := decodeJSONFields(t, wire["context"])
	if got, want := sortedMapKeys(contextFields), []string{"drawdown", "shadow"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("context fields = %v, want %v", got, want)
	}
	shadowFields := decodeJSONFields(t, contextFields["shadow"])
	if got, want := sortedMapKeys(shadowFields), []string{"count"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shadow fields = %v, want %v", got, want)
	}
	drawdownFields := decodeJSONFields(t, contextFields["drawdown"])
	if got, want := sortedMapKeys(drawdownFields), []string{"consumed_pct", "tier"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("drawdown fields = %v, want %v", got, want)
	}

	var candidateWire []map[string]json.RawMessage
	if err := json.Unmarshal(wire["candidates"], &candidateWire); err != nil {
		t.Fatalf("json.Unmarshal(candidates) error = %v", err)
	}
	for i, candidate := range candidateWire {
		for _, forbidden := range []string{"count", "tier", "consumed_pct"} {
			if _, ok := candidate[forbidden]; ok {
				t.Fatalf("candidate %d widened with %q", i, forbidden)
			}
		}
	}
}

func TestNudgesSnapshotContextPrivacyAndCandidateCanonicalization(t *testing.T) {
	if got := reflect.TypeFor[NudgeSnapshotContext]().NumField(); got != 2 {
		t.Fatalf("NudgeSnapshotContext has %d fields, want exactly 2", got)
	}
	if got := reflect.TypeFor[NudgeShadowSummary]().NumField(); got != 1 {
		t.Fatalf("NudgeShadowSummary has %d fields, want exactly 1", got)
	}
	if got := reflect.TypeFor[NudgeDrawdownSummary]().NumField(); got != 2 {
		t.Fatalf("NudgeDrawdownSummary has %d fields, want exactly 2", got)
	}

	const sentinel = "SYNTHETIC_PRIVATE_SENTINEL"
	var context NudgeSnapshotContext
	const hostileContext = `{
		"shadow":{"count":4,"note":"SYNTHETIC_PRIVATE_SENTINEL","report_id":"SYNTHETIC_PRIVATE_SENTINEL"},
		"drawdown":{"tier":"block","consumed_pct":35.5,"balance":"SYNTHETIC_PRIVATE_SENTINEL","account_id":"SYNTHETIC_PRIVATE_SENTINEL"},
		"path":"SYNTHETIC_PRIVATE_SENTINEL","fingerprint":"SYNTHETIC_PRIVATE_SENTINEL","tokens":"SYNTHETIC_PRIVATE_SENTINEL"
	}`
	if err := json.Unmarshal([]byte(hostileContext), &context); err != nil {
		t.Fatalf("json.Unmarshal(context) error = %v", err)
	}
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	shadow := testNudgeContextCandidate(NudgeKindShadowWouldBlock, NudgeStateObserved, "a", asOf)
	drawdown := testNudgeContextCandidate(NudgeKindDrawdownLatched, NudgeStateOpen, "b", asOf)
	shadow.Title, shadow.Body, shadow.Severity, shadow.Destination = sentinel, sentinel, sentinel, sentinel
	drawdown.Title, drawdown.Body, drawdown.Severity, drawdown.Destination = sentinel, sentinel, sentinel, sentinel
	raw, err := json.Marshal(testNudgeContextSnapshot(asOf, []NudgeCandidate{shadow, drawdown}, &context))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Fatalf("private/caller-authored sentinel leaked onto snapshot wire: %s", raw)
	}
	if !strings.Contains(string(raw), `"title":"A planned trade would have been blocked"`) || !strings.Contains(string(raw), `"title":"The drawdown warning remains active"`) {
		t.Fatalf("candidate display fields were not canonicalized: %s", raw)
	}
}

func TestNudgesSnapshotInvalidCandidateFailsCleanEmptyClosed(t *testing.T) {
	asOf := time.Date(2031, time.February, 3, 12, 0, 0, 0, time.UTC)
	invalid := testNudgeContextCandidate(NudgeKindPolicyDrift, NudgeStateOpen, "a", asOf)
	invalid.Fingerprint = "private-caller-value"
	result := testNudgeContextSnapshot(asOf, []NudgeCandidate{invalid}, nil)
	if _, err := json.Marshal(result); err == nil {
		t.Fatal("invalid candidate serialized")
	}
	if result.IsCleanEmpty() {
		t.Fatal("invalid candidate reported clean empty")
	}
}

func testNudgeContextSnapshot(asOf time.Time, candidates []NudgeCandidate, context *NudgeSnapshotContext) NudgesSnapshotResult {
	return NudgesSnapshotResult{
		AsOf:                  asOf,
		Candidates:            candidates,
		SourceHealth:          allReadyNudgeSourceHealth(asOf),
		ConfirmedFlowCoverage: &NudgeConfirmedFlowCoverage{CoverageFrom: asOf.Add(-time.Hour)},
		Context:               context,
	}
}

func testNudgeContextCandidate(kind, state, fingerprintDigit string, occurredAt time.Time) NudgeCandidate {
	return NudgeCandidate{
		Fingerprint: "sha256:" + strings.Repeat(fingerprintDigit, 64),
		Kind:        kind,
		State:       state,
		OccurredAt:  occurredAt,
	}
}

func decodeJSONFields(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("json.Unmarshal(fields) error = %v", err)
	}
	return fields
}

func sortedMapKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
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
