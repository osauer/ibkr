package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"testing"
	"time"
)

func TestPreviewOrderWhatIfSendsNonTransmittingWhatIfAndWaitsForOpenOrder(t *testing.T) {
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
	fields := extractWhatIfPayloadFields(t, &buf)
	if fields[27] != "0" {
		t.Fatalf("transmit field = %q, want 0", fields[27])
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

func TestPreviewOrderWhatIfRejectsBrokerError(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
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
	data := buf.Bytes()
	if len(data) < 4 {
		t.Fatalf("payload too short: %d bytes", len(data))
	}
	msgLen := binary.BigEndian.Uint32(data[:4])
	if uint32(len(data[4:])) < msgLen {
		t.Fatalf("payload length = %d, want at least %d", len(data[4:]), msgLen)
	}
	payload := data[4 : 4+msgLen]
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
