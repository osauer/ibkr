package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/discover"
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

func TestOrdersOpenCurrentContextRequiresConcreteAccountAndMode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 8, 18, 30, 0, 0, time.UTC)
	srv := &Server{
		orderJournal: newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl")),
		endpoint: discover.Endpoint{
			Host:    "127.0.0.1",
			Port:    7496,
			Account: "All",
		},
		now: func() time.Time { return now },
	}
	events := []orderJournalEvent{
		{
			At:              now.Add(-time.Hour),
			Type:            orderJournalEventBrokerAcknowledged,
			OrderRef:        "paper-sap",
			ReservedOrderID: 7,
			Account:         "DU3136804",
			Endpoint:        "127.0.0.1:7497",
			Mode:            rpc.AccountModePaper,
			Symbol:          "SAP",
			SecType:         "STK",
			Action:          rpc.OrderActionBuy,
			OrderType:       rpc.OrderTypeLMT,
			TIF:             rpc.OrderTIFDay,
			Quantity:        1,
			Remaining:       1,
			Status:          "Submitted",
			SendState:       orderSendStateBrokerAcknowledged,
		},
		{
			At:              now,
			Type:            orderJournalEventBrokerAcknowledged,
			OrderRef:        "live-aapl",
			ReservedOrderID: 8,
			Account:         "U1234567",
			Endpoint:        "127.0.0.1:7496",
			Mode:            rpc.AccountModeLive,
			Symbol:          "AAPL",
			SecType:         "STK",
			Action:          rpc.OrderActionSell,
			OrderType:       rpc.OrderTypeLMT,
			TIF:             rpc.OrderTIFDay,
			Quantity:        1,
			Remaining:       1,
			Status:          "Submitted",
			SendState:       orderSendStateBrokerAcknowledged,
		},
	}
	if err := srv.orderJournal.AppendAll(events); err != nil {
		t.Fatalf("append orders: %v", err)
	}
	raw, err := json.Marshal(rpc.OrdersOpenParams{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.handleOrdersOpen(context.Background(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatalf("handleOrdersOpen: %v", err)
	}
	if len(res.Orders) != 0 {
		t.Fatalf("aggregate open orders = %+v, want no concrete-account rows", res.Orders)
	}

	srv.endpoint.Account = "U1234567"
	res, err = srv.handleOrdersOpen(context.Background(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatalf("handleOrdersOpen concrete account: %v", err)
	}
	if len(res.Orders) != 1 || res.Orders[0].OrderRef != "live-aapl" {
		t.Fatalf("concrete account open orders = %+v, want only current live row", res.Orders)
	}

	status, err := srv.handleOrderStatus(context.Background(), &rpc.Request{Params: mustJSON(t, rpc.OrderStatusParams{ID: "paper-sap"})})
	if err != nil {
		t.Fatalf("paper order status: %v", err)
	}
	if status.Found {
		t.Fatalf("paper order status found in live context: %+v", status)
	}
}

func TestOrdersOpenUsesPinnedConcreteAccountWhenConnectorReportsAll(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 9, 19, 55, 0, 0, time.UTC)
	connector := ibkrlib.NewConnector(nil)
	connector.SeedAccountIDForTest("All")
	srv := &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: new(7497), Account: "DU3136804"},
		},
		connector:    connector,
		orderJournal: newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl")),
		endpoint: discover.Endpoint{
			Host:    "127.0.0.1",
			Port:    7497,
			Account: "DU3136804",
		},
		now: func() time.Time { return now },
	}
	events := []orderJournalEvent{
		{
			At:              now.Add(-time.Minute),
			Type:            orderJournalEventBrokerAcknowledged,
			OrderRef:        "paper-mbg",
			ReservedOrderID: 45,
			PermID:          157796279,
			Account:         "DU3136804",
			Endpoint:        "127.0.0.1:7497",
			Mode:            rpc.AccountModePaper,
			Symbol:          "MBG",
			SecType:         "STK",
			Action:          rpc.OrderActionSell,
			OrderType:       rpc.OrderTypeTRAIL,
			TIF:             rpc.OrderTIFDay,
			Quantity:        1,
			Remaining:       1,
			Status:          "PreSubmitted",
			SendState:       orderSendStateBrokerAcknowledged,
		},
		{
			At:              now,
			Type:            orderJournalEventBrokerAcknowledged,
			OrderRef:        "live-aapl",
			ReservedOrderID: 46,
			Account:         "U1234567",
			Endpoint:        "127.0.0.1:7496",
			Mode:            rpc.AccountModeLive,
			Symbol:          "AAPL",
			SecType:         "STK",
			Action:          rpc.OrderActionSell,
			OrderType:       rpc.OrderTypeLMT,
			TIF:             rpc.OrderTIFDay,
			Quantity:        1,
			Remaining:       1,
			Status:          "Submitted",
			SendState:       orderSendStateBrokerAcknowledged,
		},
	}
	if err := srv.orderJournal.AppendAll(events); err != nil {
		t.Fatalf("append orders: %v", err)
	}
	res, err := srv.handleOrdersOpen(context.Background(), &rpc.Request{Params: mustJSON(t, rpc.OrdersOpenParams{})})
	if err != nil {
		t.Fatalf("handleOrdersOpen: %v", err)
	}
	if len(res.Orders) != 1 || res.Orders[0].OrderRef != "paper-mbg" {
		t.Fatalf("open orders = %+v, want pinned paper order despite connected All", res.Orders)
	}
	status, err := srv.handleOrderStatus(context.Background(), &rpc.Request{Params: mustJSON(t, rpc.OrderStatusParams{ID: "paper-mbg"})})
	if err != nil {
		t.Fatalf("paper order status: %v", err)
	}
	if !status.Found || status.Order.OrderRef != "paper-mbg" {
		t.Fatalf("order status = %+v, want pinned paper order", status)
	}
}

