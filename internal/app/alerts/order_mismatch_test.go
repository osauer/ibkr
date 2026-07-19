package alerts

import (
	"context"
	"strings"
	"testing"

	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func mismatchOrdersResult(kind string, remaining, reduceTo float64) rpc.OrdersOpenResult {
	return rpc.OrdersOpenResult{Orders: []rpc.OrderView{{
		OrderRef:               "ord-1",
		Symbol:                 "AMD",
		Action:                 rpc.OrderActionSell,
		Open:                   true,
		Remaining:              remaining,
		ReconciliationKind:     kind,
		ReconciliationSeverity: rpc.OrderReconciliationSeverityCritical,
		ShortRiskQuantity:      remaining - reduceTo,
		ReduceToQuantity:       reduceTo,
	}}}
}

func TestOrderMismatchWatchAlertsOnSecondConsecutivePass(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	watch := &OrderMismatchWatch{Store: store}
	orders := mismatchOrdersResult(rpc.OrderReconciliationKindShortEntryExcess, 100, 50)

	watch.Observe(context.Background(), orders)
	if got := store.AlertHistory(10); len(got) != 0 {
		t.Fatalf("first pass alerted immediately: %+v", got)
	}
	watch.Observe(context.Background(), orders)
	got := store.AlertHistory(10)
	if len(got) != 1 {
		t.Fatalf("second consecutive pass should alert once, got %d", len(got))
	}
	rec := got[0]
	if rec.Action != "order_mismatch" || rec.Severity != rpc.OrderReconciliationSeverityCritical {
		t.Fatalf("record = %+v", rec)
	}
	if strings.Contains(rec.Title+rec.Body, "AMD") || strings.Contains(rec.Body, "100") {
		t.Fatalf("push record must stay redacted, got %+v", rec)
	}
	// Third pass: fingerprint dedupe keeps history at one.
	watch.Observe(context.Background(), orders)
	if got := store.AlertHistory(10); len(got) != 1 {
		t.Fatalf("repeat pass duplicated the alert: %d records", len(got))
	}
}

func TestOrderMismatchWatchFlapDoesNotAlert(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	watch := &OrderMismatchWatch{Store: store}
	// Transient zeroed positions: one pass classifies full, next pass the
	// feed recovers and the mismatch disappears entirely.
	watch.Observe(context.Background(), mismatchOrdersResult(rpc.OrderReconciliationKindShortEntryFull, 100, 0))
	watch.Observe(context.Background(), rpc.OrdersOpenResult{Orders: []rpc.OrderView{{OrderRef: "ord-1", Open: true}}})
	watch.Observe(context.Background(), mismatchOrdersResult(rpc.OrderReconciliationKindShortEntryFull, 100, 0))
	if got := store.AlertHistory(10); len(got) != 0 {
		t.Fatalf("flapping mismatch alerted: %+v", got)
	}
}

func TestOrderMismatchWatchRespectsAlertModeNone(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	if err := store.SetAlertMode(state.AlertModeNone); err != nil {
		t.Fatalf("SetAlertMode: %v", err)
	}
	watch := &OrderMismatchWatch{Store: store}
	orders := mismatchOrdersResult(rpc.OrderReconciliationKindShortEntryExcess, 100, 50)
	watch.Observe(context.Background(), orders)
	watch.Observe(context.Background(), orders)
	if got := store.AlertHistory(10); len(got) != 0 {
		t.Fatalf("alert-mode none still recorded: %+v", got)
	}
}
