package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// safeBuffer wraps bytes.Buffer with a mutex to avoid data races in -race tests.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *safeBuffer) Len() int { s.mu.Lock(); defer s.mu.Unlock(); return s.buf.Len() }
func (s *safeBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

// Test EnsureMarketDataSubscription behavior when no IBKR connection is present
func TestEnsureMarketDataSubscription_NoConnection(t *testing.T) {
	c := NewConnector(nil)
	// Do not call Start; leave c.conn nil
	made, err := c.EnsureMarketDataSubscription(context.Background(), "SPY", []string{"LAST"}, 0)
	if err == nil {
		t.Fatalf("expected error when no connection, got nil")
	}
	if made {
		t.Fatalf("expected made=false when no connection, got true")
	}
}

func TestEnsureMarketDataSubscription_ReturnsInactiveError(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.markSymbolInactive("HGENQ", "No security definition has been found for the request")

	made, err := c.EnsureMarketDataSubscription(context.Background(), "HGENQ", []string{"LAST"}, time.Minute)
	if !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("expected ErrSymbolInactive, got %v", err)
	}
	if made {
		t.Fatalf("expected made=false for inactive symbol, got true")
	}
}

func TestSubscribeMarketDataUsesContractCache(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	var out safeBuffer
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(&out)
	c.conn = conn
	c.running = true
	c.ready = true

	c.contractMu.Lock()
	c.contractCache["SPY"] = ContractDetailsLite{
		ConID:        12345,
		Symbol:       "SPY",
		Exchange:     "SMART",
		PrimaryExch:  "ARCA",
		LocalSymbol:  "SPY",
		TradingClass: "SPY",
	}
	c.contractMu.Unlock()

	if err := c.SubscribeMarketData(context.Background(), "SPY", []string{"LAST"}); err != nil {
		t.Fatalf("SubscribeMarketData: %v", err)
	}

	payload := out.Bytes()
	if len(payload) == 0 {
		t.Fatalf("expected outbound payload")
	}

	last := func(buf []byte) []byte {
		offset := 0
		var msg []byte
		for offset+4 <= len(buf) {
			length := int(binary.BigEndian.Uint32(buf[offset : offset+4]))
			start := offset + 4
			end := start + length
			if end > len(buf) {
				break
			}
			msg = append([]byte(nil), buf[start:end]...)
			offset = end
		}
		return msg
	}(payload)

	if len(last) == 0 {
		t.Fatalf("failed to decode last message from payload")
	}

	fields := conn.decodeMessage(last)
	if len(fields) < 5 {
		t.Fatalf("unexpected message fields: %#v", fields)
	}
	if fields[0] != "1" || fields[1] != "11" {
		t.Fatalf("expected reqMktData header, got %#v", fields[:2])
	}
	if fields[3] != "12345" {
		t.Fatalf("expected conID 12345 in payload, got %q (fields=%#v)", fields[3], fields)
	}
}

// SubscribeMarketData must be idempotent: a duplicate subscribe to the
// same symbol returns nil and reuses the existing reqID, with no second
// outbound reqMktData on the wire. Pre-fix, the second call returned an
// "already subscribed" error which forced daemon callers to either
// swallow it (snapshot) or propagate to the user (streaming).
func TestSubscribeMarketData_Idempotent(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	var out safeBuffer
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(&out)
	c.conn = conn
	c.running = true
	c.ready = true

	c.contractMu.Lock()
	c.contractCache["SPY"] = ContractDetailsLite{
		ConID:        12345,
		Symbol:       "SPY",
		Exchange:     "SMART",
		PrimaryExch:  "ARCA",
		LocalSymbol:  "SPY",
		TradingClass: "SPY",
	}
	c.contractMu.Unlock()

	if err := c.SubscribeMarketData(context.Background(), "SPY", []string{"LAST"}); err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	c.subMu.RLock()
	first, ok := c.subscriptions["SPY"]
	c.subMu.RUnlock()
	if !ok {
		t.Fatalf("expected subscription after first subscribe")
	}
	firstReqID := first.ReqID
	bytesAfterFirst := out.Len()

	if err := c.SubscribeMarketData(context.Background(), "SPY", []string{"LAST"}); err != nil {
		t.Fatalf("second subscribe must be idempotent (returned nil), got %v", err)
	}
	c.subMu.RLock()
	second, ok := c.subscriptions["SPY"]
	c.subMu.RUnlock()
	if !ok {
		t.Fatalf("expected subscription after second subscribe")
	}
	if second.ReqID != firstReqID {
		t.Fatalf("expected idempotent reqID %d, got %d", firstReqID, second.ReqID)
	}
	if out.Len() != bytesAfterFirst {
		t.Fatalf("second subscribe wrote %d more bytes; expected zero outbound traffic on no-op", out.Len()-bytesAfterFirst)
	}
}