func TestOrdersHistoryFiltersRangeLimitAndCurrentScope(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	srv := &Server{
		orderJournal: newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl")),
		endpoint: discover.Endpoint{
			Host:    "127.0.0.1",
			Port:    7496,
			Account: "U1234567",
		},
		now: func() time.Time { return now },
	}
	events := []orderJournalEvent{
		{
			At:              time.Date(2026, 6, 17, 15, 0, 0, 0, time.UTC),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "old-live",
			ReservedOrderID: 1,
			Account:         "U1234567",
			Mode:            rpc.AccountModeLive,
			Symbol:          "OLD",
			Status:          "Filled",
			Filled:          1,
			SendState:       orderSendStateTerminal,
		},
		{
			At:              time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "live-first",
			ReservedOrderID: 2,
			Account:         "U1234567",
			Mode:            rpc.AccountModeLive,
			Symbol:          "AAPL",
			Action:          rpc.OrderActionBuy,
			OrderType:       rpc.OrderTypeLMT,
			TIF:             rpc.OrderTIFDay,
			Quantity:        3,
			Status:          "Submitted",
			Remaining:       3,
			SendState:       orderSendStateBrokerAcknowledged,
		},
		{
			At:              time.Date(2026, 6, 18, 10, 5, 0, 0, time.UTC),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "live-first",
			ReservedOrderID: 2,
			Account:         "U1234567",
			Mode:            rpc.AccountModeLive,
			Symbol:          "AAPL",
			Status:          "Filled",
			Filled:          3,
			Remaining:       0,
			AvgFillPrice:    195.25,
			SendState:       orderSendStateTerminal,
		},
		{
			At:              time.Date(2026, 6, 18, 11, 0, 0, 0, time.UTC),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "paper-same-day",
			ReservedOrderID: 3,
			Account:         "DU1234567",
			Mode:            rpc.AccountModePaper,
			Symbol:          "MSFT",
			Status:          "Filled",
			Filled:          1,
			SendState:       orderSendStateTerminal,
		},
		{
			At:              time.Date(2026, 6, 18, 12, 55, 0, 0, time.UTC),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "live-latest",
			ReservedOrderID: 4,
			Account:         "DU1234567",
			Mode:            rpc.AccountModePaper,
			Symbol:          "AMD",
			Status:          "Filled",
			Filled:          1,
			SendState:       orderSendStateTerminal,
		},
		{
			At:              time.Date(2026, 6, 18, 13, 0, 0, 0, time.UTC),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "live-latest",
			ReservedOrderID: 4,
			Account:         "U1234567",
			Mode:            rpc.AccountModeLive,
			Symbol:          "AMD",
			Status:          "Submitted",
			Remaining:       1,
			SendState:       orderSendStateBrokerAcknowledged,
		},
		{
			At:              time.Date(2026, 6, 18, 13, 5, 0, 0, time.UTC),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "live-latest",
			ReservedOrderID: 4,
			Account:         "U1234567",
			Mode:            rpc.AccountModeLive,
			Symbol:          "AMD",
			Status:          "Submitted",
			Remaining:       1,
			SendState:       orderSendStateBrokerAcknowledged,
		},
		{
			At:              time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "next-day",
			ReservedOrderID: 5,
			Account:         "U1234567",
			Mode:            rpc.AccountModeLive,
			Symbol:          "NEXT",
			Status:          "Filled",
			Filled:          1,
			SendState:       orderSendStateTerminal,
		},
	}
	if err := srv.orderJournal.AppendAll(events); err != nil {
		t.Fatalf("append orders: %v", err)
	}

	res, err := srv.handleOrdersHistory(context.Background(), &rpc.Request{Params: mustJSON(t, rpc.OrdersHistoryParams{
		Since:      "2026-06-18",
		Until:      "2026-06-18",
		Limit:      1,
		EventLimit: 1,
	})})
	if err != nil {
		t.Fatalf("handleOrdersHistory: %v", err)
	}
	if res.Count != 1 || res.TotalCount != 2 || !res.Truncated {
		t.Fatalf("history counts = count %d total %d truncated %v, want 1/2/true", res.Count, res.TotalCount, res.Truncated)
	}
	if res.Account != "U1234567" || res.Mode != rpc.AccountModeLive {
		t.Fatalf("history scope = %q/%q, want live U1234567", res.Account, res.Mode)
	}
	if len(res.Limitations) == 0 || !strings.Contains(strings.ToLower(res.NotBrokerStatement), "not an ibkr activity statement") {
		t.Fatalf("history limitations missing broker-statement warning: %+v", res)
	}
	if len(res.Orders) != 1 || res.Orders[0].Order.OrderRef != "live-latest" {
		t.Fatalf("history rows = %+v, want latest current-scope row", res.Orders)
	}
	row := res.Orders[0]
	if res.EventsCount != 1 || res.TotalEventsCount != 2 || !res.EventsTruncated {
		t.Fatalf("history event counts = returned %d total %d truncated %v, want 1/2/true", res.EventsCount, res.TotalEventsCount, res.EventsTruncated)
	}
	if row.EventsCount != 1 || row.TotalEventsCount != 2 || !row.EventsTruncated || len(row.Events) != 1 {
		t.Fatalf("history row event counts = %+v, want one returned event from two current-scope events", row)
	}
	if !row.Order.UpdatedAt.Equal(time.Date(2026, 6, 18, 13, 5, 0, 0, time.UTC)) {
		t.Fatalf("history row updated_at = %s, want latest in-window current-scope event", row.Order.UpdatedAt)
	}
	for _, row := range res.Orders {
		for _, ev := range row.Events {
			if ev.At.Before(res.Since) || !ev.At.Before(res.Until) {
				t.Fatalf("event outside returned range: %+v in %s..%s", ev, res.Since, res.Until)
			}
			if ev.Mode != rpc.AccountModeLive || ev.Account != "U1234567" {
				t.Fatalf("event crossed broker scope: %+v", ev)
			}
		}
	}
}

