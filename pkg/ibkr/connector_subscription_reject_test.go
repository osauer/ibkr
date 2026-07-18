package ibkr

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"testing"
	"time"
)

// fastAbortBudget is the time within which a poll consumer must observe
// the rejection signal. The connection-layer error handler dispatches to
// the connector synchronously, so end-to-end push-to-receive is bounded
// by goroutine scheduling — typically sub-millisecond. A 100 ms ceiling
// gives ample headroom on a busy CI box while still failing loudly if
// the dispatch path silently regresses to the 5 s pre-fix budget.
const fastAbortBudget = 100 * time.Millisecond

// setupOptionSubscriptionFixture builds a Connector + Connection that
// believes it is talking to a live gateway, pre-seeds the option
// contract cache so SubscribeOption's resolveOptionContract round trip
// short-circuits to the in-process cache, and returns the subscription
// key and reqID after a successful subscribe. The outbound reqMktData
// frame is buffered in `out` — tests don't need to decode it.
func setupOptionSubscriptionFixture(t *testing.T) (c *Connector, conn *Connection, subKey string, reqID int) {
	c, conn, subKey, reqID, _ = setupOptionSubscriptionFixtureWithOutput(t)
	return c, conn, subKey, reqID
}

func setupOptionSubscriptionFixtureWithOutput(t *testing.T) (c *Connector, conn *Connection, subKey string, reqID int, out *safeBuffer) {
	t.Helper()
	c = NewConnector(&ConnectorConfig{})
	conn = NewConnection(nil)
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	out = &safeBuffer{}
	conn.writer = bufio.NewWriter(out)
	c.conn = conn
	c.running = true
	c.ready = true

	contract := Contract{
		Symbol:       "SPY",
		SecType:      "OPT",
		Exchange:     "SMART",
		Currency:     "USD",
		Expiry:       "20250620",
		Strike:       500,
		Right:        "C",
		Multiplier:   100,
		TradingClass: "SPY", // matches the SubscribeOption call below so the v3 cache lookup hits
	}
	cacheKey := optionContractKey(contract.Symbol, contract.TradingClass, contract.Expiry, contract.Strike, contract.Right)
	conn.optionContractMu.Lock()
	conn.optionContractCache[cacheKey] = ContractDetailsLite{
		ConID:        99999,
		Symbol:       "SPY",
		Exchange:     "SMART",
		PrimaryExch:  "CBOE",
		LocalSymbol:  "SPY   250620C00500000",
		TradingClass: "SPY",
	}
	conn.optionContractMu.Unlock()

	var err error
	subKey, reqID, err = c.SubscribeOption(context.Background(), "SPY", "SPY", "20250620", 500, "C")
	if err != nil {
		t.Fatalf("SubscribeOption: %v", err)
	}
	if reqID == 0 {
		t.Fatalf("expected non-zero reqID")
	}
	return c, conn, subKey, reqID, out
}

func decodeLastWireMessageFields(t *testing.T, conn *Connection, payload []byte) []string {
	t.Helper()
	offset := 0
	var msg []byte
	for offset+4 <= len(payload) {
		length := int(binary.BigEndian.Uint32(payload[offset : offset+4]))
		start := offset + 4
		end := start + length
		if end > len(payload) {
			break
		}
		msg = append([]byte(nil), payload[start:end]...)
		offset = end
	}
	if len(msg) == 0 {
		t.Fatalf("failed to decode last wire message from payload length %d", len(payload))
	}
	return conn.decodeMessage(msg)
}

type immediateMktDataTickWriter struct {
	conn  *Connection
	onReq func(reqID int)
	buf   []byte
	fired bool
}

func (w *immediateMktDataTickWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for !w.fired && len(w.buf) >= 4 {
		n := int(binary.BigEndian.Uint32(w.buf[:4]))
		if len(w.buf) < 4+n {
			break
		}
		msg := w.buf[4 : 4+n]
		fields := w.conn.decodeMessage(msg)
		w.buf = w.buf[4+n:]
		if len(fields) < 3 || fields[0] != strconv.Itoa(reqMktData) {
			continue
		}
		reqID, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		w.fired = true
		if w.onReq != nil {
			w.onReq(reqID)
		}
	}
	return len(p), nil
}

