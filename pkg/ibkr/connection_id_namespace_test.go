package ibkr

import (
	"bufio"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBrokerIDNamespaceMixedConcurrentAllocationsAreUnique(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.observeNextValidOrderID(1)

	const count = 256
	ids := make(chan int, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(order bool) {
			defer wg.Done()
			if order {
				ids <- conn.GetNextOrderID()
				return
			}
			ids <- conn.GetNextRequestID()
		}(i%2 == 0)
	}
	wg.Wait()
	close(ids)

	seen := make(map[int]struct{}, count)
	for id := range ids {
		if id <= 0 || id > maxProtoInt32 {
			t.Fatalf("allocated broker id %d outside valid range", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate broker id %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("unique broker IDs = %d, want %d", len(seen), count)
	}
}

func TestBrokerIDNamespaceNextValidCannotRegressAcrossReconnect(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)

	conn.processMessage(conn.encodeMsg(msgNextValidID, "1", "100"))
	if got := conn.GetNextRequestID(); got != 100 {
		t.Fatalf("first request id = %d, want 100", got)
	}
	if got := conn.GetNextOrderID(); got != 101 {
		t.Fatalf("first order id = %d, want 101", got)
	}

	conn.resetOrderIDReadiness()
	if got := conn.GetNextOrderID(); got != 0 {
		t.Fatalf("order id before reconnect nextValidId = %d, want 0", got)
	}
	conn.processMessage(conn.encodeMsg(msgNextValidID, "1", "50"))
	if got := conn.GetNextOrderID(); got != 102 {
		t.Fatalf("order id after lower reconnect nextValidId = %d, want 102", got)
	}
	if got := conn.GetNextRequestID(); got != 103 {
		t.Fatalf("request id after reconnect order = %d, want 103", got)
	}
}

func TestBrokerIDReservationsBindExactSocketEpochAcrossReconnect(t *testing.T) {
	t.Run("order", func(t *testing.T) {
		conn := NewConnection(nil)
		t.Cleanup(conn.rateLimiter.Stop)
		conn.observeNextValidOrderID(100)

		id, epoch, err := conn.reserveNextOrderID()
		if err != nil || id != 100 {
			t.Fatalf("reserve order id=%d epoch=%d err=%v, want id 100", id, epoch, err)
		}
		conn.resetOrderIDReadiness()
		newEpoch := conn.BrokerSessionEpoch()
		conn.observeNextValidOrderIDAtEpoch(50, newEpoch)

		if _, err := conn.claimOrderIDForEpoch(id, false, epoch); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
			t.Fatalf("stale order reservation claim err=%v, want epoch conflict", err)
		}
		conn.reqIDMu.Lock()
		_, leaked := conn.reservedOrderIDs[id]
		conn.reqIDMu.Unlock()
		if leaked {
			t.Fatal("stale order reservation survived reconnect reset")
		}
		if got := conn.GetNextRequestID(); got != 101 {
			t.Fatalf("request id after stale order reservation=%d, want monotonic 101", got)
		}
	})

	t.Run("request", func(t *testing.T) {
		conn := NewConnection(nil)
		t.Cleanup(conn.rateLimiter.Stop)
		conn.observeNextValidOrderID(100)

		id, epoch, err := conn.nextRequestIDForForwardingWithEpoch()
		if err != nil || id != 100 {
			t.Fatalf("reserve request id=%d epoch=%d err=%v, want id 100", id, epoch, err)
		}
		conn.resetOrderIDReadiness()
		newEpoch := conn.BrokerSessionEpoch()
		conn.observeNextValidOrderIDAtEpoch(50, newEpoch)

		if _, err := conn.claimRequestIDForEpoch(id, epoch); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
			t.Fatalf("stale request reservation claim err=%v, want epoch conflict", err)
		}
		conn.reqIDMu.Lock()
		_, leaked := conn.reservedRequestIDs[id]
		conn.reqIDMu.Unlock()
		if leaked {
			t.Fatal("stale request reservation survived reconnect reset")
		}
		if got := conn.GetNextOrderID(); got != 101 {
			t.Fatalf("order id after stale request reservation=%d, want monotonic 101", got)
		}
	})
}

func TestBrokerIDNamespaceInboundOrderAdvancesBothAllocators(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.observeNextValidOrderID(10)

	conn.processMessage(conn.encodeMsg(msgOrderStatus, "1", "900", "Submitted", "0", "1", "0"))
	if got := conn.GetNextOrderID(); got != 901 {
		t.Fatalf("order id after inbound order 900 = %d, want 901", got)
	}
	if got := conn.GetNextRequestID(); got != 902 {
		t.Fatalf("request id after inbound order 900 = %d, want 902", got)
	}
}

