package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/push"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestShouldAlertModes(t *testing.T) {
	t.Parallel()
	watch := rpc.CanaryResult{Severity: risk.SeverityWatch}
	act := rpc.CanaryResult{Severity: risk.SeverityAct}
	observe := rpc.CanaryResult{Severity: risk.SeverityObserve}
	confirm := rpc.CanaryResult{Severity: risk.SeverityObserve, Action: "confirm_inputs"}

	if ShouldAlert(state.AlertModeNone, act) {
		t.Fatalf("none mode should not alert")
	}
	if ShouldAlert(state.AlertModeActOnly, watch) {
		t.Fatalf("act_only should ignore watch severity")
	}
	if !ShouldAlert(state.AlertModeActOnly, act) {
		t.Fatalf("act_only should alert on act severity")
	}
	if !ShouldAlert(state.AlertModeActOnly, confirm) {
		t.Fatalf("act_only should alert on confirm_inputs")
	}
	if !ShouldAlert(state.AlertModeWatchAndAct, watch) {
		t.Fatalf("watch_and_act should alert on watch severity")
	}
	emptyMarketWatch := rpc.CanaryResult{
		Severity:     risk.SeverityWatch,
		PortfolioFit: "low",
		Portfolio:    rpc.CanaryPortfolioSummary{},
	}
	if ShouldAlert(state.AlertModeWatchAndAct, emptyMarketWatch) {
		t.Fatalf("watch_and_act should not alert on market-only low-exposure canary")
	}
	exposed := 1.0
	portfolioWatch := emptyMarketWatch
	portfolioWatch.Portfolio.GrossExposurePctNLV = &exposed
	if !ShouldAlert(state.AlertModeWatchAndAct, portfolioWatch) {
		t.Fatalf("watch_and_act should alert when low-fit canary still has material exposure")
	}
	if ShouldAlert("bogus", act) {
		t.Fatalf("unknown mode should not alert")
	}
	if ShouldAlert(state.AlertModeWatchAndAct, observe) {
		t.Fatalf("watch_and_act should ignore observe severity")
	}
}

func TestObserveRedactsPayloadAndDedupesFingerprint(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.EnsureVAPID(time.Now().UTC(), func() (string, string, error) {
		return "private", "public", nil
	}); err != nil {
		t.Fatalf("EnsureVAPID: %v", err)
	}
	if err := store.SetAlertMode(state.AlertModeWatchAndAct); err != nil {
		t.Fatalf("SetAlertMode: %v", err)
	}
	if err := store.AddPushSubscription(state.PushSubscription{
		ID:       "sub-1",
		DeviceID: "device-1",
		Endpoint: "https://push.example/sub",
		P256DH:   "p256dh",
		Auth:     "auth",
	}); err != nil {
		t.Fatalf("AddPushSubscription: %v", err)
	}
	sender := &recordingSender{}
	monitor := Monitor{
		Store:  store,
		Sender: sender,
		URL:    "https://relay.example",
		Now: func() time.Time {
			return time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
		},
	}
	canary := rpc.CanaryResult{
		Fingerprint:        rpc.Fingerprint{Version: rpc.CanaryFingerprintVersion, Key: "sha256:test"},
		Action:             "defend",
		Severity:           risk.SeverityAct,
		MarketConfirmation: "confirmed",
		Summary:            "private AAPL exposure is 100000 USD",
	}

	rec, attempts := monitor.Observe(context.Background(), canary)
	if rec == nil {
		t.Fatalf("expected alert record")
	}
	if len(attempts) != 1 || len(sender.payloads) != 1 {
		t.Fatalf("push attempts=%d payloads=%d, want 1 each", len(attempts), len(sender.payloads))
	}
	payloadText := sender.payloads[0].Title + " " + sender.payloads[0].Body
	for _, forbidden := range []string{"AAPL", "100000", "private"} {
		if strings.Contains(payloadText, forbidden) {
			t.Fatalf("payload leaked %q: %s", forbidden, payloadText)
		}
	}
	if !strings.Contains(payloadText, "Open ibkr for portfolio details") {
		t.Fatalf("payload missing app-open hint: %s", payloadText)
	}

	rec, attempts = monitor.Observe(context.Background(), canary)
	if rec != nil || len(attempts) != 0 {
		t.Fatalf("duplicate fingerprint should be suppressed, rec=%#v attempts=%d", rec, len(attempts))
	}
	if got := store.AlertHistory(10); len(got) != 1 {
		t.Fatalf("alert history length=%d, want 1", len(got))
	}
}

