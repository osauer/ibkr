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

func TestAlertStatusMethodIsSingleAuthority(t *testing.T) {
	if MethodAlertStatus != "alerts.status" {
		t.Fatalf("method=%q", MethodAlertStatus)
	}
	if got := (AlertStatusParams{}); got != (AlertStatusParams{}) {
		t.Fatal("alert status params unexpectedly carry caller policy")
	}
}
