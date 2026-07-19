package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestGovernanceReceiptPersistsPerTargetAcrossRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	occ, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{
		Fingerprint: "sha256:" + strings.Repeat("a", 64), Kind: "policy_drift", State: "open", Severity: "act",
		Title: "Policy pins need review", Body: "Review the policy pin status.", Destination: "alerts", OccurredAt: now,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	targetOne := GovernanceTargetRef("device-one", "subscription-one")
	targetTwo := GovernanceTargetRef("device-two", "subscription-two")
	if err := store.RecordGovernanceAttempt(GovernanceAttempt{OccurrenceID: occ.DisplayID, TargetRef: targetOne, ReceiptKey: GovernanceReceiptKey(occ.Fingerprint, targetOne), At: now, Class: GovernanceTransportAccepted}, true); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.HasGovernanceReceipt(GovernanceReceiptKey(occ.Fingerprint, targetOne)) {
		t.Fatal("accepted target receipt did not survive restart")
	}
	if reopened.HasGovernanceReceipt(GovernanceReceiptKey(occ.Fingerprint, targetTwo)) {
		t.Fatal("one target receipt suppressed a different target")
	}
	view := reopened.Governance(now)
	if len(view.Attempts) != 1 || view.Attempts[0].TargetRef != targetOne || strings.Contains(view.Attempts[0].TargetRef, "device-one") {
		t.Fatalf("attempt view=%+v, want one opaque per-target receipt", view.Attempts)
	}
}

func TestGovernanceRetentionNeverEvictsBindingTruthAndFailsLoudOnOverflow(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.governanceMaxAttempts = 2
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	for i := range 2 {
		err := store.RecordGovernanceAttempt(GovernanceAttempt{
			OccurrenceID: "gov-active", TargetRef: GovernanceTargetRef("d", string(rune('a'+i))),
			ReceiptKey: GovernanceReceiptKey("active", string(rune('a'+i))), At: now, Class: GovernanceTransportTimeoutRetry,
		}, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	err = store.RecordGovernanceAttempt(GovernanceAttempt{OccurrenceID: "gov-active", TargetRef: "third", ReceiptKey: "third", At: now, Class: GovernanceTransportTimeoutRetry}, false)
	if !errors.Is(err, ErrGovernanceOverflow) {
		t.Fatalf("overflow err=%v, want ErrGovernanceOverflow", err)
	}
	view := store.Governance(now)
	if view.DeliveryHealth.State != GovernanceDeliveryOverflow || view.DeliveryHealth.Class != GovernanceTransportOverflow || len(view.Attempts) != 2 || view.AttemptTotals.CumulativeAttempts != 2 || view.HealthTotals.Overflows != 1 {
		t.Fatalf("view=%+v, want fail-loud overflow without eviction", view)
	}
}

func TestGovernanceResolvedDetailRetainsNinetyDaysThenCompacts(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	occ, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "old", Kind: "policy_drift", DisplayID: "gov-old", OccurredAt: now.AddDate(0, 0, -100)}, now.AddDate(0, 0, -100))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGovernanceAttempt(GovernanceAttempt{OccurrenceID: occ.DisplayID, ReceiptKey: "old-receipt", TargetRef: "target", At: now.AddDate(0, 0, -100), Class: GovernanceTransportAccepted}, true); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, now.AddDate(0, 0, -99)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactGovernance(now); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now)
	if len(view.Occurrences) != 0 || len(view.Attempts) != 0 || store.HasGovernanceReceipt("old-receipt") || view.AttemptTotals.Accepted != 1 {
		t.Fatalf("expired resolved detail survived compaction: %+v", view)
	}
}

