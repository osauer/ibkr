package ibkr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRegisterUnregisterRace exercises the snapshotHandlers + UnregisterHandler
// path under -race. Before v0.16.0 snapshotHandlers released the RLock before
// iterating its captured slice; a concurrent UnregisterHandler shifted entries
// in place via append(entries[:i], entries[i+1:]...) on the same backing array.
// The fix lifts the iteration under the RLock so reader and writer are
// serialised through handlersMu.
//
// deferContractDetailsCleanup in connector.go is the canonical production
// caller — it runs UnregisterHandler from a goroutine while readMessages
// dispatches through snapshotHandlers; this test models that pattern with
// nothing of the IBKR wire layer attached.
func TestRegisterUnregisterRace(t *testing.T) {
	conn := &Connection{msgHandlers: map[int][]handlerEntry{}}

	const msgID = 42
	const handlers = 64
	ids := make([]uint64, handlers)
	for i := range handlers {
		ids[i] = conn.RegisterHandler(msgID, func([]string) {})
	}

	// Baseline dispatch outside the race window — gives us a known >0
	// call count and confirms the snapshot path is wired before the
	// concurrent goroutines start interleaving.
	var calls atomic.Int64
	for _, fn := range conn.snapshotHandlers(msgID) {
		fn(nil)
		calls.Add(1)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				fns := conn.snapshotHandlers(msgID)
				for _, fn := range fns {
					fn(nil)
					calls.Add(1)
				}
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer close(stop)
		for i := range handlers {
			conn.UnregisterHandler(msgID, ids[i])
			runtime.Gosched()
		}
	}()

	wg.Wait()
	if calls.Load() == 0 {
		t.Fatal("snapshotHandlers never dispatched; race goroutine arrangement is wrong")
	}
}

func TestConnectorPreRegistersOneOrderedStartAPILifecycleIngressBeforeReader(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(c.conn.rateLimiter.Stop)
	conn := c.conn
	conn.serverVersion = 203

	var (
		mu       sync.Mutex
		got      []string
		receipts []OrderLifecycleReceipt
	)
	c.RegisterOrderLifecycleReceiptHandler(func(receipt OrderLifecycleReceipt) {
		label := receipt.Event.Type
		if receipt.Event.Type == OrderLifecycleEventError {
			label = fmt.Sprintf("%s:%d", label, receipt.Event.ErrorCode)
		}
		mu.Lock()
		got = append(got, label)
		receipts = append(receipts, receipt)
		mu.Unlock()
	})

	// This is exactly the Connector.Start ordering: every base callback is
	// installed while Connection is still disconnected, before Connect can
	// start readMessages. A repeated hook install must not duplicate handlers.
	c.attachConnectionHooks(conn)
	c.attachConnectionHooks(conn)
	for _, msgID := range []int{msgOrderStatus, msgOpenOrder, msgExecDetails, msgErrMsg} {
		conn.handlersMu.RLock()
		count := len(conn.msgHandlers[msgID])
		conn.handlersMu.RUnlock()
		if count == 0 {
			t.Fatalf("message %d has no pre-registered handler", msgID)
		}
	}
	conn.systemNoticeMu.RLock()
	noticeReady := conn.systemNoticeHandler != nil
	conn.systemNoticeMu.RUnlock()
	if !noticeReady {
		t.Fatal("msg-204 handler was not installed before the reader")
	}

	// startAPI can process this burst while the exact socket is still in the
	// Connecting phase, before onConnect publishes outbound readiness.
	conn.setStatus(StatusConnecting)
	c.orderMu.Lock()
	c.brokerOrderIndex["91"] = "ordered-ingress"
	c.orderMu.Unlock()
	epoch := conn.BrokerSessionEpoch()

	// One reader processes this deliberately mixed sequence synchronously.
	// There is no per-msgID or system-notice startup buffer that can invert it.
	conn.processMessageAtEpoch(conn.encodeMsg(msgOrderStatus, "1", "91", "Submitted", "0", "1", "0", "7001", "0", "31", "0", "", "0"), epoch)
	conn.processMessageAtEpoch(encodeSystemNotificationForTest(91, 201, "order rejected", ""), epoch)
	conn.processMessageAtEpoch(conn.encodeMsg(msgOpenOrder, "protobuf", "orderId=92", "permId=7002", "clientId=31", "symbol=AAPL", "secType=STK", "action=BUY", "qty=1", "orderType=LMT", "lmtPrice=100", "tif=DAY", "status=Submitted"), epoch)
	conn.processMessageAtEpoch(conn.encodeMsg(msgErrMsg, "2", "91", "202", "order cancelled"), epoch)
	conn.processMessageAtEpoch(conn.encodeMsg(msgExecDetails, "protobuf", "reqId=1", "orderId=93", "permId=7003", "clientId=31", "execId=exec-1", "symbol=AAPL", "secType=STK", "shares=1", "price=100"), epoch)

	mu.Lock()
	defer mu.Unlock()
	want := []string{OrderLifecycleEventStatus, OrderLifecycleEventError + ":201", OrderLifecycleEventOpenOrder, OrderLifecycleEventError + ":202", OrderLifecycleEventExecDetails}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle ingress order = %v, want exact-once %v", got, want)
	}
	for i, receipt := range receipts {
		if receipt.Session.connector != c || receipt.Session.connection != conn || receipt.Session.epoch != epoch || !c.SessionReceiptCurrent(receipt.Session) {
			t.Fatalf("receipt %d session = %+v, want exact current startAPI Connector session epoch %d", i, receipt.Session, epoch)
		}
	}
}

