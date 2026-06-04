package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPreviewOrderWhatIfSendsBrokerWhatIfAndWaitsForOpenOrder(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder-1)
	conn.nextOrderID = 77

	var buf bytes.Buffer
	conn.writer = bufio.NewWriter(&buf)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	type outcome struct {
		result OrderWhatIfResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := conn.PreviewOrderWhatIf(ctx, &IBKROrder{
			Symbol:    "MSFT",
			SecType:   "STK",
			Exchange:  "SMART",
			Currency:  "USD",
			Action:    "BUY",
			TotalQty:  2,
			OrderType: "LMT",
			LmtPrice:  425.50,
			TIF:       "DAY",
			Account:   "DU123456",
			OrderRef:  "preview-test",
			Transmit:  false,
		})
		done <- outcome{result: result, err: err}
	}()

	waitForWhatIfFrame(t, &buf)
	fields := extractWhatIfPayloadFields(t, &buf)
	if fields[27] != "1" {
		t.Fatalf("transmit field = %q, want 1", fields[27])
	}
	if fields[placeOrderFieldWhatIf] != "1" {
		t.Fatalf("whatIf field = %q, want 1", fields[placeOrderFieldWhatIf])
	}

	conn.processMessage(conn.encodeMsg(msgOpenOrder,
		"38", "77", "265598", "MSFT", "STK", "", "0", "", "1", "SMART", "USD", "", "MSFT",
		"BUY", "2", "LMT", "425.5", "0", "DAY",
		"1", "Submitted",
		"1000", "500", "10000", "25", "10", "-425.5",
		"1025", "510", "9574.5", "1.25", "1.25", "1.25", "USD",
		"USD", "1000", "500", "10000", "25", "10", "-425.5", "1025", "510", "9574.5", "0", "", "0", "",
	))

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("PreviewOrderWhatIf err = %v", got.err)
		}
		if got.result.Status != OrderWhatIfStatusAccepted || got.result.BrokerStatus != "Submitted" {
			t.Fatalf("result status = %+v, want accepted Submitted", got.result)
		}
		if got.result.Margin.Commission == nil || *got.result.Margin.Commission != 1.25 {
			t.Fatalf("commission = %v, want 1.25", got.result.Margin.Commission)
		}
	case <-time.After(time.Second):
		t.Fatal("PreviewOrderWhatIf did not return after matching openOrder")
	}
}

func TestPreviewOrderWhatIfModernServerSendsProtobufWhatIfAndWaitsForOpenOrder(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.nextOrderID = 77

	var buf bytes.Buffer
	conn.writer = bufio.NewWriter(&buf)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	type outcome struct {
		result OrderWhatIfResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := conn.PreviewOrderWhatIf(ctx, &IBKROrder{
			Symbol:    "MSFT",
			SecType:   "STK",
			Exchange:  "SMART",
			Currency:  "USD",
			Action:    "BUY",
			TotalQty:  2,
			OrderType: "LMT",
			LmtPrice:  425.50,
			TIF:       "DAY",
			Account:   "DU123456",
			OrderRef:  "preview-test",
			Transmit:  false,
		})
		done <- outcome{result: result, err: err}
	}()

	waitForWhatIfFrame(t, &buf)
	payload := extractFramePayload(t, &buf)
	if got := binary.BigEndian.Uint32(payload[:4]); got != uint32(protoPlaceOrderMsgID) {
		t.Fatalf("protobuf msgID = %d, want %d", got, protoPlaceOrderMsgID)
	}
	if bytes.Contains(payload, []byte("1.7976931348623157e+308")) {
		t.Fatalf("protobuf placeOrder payload contains ASCII max-float sentinel: %x", payload)
	}
	maxFloat := make([]byte, 8)
	binary.LittleEndian.PutUint64(maxFloat, math.Float64bits(math.MaxFloat64))
	if bytes.Contains(payload, maxFloat) {
		t.Fatalf("protobuf placeOrder payload contains binary max-float sentinel: %x", payload)
	}

	summary, err := parsePlaceOrderProtoSummary(payload[4:])
	if err != nil {
		t.Fatalf("parse protobuf placeOrder summary: %v", err)
	}
	if summary.orderID != 77 || summary.symbol != "MSFT" || summary.secType != "STK" {
		t.Fatalf("protobuf contract summary = %+v, want order 77 MSFT STK", summary)
	}
	if summary.action != "BUY" || summary.quantity != "2" || summary.orderType != "LMT" || summary.lmtPrice != 425.5 || summary.tif != "DAY" {
		t.Fatalf("protobuf order summary = %+v, want BUY 2 LMT 425.5 DAY", summary)
	}
	if summary.account != "DU123456" || summary.orderRef != "preview-test" {
		t.Fatalf("protobuf account/ref summary = %+v, want DU123456 preview-test", summary)
	}
	if !summary.whatIf || !summary.transmit {
		t.Fatalf("protobuf flags whatIf=%v transmit=%v, want true true", summary.whatIf, summary.transmit)
	}

	expected := loadHexFixture(t, "place_order_whatif_v203.hex")
	if !bytes.Equal(payload, expected) {
		t.Fatalf("protobuf placeOrder fixture mismatch\n got: %x\nwant: %x", payload, expected)
	}

	logFields := conn.decodeOutboundMessage(payload)
	if logFields[0] != strconv.Itoa(protoPlaceOrderMsgID) || logFields[1] != "protobuf" {
		t.Fatalf("outbound log fields = %#v, want protobuf summary", logFields)
	}
	if summaryFieldValue(logFields, "orderId=") != "77" || summaryFieldValue(logFields, "symbol=") != "MSFT" {
		t.Fatalf("outbound log fields missing order summary: %#v", logFields)
	}

	conn.processMessage(encodeOpenOrderProtoCallbackForTest(testOpenOrderProtoCallback{
		OrderID:            77,
		PermID:             987654,
		ClientID:           31,
		Symbol:             "MSFT",
		SecType:            "STK",
		Exchange:           "SMART",
		PrimaryExch:        "NASDAQ",
		Currency:           "USD",
		LocalSymbol:        "MSFT",
		TradingClass:       "MSFT",
		Action:             "BUY",
		Quantity:           "2",
		OrderType:          "LMT",
		LimitPrice:         425.5,
		TIF:                "DAY",
		Account:            "DU123456",
		OrderRef:           "preview-test",
		WhatIf:             true,
		Transmit:           true,
		Status:             "Submitted",
		InitMarginBefore:   1000,
		MaintMarginBefore:  500,
		EquityBefore:       10000,
		InitMarginAfter:    1025,
		MaintMarginAfter:   510,
		EquityAfter:        9574.5,
		Commission:         1.25,
		MinCommission:      1.25,
		MaxCommission:      1.25,
		CommissionCurrency: "USD",
		MarginCurrency:     "USD",
	}))

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("PreviewOrderWhatIf err = %v", got.err)
		}
		if got.result.Status != OrderWhatIfStatusAccepted || got.result.BrokerStatus != "Submitted" {
			t.Fatalf("result status = %+v, want accepted Submitted", got.result)
		}
	case <-time.After(time.Second):
		t.Fatal("PreviewOrderWhatIf did not return after matching openOrder")
	}
}

