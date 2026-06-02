package ibkr

import (
	"bufio"
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOptionDetailMatchesRequestRejectsTradingClassMismatch(t *testing.T) {
	t.Parallel()
	requested := Contract{Symbol: "SPX", TradingClass: "SPX", Expiry: "20260619", Strike: 5400, Right: "C"}

	if optionDetailMatchesRequest(ContractDetailsLite{ConID: 123, TradingClass: "SPXW"}, requested) {
		t.Fatalf("SPX request must not accept SPXW contract details")
	}
	if !optionDetailMatchesRequest(ContractDetailsLite{ConID: 123, TradingClass: "SPX"}, requested) {
		t.Fatalf("matching SPX contract details should be accepted")
	}
	if optionDetailMatchesRequest(ContractDetailsLite{TradingClass: "SPX"}, requested) {
		t.Fatalf("zero ConID contract details should be rejected")
	}
}

func TestSendContractDetailsRequestCarriesOptionPrimaryAndMultiplier(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)

	err := conn.sendContractDetailsRequest(Contract{
		Symbol:       "SPY",
		SecType:      "OPT",
		Expiry:       "20260619",
		Strike:       500,
		Right:        "C",
		Multiplier:   100,
		Exchange:     "SMART",
		PrimaryExch:  "ARCA",
		Currency:     "USD",
		TradingClass: "SPY",
	}, 77)
	if err != nil {
		t.Fatalf("sendContractDetailsRequest: %v", err)
	}

	fields := onlyOutboundFrame(t, conn, out.Bytes())
	assertField(t, fields, 0, strconv.Itoa(reqContractData), "msgID")
	assertField(t, fields, 4, "SPY", "symbol")
	assertField(t, fields, 5, "OPT", "secType")
	assertField(t, fields, 9, "100", "multiplier")
	assertField(t, fields, 10, "SMART", "exchange")
	assertField(t, fields, 11, "ARCA", "primaryExchange")
	assertField(t, fields, 12, "USD", "currency")
	assertField(t, fields, 14, "SPY", "tradingClass")
}

func TestSendContractDetailsRequestBlanksUnresolvedStockPrimary(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)

	err := conn.sendContractDetailsRequest(Contract{
		Symbol:      "SPY",
		SecType:     "STK",
		Exchange:    "SMART",
		PrimaryExch: "ARCA",
		Currency:    "USD",
	}, 78)
	if err != nil {
		t.Fatalf("sendContractDetailsRequest: %v", err)
	}

	fields := onlyOutboundFrame(t, conn, out.Bytes())
	assertField(t, fields, 0, strconv.Itoa(reqContractData), "msgID")
	assertField(t, fields, 4, "SPY", "symbol")
	assertField(t, fields, 5, "STK", "secType")
	assertField(t, fields, 10, "SMART", "exchange")
	assertField(t, fields, 11, "", "primaryExchange")
}

func TestSubscribeOptionResolvesSPYThenBlanksPrimaryForMarketDataAndOpenInterestTicks(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn, out := newReadyWireTestConnection(t)
	c.conn = conn
	c.running = true
	c.ready = true

	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		contractReqID := waitForHandlerReqID(t, conn, msgContractData)
		if contractReqID == 0 {
			return
		}
		frame := make([]string, 29)
		frame[0] = strconv.Itoa(msgContractData)
		frame[1] = strconv.Itoa(contractReqID)
		frame[2] = "SPY"
		frame[3] = "OPT"
		frame[4] = "20260619"
		frame[6] = "500"
		frame[7] = "C"
		frame[8] = "ARCA"
		frame[9] = "USD"
		frame[10] = "SPY   260619C00500000"
		frame[12] = "SPY"
		frame[13] = "99999"
		frame[15] = "100"
		frame[21] = "ARCA"
		for _, h := range conn.snapshotHandlers(msgContractData) {
			h(frame)
		}
		time.Sleep(20 * time.Millisecond)
		endFrame := []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(contractReqID)}
		for _, h := range conn.snapshotHandlers(msgContractDataEnd) {
			h(endFrame)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	key, reqID, err := c.SubscribeOption(ctx, "SPY", "SPY", "20260619", 500, "C")
	if err != nil {
		t.Fatalf("SubscribeOption: %v", err)
	}
	<-responderDone
	if reqID == 0 || key == "" {
		t.Fatalf("expected subscription key and reqID, got key=%q reqID=%d", key, reqID)
	}

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	contractDetails := findOutboundFrame(t, frames, reqContractData)
	marketData := findOutboundFrame(t, frames, reqMktData)

	assertField(t, contractDetails, 4, "SPY", "contractDetails symbol")
	assertField(t, contractDetails, 5, "OPT", "contractDetails secType")
	assertField(t, contractDetails, 10, "SMART", "contractDetails exchange")
	assertField(t, contractDetails, 11, "ARCA", "contractDetails primaryExchange")

	assertField(t, marketData, 3, "99999", "marketData conID")
	assertField(t, marketData, 4, "SPY", "marketData symbol")
	assertField(t, marketData, 5, "OPT", "marketData secType")
	assertField(t, marketData, 9, "100", "marketData multiplier")
	assertField(t, marketData, 10, "ARCA", "marketData exchange")
	assertField(t, marketData, 11, "", "marketData primaryExchange")
	assertField(t, marketData, 13, "SPY   260619C00500000", "marketData localSymbol")
	assertField(t, marketData, 14, "SPY", "marketData tradingClass")
	if len(marketData) <= 16 || !strings.Contains(marketData[16], "101") {
		t.Fatalf("marketData generic ticks missing 101: fields=%#v", marketData)
	}
}