func TestGovernanceStateWriteFailureStaysVisibleUntilPersistedRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("blocks state directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir); _ = os.MkdirAll(dir, 0o700) })
	_, _, err = store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "write-failure", Kind: "policy_drift", OccurredAt: now}, now)
	if err == nil {
		t.Fatal("governance write unexpectedly succeeded in read-only state dir")
	}
	if got := store.Governance(now).DeliveryHealth; got.State != GovernanceDeliveryUnavailable || got.Class != GovernanceTransportStateWrite {
		t.Fatalf("failure health=%+v", got)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGovernanceDeliveryHealth(GovernanceDeliveryHealth{State: GovernanceDeliveryHealthy, Class: GovernanceTransportAccepted, UpdatedAt: now.Add(time.Minute), LastAcceptedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now.Add(time.Minute))
	if view.DeliveryHealth.Class != GovernanceTransportAccepted || len(view.Attempts) != 0 || view.AttemptTotals.CumulativeAttempts != 0 || view.HealthTotals.StateFailures != 1 || view.HealthTotals.Recoveries != 1 {
		t.Fatalf("recovery evidence=%+v", view)
	}
}

func TestGovernanceRetryBackoffCapsAtFifteenMinutesWithoutTerminalAttemptLimit(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	occ, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "retry", Kind: "policy_drift", OccurredAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := GovernanceTargetRef("device", "subscription")
	wantBackoff := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, backoff := range wantBackoff {
		reservation, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
		if err != nil || !send {
			t.Fatalf("reserve %d: send=%v err=%v", i, send, err)
		}
		if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportHTTPRetry, false, now); err != nil {
			t.Fatal(err)
		}
		view := store.Governance(now)
		latest := view.Attempts[len(view.Attempts)-1]
		if latest.RetryAt.IsZero() || !latest.RetryAt.Equal(now.Add(backoff)) {
			t.Fatalf("attempt %d retry_at=%v, want %v", i+1, latest.RetryAt, now.Add(backoff))
		}
		now = latest.RetryAt
	}
}

func TestGovernanceTerminalRejectionHasNoRetryObligation(t *testing.T) {
	t.Parallel()
	for _, class := range []string{GovernanceTransportHTTPRejected, GovernanceTransportRejected} {
		t.Run(class, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
			occurrence, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "terminal-" + class, Kind: "policy_drift", OccurredAt: now}, now)
			if err != nil {
				t.Fatal(err)
			}
			target := GovernanceTargetRef("device", "subscription")
			reservation, send, err := store.ReserveGovernanceAttempt(occurrence.DisplayID, target, now)
			if err != nil || !send {
				t.Fatalf("initial reserve send=%v err=%v", send, err)
			}
			if _, err := store.CompleteGovernanceAttempt(reservation.ID, class, false, now); err != nil {
				t.Fatal(err)
			}
			view := store.Governance(now)
			if len(view.Attempts) != 1 || view.Attempts[0].Class != class || !view.Attempts[0].RetryAt.IsZero() || view.AttemptTotals.RetryPending != 0 || view.AttemptTotals.Rejected != 1 {
				t.Fatalf("terminal rejection evidence=%+v totals=%+v", view.Attempts, view.AttemptTotals)
			}
			if retry, send, err := store.ReserveGovernanceAttempt(occurrence.DisplayID, target, now.Add(24*time.Hour)); err != nil || send || retry.Class != class {
				t.Fatalf("terminal rejection retried: reservation=%+v send=%v err=%v", retry, send, err)
			}
		})
	}
}

func TestGovernanceExpiryResolvesEpisodeAndStopsRetry(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	record := GovernanceOccurrence{Fingerprint: "expires", Kind: "policy_drift", OccurredAt: now, ExpiresAt: now.Add(time.Minute)}
	occ, _, err := store.UpsertGovernanceOccurrence(record, now)
	if err != nil {
		t.Fatal(err)
	}
	target := GovernanceTargetRef("device", "subscription")
	reservation, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
	if err != nil || !send {
		t.Fatalf("initial reserve send=%v err=%v", send, err)
	}
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportHTTPRetry, false, now); err != nil {
		t.Fatal(err)
	}
	if observed, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{record}, false, now.Add(time.Minute)); err != nil || len(observed) != 0 {
		t.Fatalf("expired observation=%+v err=%v", observed, err)
	}
	if _, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now.Add(time.Minute)); err != nil || send {
		t.Fatalf("expired retry send=%v err=%v", send, err)
	}
	view := store.Governance(now.Add(time.Minute))
	if len(view.Occurrences) != 1 || view.Occurrences[0].ResolvedAt.IsZero() {
		t.Fatalf("expired occurrence=%+v", view.Occurrences)
	}
}

