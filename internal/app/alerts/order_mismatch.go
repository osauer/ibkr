package alerts

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/push"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// OrderMismatchWatch turns daemon-classified protective-order mismatches
// (a stop whose position no longer covers it; triggering would open an
// opposite-direction position) into one push per occurrence. It rides the
// light canary-style alert path — RecordAlertIfNew fingerprint dedupe plus
// web push — never the governance dispatcher.
//
// Debounce: a fingerprint must be observed on two CONSECUTIVE passes before
// it alerts. The positions feed is known to serve transient zeroed reads;
// a one-pass flap (which would misclassify an excess mismatch as flat, or
// invent a mismatch outright) therefore never reaches the trader. The row
// coloring in the app self-corrects on the next poll either way; only the
// push needs this protection because it cannot be un-sent.
type OrderMismatchWatch struct {
	Store         *state.Store
	Sender        push.Sender
	URL           string
	Now           func() time.Time
	TradingStatus func() *rpc.TradingStatus

	mu       sync.Mutex
	lastPass map[string]bool
}

// Observe records and sends only protective-order mismatches seen on two
// consecutive observations. The stored and pushed record is redacted; order
// references and position details remain on authenticated order views.
func (w *OrderMismatchWatch) Observe(ctx context.Context, orders rpc.OrdersOpenResult) {
	if w == nil || w.Store == nil {
		return
	}
	current := map[string]rpc.OrderView{}
	for _, order := range orders.Orders {
		if !order.Open || order.ReconciliationKind == "" {
			continue
		}
		fp := fmt.Sprintf("order-mismatch:%s:%s:%.4g:%.4g",
			order.OrderRef, order.ReconciliationKind, order.Remaining, order.ReduceToQuantity)
		current[fp] = order
	}
	w.mu.Lock()
	previous := w.lastPass
	next := make(map[string]bool, len(current))
	for fp := range current {
		next[fp] = true
	}
	w.lastPass = next
	w.mu.Unlock()

	if w.Store.AlertSettings().Mode == state.AlertModeNone {
		return
	}
	now := time.Now().UTC()
	if w.Now != nil {
		now = w.Now().UTC()
	}
	for fp, order := range current {
		if !previous[fp] {
			continue // first sighting; confirm on the next pass
		}
		rec := orderMismatchRecord(order, fp, now)
		if w.TradingStatus != nil {
			if trading := w.TradingStatus(); trading != nil {
				rec.Account = trading.Account
				rec.Mode = trading.Mode
			}
		}
		created, err := w.Store.RecordAlertIfNew(rec)
		if err != nil || !created {
			continue
		}
		payload := push.Payload{
			Title:    rec.Title,
			Body:     rec.Body,
			URL:      w.URL,
			AlertID:  rec.ID,
			Action:   rec.Action,
			Severity: rec.Severity,
		}
		keys, hasKeys := w.Store.VAPID()
		if w.Sender == nil || !hasKeys {
			continue
		}
		for _, sub := range w.Store.PushSubscriptions() {
			attempt := w.Sender.Send(ctx, sub, keys, payload)
			_ = w.Store.RecordPush(attempt)
		}
	}
}

// orderMismatchRecord is redacted like the canary records: no symbol, no
// quantities, no order references on the push wire — the app's orders tab
// carries the specifics.
func orderMismatchRecord(order rpc.OrderView, fingerprint string, now time.Time) state.AlertRecord {
	body := "A protective stop no longer matches its position; triggering would open an opposite-direction position. Open ibkr to reduce it."
	if order.ReconciliationKind == rpc.OrderReconciliationKindShortEntryFull {
		body = "A protective stop covers a flat position; triggering would open a fresh position. Open ibkr to cancel it."
	}
	idHash := sha256.Sum256([]byte(fingerprint + "\x00" + now.Format(time.RFC3339Nano)))
	return state.AlertRecord{
		ID:          fmt.Sprintf("order-mismatch-%x", idHash[:12]),
		Fingerprint: fingerprint,
		Action:      "order_mismatch",
		Severity:    rpc.OrderReconciliationSeverityCritical,
		Title:       "ibkr: protective stop needs attention",
		Body:        body,
		CreatedAt:   now,
	}
}
