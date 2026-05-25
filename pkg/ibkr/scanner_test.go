package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A gateway error message tagged with the scanner's reqID must surface as
// a clear, code-bearing error rather than the generic "scanner timed out
// after 8s" the user previously saw. Errors codes 2100-2199 are
// informational warnings (market-data farm state, etc.) and must not
// short-circuit a healthy scan.
func TestRunScannerSubscription_SurfacesGatewayError(t *testing.T) {
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

	// Wait until RunScannerSubscription registers its handlers, grab the
	// reqID it allocated, then inject an error frame tagged with that reqID.
	done := make(chan struct{})
	go func() {
		defer close(done)
		var reqID int
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			conn.handlersMu.RLock()
			errHandlers := conn.msgHandlers[msgErrMsg]
			conn.handlersMu.RUnlock()
			if len(errHandlers) > 0 {
				conn.reqIDMu.Lock()
				reqID = conn.reqIDSeq - 1
				conn.reqIDMu.Unlock()
				break
			}
			time.Sleep(1 * time.Millisecond)
		}
		if reqID == 0 {
			t.Errorf("scanner handlers never registered")
			return
		}
		// First send an informational warning (must be ignored).
		warning := []string{
			strconv.Itoa(msgErrMsg), "2",
			strconv.Itoa(reqID),
			"2104", "Market data farm connection is OK",
		}
		for _, h := range conn.snapshotHandlers(msgErrMsg) {
			h(warning)
		}
		time.Sleep(5 * time.Millisecond)
		// Then a real error tagged with our reqID.
		errFrame := []string{
			strconv.Itoa(msgErrMsg), "2",
			strconv.Itoa(reqID),
			"162", "No market data subscription for this scanner",
		}
		for _, h := range conn.snapshotHandlers(msgErrMsg) {
			h(errFrame)
		}
	}()

	rows, err := c.RunScannerSubscription(context.Background(), ScannerSubscription{
		Type:     "TOP_PERC_GAIN",
		Exchange: "STK.US.MAJOR",
	}, 5*time.Second)
	<-done

	if rows != nil {
		t.Fatalf("expected nil rows on gateway error, got %v", rows)
	}
	if err == nil {
		t.Fatalf("expected error from gateway, got nil")
	}
	if !strings.Contains(err.Error(), "162") {
		t.Fatalf("expected error to mention code 162, got %v", err)
	}
	if !strings.Contains(err.Error(), "No market data subscription") {
		t.Fatalf("expected error to surface gateway message, got %v", err)
	}
}

func TestRequestScannerSubscriptionUsesInstrument(t *testing.T) {
	var out bytes.Buffer
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(&out)
	c := &Connector{conn: conn}

	err := c.requestScannerSubscription(7, ScannerSubscription{
		Type:     "TOP_PERC_GAIN",
		Exchange: "STK.EU.IBIS",
		Limit:    5,
	}, "STOCK.EU")
	if err != nil {
		t.Fatalf("requestScannerSubscription: %v", err)
	}

	payload := out.String()
	for _, want := range []string{"STOCK.EU\x00", "STK.EU.IBIS\x00", "TOP_PERC_GAIN\x00"} {
		if !strings.Contains(payload, want) {
			t.Fatalf("payload missing %q: %q", want, payload)
		}
	}
}

