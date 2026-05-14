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

// TestPlaceOrderDoesNotSendDoubleMaxSentinels pins the v0.7-era regression
// where unset numeric fields ended up as `MAX_FLOAT` strings on the wire.
// The bundled `ibkr` binary refuses every order verb at the daemon
// dispatcher, so this is the one wire-shape contract worth keeping for
// library callers (downstream forks, clean-room ports) who may genuinely
// drive Connection.PlaceOrder.
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
	parts := bytes.SplitSeq(payload, []byte{0})
	for part := range parts {
		fields = append(fields, string(part))
	}
	return fields
}
