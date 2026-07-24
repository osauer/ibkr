package ibkr

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func exactQuoteTestContract(conID int) Contract {
	return Contract{
		ConID: conID, Symbol: "SPY", SecType: "OPT", Expiry: "20261218", Strike: 500,
		Right: "C", Multiplier: 100, Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD",
		LocalSymbol: "SPY   261218C00500000", TradingClass: "SPY",
	}
}

func exactQuoteSubscriptionReqID(t *testing.T, connector *Connector, key string) int {
	t.Helper()
	connector.subMu.RLock()
	defer connector.subMu.RUnlock()
	sub := connector.subscriptions[key]
	if sub == nil || sub.ReqID <= 0 {
		t.Fatalf("exact subscription %q missing request ID: %+v", key, sub)
	}
	return sub.ReqID
}

func marketDataSlotCount(conn *Connection) int {
	conn.marketDataSlotsMu.Lock()
	defer conn.marketDataSlotsMu.Unlock()
	return len(conn.marketDataSlots)
}

func TestMarketDataKeyForContractSeparatesUnroutedPositiveConIDs(t *testing.T) {
	keyA := MarketDataKeyForContract(Contract{ConID: 700001, Symbol: "SAME", SecType: "STK"})
	keyB := MarketDataKeyForContract(Contract{ConID: 700002, Symbol: "SAME", SecType: "STK"})
	if keyA == keyB || !containsConID(keyA, 700001) || !containsConID(keyB, 700002) {
		t.Fatalf("positive-ConID keys collided: A=%q B=%q", keyA, keyB)
	}
	if got := MarketDataKeyForContract(Contract{Symbol: "SAME", SecType: "STK"}); got != "SAME" {
		t.Fatalf("legacy unrouted stock key=%q, want SAME", got)
	}
}

func TestExactSessionOptionQuoteCarriesCanonicalIdentityAndClearsUnderlyingPrimary(t *testing.T) {
	conn, connector, oldSocket, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exact quote session")
	}
	contract := exactQuoteTestContract(900001)
	key, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding, contract, []string{"BID", "ASK"})
	if err != nil {
		t.Fatalf("subscribe exact option: %v", err)
	}
	if key == "" {
		t.Fatal("exact option subscription returned empty key")
	}
	frames := decodeOutboundFrames(t, conn, oldSocket.Bytes())
	marketData := findOutboundFrame(t, frames, reqMktData)
	assertField(t, marketData, 3, "900001", "marketData conID")
	assertField(t, marketData, 4, "SPY", "marketData symbol")
	assertField(t, marketData, 5, "OPT", "marketData secType")
	assertField(t, marketData, 9, "100", "marketData multiplier")
	assertField(t, marketData, 10, "SMART", "marketData exchange")
	assertField(t, marketData, 11, "", "marketData primaryExchange")
	assertField(t, marketData, 13, contract.LocalSymbol, "marketData localSymbol")
	assertField(t, marketData, 14, contract.TradingClass, "marketData tradingClass")
}

func TestExactSessionFXQuoteUsesExplicitPairWithoutPositiveConID(t *testing.T) {
	conn, connector, oldSocket, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exact FX quote session")
	}
	key, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding, Contract{
		Symbol: "USD", SecType: "CASH", Exchange: "IDEALPRO", PrimaryExch: "IDEALPRO", Currency: "EUR",
	}, []string{"BID", "ASK"})
	if err != nil {
		t.Fatalf("subscribe exact FX pair: %v", err)
	}
	if key == "" {
		t.Fatal("exact FX subscription returned empty key")
	}
	frames := decodeOutboundFrames(t, conn, oldSocket.Bytes())
	marketData := findOutboundFrame(t, frames, reqMktData)
	assertField(t, marketData, 3, "0", "marketData conID")
	assertField(t, marketData, 4, "USD", "marketData symbol")
	assertField(t, marketData, 5, "CASH", "marketData secType")
	assertField(t, marketData, 10, "IDEALPRO", "marketData exchange")
	assertField(t, marketData, 11, "IDEALPRO", "marketData primaryExchange")
	assertField(t, marketData, 12, "EUR", "marketData currency")
}

func TestExactSessionQuoteRejectsOtherZeroConIDContracts(t *testing.T) {
	_, connector, _, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exact quote session")
	}
	for name, contract := range map[string]Contract{
		"stock":            {Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		"cash wrong venue": {Symbol: "USD", SecType: "CASH", Exchange: "SMART", Currency: "EUR"},
		"cash same legs":   {Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "EUR"},
		"cash missing leg": {Symbol: "USD", SecType: "CASH", Exchange: "IDEALPRO"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding, contract, nil); err == nil {
				t.Fatalf("zero-ConID contract unexpectedly accepted: %+v", contract)
			}
		})
	}
}