func TestGovernanceDispatcherNoSubscriptionAndNoneSuppression(t *testing.T) {
	t.Parallel()
	store := governanceStore(t, state.AlertModeWatchAndAct)
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	dispatcher := GovernanceDispatcher{Store: store, Sender: &recordingSender{}, Now: func() time.Time { return now }}
	view, err := dispatcher.Observe(t.Context(), governanceSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Attempts) != 1 || view.Attempts[0].Class != state.GovernanceTransportNoSubscription || len(view.Receipts) != 0 {
		t.Fatalf("no-subscription view=%+v", view)
	}
	if err := store.SetAlertMode(state.AlertModeNone); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	view, err = dispatcher.Observe(t.Context(), governanceSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	if view.DeliveryHealth.State != state.GovernanceDeliverySuppressed || view.DeliveryHealth.Class != state.GovernanceTransportSuppressed {
		t.Fatalf("none mode health=%+v", view.DeliveryHealth)
	}
}

func TestGovernanceDispatcherPerDevicePartialAndRestartDedupe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlertMode(state.AlertModeActOnly); err != nil {
		t.Fatal(err)
	}
	ensureGovernanceKeys(t, store)
	addGovernanceTarget(t, store, "device-one", "sub-one")
	addGovernanceTarget(t, store, "device-two", "sub-two")
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	sender := &recordingSender{results: map[string]state.PushAttempt{
		"sub-one": {OK: true, Class: state.GovernanceTransportAccepted},
		"sub-two": {Class: state.GovernanceTransportHTTPRetry},
	}}
	dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	view, err := dispatcher.Observe(t.Context(), governanceSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.payloads) != 2 || len(view.Receipts) != 1 || view.DeliveryHealth.Class != state.GovernanceTransportPartial {
		t.Fatalf("partial dispatch payloads=%d view=%+v", len(sender.payloads), view)
	}
	if view.AttemptTotals.CumulativeAttempts != 2 || view.AttemptTotals.RetryPending != 1 || view.HealthTotals.PartialEpisodes != 1 {
		t.Fatalf("partial aggregate mixed attempts and health episodes: attempts=%+v health=%+v", view.AttemptTotals, view.HealthTotals)
	}

	reopened, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restartSender := &recordingSender{results: map[string]state.PushAttempt{
		"sub-one": {OK: true, Class: state.GovernanceTransportAccepted},
		"sub-two": {OK: true, Class: state.GovernanceTransportAccepted},
	}}
	now = now.Add(time.Minute)
	restartedDispatcher := GovernanceDispatcher{Store: reopened, Sender: restartSender, Now: func() time.Time { return now }}
	view, err = restartedDispatcher.Observe(t.Context(), governanceSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(restartSender.payloads) != 1 {
		t.Fatalf("accepted target should stay terminal while retryable target retries once: %d sends", len(restartSender.payloads))
	}
	if len(view.Receipts) != 2 {
		t.Fatalf("receipts=%+v", view.Receipts)
	}
}

func TestGovernanceDispatcherTerminalRejectionDoesNotRetryOrBlockNewTarget(t *testing.T) {
	t.Parallel()
	store := governanceStore(t, state.AlertModeWatchAndAct)
	addGovernanceTarget(t, store, "device-rejected", "sub-rejected")
	now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
	sender := &recordingSender{results: map[string]state.PushAttempt{
		"sub-rejected": {Class: state.GovernanceTransportHTTPRejected},
	}}
	dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	view, err := dispatcher.Observe(t.Context(), governanceSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.payloads) != 1 || len(view.Attempts) != 1 || !view.Attempts[0].RetryAt.IsZero() || view.AttemptTotals.RetryPending != 0 {
		t.Fatalf("initial terminal rejection sends=%d view=%+v", len(sender.payloads), view)
	}

	addGovernanceTarget(t, store, "device-new", "sub-new")
	sender.results["sub-new"] = state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}
	now = now.Add(24 * time.Hour)
	view, err = dispatcher.Observe(t.Context(), governanceSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.payloads) != 2 || len(view.Attempts) != 2 || len(view.Receipts) != 1 || view.AttemptTotals.Rejected != 1 || view.AttemptTotals.Accepted != 1 || view.AttemptTotals.RetryPending != 0 {
		t.Fatalf("terminal target blocked or retried unrelated delivery: sends=%d view=%+v", len(sender.payloads), view)
	}
}

