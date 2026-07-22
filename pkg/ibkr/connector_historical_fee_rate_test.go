package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFetchHistoricalDailyFeeRatesPinsExactContractAndCoalesces(t *testing.T) {
	c, conn, out := newHistoricalFeeRateTestConnector(t)
	contract := Contract{
		ConID:        481516,
		Symbol:       "fee",
		SecType:      "stk",
		Exchange:     "smart",
		PrimaryExch:  "nasdaq",
		Currency:     "usd",
		LocalSymbol:  "FEE",
		TradingClass: "NMS",
	}

	type callResult struct {
		bars []HistoricalBar
		err  error
	}
	results := make(chan callResult, 2)
	for range 2 {
		go func() {
			bars, err := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 5, time.Second)
			results <- callResult{bars: bars, err: err}
		}()
	}

	reqID := waitForHistoricalRequest(t, c)
	c.handleHistoricalData([]string{
		strconv.Itoa(msgHistoricalData), strconv.Itoa(reqID), "1",
		"1721520000", "7.10", "8.40", "6.90", "8.10", "-1", "-1", "-1",
	})
	select {
	case got := <-results:
		t.Fatalf("fee-rate request completed before historicalDataEnd: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	c.handleHistoricalDataEnd([]string{
		strconv.Itoa(msgHistoricalDataEnd), "6", strconv.Itoa(reqID), "", "",
	})

	var first []HistoricalBar
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatalf("FetchHistoricalDailyFeeRates: %v", got.err)
		}
		if len(got.bars) != 1 || got.bars[0].Close != 8.10 {
			t.Fatalf("unexpected bars: %+v", got.bars)
		}
		if got.bars[0].Time.Unix() != 1721520000 {
			t.Fatalf("epoch timestamp not parsed: %+v", got.bars[0])
		}
		if first == nil {
			first = got.bars
		}
	}

	first[0].Close = 999
	cached, err := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 5, time.Second)
	if err != nil {
		t.Fatalf("cached fee-rate result: %v", err)
	}
	if len(cached) != 1 || cached[0].Close != 8.10 {
		t.Fatalf("cooldown result was not detached: %+v", cached)
	}

	fields := decodeSingleHistoricalRequest(t, conn, out.Bytes())
	for _, want := range []string{"481516", "FEE", "STK", "SMART", "NASDAQ", "USD", "FEE_RATE", "1 day", "5 D"} {
		if indexOf(fields, want) == -1 {
			t.Fatalf("exact fee-rate request missing %q: %v", want, fields)
		}
	}
	whatIndex := indexOf(fields, "FEE_RATE")
	if whatIndex < 0 || whatIndex+1 >= len(fields) || fields[whatIndex+1] != "2" {
		t.Fatalf("expected formatDate=2 after FEE_RATE, fields=%v", fields)
	}
	if bytes.Count(out.Bytes(), []byte("FEE_RATE")) != 1 {
		t.Fatalf("identical calls must send one request, payload=%q", out.Bytes())
	}
}

func TestFetchHistoricalDailyFeeRatesCallerCancellationDoesNotAbortSharedFlight(t *testing.T) {
	c, _, _ := newHistoricalFeeRateTestConnector(t)
	contract := Contract{ConID: 42, Symbol: "BORR", SecType: "STK", Exchange: "SMART", Currency: "USD"}

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := c.FetchHistoricalDailyFeeRates(ctx, contract, 3, time.Second)
		first <- err
	}()
	reqID := waitForHistoricalRequest(t, c)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller should observe cancellation, got %v", err)
	}

	second := make(chan error, 1)
	go func() {
		bars, err := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 3, time.Second)
		if err == nil && (len(bars) != 1 || bars[0].Close != 12.5) {
			err = errors.New("unexpected shared-flight bars")
		}
		second <- err
	}()
	c.handleHistoricalData([]string{
		strconv.Itoa(msgHistoricalData), strconv.Itoa(reqID), "1",
		"20260721", "12", "13", "11", "12.5", "-1", "-1", "-1",
	})
	c.handleHistoricalDataEnd([]string{strconv.Itoa(msgHistoricalDataEnd), "6", strconv.Itoa(reqID), "", ""})
	if err := <-second; err != nil {
		t.Fatalf("shared request did not survive caller cancellation: %v", err)
	}
}

