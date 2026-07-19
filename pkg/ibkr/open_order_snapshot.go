package ibkr

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// OpenOrderSnapshot is a one-shot broker truth read of every open order the
// gateway reports for the session (API clients and manual TWS orders alike).
// Complete is true only when the gateway's openOrderEnd terminator arrived;
// an incomplete snapshot proves nothing about absent orders and must never be
// used to infer that an order is gone.
type OpenOrderSnapshot struct {
	Complete bool
	Orders   []OrderLifecycleEvent
	AsOf     time.Time
}

// openOrderSnapshotCollector accumulates openOrder callbacks between a
// reqAllOpenOrders request and its openOrderEnd terminator.
type openOrderSnapshotCollector struct {
	mu     sync.Mutex
	orders []OrderLifecycleEvent
	done   chan struct{}
	closed bool
}

func (col *openOrderSnapshotCollector) add(ev OrderLifecycleEvent) {
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.closed {
		return
	}
	col.orders = append(col.orders, ev)
}

func (col *openOrderSnapshotCollector) finish() {
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.closed {
		return
	}
	col.closed = true
	close(col.done)
}

func (col *openOrderSnapshotCollector) snapshot() []OrderLifecycleEvent {
	col.mu.Lock()
	defer col.mu.Unlock()
	return append([]OrderLifecycleEvent{}, col.orders...)
}

// RequestAllOpenOrders asks the gateway for a snapshot of all open orders for
// the session; results arrive as openOrder/orderStatus callbacks terminated
// by openOrderEnd. This is a read request — deliberately not gated behind the
// tradingEnabled build flag, so read-only builds can reconcile stale journal
// rows too.
func (c *Connection) RequestAllOpenOrders() error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	msg := c.encodeMsg(reqAllOpenOrders, "1")
	return c.sendMessageWithType(msg, RequestTypeOrder)
}

func (c *Connector) requestAllOpenOrdersDefault() error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return fmt.Errorf("no active connection")
	}
	return conn.RequestAllOpenOrders()
}

// SnapshotOpenOrders performs a one-shot broker open-order snapshot. It
// installs a collector, issues reqAllOpenOrders, and waits for openOrderEnd
// or ctx expiry. WhatIf preview echoes never reach the collector because
// notifyOrderLifecycle drops them before collection. Callers are serialized;
// one in-flight snapshot at a time.
func (c *Connector) SnapshotOpenOrders(ctx context.Context) (OpenOrderSnapshot, error) {
	if c == nil {
		return OpenOrderSnapshot{}, fmt.Errorf("connector is nil")
	}
	c.openOrderSnapshotFlightMu.Lock()
	defer c.openOrderSnapshotFlightMu.Unlock()

	collector := &openOrderSnapshotCollector{done: make(chan struct{})}
	c.openOrderSnapshotMu.Lock()
	c.openOrderSnapshot = collector
	c.openOrderSnapshotMu.Unlock()
	defer func() {
		c.openOrderSnapshotMu.Lock()
		c.openOrderSnapshot = nil
		c.openOrderSnapshotMu.Unlock()
	}()

	request := c.requestAllOpenOrders
	if request == nil {
		request = c.requestAllOpenOrdersDefault
	}
	if err := request(); err != nil {
		return OpenOrderSnapshot{AsOf: time.Now().UTC()}, err
	}

	select {
	case <-collector.done:
		return OpenOrderSnapshot{Complete: true, Orders: collector.snapshot(), AsOf: time.Now().UTC()}, nil
	case <-ctx.Done():
		return OpenOrderSnapshot{Complete: false, Orders: collector.snapshot(), AsOf: time.Now().UTC()}, ctx.Err()
	}
}

func (c *Connector) collectOpenOrderSnapshot(ev OrderLifecycleEvent) {
	c.openOrderSnapshotMu.Lock()
	collector := c.openOrderSnapshot
	c.openOrderSnapshotMu.Unlock()
	if collector == nil {
		return
	}
	collector.add(ev)
}

func (c *Connector) finishOpenOrderSnapshot() {
	c.openOrderSnapshotMu.Lock()
	collector := c.openOrderSnapshot
	c.openOrderSnapshotMu.Unlock()
	if collector == nil {
		return
	}
	collector.finish()
}
