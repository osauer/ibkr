package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestPresentAlertCandidateCoversTypedKindsStatesSeveritiesAndDestinations(t *testing.T) {
	now := time.Date(2026, time.July, 20, 21, 0, 0, 0, time.UTC)
	kinds := []rpc.AlertKind{
		rpc.AlertKindMarketState, rpc.AlertKindPortfolioRisk, rpc.AlertKindMarginSafety,
		rpc.AlertKindDrawdown, rpc.AlertKindProtectionGap, rpc.AlertKindOrderIntegrity,
		rpc.AlertKindReconciliationException, rpc.AlertKindGovernance, rpc.AlertKindPolicyDrift,
		rpc.AlertKindDataHealth, rpc.AlertKindDeliveryHealth,
	}
	states := []rpc.AlertEpisodeState{rpc.AlertEpisodeOpen, rpc.AlertEpisodeEscalated, rpc.AlertEpisodeRecovered}
	severities := []rpc.AlertSeverity{rpc.AlertSeverityObserve, rpc.AlertSeverityWatch, rpc.AlertSeverityAct, rpc.AlertSeverityUrgent}
	destinations := []rpc.AlertDestination{rpc.AlertDestinationMonitor, rpc.AlertDestinationAlerts, rpc.AlertDestinationBrief}
	for _, kind := range kinds {
		for _, state := range states {
			for _, severity := range severities {
				for _, destination := range destinations {
					candidate := presentationCandidate(t, now)
					candidate.Kind, candidate.State, candidate.Severity, candidate.Destination = kind, state, severity, destination
					if state == rpc.AlertEpisodeRecovered {
						candidate.EvidenceHealth = rpc.AlertEvidenceCurrent
					}
					got, err := PresentAlertCandidate(candidate)
					if err != nil {
						t.Fatalf("kind=%q state=%q severity=%q destination=%q: %v", kind, state, severity, destination, err)
					}
					if got.Title == "" || got.Body == "" || got.Destination == "" {
						t.Fatalf("incomplete presentation: %+v", got)
					}
					if !strings.Contains(got.Body, "Open "+got.Destination) {
						t.Fatalf("destination copy drift: %+v", got)
					}
				}
			}
		}
	}
}

func TestPresentAlertCandidateRejectsInvalidAndLeaksNoPrivateIdentity(t *testing.T) {
	now := time.Date(2026, time.July, 20, 21, 0, 0, 0, time.UTC)
	candidate := presentationCandidate(t, now)
	private := candidate.EpisodeKey + candidate.OccurrenceKey + candidate.EvidenceFingerprint
	got, err := PresentAlertCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	combined := got.Title + got.Body + got.Destination
	for _, secret := range []string{candidate.EpisodeKey, candidate.OccurrenceKey, candidate.EvidenceFingerprint, private} {
		if secret != "" && strings.Contains(combined, secret) {
			t.Fatalf("presentation leaked private identity %q: %+v", secret, got)
		}
	}

	candidate.Kind = "broker supplied free text"
	if _, err := PresentAlertCandidate(candidate); err == nil {
		t.Fatal("invalid candidate reached presentation")
	}
}

func TestAlertPushPayloadRequiresExplicitPageClassAndSafeActiveState(t *testing.T) {
	now := time.Date(2026, time.July, 20, 21, 30, 0, 0, time.UTC)
	candidate := presentationCandidate(t, now)
	for _, preference := range []rpc.AlertDeliveryPreference{
		rpc.AlertDeliveryUnapproved, rpc.AlertDeliveryRecordOnly, rpc.AlertDeliveryInbox, rpc.AlertDeliveryDigest,
	} {
		candidate.DeliveryPreference = preference
		if _, err := AlertPushPayload(candidate, "alert-0123456789abcdef"); err == nil {
			t.Fatalf("non-page preference %q produced a lock-screen payload", preference)
		}
	}

	candidate.DeliveryPreference = rpc.AlertDeliveryPage
	for _, invalid := range []string{"", "gov-0123456789abcdef", "alert-private", "alert-0123456789ABCDEG"} {
		if _, err := AlertPushPayload(candidate, invalid); err == nil {
			t.Fatalf("invalid display id %q was accepted", invalid)
		}
	}
	payload, err := AlertPushPayload(candidate, "alert-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if payload.DisplayID != "alert-0123456789abcdef" || payload.Destination != string(rpc.AlertDestinationAlerts) || payload.Title == "" || payload.Body == "" {
		t.Fatalf("unsafe or incomplete payload: %+v", payload)
	}
	raw := payload.Title + payload.Body + payload.Kind + payload.Destination + payload.DisplayID
	for _, private := range []string{candidate.EpisodeKey, candidate.OccurrenceKey, candidate.EvidenceFingerprint} {
		if strings.Contains(raw, private) {
			t.Fatalf("payload leaked private identity %q: %+v", private, payload)
		}
	}

	candidate.State = rpc.AlertEpisodeRecovered
	if _, err := AlertPushPayload(candidate, "alert-0123456789abcdef"); err == nil {
		t.Fatal("recovered occurrence produced a lock-screen payload")
	}
}

func presentationCandidate(t *testing.T, now time.Time) rpc.AlertCandidate {
	t.Helper()
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceCanary, rpc.AlertKindPortfolioRisk, "private-test-subject")
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := rpc.BuildAlertOccurrenceKey(episode, "opening-1")
	if err != nil {
		t.Fatal(err)
	}
	return rpc.AlertCandidate{
		EpisodeKey: episode, OccurrenceKey: occurrence,
		EvidenceFingerprint: "sha256:" + strings.Repeat("a", 64),
		Source:              rpc.AlertSourceCanary, Kind: rpc.AlertKindPortfolioRisk,
		State: rpc.AlertEpisodeOpen, Severity: rpc.AlertSeverityWatch,
		DeliveryPreference: rpc.AlertDeliveryUnapproved, EvidenceHealth: rpc.AlertEvidenceCurrent,
		Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: now.Add(-time.Minute),
		StateChangedAt: now.Add(-2 * time.Minute), ObservedAt: now,
	}
}
