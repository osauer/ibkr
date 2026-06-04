package ibkr

import (
	"encoding/binary"
	"strconv"
	"testing"
)

type testOpenOrderProtoCallback struct {
	OrderID            int
	PermID             int
	ClientID           int
	Symbol             string
	SecType            string
	Exchange           string
	PrimaryExch        string
	Currency           string
	LocalSymbol        string
	TradingClass       string
	Action             string
	Quantity           string
	OrderType          string
	LimitPrice         float64
	TIF                string
	Account            string
	OrderRef           string
	OutsideRth         bool
	WhatIf             bool
	Transmit           bool
	Status             string
	InitMarginBefore   float64
	MaintMarginBefore  float64
	EquityBefore       float64
	InitMarginAfter    float64
	MaintMarginAfter   float64
	EquityAfter        float64
	Commission         float64
	MinCommission      float64
	MaxCommission      float64
	CommissionCurrency string
	MarginCurrency     string
	RejectReason       string
	WarningText        string
}

func TestDecodeMessageV203OpenOrderProtoCallback(t *testing.T) {
	t.Parallel()
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)

	fields := conn.decodeMessage(encodeOpenOrderProtoCallbackForTest(testOpenOrderProtoCallback{
		OrderID:            77,
		PermID:             987654,
		ClientID:           31,
		Symbol:             "MBG",
		SecType:            "STK",
		Exchange:           "SMART",
		PrimaryExch:        "IBIS",
		Currency:           "EUR",
		LocalSymbol:        "MBG",
		TradingClass:       "XETRA",
		Action:             "BUY",
		Quantity:           "1",
		OrderType:          "LMT",
		LimitPrice:         45,
		TIF:                "DAY",
		Account:            "DU123456",
		OrderRef:           "ibkr-test",
		WhatIf:             true,
		Transmit:           true,
		Status:             "PreSubmitted",
		InitMarginBefore:   1000,
		MaintMarginBefore:  500,
		EquityBefore:       10000,
		InitMarginAfter:    1045,
		MaintMarginAfter:   522.5,
		EquityAfter:        9955,
		Commission:         1.25,
		MinCommission:      1.25,
		MaxCommission:      1.25,
		CommissionCurrency: "EUR",
		MarginCurrency:     "EUR",
	}))
	if fields[0] != strconv.Itoa(msgOpenOrder) || fields[1] != "protobuf" || summaryFieldValue(fields, "wire_msg_id=") != strconv.Itoa(protoOpenOrderMsgID) {
		t.Fatalf("decoded fields = %#v, want openOrder protobuf summary", fields)
	}
	result, ok := parseOrderWhatIfOpenOrder(fields, 77, minServerVerProtoBufPlaceOrder)
	if !ok {
		t.Fatalf("parseOrderWhatIfOpenOrder ok=false for fields %#v", fields)
	}
	if result.Status != OrderWhatIfStatusAccepted || result.BrokerStatus != "PreSubmitted" {
		t.Fatalf("WhatIf result = %+v, want accepted PreSubmitted", result)
	}
	if result.Margin.Commission == nil || *result.Margin.Commission != 1.25 || result.Margin.Currency != "EUR" {
		t.Fatalf("margin = %+v, want EUR commission 1.25", result.Margin)
	}

	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok {
		t.Fatalf("ParseOrderLifecycleEvent ok=false for fields %#v", fields)
	}
	if ev.Type != OrderLifecycleEventOpenOrder || ev.OrderID != 77 || ev.Symbol != "MBG" || ev.Action != "BUY" || ev.TotalQuantity != 1 || ev.Account != "DU123456" || ev.OrderRef != "ibkr-test" {
		t.Fatalf("event = %+v, want MBG open order details", ev)
	}
	if !ev.WhatIf {
		t.Fatalf("event = %+v, want WhatIf marker preserved", ev)
	}
}

