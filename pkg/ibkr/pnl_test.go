package ibkr

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"testing"
	"time"
)

// TestParseAccountPnL_FullPayload covers the modern wire shape where the
// gateway emits dailyPnL + unrealizedPnL + realizedPnL. Fields 3 & 4
// (unrealized/realized) are the account's inception-to-now TOTALS, not a
// decomposition of dailyPnL — the fixture deliberately picks values that
// do NOT sum to dailyPnL so nothing bakes in that false invariant.
func TestParseAccountPnL_FullPayload(t *testing.T) {
	fields := []string{"94", "42", "621.30", "-44485.00", "1830.00"}
	reqID, snap, ok := parseAccountPnLFields(fields)
	if !ok {
		t.Fatalf("parseAccountPnLFields ok=false")
	}
	if reqID != 42 {
		t.Errorf("reqID = %d, want 42", reqID)
	}
	if snap.DailyPnL == nil || *snap.DailyPnL != 621.30 {
		t.Errorf("DailyPnL = %v, want 621.30", snap.DailyPnL)
	}
	if snap.UnrealizedTotalPnL == nil || *snap.UnrealizedTotalPnL != -44485.00 {
		t.Errorf("UnrealizedTotalPnL = %v, want -44485.00", snap.UnrealizedTotalPnL)
	}
	if snap.RealizedTotalPnL == nil || *snap.RealizedTotalPnL != 1830.00 {
		t.Errorf("RealizedTotalPnL = %v, want 1830.00", snap.RealizedTotalPnL)
	}
	if snap.AsOf.IsZero() {
		t.Errorf("AsOf should be set")
	}
}

// TestParseAccountPnL_ShortPayload covers older gateway versions that
// emit only the bare dailyPnL field.
func TestParseAccountPnL_ShortPayload(t *testing.T) {
	fields := []string{"94", "7", "100.00"}
	reqID, snap, ok := parseAccountPnLFields(fields)
	if !ok {
		t.Fatalf("ok=false")
	}
	if reqID != 7 {
		t.Errorf("reqID = %d, want 7", reqID)
	}
	if snap.DailyPnL == nil || *snap.DailyPnL != 100.00 {
		t.Errorf("DailyPnL = %v, want 100.00", snap.DailyPnL)
	}
	if snap.UnrealizedTotalPnL != nil {
		t.Errorf("UnrealizedTotalPnL = %v, want nil", snap.UnrealizedTotalPnL)
	}
	if snap.RealizedTotalPnL != nil {
		t.Errorf("RealizedTotalPnL = %v, want nil", snap.RealizedTotalPnL)
	}
}

// TestParseAccountPnL_DBLMAXSentinel asserts the gateway's DBL_MAX
// "not yet computed" sentinel becomes a nil pointer — never a fabricated
// numeric value.
func TestParseAccountPnL_DBLMAXSentinel(t *testing.T) {
	dblMax := strconv.FormatFloat(1.7976931348623157e+308, 'g', -1, 64)
	fields := []string{"94", "1", dblMax, "5.00", dblMax}
	_, snap, ok := parseAccountPnLFields(fields)
	if !ok {
		t.Fatalf("ok=false")
	}
	if snap.DailyPnL != nil {
		t.Errorf("DailyPnL = %v, want nil (DBL_MAX sentinel)", snap.DailyPnL)
	}
	if snap.UnrealizedTotalPnL == nil || *snap.UnrealizedTotalPnL != 5.00 {
		t.Errorf("UnrealizedTotalPnL = %v, want 5.00", snap.UnrealizedTotalPnL)
	}
	if snap.RealizedTotalPnL != nil {
		t.Errorf("RealizedTotalPnL = %v, want nil (DBL_MAX sentinel)", snap.RealizedTotalPnL)
	}
}

// TestParseAccountPnL_Malformed covers the short-frame and bad-reqID
// branches.
func TestParseAccountPnL_Malformed(t *testing.T) {
	t.Run("TooShort", func(t *testing.T) {
		if _, _, ok := parseAccountPnLFields([]string{"94", "1"}); ok {
			t.Errorf("expected ok=false for too-short frame")
		}
	})
	t.Run("BadReqID", func(t *testing.T) {
		if _, _, ok := parseAccountPnLFields([]string{"94", "not-a-number", "100"}); ok {
			t.Errorf("expected ok=false for bad reqID")
		}
	})
}