func TestBuildOrderViewsFilledClearsStaleRemaining(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 8, 9, 2, 0, 0, time.UTC)
	views := buildOrderViews([]orderJournalEvent{
		{
			At:              base,
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "restore-filled",
			ReservedOrderID: 24,
			Status:          "Submitted",
			Quantity:        1,
			Remaining:       1,
			SendState:       orderSendStateBrokerAcknowledged,
		},
		{
			At:              base.Add(time.Second),
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "restore-filled",
			ReservedOrderID: 24,
			Status:          "Filled",
			Filled:          1,
			Remaining:       0,
			SendState:       orderSendStateTerminal,
		},
	})
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	if views[0].Remaining != 0 || views[0].LifecycleStatus != rpc.OrderLifecycleFilled || views[0].Open {
		t.Fatalf("filled view = %+v, want remaining=0 closed filled", views[0])
	}
}

func TestOrderLifecycleApiPendingIsNotWriteEligible(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 5, 28, 9, 30, 0, 0, time.UTC)
	ev, ok := orderJournalEventFromLifecycle(ibkrlib.OrderLifecycleEvent{
		Type:      ibkrlib.OrderLifecycleEventStatus,
		OrderID:   1001,
		Status:    "ApiPending",
		Remaining: 1,
	}, base)
	if !ok {
		t.Fatal("orderJournalEventFromLifecycle ok=false")
	}
	if ev.SendState != orderSendStateUncertainSend {
		t.Fatalf("ApiPending send state = %q, want uncertain_send", ev.SendState)
	}

	views := buildOrderViews([]orderJournalEvent{ev})
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	got := views[0]
	if got.LifecycleStatus != rpc.OrderLifecyclePendingSubmit {
		t.Fatalf("ApiPending lifecycle = %q, want pending_submit: %+v", got.LifecycleStatus, got)
	}
	if got.ModifyEligible || got.CancelEligible {
		t.Fatalf("ApiPending should not be write-eligible: %+v", got)
	}
}