func TestBrokerIDNamespaceOpenOrderInventoryAdvancesLegacyAndProtobuf(t *testing.T) {
	for _, tt := range []struct {
		name   string
		fields []any
	}{
		{name: "legacy", fields: []any{msgOpenOrder, "38", "900", "265598", "TEST", "STK"}},
		{name: "protobuf", fields: []any{msgOpenOrder, "protobuf", "orderId=900", "symbol=TEST"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn := NewConnection(nil)
			t.Cleanup(conn.rateLimiter.Stop)
			conn.observeNextValidOrderID(10)
			conn.processMessage(conn.encodeMsg(tt.fields...))
			if got := conn.GetNextOrderID(); got != 901 {
				t.Fatalf("order id after reqAll/openOrder inventory 900 = %d, want 901", got)
			}
			if got := conn.GetNextRequestID(); got != 902 {
				t.Fatalf("request id after reqAll/openOrder inventory 900 = %d, want 902", got)
			}
		})
	}
}

func TestBrokerIDNamespaceRefusesAutoOrderBeforeNextValidWithoutWire(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	conn.serverVersion = minServerVerProtoBufPlaceOrder
	conn.signalHandshakeReady()
	var out safeBuffer
	conn.writer = bufio.NewWriter(&out)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := conn.PreviewOrderWhatIf(ctx, &IBKROrder{
		Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY",
	})
	if err == nil {
		t.Fatal("WhatIf before nextValidId unexpectedly succeeded")
	}
	if out.Len() != 0 {
		t.Fatalf("WhatIf before nextValidId wrote %d bytes", out.Len())
	}
}

func TestBrokerIDNamespaceRefusesRequestBeforeNextValidThenRetries(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	conn.serverVersion = maxClientVersion
	conn.signalHandshakeReady()
	var out safeBuffer
	conn.writer = bufio.NewWriter(&out)

	contract := Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	if _, err := conn.RequestContractDetails(contract); err == nil {
		t.Fatal("request before nextValidId unexpectedly succeeded")
	}
	if out.Len() != 0 {
		t.Fatalf("request before nextValidId wrote %d bytes", out.Len())
	}

	conn.observeNextValidOrderID(100)
	id, err := conn.RequestContractDetails(contract)
	if err != nil {
		t.Fatalf("request after nextValidId: %v", err)
	}
	if id != 100 || out.Len() == 0 {
		t.Fatalf("request after nextValidId id=%d bytes=%d, want id 100 and wire data", id, out.Len())
	}
}

func TestBrokerIDNamespaceKnownOrderCannotSendBeforeReconnectNextValid(t *testing.T) {
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU1234567"}
	conn := NewConnection(cfg)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.ordersMu.Lock()
	conn.openOrders[50] = &IBKROrder{OrderID: 50}
	conn.ordersMu.Unlock()
	conn.resetOrderIDReadiness()
	var out safeBuffer
	conn.writer = bufio.NewWriter(&out)

	err := conn.PlacePaperOrder(PaperOrderGate{
		Mode: "paper", Account: cfg.Account, Host: cfg.Host, Port: cfg.Port, ClientID: cfg.ClientID,
	}, &IBKROrder{
		OrderID: 50, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: cfg.Account,
	})
	if !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("known-order send before reconnect nextValidId err = %v, want namespace refusal", err)
	}
	if out.Len() != 0 {
		t.Fatalf("known-order send before reconnect nextValidId wrote %d bytes", out.Len())
	}
}

func TestExplicitReadRequestClaimsAndAdvancesSharedFrontier(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	var out safeBuffer
	conn.writer = bufio.NewWriter(&out)

	if err := conn.RequestPnL(20, "DU123", ""); err != nil {
		t.Fatalf("explicit high reqPnL ID: %v", err)
	}
	if got := conn.GetNextOrderID(); got != 21 {
		t.Fatalf("order ID after explicit request 20 = %d, want 21", got)
	}
	before := out.Len()
	if err := conn.RequestPnLSingle(20, "DU123", "", 1); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("reused explicit request ID err = %v, want namespace conflict", err)
	}
	if out.Len() != before {
		t.Fatalf("reused explicit request ID wrote %d new bytes", out.Len()-before)
	}
}

func TestBrokerIDNamespaceRefusesUnknownOldExplicitIDButPreservesReservation(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.observeNextValidOrderID(20)

	reserved := conn.GetNextOrderID()
	if reserved != 20 {
		t.Fatalf("reserved order id = %d, want 20", reserved)
	}
	if got := conn.GetNextRequestID(); got != 21 {
		t.Fatalf("intervening request id = %d, want 21", got)
	}
	if err := conn.claimOrderID(reserved, false); err != nil {
		t.Fatalf("claim exact reserved id after request: %v", err)
	}
	if err := conn.claimOrderID(21, false); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("claim completed request id err = %v, want namespace conflict", err)
	}
	if err := conn.claimOrderID(22, false); err != nil {
		t.Fatalf("claim brand-new frontier id: %v", err)
	}
}