func TestExactSessionQuoteCleanupReleasesRetiredSlotWithoutSuccessorCancel(t *testing.T) {
	conn, connector, oldSocket, newSocket, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exact quote session")
	}
	key, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding,
		Contract{ConID: 700001, Symbol: "SAME", SecType: "STK", Exchange: "SMART", Currency: "USD"}, nil)
	if err != nil {
		t.Fatalf("subscribe exact stock: %v", err)
	}
	if got := marketDataSlotCount(conn); got != 1 {
		t.Fatalf("market-data slots=%d, want 1", got)
	}
	oldBytes := oldSocket.Len()
	conn.resetOrderIDReadiness()
	conn.writer = newBufferedSafeWriter(newSocket)
	conn.observeNextValidOrderIDAtEpoch(500, conn.BrokerSessionEpoch())
	if err := connector.UnsubscribeMarketDataForSession(context.Background(), binding, key); err != nil {
		t.Fatalf("retired exact cleanup: %v", err)
	}
	if got := marketDataSlotCount(conn); got != 0 {
		t.Fatalf("retired cleanup leaked market-data slot: %d", got)
	}
	if oldSocket.Len() != oldBytes || newSocket.Len() != 0 {
		t.Fatalf("retired cleanup wrote cancel bytes oldBefore=%d oldAfter=%d new=%d", oldBytes, oldSocket.Len(), newSocket.Len())
	}
}

func TestExactSessionQuoteDoesNotReuseSameRouteAlternateConIDCache(t *testing.T) {
	_, connector, _, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exact quote session")
	}
	wrong := Contract{ConID: 700002, Symbol: "SAME", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	wrongKey := MarketDataKeyForContract(wrong)
	connector.subMu.Lock()
	connector.subscriptions[wrongKey] = &Subscription{Symbol: wrongKey, ReqID: 77, Bid: 9998, Ask: 10000, Observed: true}
	connector.reqIDMap[77] = wrongKey
	connector.subMu.Unlock()

	right := wrong
	right.ConID = 700001
	rightKey, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding, right, nil)
	if err != nil {
		t.Fatalf("subscribe exact right contract: %v", err)
	}
	if rightKey == wrongKey {
		t.Fatalf("alternate ConIDs shared exact quote key %q", rightKey)
	}
	snapshot := connector.MarketDataSnapshot()
	if got := snapshot[rightKey]; got == nil || got.Bid != 0 || got.Ask != 0 || got.Last != 0 {
		t.Fatalf("wrong-ConID cache satisfied exact quote: %+v", got)
	}
	if got := snapshot[wrongKey]; got == nil || got.Bid != 9998 || got.Ask != 10000 {
		t.Fatalf("wrong-ConID fixture unexpectedly changed: %+v", got)
	}
}

func TestConcurrentExactSessionQuotesDoNotShareOrCrossCancel(t *testing.T) {
	conn, connector, oldSocket, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exact quote session")
	}
	contract := Contract{ConID: 700001, Symbol: "SAME", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	keyA, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding, contract, nil)
	if err != nil {
		t.Fatalf("subscribe exact A: %v", err)
	}
	keyB, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding, contract, nil)
	if err != nil {
		t.Fatalf("subscribe exact B: %v", err)
	}
	if keyA == keyB {
		t.Fatalf("concurrent exact subscriptions shared key %q", keyA)
	}
	reqB := exactQuoteSubscriptionReqID(t, connector, keyB)
	if err := connector.UnsubscribeMarketDataForSession(context.Background(), binding, keyA); err != nil {
		t.Fatalf("unsubscribe exact A: %v", err)
	}
	connector.subMu.RLock()
	remaining := connector.subscriptions[keyB]
	mapped := connector.reqIDMap[reqB]
	connector.subMu.RUnlock()
	if remaining == nil || mapped != keyB {
		t.Fatalf("A cleanup crossed into B: remaining=%+v mapped=%q", remaining, mapped)
	}
	frames := decodeOutboundFrames(t, conn, oldSocket.Bytes())
	marketRequests := 0
	cancels := 0
	for _, frame := range frames {
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case "1":
			marketRequests++
		case "2":
			cancels++
		}
	}
	if marketRequests != 2 || cancels != 1 {
		t.Fatalf("exact frames requests=%d cancels=%d, want 2/1: %#v", marketRequests, cancels, frames)
	}
}

func TestStaleExactQuoteTickCannotPopulateSuccessorSubscription(t *testing.T) {
	conn, connector, _, newSocket, _ := newQueuedInstructionReconnectFixture(t)
	bindingA, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session A")
	}
	contractA := Contract{ConID: 700001, Symbol: "SAME", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	keyA, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), bindingA, contractA, nil)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	reqA := exactQuoteSubscriptionReqID(t, connector, keyA)

	conn.resetOrderIDReadiness()
	conn.writer = newBufferedSafeWriter(newSocket)
	conn.observeNextValidOrderIDAtEpoch(500, conn.BrokerSessionEpoch())
	bindingB, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session B")
	}
	contractB := Contract{ConID: 700002, Symbol: "SAME", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	keyB, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), bindingB, contractB, nil)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	conn.processMessageAtEpoch(conn.encodeMsg(msgTickPrice, "3", reqA, 1, "9999", "0"), bindingA.epoch)
	time.Sleep(time.Millisecond)
	if got := connector.MarketDataSnapshot()[keyB]; got != nil && (got.Bid != 0 || got.Ask != 0 || got.Last != 0 || got.MarkPrice != 0) {
		t.Fatalf("stale A tick populated B exact quote: %+v", got)
	}
	if keyA == keyB || !containsConID(keyA, contractA.ConID) || !containsConID(keyB, contractB.ConID) {
		t.Fatalf("exact keys do not preserve ConID identity: A=%q B=%q", keyA, keyB)
	}
}

func containsConID(key string, conID int) bool {
	return strings.Contains(key, "CONID:"+strconv.Itoa(conID))
}