func TestBuildOrderViewsAliasesBrokerOnlyLifecycleToOrderRef(t *testing.T) {
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
			Type:            orderJournalEventSendAttempted,
			OrderRef:        "ibkr-20260528-093000",
			PreviewTokenID:  "tok_1",
			ReservedOrderID: 1001,
			SendState:       orderSendStateSendAttempted,
		},
		{
			At:              base.Add(2 * time.Second),
			Type:            orderJournalEventStatusUpdated,
			ReservedOrderID: 1001,
			PermID:          987654,
			Status:          "Cancelled",
			SendState:       orderSendStateTerminal,
		},
	}

	views := buildOrderViews(events)
	if len(views) != 1 {
		t.Fatalf("views = %d, want one canonical order-ref row: %+v", len(views), views)
	}
	got := views[0]
	if got.OrderRef != "ibkr-20260528-093000" || got.ReservedOrderID != 1001 || got.PermID != 987654 || got.Status != "Cancelled" {
		t.Fatalf("aliased view fields wrong: %+v", got)
	}
	if got.Open || got.LifecycleStatus != rpc.OrderLifecycleCancelled {
		t.Fatalf("aliased lifecycle = open:%v %s, want closed cancelled: %+v", got.Open, got.LifecycleStatus, got)
	}

	eventsByKey := buildOrderEventsByKey(events)
	if got := len(eventsByKey["ref:ibkr-20260528-093000"]); got != 3 {
		t.Fatalf("canonical events = %d, want 3: %+v", got, eventsByKey)
	}
	if _, ok := eventsByKey["order:1001"]; ok {
		t.Fatalf("broker-only alias should not create separate events row: %+v", eventsByKey)
	}
}