func TestParsePositionPnL_FullPayload(t *testing.T) {
	// Fields 4 & 5 (unrealized/realized) are the position's inception-to-now
	// TOTALS, not a decomposition of dailyPnL — values chosen so they do not
	// sum to dailyPnL.
	fields := []string{"95", "11", "100", "25.50", "-1440.00", "310.00", "9999.00"}
	reqID, snap, ok := parsePositionPnLFields(fields)
	if !ok {
		t.Fatalf("ok=false")
	}
	if reqID != 11 {
		t.Errorf("reqID = %d, want 11", reqID)
	}
	if snap.DailyPnL == nil || *snap.DailyPnL != 25.50 {
		t.Errorf("DailyPnL = %v, want 25.50", snap.DailyPnL)
	}
	if snap.UnrealizedTotalPnL == nil || *snap.UnrealizedTotalPnL != -1440.00 {
		t.Errorf("UnrealizedTotalPnL = %v", snap.UnrealizedTotalPnL)
	}
	if snap.RealizedTotalPnL == nil || *snap.RealizedTotalPnL != 310.00 {
		t.Errorf("RealizedTotalPnL = %v", snap.RealizedTotalPnL)
	}
}

func TestParsePositionPnL_DBLMAXSentinel(t *testing.T) {
	dblMax := strconv.FormatFloat(1.7976931348623157e+308, 'g', -1, 64)
	fields := []string{"95", "1", "100", dblMax, "1.0", "2.0", "0"}
	_, snap, ok := parsePositionPnLFields(fields)
	if !ok {
		t.Fatalf("ok=false")
	}
	if snap.DailyPnL != nil {
		t.Errorf("DailyPnL = %v, want nil", snap.DailyPnL)
	}
}

func TestParsePnLFloat_EmptyAndNaN(t *testing.T) {
	if v := parsePnLFloat(""); v != nil {
		t.Errorf("empty string → %v, want nil", v)
	}
	if v := parsePnLFloat("nope"); v != nil {
		t.Errorf("garbage → %v, want nil", v)
	}
	if v := parsePnLFloat("NaN"); v != nil {
		t.Errorf("NaN → %v, want nil", v)
	}
	v := parsePnLFloat("0")
	if v == nil || *v != 0 {
		t.Errorf("\"0\" → %v, want 0 (zero is a real value)", v)
	}
}

// TestEncodeMsg_RequestPnL verifies the byte-level wire format for the
// reqPnL message on serverVersion >= 100 (4-byte big-endian msgID,
// null-terminated string fields). Field order matches the TWS API
// reference: [msgID][reqId][account][modelCode]. We test the encoder
// directly — sendMessage's transport plumbing is exercised by tests in
// connection_test.go.
func TestEncodeMsg_RequestPnL(t *testing.T) {
	conn := encoderTestConn()
	got := conn.encodeMsg(reqPnL, 42, "U1234567", "")

	if len(got) < 4 {
		t.Fatalf("payload too short: % x", got)
	}
	if msgID := binary.BigEndian.Uint32(got[:4]); msgID != reqPnL {
		t.Errorf("msgID = %d, want %d", msgID, reqPnL)
	}
	parts := splitNullFields(got[4:])
	wantParts := []string{"42", "U1234567", ""}
	if len(parts) != len(wantParts) {
		t.Fatalf("parts = %d, want %d: %q", len(parts), len(wantParts), parts)
	}
	for i, w := range wantParts {
		if parts[i] != w {
			t.Errorf("part[%d] = %q, want %q", i, parts[i], w)
		}
	}
}

// TestEncodeMsg_RequestPnLSingle verifies the wire format for
// reqPnLSingle: [msgID][reqId][account][modelCode][conId].
func TestEncodeMsg_RequestPnLSingle(t *testing.T) {
	conn := encoderTestConn()
	got := conn.encodeMsg(reqPnLSingle, 7, "U1", "", 265598)

	if msgID := binary.BigEndian.Uint32(got[:4]); msgID != reqPnLSingle {
		t.Errorf("msgID = %d, want %d", msgID, reqPnLSingle)
	}
	parts := splitNullFields(got[4:])
	want := []string{"7", "U1", "", "265598"}
	if len(parts) != len(want) {
		t.Fatalf("parts = %d, want %d: %q", len(parts), len(want), parts)
	}
	for i, w := range want {
		if parts[i] != w {
			t.Errorf("part[%d] = %q, want %q", i, parts[i], w)
		}
	}
}