func TestFetchHistoricalDailyFeeRatesShortCallerDoesNotPoisonCanonicalFlight(t *testing.T) {
	c, _, _ := newHistoricalFeeRateTestConnector(t)
	contract := Contract{ConID: 4242, Symbol: "SLOW", SecType: "STK", Exchange: "SMART", Currency: "USD"}

	first := make(chan error, 1)
	go func() {
		_, err := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 3, 10*time.Millisecond)
		first <- err
	}()
	reqID := waitForHistoricalRequest(t, c)
	err := <-first
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("detached request timeout = %T %v, want context deadline exceeded", err, err)
	}
	if requestErr, ok := errors.AsType[*HistoricalRequestError](err); ok && requestErr.Category == HistoricalFailureGatewayUnavailable {
		t.Fatalf("detached request timeout was misclassified as gateway unavailable: %+v", requestErr)
	}
	second := make(chan error, 1)
	go func() {
		bars, fetchErr := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 3, time.Second)
		if fetchErr == nil && (len(bars) != 1 || bars[0].Close != 4.5) {
			fetchErr = errors.New("unexpected canonical-flight bars")
		}
		second <- fetchErr
	}()
	c.handleHistoricalData([]string{
		strconv.Itoa(msgHistoricalData), strconv.Itoa(reqID), "1",
		"1721520000", "4", "5", "3", "4.5", "-1", "-1", "-1",
	})
	c.handleHistoricalDataEnd([]string{strconv.Itoa(msgHistoricalDataEnd), "6", strconv.Itoa(reqID), "", ""})
	if err := <-second; err != nil {
		t.Fatalf("longer waiter was poisoned by first timeout: %v", err)
	}
}

func TestFetchHistoricalDailyFeeRatesRejectsMalformedBar(t *testing.T) {
	c, _, _ := newHistoricalFeeRateTestConnector(t)
	contract := Contract{ConID: 43, Symbol: "BAD", SecType: "STK", Exchange: "SMART", Currency: "USD"}

	result := make(chan error, 1)
	go func() {
		_, err := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 2, time.Second)
		result <- err
	}()
	reqID := waitForHistoricalRequest(t, c)
	c.handleHistoricalData([]string{
		strconv.Itoa(msgHistoricalData), strconv.Itoa(reqID), "1",
		"20260721", "not-a-number", "13", "11", "12.5", "-1", "-1", "-1",
	})
	err := <-result
	requestErr, ok := errors.AsType[*HistoricalRequestError](err)
	if !ok || requestErr.Category != HistoricalFailureInvalidPayload || requestErr.Message != "" {
		t.Fatalf("expected sanitized invalid_payload error, got %T %v", err, err)
	}
}

func TestFetchHistoricalDailyFeeRatesRejectsTrailingAndDuplicateDailyPayload(t *testing.T) {
	for _, test := range []struct {
		name string
		send func(*Connector, int)
	}{
		{name: "trailing bar payload", send: func(c *Connector, reqID int) {
			c.handleHistoricalData([]string{
				strconv.Itoa(msgHistoricalData), strconv.Itoa(reqID), "1",
				"1721520000", "7", "8", "6", "7.5", "-1", "-1", "-1", "unexpected",
			})
		}},
		{name: "trailing end payload", send: func(c *Connector, reqID int) {
			c.handleHistoricalData([]string{
				strconv.Itoa(msgHistoricalData), strconv.Itoa(reqID), "1",
				"1721520000", "7", "8", "6", "7.5", "-1", "-1", "-1",
			})
			c.handleHistoricalDataEnd([]string{strconv.Itoa(msgHistoricalDataEnd), "6", strconv.Itoa(reqID), "", "", "unexpected"})
		}},
		{name: "duplicate session date", send: func(c *Connector, reqID int) {
			bar := []string{
				strconv.Itoa(msgHistoricalData), strconv.Itoa(reqID), "1",
				"1721520000", "7", "8", "6", "7.5", "-1", "-1", "-1",
			}
			c.handleHistoricalData(bar)
			c.handleHistoricalData(bar)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _, _ := newHistoricalFeeRateTestConnector(t)
			result := make(chan error, 1)
			go func() {
				_, err := c.FetchHistoricalDailyFeeRates(context.Background(), Contract{
					ConID: 46, Symbol: "STRICT", SecType: "STK", Exchange: "SMART", Currency: "USD",
				}, 2, time.Second)
				result <- err
			}()
			reqID := waitForHistoricalRequest(t, c)
			test.send(c, reqID)
			err := <-result
			requestErr, ok := errors.AsType[*HistoricalRequestError](err)
			if !ok || requestErr.Category != HistoricalFailureInvalidPayload || requestErr.Message != "" {
				t.Fatalf("strict payload result = %T %+v, want sanitized invalid_payload", err, requestErr)
			}
		})
	}
}

