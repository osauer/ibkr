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

func TestRelayRoutePersistsAndFiltersByRemoteURLAndExpiry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)
	route := RelayRoute{
		RemoteURL:      "https://remote.example",
		RouteID:        "r_route",
		ConnectorToken: "tok_route",
		PublicURL:      "https://remote.example",
		ConnectorURL:   "wss://remote.example/api/connect?route_id=r_route",
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.SetRelayRoute(route); err != nil {
		t.Fatalf("SetRelayRoute: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.RelayRoute("https://remote.example", now)
	if !ok {
		t.Fatalf("RelayRoute not returned")
	}
	if got.RouteID != route.RouteID || got.ConnectorToken != route.ConnectorToken || got.UpdatedAt.IsZero() {
		t.Fatalf("RelayRoute = %#v, want persisted route/token with UpdatedAt", got)
	}
	if _, ok := reopened.RelayRoute("https://other.example", now); ok {
		t.Fatalf("RelayRoute returned for a different remote URL")
	}
	if _, ok := reopened.RelayRoute("https://remote.example", now.Add(2*time.Hour)); ok {
		t.Fatalf("RelayRoute returned after expiry")
	}
}