// TestEncodeMsg_CancelPnL asserts the 2-field [msgID][reqId] cancel
// shape for both account-level and per-position cancels.
func TestEncodeMsg_CancelPnL(t *testing.T) {
	conn := encoderTestConn()
	got := conn.encodeMsg(cancelPnL, 42)
	if msgID := binary.BigEndian.Uint32(got[:4]); msgID != cancelPnL {
		t.Errorf("cancelPnL msgID = %d, want %d", msgID, cancelPnL)
	}
	parts := splitNullFields(got[4:])
	if len(parts) != 1 || parts[0] != "42" {
		t.Errorf("cancelPnL parts = %q, want [\"42\"]", parts)
	}

	got = conn.encodeMsg(cancelPnLSingle, 99)
	if msgID := binary.BigEndian.Uint32(got[:4]); msgID != cancelPnLSingle {
		t.Errorf("cancelPnLSingle msgID = %d, want %d", msgID, cancelPnLSingle)
	}
}

// TestRequestPnL_NotConnected confirms the disconnected guard refuses
// the call without touching the wire.
func TestRequestPnL_NotConnected(t *testing.T) {
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	// Connection defaults to StatusDisconnected.
	if err := conn.RequestPnL(1, "U1", ""); err == nil {
		t.Errorf("expected error on disconnected connection")
	}
}

// TestRequestPnL_EmptyAccount confirms the account-required guard.
func TestRequestPnL_EmptyAccount(t *testing.T) {
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	if err := conn.RequestPnL(1, "", ""); err == nil {
		t.Errorf("expected error on empty account")
	}
}

// TestRequestPnLSingle_BadConID confirms the conId-required guard.
func TestRequestPnLSingle_BadConID(t *testing.T) {
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	if err := conn.RequestPnLSingle(1, "U1", "", 0); err == nil {
		t.Errorf("expected error for conId=0")
	}
}

// TestSubscribeAccountPnL_Disconnected returns ErrIBKRUnavailable
// without issuing wire traffic.
func TestSubscribeAccountPnL_Disconnected(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	err := c.SubscribeAccountPnL("U1")
	if !errors.Is(err, ErrIBKRUnavailable) {
		t.Errorf("err = %v, want ErrIBKRUnavailable", err)
	}
}

// TestSubscribeAccountPnL_EmptyAccount refuses the noop subscribe.
// We use a Connection in StatusConnected so isConnected() passes the
// gate, then verify the account-required guard fires before any wire
// traffic. No transport — the encoder path is never reached on the
// guarded branch.
func TestSubscribeAccountPnL_EmptyAccount(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	c.conn = conn
	c.running = true
	c.ready = true
	if err := c.SubscribeAccountPnL(""); err == nil {
		t.Errorf("expected error on empty account")
	}
}

// TestAccountDailyPnL_BeforeFrame asserts the (snapshot, false) contract
// before any frame has arrived.
func TestAccountDailyPnL_BeforeFrame(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	if _, ok := c.AccountDailyPnL(); ok {
		t.Errorf("expected ok=false before any subscription")
	}
}

// TestHandlePnL_UpdatesCache pushes a synthetic frame through the
// connector handler and checks the cache is updated atomically.
func TestHandlePnL_UpdatesCache(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	// Pretend we subscribed with reqID 42.
	c.pnl.accountReqID = 42
	c.pnl.accountAcct = "U1"

	// Totals (fields 3 & 4) deliberately do not sum to dailyPnL.
	c.handlePnL([]string{"94", "42", "310.00", "-12750.00", "980.00"})

	snap, ok := c.AccountDailyPnL()
	if !ok {
		t.Fatalf("ok=false after handler")
	}
	if snap.DailyPnL == nil || *snap.DailyPnL != 310.00 {
		t.Errorf("DailyPnL = %v", snap.DailyPnL)
	}
}

// TestHandlePnL_StaleReqIDIgnored covers the race where a frame from a
// canceled subscription arrives after we've moved on.
func TestHandlePnL_StaleReqIDIgnored(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.pnl.accountReqID = 100
	c.pnl.accountAcct = "U1"

	// Stale frame from old reqID 99 must be dropped without populating cache.
	c.handlePnL([]string{"94", "99", "310.00", "-12750.00", "980.00"})

	if !c.pnl.account.AsOf.IsZero() {
		t.Errorf("stale frame leaked into cache: AsOf=%v", c.pnl.account.AsOf)
	}
}