func TestFetchHistoricalDailyFeeRatesSanitizesEntitlementFailure(t *testing.T) {
	c, _, _ := newHistoricalFeeRateTestConnector(t)
	contract := Contract{ConID: 44, Symbol: "NOFEE", SecType: "STK", Exchange: "SMART", Currency: "USD"}

	result := make(chan error, 1)
	go func() {
		_, err := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 2, time.Second)
		result <- err
	}()
	reqID := waitForHistoricalRequest(t, c)
	c.handleIBKRError([]string{
		"4", "2", strconv.Itoa(reqID), "162",
		"Historical Market Data Service error message: no market data permissions for this instrument",
	})
	err := <-result
	requestErr, ok := errors.AsType[*HistoricalRequestError](err)
	if !ok {
		t.Fatalf("expected typed historical error, got %T %v", err, err)
	}
	if requestErr.Category != HistoricalFailureNotEntitled || requestErr.Message != "" {
		t.Fatalf("entitlement error was not sanitized: %+v", requestErr)
	}
	if strings.Contains(strings.ToLower(err.Error()), "permissions for this instrument") {
		t.Fatalf("broker prose leaked through sanitized error: %q", err.Error())
	}

	_, cachedErr := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 2, time.Second)
	cachedRequestErr, ok := errors.AsType[*HistoricalRequestError](cachedErr)
	if !ok || cachedRequestErr.Category != HistoricalFailureNotEntitled || cachedRequestErr.Message != "" {
		t.Fatalf("cached error was not detached and sanitized: %T %+v", cachedErr, cachedRequestErr)
	}
	if cachedRequestErr == requestErr {
		t.Fatal("cached caller received shared mutable error pointer")
	}
}

func TestFetchHistoricalDailyFeeRatesIDCollisionDoesNotEmitOrderLifecycle(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		message  string
		category string
		legacy   bool
	}{
		{name: "system notice no data", code: 162, message: "HMDS query returned no data", category: HistoricalFailureNoData},
		{name: "system notice contract", code: 200, message: "request rejected", category: HistoricalFailureContractUnavailable},
		{name: "system notice validation", code: 321, message: "request validation failed", category: HistoricalFailureProtocolRejected},
		{name: "system notice missing query", code: 366, message: "request rejected", category: HistoricalFailureProtocolRejected},
		{name: "legacy error no data", code: 162, message: "HMDS query returned no data", category: HistoricalFailureNoData, legacy: true},
		{name: "legacy error contract", code: 200, message: "request rejected", category: HistoricalFailureContractUnavailable, legacy: true},
		{name: "legacy error validation", code: 321, message: "request validation failed", category: HistoricalFailureProtocolRejected, legacy: true},
		{name: "legacy error missing query", code: 366, message: "request rejected", category: HistoricalFailureProtocolRejected, legacy: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _ := newHistoricalFeeRateTestConnector(t)
			contract := Contract{ConID: 45, Symbol: "COLLIDE", SecType: "STK", Exchange: "SMART", Currency: "USD"}

			lifecycle := make(chan OrderLifecycleEvent, 1)
			c.RegisterOrderLifecycleHandler(func(event OrderLifecycleEvent) { lifecycle <- event })
			result := make(chan error, 1)
			go func() {
				_, err := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 2, time.Second)
				result <- err
			}()
			reqID := waitForHistoricalRequest(t, c)
			c.orderMu.Lock()
			c.brokerOrderIndex[strconv.Itoa(reqID)] = "known-order-ref"
			c.orderMu.Unlock()

			if tt.legacy {
				c.handleIBKRError([]string{"4", "2", strconv.Itoa(reqID), strconv.Itoa(tt.code), tt.message})
			} else {
				c.processSystemNotice(reqAliasEntry{}, &systemNotification{
					tickerID: int64(reqID), code: tt.code, message: tt.message, timestamp: time.Now(),
				})
			}

			err := <-result
			requestErr, ok := errors.AsType[*HistoricalRequestError](err)
			if !ok || requestErr.Category != tt.category || requestErr.Message != "" {
				t.Fatalf("colliding FEE_RATE failure = %T %+v, want sanitized %s", err, requestErr, tt.category)
			}
			select {
			case event := <-lifecycle:
				t.Fatalf("historical request-id collision emitted order lifecycle: %+v", event)
			default:
			}
		})
	}
}

