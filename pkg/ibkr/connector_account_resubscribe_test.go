package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"testing"
	"time"
)

// These tests pin the decision function of the dead-portfolio-stream
// self-heal (maybeResubscribeAccountUpdates). The TWS account-updates
// stream occasionally fails to start after a rapid reconnect (observed
// 2026-06-11: one boot delivered no msgPortfolioValue at all while quotes
// and account summary flowed normally), so GetCachedPositions resubscribes
// behind an empty read. Until the 2026-06-12 pre-release review only the
// leaf predicate accountSummaryShowsPositions was covered — the trigger,
// throttle, and guards shipped untested.

// newAcctResubscribeRig returns a connector wired to a fake connected
// gateway whose outbound frames land in the returned buffer.
func newAcctResubscribeRig(t *testing.T) (*Connector, *Connection, *safeBuffer) {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	var out safeBuffer
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(&out)
	c.conn = conn
	t.Cleanup(conn.rateLimiter.Stop)
	return c, conn, &out
}

// TestGetCachedPositionsHealsDeadPortfolioStream pins the heal end to end:
// an empty position cache while the account summary reports gross position
// value sends exactly one reqAcctData frame, polls inside the throttle
// window stay quiet, and the window boundary (>=) re-arms the attempt. An
// inverted staleness comparison would fail twice here — firing inside the
// window (and self-perpetuating, since every RequestAccountUpdates resets
// the stamp it is compared against) and staying dead at the boundary.
func TestGetCachedPositionsHealsDeadPortfolioStream(t *testing.T) {
	c, conn, out := newAcctResubscribeRig(t)
	conn.accountMu.Lock()
	conn.account = "DU1234567"
	conn.accountSummary["GrossPositionValue"] = "250000.00"
	conn.accountMu.Unlock()

	// Far from wall-clock time so a half-seamed path (one side stamping
	// real time.Now) cannot land near the virtual timeline and pass.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	c.acctUpdatesNow = func() time.Time { return now }

	acctFrame := conn.encodeMsg(reqAcctData, "2", "1", "DU1234567")
	frames := func() int { return bytes.Count(out.Bytes(), acctFrame) }

	positions, err := c.GetCachedPositions()
	if err != nil || len(positions) != 0 {
		t.Fatalf("GetCachedPositions = %v, %v; want empty, nil", positions, err)
	}
	if got := frames(); got != 1 {
		t.Fatalf("first empty poll sent %d reqAcctData frames, want exactly 1", got)
	}

	now = now.Add(acctUpdatesResubscribeThrottle - time.Second)
	if _, err := c.GetCachedPositions(); err != nil {
		t.Fatalf("GetCachedPositions: %v", err)
	}
	if got := frames(); got != 1 {
		t.Fatalf("poll inside throttle window sent %d reqAcctData frames, want 1", got)
	}

	now = now.Add(time.Second) // exactly acctUpdatesResubscribeThrottle after the subscribe
	if _, err := c.GetCachedPositions(); err != nil {
		t.Fatalf("GetCachedPositions: %v", err)
	}
	if got := frames(); got != 2 {
		t.Fatalf("poll at throttle boundary sent %d reqAcctData frames, want 2", got)
	}
}