// TestHandlePnLSingle_UpdatesPositionCache covers the per-conId handler.
func TestHandlePnLSingle_UpdatesPositionCache(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.pnl.positionReqIDs[265598] = 17
	c.pnl.positionByReqID[17] = 265598

	// Totals (fields 4 & 5) deliberately do not sum to dailyPnL.
	c.handlePnLSingle([]string{"95", "17", "100", "12.50", "-880.00", "140.00", "9999.00"})

	snap, ok := c.PositionDailyPnL(265598)
	if !ok {
		t.Fatalf("PositionDailyPnL(265598) ok=false")
	}
	if snap.DailyPnL == nil || *snap.DailyPnL != 12.50 {
		t.Errorf("DailyPnL = %v, want 12.50", snap.DailyPnL)
	}
}

// TestPositionDailyPnL_UnknownConID returns (zero, false) for never-
// subscribed contracts.
func TestPositionDailyPnL_UnknownConID(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	if _, ok := c.PositionDailyPnL(99999); ok {
		t.Errorf("expected ok=false for unsubscribed conId")
	}
}

// TestSubscribeAccountPnL_NoConnection verifies the guard when the
// connector's internal *Connection is nil. Covers the
// "connector running but Connection rebuild in progress" race.
func TestSubscribeAccountPnL_NoConnection(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.running = true
	c.ready = true
	c.conn = nil
	err := c.SubscribeAccountPnL("U1")
	if !errors.Is(err, ErrIBKRUnavailable) {
		t.Errorf("err = %v, want ErrIBKRUnavailable", err)
	}
}

// TestCancelAllPnL_ClearsState covers the shutdown path: every cached
// reqID is forgotten so the next post-connect subscribe starts clean.
// The Connection is in StatusDisconnected so cancelAllPnL bypasses the
// wire layer — only the state reset is exercised.
func TestCancelAllPnL_ClearsState(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	// Leave status Disconnected: cancelAllPnL's per-reqID CancelPnL /
	// CancelPnLSingle calls are best-effort and return cleanly when the
	// connection is down.
	c.conn = conn

	c.pnl.accountReqID = 99
	c.pnl.accountAcct = "U1"
	c.pnl.positionReqIDs[123] = 17
	c.pnl.positionByReqID[17] = 123
	c.pnl.positionSnapshot[123] = PositionDailyPnL{}

	c.cancelAllPnL()

	if c.pnl.accountReqID != 0 {
		t.Errorf("accountReqID = %d, want 0 after cancelAllPnL", c.pnl.accountReqID)
	}
	if len(c.pnl.positionReqIDs) != 0 {
		t.Errorf("positionReqIDs left = %d, want 0", len(c.pnl.positionReqIDs))
	}
	if len(c.pnl.positionByReqID) != 0 {
		t.Errorf("positionByReqID left = %d, want 0", len(c.pnl.positionByReqID))
	}
	if len(c.pnl.positionSnapshot) != 0 {
		t.Errorf("positionSnapshot left = %d, want 0", len(c.pnl.positionSnapshot))
	}
}

// TestActiveDailyPnLSubscriptions_Counts asserts the counter the daemon
// uses to honor maxDailyPnLSubscriptions.
func TestActiveDailyPnLSubscriptions_Counts(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	if n := c.ActiveDailyPnLSubscriptions(); n != 0 {
		t.Errorf("expected 0 on fresh connector, got %d", n)
	}
	c.pnl.positionReqIDs[111] = 1
	c.pnl.positionReqIDs[222] = 2
	if n := c.ActiveDailyPnLSubscriptions(); n != 2 {
		t.Errorf("expected 2, got %d", n)
	}
}

// --- test helpers ---

// encoderTestConn returns a *Connection wired only for encodeMsg calls:
// serverVersion >= 100 (binary msgID path), no live transport. The
// underlying rate limiter is never started so the test goroutine count
// stays flat.
func encoderTestConn() *Connection {
	c := &Connection{serverVersion: 200}
	return c
}

// splitNullFields splits a payload on the null byte and drops the
// trailing empty element produced by the terminal null. This matches
// the inverse of encodeMsg's per-field "string + \x00" output.
func splitNullFields(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	// encodeMsg adds a trailing null after every field; strip the last
	// empty split before returning.
	parts := bytes.Split(b, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

// Silence unused-import diagnostics when the file evolves; kept so the
// helpers reading time.Time / context.Context downstream don't break
// the test file's import set.
var _ = time.Now
var _ = context.Background
