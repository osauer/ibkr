package ibkr

import (
	"log/slog"
	"strings"
	"testing"
)

// These tests pin the order-status log dedupe (logOrderStatus /
// orderStatusChanged). IBKR re-sends orderStatus frames for unchanged working
// orders many times per second; logging each at INFO grew ibkr-daemon.log to
// 1.5 GB (observed 2026-07-01). Only a change in status/filled/remaining may
// reach INFO now — verbatim repeats drop to Debug, which the default level
// discards. This is a logging-volume fix: order state tracking must be
// untouched, so the terminal-removal behavior is pinned alongside.

// newInfoLogCapture installs a buffer sink at INFO level for the duration of a
// test and restores the package defaults (discard sink, info level) after. The
// logging package sink is process-global, so callers must not run in parallel.
func newInfoLogCapture(t *testing.T) *safeBuffer {
	t.Helper()
	var out safeBuffer
	SetLogger(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	SetLogLevel("info")
	t.Cleanup(func() {
		SetLogger(nil)
		SetLogLevel("info")
	})
	return &out
}

func newOrderStatusRig(t *testing.T) *Connector {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(func() {
		if c.conn != nil && c.conn.rateLimiter != nil {
			c.conn.rateLimiter.Stop()
		}
	})
	return c
}

func countOrderStatusLines(out *safeBuffer, orderID string) int {
	return strings.Count(string(out.Bytes()), "Order status - ID: "+orderID+",")
}

// presubmittedFrame is a resting working order: PreSubmitted, nothing filled.
// isNumeric("PreSubmitted") is false, so handleOrderStatus keeps start=1 and
// reads orderID/status/filled/remaining from fields[1..4].
func presubmittedFrame(orderID string) []string {
	return []string{"3", orderID, "PreSubmitted", "0", "100", "0"}
}

func TestHandleOrderStatus_UnchangedRepeatsStayOffINFO(t *testing.T) {
	out := newInfoLogCapture(t)
	c := newOrderStatusRig(t)
	c.openOrders["87"] = &Order{ID: "87", Symbol: "SPY", Status: OrderStatusSubmitted}

	// The flood: the same resting PreSubmitted frame re-sent many times.
	frame := presubmittedFrame("87")
	for range 8 {
		c.handleOrderStatus(frame)
	}
	if got := countOrderStatusLines(out, "87"); got != 1 {
		t.Fatalf("8 identical PreSubmitted frames logged %d INFO lines, want exactly 1", got)
	}

	// A genuine change (partial fill) earns one fresh INFO line; its own
	// repeats then go quiet again.
	partial := []string{"3", "87", "PreSubmitted", "40", "60", "40.10"}
	c.handleOrderStatus(partial)
	c.handleOrderStatus(partial)
	c.handleOrderStatus(partial)
	if got := countOrderStatusLines(out, "87"); got != 2 {
		t.Fatalf("after a partial fill + repeats, %d INFO lines, want 2", got)
	}
}

func TestOrderStatusChanged_TracksSignatureAcrossOrders(t *testing.T) {
	t.Parallel()
	c := newOrderStatusRig(t)

	sigResting := orderStatusLogSignature("PreSubmitted", 0, 100)
	if !c.orderStatusChanged("87", sigResting) {
		t.Fatal("first frame for order 87: want changed=true")
	}
	if c.orderStatusChanged("87", sigResting) {
		t.Fatal("identical repeat for order 87: want changed=false")
	}

	sigPartial := orderStatusLogSignature("PreSubmitted", 40, 60)
	if !c.orderStatusChanged("87", sigPartial) {
		t.Fatal("partial fill moved the signature: want changed=true")
	}

	// A second order id is tracked independently — order 87's churn must not
	// suppress order 99's first frame.
	if !c.orderStatusChanged("99", sigResting) {
		t.Fatal("first frame for order 99: want changed=true")
	}

	// Forgetting a terminal/removed order lets a later reused id log fresh.
	c.forgetOrderStatusLog("87")
	if !c.orderStatusChanged("87", sigPartial) {
		t.Fatal("after forget, the same signature again: want changed=true")
	}
}

// TestHandleOrderStatus_TerminalForgetsSignature pins the memory-hygiene half:
// a terminal frame removes the order from openOrders (unchanged behavior) and
// drops its dedupe signature so the map cannot accumulate a slot per order id
// forever.
func TestHandleOrderStatus_TerminalForgetsSignature(t *testing.T) {
	t.Parallel()
	c := newOrderStatusRig(t)
	c.openOrders["87"] = &Order{ID: "87", Symbol: "SPY", Status: OrderStatusSubmitted}

	c.handleOrderStatus(presubmittedFrame("87"))
	c.handleOrderStatus([]string{"3", "87", "Filled", "100", "0", "50.25"})

	c.orderStatusLogMu.Lock()
	_, sigPresent := c.orderStatusLogSig["87"]
	c.orderStatusLogMu.Unlock()
	if sigPresent {
		t.Fatal("terminal Filled frame should forget the dedupe signature for 87")
	}

	c.orderMu.RLock()
	_, stillOpen := c.openOrders["87"]
	c.orderMu.RUnlock()
	if stillOpen {
		t.Fatal("Filled order should be removed from openOrders (unchanged behavior)")
	}
}
