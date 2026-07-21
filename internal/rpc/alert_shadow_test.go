package rpc

import "testing"

func TestAlertCandidatesMethodIsStableAndParamsAreEmpty(t *testing.T) {
	if MethodAlertCandidates != "alerts.candidates" {
		t.Fatalf("method=%q", MethodAlertCandidates)
	}
	if got := (AlertCandidatesParams{}); got != (AlertCandidatesParams{}) {
		t.Fatal("alert candidate params unexpectedly carry caller policy")
	}
}