func TestEncodePlaceOrderProtoSupportsOptionClose(t *testing.T) {
	order := &IBKROrder{
		OrderID:      88,
		ClientID:     31,
		ConID:        123456,
		Symbol:       "SPY",
		SecType:      "OPT",
		Expiry:       "20260619",
		Strike:       520,
		Right:        "C",
		Multiplier:   "100",
		Exchange:     "SMART",
		Currency:     "USD",
		LocalSymbol:  "SPY  260619C00520000",
		TradingClass: "SPY",
		Action:       "BUY",
		TotalQty:     2,
		OrderType:    "LMT",
		LmtPrice:     2.18,
		TIF:          "DAY",
		Account:      "DU123456",
		OrderRef:     "purge-test",
		Transmit:     true,
		OpenClose:    "C",
	}
	payload, err := encodePlaceOrderProtoFrame(order)
	if err != nil {
		t.Fatalf("encodePlaceOrderProtoFrame option close: %v", err)
	}
	summary, err := parsePlaceOrderProtoSummary(payload[4:])
	if err != nil {
		t.Fatalf("parse option protobuf summary: %v", err)
	}
	if summary.conID != 123456 || summary.symbol != "SPY" || summary.secType != "OPT" ||
		summary.expiry != "20260619" || summary.strike != 520 || summary.right != "C" || summary.multiplier != "100" {
		t.Fatalf("option contract summary = %+v", summary)
	}
	if summary.action != "BUY" || summary.quantity != "2" || summary.orderType != "LMT" || summary.lmtPrice != 2.18 ||
		summary.tif != "DAY" || !summary.transmit || summary.openClose != "C" {
		t.Fatalf("option order summary = %+v", summary)
	}
}