func TestFetchHistoricalDailyFeeRatesSkipsKnownOrderRequestID(t *testing.T) {
	c, conn, _ := newHistoricalFeeRateTestConnector(t)
	if orderID := conn.GetNextOrderID(); orderID != 1 {
		t.Fatalf("reserved order id = %d, want 1", orderID)
	}

	result := make(chan error, 1)
	go func() {
		_, err := c.FetchHistoricalDailyFeeRates(context.Background(), Contract{
			ConID: 45, Symbol: "DISJOINT", SecType: "STK", Exchange: "SMART", Currency: "USD",
		}, 2, time.Second)
		result <- err
	}()
	reqID := waitForHistoricalRequest(t, c)
	if reqID != 2 {
		t.Fatalf("FEE_RATE request id = %d, want 2 after reserving around known order id 1", reqID)
	}
	c.handleHistoricalDataEnd([]string{strconv.Itoa(msgHistoricalDataEnd), "6", strconv.Itoa(reqID), "", ""})
	if err := <-result; err == nil {
		t.Fatal("empty completed historical response unexpectedly succeeded")
	} else if requestErr, ok := errors.AsType[*HistoricalRequestError](err); !ok || requestErr.Category != HistoricalFailureNoData {
		t.Fatalf("empty completed response = %T %v, want typed no_data", err, err)
	}
	conn.reqIDMu.Lock()
	next := conn.reqIDSeq
	conn.reqIDMu.Unlock()
	if next != 3 {
		t.Fatalf("next request id = %d, want 3", next)
	}
	if orderID := conn.GetNextOrderID(); orderID != 3 {
		t.Fatalf("completed request id was reused by order allocator: got %d want 3", orderID)
	}
}

func TestOrderClaimingActiveFeeRateIDFailsClosedAndDelayedErrorsRemainRequestOwned(t *testing.T) {
	c, conn, _ := newHistoricalFeeRateTestConnector(t)
	conn.config.Host = "127.0.0.1"
	conn.config.Port = 7497
	conn.config.ClientID = 41
	conn.config.Account = "DU1234567"

	feeResult := make(chan error, 1)
	go func() {
		_, err := c.FetchHistoricalDailyFeeRates(context.Background(), Contract{
			ConID: 45, Symbol: "COLLIDE", SecType: "STK", Exchange: "SMART", Currency: "USD",
		}, 2, time.Second)
		feeResult <- err
	}()
	reqID := waitForHistoricalRequest(t, c)

	lifecycle := make(chan OrderLifecycleEvent, 1)
	c.RegisterOrderLifecycleHandler(func(event OrderLifecycleEvent) { lifecycle <- event })
	err := c.SubmitPaperOrder(PaperOrderGate{
		Mode: "paper", Account: "DU1234567", Host: "127.0.0.1", Port: 7497, ClientID: 41,
	}, &Contract{
		ConID: 45, Symbol: "COLLIDE", SecType: "STK", Exchange: "SMART", Currency: "USD",
	}, &RawOrder{
		OrderID: reqID, Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: "DU1234567", OrderRef: "collision-order",
	})
	if !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("SubmitPaperOrder err=%v, want collision refusal", err)
	}

	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		tickerID: int64(reqID), code: 200, message: "delayed request rejection", timestamp: time.Now(),
	})
	feeErr := <-feeResult
	requestErr, ok := errors.AsType[*HistoricalRequestError](feeErr)
	if !ok || requestErr.Category != HistoricalFailureContractUnavailable || requestErr.Message != "" {
		t.Fatalf("delayed FEE failure = %T %+v, want sanitized contract_unavailable", feeErr, requestErr)
	}
	select {
	case event := <-lifecycle:
		t.Fatalf("refused order received delayed request lifecycle: %+v", event)
	default:
	}
}