// Test UnsubscribeMarketData is idempotent (does not error if symbol absent)
func TestUnsubscribeMarketData_Idempotent(t *testing.T) {
	c := NewConnector(nil)
	// No subscription added; unsubscribe should be a no-op
	if err := c.UnsubscribeMarketData("SPY"); err != nil {
		t.Fatalf("expected no error on idempotent unsubscribe, got: %v", err)
	}
}

// SubscribeMarketData stores the symbol under strings.ToUpper(symbol);
// UnsubscribeMarketData must apply the same normalisation before lookup,
// otherwise a caller passing user-typed lowercase silently fails to
// release the IBKR market-data line (the entry stays in c.subscriptions,
// the IBKR-side reqMktData stays open, the 100-line subscription budget
// erodes).
func TestUnsubscribeMarketData_CaseInsensitive(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	// Mirror what SubscribeMarketData would have inserted: upper-case key,
	// not yet observed (so Unsubscribe takes the no-cancel path and does
	// not require a live writer).
	c.subscriptions["SPY"] = &Subscription{Symbol: "SPY", ReqID: 123, Observed: false}

	if err := c.UnsubscribeMarketData("spy"); err != nil {
		t.Fatalf("unexpected error from case-mismatched Unsubscribe: %v", err)
	}
	if _, still := c.subscriptions["SPY"]; still {
		t.Fatalf("subscription still present after lowercase Unsubscribe — case-normalisation regressed")
	}
}

// When a subscription has not yet been observed, Unsubscribe should not attempt
// to call CancelMarketData (which would panic in this test because the underlying
// connection has no writer). This guards against 300 spam during shutdown.
func TestUnsubscribeMarketData_NoCancelWhenNotObserved(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	// Create a fake subscription with a non-zero reqID but not observed
	c.subscriptions["SPY"] = &Subscription{Symbol: "SPY", ReqID: 123, Observed: false}
	// Attach a connection that appears connected but lacks a writer; if
	// CancelMarketData were called, it would panic due to nil writer.
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	c.conn = conn

	// This should not panic
	if err := c.UnsubscribeMarketData("SPY"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Simulate an IBKR error 200 on a subscription and ensure the connector attempts a refresh
func TestHandleIBKRError_RefreshOn200(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	// Prepare a connected fake connection with writer buffer
	var out safeBuffer
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(&out)
	c.conn = conn
	c.running = true
	c.ready = true

	c.contractMu.Lock()
	c.contractCache["SPY"] = ContractDetailsLite{
		Symbol:       "SPY",
		Exchange:     "SMART",
		PrimaryExch:  "ARCA",
		ConID:        12345,
		LocalSymbol:  "SPY",
		TradingClass: "SPY",
	}
	c.contractMu.Unlock()

	// Seed a subscription mapping
	c.subMu.Lock()
	c.subscriptions["SPY"] = &Subscription{Symbol: "SPY", ReqID: 101}
	c.reqIDMap[101] = "SPY"
	c.subMu.Unlock()

	// Fields: [msgID=4, version=2, reqID=101, code=200, msg=...]
	c.handleIBKRError([]string{"4", "2", "101", "200", "The destination or exchange selected is Invalid."})

	// Give time for async refresh
	time.Sleep(100 * time.Millisecond)

	if out.Len() == 0 {
		t.Fatalf("expected a re-subscribe request to be written")
	}
}

// Refreshing a stale subscription should cancel the previous reqID so the
// market-data semaphore does not steadily leak slots.
func TestEnsureMarketDataSubscription_RefreshReleasesSlot(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})

	// Prepare a live-looking connection with a real rate limiter and writer
	var out safeBuffer
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(&out)
	c.conn = conn
	c.running = true
	c.ready = true

	c.contractMu.Lock()
	c.contractCache["SPY"] = ContractDetailsLite{
		Symbol:       "SPY",
		Exchange:     "SMART",
		PrimaryExch:  "ARCA",
		ConID:        424242,
		LocalSymbol:  "SPY",
		TradingClass: "SPY",
	}
	c.contractMu.Unlock()

	// Simulate an existing subscription that consumed a slot. Goes through
	// the tracking helper so the reqID is registered and a subsequent
	// release-on-cancel can actually free the slot.
	if err := conn.acquireMarketDataSlot(conn.ctx, 42); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	c.subMu.Lock()
	c.subscriptions["SPY"] = &Subscription{
		Symbol:   "SPY",
		ReqID:    42,
		Observed: true,
		LastTime: time.Now().Add(-time.Hour),
	}
	c.subMu.Unlock()

	// Force refresh by passing a tiny stale window
	if !c.IsReady() {
		t.Fatalf("connector not ready for test setup (ready=%v connNil=%v connStatus=%v)", c.ready, c.conn == nil, conn.Status())
	}

	refreshed, err := c.EnsureMarketDataSubscription(context.Background(), "SPY", []string{"LAST"}, time.Millisecond)
	if err != nil {
		t.Fatalf("EnsureMarketDataSubscription: %v", err)
	}
	if !refreshed {
		t.Fatalf("expected refresh to occur")
	}

	// We should still only have a single slot checked out after refresh
	if count := conn.rateLimiter.marketDataSubs.Count(); count != 1 {
		t.Fatalf("expected 1 active slot after refresh, got %d", count)
	}

	if out.Len() == 0 {
		t.Fatalf("expected outbound traffic for cancel + resubscribe")
	}
}