func TestPreviewOrderWhatIfRejectsBrokerError(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder-1)
	conn.nextOrderID = 88

	var buf bytes.Buffer
	conn.writer = bufio.NewWriter(&buf)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan OrderWhatIfResult, 1)
	go func() {
		result, _ := conn.PreviewOrderWhatIf(ctx, &IBKROrder{
			Symbol:    "MSFT",
			SecType:   "STK",
			Exchange:  "SMART",
			Currency:  "USD",
			Action:    "BUY",
			TotalQty:  2,
			OrderType: "LMT",
			LmtPrice:  425.50,
			TIF:       "DAY",
			Account:   "DU123456",
		})
		done <- result
	}()

	waitForWhatIfFrame(t, &buf)
	conn.processMessage(conn.encodeMsg(msgErrMsg, "2", "88", "201", "Order rejected"))

	select {
	case got := <-done:
		if got.Status != OrderWhatIfStatusRejected {
			t.Fatalf("status = %q, want rejected: %+v", got.Status, got)
		}
		if got.Message == "" {
			t.Fatalf("rejected result missing message: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("PreviewOrderWhatIf did not return after broker error")
	}
}

func TestPreviewOrderWhatIfRejectsBrokerSystemNotice(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.nextOrderID = 91

	var buf bytes.Buffer
	conn.writer = bufio.NewWriter(&buf)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan OrderWhatIfResult, 1)
	go func() {
		result, _ := conn.PreviewOrderWhatIf(ctx, &IBKROrder{
			Symbol:    "MSFT",
			SecType:   "STK",
			Exchange:  "SMART",
			Currency:  "USD",
			Action:    "BUY",
			TotalQty:  2,
			OrderType: "LMT",
			LmtPrice:  425.50,
			TIF:       "DAY",
			Account:   "DU123456",
		})
		done <- result
	}()

	waitForWhatIfFrame(t, &buf)
	conn.processMessage(encodeSystemNotificationForTest(91, 321, "Error validating request.-'v' : cause - The API interface is currently in Read-Only mode.", `{"reason":"precautionary"}`))

	select {
	case got := <-done:
		if got.Status != OrderWhatIfStatusRejected {
			t.Fatalf("status = %q, want rejected: %+v", got.Status, got)
		}
		if !strings.Contains(got.Message, "321") || !strings.Contains(got.Message, "Read-Only") {
			t.Fatalf("rejected message = %q, want code 321 Read-Only", got.Message)
		}
		if got.AdvancedRejectJSON != `{"reason":"precautionary"}` || !strings.Contains(got.Message, "advanced_reject_json") {
			t.Fatalf("advanced reject = %q message=%q, want preserved JSON in message", got.AdvancedRejectJSON, got.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("PreviewOrderWhatIf did not return after broker system notice")
	}
}

func TestEncodePlaceOrderProtoRejectsUnsupportedPopulatedOrderField(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.nextOrderID = 77

	order := &IBKROrder{
		Symbol:    "MSFT",
		SecType:   "STK",
		Exchange:  "SMART",
		Currency:  "USD",
		Action:    "BUY",
		TotalQty:  2,
		OrderType: "LMT",
		LmtPrice:  425.50,
		TIF:       "DAY",
		Account:   "DU123456",
		OcaGroup:  "unsupported-oca",
	}
	if err := preparePlaceOrder(order, conn); err != nil {
		t.Fatalf("preparePlaceOrder err = %v", err)
	}

	_, err := encodePlaceOrderProtoFrame(order)
	if err == nil {
		t.Fatal("encodePlaceOrderProtoFrame succeeded with unsupported ocaGroup")
	}
	if !strings.Contains(err.Error(), "ocaGroup") {
		t.Fatalf("unsupported error = %v, want ocaGroup", err)
	}
}

func waitForWhatIfFrame(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if buf.Len() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for outbound whatIf frame")
}

func extractWhatIfPayloadFields(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	payload := extractFramePayload(t, buf)
	fields := make([]string, 0, 32)
	if len(payload) >= 4 {
		msgType := binary.BigEndian.Uint32(payload[:4])
		fields = append(fields, strconv.Itoa(int(msgType)))
		payload = payload[4:]
		if len(payload) > 0 && payload[0] == 0x00 {
			payload = payload[1:]
		}
	}
	parts := bytes.SplitSeq(payload, []byte{0})
	for part := range parts {
		fields = append(fields, string(part))
	}
	return fields
}

func extractFramePayload(t *testing.T, buf *bytes.Buffer) []byte {
	t.Helper()
	data := buf.Bytes()
	if len(data) < 4 {
		t.Fatalf("payload too short: %d bytes", len(data))
	}
	msgLen := binary.BigEndian.Uint32(data[:4])
	if uint32(len(data[4:])) < msgLen {
		t.Fatalf("payload length = %d, want at least %d", len(data[4:]), msgLen)
	}
	return data[4 : 4+msgLen]
}

func loadHexFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/wire/" + name)
	if err != nil {
		t.Fatalf("read hex fixture %s: %v", name, err)
	}
	compact := strings.Join(strings.Fields(string(raw)), "")
	decoded, err := hex.DecodeString(compact)
	if err != nil {
		t.Fatalf("decode hex fixture %s: %v", name, err)
	}
	return decoded
}

func encodeSystemNotificationForTest(reqID, code int, message, advancedRejectJSON string) []byte {
	var body []byte
	body = protoAppendInt64(body, 1, int64(reqID))
	body = protoAppendInt64(body, 2, time.Now().UnixMilli())
	body = protoAppendInt32(body, 3, int32(code))
	body = protoAppendString(body, 4, message)
	if advancedRejectJSON != "" {
		body = protoAppendString(body, 5, advancedRejectJSON)
	}

	var msg []byte
	msg = binary.BigEndian.AppendUint32(msg, uint32(msgSystemNotification))
	msg = append(msg, body...)
	return msg
}