func TestGovernanceDispatcherTimeoutRetriesWithoutCallingItAccepted(t *testing.T) {
	t.Parallel()
	store := governanceStore(t, state.AlertModeWatchAndAct)
	addGovernanceTarget(t, store, "device", "sub")
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	sender := &recordingSender{results: map[string]state.PushAttempt{"sub": {Class: state.GovernanceTransportTimeoutRetry}}}
	dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	view, err := dispatcher.Observe(t.Context(), governanceSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Receipts) != 0 || view.Attempts[len(view.Attempts)-1].Class != state.GovernanceTransportTimeoutRetry {
		t.Fatalf("timeout view=%+v", view)
	}
	now = now.Add(30 * time.Second)
	_, _ = dispatcher.Observe(t.Context(), governanceSnapshot(now))
	if len(sender.payloads) != 1 {
		t.Fatalf("timeout retried before bounded backoff: sends=%d", len(sender.payloads))
	}
	now = now.Add(30 * time.Second)
	_, _ = dispatcher.Observe(t.Context(), governanceSnapshot(now))
	if len(sender.payloads) != 2 {
		t.Fatalf("timeout did not retry when due: sends=%d", len(sender.payloads))
	}
	now = now.Add(5 * time.Minute)
	_, _ = dispatcher.Observe(t.Context(), governanceSnapshot(now))
	for range 3 {
		now = now.Add(15 * time.Minute)
		_, _ = dispatcher.Observe(t.Context(), governanceSnapshot(now))
	}
	if len(sender.payloads) != 6 {
		t.Fatalf("capped retry stopped while occurrence remained active: sends=%d", len(sender.payloads))
	}
}

func TestGovernanceSuppressedSnapshotDoesNotResolveActiveOccurrence(t *testing.T) {
	t.Parallel()
	store := governanceStore(t, state.AlertModeWatchAndAct)
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	dispatcher := GovernanceDispatcher{Store: store, Now: func() time.Time { return now }}
	if _, err := dispatcher.Observe(t.Context(), governanceSnapshot(now)); err != nil {
		t.Fatal(err)
	}
	suppressed := governanceSnapshot(now.Add(time.Minute))
	suppressed.Candidates = nil
	suppressed.SourceHealth.Reconciliation = rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusUnavailable, Reason: rpc.NudgeHealthReasonSourceUnavailable, AsOf: now.Add(time.Minute)}
	now = now.Add(time.Minute)
	if _, err := dispatcher.Observe(t.Context(), suppressed); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now)
	if len(view.Occurrences) != 1 || !view.Occurrences[0].ResolvedAt.IsZero() {
		t.Fatalf("suppressed daemon input falsely resolved occurrence: %+v", view.Occurrences)
	}
}

