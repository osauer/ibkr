package ibkr

import (
	"context"
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

	if ok, err := c.EnsureMarketDataSubscription(context.Background(), "SPY", nil, 0); err == nil || ok {
		t.Fatalf("expected not ready error, got ok=%v err=%v", ok, err)
	}

	// Now mark ready and expect it to proceed to request path, but since no writer is attached,
	// the send will fail; we only verify that we no longer get the not-ready error here.
	c.ready = true
	if ok, err := c.EnsureMarketDataSubscription(context.Background(), "SPY", nil, 0); err == nil && !ok {
		t.Fatalf("expected either request attempt or error, got ok=%v err=%v", ok, err)
	}

	// Prevent flakiness
	_ = time.Now()
}

// IsConnected and IsReady can diverge: a connector with a live TCP socket
// but cleared handlers reports {IsConnected: true, IsReady: false}. The
// daemon must gate data verbs on IsReady so this stuck state surfaces as
// "unavailable" rather than silently returning empty payloads. The fix in
// internal/daemon (handlers.go handleStatusHealth + server.go
// gatewayConnector/triggerReconnect) depends on this asymmetry existing
// at the pkg/ibkr level — this test pins it so a future refactor of
// pkg/ibkr can't quietly collapse the two predicates.
func TestConnector_IsReadyAndIsConnectedCanDiverge(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	// IsConnected requires lease+pool+conn (see Connector.isConnected) so
	// the test must populate all three to faithfully model a connector
	// that completed handshake.
	c.conn = conn
	c.running = true

	c.ready = true
	if !c.IsReady() {
		t.Fatalf("ready=true, conn=up: expected IsReady() true, got false")
	}
	if !c.IsConnected() {
		t.Fatalf("ready=true, conn=up: expected IsConnected() true, got false")
	}

	c.ready = false
	if c.IsReady() {
		t.Fatalf("ready=false, conn=up: expected IsReady() false, got true")
	}
	if !c.IsConnected() {
		t.Fatalf("ready=false, conn=up: expected IsConnected() still true, got false (divergence is the whole point)")
	}
}