func TestFetchHistoricalDailyFeeRatesRequiresExactStockRoute(t *testing.T) {
	c, _, out := newHistoricalFeeRateTestConnector(t)
	tests := []struct {
		name     string
		contract Contract
		reason   string
	}{
		{name: "missing conid", contract: Contract{Symbol: "A", SecType: "STK", Exchange: "SMART", Currency: "USD"}, reason: "missing_contract_id"},
		{name: "non-stock", contract: Contract{ConID: 1, Symbol: "A", SecType: "OPT", Exchange: "SMART", Currency: "USD"}, reason: "unsupported_security_type"},
		{name: "missing currency", contract: Contract{ConID: 1, Symbol: "A", SecType: "STK", Exchange: "NASDAQ"}, reason: "incomplete_exact_route"},
		{name: "non-US currency", contract: Contract{ConID: 1, Symbol: "A", SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR"}, reason: "unsupported_market_calendar"},
		{name: "non-US route", contract: Contract{ConID: 1, Symbol: "A", SecType: "STK", Exchange: "IBIS", PrimaryExch: "IBIS", Currency: "USD"}, reason: "unsupported_market_calendar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.FetchHistoricalDailyFeeRates(context.Background(), tt.contract, 2, time.Second)
			validation, ok := errors.AsType[*HistoricalDataValidationError](err)
			if !ok || validation.Reason != tt.reason {
				t.Fatalf("expected %s, got %T %v", tt.reason, err, err)
			}
		})
	}
	if out.Len() != 0 {
		t.Fatalf("invalid exact contracts must not reach wire: %q", out.Bytes())
	}
}

func TestFetchHistoricalDailyFeeRatesHydratesProductionPositionByExactConID(t *testing.T) {
	c, conn, out := newHistoricalFeeRateTestConnector(t)
	contract := Contract{
		ConID: 481516, Symbol: "FEE", SecType: "STK", PrimaryExch: "NASDAQ",
		Currency: "USD", LocalSymbol: "FEE", TradingClass: "NMS",
	}
	type result struct {
		bars []HistoricalBar
		err  error
	}
	done := make(chan result, 1)
	go func() {
		bars, err := c.FetchHistoricalDailyFeeRates(context.Background(), contract, 5, time.Second)
		done <- result{bars: bars, err: err}
	}()

	detailReqID := waitForHandlerReqID(t, conn, msgContractData)
	sendExactContractDetailsForTest(conn, detailReqID, ContractDetailsLite{
		ConID: 481516, Symbol: "FEE", SecType: "STK", Exchange: "NASDAQ",
		PrimaryExch: "NASDAQ", Currency: "USD", LocalSymbol: "FEE", TradingClass: "NMS",
	})
	sendContractDetailsEndForTest(conn, detailReqID)

	histReqID := waitForHistoricalRequest(t, c)
	c.handleHistoricalData([]string{
		strconv.Itoa(msgHistoricalData), strconv.Itoa(histReqID), "1",
		"1721520000", "7", "8", "6", "7.5", "-1", "-1", "-1",
	})
	c.handleHistoricalDataEnd([]string{strconv.Itoa(msgHistoricalDataEnd), "6", strconv.Itoa(histReqID), "", ""})
	got := <-done
	if got.err != nil || len(got.bars) != 1 || got.bars[0].Close != 7.5 {
		t.Fatalf("exact route hydration result = bars=%+v err=%v", got.bars, got.err)
	}

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	detailFrame := findOutboundFrame(t, frames, reqContractData)
	assertField(t, detailFrame, 3, "481516", "exact detail conID")
	assertField(t, detailFrame, 10, "", "exact detail unresolved exchange")
	assertField(t, detailFrame, 11, "NASDAQ", "exact detail primary")
	histFrame := findOutboundFrame(t, frames, reqHistoricalData)
	for _, want := range []string{"481516", "FEE", "STK", "NASDAQ", "USD", "FEE_RATE"} {
		if indexOf(histFrame, want) == -1 {
			t.Fatalf("hydrated historical request missing %q: %v", want, histFrame)
		}
	}
}

func TestFetchHistoricalDailyFeeRatesRejectsWrongMissingAndAmbiguousExactDetails(t *testing.T) {
	tests := []struct {
		name    string
		details []ContractDetailsLite
	}{
		{name: "no details"},
		{name: "wrong conID", details: []ContractDetailsLite{{
			ConID: 999, Symbol: "FEE", SecType: "STK", Exchange: "NASDAQ", PrimaryExch: "NASDAQ", Currency: "USD",
		}}},
		{name: "ambiguous routes", details: []ContractDetailsLite{
			{ConID: 481516, Symbol: "FEE", SecType: "STK", Exchange: "NASDAQ", PrimaryExch: "NASDAQ", Currency: "USD"},
			{ConID: 481516, Symbol: "FEE", SecType: "STK", Exchange: "NYSE", PrimaryExch: "NASDAQ", Currency: "USD"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, conn, out := newHistoricalFeeRateTestConnector(t)
			done := make(chan error, 1)
			go func() {
				_, err := c.FetchHistoricalDailyFeeRates(context.Background(), Contract{
					ConID: 481516, Symbol: "FEE", SecType: "STK", PrimaryExch: "NASDAQ", Currency: "USD",
				}, 5, time.Second)
				done <- err
			}()
			reqID := waitForHandlerReqID(t, conn, msgContractData)
			for _, detail := range tt.details {
				sendExactContractDetailsForTest(conn, reqID, detail)
			}
			sendContractDetailsEndForTest(conn, reqID)
			err := <-done
			requestErr, ok := errors.AsType[*HistoricalRequestError](err)
			if !ok || requestErr.Category != HistoricalFailureContractUnavailable || requestErr.Message != "" {
				t.Fatalf("exact detail rejection = %T %+v, want sanitized contract_unavailable", err, requestErr)
			}
			if bytes.Contains(out.Bytes(), []byte("FEE_RATE")) {
				t.Fatalf("rejected exact details reached HMDS: %q", out.Bytes())
			}
		})
	}
}

func sendExactContractDetailsForTest(conn *Connection, reqID int, detail ContractDetailsLite) {
	frame := make([]string, 29)
	frame[0] = strconv.Itoa(msgContractData)
	frame[1] = strconv.Itoa(reqID)
	frame[2] = detail.Symbol
	frame[3] = detail.SecType
	frame[8] = detail.Exchange
	frame[9] = detail.Currency
	frame[10] = detail.LocalSymbol
	frame[12] = detail.TradingClass
	frame[13] = strconv.Itoa(detail.ConID)
	frame[21] = detail.PrimaryExch
	for _, handler := range conn.snapshotHandlers(msgContractData) {
		handler(frame)
	}
}

func sendContractDetailsEndForTest(conn *Connection, reqID int) {
	frame := []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(reqID)}
	for _, handler := range conn.snapshotHandlers(msgContractDataEnd) {
		handler(frame)
	}
}

func TestResolveExactHistoricalStockRouteCachesSuccessAndFailureBeforeWire(t *testing.T) {
	for _, test := range []struct {
		name    string
		succeed bool
	}{
		{name: "success", succeed: true},
		{name: "failure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, conn, out := newHistoricalFeeRateTestConnector(t)
			contract := Contract{ConID: 481516, Symbol: "ROUTE", SecType: "STK", PrimaryExch: "NASDAQ", Currency: "USD"}
			first := make(chan error, 1)
			go func() {
				_, err := c.ResolveExactHistoricalStockRoute(context.Background(), contract, time.Second)
				first <- err
			}()
			reqID := waitForHistoricalRouteRequest(t, c)
			if test.succeed {
				sendExactContractDetailsForTest(conn, reqID, ContractDetailsLite{
					ConID: 481516, Symbol: "ROUTE", SecType: "STK", Exchange: "NASDAQ",
					PrimaryExch: "NASDAQ", Currency: "USD",
				})
			}
			sendContractDetailsEndForTest(conn, reqID)
			firstErr := <-first
			if test.succeed && firstErr != nil {
				t.Fatalf("first route resolution: %v", firstErr)
			}
			if !test.succeed {
				requestErr, ok := errors.AsType[*HistoricalRequestError](firstErr)
				if !ok || requestErr.Category != HistoricalFailureContractUnavailable {
					t.Fatalf("first route failure = %T %+v", firstErr, requestErr)
				}
			}
			before := len(out.Bytes())
			_, secondErr := c.ResolveExactHistoricalStockRoute(context.Background(), contract, time.Second)
			if test.succeed != (secondErr == nil) {
				t.Fatalf("cached route result err=%v succeed=%v", secondErr, test.succeed)
			}
			if len(out.Bytes()) != before {
				t.Fatalf("cached route result reached wire: before=%d after=%d", before, len(out.Bytes()))
			}
		})
	}
}

