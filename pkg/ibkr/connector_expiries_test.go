package ibkr

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// TestFetchOptionExpiriesAcrossExchangesDedupesAndSorts drives the connector
// fetch end-to-end against canned frames from two exchanges (SMART, AMEX)
// with overlapping expiries. Asserts the returned slice is in YYYY-MM-DD form,
// deduped, and sorted ascending.
func TestFetchOptionExpiriesAcrossExchangesDedupesAndSorts(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	var out bytes.Buffer
	conn.writer = bufio.NewWriter(&out)
	c.conn = conn
	c.running = true
	c.ready = true
	c.contractCache["AAPL"] = ContractDetailsLite{
		ConID:        265598,
		LocalSymbol:  "AAPL",
		TradingClass: "AAPL",
		Exchange:     "SMART",
		PrimaryExch:  "NASDAQ",
	}

	// Goroutine: poll for the registered handlers (which are keyed by reqID),
	// then deliver canned 75 frames from two exchanges plus the 76 end marker.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Wait until the request has been allocated and the handlers registered.
		var reqID int
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			conn.handlersMu.RLock()
			entries := conn.msgHandlers[msgSecurityDefinitionOptionalParameter]
			conn.handlersMu.RUnlock()
			if len(entries) > 0 {
				// Inspect last allocated reqID (the one our request just claimed).
				conn.reqIDMu.Lock()
				reqID = conn.reqIDSeq - 1
				conn.reqIDMu.Unlock()
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if reqID == 0 {
			t.Errorf("handlers never registered")
			return
		}

		// Frame 1: SMART exchange. Expirations: 20260116, 20260619. Strikes: 200, 210, 220.
		frame1 := []string{
			strconv.Itoa(msgSecurityDefinitionOptionalParameter),
			strconv.Itoa(reqID),
			"SMART", "265598", "AAPL", "100",
			"2", "20260116", "20260619",
			"3", "200", "210", "220",
		}
		// Frame 2: AMEX exchange. Overlap on 20260619, new on 20260918. Strikes: 215, 220.
		frame2 := []string{
			strconv.Itoa(msgSecurityDefinitionOptionalParameter),
			strconv.Itoa(reqID),
			"AMEX", "265598", "AAPL", "100",
			"2", "20260619", "20260918",
			"2", "215", "220",
		}
		// End marker.
		endFrame := []string{
			strconv.Itoa(msgSecurityDefinitionOptionalParameterEnd),
			strconv.Itoa(reqID),
		}

		for _, h := range conn.snapshotHandlers(msgSecurityDefinitionOptionalParameter) {
			h(frame1)
			h(frame2)
		}
		for _, h := range conn.snapshotHandlers(msgSecurityDefinitionOptionalParameterEnd) {
			h(endFrame)
		}
	}()

	expiries, err := c.FetchOptionExpiries("AAPL", time.Second)
	if err != nil {
		t.Fatalf("FetchOptionExpiries: %v", err)
	}
	<-done

	want := []string{"2026-01-16", "2026-06-19", "2026-09-18"}
	if !reflect.DeepEqual(expiries, want) {
		t.Fatalf("expiries mismatch: got %v, want %v", expiries, want)
	}

	// Verify the outbound frame: msgID=78, then reqID, AAPL, "" (futFopExchange),
	// STK, conID. No version field per the IBKR ibapi reference.
	payload := out.Bytes()
	if len(payload) < 4 {
		t.Fatalf("outbound payload too short: %q", payload)
	}
	length := binary.BigEndian.Uint32(payload[:4])
	if int(length) > len(payload)-4 {
		t.Fatalf("invalid payload length %d", length)
	}
	body := payload[4 : 4+length]
	fields := conn.decodeMessage(body)
	// fields[len-1] is the trailing empty string from the null-terminated last field.
	if len(fields) < 6 {
		t.Fatalf("expected at least 6 outbound fields, got %d: %v", len(fields), fields)
	}
	if fields[0] != strconv.Itoa(reqSecDefOptParams) {
		t.Errorf("outbound msgID = %q, want %d", fields[0], reqSecDefOptParams)
	}
	if fields[2] != "AAPL" {
		t.Errorf("outbound underlyingSymbol = %q, want AAPL", fields[2])
	}
	if fields[3] != "" {
		t.Errorf("outbound futFopExchange = %q, want empty string", fields[3])
	}
	if fields[4] != "STK" {
		t.Errorf("outbound underlyingSecType = %q, want STK", fields[4])
	}
	if fields[5] != "265598" {
		t.Errorf("outbound underlyingConId = %q, want 265598", fields[5])
	}
}