func TestGovernanceReactivatedFingerprintCreatesNewEpisodeAndReceiptNamespace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	fingerprint := "sha256:" + strings.Repeat("b", 64)
	first, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: fingerprint, Kind: "policy_drift", State: "open", OccurredAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := GovernanceTargetRef("device", "subscription")
	reservation, send, err := store.ReserveGovernanceAttempt(first.DisplayID, target, now)
	if err != nil || !send {
		t.Fatalf("reserve first: send=%v err=%v", send, err)
	}
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportAccepted, true, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := reopened.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: fingerprint, Kind: "policy_drift", State: "open", OccurredAt: now.Add(2 * time.Minute)}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !created || second.DisplayID == first.DisplayID {
		t.Fatalf("reactivation created=%v first=%s second=%s", created, first.DisplayID, second.DisplayID)
	}
	if GovernanceReceiptKey(first.DisplayID, target) == GovernanceReceiptKey(second.DisplayID, target) {
		t.Fatal("reactivated episode reused receipt namespace")
	}
	if _, send, err := reopened.ReserveGovernanceAttempt(second.DisplayID, target, now.Add(2*time.Minute)); err != nil || !send {
		t.Fatalf("reactivated episode suppressed by resolved receipt: send=%v err=%v", send, err)
	}
	if got := reopened.Governance(now); len(got.Occurrences) != 2 || got.Occurrences[0].ResolvedAt.IsZero() || !got.Occurrences[1].ResolvedAt.IsZero() {
		t.Fatalf("episodes=%+v", got.Occurrences)
	}
}

func TestGovernanceReservationReservesCompletionCapacityAndInterruptedRetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.governanceMaxAttempts = 1
	store.governanceMaxItems = 1
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	occ, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "reserve", Kind: "policy_drift", OccurredAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	reservation, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
	if err != nil || !send || reservation.Class != GovernanceTransportReserved {
		t.Fatalf("reservation=%+v send=%v err=%v", reservation, send, err)
	}
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportAccepted, true, now.Add(time.Second)); err != nil {
		t.Fatalf("completion required unreserved capacity: %v", err)
	}
	if len(store.Governance(now).Receipts) != 1 {
		t.Fatal("accepted completion was not durably recorded")
	}

	interruptedDir := t.TempDir()
	interrupted, err := Open(interruptedDir)
	if err != nil {
		t.Fatal(err)
	}
	occ, _, _ = interrupted.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "interrupted", Kind: "policy_drift", OccurredAt: now}, now)
	pending, send, err := interrupted.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
	if err != nil || !send {
		t.Fatalf("pending reserve: send=%v err=%v", send, err)
	}
	reopened, err := Open(interruptedDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, send, err := reopened.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now.Add(30*time.Second)); err != nil || send {
		t.Fatalf("interrupted reservation retried early: send=%v err=%v", send, err)
	}
	retry, send, err := reopened.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now.Add(time.Minute))
	if err != nil || !send || retry.ID == pending.ID || retry.TransportCount != 1 {
		t.Fatalf("interrupted retry=%+v send=%v err=%v", retry, send, err)
	}
	view := reopened.Governance(now.Add(time.Minute))
	if len(view.Attempts) != 2 || view.Attempts[0].Class != GovernanceTransportInterrupted || !view.Attempts[0].At.Equal(now) || !view.Attempts[0].RetryAt.IsZero() || view.Attempts[1].Class != GovernanceTransportReserved {
		t.Fatalf("immutable interrupted evidence=%+v", view.Attempts)
	}
}

func TestGovernanceTargetLifecycleRetiresBindingDetailButPreservesTotals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		retire func(*Store, time.Time) error
	}{
		{name: "subscription deletion", retire: func(store *Store, _ time.Time) error { return store.RemovePushSubscription("sub") }},
		{name: "device pruning", retire: func(store *Store, now time.Time) error { _, err := store.PruneDevices(now.Add(-time.Hour)); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
			if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now.Add(-24 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := store.AddPushSubscription(PushSubscription{ID: "sub", DeviceID: "device", Endpoint: "https://push/sub", P256DH: "key", Auth: "auth"}); err != nil {
				t.Fatal(err)
			}
			occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "long-lived", Kind: "policy_drift", OccurredAt: now}, now)
			reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
			if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportNetworkRetry, false, now); err != nil {
				t.Fatal(err)
			}
			before := store.Governance(now).AttemptTotals
			if err := tc.retire(store, now); err != nil {
				t.Fatal(err)
			}
			view := store.Governance(now)
			if len(view.Attempts) != 1 || view.Attempts[0].RetiredAt.IsZero() || !view.Attempts[0].RetryAt.IsZero() || len(view.Receipts) != 0 || view.AttemptTotals.CumulativeAttempts != before.CumulativeAttempts || view.AttemptTotals.RetryPending != 0 {
				t.Fatalf("retired view=%+v totals before=%+v", view, before)
			}
		})
	}
}