func TestMsgErrRechecksExactEpochBeforeCurrentSessionSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []any
		seed   func(*testing.T, *Connection)
		check  func(*testing.T, *Connection, uint64)
	}{
		{
			name:   "market_data_slot",
			fields: []any{msgErrMsg, "2", "9", "200", "no security definition"},
			seed: func(t *testing.T, conn *Connection) {
				if err := conn.acquireMarketDataSlot(context.Background(), 9); err != nil {
					t.Fatalf("acquire current-session market-data slot: %v", err)
				}
			},
			check: func(t *testing.T, conn *Connection, _ uint64) {
				if got := conn.rateLimiter.marketDataSubs.Count(); got != 0 {
					t.Fatalf("retired-session market-data slots=%d, want reset to 0", got)
				}
			},
		},
		{
			name:   "client_id_error",
			fields: []any{msgErrMsg, "2", "-1", "326", "client id already in use"},
			seed: func(_ *testing.T, conn *Connection) {
				conn.statusMu.Lock()
				conn.lastError = errors.New("current session sentinel")
				conn.statusMu.Unlock()
			},
			check: func(t *testing.T, conn *Connection, _ uint64) {
				conn.statusMu.RLock()
				defer conn.statusMu.RUnlock()
				if conn.lastError == nil || conn.lastError.Error() != "current session sentinel" {
					t.Fatalf("current-session lastError=%v, want sentinel unchanged", conn.lastError)
				}
			},
		},
		{
			name:   "competing_session",
			fields: []any{msgErrMsg, "2", "9", "10197", "competing live session"},
			check: func(t *testing.T, conn *Connection, _ uint64) {
				if conn.HasCompetingLiveSession() {
					t.Fatal("stale 10197 mutated current competing-session state")
				}
			},
		},
		{
			name:   "disconnect",
			fields: []any{msgErrMsg, "2", "-1", "502", "connection failed"},
			check: func(t *testing.T, conn *Connection, outbound uint64) {
				if conn.Status() != StatusConnected {
					t.Fatalf("stale 502 changed status to %v", conn.Status())
				}
				if got := conn.outboundSessionState.Load(); got != outbound {
					t.Fatalf("stale 502 changed outbound state from %d to %d", outbound, got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := NewConnection(&ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 31, Account: "DU7654321"})
			t.Cleanup(conn.rateLimiter.Stop)
			conn.status = StatusConnected
			setServerVersionReady(conn, maxClientVersion)
			conn.observeNextValidOrderID(100)
			conn.writer = bufio.NewWriter(&safeBuffer{})
			if tc.seed != nil {
				tc.seed(t, conn)
			}
			oldEpoch := conn.BrokerSessionEpoch()
			outbound := conn.outboundSessionState.Load()
			var legacyCalls atomic.Int32
			var epochCalls atomic.Int32
			var receiptEpoch atomic.Uint64
			conn.RegisterHandler(msgErrMsg, func([]string) { legacyCalls.Add(1) })
			conn.RegisterHandlerAtEpoch(msgErrMsg, func(_ []string, epoch uint64) {
				epochCalls.Add(1)
				receiptEpoch.Store(epoch)
			})
			conn.errorMessageAfterInitialEpochCheck = func() {
				conn.errorMessageAfterInitialEpochCheck = nil
				conn.resetOrderIDReadiness()
				conn.observeNextValidOrderIDAtEpoch(500, conn.BrokerSessionEpoch())
			}
			conn.processMessageAtEpoch(conn.encodeMsg(tc.fields...), oldEpoch)
			if conn.BrokerSessionEpoch() == oldEpoch {
				t.Fatal("test seam did not advance socket epoch")
			}
			if legacyCalls.Load() != 0 || epochCalls.Load() != 1 || receiptEpoch.Load() != oldEpoch {
				t.Fatalf("legacy=%d epoch=%d receipt_epoch=%d, want 0/1/%d", legacyCalls.Load(), epochCalls.Load(), receiptEpoch.Load(), oldEpoch)
			}
			if tc.check != nil {
				tc.check(t, conn, outbound)
			}
		})
	}
}

