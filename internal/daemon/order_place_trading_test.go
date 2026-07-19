//go:build trading

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/config"
	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

func TestOrderPlaceConsumesAcceptedTokenAndJournalsSendAttempt(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
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
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
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

func TestOrderPlaceOriginPolicy(t *testing.T) {
	t.Parallel()

	// Paper: agent origin is unrestricted and journaled for audit.
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error { return nil }
	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{
		Status:    rpc.OrderWhatIfStatusAccepted,
		Available: true,
	})
	res, err := srv.placeOrder(context.Background(), rpc.OrderPlaceParams{PreviewToken: token, Origin: rpc.OrderOriginAgent})
	if err != nil || !res.Accepted {
		t.Fatalf("paper agent placeOrder = %+v, %v; want accepted", res, err)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var sendEvent *orderJournalEvent
	for i := range events {
		if events[i].Type == orderJournalEventSendAttempted {
			sendEvent = &events[i]
		}
	}
	if sendEvent == nil || sendEvent.Origin != rpc.OrderOriginAgent {
		t.Fatalf("send-attempted journal event = %+v, want origin=agent stamped", sendEvent)
	}

	// Live: all origins inherit the same base broker-write gate. Origin is
	// still journaled for audit, but agent is not a separate live blocker.
	liveSrv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModeLive})
	hasCode := func(blockers []rpc.TradingBlocker, code string) bool {
		for _, b := range blockers {
			if b.Code == code {
				return true
			}
		}
		return false
	}
	agentAuth := liveSrv.brokerWriteAuthorizationForRequest(rpc.OrderOriginAgent)
	if hasCode(agentAuth.Blockers, "live_agent_origin_blocked") {
		t.Fatalf("live agent auth = %+v, want no origin blockers", agentAuth.Blockers)
	}
	humanAuth := liveSrv.brokerWriteAuthorizationForRequest(rpc.OrderOriginHumanTTY)
	if hasCode(humanAuth.Blockers, "live_agent_origin_blocked") {
		t.Fatalf("live human auth = %+v, want no origin blockers", humanAuth.Blockers)
	}
	pairedAuth := liveSrv.brokerWriteAuthorizationForRequest(rpc.OrderOriginPairedDevice)
	if hasCode(pairedAuth.Blockers, "live_agent_origin_blocked") {
		t.Fatalf("live paired-device auth = %+v, want no origin blockers", pairedAuth.Blockers)
	}
	if len(agentAuth.Blockers) != len(humanAuth.Blockers) || len(agentAuth.Blockers) != len(pairedAuth.Blockers) {
		t.Fatalf("live origin auth blockers differ: agent=%+v human=%+v paired=%+v", agentAuth.Blockers, humanAuth.Blockers, pairedAuth.Blockers)
	}
}

func TestSubmitConfiguredOrderRejectsBlockedLiveBeforeBrokerHook(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("blocked live mode must be rejected before broker hook")
		return nil
	}
	err := srv.submitConfiguredOrder(context.Background(), rpc.TradingStatus{Mode: config.TradingModeLive,
		Account:  "U1234567",
		Endpoint: "127.0.0.1:4001",
		ClientID: 31,
		Blockers: []rpc.TradingBlocker{{
			Code:    "gateway_port_unpinned",
			Message: "order submission requires a pinned gateway port",
			Action:  "Set [gateway].port.",
		}},
	}, &ibkrlib.Contract{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"}, &ibkrlib.RawOrder{Action: rpc.OrderActionSell, TotalQty: 1, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay})
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "pinned gateway port") {
		t.Fatalf("submitConfiguredOrder err = %v, want live-readiness trading refusal", err)
	}
}

