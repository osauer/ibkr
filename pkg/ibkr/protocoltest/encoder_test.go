package protocoltest

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

const serverVersion = 203

func TestEncodeMessageVariants(t *testing.T) {
	var snapshots []string

	for _, tc := range SampleCases {
		withNull, err := EncodeMessage(serverVersion, true, tc.Fields...)
		if err != nil {
			t.Fatalf("%s with-null encode: %v", tc.Name, err)
		}
		withoutNull, err := EncodeMessage(serverVersion, false, tc.Fields...)
		if err != nil {
			t.Fatalf("%s without-null encode: %v", tc.Name, err)
		}

		if len(withNull) != len(withoutNull)+1 {
			t.Fatalf("%s length mismatch: with-null=%d without-null=%d", tc.Name, len(withNull), len(withoutNull))
		}

		msgType := binary.BigEndian.Uint32(withNull[:4])
		if msgType == 0 {
			t.Fatalf("%s message type decoded as zero", tc.Name)
		}
		if binary.BigEndian.Uint32(withoutNull[:4]) != msgType {
			t.Fatalf("%s message type mismatch between variants", tc.Name)
		}

		if withNull[4] != 0x00 {
			t.Fatalf("%s expected null terminator at index 4", tc.Name)
		}

		// Produce hex snapshots for later inspection.
		snapshots = append(snapshots, snapshotLine(tc.Name, withNull, withoutNull))
	}

	t.Log("\n" + strings.Join(snapshots, "\n"))
}

func TestHandshakeFrame(t *testing.T) {
	descriptor := "v100..203"
	frame := HandshakeFrame(descriptor)

	if len(frame) < 8 {
		t.Fatalf("handshake frame too short: %d", len(frame))
	}
	if string(frame[:4]) != "API\x00" {
		t.Fatalf("handshake prefix mismatch: %q", frame[:4])
	}

	length := binary.BigEndian.Uint32(frame[4:8])
	expectedLen := uint32(len(descriptor) + 1)
	if length != expectedLen {
		t.Fatalf("handshake length mismatch: got %d want %d", length, expectedLen)
	}

	payload := frame[8:]
	if len(payload) != int(expectedLen) {
		t.Fatalf("handshake payload length mismatch: got %d want %d", len(payload), expectedLen)
	}
	if payload[len(payload)-1] != 0x00 {
		t.Fatalf("handshake payload not null terminated")
	}

	t.Logf("handshake frame hex: %s", hex.EncodeToString(frame))
}

func snapshotLine(name string, withNull, withoutNull []byte) string {
	return name + "|" + hex.EncodeToString(withNull) + "|" + hex.EncodeToString(withoutNull)
}
