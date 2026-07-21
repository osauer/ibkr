package rpc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReconAutomationReadParamsAreExactEmptyObjects(t *testing.T) {
	for _, tt := range []struct {
		name string
		new  func() any
	}{
		{name: "status", new: func() any { return &ReconStatusParams{} }},
		{name: "check", new: func() any { return &ReconCheckParams{} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if raw, err := json.Marshal(tt.new()); err != nil || string(raw) != `{}` {
				t.Fatalf("marshal = %s err=%v, want exact {}", raw, err)
			}
			if err := json.Unmarshal([]byte(`{}`), tt.new()); err != nil {
				t.Fatalf("empty object rejected: %v", err)
			}
			for _, hostile := range []string{
				`{"refresh":true}`,
				`{"report_id":"PRIVATE"}`,
				`{"origin":"agent"}`,
				`{"trading":true}`,
			} {
				if err := json.Unmarshal([]byte(hostile), tt.new()); err == nil {
					t.Fatalf("unknown action field was accepted: %s", hostile)
				}
			}
		})
	}
}

func TestValidateReconAutomationStatusAcceptsCoherentStates(t *testing.T) {
	now := time.Date(2026, 7, 21, 7, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		status ReconAutomationStatus
	}{
		{
			name: "current report and completed evaluation",
			status: ReconAutomationStatus{
				Report:     ReconFetchStatus{State: ReconReportStateCurrent, CoverageTo: now, LastSuccess: now},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateComplete},
			},
		},
		{
			name: "broker report not ready with automatic retry",
			status: ReconAutomationStatus{
				Report: ReconFetchStatus{
					State: ReconReportStateRetryScheduled, Reason: ReconReportReasonReportNotReady,
					LastAttempt: now, NextAttempt: now.Add(30 * time.Minute), RetryAutomatic: true,
				},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateWaiting, Reason: ReconEvaluationReasonReportPending},
			},
		},
		{
			name: "report checking and evaluation checking",
			status: ReconAutomationStatus{
				Report:     ReconFetchStatus{State: ReconReportStateChecking, Busy: true},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateChecking, Reason: ReconEvaluationReasonReportPending},
			},
		},
		{
			name: "credentials need user action",
			status: ReconAutomationStatus{
				Report:     ReconFetchStatus{State: ReconReportStateActionRequired, Reason: ReconReportReasonTokenExpired},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateWaiting, Reason: ReconEvaluationReasonReportPending},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateReconAutomationStatus(tt.status); err != nil {
				t.Fatalf("coherent status rejected: %v", err)
			}
		})
	}
}

func TestValidateReconAutomationStatusRejectsIncoherentStates(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status ReconAutomationStatus
	}{
		{
			name: "unknown report state",
			status: ReconAutomationStatus{
				Report:     ReconFetchStatus{State: "mystery"},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateWaiting, Reason: ReconEvaluationReasonReportPending},
			},
		},
		{
			name: "unknown evaluation reason",
			status: ReconAutomationStatus{
				Report:     ReconFetchStatus{State: ReconReportStateCurrent},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateFailed, Reason: "mystery"},
			},
		},
		{
			name: "checking without busy flag",
			status: ReconAutomationStatus{
				Report:     ReconFetchStatus{State: ReconReportStateChecking},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateChecking, Reason: ReconEvaluationReasonReportPending},
			},
		},
		{
			name: "busy outside checking",
			status: ReconAutomationStatus{
				Report:     ReconFetchStatus{State: ReconReportStateDue, Reason: ReconReportReasonCoveragePending, Busy: true},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateWaiting, Reason: ReconEvaluationReasonReportPending},
			},
		},
		{
			name: "current report with failure reason",
			status: ReconAutomationStatus{
				Report:     ReconFetchStatus{State: ReconReportStateCurrent, Reason: ReconReportReasonNetworkUnavailable},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateWaiting, Reason: ReconEvaluationReasonAccountValuePending},
			},
		},
		{
			name: "completed evaluation without current report",
			status: ReconAutomationStatus{
				Report:     ReconFetchStatus{State: ReconReportStateRetryScheduled, Reason: ReconReportReasonReportNotReady},
				Evaluation: ReconEvaluationStatus{State: ReconEvaluationStateComplete},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateReconAutomationStatus(tt.status); err == nil {
				t.Fatal("incoherent status was accepted")
			}
		})
	}
}

func TestReconCheckResultUsesTypedStrictReceipt(t *testing.T) {
	status := ReconAutomationStatus{
		Report: ReconFetchStatus{State: ReconReportStateChecking, Busy: true},
		Evaluation: ReconEvaluationStatus{
			State: ReconEvaluationStateChecking, Reason: ReconEvaluationReasonReportPending,
		},
	}
	for _, outcome := range []string{
		ReconCheckOutcomeStarted,
		ReconCheckOutcomeAlreadyChecking,
		ReconCheckOutcomeCooldown,
		ReconCheckOutcomeActionRequired,
	} {
		result := ReconCheckResult{Outcome: outcome, Status: status}
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("outcome %q marshal: %v", outcome, err)
		}
		var decoded ReconCheckResult
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("outcome %q round trip: %v", outcome, err)
		}
		if decoded.Outcome != outcome {
			t.Fatalf("outcome round trip = %q, want %q", decoded.Outcome, outcome)
		}
	}

	if _, err := json.Marshal(ReconCheckResult{Outcome: "started_and_signed_off", Status: status}); err == nil {
		t.Fatal("unknown check outcome serialized")
	}
	valid, err := json.Marshal(ReconCheckResult{Outcome: ReconCheckOutcomeStarted, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	hostile := []byte(strings.TrimSuffix(string(valid), "}") + `,"account_id":"PRIVATE"}`)
	var decoded ReconCheckResult
	if err := json.Unmarshal(hostile, &decoded); err == nil {
		t.Fatal("unknown receipt field was accepted")
	}
}
