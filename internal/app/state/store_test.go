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

func TestRelayRoutePersistsAndFiltersByRemoteURL(t *testing.T) {
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
		ExpiresAt:      now.Add(-time.Hour),
	}
	if err := store.SetRelayRoute(route); err != nil {
		t.Fatalf("SetRelayRoute: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	// The route is returned even past its ExpiresAt: the relay revives a
	// token-matched resume, so a locally expired route must still resume
	// instead of being abandoned for a fresh route id.
	got, ok := reopened.RelayRoute("https://remote.example")
	if !ok {
		t.Fatalf("RelayRoute not returned")
	}
	if got.RouteID != route.RouteID || got.ConnectorToken != route.ConnectorToken || got.UpdatedAt.IsZero() {
		t.Fatalf("RelayRoute = %#v, want persisted route/token with UpdatedAt", got)
	}
	if _, ok := reopened.RelayRoute("https://other.example"); ok {
		t.Fatalf("RelayRoute returned for a different remote URL")
	}
}

func TestPruneDevicesRemovesStaleGrantsAndTheirPushSubscriptions(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	stale := DeviceGrant{ID: "dev-stale", Name: "old", CreatedAt: now.AddDate(0, 0, -40), LastSeenAt: now.AddDate(0, 0, -30)}
	// Freshly paired but never used: activity is the later of created/seen.
	freshUnused := DeviceGrant{ID: "dev-fresh", Name: "new", CreatedAt: now.AddDate(0, 0, -1)}
	active := DeviceGrant{ID: "dev-active", Name: "iPhone", CreatedAt: now.AddDate(0, 0, -60), LastSeenAt: now.AddDate(0, 0, -2)}
	for _, d := range []DeviceGrant{stale, freshUnused, active} {
		if err := store.AddDevice(d); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "s1", DeviceID: "dev-stale", Endpoint: "https://push/stale"}); err != nil {
		t.Fatalf("AddPushSubscription: %v", err)
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "s2", DeviceID: "dev-active", Endpoint: "https://push/active"}); err != nil {
		t.Fatalf("AddPushSubscription: %v", err)
	}

	removed, err := store.PruneDevices(now.AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("PruneDevices: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, ok := store.Device("dev-stale"); ok {
		t.Fatalf("stale device survived the prune")
	}
	for _, id := range []string{"dev-fresh", "dev-active"} {
		if _, ok := store.Device(id); !ok {
			t.Fatalf("device %s should have survived the prune", id)
		}
	}
	subs := store.PushSubscriptions()
	if len(subs) != 1 || subs[0].DeviceID != "dev-active" {
		t.Fatalf("push subscriptions = %#v, want only the active device's", subs)
	}
}

func TestSetRelayRouteKeepsCreatedAtForSameRoute(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	route := RelayRoute{
		RemoteURL:      "https://remote.example",
		RouteID:        "r_route",
		ConnectorToken: "tok_route",
	}
	if err := store.SetRelayRoute(route); err != nil {
		t.Fatalf("SetRelayRoute: %v", err)
	}
	first, _ := store.RelayRoute("https://remote.example")
	if first.CreatedAt.IsZero() {
		t.Fatalf("CreatedAt not stamped on first persist")
	}
	// A route extension re-persists the same route id with a fresh token
	// expiry; the birth time must survive so route age stays observable.
	route.ConnectorToken = "tok_rotated"
	if err := store.SetRelayRoute(route); err != nil {
		t.Fatalf("SetRelayRoute extension: %v", err)
	}
	extended, _ := store.RelayRoute("https://remote.example")
	if !extended.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt changed on extension: %v -> %v", first.CreatedAt, extended.CreatedAt)
	}
	// A different route id is a new route and gets a new birth time.
	fresh := RelayRoute{RemoteURL: "https://remote.example", RouteID: "r_new", ConnectorToken: "tok_new"}
	if err := store.SetRelayRoute(fresh); err != nil {
		t.Fatalf("SetRelayRoute fresh: %v", err)
	}
	got, _ := store.RelayRoute("https://remote.example")
	if got.CreatedAt.Before(first.CreatedAt) {
		t.Fatalf("fresh route CreatedAt %v predates previous route %v", got.CreatedAt, first.CreatedAt)
	}
}
