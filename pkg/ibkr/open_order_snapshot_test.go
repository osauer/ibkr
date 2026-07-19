package ibkr

import (
	"context"
	"testing"
	"time"
)

func openOrderSnapshotTestFields(orderID, permID string) []string {
	return []string{
		"5", "38", orderID, "265598", "AAPL", "STK", "", "0", "", "1", "SMART", "USD", "", "AAPL",
		"BUY", "10", "LMT", "190.5", "0", "DAY", "Submitted", permID,
	}
}

func TestRequestAllOpenOrdersEncoding(t *testing.T) {
	t.Parallel()
	c := &Connection{}
	got := string(c.encodeMsg(reqAllOpenOrders, "1"))
	if got != "16\x001\x00" {
		t.Fatalf("reqAllOpenOrders encoding = %q, want %q", got, "16\x001\x00")
	}
}

func TestRequestAllOpenOrdersRequiresConnection(t *testing.T) {
	t.Parallel()
	c := &Connection{}
	if err := c.RequestAllOpenOrders(); err == nil {
		t.Fatal("RequestAllOpenOrders on a disconnected Connection must error")
	}
}

func TestSnapshotOpenOrdersCompletesOnOpenOrderEnd(t *testing.T) {
	t.Parallel()
	c := NewConnector(&ConnectorConfig{})
	c.requestAllOpenOrders = func() error {
		c.notifyOrderLifecycle(openOrderSnapshotTestFields("1001", "987654"))
		c.notifyOrderLifecycle(openOrderSnapshotTestFields("1002", "987655"))
		c.finishOpenOrderSnapshot()
		return nil
	}

	snap, err := c.SnapshotOpenOrders(context.Background())
	if err != nil {
		t.Fatalf("SnapshotOpenOrders err = %v", err)
	}
	if !snap.Complete || len(snap.Orders) != 2 {
		t.Fatalf("snapshot = complete=%v orders=%d, want complete with 2 orders", snap.Complete, len(snap.Orders))
	}
	if snap.Orders[0].PermID != 987654 || snap.Orders[1].PermID != 987655 {
		t.Fatalf("snapshot perm ids = %d,%d", snap.Orders[0].PermID, snap.Orders[1].PermID)
	}
	if snap.AsOf.IsZero() {
		t.Fatal("snapshot AsOf is zero")
	}
}

func TestSnapshotOpenOrdersTimeoutIsIncomplete(t *testing.T) {
	t.Parallel()
	c := NewConnector(&ConnectorConfig{})
	c.requestAllOpenOrders = func() error {
		c.notifyOrderLifecycle(openOrderSnapshotTestFields("1001", "987654"))
		return nil // openOrderEnd never arrives
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	snap, err := c.SnapshotOpenOrders(ctx)
	if err == nil {
		t.Fatal("timed-out snapshot should surface ctx error")
	}
	if snap.Complete {
		t.Fatal("timed-out snapshot must not report Complete")
	}
	if len(snap.Orders) != 1 {
		t.Fatalf("partial orders = %d, want 1", len(snap.Orders))
	}
}

func TestSnapshotOpenOrdersExcludesWhatIf(t *testing.T) {
	t.Parallel()
	c := NewConnector(&ConnectorConfig{})
	whatIf := []string{
		"5", "38", "1001", "265598", "AAPL", "STK", "", "0", "", "1", "SMART", "USD", "", "AAPL",
		"BUY", "10", "LMT", "190.5", "0", "DAY", "1", "Submitted", "987654",
	}
	c.requestAllOpenOrders = func() error {
		c.notifyOrderLifecycle(whatIf)
		c.notifyOrderLifecycle(openOrderSnapshotTestFields("1002", "987655"))
		c.finishOpenOrderSnapshot()
		return nil
	}

	snap, err := c.SnapshotOpenOrders(context.Background())
	if err != nil {
		t.Fatalf("SnapshotOpenOrders err = %v", err)
	}
	if !snap.Complete || len(snap.Orders) != 1 || snap.Orders[0].PermID != 987655 {
		t.Fatalf("snapshot = complete=%v orders=%+v, want only the non-WhatIf order", snap.Complete, snap.Orders)
	}
}

func TestSnapshotOpenOrdersIgnoresStrayCallbacksOutsideFlight(t *testing.T) {
	t.Parallel()
	c := NewConnector(&ConnectorConfig{})
	// No snapshot in flight: collection and finish must be harmless no-ops.
	c.notifyOrderLifecycle(openOrderSnapshotTestFields("1001", "987654"))
	c.finishOpenOrderSnapshot()
}