func TestHeartbeatAndMarketDataTypeAreSocketEpochBound(t *testing.T) {
	conn := NewConnection(&ConnectionConfig{ClientID: 31})
	t.Cleanup(conn.rateLimiter.Stop)
	oldEpoch := conn.BrokerSessionEpoch()
	if !conn.recordHeartbeatAtEpoch(oldEpoch, 100) {
		t.Fatal("current heartbeat was rejected")
	}
	if !conn.processMarketDataTypeAtEpoch([]string{"58", "1", "7", "1"}, oldEpoch) {
		t.Fatal("current market-data type was rejected")
	}
	conn.resetOrderIDReadiness()
	newEpoch := conn.BrokerSessionEpoch()
	if newEpoch == oldEpoch {
		t.Fatal("socket epoch did not advance")
	}
	if got := conn.MarketDataType(7); got != 0 {
		t.Fatalf("market-data type survived epoch rollover: %d", got)
	}
	if got := conn.lastHeartbeatNano.Load(); got != 0 {
		t.Fatalf("heartbeat survived epoch rollover: %d", got)
	}
	if !conn.recordHeartbeatAtEpoch(newEpoch, 200) {
		t.Fatal("new current heartbeat was rejected")
	}
	if conn.recordHeartbeatAtEpoch(oldEpoch, 999) {
		t.Fatal("stale heartbeat was accepted")
	}
	if got := conn.lastHeartbeatNano.Load(); got != 200 {
		t.Fatalf("stale heartbeat changed current freshness to %d", got)
	}
	if conn.processMarketDataTypeAtEpoch([]string{"58", "1", "7", "3"}, oldEpoch) {
		t.Fatal("stale market-data type was accepted")
	}
	if got := conn.MarketDataType(7); got != 0 {
		t.Fatalf("stale market-data type changed current map: %d", got)
	}
	if !conn.processMarketDataTypeAtEpoch([]string{"58", "1", "7", "4"}, newEpoch) || conn.MarketDataType(7) != 4 {
		t.Fatal("current market-data type was not recorded")
	}
}

func TestPanickingErrorHandlerReleasesInboundEpochLease(t *testing.T) {
	conn := NewConnection(&ConnectionConfig{ClientID: 31})
	t.Cleanup(conn.rateLimiter.Stop)
	epoch := conn.BrokerSessionEpoch()
	conn.RegisterHandler(msgErrMsg, func([]string) { panic("test handler panic") })
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected handler panic")
			}
		}()
		conn.processErrorMessageAtEpoch([]string{"4", "2", "9", "200", "no definition"}, epoch)
	}()
	done := make(chan struct{})
	go func() {
		conn.resetOrderIDReadiness()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panicking error handler leaked inbound epoch read lease")
	}
}