// TestFetchOptionExpiryStrikesMergesAcrossExchanges verifies the strikes map
// is keyed by YYYY-MM-DD and that strikes from multiple exchanges for the
// same expiry are merged (not overwritten).
func TestFetchOptionExpiryStrikesMergesAcrossExchanges(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	conn.writer = bufio.NewWriter(&bytes.Buffer{})
	c.conn = conn
	c.running = true
	c.ready = true
	c.contractCache["AAPL"] = ContractDetailsLite{ConID: 265598, LocalSymbol: "AAPL", TradingClass: "AAPL", Exchange: "SMART", PrimaryExch: "NASDAQ"}

	go func() {
		var reqID int
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			conn.handlersMu.RLock()
			n := len(conn.msgHandlers[msgSecurityDefinitionOptionalParameter])
			conn.handlersMu.RUnlock()
			if n > 0 {
				conn.reqIDMu.Lock()
				reqID = conn.reqIDSeq - 1
				conn.reqIDMu.Unlock()
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		// Two frames touching the same expiry with disjoint strike sets.
		smart := []string{
			strconv.Itoa(msgSecurityDefinitionOptionalParameter),
			strconv.Itoa(reqID),
			"SMART", "265598", "AAPL", "100",
			"1", "20260619",
			"3", "200", "210", "220",
		}
		amex := []string{
			strconv.Itoa(msgSecurityDefinitionOptionalParameter),
			strconv.Itoa(reqID),
			"AMEX", "265598", "AAPL", "100",
			"1", "20260619",
			"3", "215", "220", "225",
		}
		end := []string{
			strconv.Itoa(msgSecurityDefinitionOptionalParameterEnd),
			strconv.Itoa(reqID),
		}
		for _, h := range conn.snapshotHandlers(msgSecurityDefinitionOptionalParameter) {
			h(smart)
			h(amex)
		}
		for _, h := range conn.snapshotHandlers(msgSecurityDefinitionOptionalParameterEnd) {
			h(end)
		}
	}()

	strikes, err := c.FetchOptionExpiryStrikes("AAPL", time.Second)
	if err != nil {
		t.Fatalf("FetchOptionExpiryStrikes: %v", err)
	}
	got := strikes["2026-06-19"]
	want := []float64{200, 210, 215, 220, 225}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged strikes for 2026-06-19: got %v, want %v", got, want)
	}
}

// TestFetchOptionExpiriesReturnsErrorOnEmptyTimeout ensures that a fetch
// which sees no msg-75 frames and times out returns an error rather than an
// empty success — daemons need this to surface as gateway_unavailable instead
// of "0 expiries".
func TestFetchOptionExpiriesReturnsErrorOnEmptyTimeout(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	conn.writer = bufio.NewWriter(&bytes.Buffer{})
	c.conn = conn
	c.running = true
	c.ready = true
	c.contractCache["AAPL"] = ContractDetailsLite{ConID: 265598, LocalSymbol: "AAPL", TradingClass: "AAPL", Exchange: "SMART", PrimaryExch: "NASDAQ"}

	start := time.Now()
	_, err := c.FetchOptionExpiries("AAPL", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error when no frames arrive")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("expected fast timeout, took %v", elapsed)
	}
}

// TestFetchOptionExpiriesReturnsPartialOnLateTimeout verifies the partial-data
// contract: if at least one msg-75 frame arrived but the end marker (76) does
// not, callers still get the observed expiries on timeout. The CLI-side
// rendering shows the listing rather than an error in this branch.
func TestFetchOptionExpiriesReturnsPartialOnLateTimeout(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	conn.writer = bufio.NewWriter(&bytes.Buffer{})
	c.conn = conn
	c.running = true
	c.ready = true
	c.contractCache["AAPL"] = ContractDetailsLite{ConID: 265598, LocalSymbol: "AAPL", TradingClass: "AAPL", Exchange: "SMART", PrimaryExch: "NASDAQ"}

	go func() {
		var reqID int
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			conn.handlersMu.RLock()
			n := len(conn.msgHandlers[msgSecurityDefinitionOptionalParameter])
			conn.handlersMu.RUnlock()
			if n > 0 {
				conn.reqIDMu.Lock()
				reqID = conn.reqIDSeq - 1
				conn.reqIDMu.Unlock()
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		frame := []string{
			strconv.Itoa(msgSecurityDefinitionOptionalParameter),
			strconv.Itoa(reqID),
			"SMART", "265598", "AAPL", "100",
			"1", "20260116",
			"1", "200",
		}
		for _, h := range conn.snapshotHandlers(msgSecurityDefinitionOptionalParameter) {
			h(frame)
		}
		// No end marker — let the timeout fire.
	}()

	expiries, err := c.FetchOptionExpiries("AAPL", 150*time.Millisecond)
	if err != nil {
		t.Fatalf("expected partial result, got error %v", err)
	}
	want := []string{"2026-01-16"}
	if !reflect.DeepEqual(expiries, want) {
		t.Fatalf("partial expiries: got %v, want %v", expiries, want)
	}
}

// TestNormaliseExpiry8 covers the small parser used by snapshot().
func TestNormaliseExpiry8(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"20260619", "2026-06-19", true},
		{"20260116", "2026-01-16", true},
		{"2026-06-19", "", false},
		{"", "", false},
		{"abcdefgh", "", false},
		{"2026061", "", false},
	}
	for _, tc := range cases {
		got, ok := normaliseExpiry8(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("normaliseExpiry8(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