func TestOrderViewWriteEligibilityRequiresBrokerConfirmedState(t *testing.T) {
	t.Parallel()
	base := rpc.OrderView{
		Open:            true,
		ReservedOrderID: 1001,
		SecType:         "STK",
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
	}

	confirmed := base
	confirmed.SendState = orderSendStateBrokerAcknowledged
	confirmed.LifecycleStatus = rpc.OrderLifecycleSubmitted
	if !orderViewModifyEligible(confirmed) || !orderViewCancelEligible(confirmed) {
		t.Fatalf("confirmed submitted order should be write-eligible: %+v", confirmed)
	}

	for _, view := range []rpc.OrderView{
		func() rpc.OrderView {
			v := base
			v.SendState = orderSendStateSendAttempted
			v.LifecycleStatus = rpc.OrderLifecyclePendingSubmit
			return v
		}(),
		func() rpc.OrderView {
			v := base
			v.SendState = orderSendStateUncertainSend
			v.LifecycleStatus = rpc.OrderLifecycleUnknownReconcileRequired
			return v
		}(),
		func() rpc.OrderView {
			v := base
			v.SendState = orderSendStateBrokerAcknowledged
			v.LifecycleStatus = rpc.OrderLifecycleUnknownReconcileRequired
			return v
		}(),
	} {
		if orderViewModifyEligible(view) || orderViewCancelEligible(view) {
			t.Fatalf("non-confirmed/reconcile-required order should not be write-eligible: %+v", view)
		}
	}
}