func TestGovernanceCompletedTargetEvidenceRetainsReceiptUntilCompaction(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "sub", DeviceID: "device", Endpoint: "https://push/sub", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "accepted", Kind: "policy_drift", OccurredAt: now}, now)
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportAccepted, true, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RemovePushSubscription("sub"); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now)
	if len(view.Attempts) != 1 || view.Attempts[0].RetiredAt.IsZero() || len(view.Receipts) != 1 || view.Receipts[0].RetiredAt.IsZero() || view.AttemptTotals.Accepted != 1 {
		t.Fatalf("retained completed target evidence=%+v", view)
	}
	if err := store.CompactGovernance(view.Attempts[0].RetiredAt.Add(governanceRetention + time.Second)); err != nil {
		t.Fatal(err)
	}
	compacted := store.Governance(now)
	if len(compacted.Occurrences) != 1 || len(compacted.Attempts) != 0 || len(compacted.Receipts) != 0 || compacted.AttemptTotals.Accepted != 1 {
		t.Fatalf("retired target detail did not compact independently: %+v", compacted)
	}
}

func TestGovernancePendingReservationBecomesRetiredNonDueEvidence(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "sub", DeviceID: "device", Endpoint: "https://push/pending", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "pending-retire", Kind: "policy_drift", OccurredAt: now}, now)
	target := GovernanceTargetRef("device", "sub")
	reserved, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
	if err != nil || !send {
		t.Fatalf("reserve send=%v err=%v", send, err)
	}
	if err := store.RemovePushSubscriptionAt("sub", now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now.Add(time.Minute))
	if len(view.Attempts) != 1 || view.Attempts[0].Class != GovernanceTransportTargetRetired || !view.Attempts[0].At.Equal(reserved.At) || view.Attempts[0].RetiredAt.IsZero() || !view.Attempts[0].RetryAt.IsZero() || view.AttemptTotals.RetryPending != 0 || view.AttemptTotals.TargetRetired != 1 {
		t.Fatalf("pending retirement evidence=%+v totals=%+v", view.Attempts, view.AttemptTotals)
	}
}

func TestGovernanceRetiredInFlightCompletionPersistsActualOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		class    string
		accepted bool
	}{
		{name: "accepted", class: GovernanceTransportAccepted, accepted: true},
		{name: "failed", class: GovernanceTransportNetworkRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
			if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := store.AddPushSubscription(PushSubscription{ID: "sub", DeviceID: "device", Endpoint: "https://push/in-flight", P256DH: "key", Auth: "auth"}); err != nil {
				t.Fatal(err)
			}
			occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "in-flight-" + tc.name, Kind: "policy_drift", OccurredAt: now}, now)
			reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
			if !store.BeginGovernanceTransport(reservation.ID) {
				t.Fatal("reserved transport did not enter in-flight state")
			}
			retiredAt := now.Add(30 * time.Second)
			if err := store.RemovePushSubscriptionAt("sub", retiredAt); err != nil {
				t.Fatal(err)
			}
			outcome, err := store.CompleteGovernanceAttempt(reservation.ID, tc.class, tc.accepted, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Disposition != GovernanceCompletionRetired {
				t.Fatalf("completion outcome=%+v", outcome)
			}
			view := store.Governance(now.Add(time.Minute))
			if len(view.Attempts) != 1 || view.Attempts[0].Class != tc.class || !view.Attempts[0].RetiredAt.Equal(retiredAt) || !view.Attempts[0].RetryAt.IsZero() || view.AttemptTotals.RetryPending != 0 || view.AttemptTotals.TargetRetired != 0 {
				t.Fatalf("retired completion evidence=%+v totals=%+v", view.Attempts, view.AttemptTotals)
			}
			if tc.accepted {
				if len(view.Receipts) != 1 || !view.Receipts[0].RetiredAt.Equal(retiredAt) || view.AttemptTotals.Accepted != 1 {
					t.Fatalf("retired acceptance evidence=%+v totals=%+v", view.Receipts, view.AttemptTotals)
				}
			} else if len(view.Receipts) != 0 || view.AttemptTotals.RetryableFailures != 1 {
				t.Fatalf("retired failure evidence=%+v totals=%+v", view.Receipts, view.AttemptTotals)
			}
		})
	}
}