func TestSubscribeOptionCapturesImmediateOpenInterestTick(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	c.conn = conn
	c.running = true
	c.ready = true

	writer := &immediateMktDataTickWriter{conn: conn}
	writer.onReq = func(reqID int) {
		c.handleTickSize([]string{"2", "6", strconv.Itoa(reqID), "27", "4321"})
		c.handleTickSize([]string{"2", "6", strconv.Itoa(reqID), "28", "0"})
	}
	conn.writer = bufio.NewWriter(writer)

	contract := Contract{
		Symbol:       "SPY",
		SecType:      "OPT",
		Exchange:     "SMART",
		Currency:     "USD",
		Expiry:       "20250620",
		Strike:       500,
		Right:        "C",
		Multiplier:   100,
		TradingClass: "SPY",
	}
	cacheKey := optionContractKey(contract.Symbol, contract.TradingClass, contract.Expiry, contract.Strike, contract.Right)
	conn.optionContractMu.Lock()
	conn.optionContractCache[cacheKey] = ContractDetailsLite{
		ConID:        99999,
		Symbol:       "SPY",
		Exchange:     "SMART",
		PrimaryExch:  "CBOE",
		LocalSymbol:  "SPY   250620C00500000",
		TradingClass: "SPY",
	}
	conn.optionContractMu.Unlock()

	subKey, reqID, err := c.SubscribeOption(context.Background(), "SPY", "SPY", "20250620", 500, "C")
	if err != nil {
		t.Fatalf("SubscribeOption: %v", err)
	}
	if reqID == 0 {
		t.Fatalf("expected non-zero reqID")
	}
	if !writer.fired {
		t.Fatalf("test writer did not observe outbound reqMktData")
	}

	md := c.MarketDataSnapshot()
	if md[subKey] == nil {
		t.Fatalf("MarketDataSnapshot missing entry for %q", subKey)
	}
	if md[subKey].OpenInt != 4321 {
		t.Fatalf("OpenInt = %d, want 4321", md[subKey].OpenInt)
	}
	if !md[subKey].OpenIntObserved {
		t.Fatalf("OpenIntObserved = false, want true")
	}

	c.subMu.RLock()
	storedRight := c.subscriptions[subKey].Right
	c.subMu.RUnlock()
	if storedRight != "C" {
		t.Fatalf("stored subscription Right = %q, want C", storedRight)
	}
}

func TestSubscribeOptionRequestsSPYTradingClassBlankPrimaryAndOpenInterestGenericTick(t *testing.T) {
	_, conn, _, _, out := setupOptionSubscriptionFixtureWithOutput(t)
	fields := decodeLastWireMessageFields(t, conn, out.Bytes())
	if len(fields) < 17 {
		t.Fatalf("unexpected reqMktData fields: %#v", fields)
	}
	if fields[0] != strconv.Itoa(reqMktData) || fields[1] != "11" {
		t.Fatalf("expected reqMktData v11 header, got %#v", fields[:2])
	}
	if fields[3] != "99999" || fields[4] != "SPY" || fields[5] != "OPT" {
		t.Fatalf("unexpected option route fields: %#v", fields)
	}
	if fields[10] != "SMART" || fields[11] != "" || fields[12] != "USD" || fields[14] != "SPY" {
		t.Fatalf("unexpected SPY option contract fields: exchange=%q primary=%q currency=%q tradingClass=%q fields=%#v",
			fields[10], fields[11], fields[12], fields[14], fields)
	}
	if fields[16] != OptionSubscriptionGenericTicks {
		t.Fatalf("generic ticks = %q, want %q (must include open-interest tick %s)",
			fields[16], OptionSubscriptionGenericTicks, OptionOpenInterestGenericTick)
	}
}

type failingMktDataWriter struct{}

func (failingMktDataWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("synthetic write failure")
}

