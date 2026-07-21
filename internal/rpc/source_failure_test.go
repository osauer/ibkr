package rpc

import "testing"

func TestValidSourceFailureRejectsFreeText(t *testing.T) {
	t.Parallel()
	if !ValidSourceFailure(&SourceFailure{Code: SourceFailureTimeout, Stage: SourceFailureStageFTPControlConnect, Retryable: true}) {
		t.Fatal("allowlisted source failure rejected")
	}
	for _, failure := range []*SourceFailure{
		{Code: "dial tcp 10.0.0.1:21: i/o timeout", Stage: SourceFailureStageFTPControlConnect},
		{Code: SourceFailureTimeout, Stage: "USER shortstock rejected: private upstream text"},
	} {
		if ValidSourceFailure(failure) {
			t.Fatalf("free-text source failure accepted: %+v", failure)
		}
	}
}

func TestCompactSourceHealthClonesTypedFailure(t *testing.T) {
	t.Parallel()
	failure := &SourceFailure{Code: SourceFailureInvalidPayload, Stage: SourceFailureStageNasdaqSchema}
	compact := compactSourceHealth([]SourceHealth{{Source: "earnings", Status: SourceStatusDegraded, LastFailure: failure}})
	if len(compact) != 1 || compact[0].LastFailure == nil || compact[0].LastFailure.Code != SourceFailureInvalidPayload {
		t.Fatalf("compact failure missing: %+v", compact)
	}
	compact[0].LastFailure.Code = SourceFailureTimeout
	if failure.Code != SourceFailureInvalidPayload {
		t.Fatal("compact source health aliased the full result's failure pointer")
	}
}