func TestPrewarmOptionChainRetriesSPYZeroContractDetailsWithRouteVariants(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)

	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		firstReqID := waitForHandlerReqIDAfter(t, conn, msgContractData, 0)
		if firstReqID == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
		endFrame := []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(firstReqID)}
		for _, h := range conn.snapshotHandlers(msgContractDataEnd) {
			h(endFrame)
		}

		secondReqID := waitForHandlerReqIDAfter(t, conn, msgContractData, firstReqID)
		if secondReqID == 0 {
			return
		}
		frame := make([]string, 29)
		frame[0] = strconv.Itoa(msgContractData)
		frame[1] = strconv.Itoa(secondReqID)
		frame[2] = "SPY"
		frame[3] = "OPT"
		frame[4] = "20260619"
		frame[6] = "500"
		frame[7] = "C"
		frame[8] = "ARCA"
		frame[9] = "USD"
		frame[10] = "SPY   260619C00500000"
		frame[12] = "SPY"
		frame[13] = "99999"
		frame[15] = "100"
		frame[21] = "ARCA"
		for _, h := range conn.snapshotHandlers(msgContractData) {
			h(frame)
		}
		time.Sleep(20 * time.Millisecond)
		endFrame = []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(secondReqID)}
		for _, h := range conn.snapshotHandlers(msgContractDataEnd) {
			h(endFrame)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cached, dropped, err := conn.prewarmOneExpiry(ctx, "SPY", "20260619", "SPY", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("prewarmOneExpiry: %v", err)
	}
	if cached != 1 || dropped != 0 {
		t.Fatalf("prewarmOneExpiry cached=%d dropped=%d, want 1/0", cached, dropped)
	}
	<-responderDone

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	if len(frames) < 2 {
		t.Fatalf("expected at least 2 contract-details attempts, got %#v", frames)
	}
	assertField(t, frames[0], 10, "SMART", "first exchange")
	assertField(t, frames[0], 11, "ARCA", "first primaryExchange")
	assertField(t, frames[1], 10, "ARCA", "second exchange")
	assertField(t, frames[1], 11, "", "second primaryExchange")
}

func newReadyWireTestConnection(t *testing.T) (*Connection, *safeBuffer) {
	t.Helper()
	conn := NewConnection(nil)
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	out := &safeBuffer{}
	conn.writer = bufio.NewWriter(out)
	return conn, out
}

func decodeOutboundFrames(t *testing.T, conn *Connection, payload []byte) [][]string {
	t.Helper()
	var frames [][]string
	offset := 0
	for offset+4 <= len(payload) {
		length := int(binary.BigEndian.Uint32(payload[offset : offset+4]))
		start := offset + 4
		end := start + length
		if length < 0 || end > len(payload) {
			t.Fatalf("invalid outbound length=%d offset=%d payloadLen=%d", length, offset, len(payload))
		}
		frames = append(frames, conn.decodeMessage(payload[start:end]))
		offset = end
	}
	if offset != len(payload) {
		t.Fatalf("trailing partial outbound frame: offset=%d payloadLen=%d", offset, len(payload))
	}
	return frames
}

func onlyOutboundFrame(t *testing.T, conn *Connection, payload []byte) []string {
	t.Helper()
	frames := decodeOutboundFrames(t, conn, payload)
	if len(frames) != 1 {
		t.Fatalf("expected 1 outbound frame, got %d: %#v", len(frames), frames)
	}
	return frames[0]
}

func findOutboundFrame(t *testing.T, frames [][]string, msgID int) []string {
	t.Helper()
	want := strconv.Itoa(msgID)
	for _, frame := range frames {
		if len(frame) > 0 && frame[0] == want {
			return frame
		}
	}
	t.Fatalf("outbound frame msgID=%d not found: %#v", msgID, frames)
	return nil
}

func assertField(t *testing.T, fields []string, idx int, want string, name string) {
	t.Helper()
	if len(fields) <= idx {
		t.Fatalf("%s field[%d] missing: %#v", name, idx, fields)
	}
	if fields[idx] != want {
		t.Fatalf("%s field[%d] = %q, want %q; fields=%#v", name, idx, fields[idx], want, fields)
	}
}

func waitForHandlerReqID(t *testing.T, conn *Connection, msgID int) int {
	t.Helper()
	return waitForHandlerReqIDAfter(t, conn, msgID, 0)
}

func waitForHandlerReqIDAfter(t *testing.T, conn *Connection, msgID int, after int) int {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn.handlersMu.RLock()
		registered := len(conn.msgHandlers[msgID]) > 0
		conn.handlersMu.RUnlock()
		if registered {
			conn.reqIDMu.Lock()
			reqID := conn.reqIDSeq - 1
			conn.reqIDMu.Unlock()
			if reqID > after {
				return reqID
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("handler for msgID=%d never registered", msgID)
	return 0
}