func TestResolveExactHistoricalStockRouteCollectorOverflowFailsClosed(t *testing.T) {
	c, conn, out := newHistoricalFeeRateTestConnector(t)
	conn.pauseTransport()
	defer conn.resumeTransport()
	contract := Contract{ConID: 481516, Symbol: "OVERFLOW", SecType: "STK", PrimaryExch: "NASDAQ", Currency: "USD"}
	done := make(chan error, 1)
	go func() {
		_, err := c.ResolveExactHistoricalStockRoute(context.Background(), contract, time.Second)
		done <- err
	}()
	reqID := waitForHistoricalRouteRequest(t, c)
	for range 9 {
		sendExactContractDetailsForTest(conn, reqID, ContractDetailsLite{
			ConID: 481516, Symbol: "OVERFLOW", SecType: "STK", Exchange: "NASDAQ",
			PrimaryExch: "NASDAQ", Currency: "USD",
		})
	}
	conn.resumeTransport()
	err := <-done
	requestErr, ok := errors.AsType[*HistoricalRequestError](err)
	if !ok || requestErr.Category != HistoricalFailureInvalidPayload || requestErr.Message != "" {
		t.Fatalf("overflow result = %T %+v, want sanitized invalid_payload", err, requestErr)
	}
	if bytes.Contains(out.Bytes(), []byte("FEE_RATE")) {
		t.Fatalf("overflow reached historical FEE_RATE wire: %q", out.Bytes())
	}
	before := len(out.Bytes())
	if _, err := c.ResolveExactHistoricalStockRoute(context.Background(), contract, time.Second); err == nil {
		t.Fatal("cached overflow unexpectedly succeeded")
	}
	if len(out.Bytes()) != before {
		t.Fatal("cached overflow repeated contract-details wire")
	}
}

