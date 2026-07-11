package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHandleHistoricalDataParsesBars(t *testing.T) {
	tests := []struct {
		name          string
		serverVersion int
		fields        []string
		wantStart     string
		wantEnd       string
	}{
		{
			name:          "legacy_format",
			serverVersion: 150,
			fields: []string{
				strconv.Itoa(msgHistoricalData),
				"101",
				"20240515 16:00:00",
				"20240516 16:00:00",
				"2",
				"20240515",
				"500.10",
				"505.20",
				"495.80",
				"503.55",
				"1200000",
				"501.10",
				"780",
				"20240516",
				"503.55",
				"507.10",
				"499.90",
				"506.80",
				"1250000",
				"504.25",
				"800",
			},
			wantStart: "20240515 16:00:00",
			wantEnd:   "20240516 16:00:00",
		},
		{
			name:          "modern_format",
			serverVersion: 203,
			fields: []string{
				strconv.Itoa(msgHistoricalData),
				"202",
				"2",
				"20240515",
				"500.10",
				"505.20",
				"495.80",
				"503.55",
				"1200000",
				"501.10",
				"780",
				"20240516",
				"503.55",
				"507.10",
				"499.90",
				"506.80",
				"1250000",
				"504.25",
				"800",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewConnector(&ConnectorConfig{})
			conn := NewConnection(nil)
			defer conn.rateLimiter.Stop()
			conn.status = StatusConnected
			setServerVersionReady(conn, tt.serverVersion)
			c.conn = conn
			c.running = true
			c.ready = true

			reqID := 101
			if tt.serverVersion >= minServerVerHistoricalDataEnd {
				reqID = 202
			}
			req := c.createHistoricalRequest(reqID, "SPY")

			c.handleHistoricalData(tt.fields)

			res := <-req.result
			if res.err != nil {
				t.Fatalf("unexpected error: %v", res.err)
			}
			if len(res.bars) != 2 {
				t.Fatalf("expected 2 bars, got %d", len(res.bars))
			}
			if res.bars[0].Open != 500.10 || res.bars[1].Close != 506.80 {
				t.Fatalf("unexpected bar values: %+v", res.bars)
			}
			if res.bars[0].Time.IsZero() {
				t.Fatalf("expected parsed time for first bar")
			}
			if tt.wantStart != "" && res.start != tt.wantStart {
				t.Fatalf("expected start %q, got %q", tt.wantStart, res.start)
			}
			if tt.wantEnd != "" && res.end != tt.wantEnd {
				t.Fatalf("expected end %q, got %q", tt.wantEnd, res.end)
			}
		})
	}
}

func TestHandleHistoricalDataEndCompletesEmptyResult(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	c.conn = conn
	c.running = true
	c.ready = true

	req := c.createHistoricalRequest(11, "DXY")

	fields := []string{
		strconv.Itoa(msgHistoricalDataEnd),
		"6",
		"11",
		"20250927 07:55:37 US/Eastern",
		"20250928 07:55:37 US/Eastern",
	}

	c.handleHistoricalDataEnd(fields)

	res := <-req.result
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if len(res.bars) != 0 {
		t.Fatalf("expected zero bars, got %d", len(res.bars))
	}
	if res.start != "20250927 07:55:37 US/Eastern" || res.end != "20250928 07:55:37 US/Eastern" {
		t.Fatalf("unexpected start/end: %+v", res)
	}
}

func TestHandleIBKRErrorFailsHistoricalRequest(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	c.conn = conn
	c.running = true
	c.ready = true

	req := c.createHistoricalRequest(202, "QQQ")

	c.handleIBKRError([]string{"4", "2", "202", "162", "Historical data request error"})

	res := <-req.result
	if res.err == nil {
		t.Fatalf("expected error result")
	}
	hErr, ok := res.err.(*HistoricalRequestError)
	if !ok {
		t.Fatalf("expected HistoricalRequestError, got %T", res.err)
	}
	if hErr.Code != 162 {
		t.Fatalf("expected code 162, got %d", hErr.Code)
	}
	if hErr.RetryAfter != 30*time.Second {
		t.Fatalf("expected base retry of 30s, got %v", hErr.RetryAfter)
	}
	if hErr.Message == "" {
		t.Fatalf("expected error message for pacing violation")
	}
}

