package state

import (
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/app/orderreview"
)

func TestClearAlertHistoryRemovesRecordedAlerts(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.RecordAlert(AlertRecord{
		ID:          "alert-1",
		Fingerprint: "fp-1",
		Title:       "canary",
		Body:        "watch",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordAlert: %v", err)
	}
	if got := store.AlertHistory(10); len(got) != 1 {
		t.Fatalf("AlertHistory len=%d, want 1", len(got))
	}
	if err := store.ClearAlertHistory(); err != nil {
		t.Fatalf("ClearAlertHistory: %v", err)
	}
	if got := store.AlertHistory(10); len(got) != 0 {
		t.Fatalf("AlertHistory len=%d, want 0", len(got))
	}
}

func TestOrderReviewSetPersistence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	set := orderreview.Set{
		ID:                "ors-1",
		Revision:          "rev-1",
		SourceKind:        orderreview.SourceKindRiskPlan,
		Intent:            orderreview.IntentMitigateRisk,
		PlanID:            "plan-1",
		CanaryFingerprint: "fp-1",
		Rows: []orderreview.Row{{
			RowID:            "candidate-1:1",
			CandidateID:      "candidate-1",
			ProposedQuantity: 3,
			EditableQuantity: 2,
			Included:         true,
		}},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := store.RecordOrderReviewSet(set); err != nil {
		t.Fatalf("RecordOrderReviewSet: %v", err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.OrderReviewSet("ors-1")
	if !ok {
		t.Fatalf("OrderReviewSet did not find persisted set")
	}
	if got.CanaryFingerprint != "fp-1" || len(got.Rows) != 1 || got.Rows[0].EditableQuantity != 2 {
		t.Fatalf("unexpected persisted set: %#v", got)
	}
}
