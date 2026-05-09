package ibkr

import (
	"context"
	"strconv"
	"testing"
)

// fieldsTickPrice builds a tick-price message field slice as the wire decoder
// would deliver it: [msgID, version, reqID, tickType, price, attrMask].
func fieldsTickPrice(reqID, tickType int, price string) []string {
	return []string{
		strconv.Itoa(msgTickPrice),
		"6", // version
		strconv.Itoa(reqID),
		strconv.Itoa(tickType),
		price,
		"0", // attribute mask
	}
}

// fieldsTickGeneric builds a generic-tick message: [msgID, version, reqID, tickType, value].
func fieldsTickGeneric(reqID, tickType int, value string) []string {
	return []string{
		strconv.Itoa(msgTickGeneric),
		"1",
		strconv.Itoa(reqID),
		strconv.Itoa(tickType),
		value,
	}
}

// fieldsTickSnapshotEnd builds a snapshot-end message: [msgID, version, reqID].
func fieldsTickSnapshotEnd(reqID int) []string {
	return []string{
		strconv.Itoa(msgTickSnapshotEnd),
		"1",
		strconv.Itoa(reqID),
	}
}

// fieldsMarketDataType: [msgID, version, reqID, type].
func fieldsMarketDataType(reqID, dataType int) []string {
	return []string{
		strconv.Itoa(msgMarketDataType),
		"1",
		strconv.Itoa(reqID),
		strconv.Itoa(dataType),
	}
}

func TestSnapshotSession_BidAskLastPopulated(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("SPY")
	s.targetReqID.Store(42)

	c.handleSnapshotTickPrice(s, fieldsTickPrice(42, 1, "412.48"))
	c.handleSnapshotTickPrice(s, fieldsTickPrice(42, 2, "412.50"))
	c.handleSnapshotTickPrice(s, fieldsTickPrice(42, 4, "412.49"))

	if s.snap.Bid == nil || *s.snap.Bid != 412.48 {
		t.Fatalf("Bid = %v, want 412.48", s.snap.Bid)
	}
	if s.snap.Ask == nil || *s.snap.Ask != 412.50 {
		t.Fatalf("Ask = %v, want 412.50", s.snap.Ask)
	}
	if s.snap.Last == nil || *s.snap.Last != 412.49 {
		t.Fatalf("Last = %v, want 412.49", s.snap.Last)
	}
}

func TestSnapshotSession_NonPositivePriceDropped(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("SPY")
	s.targetReqID.Store(42)

	c.handleSnapshotTickPrice(s, fieldsTickPrice(42, 1, "0"))
	c.handleSnapshotTickPrice(s, fieldsTickPrice(42, 2, "-1"))
	c.handleSnapshotTickPrice(s, fieldsTickPrice(42, 4, ""))

	if s.snap.Bid != nil || s.snap.Ask != nil || s.snap.Last != nil {
		t.Fatalf("non-positive prices must not populate fields; got bid=%v ask=%v last=%v", s.snap.Bid, s.snap.Ask, s.snap.Last)
	}
}

func TestSnapshotSession_WrongReqIDIgnored(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("SPY")
	s.targetReqID.Store(42)

	c.handleSnapshotTickPrice(s, fieldsTickPrice(99, 1, "412.48")) // wrong reqID
	if s.snap.Bid != nil {
		t.Fatalf("tick with wrong reqID must be ignored; bid=%v", s.snap.Bid)
	}
}

func TestSnapshotSession_PreSendTicksDropped(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("SPY")
	// Do NOT call Store — targetReqID remains -1, simulating ticks arriving
	// before RequestMarketDataWithContract returns.

	c.handleSnapshotTickPrice(s, fieldsTickPrice(42, 1, "412.48"))
	if s.snap.Bid != nil {
		t.Fatalf("tick before request-send must be dropped; bid=%v", s.snap.Bid)
	}
}

func TestSnapshotSession_IVFromTick106(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("AAPL250620C200")
	s.targetReqID.Store(7)

	c.handleSnapshotTickGeneric(s, fieldsTickGeneric(7, 106, "0.2845"))

	if s.snap.IV == nil || *s.snap.IV != 0.2845 {
		t.Fatalf("IV = %v, want 0.2845", s.snap.IV)
	}
	if s.snap.IVStatus != "real" {
		t.Fatalf("IVStatus = %q, want real", s.snap.IVStatus)
	}
}