func TestGovernanceSuppressedEmptySnapshotStillResolvesOwnExpiry(t *testing.T) {
	t.Parallel()
	store := governanceStore(t, state.AlertModeWatchAndAct)
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	dispatcher := GovernanceDispatcher{Store: store, Now: func() time.Time { return now }}
	occurrence, _, err := store.UpsertGovernanceOccurrence(state.GovernanceOccurrence{
		Fingerprint: "sha256:" + strings.Repeat("e", 64), Kind: rpc.NudgeKindPolicyDrift, State: rpc.NudgeStateOpen,
		OccurredAt: now, ExpiresAt: now.Add(time.Minute),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := state.GovernanceTargetRef("device", "sub")
	reservation, _, _ := store.ReserveGovernanceAttempt(occurrence.DisplayID, target, now)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, state.GovernanceTransportNetworkRetry, false, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	suppressed := governanceSnapshot(now)
	suppressed.Candidates = nil
	suppressed.SourceHealth.Reconciliation = rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusUnavailable, Reason: rpc.NudgeHealthReasonSourceUnavailable, AsOf: now}
	if _, err := dispatcher.Observe(t.Context(), suppressed); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now)
	if len(view.Occurrences) != 1 || view.Occurrences[0].ResolvedAt.IsZero() || view.AttemptTotals.RetryPending != 0 {
		t.Fatalf("suppressed empty expiry remained active: %+v", view)
	}
}

func TestGovernanceDispatcherSingleSuccessAllFailureAndDeadSubscription(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		result      state.PushAttempt
		wantHealth  string
		wantReceipt int
		wantSubs    int
	}{
		{name: "single success", result: state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}, wantHealth: state.GovernanceTransportAccepted, wantReceipt: 1, wantSubs: 1},
		{name: "all failure", result: state.PushAttempt{Class: state.GovernanceTransportRejected}, wantHealth: state.GovernanceTransportAllFailed, wantSubs: 1},
		{name: "dead subscription", result: state.PushAttempt{Class: state.GovernanceTransportDead}, wantHealth: state.GovernanceTransportAllFailed, wantSubs: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := governanceStore(t, state.AlertModeActOnly)
			addGovernanceTarget(t, store, "device", "sub")
			now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
			sender := &recordingSender{results: map[string]state.PushAttempt{"sub": tc.result}}
			dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
			view, err := dispatcher.Observe(t.Context(), governanceSnapshot(now))
			if err != nil {
				t.Fatal(err)
			}
			if view.DeliveryHealth.Class != tc.wantHealth || len(view.Receipts) != tc.wantReceipt || len(store.ActivePushSubscriptions()) != tc.wantSubs {
				t.Fatalf("view=%+v active_subscriptions=%d", view, len(store.ActivePushSubscriptions()))
			}
			if tc.name == "dead subscription" && (len(view.Attempts) != 1 || view.Attempts[0].RetiredAt.IsZero() || view.Attempts[0].Class != state.GovernanceTransportDead) {
				t.Fatalf("dead-subscription evidence was deleted: %+v", view.Attempts)
			}
		})
	}
}

func TestGovernanceDispatcherFinalizesOutcomeWhenTargetRetiresInFlight(t *testing.T) {
	for _, mutation := range []string{"unsubscribe", "reassign"} {
		for _, outcome := range []string{"accepted", "failed"} {
			t.Run(mutation+"/"+outcome, func(t *testing.T) {
				store := governanceStore(t, state.AlertModeWatchAndAct)
				addGovernanceTarget(t, store, "device-old", "sub")
				now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
				result := state.PushAttempt{Class: state.GovernanceTransportNetworkRetry}
				if outcome == "accepted" {
					result = state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}
				}
				sender := &retiringTargetSender{started: make(chan struct{}), release: make(chan struct{}), result: result}
				dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
				done := make(chan error, 1)
				go func() {
					_, err := dispatcher.Observe(t.Context(), governanceSnapshot(now))
					done <- err
				}()
				<-sender.started
				switch mutation {
				case "unsubscribe":
					if err := store.RemovePushSubscriptionAt("sub", now); err != nil {
						t.Fatal(err)
					}
				case "reassign":
					if err := store.AddDevice(state.DeviceGrant{ID: "device-new", CreatedAt: now}); err != nil {
						t.Fatal(err)
					}
					if err := store.AddPushSubscription(state.PushSubscription{ID: "replacement", DeviceID: "device-new", Endpoint: "https://push.example/sub", P256DH: "key", Auth: "auth", LastSeenAt: now}); err != nil {
						t.Fatal(err)
					}
				}
				close(sender.release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				view := store.Governance(now)
				if len(view.Attempts) != 1 || view.Attempts[0].Class != result.Class || view.Attempts[0].RetiredAt.IsZero() || view.AttemptTotals.RetryPending != 0 {
					t.Fatalf("retired in-flight attempt=%+v totals=%+v", view.Attempts, view.AttemptTotals)
				}
				if view.DeliveryHealth.State == state.GovernanceDeliveryHealthy || view.DeliveryHealth.Class != state.GovernanceTransportTargetRetired {
					t.Fatalf("retired target distorted active health: %+v", view.DeliveryHealth)
				}
				if outcome == "accepted" {
					if len(view.Receipts) != 1 || view.Receipts[0].RetiredAt.IsZero() || view.DeliveryHealth.LastAcceptedAt.IsZero() {
						t.Fatalf("retired acceptance truth=%+v health=%+v", view.Receipts, view.DeliveryHealth)
					}
				} else if len(view.Receipts) != 0 {
					t.Fatalf("failed retired transport invented receipt: %+v", view.Receipts)
				}
			})
		}
	}
}

