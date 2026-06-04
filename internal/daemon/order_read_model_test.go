package daemon

import (
	"testing"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestBuildOrderViewsFromJournalLifecycle(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 5, 28, 9, 30, 0, 0, time.UTC)
	events := []orderJournalEvent{
		{
			At:             base,
			Type:           orderJournalEventPreviewed,
			OrderRef:       "ibkr-20260528-093000",
			PreviewTokenID: "tok_1",
			Account:        "DU1234567",
			Endpoint:       "127.0.0.1:4002",
			Mode:           "paper",
			Symbol:         "AAPL",
			SecType:        "STK",
			Action:         rpc.OrderActionBuy,
			OrderType:      rpc.OrderTypeLMT,
			TIF:            rpc.OrderTIFDay,
			Quantity:       10,
			LimitPrice:     190.50,
		},
		{
			At:              base.Add(time.Second),
			Type:            orderJournalEventBrokerAcknowledged,
			OrderRef:        "ibkr-20260528-093000",
			ReservedOrderID: 1001,
			PermID:          987654,
			Status:          "Submitted",
			SendState:       orderSendStateBrokerAcknowledged,
		},
		{
			At:              base.Add(2 * time.Second),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "ibkr-20260528-093000",
			ReservedOrderID: 1001,
			PermID:          987654,
			Status:          "Submitted",
			Filled:          2,
			Remaining:       8,
			SendState:       orderSendStateBrokerAcknowledged,
		},
	}

	views := buildOrderViews(events)
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	got := views[0]
	if !got.Open || got.LifecycleStatus != rpc.OrderLifecyclePartiallyFilled {
		t.Fatalf("view lifecycle = open:%v %s, want open partially_filled: %+v", got.Open, got.LifecycleStatus, got)
	}
	if got.OrderRef != "ibkr-20260528-093000" || got.ReservedOrderID != 1001 || got.PermID != 987654 || got.Filled != 2 || got.Remaining != 8 {
		t.Fatalf("view fields wrong: %+v", got)
	}
}

func TestBuildOrderViewsTerminalStatusNotOpen(t *testing.T) {
	t.Parallel()
	events := []orderJournalEvent{
		{
			At:              time.Date(2026, 5, 28, 9, 30, 0, 0, time.UTC),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "ibkr-20260528-093000",
			ReservedOrderID: 1001,
			Status:          "Cancelled",
			SendState:       orderSendStateTerminal,
		},
	}

	views := buildOrderViews(events)
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	if views[0].Open || views[0].LifecycleStatus != rpc.OrderLifecycleCancelled {
		t.Fatalf("terminal view = open:%v %s, want closed cancelled: %+v", views[0].Open, views[0].LifecycleStatus, views[0])
	}
}

func TestOrderJournalEventFromLifecycle(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 28, 9, 31, 0, 0, time.UTC)
	ev, ok := orderJournalEventFromLifecycle(ibkrlib.OrderLifecycleEvent{
		Type:          ibkrlib.OrderLifecycleEventStatus,
		OrderID:       1001,
		PermID:        987654,
		ClientID:      31,
		Status:        "Filled",
		Filled:        10,
		Remaining:     0,
		AvgFillPrice:  190.30,
		LastFillPrice: 190.30,
	}, at)
	if !ok {
		t.Fatal("orderJournalEventFromLifecycle ok=false")
	}
	if ev.Type != orderJournalEventStatusUpdated || ev.SendState != orderSendStateTerminal {
		t.Fatalf("event type/state = %s/%s, want status-updated/terminal", ev.Type, ev.SendState)
	}
	if ev.ReservedOrderID != 1001 || ev.PermID != 987654 || ev.Status != "Filled" || ev.Filled != 10 {
		t.Fatalf("event fields wrong: %+v", ev)
	}
}