func TestSubscribeOptionCleansPreparedStateOnSendFailure(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(failingMktDataWriter{})
	c.conn = conn
	c.running = true
	c.ready = true

	contract := Contract{
		Symbol:       "SPY",
		SecType:      "OPT",
		Exchange:     "SMART",
		Currency:     "USD",
		Expiry:       "20250620",
		Strike:       500,
		Right:        "C",
		Multiplier:   100,
		TradingClass: "SPY",
	}
	cacheKey := optionContractKey(contract.Symbol, contract.TradingClass, contract.Expiry, contract.Strike, contract.Right)
	conn.optionContractMu.Lock()
	conn.optionContractCache[cacheKey] = ContractDetailsLite{
		ConID:        99999,
		Symbol:       "SPY",
		Exchange:     "SMART",
		PrimaryExch:  "CBOE",
		LocalSymbol:  "SPY   250620C00500000",
		TradingClass: "SPY",
	}
	conn.optionContractMu.Unlock()

	key := optionMarketDataKeyForClass("SPY", "SPY", "20250620", "C", 500)
	if _, _, err := c.SubscribeOption(context.Background(), "SPY", "SPY", "20250620", 500, "C"); err == nil {
		t.Fatalf("SubscribeOption succeeded with failing writer")
	}

	c.subMu.RLock()
	_, subExists := c.subscriptions[key]
	var reqMapped bool
	for _, mapped := range c.reqIDMap {
		if mapped == key {
			reqMapped = true
		}
	}
	c.subMu.RUnlock()
	if subExists || reqMapped {
		t.Fatalf("prepared subscription state survived send failure: sub=%v reqMapped=%v", subExists, reqMapped)
	}

	c.optMu.RLock()
	var optMapped bool
	for _, mapped := range c.optReqIDs {
		if mapped == key {
			optMapped = true
		}
	}
	c.optMu.RUnlock()
	if optMapped {
		t.Fatalf("prepared option routing state survived send failure")
	}

	conn.marketDataSlotsMu.Lock()
	slots := len(conn.marketDataSlots)
	conn.marketDataSlotsMu.Unlock()
	if slots != 0 {
		t.Fatalf("market-data slot leaked after send failure: %d", slots)
	}
}

// TestSubscriptionRejection_FastAbortOnCode200 ensures that an
// in-flight per-leg poller selecting on the subscription's reject
// channel exits within fastAbortBudget when the gateway returns
// code 200 for the reqID. Pre-fix, the connection-layer handler logged
// a warning and released the rate-limiter slot but never signalled the
// in-flight subscription, so the gamma compute's 5 s OI poll burned
// the full budget for every rejected leg.
func TestSubscriptionRejection_FastAbortOnCode200(t *testing.T) {
	c, _, subKey, reqID := setupOptionSubscriptionFixture(t)

	rejectCh := c.SubscriptionRejectCh(subKey)
	if rejectCh == nil {
		t.Fatalf("SubscriptionRejectCh returned nil; SubscribeOption must initialise RejectCh")
	}

	// Model the per-leg poll's select: rejection vs. the 5 s budget the
	// production fetcher uses. With the fix, the rejection branch fires
	// the moment handleIBKRError pushes.
	const pollBudget = 5 * time.Second
	got := make(chan SubscriptionRejection, 1)
	timedOut := make(chan struct{})
	start := time.Now()
	go func() {
		select {
		case rej := <-rejectCh:
			got <- rej
		case <-time.After(pollBudget):
			close(timedOut)
		}
	}()

	c.handleIBKRError([]string{
		strconv.Itoa(msgErrMsg), "2",
		strconv.Itoa(reqID),
		"200", "No security definition has been found for the request",
	})

	select {
	case rej := <-got:
		elapsed := time.Since(start)
		if elapsed > fastAbortBudget {
			t.Fatalf("fast-abort took %s; ceiling is %s — connection-layer dispatch path regressed", elapsed, fastAbortBudget)
		}
		if rej.Code != 200 {
			t.Fatalf("expected rejection code 200, got %d", rej.Code)
		}
		if rej.Message == "" {
			t.Fatalf("rejection must carry the gateway message")
		}
	case <-timedOut:
		t.Fatalf("poll ran out the full %s budget; fast-abort did not fire", pollBudget)
	}
}