func TestGovernanceDispatcherSerializesConcurrentPolls(t *testing.T) {
	t.Parallel()
	store := governanceStore(t, state.AlertModeWatchAndAct)
	addGovernanceTarget(t, store, "device", "sub")
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	sender := &blockingGovernanceSender{started: make(chan struct{}), release: make(chan struct{})}
	dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			_, _ = dispatcher.Observe(t.Context(), governanceSnapshot(now))
		}()
	}
	<-sender.started
	close(sender.release)
	wait.Wait()
	if sender.calls != 1 {
		t.Fatalf("concurrent polls sent %d notifications for one occurrence/target", sender.calls)
	}
}

func TestGovernanceDispatcherDoesNotSendWithoutDurableReservationCapacity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	attempts := make([]state.GovernanceAttempt, 4096)
	for i := range attempts {
		attempts[i] = state.GovernanceAttempt{ID: "full-" + string(rune(i)), OccurrenceID: "old", TargetRef: "old", ReceiptKey: "old", At: now, Class: state.GovernanceTransportHTTPRejected, RetryAt: now.Add(time.Hour)}
	}
	data := state.Data{
		Devices:           []state.DeviceGrant{{ID: "device", CreatedAt: now}},
		PushSubscriptions: []state.PushSubscription{{ID: "sub", DeviceID: "device", Endpoint: "https://push/sub", P256DH: "key", Auth: "auth"}},
		VAPID:             &state.VAPIDKeys{PublicKey: "public", PrivateKey: "private", CreatedAt: now},
		AlertSettings:     state.AlertSettings{Mode: state.AlertModeWatchAndAct}, GovernanceAttempts: attempts,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	if _, err := dispatcher.Observe(t.Context(), governanceSnapshot(now)); !errors.Is(err, state.ErrGovernanceOverflow) {
		t.Fatalf("err=%v, want overflow", err)
	}
	if len(sender.payloads) != 0 {
		t.Fatalf("sender called %d times without durable capacity", len(sender.payloads))
	}
}

func TestGovernanceReactivatedIdenticalFingerprintSendsNewEpisode(t *testing.T) {
	t.Parallel()
	store := governanceStore(t, state.AlertModeWatchAndAct)
	addGovernanceTarget(t, store, "device", "sub")
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	sender := &recordingSender{}
	dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	if _, err := dispatcher.Observe(t.Context(), governanceSnapshot(now)); err != nil {
		t.Fatal(err)
	}
	empty := governanceSnapshot(now.Add(time.Minute))
	empty.Candidates = nil
	now = now.Add(time.Minute)
	if _, err := dispatcher.Observe(t.Context(), empty); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := dispatcher.Observe(t.Context(), governanceSnapshot(now)); err != nil {
		t.Fatal(err)
	}
	if len(sender.payloads) != 2 || sender.payloads[0].DisplayID == sender.payloads[1].DisplayID {
		t.Fatalf("reactivated sends=%d payloads=%+v", len(sender.payloads), sender.payloads)
	}
}