func TestHistoricalFeeRateEpochChecksPreventReconnectGapWire(t *testing.T) {
	t.Run("contract details", func(t *testing.T) {
		c, conn, _ := newHistoricalFeeRateTestConnector(t)
		conn.pauseTransport()
		defer conn.resumeTransport()
		done := make(chan error, 1)
		go func() {
			_, err := c.ResolveExactHistoricalStockRoute(context.Background(), Contract{
				ConID: 481516, Symbol: "EPOCHROUTE", SecType: "STK", PrimaryExch: "NASDAQ", Currency: "USD",
			}, time.Second)
			done <- err
		}()
		_ = waitForHistoricalRouteRequest(t, c)
		var newSocket bytes.Buffer
		conn.transportMu.Lock()
		conn.writer = bufio.NewWriter(&newSocket)
		conn.transportMu.Unlock()
		conn.resetOrderIDReadiness()
		conn.observeNextValidOrderID(100)
		conn.resumeTransport()
		if err := <-done; err == nil {
			t.Fatal("old-epoch route request unexpectedly succeeded")
		}
		if newSocket.Len() != 0 {
			t.Fatalf("old-epoch route wrote %d bytes on new socket", newSocket.Len())
		}
	})

	t.Run("historical data", func(t *testing.T) {
		c, conn, _ := newHistoricalFeeRateTestConnector(t)
		conn.pauseTransport()
		defer conn.resumeTransport()
		done := make(chan error, 1)
		go func() {
			_, err := c.FetchHistoricalDailyFeeRates(context.Background(), Contract{
				ConID: 481516, Symbol: "EPOCHFEE", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD",
			}, 3, time.Second)
			done <- err
		}()
		_ = waitForHistoricalRequest(t, c)
		var newSocket bytes.Buffer
		conn.transportMu.Lock()
		conn.writer = bufio.NewWriter(&newSocket)
		conn.transportMu.Unlock()
		conn.resetOrderIDReadiness()
		conn.observeNextValidOrderID(100)
		conn.resumeTransport()
		err := <-done
		requestErr, ok := errors.AsType[*HistoricalRequestError](err)
		if !ok || requestErr.Category != HistoricalFailureGatewayUnavailable {
			t.Fatalf("old-epoch historical result = %T %+v", err, requestErr)
		}
		if newSocket.Len() != 0 {
			t.Fatalf("old-epoch historical request wrote %d bytes on new socket", newSocket.Len())
		}
	})
}

