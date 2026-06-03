package alerts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/app/push"
	"github.com/osauer/ibkr/internal/app/state"
	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
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

type recordingSender struct {
	payloads []push.Payload
}

func (s *recordingSender) Send(_ context.Context, sub state.PushSubscription, _ state.VAPIDKeys, payload push.Payload) state.PushAttempt {
	s.payloads = append(s.payloads, payload)
	return state.PushAttempt{At: time.Now().UTC(), SubscriptionID: sub.ID, AlertID: payload.AlertID, OK: true, Status: "202 Accepted"}
}