func TestDecodeMessageV203OrderStatusProtoCallback(t *testing.T) {
	t.Parallel()
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)

	fields := conn.decodeMessage(encodeOrderStatusProtoCallbackForTest(88, "Submitted", "0", "1"))
	if fields[0] != strconv.Itoa(msgOrderStatus) || fields[1] != "protobuf" || summaryFieldValue(fields, "wire_msg_id=") != strconv.Itoa(protoOrderStatusMsgID) {
		t.Fatalf("decoded fields = %#v, want orderStatus protobuf summary", fields)
	}
	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok {
		t.Fatalf("ParseOrderLifecycleEvent ok=false for fields %#v", fields)
	}
	if ev.Type != OrderLifecycleEventStatus || ev.OrderID != 88 || ev.Status != "Submitted" || ev.Filled != 0 || ev.Remaining != 1 || ev.PermID != 555666 || ev.ClientID != 41 {
		t.Fatalf("event = %+v, want order status details", ev)
	}
	if ev.WhyHeld != "held for review" || ev.MktCapPrice != 44.5 {
		t.Fatalf("event diagnostics = %+v, want held reason and cap price", ev)
	}
}

func TestDecodeMessageV203ExecDetailsProtoCallback(t *testing.T) {
	t.Parallel()
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)

	fields := conn.decodeMessage(encodeExecDetailsProtoCallbackForTest())
	if fields[0] != strconv.Itoa(msgExecDetails) || fields[1] != "protobuf" || summaryFieldValue(fields, "wire_msg_id=") != strconv.Itoa(protoExecDetailsMsgID) {
		t.Fatalf("decoded fields = %#v, want execDetails protobuf summary", fields)
	}
	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok {
		t.Fatalf("ParseOrderLifecycleEvent ok=false for fields %#v", fields)
	}
	if ev.Type != OrderLifecycleEventExecDetails || ev.OrderID != 88 || ev.Symbol != "MBG" || ev.ExecID != "0000e1.test" || ev.Shares != 1 || ev.Price != 45.25 || ev.OrderRef != "ibkr-test" {
		t.Fatalf("event = %+v, want execution details", ev)
	}
}

func TestParseSystemNotificationPayloadPreservesAdvancedRejectJSON(t *testing.T) {
	t.Parallel()
	var body []byte
	body = protoAppendInt64(body, 1, 1001)
	body = protoAppendInt32(body, 3, 201)
	body = protoAppendString(body, 4, "Order rejected by precautionary settings")
	body = protoAppendString(body, 5, `{"reason":"size"}`)

	note, err := parseSystemNotificationPayload(body)
	if err != nil {
		t.Fatalf("parseSystemNotificationPayload: %v", err)
	}
	if note.tickerID != 1001 || note.code != 201 || note.message != "Order rejected by precautionary settings" || note.advancedOrderRejectJSON != `{"reason":"size"}` {
		t.Fatalf("note = %+v, want advanced reject details", note)
	}
}

func encodeOpenOrderProtoCallbackForTest(f testOpenOrderProtoCallback) []byte {
	contract := encodeContractProtoForTest(f.Symbol, f.SecType, f.Exchange, f.PrimaryExch, f.Currency, f.LocalSymbol, f.TradingClass)

	var order []byte
	order = protoAppendInt32(order, 1, int32(f.ClientID))
	order = protoAppendInt32(order, 2, int32(f.OrderID))
	order = protoAppendInt64(order, 3, int64(f.PermID))
	order = protoAppendString(order, 5, f.Action)
	order = protoAppendString(order, 6, f.Quantity)
	order = protoAppendString(order, 8, f.OrderType)
	order = protoAppendDouble(order, 9, f.LimitPrice)
	order = protoAppendString(order, 11, f.TIF)
	order = protoAppendString(order, 12, f.Account)
	if f.OutsideRth {
		order = protoAppendBool(order, 19, true)
	}
	order = protoAppendString(order, 28, f.OrderRef)
	if f.WhatIf {
		order = protoAppendBool(order, 65, true)
	}
	if f.Transmit {
		order = protoAppendBool(order, 66, true)
	}

	var state []byte
	state = protoAppendString(state, 1, f.Status)
	state = protoAppendDouble(state, 2, f.InitMarginBefore)
	state = protoAppendDouble(state, 3, f.MaintMarginBefore)
	state = protoAppendDouble(state, 4, f.EquityBefore)
	state = protoAppendDouble(state, 8, f.InitMarginAfter)
	state = protoAppendDouble(state, 9, f.MaintMarginAfter)
	state = protoAppendDouble(state, 10, f.EquityAfter)
	state = protoAppendDouble(state, 11, f.Commission)
	state = protoAppendDouble(state, 12, f.MinCommission)
	state = protoAppendDouble(state, 13, f.MaxCommission)
	state = protoAppendString(state, 14, f.CommissionCurrency)
	state = protoAppendString(state, 15, f.MarginCurrency)
	state = protoAppendString(state, 26, f.RejectReason)
	state = protoAppendString(state, 28, f.WarningText)

	var body []byte
	body = protoAppendInt32(body, 1, int32(f.OrderID))
	body = protoAppendMessage(body, 2, contract)
	body = protoAppendMessage(body, 3, order)
	body = protoAppendMessage(body, 4, state)
	return encodeProtoCallbackFrameForTest(protoOpenOrderMsgID, body)
}

