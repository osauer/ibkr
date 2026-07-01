package ibkr

import (
	"strings"
	"testing"
)

// These tests pin the reconnect-retry log-volume fix in connection.go. While
// the gateway is down the daemon rebuilds this Connection every cycle, so the
// per-attempt connect narration ("Starting connection process", "Attempting
// connection", "Client N: Connecting to ...") and the degraded-teardown
// "Connection closed" were re-emitted at INFO ~4k×/night, drowning
// ibkr-daemon.log. They log at Debug now; only a real disconnect (wasConnected)
// keeps "Connection closed" at INFO. Follow-up #3 to the order-status log
// dedupe (project_daily_pnl_freeze_2026_07_01). newInfoLogCapture / safeBuffer
// live in the order-status / subscribe test files (same package); the sink is
// process-global, so these must not run in parallel.

// TestConnectionClosed_NeverConnected_StaysOffINFO models the off-hours rebuild
// loop tearing down a connection-refused socket: wasConnected is false, so the
// line must not reach INFO.
func TestConnectionClosed_NeverConnected_StaysOffINFO(t *testing.T) {
	out := newInfoLogCapture(t)
	c := NewConnection(nil) // Disconnect stops the rate limiter itself.

	if err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := string(out.Bytes()); strings.Contains(got, "Connection closed") {
		t.Fatalf("never-connected Disconnect logged \"Connection closed\" at INFO; want Debug only:\n%s", got)
	}
}

// TestConnectionClosed_WasConnected_LogsAtINFO pins the other half: a real
// disconnect (we were Connected) is a genuine state change and must stay at
// INFO — the demotion is scoped to the never-connected teardown only.
func TestConnectionClosed_WasConnected_LogsAtINFO(t *testing.T) {
	out := newInfoLogCapture(t)
	c := NewConnection(nil) // Disconnect stops the rate limiter itself.

	c.statusMu.Lock()
	c.status = StatusConnected
	c.statusMu.Unlock()

	if err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := string(out.Bytes()); !strings.Contains(got, "Connection closed") {
		t.Fatalf("real disconnect (wasConnected) must log \"Connection closed\" at INFO; got:\n%s", got)
	}
}