func TestGovernanceWorkerCoalescesWhileHungAndRecoversAfterTimeout(t *testing.T) {
	t.Parallel()
	store := governanceStore(t, state.AlertModeWatchAndAct)
	addGovernanceTarget(t, store, "device", "sub")
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	sender := &contextAwareSender{started: make(chan struct{}, 4)}
	dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }, SendTimeout: 30 * time.Millisecond}
	worker := NewGovernanceWorker(&dispatcher)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	worker.Submit(governanceSnapshot(now))
	<-sender.started
	now = now.Add(time.Minute)
	for i := range 100 {
		latest := governanceSnapshot(now.Add(time.Duration(i+1) * time.Minute))
		worker.Submit(latest)
	}
	select {
	case <-sender.started:
	case <-time.After(time.Second):
		t.Fatal("coalesced latest snapshot did not run after timeout")
	}
	if sender.MaxConcurrent() != 1 || worker.Pending() > 1 {
		t.Fatalf("max concurrent=%d pending=%d", sender.MaxConcurrent(), worker.Pending())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("governance worker did not stop after cancellation")
	}
}

func TestGovernanceWorkerConcurrentSubmitProcessesNewestGenerationLast(t *testing.T) {
	store := governanceStore(t, state.AlertModeWatchAndAct)
	addGovernanceTarget(t, store, "device", "sub")
	base := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	now := base
	sender := &generationSender{started: make(chan struct{}), release: make(chan struct{})}
	dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	worker := NewGovernanceWorker(&dispatcher)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	worker.Submit(governanceSnapshot(base))
	<-sender.started

	type submission struct {
		generation uint64
		occurredAt time.Time
	}
	const count = 64
	start := make(chan struct{})
	results := make(chan submission, count)
	var submits sync.WaitGroup
	submits.Add(count)
	for i := range count {
		go func(index int) {
			defer submits.Done()
			<-start
			at := base.Add(time.Duration(index+1) * time.Second)
			snapshot := governanceSnapshot(at)
			snapshot.Candidates[0].Fingerprint = "sha256:" + fmt.Sprintf("%064x", index+1)
			generation := worker.Submit(snapshot)
			results <- submission{generation: generation, occurredAt: at}
		}(i)
	}
	close(start)
	submits.Wait()
	close(results)
	var newest submission
	for result := range results {
		if result.generation > newest.generation {
			newest = result
		}
	}
	now = base.Add(2 * time.Minute)
	close(sender.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		view := store.Governance(now)
		if worker.Pending() == 0 && sender.Calls() >= 2 && len(view.Occurrences) >= 2 {
			last := view.Occurrences[len(view.Occurrences)-1]
			if !last.OccurredAt.Equal(newest.occurredAt) || !last.ResolvedAt.IsZero() {
				t.Fatalf("last processed occurrence=%+v, newest generation=%+v", last, newest)
			}
			if sender.Calls() != 2 {
				t.Fatalf("coalescing processed %d sends, want initial plus newest", sender.Calls())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("newest generation was not processed: pending=%d calls=%d view=%+v", worker.Pending(), sender.Calls(), view)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestGovernanceHealthRecoversAfterNoneWithoutAnotherSend(t *testing.T) {
	t.Parallel()
	store := governanceStore(t, state.AlertModeWatchAndAct)
	addGovernanceTarget(t, store, "device", "sub")
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	sender := &recordingSender{}
	dispatcher := GovernanceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return now }}
	if _, err := dispatcher.Observe(t.Context(), governanceSnapshot(now)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlertMode(state.AlertModeNone); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := dispatcher.Observe(t.Context(), governanceSnapshot(now)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlertMode(state.AlertModeWatchAndAct); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	view, err := dispatcher.Observe(t.Context(), governanceSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.payloads) != 1 || view.DeliveryHealth.State != state.GovernanceDeliveryHealthy || view.DeliveryHealth.Class != state.GovernanceTransportAccepted {
		t.Fatalf("sends=%d health=%+v", len(sender.payloads), view.DeliveryHealth)
	}
}

func governanceStore(t *testing.T, mode string) *state.Store {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlertMode(mode); err != nil {
		t.Fatal(err)
	}
	ensureGovernanceKeys(t, store)
	return store
}

func ensureGovernanceKeys(t *testing.T, store *state.Store) {
	t.Helper()
	if _, err := store.EnsureVAPID(time.Now().UTC(), func() (string, string, error) { return "private", "public", nil }); err != nil {
		t.Fatal(err)
	}
}

func addGovernanceTarget(t *testing.T, store *state.Store, deviceID, subID string) {
	t.Helper()
	if err := store.AddDevice(state.DeviceGrant{ID: deviceID, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddPushSubscription(state.PushSubscription{ID: subID, DeviceID: deviceID, Endpoint: "https://push.example/" + subID, P256DH: "key", Auth: "auth", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

func governanceSnapshot(now time.Time) rpc.NudgesSnapshotResult {
	ok := rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: now}
	return rpc.NudgesSnapshotResult{AsOf: now, Candidates: []rpc.NudgeCandidate{{
		Fingerprint: "sha256:" + strings.Repeat("a", 64), Kind: rpc.NudgeKindPolicyDrift, State: rpc.NudgeStateOpen,
		Severity: rpc.NudgeSeverityAct, Title: "Policy pins need review", Body: "Review the policy pin status.", OccurredAt: now, Destination: rpc.NudgeDestinationAlerts,
	}}, SourceHealth: rpc.NudgeSourceHealth{Policy: ok, Reconciliation: ok, Capital: ok, Pins: ok, Cadence: ok, ConfirmedFlow: ok}, ConfirmedFlowCoverage: &rpc.NudgeConfirmedFlowCoverage{CoverageFrom: now}}
}

type recordingSender struct {
	payloads []push.Payload
	results  map[string]state.PushAttempt
}

type blockingGovernanceSender struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   int
}

type contextAwareSender struct {
	started chan struct{}
	mu      sync.Mutex
	current int
	max     int
}

type retiringTargetSender struct {
	started chan struct{}
	release chan struct{}
	result  state.PushAttempt
}

func (s *retiringTargetSender) Send(_ context.Context, sub state.PushSubscription, _ state.VAPIDKeys, _ push.Payload) state.PushAttempt {
	close(s.started)
	<-s.release
	result := s.result
	result.SubscriptionID = sub.ID
	return result
}

type generationSender struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (s *generationSender) Send(_ context.Context, sub state.PushSubscription, _ state.VAPIDKeys, _ push.Payload) state.PushAttempt {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		close(s.started)
		<-s.release
	}
	return state.PushAttempt{SubscriptionID: sub.ID, OK: true, Class: state.GovernanceTransportAccepted}
}

func (s *generationSender) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *contextAwareSender) Send(ctx context.Context, sub state.PushSubscription, _ state.VAPIDKeys, _ push.Payload) state.PushAttempt {
	s.mu.Lock()
	s.current++
	if s.current > s.max {
		s.max = s.current
	}
	s.mu.Unlock()
	s.started <- struct{}{}
	<-ctx.Done()
	s.mu.Lock()
	s.current--
	s.mu.Unlock()
	return state.PushAttempt{SubscriptionID: sub.ID, Class: state.GovernanceTransportDeadlineRetry}
}

func (s *contextAwareSender) MaxConcurrent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max
}

func (s *blockingGovernanceSender) Send(_ context.Context, sub state.PushSubscription, _ state.VAPIDKeys, _ push.Payload) state.PushAttempt {
	s.calls++
	s.once.Do(func() { close(s.started) })
	<-s.release
	return state.PushAttempt{SubscriptionID: sub.ID, OK: true, Class: state.GovernanceTransportAccepted}
}

func (s *recordingSender) Send(_ context.Context, sub state.PushSubscription, _ state.VAPIDKeys, payload push.Payload) state.PushAttempt {
	s.payloads = append(s.payloads, payload)
	if attempt, ok := s.results[sub.ID]; ok {
		attempt.SubscriptionID = sub.ID
		return attempt
	}
	return state.PushAttempt{At: time.Now().UTC(), SubscriptionID: sub.ID, AlertID: payload.AlertID, OK: true, Status: "202 Accepted"}
}
