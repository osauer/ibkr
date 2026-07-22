package ibkr

import (
	"bufio"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestSystemNoticeDispatchUsesPreinstalledHandler(t *testing.T) {
	t.Parallel()
	conn := &Connection{}

	farm := func(code int, msg string) *systemNotification {
		return &systemNotification{tickerID: -1, code: code, message: msg}
	}

	var got []int
	conn.SetSystemNoticeHandler(func(note *systemNotification, _ reqAliasEntry) {
		got = append(got, note.code)
	})
	conn.dispatchSystemNotice(farm(2104, "Market data farm connection is OK:usfarm"), reqAliasEntry{})
	conn.dispatchSystemNotice(farm(2106, "HMDS data farm connection is OK:ushmds"), reqAliasEntry{})
	if want := []int{2104, 2106}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatched codes = %v, want %v", got, want)
	}

	// After wiring, notices dispatch live and are not re-buffered.
	got = nil
	conn.dispatchSystemNotice(farm(2103, "Market data farm connection is broken:usfarm"), reqAliasEntry{})
	if want := []int{2103}; !reflect.DeepEqual(got, want) {
		t.Fatalf("post-wire dispatch = %v, want %v (live dispatch, no buffering)", got, want)
	}

}

func TestSystemNoticeReplayPreservesInboundSocketEpoch(t *testing.T) {
	conn := &Connection{}
	note := &systemNotification{tickerID: 77, code: 201, message: "rejected"}
	var gotEpoch uint64
	conn.SetSystemNoticeHandlerAtEpoch(func(_ *systemNotification, _ reqAliasEntry, epoch uint64) {
		gotEpoch = epoch
	})
	conn.dispatchSystemNotice(note, reqAliasEntry{}, 7)
	if gotEpoch != 7 {
		t.Fatalf("system-notice epoch=%d, want inbound epoch 7", gotEpoch)
	}
}

// TestPreRegisteredReadLoopRecordsFarmNotice is the end-to-end guard for the
// false-degraded farm status: Connector installs this handler before Connect
// starts readMessages, so the first msg-204 burst is processed in wire order.
func TestPreRegisteredReadLoopRecordsFarmNotice(t *testing.T) {
	t.Parallel()
	c := &Connector{}
	conn := &Connection{
		msgHandlers:   map[int][]handlerEntry{},
		serverVersion: 203,
		config:        &ConnectionConfig{ClientID: 15},
	}
	conn.status = StatusConnecting
	c.conn = conn
	conn.SetSystemNoticeHandlerAtEpoch(func(note *systemNotification, alias reqAliasEntry, epoch uint64) {
		c.processSystemNoticeFrom(ConnectorSessionBinding{connector: c, connection: conn, epoch: epoch}, alias, note)
	})

	// tickerID -1 == global-scope notice, matching the gateway's farm burst.
	conn.processMessage(encodeSystemNotificationForTest(-1, 2104, "Market data farm connection is OK:usfarm", ""))
	conn.processMessage(encodeSystemNotificationForTest(-1, 2106, "HMDS data farm connection is OK:ushmds", ""))

	byType := map[string]DataFarmStatus{}
	for _, f := range c.DataFarmStatuses() {
		byType[f.Type] = f
	}
	if got, ok := byType["market"]; !ok || got.Status != "ok" || got.Name != "usfarm" {
		t.Fatalf("market farm = %+v (ok=%v), want usfarm ok recorded via replay", got, ok)
	}
	if got, ok := byType["historical"]; !ok || got.Status != "ok" || got.Name != "ushmds" {
		t.Fatalf("historical farm = %+v (ok=%v), want ushmds ok recorded via replay", got, ok)
	}
}

func TestCurrentSystemNotice10197SendsDelayedModeExactlyOnce(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	connector.registerHandlers(conn)
	epoch := conn.BrokerSessionEpoch()
	notice := encodeSystemNotificationForTest(31, 10197, "competing live session", "")

	conn.processMessageAtEpoch(notice, epoch)
	firstLen := socket.Len()
	if firstLen == 0 {
		t.Fatal("current 10197 did not emit delayed-market-data frame")
	}
	if !conn.HasCompetingLiveSession() {
		t.Fatal("current 10197 did not mark competing live session")
	}
	conn.processMessageAtEpoch(notice, epoch)
	if got := socket.Len(); got != firstLen {
		t.Fatalf("duplicate 10197 emitted another frame: before=%d after=%d", firstLen, got)
	}
}