func TestHistoricalFeeRateEpochSendDoesNotRetryOrEscapeCancellation(t *testing.T) {
	t.Run("partial write is not retried", func(t *testing.T) {
		c, conn, _ := newHistoricalFeeRateTestConnector(t)
		failing := &feeRateFailWriter{}
		conn.writer = bufio.NewWriter(failing)
		_, err := c.FetchHistoricalDailyFeeRates(context.Background(), Contract{
			ConID: 481516, Symbol: "PARTIAL", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD",
		}, 3, time.Second)
		if err == nil {
			t.Fatal("partial write unexpectedly succeeded")
		}
		if failing.calls != 1 {
			t.Fatalf("partial epoch-bound write attempts = %d, want exactly one", failing.calls)
		}
	})

	t.Run("canceled queued send cannot write later", func(t *testing.T) {
		_, conn, out := newHistoricalFeeRateTestConnector(t)
		conn.pauseTransport()
		defer conn.resumeTransport()
		ctx, cancel := context.WithCancel(context.Background())
		registered := make(chan struct{})
		done := make(chan error, 1)
		epoch := conn.BrokerSessionEpoch()
		go func() {
			_, err := conn.requestHistoricalDailyFeeRateForEpoch(ctx, Contract{
				ConID: 481516, Symbol: "CANCELLED", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD",
			}, 3, epoch, func(int) { close(registered) })
			done <- err
		}()
		<-registered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled queued FEE_RATE send = %v", err)
		}
		conn.resumeTransport()
		time.Sleep(20 * time.Millisecond)
		if out.Len() != 0 {
			t.Fatalf("canceled queued FEE_RATE send wrote %d bytes later", out.Len())
		}
	})
}

type feeRateFailWriter struct {
	calls int
}

func (w *feeRateFailWriter) Write(p []byte) (int, error) {
	w.calls++
	return 0, errors.New("forced fee-rate write failure")
}

func TestHistoricalDailyFeeRateCooldownExpiresAtFifteenSeconds(t *testing.T) {
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	c := &Connector{
		historicalExactFlights: make(map[string]*historicalExactFlight),
		historicalNow:          func() time.Time { return now },
	}
	binding := HistoricalSessionBinding{}
	first, leader := c.acquireHistoricalExactFlight("exact-contract", binding)
	if !leader {
		t.Fatal("first caller must lead")
	}
	c.historicalMu.Lock()
	first.completedAt = now
	first.expiresAt = now.Add(historicalIdenticalRequestCooldown)
	close(first.done)
	c.historicalMu.Unlock()

	now = now.Add(historicalIdenticalRequestCooldown - time.Nanosecond)
	withinCooldown, leader := c.acquireHistoricalExactFlight("exact-contract", binding)
	if leader || withinCooldown != first {
		t.Fatal("request before 15 seconds must reuse completed flight")
	}

	now = now.Add(time.Nanosecond)
	afterCooldown, leader := c.acquireHistoricalExactFlight("exact-contract", binding)
	if !leader || afterCooldown == first {
		t.Fatal("request at 15 seconds must start a fresh flight")
	}
}

func TestClassifyHistoricalRequestFailure162IsConservative(t *testing.T) {
	tests := []struct {
		message string
		want    string
	}{
		{message: "no market data permissions", want: HistoricalFailureNotEntitled},
		{message: "HMDS query returned no data", want: HistoricalFailureNoData},
		{message: "historical data request pacing violation", want: HistoricalFailurePacing},
		{message: "historical market data service error", want: HistoricalFailureProtocolRejected},
	}
	for _, tt := range tests {
		got := classifyHistoricalRequestFailure(&HistoricalRequestError{Code: 162, Message: tt.message})
		if got != tt.want {
			t.Fatalf("classify 162 %q = %q, want %q", tt.message, got, tt.want)
		}
	}
}

func newHistoricalFeeRateTestConnector(t *testing.T) (*Connector, *Connection, *bytes.Buffer) {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	out := &bytes.Buffer{}
	conn.writer = bufio.NewWriter(out)
	c.conn = conn
	c.running = true
	c.ready = true
	return c, conn, out
}

func waitForHistoricalRequest(t *testing.T, c *Connector) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.historicalMu.Lock()
		for reqID := range c.historicalReqs {
			c.historicalMu.Unlock()
			return reqID
		}
		c.historicalMu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for historical request")
	return 0
}

func waitForHistoricalRouteRequest(t *testing.T, c *Connector) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.historicalMu.Lock()
		for reqID := range c.historicalRouteReqs {
			c.historicalMu.Unlock()
			return reqID
		}
		c.historicalMu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for exact historical route request")
	return 0
}

func decodeSingleHistoricalRequest(t *testing.T, conn *Connection, data []byte) []string {
	t.Helper()
	if len(data) < 4 {
		t.Fatalf("historical request payload too short: %d", len(data))
	}
	length := int(binary.BigEndian.Uint32(data[:4]))
	if length <= 0 || length > len(data)-4 {
		t.Fatalf("invalid historical request frame length %d for %d bytes", length, len(data))
	}
	return conn.decodeMessage(data[4 : 4+length])
}
