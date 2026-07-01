package ibkr

import (
	"reflect"
	"testing"
)

// TestSystemNoticeDispatchBuffersUntilHandlerWired pins the startup-race fix:
// the gateway sends the farm-status burst (2104/2106/2158) immediately after
// startAPI, and readMessages consumes it before Connect() runs onConnect ->
// registerHandlers -> SetSystemNoticeHandler. Before the fix those notices were
// logged but dropped, and since the gateway only re-sends farm status on
// change, DataFarmStatuses() stayed empty for the whole session — the
// false-degraded quote/scanner/history/chain status. dispatchSystemNotice must
// buffer while the handler is nil and SetSystemNoticeHandler must replay in
// arrival order.
func TestSystemNoticeDispatchBuffersUntilHandlerWired(t *testing.T) {
	t.Parallel()
	conn := &Connection{}

	farm := func(code int, msg string) *systemNotification {
		return &systemNotification{tickerID: -1, code: code, message: msg}
	}

	// Two farm notices arrive before any handler is wired.
	conn.dispatchSystemNotice(farm(2104, "Market data farm connection is OK:usfarm"), reqAliasEntry{})
	conn.dispatchSystemNotice(farm(2106, "HMDS data farm connection is OK:ushmds"), reqAliasEntry{})

	var got []int
	conn.SetSystemNoticeHandler(func(note *systemNotification, _ reqAliasEntry) {
		got = append(got, note.code)
	})
	if want := []int{2104, 2106}; !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed codes = %v, want %v (buffered notices must replay in arrival order when the handler is wired)", got, want)
	}

	// After wiring, notices dispatch live and are not re-buffered.
	got = nil
	conn.dispatchSystemNotice(farm(2103, "Market data farm connection is broken:usfarm"), reqAliasEntry{})
	if want := []int{2103}; !reflect.DeepEqual(got, want) {
		t.Fatalf("post-wire dispatch = %v, want %v (live dispatch, no buffering)", got, want)
	}

	// A disconnect nils the handler; a notice that arrives during the gap is
	// buffered again and replayed when the next connection re-wires.
	conn.SetSystemNoticeHandler(nil)
	got = nil
	conn.dispatchSystemNotice(farm(2104, "Market data farm connection is OK:usfarm"), reqAliasEntry{})
	conn.SetSystemNoticeHandler(func(note *systemNotification, _ reqAliasEntry) {
		got = append(got, note.code)
	})
	if want := []int{2104}; !reflect.DeepEqual(got, want) {
		t.Fatalf("post-disconnect buffer+rewire = %v, want %v", got, want)
	}
}

// TestReadLoopReplaysFarmNoticeArrivingBeforeHandler is the end-to-end
// regression test for the false-degraded farm status: it drives a real msg-204
// wire frame through processMessage with no handler wired (exactly the
// post-startAPI race on server 203), then wires the connector's
// processSystemNotice the way registerHandlers does, and asserts the connector
// records the farm as ready via replay.
func TestReadLoopReplaysFarmNoticeArrivingBeforeHandler(t *testing.T) {
	t.Parallel()
	c := &Connector{}
	conn := &Connection{
		msgHandlers:   map[int][]handlerEntry{},
		serverVersion: 203,
		config:        &ConnectionConfig{ClientID: 15},
	}

	// tickerID -1 == global-scope notice, matching the gateway's farm burst.
	conn.processMessage(encodeSystemNotificationForTest(-1, 2104, "Market data farm connection is OK:usfarm", ""))
	conn.processMessage(encodeSystemNotificationForTest(-1, 2106, "HMDS data farm connection is OK:ushmds", ""))

	if farms := c.DataFarmStatuses(); len(farms) != 0 {
		t.Fatalf("connector recorded %d farms before handler wiring, want 0: %+v", len(farms), farms)
	}

	// registerHandlers wires the connector; the fix replays the buffered burst.
	conn.SetSystemNoticeHandler(func(note *systemNotification, alias reqAliasEntry) {
		c.processSystemNotice(alias, note)
	})

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