func encodeOrderStatusProtoCallbackForTest(orderID int, status, filled, remaining string) []byte {
	var body []byte
	body = protoAppendInt32(body, 1, int32(orderID))
	body = protoAppendString(body, 2, status)
	body = protoAppendString(body, 3, filled)
	body = protoAppendString(body, 4, remaining)
	body = protoAppendDouble(body, 5, 45.25)
	body = protoAppendInt64(body, 6, 555666)
	body = protoAppendDouble(body, 8, 45.25)
	body = protoAppendInt32(body, 9, 41)
	body = protoAppendString(body, 10, "held for review")
	body = protoAppendDouble(body, 11, 44.5)
	return encodeProtoCallbackFrameForTest(protoOrderStatusMsgID, body)
}

func encodeExecDetailsProtoCallbackForTest() []byte {
	contract := encodeContractProtoForTest("MBG", "STK", "SMART", "IBIS", "EUR", "MBG", "XETRA")

	var exec []byte
	exec = protoAppendInt32(exec, 1, 88)
	exec = protoAppendString(exec, 2, "0000e1.test")
	exec = protoAppendString(exec, 3, "20260604 09:31:02")
	exec = protoAppendString(exec, 4, "DU123456")
	exec = protoAppendString(exec, 5, "IBIS")
	exec = protoAppendString(exec, 6, "BOT")
	exec = protoAppendString(exec, 7, "1")
	exec = protoAppendDouble(exec, 8, 45.25)
	exec = protoAppendInt64(exec, 9, 555666)
	exec = protoAppendInt32(exec, 10, 41)
	exec = protoAppendString(exec, 12, "1")
	exec = protoAppendDouble(exec, 13, 45.25)
	exec = protoAppendString(exec, 14, "ibkr-test")

	var body []byte
	body = protoAppendInt32(body, 1, -1)
	body = protoAppendMessage(body, 2, contract)
	body = protoAppendMessage(body, 3, exec)
	return encodeProtoCallbackFrameForTest(protoExecDetailsMsgID, body)
}

func encodeContractProtoForTest(symbol, secType, exchange, primaryExch, currency, localSymbol, tradingClass string) []byte {
	var contract []byte
	contract = protoAppendString(contract, 2, symbol)
	contract = protoAppendString(contract, 3, secType)
	contract = protoAppendString(contract, 8, exchange)
	contract = protoAppendString(contract, 9, primaryExch)
	contract = protoAppendString(contract, 10, currency)
	contract = protoAppendString(contract, 11, localSymbol)
	contract = protoAppendString(contract, 12, tradingClass)
	return contract
}

func encodeProtoCallbackFrameForTest(msgID int, body []byte) []byte {
	var msg []byte
	msg = binary.BigEndian.AppendUint32(msg, uint32(msgID))
	return append(msg, body...)
}