func TestHandleIBKRErrorHistoricalDurationValidation(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	c.conn = conn
	c.running = true
	c.ready = true

	req := c.createHistoricalRequest(303, "IWM")

	c.handleIBKRError([]string{"4", "2", "303", "321", "Invalid duration"})

	res := <-req.result
	if res.err == nil {
		t.Fatalf("expected error result")
	}
	hErr, ok := res.err.(*HistoricalRequestError)
	if !ok {
		t.Fatalf("expected HistoricalRequestError, got %T", res.err)
	}
	if hErr.Code != 321 {
		t.Fatalf("expected code 321, got %d", hErr.Code)
	}
	if hErr.RetryAfter != 0 {
		t.Fatalf("expected no retry hint for validation error, got %v", hErr.RetryAfter)
	}
	if !strings.Contains(strings.ToLower(hErr.Error()), "invalid") {
		t.Fatalf("unexpected error message: %q", hErr.Error())
	}
}

func TestHistoricalBackoffExponentialAndReset(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	c.conn = conn
	c.running = true
	c.ready = true

	req := c.createHistoricalRequest(400, "SPY")
	c.handleIBKRError([]string{"4", "2", "400", "162", "Historical data request error"})
	res := <-req.result
	hErr, ok := res.err.(*HistoricalRequestError)
	if !ok {
		t.Fatalf("expected HistoricalRequestError, got %T", res.err)
	}
	firstDelay := hErr.RetryAfter
	if firstDelay != 30*time.Second {
		t.Fatalf("expected first backoff 30s, got %v", firstDelay)
	}

	req2 := c.createHistoricalRequest(401, "SPY")
	c.handleIBKRError([]string{"4", "2", "401", "162", "Historical data request error"})
	res2 := <-req2.result
	hErr2, ok := res2.err.(*HistoricalRequestError)
	if !ok {
		t.Fatalf("expected HistoricalRequestError, got %T", res2.err)
	}
	if hErr2.RetryAfter <= firstDelay {
		t.Fatalf("expected increased backoff, got first=%v second=%v", firstDelay, hErr2.RetryAfter)
	}

	reqSuccess := c.createHistoricalRequest(402, "SPY")
	c.completeHistoricalRequest(402, historicalResult{bars: []HistoricalBar{{Close: 10}}})
	<-reqSuccess.result

	req3 := c.createHistoricalRequest(403, "SPY")
	c.handleIBKRError([]string{"4", "2", "403", "162", "Historical data request error"})
	res3 := <-req3.result
	hErr3, ok := res3.err.(*HistoricalRequestError)
	if !ok {
		t.Fatalf("expected HistoricalRequestError, got %T", res3.err)
	}
	if hErr3.RetryAfter != 30*time.Second {
		t.Fatalf("expected backoff reset to 30s after success, got %v", hErr3.RetryAfter)
	}
}

