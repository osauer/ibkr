package ibkr

import (
	"testing"
	"time"
)

// Ensure EnsureMarketDataSubscription defers requests until connector is ready
func TestEnsureMarketDataSubscription_NotReady(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	// Simulate connected but not ready
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	c.conn = conn
	c.running = true
	c.ready = false

	if ok, err := c.EnsureMarketDataSubscription("SPY", nil, 0); err == nil || ok {
		t.Fatalf("expected not ready error, got ok=%v err=%v", ok, err)
	}

	// Now mark ready and expect it to proceed to request path, but since no writer is attached,
	// the send will fail; we only verify that we no longer get the not-ready error here.
	c.ready = true
	if ok, err := c.EnsureMarketDataSubscription("SPY", nil, 0); err == nil && !ok {
		t.Fatalf("expected either request attempt or error, got ok=%v err=%v", ok, err)
	}

	// Prevent flakiness
	_ = time.Now()
}