// TestParseScannerData_LiveFixture decodes the captured msgScannerData frame
// recorded against IB Gateway 10.37 (serverVersion 203) on 2026-05-09 to
// nail down field offsets after the [msgID, version, reqID, count, rows...]
// dispatcher contract. The original implementation read fields[1] as the
// row count (which is actually the version field), silently dropping every
// scanner response. Adding this fixture prevents that class of regression.
func TestParseScannerData_LiveFixture(t *testing.T) {
	fields := loadWireFixture(t, "scanner_data_top_movers_20rows.fields")

	// Smoke check that the fixture matches what the dispatcher would deliver.
	if got := fields[0]; got != "20" {
		t.Fatalf("fixture msgID = %q, want 20 (msgScannerData)", got)
	}
	if got := fields[2]; got != "1" {
		t.Fatalf("fixture reqID = %q, want 1", got)
	}
	if got := fields[3]; got != "20" {
		t.Fatalf("fixture numberOfElements = %q, want 20", got)
	}

	rows := parseScannerData(fields)
	if got, want := len(rows), 20; got != want {
		t.Fatalf("parseScannerData rows = %d, want %d", got, want)
	}

	// Spot-check first, middle, and last rows. Values lifted directly from
	// the gateway response — if any of these change you're either looking at
	// a fixture mismatch or a real decoder regression.
	tests := []struct {
		idx          int
		rank         int
		symbol       string
		secType      string
		exchange     string
		currency     string
		marketName   string
		tradingClass string
	}{
		{0, 0, "AEHL", "STK", "SMART", "USD", "SCM", "SCM"},
		{1, 1, "YMAT", "STK", "SMART", "USD", "SCM", "SCM"},
		{4, 4, "RXT", "STK", "SMART", "USD", "NMS", "NMS"},
		{8, 8, "GENVR", "STK", "SMART", "USD", "NMS", "NMS"},
		{19, 19, "ARAY", "STK", "SMART", "USD", "NMS", "NMS"},
	}
	for _, tt := range tests {
		row := rows[tt.idx]
		if row.Rank != tt.rank {
			t.Errorf("rows[%d].Rank = %d, want %d", tt.idx, row.Rank, tt.rank)
		}
		if row.Symbol != tt.symbol {
			t.Errorf("rows[%d].Symbol = %q, want %q", tt.idx, row.Symbol, tt.symbol)
		}
		if row.SecType != tt.secType {
			t.Errorf("rows[%d].SecType = %q, want %q", tt.idx, row.SecType, tt.secType)
		}
		if row.Exchange != tt.exchange {
			t.Errorf("rows[%d].Exchange = %q, want %q", tt.idx, row.Exchange, tt.exchange)
		}
		if row.Currency != tt.currency {
			t.Errorf("rows[%d].Currency = %q, want %q", tt.idx, row.Currency, tt.currency)
		}
		if row.TradingClass != tt.tradingClass {
			t.Errorf("rows[%d].TradingClass = %q, want %q", tt.idx, row.TradingClass, tt.tradingClass)
		}
	}
}

// TestParseScannerData_TooShort guards the early-return paths so a malformed
// frame can't panic the connector.
func TestParseScannerData_TooShort(t *testing.T) {
	cases := [][]string{
		nil,
		{"20"},
		{"20", "3"},
		{"20", "3", "1"},
		{"20", "3", "1", "not-a-number"},
	}
	for i, fields := range cases {
		if got := parseScannerData(fields); got != nil {
			t.Errorf("case %d: parseScannerData(%v) = %v, want nil", i, fields, got)
		}
	}
}

// TestParseScannerData_PartialRow ensures we stop cleanly when the frame
// claims more rows than it actually delivered (defensive against future
// gateway-side truncation).
func TestParseScannerData_PartialRow(t *testing.T) {
	header := []string{"20", "3", "1", "2"} // claims 2 rows
	one := []string{
		"0", "12345", "AAA", "STK", "", "0", "",
		"SMART", "USD", "AAA", "NMS", "NMS",
		"", "", "", "",
	}
	// Provide only 1 row; expect 1 row back, no panic, no garbage row.
	fields := append([]string{}, header...)
	fields = append(fields, one...)
	rows := parseScannerData(fields)
	if len(rows) != 1 {
		t.Fatalf("partial frame parsed %d rows, want 1", len(rows))
	}
	if rows[0].Symbol != "AAA" {
		t.Errorf("rows[0].Symbol = %q, want AAA", rows[0].Symbol)
	}
}

// TestParseScannerData_DispatcherContract documents the [msgID, version,
// reqID, count, ...] layout assumption with a tiny synthetic frame. If the
// underlying dispatcher ever stops including msgID at fields[0], this test
// fails first and tells you to revisit every msgID handler.
func TestParseScannerData_DispatcherContract(t *testing.T) {
	const reqID = 42
	frame := []string{
		strconv.Itoa(msgScannerData), // msgID
		"3",                          // version
		strconv.Itoa(reqID),          // reqID
		"1",                          // count
		"0", "999", "TEST", "STK", "", "0", "",
		"SMART", "USD", "TEST", "NMS", "NMS",
		"", "", "", "",
	}
	rows := parseScannerData(frame)
	if len(rows) != 1 {
		t.Fatalf("synthetic frame parsed %d rows, want 1", len(rows))
	}
	if rows[0].Symbol != "TEST" {
		t.Errorf("Symbol = %q, want TEST", rows[0].Symbol)
	}
}