func TestWaitForContractHydrationAdoptsCachedDetail(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	base := Contract{Symbol: "SPY", SecType: "STK", Exchange: "SMART"}

	go func() {
		time.Sleep(60 * time.Millisecond)
		c.contractMu.Lock()
		c.contractCache["SPY"] = ContractDetailsLite{
			Symbol:       "SPY",
			ConID:        12345,
			PrimaryExch:  "ARCA",
			Exchange:     "SMART",
			LocalSymbol:  "SPY",
			TradingClass: "SPY",
		}
		c.contractMu.Unlock()
	}()

	start := time.Now()
	contract, hydrated := c.waitForContractDetails("SPY", base, false)
	if !hydrated {
		t.Fatalf("expected contract details to populate")
	}
	if contract.ConID != 12345 {
		t.Fatalf("expected conID 12345, got %d", contract.ConID)
	}
	if elapsed := time.Since(start); elapsed < 55*time.Millisecond {
		t.Fatalf("helper returned too early, elapsed=%s", elapsed)
	}
}

func TestWaitForContractHydrationFastPath(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	base := Contract{Symbol: "QQQ", SecType: "STK", ConID: 222}
	contract, hydrated := c.waitForContractDetails("QQQ", base, true)
	if !hydrated {
		t.Fatalf("expected details fast path")
	}
	if contract.ConID != 222 {
		t.Fatalf("expected conID to remain 222, got %d", contract.ConID)
	}
}

func TestPrepareContractRecordsTiming(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.fetchContractDetails = func(symbol string, timeout time.Duration) ([]ContractDetailsLite, error) {
		time.Sleep(20 * time.Millisecond)
		return nil, fmt.Errorf("timeout")
	}

	var captured []time.Duration
	c.contractTimingHook = func(symbol string, elapsed time.Duration, resolved bool) {
		if symbol == "IWM" {
			captured = append(captured, elapsed)
			if resolved {
				t.Fatalf("expected unresolved timing record")
			}
		}
	}

	conn := NewConnection(nil)
	conn.setStatus(StatusConnected)
	c.conn = conn

	_, ready := c.prepareContract("IWM", 10*time.Millisecond, false)
	if ready {
		t.Fatalf("expected contract details to remain unresolved")
	}
	if len(captured) == 0 {
		t.Fatalf("expected timing hook to be invoked")
	}
	if captured[0] < 10*time.Millisecond {
		t.Fatalf("expected timing >= fetch timeout, got %s", captured[0])
	}
}
