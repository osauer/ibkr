package ibkr

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnectionPlaceAndCancelOrderEncodesMessages(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.nextOrderID = 1

	var buf bytes.Buffer
	conn.writer = bufio.NewWriter(&buf)

	order := &IBKROrder{
		Symbol:    "AAPL",
		SecType:   "STK",
		Exchange:  "SMART",
		Currency:  "USD",
		Action:    "BUY",
		TotalQty:  100,
		OrderType: "LMT",
		LmtPrice:  180.25,
		TIF:       "DAY",
		Account:   "DU123456",
		OrderRef:  "test_ref",
	}

	require.NoError(t, conn.PlaceOrder(order))
	require.NotZero(t, order.OrderID)
	require.Greater(t, buf.Len(), 0)

	fields := extractPayloadFields(t, &buf)
	require.Equal(t, "3", fields[0])
	require.Equal(t, strconv.Itoa(order.OrderID), fields[1])
	require.Equal(t, "AAPL", fields[3])
	require.Equal(t, "STK", fields[4])
	require.Equal(t, "180.25", fields[19])
	require.Equal(t, "DU123456", fields[23])
	require.Equal(t, "1", fields[27]) // transmit flag
	require.Equal(t, "DAY", fields[21])

	conn.ordersMu.RLock()
	_, ok := conn.openOrders[order.OrderID]
	conn.ordersMu.RUnlock()
	require.True(t, ok, "order should be tracked after placement")

	buf.Reset()
	require.NoError(t, conn.CancelOrder(order.OrderID))
	cancelFields := extractPayloadFields(t, &buf)
	require.Equal(t, "4", cancelFields[0])
	require.Equal(t, strconv.Itoa(order.OrderID), cancelFields[1])

	conn.ordersMu.RLock()
	_, ok = conn.openOrders[order.OrderID]
	conn.ordersMu.RUnlock()
	require.False(t, ok, "order should be cleared after cancel request")
}

func TestPlaceOrderDoesNotSendDoubleMaxSentinels(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.nextOrderID = 1

	var buf bytes.Buffer
	conn.writer = bufio.NewWriter(&buf)

	order := &IBKROrder{
		Symbol:    "MSFT",
		SecType:   "STK",
		Exchange:  "SMART",
		Currency:  "USD",
		Action:    "BUY",
		TotalQty:  50,
		OrderType: "LMT",
		LmtPrice:  330.10,
		TIF:       "DAY",
	}

	require.NoError(t, conn.PlaceOrder(order))
	fields := extractPayloadFields(t, &buf)
	payload := strings.Join(fields, "\x00")

	if strings.Contains(payload, "1.7976931348623157e+308") {
		t.Fatalf("placeOrder payload should not contain double max sentinel, payload=%q", payload)
	}
}

func extractPayloadFields(t *testing.T, buf *bytes.Buffer) []string {
	data := buf.Bytes()
	require.GreaterOrEqual(t, len(data), 4)
	msgLen := binary.BigEndian.Uint32(data[:4])
	require.GreaterOrEqual(t, uint32(len(data[4:])), msgLen)
	payload := data[4 : 4+msgLen]
	fields := make([]string, 0, 32)
	if len(payload) >= 4 {
		msgType := binary.BigEndian.Uint32(payload[:4])
		fields = append(fields, strconv.Itoa(int(msgType)))
		payload = payload[4:]
		// Skip null terminator after message type if present
		if len(payload) > 0 && payload[0] == 0x00 {
			payload = payload[1:]
		}
	}
	parts := bytes.Split(payload, []byte{0})
	for _, part := range parts {
		fields = append(fields, string(part))
	}
	return fields
}
