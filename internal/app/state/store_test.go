package state

import (
	"testing"
	"time"
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