func TestSystemNoticeEpochRolloverAfterFastCheckIsStaleOnly(t *testing.T) {
	conn, connector, oldSocket, newSocket, _ := newQueuedInstructionReconnectFixture(t)
	connector.registerHandlers(conn)
	oldEpoch := conn.BrokerSessionEpoch()
	entered := make(chan struct{})
	release := make(chan struct{})
	conn.systemNoticeAfterInitialEpochCheck = func() {
		close(entered)
		<-release
	}
	defer func() { conn.systemNoticeAfterInitialEpochCheck = nil }()
	var legacyCalls atomic.Int32
	conn.RegisterHandler(msgSystemNotification, func([]string) { legacyCalls.Add(1) })
	done := make(chan struct{})
	go func() {
		conn.processMessageAtEpoch(encodeSystemNotificationForTest(31, 10197, "competing live session", ""), oldEpoch)
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("system notice did not reach post-fast-check pause")
	}
	conn.resetOrderIDReadiness()
	conn.writer = newBufferedSafeWriter(newSocket)
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stale system notice did not complete after epoch rollover")
	}
	if conn.HasCompetingLiveSession() {
		t.Fatal("stale 10197 mutated current competing-session state")
	}
	if got := legacyCalls.Load(); got != 0 {
		t.Fatalf("stale 10197 reached %d current legacy handlers", got)
	}
	if oldSocket.Len() != 0 || newSocket.Len() != 0 {
		t.Fatalf("stale 10197 wrote bytes old=%d new=%d", oldSocket.Len(), newSocket.Len())
	}
}

func TestSystemNotice10197PostActionReleasesAllMutationBarriers(t *testing.T) {
	conn, connector, oldSocket, newSocket, _ := newQueuedInstructionReconnectFixture(t)
	connector.registerHandlers(conn)
	conn.pauseTransport()
	defer conn.resumeTransport()
	before := conn.rateLimiter.GetMetrics().TotalRequests
	done := make(chan struct{})
	go func() {
		conn.processMessageAtEpoch(encodeSystemNotificationForTest(31, 10197, "competing live session", ""), conn.BrokerSessionEpoch())
		close(done)
	}()
	waitForProtectedDispatch(t, conn, before)

	mutationDone := make(chan struct{})
	go func() {
		connector.WithBrokerEvidenceMutation(func() {})
		close(mutationDone)
	}()
	select {
	case <-mutationDone:
	case <-time.After(time.Second):
		t.Fatal("10197 post action retained connector publication/evidence barrier")
	}
	conn.resetOrderIDReadiness()
	conn.writer = newBufferedSafeWriter(newSocket)
	conn.resumeTransport()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retired 10197 post action did not finish")
	}
	if oldSocket.Len() != 0 || newSocket.Len() != 0 {
		t.Fatalf("retired 10197 wrote bytes old=%d new=%d", oldSocket.Len(), newSocket.Len())
	}
}

func TestError200InactiveCleanupRunsAfterAllMutationBarriers(t *testing.T) {
	conn, connector, oldSocket, newSocket, _ := newQueuedInstructionReconnectFixture(t)
	connector.registerHandlers(conn)
	const (
		key   = "DEAD"
		reqID = 101
	)
	message := "No security definition has been found"
	connector.subMu.Lock()
	connector.subscriptions[key] = &Subscription{Symbol: key, ReqID: reqID, SessionEpoch: conn.BrokerSessionEpoch()}
	connector.reqIDMap[reqID] = key
	connector.subMu.Unlock()
	if marked := connector.registerInactiveCandidate(key, message); marked {
		t.Fatal("first definition error unexpectedly marked symbol inactive")
	}

	conn.pauseTransport()
	before := conn.rateLimiter.GetMetrics().TotalRequests
	done := make(chan struct{})
	go func() {
		conn.processMessageAtEpoch(conn.encodeMsg(msgErrMsg, "2", reqID, 200, message), conn.BrokerSessionEpoch())
		close(done)
	}()
	waitForProtectedDispatch(t, conn, before)

	mutationDone := make(chan struct{})
	go func() {
		connector.WithBrokerEvidenceMutation(func() {})
		close(mutationDone)
	}()
	select {
	case <-mutationDone:
	case <-time.After(time.Second):
		t.Fatal("error-200 post action retained inbound/publication/evidence barrier")
	}
	conn.resetOrderIDReadiness()
	conn.writer = newBufferedSafeWriter(newSocket)
	conn.resumeTransport()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retired error-200 cleanup did not finish")
	}
	if oldSocket.Len() != 0 || newSocket.Len() != 0 {
		t.Fatalf("retired error-200 cleanup wrote bytes old=%d new=%d", oldSocket.Len(), newSocket.Len())
	}
	connector.subMu.RLock()
	_, retained := connector.subscriptions[key]
	connector.subMu.RUnlock()
	if retained || !connector.IsSymbolInactive(key) {
		t.Fatalf("inactive cleanup state retained=%t inactive=%t", retained, connector.IsSymbolInactive(key))
	}
}

func newBufferedSafeWriter(socket *safeBuffer) *bufio.Writer {
	return bufio.NewWriter(socket)
}