func TestReservedOrderSurvivesInterveningAccountRequestAndSubmit(t *testing.T) {
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU1234567"}
	conn := NewConnection(cfg)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(50)
	var out safeBuffer
	conn.writer = bufio.NewWriter(&out)

	c := NewConnector(&ConnectorConfig{BaseConfig: cfg})
	c.conn = conn
	c.running = true
	c.ready = true

	c.brokerIDNamespaceMu.Lock()
	reserved, _, err := c.nextDisjointOrderIDLocked(conn)
	if err == nil {
		c.orderIDHighWater = max(c.orderIDHighWater, reserved)
	}
	c.brokerIDNamespaceMu.Unlock()
	if err != nil {
		t.Fatalf("reserve order ID: %v", err)
	}

	reqID, err := conn.nextRequestIDForForwarding()
	if err != nil {
		t.Fatalf("reserve account request ID: %v", err)
	}
	if err := conn.RequestAccountSummaryForAccount(reqID, "NetLiquidation", cfg.Account); err != nil {
		t.Fatalf("send intervening account request: %v", err)
	}

	order := &RawOrder{
		OrderID: reserved, Action: "BUY", TotalQty: 1, OrderType: "LMT",
		LmtPrice: 1, TIF: "DAY", Account: cfg.Account, OrderRef: "reserved-submit",
	}
	err = c.SubmitPaperOrder(PaperOrderGate{
		Mode: "paper", Account: cfg.Account, Host: cfg.Host, Port: cfg.Port, ClientID: cfg.ClientID,
	}, &Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"}, order)
	if err != nil {
		t.Fatalf("submit exact reserved order after account request: %v", err)
	}
	if order.OrderID != reserved {
		t.Fatalf("submitted order id = %d, want reserved %d", order.OrderID, reserved)
	}
}

func TestBrokerIDNamespaceFailsClosedAtMaxInt32(t *testing.T) {
	requestConn := NewConnection(nil)
	t.Cleanup(requestConn.rateLimiter.Stop)
	requestConn.observeNextValidOrderID(1)
	requestConn.reqIDMu.Lock()
	requestConn.reqIDSeq = maxProtoInt32
	requestConn.reqIDMu.Unlock()
	if got := requestConn.GetNextRequestID(); got != maxProtoInt32 {
		t.Fatalf("last request id = %d, want %d", got, maxProtoInt32)
	}
	if got := requestConn.GetNextRequestID(); got != 0 {
		t.Fatalf("request id after exhaustion = %d, want 0", got)
	}

	orderConn := NewConnection(nil)
	t.Cleanup(orderConn.rateLimiter.Stop)
	orderConn.observeNextValidOrderID(maxProtoInt32)
	if got := orderConn.GetNextOrderID(); got != maxProtoInt32 {
		t.Fatalf("last order id = %d, want %d", got, maxProtoInt32)
	}
	if got := orderConn.GetNextRequestID(); got != 0 {
		t.Fatalf("request id after order exhaustion = %d, want 0", got)
	}
}

func TestFailedRequestStillConsumesSharedBrokerID(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.observeNextValidOrderID(1)

	failedID := conn.GetNextRequestID()
	if failedID != 1 {
		t.Fatalf("failed request id = %d, want 1", failedID)
	}
	// No completion is needed: allocation itself consumes the ambiguous ID.
	if got := conn.GetNextOrderID(); got != 2 {
		t.Fatalf("order id after failed request = %d, want 2", got)
	}
}

func TestRejectedRequestReservationCannotBeReused(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.observeNextValidOrderID(1)
	id, err := conn.reserveRequestID(func(candidate int) bool { return candidate != 1 })
	if err != nil || id != 2 {
		t.Fatalf("guarded request reservation id=%d err=%v, want id 2", id, err)
	}
	if err := conn.claimRequestID(1); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("rejected request ID reuse err = %v, want namespace conflict", err)
	}
	conn.reqIDMu.Lock()
	_, leaked := conn.reservedRequestIDs[1]
	conn.reqIDMu.Unlock()
	if leaked {
		t.Fatal("rejected request ID retained reusable reservation provenance")
	}
}

func TestOrderReservationSurvivesValidationOnlyFailure(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.observeNextValidOrderID(30)
	reserved := conn.GetNextOrderID()

	order := &IBKROrder{OrderID: reserved}
	if _, err := preparePlaceOrder(order, conn, nil); err == nil {
		t.Fatal("invalid order unexpectedly passed local validation")
	}
	if got := conn.GetNextRequestID(); got != 31 {
		t.Fatalf("intervening request id = %d, want 31", got)
	}
	*order = IBKROrder{
		OrderID: reserved, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY",
	}
	if _, err := preparePlaceOrder(order, conn, nil); err != nil {
		t.Fatalf("corrected order could not consume exact reservation: %v", err)
	}
}

func TestOrderIDRemainsConsumedAfterSendAttemptFailure(t *testing.T) {
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU1234567"}
	conn := NewConnection(cfg)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(40)
	reserved := conn.GetNextOrderID()

	err := conn.PlacePaperOrder(PaperOrderGate{
		Mode: "paper", Account: cfg.Account, Host: cfg.Host, Port: cfg.Port, ClientID: cfg.ClientID,
	}, &IBKROrder{
		OrderID: reserved, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: cfg.Account,
	})
	if err == nil {
		t.Fatal("order send without writer unexpectedly succeeded")
	}
	if err := conn.claimOrderID(reserved, false); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("reclaim attempted-send order ID err = %v, want namespace conflict", err)
	}
	if got := conn.GetNextOrderID(); got != 41 {
		t.Fatalf("new order ID after attempted-send failure = %d, want 41", got)
	}
}