func TestOrderViewModifyEligibleTrailTIF(t *testing.T) {
	t.Parallel()
	base := rpc.OrderView{
		Open:            true,
		ReservedOrderID: 1001,
		SecType:         "STK",
		SendState:       orderSendStateBrokerAcknowledged,
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
	}
	cases := []struct {
		name      string
		orderType string
		secType   string
		tif       string
		want      bool
	}{
		{"lmt day", rpc.OrderTypeLMT, "STK", rpc.OrderTIFDay, true},
		{"lmt gtc stays ineligible", rpc.OrderTypeLMT, "STK", rpc.OrderTIFGTC, false},
		{"trail day", rpc.OrderTypeTRAIL, "STK", rpc.OrderTIFDay, true},
		{"trail gtc", rpc.OrderTypeTRAIL, "STK", rpc.OrderTIFGTC, true},
		{"trail limit gtc", rpc.OrderTypeTRAILLIMIT, "STK", rpc.OrderTIFGTC, true},
		{"option trail stays ineligible", rpc.OrderTypeTRAIL, "OPT", rpc.OrderTIFGTC, false},
		{"unsupported order type", "STP", "STK", rpc.OrderTIFDay, false},
	}
	for _, tc := range cases {
		view := base
		view.OrderType = tc.orderType
		view.SecType = tc.secType
		view.TIF = tc.tif
		if got := orderViewModifyEligible(view); got != tc.want {
			t.Fatalf("%s: orderViewModifyEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeExecutionLifecycleFullFillBecomesTerminal(t *testing.T) {
	t.Parallel()
	ev := orderJournalEvent{
		Type:      orderJournalEventStatusUpdated,
		Status:    "Execution",
		Quantity:  200,
		Filled:    200,
		Remaining: 200,
		SendState: orderSendStateBrokerAcknowledged,
	}

	normalizeOrderLifecycleJournalEvent(&ev)
	if ev.Status != "Filled" || ev.Remaining != 0 || ev.SendState != orderSendStateTerminal {
		t.Fatalf("event = %+v, want full execution normalized to terminal filled", ev)
	}
}

func TestReconcileFlatPositionProtectiveOrderRequiresBrokerTruth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	views := []rpc.OrderView{{
		Open:            true,
		ModifyEligible:  true,
		CancelEligible:  true,
		Source:          proposalOrderSource,
		Symbol:          "PBLS",
		SecType:         "STK",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		Quantity:        200,
		Remaining:       200,
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
	}}

	reconcileFlatPositionProtectiveOrders(views, &rpc.PositionsResult{}, now)
	got := views[0]
	if !got.Open || !got.CancelEligible || got.ModifyEligible || got.LifecycleStatus != rpc.OrderLifecycleUnknownReconcileRequired {
		t.Fatalf("view = %+v, want open reconcile-required with modify disabled and cancel still visible", got)
	}
	if got.ReconciliationState != "position_mismatch" || got.BrokerTruthAsOf.IsZero() || !strings.Contains(got.LastMessage, "broker reconciliation required") {
		t.Fatalf("reconciliation annotation = %+v, want position_mismatch with broker-truth timestamp", got)
	}
}

func TestReconcileCoveredProtectiveOrderLeavesOrderActionable(t *testing.T) {
	t.Parallel()
	views := []rpc.OrderView{{
		Open:            true,
		ModifyEligible:  true,
		CancelEligible:  true,
		Source:          proposalOrderSource,
		Symbol:          "PBLS",
		SecType:         "STK",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		Quantity:        200,
		Remaining:       200,
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
	}}
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "PBLS", SecType: "STK", Quantity: 250}}}

	reconcileFlatPositionProtectiveOrders(views, pos, time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))
	got := views[0]
	if !got.Open || !got.CancelEligible || !got.ModifyEligible || got.LifecycleStatus != rpc.OrderLifecycleSubmitted || got.ReconciliationState != "" {
		t.Fatalf("view = %+v, want covered protective order left unchanged", got)
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

func TestOrderJournalEventFromLifecycleBrokerError(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 28, 9, 31, 0, 0, time.UTC)
	ev, ok := orderJournalEventFromLifecycle(ibkrlib.OrderLifecycleEvent{
		Type:      ibkrlib.OrderLifecycleEventError,
		OrderID:   1001,
		Status:    "Rejected",
		ErrorCode: 201,
		Message:   `broker order error code 201: Order rejected advanced_reject_json={"reason":"size"}`,
	}, at)
	if !ok {
		t.Fatal("orderJournalEventFromLifecycle ok=false")
	}
	if ev.Type != orderJournalEventBrokerError || ev.SendState != orderSendStateTerminal || ev.Status != "Rejected" {
		t.Fatalf("event = %+v, want terminal broker error", ev)
	}
	views := buildOrderViews([]orderJournalEvent{
		{At: at.Add(-time.Second), Type: orderJournalEventSendAttempted, OrderRef: "ord-1", ReservedOrderID: 1001, SendState: orderSendStateSendAttempted},
		ev,
	})
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	if views[0].Open || views[0].LifecycleStatus != rpc.OrderLifecycleRejected || !strings.Contains(views[0].LastMessage, "advanced_reject_json") {
		t.Fatalf("view = %+v, want closed rejected with broker message", views[0])
	}
}

