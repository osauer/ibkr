package ibkr

import (
	"bufio"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
)

func TestPlacePaperOrderRejectsLiveGateBeforeConnectionCheck(t *testing.T) {
	t.Parallel()
	conn := NewConnection(&ConnectionConfig{Host: "127.0.0.1", Port: 7496, ClientID: 31, Account: "U1234567"})

	err := conn.PlacePaperOrder(PaperOrderGate{
		Mode:     "live",
		Account:  "U1234567",
		Endpoint: "127.0.0.1:7496",
		Host:     "127.0.0.1",
		Port:     7496,
		ClientID: 31,
	}, &IBKROrder{})
	if err == nil || !strings.Contains(err.Error(), "mode=paper") {
		t.Fatalf("PlacePaperOrder err = %v, want paper-gate refusal", err)
	}
}

func TestCancelPaperOrderRejectsAggregateAccount(t *testing.T) {
	t.Parallel()
	conn := NewConnection(&ConnectionConfig{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "All"})

	err := conn.CancelPaperOrder(PaperOrderGate{
		Mode:     "paper",
		Account:  "All",
		Endpoint: "127.0.0.1:4002",
		Host:     "127.0.0.1",
		Port:     4002,
		ClientID: 31,
	}, 1001)
	if err == nil || !strings.Contains(err.Error(), "aggregate account") {
		t.Fatalf("CancelPaperOrder err = %v, want aggregate-account refusal", err)
	}
}

func TestCancelPaperOrderRejectsLiveAccountOnPaperPort(t *testing.T) {
	t.Parallel()
	conn := NewConnection(&ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 31, Account: "U1234567"})

	err := conn.CancelPaperOrder(PaperOrderGate{
		Mode:     "paper",
		Account:  "U1234567",
		Endpoint: "127.0.0.1:7497",
		Host:     "127.0.0.1",
		Port:     7497,
		ClientID: 31,
	}, 1001)
	if err == nil || !strings.Contains(err.Error(), "DU paper account") {
		t.Fatalf("CancelPaperOrder err = %v, want DU-account refusal", err)
	}
}

func TestCancelPaperOrderModernServerSendsProtobufCancel(t *testing.T) {
	conn := NewConnection(&ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU7654321"})
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)

	var buf safeBuffer
	conn.writer = bufio.NewWriter(&buf)

	err := conn.CancelPaperOrder(PaperOrderGate{
		Mode:     "paper",
		Account:  "DU7654321",
		Endpoint: "127.0.0.1:7497",
		Host:     "127.0.0.1",
		Port:     7497,
		ClientID: 41,
	}, 6)
	if err != nil {
		t.Fatalf("CancelPaperOrder err = %v", err)
	}

	payload := extractFramePayload(t, &buf)
	if got := binary.BigEndian.Uint32(payload[:4]); got != uint32(protoCancelOrderMsgID) {
		t.Fatalf("protobuf cancel msgID = %d, want %d", got, protoCancelOrderMsgID)
	}
	summary, err := parseCancelOrderProtoSummary(payload[4:])
	if err != nil {
		t.Fatalf("parse protobuf cancel summary: %v", err)
	}
	if summary.orderID != 6 || !summary.hasOrderCancel {
		t.Fatalf("protobuf cancel summary = %+v, want order 6 with OrderCancel", summary)
	}

	logFields := conn.decodeOutboundMessage(payload)
	if logFields[0] != strconv.Itoa(protoCancelOrderMsgID) || logFields[1] != "protobuf" {
		t.Fatalf("outbound log fields = %#v, want protobuf summary", logFields)
	}
	if summaryFieldValue(logFields, "orderId=") != "6" {
		t.Fatalf("outbound log fields missing order ID: %#v", logFields)
	}
}

func TestCancelPaperOrderLegacyV202OmitsDeprecatedVersionField(t *testing.T) {
	conn := NewConnection(&ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU7654321"})
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder-1)

	var buf safeBuffer
	conn.writer = bufio.NewWriter(&buf)

	err := conn.CancelPaperOrder(PaperOrderGate{
		Mode:     "paper",
		Account:  "DU7654321",
		Endpoint: "127.0.0.1:7497",
		Host:     "127.0.0.1",
		Port:     7497,
		ClientID: 41,
	}, 6)
	if err != nil {
		t.Fatalf("CancelPaperOrder err = %v", err)
	}

	payload := extractFramePayload(t, &buf)
	if got := binary.BigEndian.Uint32(payload[:4]); got != uint32(cancelOrder) {
		t.Fatalf("legacy cancel msgID = %d, want %d", got, cancelOrder)
	}
	fields := strings.Split(string(payload[4:]), "\x00")
	want := []string{"6", "", "", strconv.Itoa(maxProtoInt32), ""}
	if strings.Join(fields, "|") != strings.Join(want, "|") {
		t.Fatalf("legacy cancel fields = %#v, want %#v", fields, want)
	}
}