func TestGetCachedPositionsKeepsZeroValueStockPositionsVisible(t *testing.T) {
	c, conn, _ := newAcctResubscribeRig(t)
	conn.positionsMu.Lock()
	conn.positions = map[string]*RawPosition{
		"AMD": {
			Contract:    Contract{ConID: 4391, Symbol: "AMD", SecType: "STK", Currency: "USD"},
			Position:    20,
			MarketPrice: 100,
			MarketValue: 2000,
		},
		"HGENQ": {
			Contract:      Contract{ConID: 12345, Symbol: "HGENQ", SecType: "STK", Currency: "USD"},
			Position:      20000,
			MarketPrice:   0,
			MarketValue:   0,
			AverageCost:   0.33,
			UnrealizedPNL: -6600,
		},
	}
	conn.positionsMu.Unlock()

	positions, err := c.GetCachedPositions()
	if err != nil {
		t.Fatalf("GetCachedPositions: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("positions len = %d, want AMD and HGENQ: %+v", len(positions), positions)
	}
	if c.IsSymbolInactive("HGENQ") {
		t.Fatal("zero-value stock position must stay visible and must not be marked inactive without broker definition evidence")
	}
	c.contractMu.RLock()
	_, cached := c.contractCache["HGENQ"]
	c.contractMu.RUnlock()
	if cached {
		t.Fatal("zero-value stock position must not seed contract cache for quote routing")
	}
}

func TestGetCachedPositionsKeepsPersistedInactiveHeldZeroValueStockVisible(t *testing.T) {
	c, conn, _ := newAcctResubscribeRig(t)
	if err := c.useInactiveSymbolStore(context.Background(), &stubInactiveStore{
		load: map[string]inactiveSymbolState{
			"HGENQ": {
				reason:   "No security definition has been found for the request",
				markedAt: time.Now(),
			},
		},
	}); err != nil {
		t.Fatalf("useInactiveSymbolStore: %v", err)
	}
	conn.positionsMu.Lock()
	conn.positions = map[string]*RawPosition{
		"HGENQ": {
			Contract:      Contract{ConID: 12345, Symbol: "HGENQ", SecType: "STK", Currency: "USD"},
			Position:      20000,
			MarketPrice:   0,
			MarketValue:   0,
			AverageCost:   0.33,
			UnrealizedPNL: -6600,
		},
	}
	conn.positionsMu.Unlock()

	positions, err := c.GetCachedPositions()
	if err != nil {
		t.Fatalf("GetCachedPositions: %v", err)
	}
	if len(positions) != 1 || positions[0].Contract.Symbol != "HGENQ" {
		t.Fatalf("held inactive zero-value stock should remain visible, got %+v", positions)
	}
	c.contractMu.RLock()
	_, cached := c.contractCache["HGENQ"]
	c.contractMu.RUnlock()
	if cached {
		t.Fatal("held inactive zero-value stock must not seed contract cache for quote routing")
	}
}

// TestMaybeResubscribeAccountUpdatesRequiresConnection pins the connected
// guard: no bytes reach a disconnected wire, and a torn-down connection
// (conn == nil, possible between GetCachedPositions' connectivity check
// and the heal) must not panic the poll path on the GetAccountSummary
// dereference.
func TestMaybeResubscribeAccountUpdatesRequiresConnection(t *testing.T) {
	c, conn, out := newAcctResubscribeRig(t)
	conn.accountMu.Lock()
	conn.accountSummary["GrossPositionValue"] = "250000.00"
	conn.accountMu.Unlock()
	conn.status = StatusDisconnected

	c.maybeResubscribeAccountUpdates()
	if got := out.Len(); got != 0 {
		t.Fatalf("disconnected resubscribe wrote %d bytes to the wire, want 0", got)
	}

	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
	c.maybeResubscribeAccountUpdates()
}

// TestMaybeResubscribeAccountUpdatesSkipsFlatAccount pins the
// genuinely-flat case: with the throttle long expired (zero stamp), only
// the absent gross position value stands between a constantly-polled empty
// cache and a resubscribe loop — a flat account must never trigger one.
func TestMaybeResubscribeAccountUpdatesSkipsFlatAccount(t *testing.T) {
	c, conn, out := newAcctResubscribeRig(t)
	conn.accountMu.Lock()
	conn.account = "DU1234567"
	conn.accountSummary["GrossPositionValue"] = "0"
	conn.accountSummary["NetLiquidation"] = "100000.00"
	conn.accountMu.Unlock()

	if _, err := c.GetCachedPositions(); err != nil {
		t.Fatalf("GetCachedPositions: %v", err)
	}
	if got := out.Len(); got != 0 {
		t.Fatalf("flat account put %d bytes on the wire, want none", got)
	}
}