func TestGovernanceEndpointReassignmentRetiresOldTargetBeforeMove(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	for _, deviceID := range []string{"old-device", "new-device"} {
		if err := store.AddDevice(DeviceGrant{ID: deviceID, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	sub := PushSubscription{ID: "sub", DeviceID: "old-device", Endpoint: "https://push/same", P256DH: "key", Auth: "auth", LastSeenAt: now}
	if err := store.AddPushSubscription(sub); err != nil {
		t.Fatal(err)
	}
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "reassign", Kind: "policy_drift", OccurredAt: now}, now)
	oldTarget := GovernanceTargetRef("old-device", "sub")
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, oldTarget, now)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportNetworkRetry, false, now); err != nil {
		t.Fatal(err)
	}
	sub.DeviceID = "new-device"
	sub.LastSeenAt = now.Add(time.Minute)
	if err := store.AddPushSubscription(sub); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now.Add(time.Minute))
	if len(view.Attempts) != 1 || view.Attempts[0].RetiredAt.IsZero() || view.AttemptTotals.RetryPending != 0 {
		t.Fatalf("old target remained binding: %+v", view)
	}
	active := store.ActivePushSubscriptionsForDevice("new-device")
	if len(active) != 1 || active[0].ID != "sub" {
		t.Fatalf("subscription was not reassigned after retirement: %+v", active)
	}
	if _, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("new-device", "sub"), now.Add(time.Minute)); err != nil || !send {
		t.Fatalf("new target reserve send=%v err=%v", send, err)
	}
}

func TestGovernanceExpiryResolvesOmittedOccurrenceWithoutAbsenceAuthority(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "omitted-expiry", Kind: "policy_drift", OccurredAt: now, ExpiresAt: now.Add(time.Minute)}, now)
	target := GovernanceTargetRef("device", "sub")
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportNetworkRetry, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveGovernanceOccurrences(nil, false, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now.Add(time.Minute))
	if view.Occurrences[0].ResolvedAt.IsZero() || view.AttemptTotals.RetryPending != 0 {
		t.Fatalf("expired omitted occurrence remained binding: %+v", view)
	}
}

func TestGovernanceAttemptAggregatesSeparateCumulativeAndCurrentState(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "aggregate", Kind: "policy_drift", OccurredAt: now}, now)
	target := GovernanceTargetRef("device", "sub")
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportNetworkRetry, false, now); err != nil {
		t.Fatal(err)
	}
	failed := store.Governance(now)
	if failed.AttemptTotals.CumulativeAttempts != 1 || failed.AttemptTotals.RetryPending != 1 || failed.AttemptTotals.RetryableFailures != 1 {
		t.Fatalf("failed aggregate=%+v", failed.AttemptTotals)
	}
	retry, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now.Add(time.Minute))
	if err != nil || !send {
		t.Fatalf("retry reserve send=%v err=%v", send, err)
	}
	if _, err := store.CompleteGovernanceAttempt(retry.ID, GovernanceTransportAccepted, true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	succeeded := store.Governance(now.Add(time.Minute))
	if succeeded.AttemptTotals.CumulativeAttempts != 2 || succeeded.AttemptTotals.RetryPending != 0 || succeeded.AttemptTotals.Accepted != 1 || succeeded.AttemptTotals.RetryableFailures != 1 {
		t.Fatalf("success aggregate=%+v", succeeded.AttemptTotals)
	}
	before := succeeded.AttemptTotals.CumulativeAttempts
	if err := store.SetGovernanceDeliveryHealth(GovernanceDeliveryHealth{State: GovernanceDeliveryDegraded, Class: GovernanceTransportPartial, UpdatedAt: now.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got := store.Governance(now).AttemptTotals.CumulativeAttempts; got != before {
		t.Fatalf("partial health inflated attempts: got=%d want=%d", got, before)
	}
}

func TestGovernanceResolvedReservationDoesNotBindReceiptCapacity(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	old, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "old-crash", Kind: "policy_drift", OccurredAt: now}, now)
	oldReservation, send, err := store.ReserveGovernanceAttempt(old.DisplayID, GovernanceTargetRef("device", "old"), now)
	if err != nil || !send {
		t.Fatalf("old reservation send=%v err=%v", send, err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newOccurrence, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "new-active", Kind: "policy_drift", OccurredAt: now.Add(2 * time.Minute)}, now.Add(2*time.Minute))
	store.governanceMaxItems = 1
	newReservation, send, err := store.ReserveGovernanceAttempt(newOccurrence.DisplayID, GovernanceTargetRef("device", "new"), now.Add(2*time.Minute))
	if err != nil || !send {
		t.Fatalf("resolved reservation consumed receipt capacity: send=%v err=%v", send, err)
	}
	view := store.Governance(now.Add(2 * time.Minute))
	if len(view.Attempts) != 2 || view.Attempts[0].OccurrenceID != old.DisplayID || view.Attempts[0].Class != GovernanceTransportReserved || view.Attempts[1].OccurrenceID != newOccurrence.DisplayID || newReservation.ID == oldReservation.ID {
		t.Fatalf("reservation audit detail=%+v", view.Attempts)
	}
}

