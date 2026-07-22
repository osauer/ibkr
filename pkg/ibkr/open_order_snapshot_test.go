package ibkr

import (
	"bufio"
	"context"
	"errors"
	"sync/atomic"
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
		c.collectOpenOrderSnapshotFields(openOrderSnapshotTestFields("1001", "987654"))
		c.collectOpenOrderSnapshotFields(openOrderSnapshotTestFields("1002", "987655"))
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

func TestSnapshotOpenOrdersCapturesLifecycleGeneration(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	fields := openOrderSnapshotTestFields("1001", "987654")
	c.requestAllOpenOrders = func() error {
		c.collectOpenOrderSnapshotFields(fields)
		c.notifyOrderLifecycle(fields)
		c.finishOpenOrderSnapshot()
		return nil
	}

	snapshot, err := c.SnapshotOpenOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Complete || snapshot.Generation == 0 || snapshot.Generation != c.OrderLifecycleGeneration() {
		t.Fatalf("snapshot generation=%d current=%d complete=%t", snapshot.Generation, c.OrderLifecycleGeneration(), snapshot.Complete)
	}
	c.notifyOrderLifecycle(fields)
	if current := c.OrderLifecycleGeneration(); current <= snapshot.Generation {
		t.Fatalf("later order callback generation=%d, want greater than receipt %d", current, snapshot.Generation)
	}
}

func TestSnapshotOpenOrdersTimeoutIsIncomplete(t *testing.T) {
	t.Parallel()
	c := NewConnector(&ConnectorConfig{})
	c.openOrderSnapshotTimeout = 100 * time.Millisecond
	c.requestAllOpenOrders = func() error {
		c.collectOpenOrderSnapshotFields(openOrderSnapshotTestFields("1001", "987654"))
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
		c.collectOpenOrderSnapshotFields(whatIf)
		c.collectOpenOrderSnapshotFields(openOrderSnapshotTestFields("1002", "987655"))
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
	c.collectOpenOrderSnapshotFields(openOrderSnapshotTestFields("1001", "987654"))
	c.finishOpenOrderSnapshot()
}

func TestSnapshotOpenOrdersCanceledWaiterIssuesNoLateWireRequest(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	entered := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	c.requestAllOpenOrders = func() error {
		if requests.Add(1) == 1 {
			close(entered)
			<-release
		}
		c.finishOpenOrderSnapshot()
		return nil
	}

	first := make(chan error, 1)
	go func() {
		_, err := c.SnapshotOpenOrders(context.Background())
		first <- err
	}()
	<-entered

	waitCtx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, err := c.SnapshotOpenOrders(waitCtx)
		waiter <- err
	}()
	cancel()
	select {
	case err := <-waiter:
		if err == nil {
			t.Fatal("canceled waiter unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked behind snapshot flight")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("wire requests before leader release = %d, want 1", got)
	}

	close(release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("leader snapshot err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("leader snapshot did not complete")
	}
	time.Sleep(10 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("canceled waiter issued late wire request: requests=%d", got)
	}
}

func TestSnapshotOpenOrdersCallerTimeoutDrainsLateEndBeforeNextFlight(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.openOrderSnapshotTimeout = time.Second
	var requests atomic.Int32
	c.requestAllOpenOrders = func() error {
		switch requests.Add(1) {
		case 1:
			c.collectOpenOrderSnapshotFields(openOrderSnapshotTestFields("1001", "987654"))
		case 2:
			c.collectOpenOrderSnapshotFields(openOrderSnapshotTestFields("2001", "987656"))
			c.finishOpenOrderSnapshot()
		}
		return nil
	}

	firstCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	first, err := c.SnapshotOpenOrders(firstCtx)
	if err == nil || first.Complete || len(first.Orders) != 1 {
		t.Fatalf("timed-out first waiter = %+v err=%v, want incomplete partial", first, err)
	}

	secondDone := make(chan OpenOrderSnapshot, 1)
	go func() {
		snap, _ := c.SnapshotOpenOrders(context.Background())
		secondDone <- snap
	}()
	time.Sleep(10 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("second waiter issued a new request before late end: %d", got)
	}
	c.collectOpenOrderSnapshotFields(openOrderSnapshotTestFields("1002", "987655"))
	c.finishOpenOrderSnapshot()
	select {
	case second := <-secondDone:
		if !second.Complete || len(second.Orders) != 2 {
			t.Fatalf("joined drained flight = complete=%v orders=%d", second.Complete, len(second.Orders))
		}
	case <-time.After(time.Second):
		t.Fatal("second waiter did not complete on late same-flight terminator")
	}

	third, err := c.SnapshotOpenOrders(context.Background())
	if err != nil || !third.Complete || len(third.Orders) != 1 || third.Orders[0].OrderID != 2001 {
		t.Fatalf("next clean flight = %+v err=%v, want only order 2001", third, err)
	}
}

func TestSnapshotOpenOrdersProtocolTimeoutPoisonsOnlyCurrentEpoch(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.openOrderSnapshotTimeout = 25 * time.Millisecond
	var requests atomic.Int32
	c.requestAllOpenOrders = func() error {
		if requests.Add(1) == 2 {
			c.finishOpenOrderSnapshot()
		}
		return nil
	}

	if _, err := c.SnapshotOpenOrders(context.Background()); !errors.Is(err, ErrOpenOrderSnapshotPoisoned) {
		t.Fatalf("protocol timeout err = %v, want poisoned epoch", err)
	}
	if _, err := c.SnapshotOpenOrders(context.Background()); !errors.Is(err, ErrOpenOrderSnapshotPoisoned) {
		t.Fatalf("same-epoch retry err = %v, want poisoned epoch", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("poisoned same epoch wire requests = %d, want 1", got)
	}

	c.conn.resetOrderIDReadiness()
	refreshed, err := c.SnapshotOpenOrders(context.Background())
	if err != nil || !refreshed.Complete {
		t.Fatalf("new epoch snapshot = %+v err=%v, want complete", refreshed, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("new epoch wire requests = %d, want 2", got)
	}
}

func TestSnapshotOpenOrdersRejectsOldConnectionCallbacks(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.openOrderSnapshotTimeout = 150 * time.Millisecond
	oldConn := c.conn
	c.registerHandlers(oldConn)
	var requests atomic.Int32
	c.requestAllOpenOrders = func() error {
		requests.Add(1)
		return nil
	}

	oldCtx, cancelOld := context.WithTimeout(context.Background(), 15*time.Millisecond)
	_, _ = c.SnapshotOpenOrders(oldCtx)
	cancelOld()

	newConn := NewConnection(nil)
	t.Cleanup(newConn.rateLimiter.Stop)
	c.mu.Lock()
	c.conn = newConn
	c.mu.Unlock()
	c.registerHandlers(newConn)

	newDone := make(chan OpenOrderSnapshot, 1)
	go func() {
		snap, _ := c.SnapshotOpenOrders(context.Background())
		newDone <- snap
	}()
	deadline := time.Now().Add(time.Second)
	for requests.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if requests.Load() != 2 {
		t.Fatalf("new connection flight did not start: requests=%d", requests.Load())
	}

	oldConn.processMessageAtEpoch(encodeOpenOrderSnapshotTestMessage(oldConn, openOrderSnapshotTestFields("1001", "987654")), oldConn.BrokerSessionEpoch())
	oldConn.processMessageAtEpoch(oldConn.encodeMsg(msgOpenOrderEnd, "1"), oldConn.BrokerSessionEpoch())
	select {
	case snap := <-newDone:
		t.Fatalf("old connection completed new snapshot: %+v", snap)
	case <-time.After(20 * time.Millisecond):
	}

	newConn.processMessageAtEpoch(encodeOpenOrderSnapshotTestMessage(newConn, openOrderSnapshotTestFields("2001", "987655")), newConn.BrokerSessionEpoch())
	newConn.processMessageAtEpoch(newConn.encodeMsg(msgOpenOrderEnd, "1"), newConn.BrokerSessionEpoch())
	select {
	case snap := <-newDone:
		if !snap.Complete || len(snap.Orders) != 1 || snap.Orders[0].OrderID != 2001 {
			t.Fatalf("new connection snapshot = %+v, want only order 2001", snap)
		}
	case <-time.After(time.Second):
		t.Fatal("new connection callbacks did not complete snapshot")
	}
}

func TestSnapshotOpenOrdersEpochCheckPreventsReconnectGapWire(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := c.conn
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder-1)
	var out safeBuffer
	conn.writer = bufio.NewWriter(&out)
	c.registerHandlers(conn)
	entered := make(chan struct{})
	release := make(chan struct{})
	c.openOrderSnapshotBeforeSend = func() {
		close(entered)
		<-release
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := c.SnapshotOpenOrders(context.Background())
		firstDone <- err
	}()
	<-entered
	conn.pauseTransport()
	conn.resetOrderIDReadiness()
	conn.observeNextValidOrderID(100)
	conn.resumeTransport()
	close(release)
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("old-epoch pre-send flight unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("old-epoch pre-send flight did not fail")
	}
	if out.Len() != 0 {
		t.Fatalf("old-epoch flight wrote %d bytes on new socket", out.Len())
	}

	c.openOrderSnapshotBeforeSend = nil
	secondDone := make(chan OpenOrderSnapshot, 1)
	go func() {
		snap, _ := c.SnapshotOpenOrders(context.Background())
		secondDone <- snap
	}()
	deadline := time.Now().Add(time.Second)
	for out.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if out.Len() == 0 {
		t.Fatal("new-epoch snapshot did not write request")
	}
	c.collectOpenOrderSnapshotFields(openOrderSnapshotTestFields("3001", "987657"))
	c.finishOpenOrderSnapshot()
	select {
	case snap := <-secondDone:
		if !snap.Complete || len(snap.Orders) != 1 || snap.Orders[0].OrderID != 3001 {
			t.Fatalf("new-epoch snapshot = %+v, want only order 3001", snap)
		}
	case <-time.After(time.Second):
		t.Fatal("new-epoch snapshot did not complete")
	}
}

func TestSnapshotOpenOrdersPartialWriteAttemptsOnceAndPoisonsEpoch(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := c.conn
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	conn.observeNextValidOrderID(100)
	writer := &partialOrderWriteError{}
	conn.writer = bufio.NewWriterSize(writer, 64*1024)

	before := conn.rateLimiter.GetMetrics().TotalRequests
	if _, err := c.SnapshotOpenOrders(context.Background()); err == nil || !brokerSendMayHaveBeenWritten(err) {
		t.Fatalf("partial reqAllOpenOrders err=%v, want uncertain send", err)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests - before; got != 1 {
		t.Fatalf("reqAllOpenOrders dispatches=%d, want exactly one", got)
	}
	if got := writer.calls.Load(); got != 1 {
		t.Fatalf("reqAllOpenOrders underlying writes=%d, want exactly one", got)
	}
	if _, err := c.SnapshotOpenOrders(context.Background()); !errors.Is(err, ErrOpenOrderSnapshotPoisoned) {
		t.Fatalf("same-epoch retry err=%v, want poisoned socket generation", err)
	}
}

func TestSnapshotOpenOrdersLimiterTimeoutCancelsQueuedSend(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := c.conn
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	conn.observeNextValidOrderID(100)
	var out safeBuffer
	conn.writer = bufio.NewWriter(&out)
	conn.rateLimiter.submitTimeoutFn = func(RequestType) time.Duration { return 20 * time.Millisecond }
	conn.rateLimiter.messageRate.mu.Lock()
	conn.rateLimiter.messageRate.tokens = 0
	conn.rateLimiter.messageRate.refillRate = 0.001
	conn.rateLimiter.messageRate.lastRefill = time.Now()
	conn.rateLimiter.messageRate.mu.Unlock()

	if _, err := c.SnapshotOpenOrders(context.Background()); err == nil {
		t.Fatal("rate-limiter completion timeout unexpectedly succeeded")
	} else if brokerSendMayHaveBeenWritten(err) {
		t.Fatalf("queued-not-entered timeout reported uncertain: %v", err)
	}

	// Make pacing immediately available after the caller has returned. The
	// canceled first dispatch must not wake up and write a late uncorrelated
	// request when the bucket is refilled.
	conn.rateLimiter.messageRate.mu.Lock()
	conn.rateLimiter.messageRate.tokens = 1
	conn.rateLimiter.messageRate.refillRate = 40
	conn.rateLimiter.messageRate.lastRefill = time.Now()
	conn.rateLimiter.messageRate.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if got := out.Len(); got != 0 {
		t.Fatalf("timed-out queued request wrote %d late bytes", got)
	}

	conn.rateLimiter.submitTimeoutFn = nil
	secondDone := make(chan struct {
		snap OpenOrderSnapshot
		err  error
	}, 1)
	go func() {
		snap, err := c.SnapshotOpenOrders(context.Background())
		secondDone <- struct {
			snap OpenOrderSnapshot
			err  error
		}{snap: snap, err: err}
	}()
	deadline := time.Now().Add(time.Second)
	for out.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if out.Len() == 0 {
		t.Fatal("clean flight after canceled queued request did not reach wire")
	}
	c.finishOpenOrderSnapshot()
	select {
	case got := <-secondDone:
		if got.err != nil || !got.snap.Complete || len(got.snap.Orders) != 0 {
			t.Fatalf("clean flight = snap %+v err %v, want complete empty uncontaminated result", got.snap, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("clean flight did not complete")
	}
}

func encodeOpenOrderSnapshotTestMessage(conn *Connection, fields []string) []byte {
	values := make([]any, len(fields))
	for i := range fields {
		values[i] = fields[i]
	}
	return conn.encodeMsg(values...)
}
