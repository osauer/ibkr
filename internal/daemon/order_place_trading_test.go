//go:build trading

package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

func TestOrderPlaceConsumesAcceptedTokenAndJournalsSendAttempt(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	srv.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	var sentOrder *ibkrlib.RawOrder
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		copy := *order
		sentOrder = &copy
		return nil
	}

	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{
		Status:    rpc.OrderWhatIfStatusAccepted,
		Available: true,
	})
	res, err := srv.placeOrder(context.Background(), rpc.OrderPlaceParams{PreviewToken: token})
	if err != nil {
		t.Fatalf("placeOrder err = %v", err)
	}
	if !res.Accepted || res.ReservedOrderID != 1001 || res.LifecycleStatus != rpc.OrderLifecyclePendingSubmit {
		t.Fatalf("place result = %+v", res)
	}
	if sentOrder == nil || sentOrder.OrderID != 1001 || sentOrder.Account != "DU1234567" || sentOrder.Action != rpc.OrderActionBuy {
		t.Fatalf("sent order = %+v", sentOrder)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 2 || events[0].Type != orderJournalEventTokenConfirmed || events[1].Type != orderJournalEventSendAttempted {
		t.Fatalf("events = %+v", events)
	}
	if events[1].ReservedOrderID != 1001 || events[1].SendState != orderSendStateSendAttempted {
		t.Fatalf("send event = %+v", events[1])
	}

	_, err = srv.placeOrder(context.Background(), rpc.OrderPlaceParams{PreviewToken: token})
	if !errors.Is(err, ErrTradingDisabled) || !errors.Is(err, errOrderPreviewTokenAlreadyUsed) {
		t.Fatalf("second place err = %v, want token-used trading refusal", err)
	}
}

func TestOrderPlaceRejectsRejectedWhatIfTokenBeforeBrokerSend(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	srv.orderReserveBrokerID = func(context.Context) (int, error) {
		t.Fatal("order ID should not be reserved for rejected WhatIf token")
		return 0, nil
	}
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("broker send should not run for rejected WhatIf token")
		return nil
	}

	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{
		Status:            rpc.OrderWhatIfStatusRejected,
		RequiredForSubmit: true,
		Available:         true,
		Message:           "broker rejected",
	})
	_, err := srv.placeOrder(context.Background(), rpc.OrderPlaceParams{PreviewToken: token})
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "accepted broker WhatIf") {
		t.Fatalf("placeOrder err = %v, want accepted WhatIf refusal", err)
	}
}

func TestOrderCancelAppendsPendingCancelWithoutTerminalState(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	now := time.Date(2026, 5, 28, 9, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	if err := srv.orderJournal.Append(orderJournalEvent{
		At:              now.Add(-time.Minute),
		Type:            orderJournalEventBrokerAcknowledged,
		OrderRef:        "ord-1",
		PreviewTokenID:  "tok-1",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Symbol:          "AAPL",
		SecType:         "STK",
		Action:          "BUY",
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        1,
		LimitPrice:      100,
		Status:          "Submitted",
		SendState:       orderSendStateBrokerAcknowledged,
	}); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	var cancelled int
	srv.orderCancelBroker = func(_ context.Context, orderID int) error {
		cancelled = orderID
		return nil
	}

	res, err := srv.cancelOrder(context.Background(), rpc.OrderCancelParams{ID: "ord-1"})
	if err != nil {
		t.Fatalf("cancelOrder err = %v", err)
	}
	if !res.Accepted || cancelled != 1001 || res.LifecycleStatus != rpc.OrderLifecyclePendingCancel || !res.Order.Open || res.Order.ModifyEligible || res.Order.CancelEligible {
		t.Fatalf("cancel result = %+v cancelled=%d", res, cancelled)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if events[len(events)-1].Type != orderJournalEventCancelRequested {
		t.Fatalf("last event = %+v", events[len(events)-1])
	}
}

func TestOrderModifyRejectsSymbolChange(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	now := time.Date(2026, 5, 28, 9, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	if err := srv.orderJournal.Append(orderJournalEvent{
		At:              now.Add(-time.Minute),
		Type:            orderJournalEventBrokerAcknowledged,
		OrderRef:        "ord-1",
		PreviewTokenID:  "tok-1",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Symbol:          "MSFT",
		SecType:         "STK",
		Action:          "BUY",
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        2,
		Remaining:       2,
		LimitPrice:      250,
		Status:          "Submitted",
		SendState:       orderSendStateBrokerAcknowledged,
	}); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{
		Status:    rpc.OrderWhatIfStatusAccepted,
		Available: true,
	})
	_, err := srv.modifyOrder(context.Background(), rpc.OrderModifyParams{ID: "ord-1", PreviewToken: token})
	if err == nil || !strings.Contains(err.Error(), "cannot be used for modify") {
		t.Fatalf("modifyOrder err = %v, want place-token scope refusal", err)
	}

	token = mintModifyPreviewTokenForWriteTest(t, srv, rpc.OrderView{
		OrderRef:        "ord-1",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Symbol:          "MSFT",
		SecType:         "STK",
		Action:          "BUY",
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        2,
		Remaining:       2,
		LimitPrice:      250,
		Status:          "Submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       orderSendStateBrokerAcknowledged,
	}, rpc.OrderDraft{
		Action:     rpc.OrderActionBuy,
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		Quantity:   1,
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: 100,
		TIF:        rpc.OrderTIFDay,
		Strategy:   rpc.OrderStrategyExplicitLimit,
		OrderRef:   "ibkr-20260528-084500",
	})
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("broker modify should not run for symbol change")
		return nil
	}

	_, err = srv.modifyOrder(context.Background(), rpc.OrderModifyParams{ID: "ord-1", PreviewToken: token})
	if err == nil || !strings.Contains(err.Error(), "cannot change symbol") {
		t.Fatalf("modifyOrder err = %v, want symbol-change refusal", err)
	}
}

func mintModifyPreviewTokenForWriteTest(t *testing.T, srv *Server, view rpc.OrderView, draft rpc.OrderDraft) string {
	t.Helper()
	token, _, _, err := srv.orderTokens.mint(orderPreviewTokenPayload{
		Scope:    rpc.OrderTokenScopeModify,
		Mode:     "paper",
		Account:  "DU1234567",
		Endpoint: "127.0.0.1:4002",
		ClientID: 31,
		Draft:    draft,
		Quote:    rpc.OrderQuoteSnapshot{Symbol: draft.Contract.Symbol},
		Position: rpc.OrderPositionImpact{
			Before: 0,
			After:  float64(draft.Quantity),
			Effect: rpc.OrderPositionEffectOpen,
		},
		Notional: 100,
		WhatIf: rpc.OrderWhatIfResult{
			Status:    rpc.OrderWhatIfStatusAccepted,
			Available: true,
		},
		WhatIfStatus: rpc.OrderWhatIfStatusAccepted,
		Replace:      replaceTargetFromView(view),
	})
	if err != nil {
		t.Fatalf("mint modify preview token: %v", err)
	}
	return token
}