func TestGovernanceStateWriteRecoveryPreservesRequestedPosture(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health GovernanceDeliveryHealth
	}{
		{name: "suppressed", health: GovernanceDeliveryHealth{State: GovernanceDeliverySuppressed, Class: GovernanceTransportSuppressed}},
		{name: "partial", health: GovernanceDeliveryHealth{State: GovernanceDeliveryDegraded, Class: GovernanceTransportPartial}},
		{name: "unavailable", health: GovernanceDeliveryHealth{State: GovernanceDeliveryUnavailable, Class: GovernanceTransportNoSubscription}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, []byte("block state"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "write-" + tc.name, Kind: "policy_drift", OccurredAt: now}, now); err == nil {
				t.Fatal("expected injected state write failure")
			}
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.health.UpdatedAt = now.Add(time.Minute)
			if err := store.SetGovernanceDeliveryHealth(tc.health); err != nil {
				t.Fatal(err)
			}
			view := store.Governance(now.Add(time.Minute))
			if view.DeliveryHealth.State != tc.health.State || view.DeliveryHealth.Class != tc.health.Class || view.HealthTotals.StateFailures != 1 || view.HealthTotals.Recoveries != 1 {
				t.Fatalf("requested posture/recovery=%+v health totals=%+v", view.DeliveryHealth, view.HealthTotals)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			persisted := reopened.Governance(now.Add(time.Minute))
			if persisted.DeliveryHealth.State != tc.health.State || persisted.DeliveryHealth.Class != tc.health.Class || persisted.HealthTotals.StateFailures != 1 || persisted.HealthTotals.Recoveries != 1 {
				t.Fatalf("persisted requested posture/recovery=%+v totals=%+v", persisted.DeliveryHealth, persisted.HealthTotals)
			}
		})
	}
}