func TestBuildOrderViewsDoesNotAliasPreexistingBrokerOnlyOrderID(t *testing.T) {
	t.Parallel()
	oldAt := time.Date(2026, 6, 4, 14, 35, 35, 0, time.UTC)
	newAt := time.Date(2026, 6, 8, 8, 51, 51, 0, time.UTC)
	events := []orderJournalEvent{
		{
			At:              oldAt,
			Type:            orderJournalEventStatusUpdated,
			ReservedOrderID: 11,
			PermID:          1995374765,
			Status:          "Filled",
			Filled:          1,
			AvgFillPrice:    49.51,
			LastFillPrice:   49.51,
			SendState:       orderSendStateTerminal,
		},
		{
			At:              newAt,
			Type:            orderJournalEventSendAttempted,
			OrderRef:        "purge-20260608-sap",
			ReservedOrderID: 11,
			Source:          purgeExecuteSource,
			LegID:           "leg_sap",
			Symbol:          "SAP",
			SecType:         "STK",
			Action:          rpc.OrderActionSell,
			Quantity:        1,
			LimitPrice:      159.53,
			SendState:       orderSendStateSendAttempted,
		},
		{
			At:              newAt.Add(time.Second),
			Type:            orderJournalEventBrokerError,
			OrderRef:        "purge-20260608-sap",
			ReservedOrderID: 11,
			Source:          purgeExecuteSource,
			LegID:           "leg_sap",
			Symbol:          "SAP",
			SecType:         "STK",
			Action:          rpc.OrderActionSell,
			Quantity:        1,
			LimitPrice:      159.53,
			SendState:       orderSendStateUncertainSend,
			Message:         "broker error 110: The price does not conform to the minimum price variation for this contract.",
		},
	}

	views := buildOrderViews(events)
	if len(views) != 2 {
		t.Fatalf("views = %d, want old broker-only row plus new purge row: %+v", len(views), views)
	}
	var purgeView, oldView *rpc.OrderView
	for i := range views {
		switch views[i].OrderRef {
		case "purge-20260608-sap":
			purgeView = &views[i]
		case "":
			oldView = &views[i]
		}
	}
	if purgeView == nil || purgeView.Open || purgeView.LifecycleStatus != rpc.OrderLifecycleRejected || purgeView.Filled != 0 || purgeView.AvgFillPrice != 0 {
		t.Fatalf("purge view = %+v, want closed rejected without old fill fields", purgeView)
	}
	if oldView == nil || oldView.Open || oldView.LifecycleStatus != rpc.OrderLifecycleFilled || oldView.Filled != 1 {
		t.Fatalf("old view = %+v, want separate filled broker-only row", oldView)
	}

	eventsByKey := buildOrderEventsByKey(events)
	if got := len(eventsByKey["ref:purge-20260608-sap"]); got != 2 {
		t.Fatalf("purge canonical events = %d, want 2: %+v", got, eventsByKey)
	}
	if got := len(eventsByKey["order:11"]); got != 1 {
		t.Fatalf("broker-only canonical events = %d, want 1: %+v", got, eventsByKey)
	}
}

func TestBuildOrderViewsModifyBrokerErrorPreservesWorkingOrder(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 5, 28, 9, 30, 0, 0, time.UTC)
	views := buildOrderViews([]orderJournalEvent{
		{
			At:              base,
			Type:            orderJournalEventBrokerAcknowledged,
			OrderRef:        "ord-1",
			ReservedOrderID: 1001,
			Account:         "DU1234567",
			Endpoint:        "127.0.0.1:4002",
			Mode:            "paper",
			Symbol:          "AAPL",
			SecType:         "STK",
			Action:          rpc.OrderActionBuy,
			OrderType:       rpc.OrderTypeLMT,
			TIF:             rpc.OrderTIFDay,
			Quantity:        1,
			Remaining:       1,
			LimitPrice:      190.50,
			Status:          "Submitted",
			SendState:       orderSendStateBrokerAcknowledged,
		},
		{
			At:              base.Add(time.Second),
			Type:            orderJournalEventModifyRequested,
			OrderRef:        "ord-1",
			ReservedOrderID: 1001,
			Quantity:        1,
			Remaining:       1,
			LimitPrice:      189.50,
			Status:          "Submitted",
			SendState:       orderSendStateSendAttempted,
		},
		{
			At:              base.Add(2 * time.Second),
			Type:            orderJournalEventBrokerError,
			ReservedOrderID: 1001,
			Status:          "Rejected",
			SendState:       orderSendStateTerminal,
			Message:         "broker order error code 321: modify rejected",
		},
	})
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	got := views[0]
	if !got.Open || got.Status != "Submitted" || got.LifecycleStatus != rpc.OrderLifecycleUnknownReconcileRequired || got.SendState != orderSendStateUncertainSend {
		t.Fatalf("view after modify reject = %+v, want open submitted reconcile-required", got)
	}
	if got.ModifyEligible || got.CancelEligible {
		t.Fatalf("reconcile-required order should not be write-eligible: %+v", got)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