func TestFetchHistoricalDailyBarsReturnsData(t *testing.T) {
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
	c.contractCache["SPY"] = ContractDetailsLite{
		ConID:        756733,
		LocalSymbol:  "SPY",
		TradingClass: "SPY",
		Exchange:     "SMART",
		PrimaryExch:  "ARCA",
	}
	c.contractCache["VIX"] = ContractDetailsLite{
		ConID:        13455763,
		LocalSymbol:  "VIX",
		TradingClass: "IND",
		Exchange:     "CBOE",
		PrimaryExch:  "CBOE",
	}

	done := make(chan struct{})
	go func() {
		deadline := time.Now().Add(500 * time.Millisecond)
		var reqID int
		for time.Now().Before(deadline) {
			c.historicalMu.Lock()
			for id := range c.historicalReqs {
				reqID = id
				break
			}
			c.historicalMu.Unlock()
			if reqID != 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if reqID != 0 {
			fields := []string{
				strconv.Itoa(msgHistoricalData),
				strconv.Itoa(reqID),
				"1",
				"20240520",
				"430.21",
				"432.00",
				"428.50",
				"431.75",
				"1500000",
				"430.85",
				"900",
			}
			c.handleHistoricalData(fields)
		}
		close(done)
	}()

	bars, err := c.FetchHistoricalDailyBars(context.Background(), "SPY", 10, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("expected 1 bar, got %d", len(bars))
	}
	if bars[0].Close != 431.75 {
		t.Fatalf("unexpected close price: %+v", bars[0])
	}
	<-done
}

func TestFetchHistoricalDailyBarsUsesSmartExchange(t *testing.T) {
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
	c.contractCache["GLD"] = ContractDetailsLite{
		ConID:        1234567,
		LocalSymbol:  "GLD",
		TradingClass: "GLD",
		Exchange:     "SMART",
		PrimaryExch:  "ARCA",
	}

	done := make(chan struct{})
	go func() {
		deadline := time.Now().Add(500 * time.Millisecond)
		var reqID int
		for time.Now().Before(deadline) {
			c.historicalMu.Lock()
			for id := range c.historicalReqs {
				reqID = id
				break
			}
			c.historicalMu.Unlock()
			if reqID != 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if reqID != 0 {
			fields := []string{
				strconv.Itoa(msgHistoricalData),
				strconv.Itoa(reqID),
				"1",
				"20240520",
				"215.11",
				"216.00",
				"214.50",
				"215.75",
				"1200000",
				"215.40",
				"900",
			}
			c.handleHistoricalData(fields)
		}
		close(done)
	}()

	if _, err := c.FetchHistoricalDailyBars(context.Background(), "GLD", 10, time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	<-done

	payload := out.Bytes()
	if !bytes.Contains(payload, []byte("SMART")) {
		t.Fatalf("expected historical request to include SMART exchange, payload=%q", payload)
	}
	if bytes.Contains(payload, []byte("ADJUSTED_LAST")) {
		t.Fatalf("did not expect ADJUSTED_LAST fallback when SMART/TRADES succeeded, payload=%q", payload)
	}
	if bytes.Contains(payload, []byte("MIDPOINT")) {
		t.Fatalf("did not expect MIDPOINT fallback when SMART/TRADES succeeded, payload=%q", payload)
	}
	if bytes.Count(payload, []byte("TRADES")) != 1 {
		t.Fatalf("expected single TRADES request, payload=%q", payload)
	}
}

func TestFetchHistoricalDailyBarsWhatToShowForcesAdjustedLast(t *testing.T) {
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
	c.contractCache["GLD"] = ContractDetailsLite{
		ConID:        1234567,
		LocalSymbol:  "GLD",
		TradingClass: "GLD",
		Exchange:     "SMART",
		PrimaryExch:  "ARCA",
	}

	done := make(chan struct{})
	go func() {
		deadline := time.Now().Add(500 * time.Millisecond)
		var reqID int
		for time.Now().Before(deadline) {
			c.historicalMu.Lock()
			for id := range c.historicalReqs {
				reqID = id
				break
			}
			c.historicalMu.Unlock()
			if reqID != 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if reqID != 0 {
			fields := []string{
				strconv.Itoa(msgHistoricalData),
				strconv.Itoa(reqID),
				"1",
				"20240520",
				"215.11",
				"216.00",
				"214.50",
				"215.75",
				"1200000",
				"215.40",
				"900",
			}
			c.handleHistoricalData(fields)
		}
		close(done)
	}()

	if _, err := c.FetchHistoricalDailyBarsWhatToShow(context.Background(), "GLD", 10, "ADJUSTED_LAST", time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	<-done

	payload := out.Bytes()
	if !bytes.Contains(payload, []byte("ADJUSTED_LAST")) {
		t.Fatalf("expected ADJUSTED_LAST request, payload=%q", payload)
	}
	if bytes.Contains(payload, []byte("TRADES")) || bytes.Contains(payload, []byte("MIDPOINT")) {
		t.Fatalf("explicit whatToShow should not fallback through TRADES/MIDPOINT, payload=%q", payload)
	}
}

func TestFetchHistoricalDailyBarsWithContractUsesExplicitRoute(t *testing.T) {
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
	c.contractCache["MBG"] = ContractDetailsLite{
		ConID:        1234568,
		Symbol:       "MBG",
		LocalSymbol:  "MBG",
		TradingClass: "MBG",
		Exchange:     "SMART",
		PrimaryExch:  "IBIS",
	}

	done := make(chan struct{})
	go func() {
		deadline := time.Now().Add(500 * time.Millisecond)
		var reqID int
		for time.Now().Before(deadline) {
			c.historicalMu.Lock()
			for id := range c.historicalReqs {
				reqID = id
				break
			}
			c.historicalMu.Unlock()
			if reqID != 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if reqID != 0 {
			fields := []string{
				strconv.Itoa(msgHistoricalData),
				strconv.Itoa(reqID),
				"1",
				"20260525",
				"50.50",
				"51.15",
				"50.23",
				"50.81",
				"3000000",
				"50.80",
				"900",
			}
			c.handleHistoricalData(fields)
		}
		close(done)
	}()

	contract := Contract{Symbol: "MBG", SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR"}
	if _, err := c.FetchHistoricalDailyBarsWithContract(context.Background(), contract, 10, time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	<-done

	payload := out.Bytes()
	for _, want := range [][]byte{[]byte("MBG"), []byte("SMART"), []byte("IBIS"), []byte("EUR"), []byte("TRADES")} {
		if !bytes.Contains(payload, want) {
			t.Fatalf("expected historical request to include %q, payload=%q", want, payload)
		}
	}
}

func TestFetchHistoricalDailyBarsWithContractResolvesExplicitRoute(t *testing.T) {
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
	c.contractCache["MBG"] = ContractDetailsLite{
		ConID:       999,
		Symbol:      "MBG",
		Exchange:    "NYSE",
		PrimaryExch: "NYSE",
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		var detailReqID int
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			conn.handlersMu.RLock()
			registered := len(conn.msgHandlers[msgContractData]) > 0
			conn.handlersMu.RUnlock()
			if registered {
				conn.reqIDMu.Lock()
				detailReqID = conn.reqIDSeq - 1
				conn.reqIDMu.Unlock()
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if detailReqID == 0 {
			t.Errorf("contract data handlers never registered")
			return
		}

		frame := make([]string, 29)
		frame[0] = strconv.Itoa(msgContractData)
		frame[1] = strconv.Itoa(detailReqID)
		frame[2] = "MBG"
		frame[3] = "STK"
		frame[8] = "IBIS"
		frame[9] = "EUR"
		frame[10] = "MBG"
		frame[12] = "MBG"
		frame[13] = "1357911"
		frame[21] = "IBIS"
		for _, h := range conn.snapshotHandlers(msgContractData) {
			h(frame)
		}
		time.Sleep(20 * time.Millisecond)
		endFrame := []string{
			strconv.Itoa(msgContractDataEnd),
			"1",
			strconv.Itoa(detailReqID),
		}
		for _, h := range conn.snapshotHandlers(msgContractDataEnd) {
			h(endFrame)
		}

		var histReqID int
		deadline = time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			c.historicalMu.Lock()
			for id := range c.historicalReqs {
				histReqID = id
				break
			}
			c.historicalMu.Unlock()
			if histReqID != 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if histReqID == 0 {
			t.Errorf("historical request was not sent")
			return
		}
		fields := []string{
			strconv.Itoa(msgHistoricalData),
			strconv.Itoa(histReqID),
			"1",
			"20260525",
			"50.50",
			"51.15",
			"50.23",
			"50.81",
			"3000000",
			"50.80",
			"900",
		}
		c.handleHistoricalData(fields)
	}()

	contract := Contract{Symbol: "MBG", SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR"}
	if _, err := c.FetchHistoricalDailyBarsWithContract(context.Background(), contract, 10, 2*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	<-done

	payload := out.Bytes()
	for _, want := range [][]byte{[]byte("MBG"), []byte("SMART"), []byte("IBIS"), []byte("EUR"), []byte("1357911")} {
		if !bytes.Contains(payload, want) {
			t.Fatalf("expected payload to include %q, payload=%q", want, payload)
		}
	}
}

func TestFetchHistoricalDailyBarsFallbackToPrimaryExchange(t *testing.T) {
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
	c.contractCache["GLD"] = ContractDetailsLite{
		ConID:        1234567,
		LocalSymbol:  "GLD",
		TradingClass: "GLD",
		Exchange:     "SMART",
		PrimaryExch:  "ARCA",
	}

	done := make(chan struct{})
	go func() {
		attempt := 0
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			c.historicalMu.Lock()
			for id := range c.historicalReqs {
				attempt++
				switch attempt {
				case 1, 2:
					c.historicalMu.Unlock()
					c.completeHistoricalRequest(id, historicalResult{bars: nil})
				default:
					c.historicalMu.Unlock()
					fields := []string{
						strconv.Itoa(msgHistoricalData),
						strconv.Itoa(id),
						"1",
						"20240520",
						"168.10",
						"169.00",
						"167.50",
						"168.75",
						"850000",
						"168.40",
						"600",
					}
					c.handleHistoricalData(fields)
					close(done)
					return
				}
				goto NEXT
			}
			c.historicalMu.Unlock()
		NEXT:
			time.Sleep(5 * time.Millisecond)
		}
		close(done)
	}()

	bars, err := c.FetchHistoricalDailyBars(context.Background(), "GLD", 10, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("expected fallback to return 1 bar, got %d", len(bars))
	}
	<-done

	payload := out.Bytes()
	if bytes.Count(payload, []byte("SMART")) < 2 {
		t.Fatalf("expected SMART requests before primary fallback, payload=%q", payload)
	}
	if bytes.Count(payload, []byte("ARCA")) == 0 {
		t.Fatalf("expected fallback to primary exchange, payload=%q", payload)
	}
	if !bytes.Contains(payload, []byte("ADJUSTED_LAST")) {
		t.Fatalf("expected ADJUSTED_LAST retry before primary exchange success, payload=%q", payload)
	}
	if !bytes.Contains(payload, []byte("MIDPOINT")) {
		t.Fatalf("expected MIDPOINT retry before primary exchange success, payload=%q", payload)
	}
}

func TestFetchHistoricalDailyBarsRetriesOn162(t *testing.T) {
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
	c.contractCache["VIX"] = ContractDetailsLite{
		ConID:        13455763,
		LocalSymbol:  "VIX",
		TradingClass: "IND",
		Exchange:     "CBOE",
		PrimaryExch:  "CBOE",
	}

	done := make(chan struct{})
	go func() {
		attempt := 0
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			c.historicalMu.Lock()
			for id := range c.historicalReqs {
				attempt++
				reqID := id
				c.historicalMu.Unlock()
				switch attempt {
				case 1:
					c.handleIBKRError([]string{"4", "2", strconv.Itoa(reqID), "162", "Historical Market Data Service error"})
				case 2:
					fields := []string{
						strconv.Itoa(msgHistoricalData),
						strconv.Itoa(reqID),
						"1",
						"20240520",
						"17.10",
						"17.80",
						"16.95",
						"17.50",
						"350000",
						"17.45",
						"240",
					}
					c.handleHistoricalData(fields)
					close(done)
					return
				default:
					// No additional attempts expected
				}
				goto NEXT
			}
			c.historicalMu.Unlock()
		NEXT:
			time.Sleep(5 * time.Millisecond)
		}
		close(done)
	}()

	bars, err := c.FetchHistoricalDailyBars(context.Background(), "VIX", 10, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("expected retry to return 1 bar, got %d", len(bars))
	}
	<-done

	payloadBytes := out.Bytes()
	if !bytes.Contains(payloadBytes, []byte("13455763")) {
		t.Fatalf("expected historical request to include contract conID, payload=%q", payloadBytes)
	}
	payload := bytes.ToUpper(payloadBytes)
	firstTrades := bytes.Index(payload, []byte("TRADES"))
	if firstTrades == -1 {
		t.Fatalf("expected TRADES request in payload=%q", payload)
	}
	firstMid := bytes.Index(payload, []byte("MIDPOINT"))
	if firstMid == -1 {
		t.Fatalf("expected MIDPOINT fallback in payload=%q", payload)
	}
	if firstTrades > firstMid {
		t.Fatalf("expected TRADES request to precede MIDPOINT fallback, payload=%q", payload)
	}
}

func TestFetchHistoricalDailyBarsErrorsWhenContractDetailsMissing(t *testing.T) {
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

	c.fetchContractDetails = func(symbol string, timeout time.Duration) ([]ContractDetailsLite, error) {
		return nil, fmt.Errorf("timeout waiting for contract details")
	}

	start := time.Now()
	_, err := c.FetchHistoricalDailyBars(context.Background(), "SPY", 10, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when contract details are unavailable")
	}
	if !strings.Contains(err.Error(), "contract details unresolved") {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected no outbound frames, got %q", out.String())
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("expected quick failure, took %v", elapsed)
	}
}

func TestFetchHistoricalDailyBarsWaitsForLateContractDetails(t *testing.T) {
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

	c.fetchContractDetails = func(symbol string, timeout time.Duration) ([]ContractDetailsLite, error) {
		go func() {
			time.Sleep(25 * time.Millisecond)
			c.contractMu.Lock()
			c.contractCache[symbol] = ContractDetailsLite{
				Symbol:       symbol,
				ConID:        756733,
				LocalSymbol:  symbol,
				TradingClass: symbol,
				Exchange:     "SMART",
				PrimaryExch:  "ARCA",
			}
			c.contractMu.Unlock()
		}()
		return nil, fmt.Errorf("timeout waiting for contract details")
	}

	done := make(chan struct{})
	go func() {
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			c.historicalMu.Lock()
			for id := range c.historicalReqs {
				reqID := id
				c.historicalMu.Unlock()
				fields := []string{
					strconv.Itoa(msgHistoricalData),
					strconv.Itoa(reqID),
					"1",
					"20240520",
					"430.21",
					"432.00",
					"428.50",
					"431.75",
					"1500000",
					"430.85",
					"900",
				}
				c.handleHistoricalData(fields)
				close(done)
				return
			}
			c.historicalMu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
		close(done)
	}()

	bars, err := c.FetchHistoricalDailyBars(context.Background(), "SPY", 10, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("expected 1 bar, got %d", len(bars))
	}
	<-done
	if out.Len() == 0 {
		t.Fatal("expected historical request to be sent")
	}
}

func TestRequestHistoricalDataOrder(t *testing.T) {
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	var out bytes.Buffer
	conn.writer = bufio.NewWriter(&out)

	contract := Contract{ConID: 999999, Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"}
	reqID, err := conn.RequestHistoricalData(context.Background(), contract, "", "5 D", "1 day", "TRADES", true, false, 1, false, nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if reqID == 0 {
		t.Fatalf("expected non-zero reqID")
	}

	data := out.Bytes()
	if len(data) < 4 {
		t.Fatalf("unexpected message length")
	}
	length := binary.BigEndian.Uint32(data[:4])
	if int(length) > len(data)-4 {
		t.Fatalf("invalid payload length %d", length)
	}
	payload := data[4 : 4+length]
	fields := conn.decodeMessage(payload)

	if len(fields) == 0 || fields[0] != strconv.Itoa(reqHistoricalData) {
		t.Fatalf("expected message id %d, got %v", reqHistoricalData, fields)
	}
	if len(fields) < 6 {
		t.Fatalf("unexpected field count: %v", fields)
	}

	barIdx := indexOf(fields, "1 day")
	durIdx := indexOf(fields, "5 D")
	if barIdx == -1 || durIdx == -1 {
		t.Fatalf("missing barSize/duration fields: %v", fields)
	}
	if barIdx > durIdx {
		t.Fatalf("barSize should precede duration: %v", fields)
	}

	whatIdx := indexOf(fields, "TRADES")
	if whatIdx == -1 {
		t.Fatalf("missing whatToShow field: %v", fields)
	}
	if whatIdx == 0 {
		t.Fatalf("whatToShow at unexpected position: %v", fields)
	}
	if fields[whatIdx-1] != "1" { // useRTH encoded before whatToShow
		t.Fatalf("expected useRTH before whatToShow, got %v", fields)
	}
}

func TestFormatHistoricalDuration(t *testing.T) {
	tests := []struct {
		name string
		days int
		want string
	}{
		{name: "underYear", days: 120, want: "120 D"},
		{name: "exactYear", days: 365, want: "365 D"},
		{name: "overYear", days: 366, want: "2 Y"},
		{name: "partialYear", days: 400, want: "2 Y"},
		{name: "sixHundredFiftyDays", days: 650, want: "2 Y"},
		{name: "twoYears", days: 730, want: "2 Y"},
		{name: "overTwoYears", days: 731, want: "3 Y"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatHistoricalDuration(tt.days); got != tt.want {
				t.Fatalf("formatHistoricalDuration(%d) = %q, want %q", tt.days, got, tt.want)
			}
		})
	}
}

func indexOf(fields []string, target string) int {
	for i, f := range fields {
		if f == target {
			return i
		}
	}
	return -1
}