// TestSubscriptionRejection_AllTerminalCodes is the table-driven
// counterpart that exercises every error code documented as "the
// subscription will never produce ticks": 200 (no security def),
// 320/321/322 (server-side parse/validate/duplicate), 354 (entitlement
// gap), and 10197 (competing live session). Each must produce a
// rejection on the per-subscription channel within fastAbortBudget.
func TestSubscriptionRejection_AllTerminalCodes(t *testing.T) {
	terminals := []struct {
		code int
		msg  string
	}{
		{200, "No security definition has been found for the request"},
		{320, "Server error when reading request"},
		{321, "Server error when validating request"},
		{322, "Error processing request - Duplicate ticker ID"},
		{354, "Requested market data is not subscribed"},
		{10197, "Competing live session"},
	}
	for _, tc := range terminals {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			c, _, subKey, reqID := setupOptionSubscriptionFixture(t)
			rejectCh := c.SubscriptionRejectCh(subKey)
			if rejectCh == nil {
				t.Fatalf("RejectCh missing")
			}
			c.handleIBKRError([]string{
				strconv.Itoa(msgErrMsg), "2",
				strconv.Itoa(reqID),
				strconv.Itoa(tc.code), tc.msg,
			})
			select {
			case rej := <-rejectCh:
				if rej.Code != tc.code {
					t.Fatalf("expected code %d, got %d", tc.code, rej.Code)
				}
				if rej.Message != tc.msg {
					t.Fatalf("expected message %q, got %q", tc.msg, rej.Message)
				}
			case <-time.After(fastAbortBudget):
				t.Fatalf("code %d did not produce a rejection within %s", tc.code, fastAbortBudget)
			}
		})
	}
}

// TestSubscriptionRejection_NonTerminalCodeIsSilent guards the
// classifier: codes that don't terminate the subscription
// (informational, transient warnings) must NOT poison the rejection
// channel. If they did, a later legitimate timeout could be
// misattributed as a gateway rejection.
func TestSubscriptionRejection_NonTerminalCodeIsSilent(t *testing.T) {
	nonTerminals := []int{
		162,  // historical data pacing — handled by histPending path
		366,  // no historical data found
		2104, // farm connected (informational)
		2119, // farm OK (informational)
	}
	for _, code := range nonTerminals {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			c, _, subKey, reqID := setupOptionSubscriptionFixture(t)
			rejectCh := c.SubscriptionRejectCh(subKey)
			c.handleIBKRError([]string{
				strconv.Itoa(msgErrMsg), "2",
				strconv.Itoa(reqID),
				strconv.Itoa(code), "informational",
			})
			select {
			case rej := <-rejectCh:
				t.Fatalf("non-terminal code %d incorrectly pushed rejection %+v", code, rej)
			case <-time.After(fastAbortBudget):
				// expected: nothing on the channel
			}
		})
	}
}

// TestSubscriptionRejection_BufferFullDoesNotBlock guards the
// non-blocking semantics of the producer: if a previous rejection is
// still queued on the buffered channel (consumer hasn't drained yet),
// a second error frame must not stall the error-handler goroutine.
// The drop is benign — every terminal code means the same thing to
// the poller — but the producer hanging would cascade into every
// other gateway message getting delayed.
func TestSubscriptionRejection_BufferFullDoesNotBlock(t *testing.T) {
	c, _, _, reqID := setupOptionSubscriptionFixture(t)

	// First rejection fills the buffer (buffer size 1).
	c.handleIBKRError([]string{
		strconv.Itoa(msgErrMsg), "2",
		strconv.Itoa(reqID),
		"200", "first rejection",
	})

	// Second rejection must drop, not block. Run with a tight deadline
	// to catch a regression to blocking-send (which would deadlock with
	// the test goroutine).
	done := make(chan struct{})
	go func() {
		c.handleIBKRError([]string{
			strconv.Itoa(msgErrMsg), "2",
			strconv.Itoa(reqID),
			"354", "second rejection",
		})
		close(done)
	}()

	select {
	case <-done:
		// expected: completed promptly
	case <-time.After(fastAbortBudget):
		t.Fatalf("second handleIBKRError blocked; producer must drop on full buffer")
	}
}
