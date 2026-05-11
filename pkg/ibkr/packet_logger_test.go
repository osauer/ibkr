package ibkr

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type stubPacketLogger struct {
	labels   []string
	payloads [][]byte
}

func (s *stubPacketLogger) Outbound(label string, payload []byte) {
	s.labels = append(s.labels, label)
	copyBuf := make([]byte, len(payload))
	copy(copyBuf, payload)
	s.payloads = append(s.payloads, copyBuf)
}

func TestDetermineMessageID(t *testing.T) {
	binPayload := make([]byte, 6)
	binary.BigEndian.PutUint32(binPayload[:4], 9)
	copy(binPayload[4:], []byte{'2', 0})
	if id := determineMessageID(203, binPayload); id != 9 {
		t.Fatalf("expected binary message id 9, got %d", id)
	}

	asciiPayload := []byte("17\x00123\x00")
	if id := determineMessageID(10, asciiPayload); id != 17 {
		t.Fatalf("expected ASCII message id 17, got %d", id)
	}

	garbage := []byte("abc")
	if id := determineMessageID(203, garbage); id != -1 {
		t.Fatalf("expected -1 for unparsable payload, got %d", id)
	}
}

func TestHexPacketLoggerWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "packets.log")
	logger, err := NewHexPacketLogger(path)
	if err != nil {
		t.Fatalf("NewHexPacketLogger: %v", err)
	}
	defer logger.Close()

	logger.Outbound("out msgID=1", []byte{0x00, 0x01})
	if err := logger.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	contents := string(data)
	if !strings.Contains(contents, "out msgID=1") || !strings.Contains(contents, "0001") {
		t.Fatalf("log contents unexpected: %q", contents)
	}
}

func TestConnectionPacketLoggerIntegration(t *testing.T) {
	c := NewConnection(nil)
	setServerVersionReady(c, 203)

	stub := &stubPacketLogger{}
	c.SetPacketLogger(stub)

	payload := c.encodeMsg(89, "1")
	c.logPacketOutbound(payload)

	if len(stub.labels) != 1 {
		t.Fatalf("expected 1 label, got %d", len(stub.labels))
	}
	if stub.labels[0] != "out msgID=89" {
		t.Fatalf("expected label with msgID=89, got %q", stub.labels[0])
	}
	if len(stub.payloads[0]) != len(payload) {
		t.Fatalf("payload length mismatch")
	}
	if &stub.payloads[0][0] == &payload[0] {
		t.Fatalf("expected payload copy, buffers alias")
	}
}