func TestSubmitConfiguredOrderRoutesAuthorizedLiveToBrokerHook(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	var sent bool
	srv.orderPlaceBroker = func(_ context.Context, contract *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		sent = true
		if contract.Symbol != "MSFT" || order.Account != "U1234567" {
			t.Fatalf("broker hook contract/order = %+v / %+v", contract, order)
		}
		return nil
	}
	err := srv.submitConfiguredOrder(context.Background(), rpc.TradingStatus{Mode: config.TradingModeLive,
		Account:  "U1234567",
		Endpoint: "127.0.0.1:4001",
		ClientID: 31,
	}, &ibkrlib.Contract{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"}, &ibkrlib.RawOrder{Action: rpc.OrderActionSell, TotalQty: 1, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Account: "U1234567"})
	if err != nil {
		t.Fatalf("submitConfiguredOrder err = %v", err)
	}
	if !sent {
		t.Fatal("broker hook was not called for authorized live route")
	}
}

func TestOrderCancelAppendsPendingCancelWithoutTerminalState(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
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
	// The cancel attempt must not regress the order's transmit state: a
	// missed cancel confirmation previously wedged the row write-ineligible
	// forever (send_state downgraded to send_attempted).
	if res.Order.SendState != orderSendStateBrokerAcknowledged {
		t.Fatalf("result send_state = %q, want preserved broker_acknowledged", res.Order.SendState)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != orderJournalEventCancelRequested {
		t.Fatalf("last event = %+v", last)
	}
	if last.SendState != "" {
		t.Fatalf("cancel event send_state = %q, want empty (merge keeps broker_acknowledged)", last.SendState)
	}
	// Re-fold: with the sticky broker Status and preserved send_state the
	// row stays cancel-RETRYABLE if no cancel confirmation ever arrives.
	views, _, err := srv.loadOrderViews()
	if err != nil {
		t.Fatalf("loadOrderViews: %v", err)
	}
	var view rpc.OrderView
	for _, v := range views {
		if v.OrderRef == "ord-1" {
			view = v
		}
	}
	if view.SendState != orderSendStateBrokerAcknowledged || !view.Open || !view.CancelEligible {
		t.Fatalf("re-folded view = open=%v send_state=%q cancel_eligible=%v, want open broker_acknowledged cancel-retryable", view.Open, view.SendState, view.CancelEligible)
	}
}

func TestOrderModifyRejectsSymbolChange(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
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

func seedTrailGTCOrderJournal(t *testing.T, srv *Server, now time.Time) {
	t.Helper()
	if err := srv.orderJournal.Append(orderJournalEvent{
		At:              now.Add(-time.Minute),
		Type:            orderJournalEventBrokerAcknowledged,
		OrderRef:        "ord-trail",
		PreviewTokenID:  "tok-1",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Symbol:          "DTE",
		SecType:         "STK",
		Exchange:        "IBIS",
		Currency:        "EUR",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		TIF:             rpc.OrderTIFGTC,
		Quantity:        5,
		Remaining:       5,
		Trail: &rpc.OrderTrailSpec{
			Basis:            rpc.OrderTrailBasisInstrumentPrice,
			OffsetType:       rpc.OrderTrailOffsetPercent,
			TrailingPercent:  new(3.0),
			InitialStopPrice: 198.5,
		},
		Status:    "Submitted",
		SendState: orderSendStateBrokerAcknowledged,
	}); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
}

func trailGTCOrderViewForToken() rpc.OrderView {
	return rpc.OrderView{
		OrderRef:        "ord-trail",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Symbol:          "DTE",
		SecType:         "STK",
		Exchange:        "IBIS",
		Currency:        "EUR",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		TIF:             rpc.OrderTIFGTC,
		Quantity:        5,
		Remaining:       5,
		Trail: &rpc.OrderTrailSpec{
			Basis:            rpc.OrderTrailBasisInstrumentPrice,
			OffsetType:       rpc.OrderTrailOffsetPercent,
			TrailingPercent:  new(3.0),
			InitialStopPrice: 198.5,
		},
		Status:          "Submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       orderSendStateBrokerAcknowledged,
	}
}

func trailGTCModifyDraft() rpc.OrderDraft {
	return rpc.OrderDraft{
		Action:    rpc.OrderActionSell,
		Contract:  rpc.ContractParams{Symbol: "DTE", SecType: "STK", Exchange: "IBIS", Currency: "EUR"},
		Quantity:  5,
		OrderType: rpc.OrderTypeTRAIL,
		TIF:       rpc.OrderTIFGTC,
		Strategy:  rpc.OrderStrategyBrokerTrail,
		OrderRef:  "ibkr-20260610-090000",
		Trail: &rpc.OrderTrailSpec{
			Basis:            rpc.OrderTrailBasisInstrumentPrice,
			OffsetType:       rpc.OrderTrailOffsetPercent,
			TrailingPercent:  new(2.0),
			InitialStopPrice: 199.25,
		},
	}
}

func TestOrderModifyTrailGTCAmendsInPlace(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	seedTrailGTCOrderJournal(t, srv, now)
	var sentOrder *ibkrlib.RawOrder
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		copy := *order
		sentOrder = &copy
		return nil
	}

	token := mintModifyPreviewTokenForWriteTest(t, srv, trailGTCOrderViewForToken(), trailGTCModifyDraft())
	res, err := srv.modifyOrder(context.Background(), rpc.OrderModifyParams{ID: "ord-trail", PreviewToken: token})
	if err != nil {
		t.Fatalf("modifyOrder err = %v", err)
	}
	if !res.Accepted || res.ReservedOrderID != 1001 || res.OrderRef != "ord-trail" {
		t.Fatalf("modify result = %+v, want amend bound to broker order 1001", res)
	}
	if sentOrder == nil || sentOrder.OrderID != 1001 {
		t.Fatalf("sent order = %+v, want re-transmit on broker order ID 1001", sentOrder)
	}
	if sentOrder.OrderType != rpc.OrderTypeTRAIL || sentOrder.TIF != rpc.OrderTIFGTC ||
		sentOrder.TrailingPercent != 2 || sentOrder.TrailStopPrice != 199.25 || sentOrder.LmtPrice != 0 {
		t.Fatalf("sent order = %+v, want GTC TRAIL 2%% stop 199.25 without limit price", sentOrder)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != orderJournalEventModifyRequested || last.OrderRef != "ord-trail" || last.ReservedOrderID != 1001 {
		t.Fatalf("last event = %+v, want modify-requested on the original order", last)
	}
	if last.Trail == nil || last.Trail.TrailingPercent == nil || *last.Trail.TrailingPercent != 2 {
		t.Fatalf("modify event trail = %+v, want re-priced trail journaled", last.Trail)
	}
}

func TestOrderModifyTrailRejectsTIFChange(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	seedTrailGTCOrderJournal(t, srv, now)
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("broker modify should not run for TIF change")
		return nil
	}

	draft := trailGTCModifyDraft()
	draft.TIF = rpc.OrderTIFDay
	token := mintModifyPreviewTokenForWriteTest(t, srv, trailGTCOrderViewForToken(), draft)
	_, err := srv.modifyOrder(context.Background(), rpc.OrderModifyParams{ID: "ord-trail", PreviewToken: token})
	if err == nil || !strings.Contains(err.Error(), "cannot change time-in-force") {
		t.Fatalf("modifyOrder err = %v, want TIF-change refusal", err)
	}
}

func TestOrderModifyTrailRejectsStaleTrailBinding(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	seedTrailGTCOrderJournal(t, srv, now)
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("broker modify should not run for a stale trail binding")
		return nil
	}

	staleView := trailGTCOrderViewForToken()
	staleView.Trail.InitialStopPrice = 195
	token := mintModifyPreviewTokenForWriteTest(t, srv, staleView, trailGTCModifyDraft())
	_, err := srv.modifyOrder(context.Background(), rpc.OrderModifyParams{ID: "ord-trail", PreviewToken: token})
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "trail fields changed") {
		t.Fatalf("modifyOrder err = %v, want stale trail-binding refusal", err)
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

func TestTradingFreezeBlocksPlaceAllowsCancel(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
	if err != nil {
		t.Fatalf("newPlatformSettingsStore: %v", err)
	}
	srv.platformSettings = store
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"freeze":true}}`)}); err != nil {
		t.Fatalf("engage freeze: %v", err)
	}
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
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("placeOrder must not reach the broker while frozen")
		return nil
	}
	var cancelled int
	srv.orderCancelBroker = func(_ context.Context, orderID int) error {
		cancelled = orderID
		return nil
	}

	_, err = srv.placeOrder(context.Background(), rpc.OrderPlaceParams{PreviewToken: "tok"})
	if err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("placeOrder while frozen err = %v, want trading_frozen block", err)
	}
	_, err = srv.modifyOrder(context.Background(), rpc.OrderModifyParams{ID: "ord-1", PreviewToken: "tok"})
	if err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("modifyOrder while frozen err = %v, want trading_frozen block", err)
	}

	res, err := srv.cancelOrder(context.Background(), rpc.OrderCancelParams{ID: "ord-1"})
	if err != nil {
		t.Fatalf("cancelOrder while frozen err = %v, want success", err)
	}
	if !res.Accepted || cancelled != 1001 || res.LifecycleStatus != rpc.OrderLifecyclePendingCancel {
		t.Fatalf("cancel result = %+v cancelled=%d, want accepted pending-cancel", res, cancelled)
	}
}
