package ibkr

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// OpenOrderSnapshot is a one-shot read of the API-created open orders the
// gateway reports across client IDs. It does not bind or include manual TWS
// orders merely because Complete is true; those require the separate client-0
// open-order binding flow, which this request does not perform.
//
// Complete is true only when openOrderEnd arrived. When Complete is false,
// Orders may contain callbacks collected before the context ended, but that
// proves nothing about absent orders. A request failure returns no collected
// orders.
// AsOf is the local UTC completion or failure time, not a broker timestamp.
type OpenOrderSnapshot struct {
	Complete bool                  // Complete reports whether openOrderEnd arrived.
	Orders   []OrderLifecycleEvent // Orders contains collected openOrder events and may be empty.
	AsOf     time.Time             // AsOf is the local UTC time at which the method returned.
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

// RequestAllOpenOrders sends reqAllOpenOrders to the gateway. Results arrive
// asynchronously as openOrder and orderStatus callbacks followed by
// openOrderEnd; a nil error means only that the request frame was accepted for
// the socket write. This read request is available in both build modes.
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

// SnapshotOpenOrders performs a one-shot broker open-order snapshot and waits
// for openOrderEnd or ctx completion. Concurrent calls are serialized, and a
// call waiting for another snapshot does not observe ctx until it acquires the
// serialization lock. WhatIf preview echoes are excluded.
//
// On openOrderEnd it returns Complete=true and a nil error. On ctx completion it
// returns Complete=false, callbacks collected so far, a local UTC AsOf, and
// ctx.Err(). A request failure returns Complete=false, no collected orders, a
// local UTC AsOf, and the request error. An incomplete or empty snapshot must
// not be used to infer that an order is absent or final.
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