func TestGovernanceLegacyTotalsLoadIntoAttemptAndHealthSplit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := `{"alert_settings":{"mode":"watch_and_act"},"governance_attempt_totals":{"total":8,"push_service_accepted":1,"rejected":1,"retry_pending":2,"partial":1,"state_write_failure":1,"recovery":1,"overflow":1}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	view := store.Governance(time.Now().UTC())
	if view.AttemptTotals.CumulativeAttempts != 4 || view.AttemptTotals.RetryableFailures != 2 || view.AttemptTotals.Accepted != 1 || view.AttemptTotals.Rejected != 1 {
		t.Fatalf("migrated attempt totals=%+v", view.AttemptTotals)
	}
	if view.HealthTotals != (GovernanceHealthEventTotals{PartialEpisodes: 1, StateFailures: 1, Recoveries: 1, Overflows: 1}) {
		t.Fatalf("migrated health totals=%+v", view.HealthTotals)
	}
}

func TestCompactGovernanceFailureRestoresDeepCopiedEvidence(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour)
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "compact-rollback", Kind: "policy_drift", OccurredAt: old}, old)
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), old)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportAccepted, true, old); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	before := store.Governance(now)
	store.saveHook = func(stage string) error {
		if stage == "rename" {
			return errors.New("injected compact rename failure")
		}
		return nil
	}
	if err := store.CompactGovernance(now); err == nil {
		t.Fatal("compaction persistence failure was ignored")
	}
	store.saveHook = nil
	after := store.Governance(now)
	if !reflect.DeepEqual(after.Occurrences, before.Occurrences) || !reflect.DeepEqual(after.Attempts, before.Attempts) || !reflect.DeepEqual(after.Receipts, before.Receipts) || after.AttemptTotals != before.AttemptTotals {
		t.Fatalf("visible state changed after failed compaction:\nbefore=%+v\nafter=%+v", before, after)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Governance(now); !reflect.DeepEqual(got.Occurrences, before.Occurrences) || !reflect.DeepEqual(got.Attempts, before.Attempts) || !reflect.DeepEqual(got.Receipts, before.Receipts) || got.AttemptTotals != before.AttemptTotals {
		t.Fatalf("reopened evidence changed after failed compaction:\nbefore=%+v\nafter=%+v", before, got)
	}
}

func TestAtomicSavePreservesReadablePriorStateAndCleansTemporaryFiles(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AddDevice(DeviceGrant{ID: "prior", CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			store.saveHook = func(got string) error {
				if got == stage {
					return errors.New("injected " + stage)
				}
				return nil
			}
			if err := store.AddDevice(DeviceGrant{ID: "new", CreatedAt: time.Now().UTC()}); err == nil {
				t.Fatal("injected save failure was ignored")
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatalf("reopen after %s failure: %v", stage, err)
			}
			if _, ok := reopened.Device("prior"); !ok {
				t.Fatal("prior state was lost")
			}
			if _, ok := reopened.Device("new"); ok {
				t.Fatal("failed write replaced prior state")
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".state-*.tmp"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("temporary files=%v err=%v", temps, err)
			}
		})
	}
}

func TestAddPushSubscriptionRollsBackNewTargetOnPersistenceFailure(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
			if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			before := store.PushSubscriptions()
			store.saveHook = func(got string) error {
				if got == stage {
					return errors.New("injected new-subscription " + stage + " failure")
				}
				return nil
			}
			failed := PushSubscription{ID: "failed", DeviceID: "device", Endpoint: "https://push.example/failed", P256DH: "key", Auth: "auth", CreatedAt: now}
			if err := store.AddPushSubscription(failed); err == nil {
				t.Fatal("new subscription persistence failure was ignored")
			}
			if got := store.PushSubscriptions(); !reflect.DeepEqual(got, before) {
				t.Fatalf("in-memory subscriptions changed after failed save: got=%+v want=%+v", got, before)
			}
			if got := store.ActivePushSubscriptions(); len(got) != 0 {
				t.Fatalf("failed target became active: %+v", got)
			}

			store.saveHook = nil
			if _, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "later-" + stage, Kind: "policy_drift", OccurredAt: now.Add(time.Minute)}, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := reopened.PushSubscriptions(); !reflect.DeepEqual(got, before) {
				t.Fatalf("later governance save persisted failed target: got=%+v want=%+v", got, before)
			}
		})
	}
}

func TestGovernanceQuietPersistenceRecoversStateWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failureStage string
		recover      func(*Store, time.Time) error
	}{
		{
			name:         "empty authoritative observation",
			failureStage: "write",
			recover: func(store *Store, now time.Time) error {
				_, err := store.ObserveGovernanceOccurrences(nil, true, now)
				return err
			},
		},
		{
			name:         "no-op compaction",
			failureStage: "rename",
			recover: func(store *Store, now time.Time) error {
				return store.CompactGovernance(now)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
			quiet := GovernanceDeliveryHealth{State: GovernanceDeliveryUnavailable, Class: GovernanceTransportNoSubscription, UpdatedAt: now}
			if err := store.SetGovernanceDeliveryHealth(quiet); err != nil {
				t.Fatal(err)
			}
			store.saveHook = func(stage string) error {
				if stage == tc.failureStage {
					return errors.New("injected quiet " + tc.failureStage + " failure")
				}
				return nil
			}
			if _, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "quiet-failure-" + tc.failureStage, Kind: "policy_drift", OccurredAt: now}, now); err == nil {
				t.Fatal("governance persistence failure was ignored")
			}
			if got := store.Governance(now).DeliveryHealth; got.Class != GovernanceTransportStateWrite {
				t.Fatalf("volatile failure not visible: %+v", got)
			}

			store.saveHook = nil
			if err := tc.recover(store, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			view := store.Governance(now.Add(time.Minute))
			if view.DeliveryHealth.State != quiet.State || view.DeliveryHealth.Class != quiet.Class || view.HealthTotals.StateFailures != 1 || view.HealthTotals.Recoveries != 1 {
				t.Fatalf("quiet recovery view=%+v totals=%+v", view.DeliveryHealth, view.HealthTotals)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			persisted := reopened.Governance(now.Add(time.Minute))
			if persisted.DeliveryHealth.State != quiet.State || persisted.DeliveryHealth.Class != quiet.Class || persisted.HealthTotals.StateFailures != 1 || persisted.HealthTotals.Recoveries != 1 {
				t.Fatalf("reopened quiet recovery=%+v totals=%+v", persisted.DeliveryHealth, persisted.HealthTotals)
			}
		})
	}
}

func TestGovernanceCompletionFailureReleasesRetiredInFlightCapacity(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		for _, outcome := range []struct {
			name     string
			class    string
			accepted bool
		}{
			{name: "accepted", class: GovernanceTransportAccepted, accepted: true},
			{name: "failed", class: GovernanceTransportNetworkRetry},
		} {
			t.Run(stage+"/"+outcome.name, func(t *testing.T) {
				store, err := Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
				quiet := GovernanceDeliveryHealth{State: GovernanceDeliveryUnavailable, Class: GovernanceTransportNoSubscription, UpdatedAt: now}
				if err := store.SetGovernanceDeliveryHealth(quiet); err != nil {
					t.Fatal(err)
				}
				if err := store.AddDevice(DeviceGrant{ID: "retired-device", CreatedAt: now}); err != nil {
					t.Fatal(err)
				}
				if err := store.AddPushSubscription(PushSubscription{ID: "retired-sub", DeviceID: "retired-device", Endpoint: "https://push.example/retired", P256DH: "key", Auth: "auth", CreatedAt: now}); err != nil {
					t.Fatal(err)
				}
				occurrence, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "completion-capacity-" + stage + "-" + outcome.name, Kind: "policy_drift", OccurredAt: now}, now)
				if err != nil {
					t.Fatal(err)
				}
				oldTarget := GovernanceTargetRef("retired-device", "retired-sub")
				reservation, send, err := store.ReserveGovernanceAttempt(occurrence.DisplayID, oldTarget, now)
				if err != nil || !send || !store.BeginGovernanceTransport(reservation.ID) {
					t.Fatalf("initial reservation=%+v send=%v err=%v", reservation, send, err)
				}
				if err := store.RemovePushSubscriptionAt("retired-sub", now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				store.governanceMaxItems = 1
				store.saveHook = func(got string) error {
					if got == stage {
						return errors.New("injected completion " + stage + " failure")
					}
					return nil
				}
				if _, err := store.CompleteGovernanceAttempt(reservation.ID, outcome.class, outcome.accepted, now.Add(2*time.Second)); err == nil {
					t.Fatal("completion persistence failure was ignored")
				}
				failed := store.Governance(now.Add(2 * time.Second))
				if len(failed.Attempts) != 1 || failed.Attempts[0].Class != GovernanceTransportTargetRetired || len(failed.Receipts) != 0 || failed.DeliveryHealth.Class != GovernanceTransportStateWrite {
					t.Fatalf("failed completion claimed durable outcome: %+v", failed)
				}

				store.saveHook = nil
				newTarget := GovernanceTargetRef("active-device", "active-sub")
				fresh, send, err := store.ReserveGovernanceAttempt(occurrence.DisplayID, newTarget, now.Add(time.Minute))
				if err != nil || !send || fresh.TargetRef != newTarget {
					t.Fatalf("retired failed completion retained capacity: reservation=%+v send=%v err=%v", fresh, send, err)
				}
				recovered := store.Governance(now.Add(time.Minute))
				if recovered.DeliveryHealth.State != quiet.State || recovered.DeliveryHealth.Class != quiet.Class || recovered.HealthTotals.StateFailures != 1 || recovered.HealthTotals.Recoveries != 1 {
					t.Fatalf("completion failure recovery=%+v totals=%+v", recovered.DeliveryHealth, recovered.HealthTotals)
				}
			})
		}
	}
}

func TestGovernanceNoOpObservationResolutionCompactionAndHealthSkipSaves(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	saves := 0
	store.saveObserver = func() { saves++ }
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	record := GovernanceOccurrence{Fingerprint: "stable", Kind: "policy_drift", State: "open", OccurredAt: now}
	if _, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{record}, true, now); err != nil {
		t.Fatal(err)
	}
	baseline := saves
	if _, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{record}, true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGovernanceOccurrences([]string{"stable"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactGovernance(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	health := GovernanceDeliveryHealth{State: GovernanceDeliverySuppressed, Class: GovernanceTransportSuppressed, UpdatedAt: now}
	if err := store.SetGovernanceDeliveryHealth(health); err != nil {
		t.Fatal(err)
	}
	baseline++
	health.UpdatedAt = now.Add(time.Minute)
	if err := store.SetGovernanceDeliveryHealth(health); err != nil {
		t.Fatal(err)
	}
	if saves != baseline {
		t.Fatalf("no-op operations wrote state: saves=%d baseline=%d", saves, baseline)
	}
}

func TestStateJSONNeverStoresRawGovernanceTransportError(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(store.data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "raw_error") {
		t.Fatal("state schema grew a raw governance error field")
	}
}