func TestSnapshotSession_IVPercentNormalized(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("AAPL250620C200")
	s.targetReqID.Store(7)

	c.handleSnapshotTickGeneric(s, fieldsTickGeneric(7, 106, "28.45"))

	if s.snap.IV == nil {
		t.Fatalf("IV should populate from percent-form 28.45")
	}
	if got := *s.snap.IV; got < 0.2844 || got > 0.2846 {
		t.Fatalf("IV = %v, want ~0.2845 (percent normalized)", got)
	}
}

func TestSnapshotSession_NonIVGenericTicksIgnored(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("SPY")
	s.targetReqID.Store(7)

	// Tick 24 = option historical vol — must NOT populate IV per AGENTS.md.
	c.handleSnapshotTickGeneric(s, fieldsTickGeneric(7, 24, "0.2500"))

	if s.snap.IV != nil {
		t.Fatalf("tick 24 (historical vol) must not populate IV; got %v", s.snap.IV)
	}
	if s.snap.IVStatus != "unavailable" {
		t.Fatalf("IVStatus must remain 'unavailable'; got %q", s.snap.IVStatus)
	}
}

func TestSnapshotSession_NoIVMessage_LeavesUnavailable(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("SPY")
	s.targetReqID.Store(7)

	c.handleSnapshotTickPrice(s, fieldsTickPrice(7, 1, "412.48"))
	c.handleSnapshotTickPrice(s, fieldsTickPrice(7, 2, "412.50"))
	// no tick 106

	if s.snap.IV != nil {
		t.Fatalf("IV should remain nil when tick 106 not received")
	}
	if s.snap.IVStatus != "unavailable" {
		t.Fatalf("IVStatus must be 'unavailable'; got %q", s.snap.IVStatus)
	}
}

func TestSnapshotSession_MarketDataTypeRecorded(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("SPY")
	s.targetReqID.Store(11)

	c.handleSnapshotMarketDataType(s, fieldsMarketDataType(11, 3)) // delayed

	s.mu.Lock()
	if s.dataType != 3 {
		t.Fatalf("dataType = %d, want 3", s.dataType)
	}
	s.mu.Unlock()
}

func TestSnapshotSession_EndSignalDelivered(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("SPY")
	s.targetReqID.Store(11)

	c.handleSnapshotEnd(s, fieldsTickSnapshotEnd(11))

	select {
	case <-s.gotEndCh:
		// good
	default:
		t.Fatalf("end signal was not delivered to gotEndCh")
	}
}

func TestSnapshotSession_DuplicateEndSignalDoesNotDeadlock(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	s := newSnapshotSession("SPY")
	s.targetReqID.Store(11)

	// Twice — second non-blocking send must be dropped, not deadlock.
	c.handleSnapshotEnd(s, fieldsTickSnapshotEnd(11))
	c.handleSnapshotEnd(s, fieldsTickSnapshotEnd(11))

	// First receive succeeds.
	<-s.gotEndCh
	// Second receive should NOT block — the channel was buffered and the
	// duplicate send was dropped via the default branch.
	select {
	case <-s.gotEndCh:
		t.Fatalf("unexpected second value on gotEndCh; duplicate send should have been dropped")
	default:
	}
}

func TestFetchMarketSnapshot_InactiveSymbolReturnsErr(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.markSymbolInactive("XOM", "delisted")

	_, err := c.FetchMarketSnapshot(context.TODO(), "XOM", 1)
	if err != ErrSymbolInactive {
		t.Fatalf("expected ErrSymbolInactive, got %v", err)
	}
}

func TestFetchMarketSnapshot_NotReadyReturnsUnavailable(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusDisconnected
	c.conn = conn
	c.running = true
	c.ready = false

	_, err := c.FetchMarketSnapshot(context.TODO(), "SPY", 1)
	if err != ErrIBKRUnavailable {
		t.Fatalf("expected ErrIBKRUnavailable, got %v", err)
	}
}

func TestFetchMarketSnapshot_EmptySymbolRejected(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})

	_, err := c.FetchMarketSnapshot(context.TODO(), "  ", 1)
	if err == nil {
		t.Fatalf("expected error for empty symbol")
	}
}
