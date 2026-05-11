package ibkr

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

func TestHandshakeParsesLengthPrefixedServerResponse(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	conn := &Connection{
		config: &ConnectionConfig{ClientID: 42},
		conn:   client,
		reader: bufio.NewReader(client),
		writer: bufio.NewWriter(client),
	}

	errCh := make(chan error, 1)
	go func() {
		expected := buildHandshakeFrame("v100..203")
		buf := make([]byte, len(expected))
		if _, err := io.ReadFull(server, buf); err != nil {
			errCh <- err
			return
		}
		if !bytes.Equal(buf, expected) {
			errCh <- fmt.Errorf("unexpected handshake payload: got %x", buf)
			return
		}

		ack := buildHandshakeAck("131", "20250922 12:34:56")
		if _, err := server.Write(ack); err != nil {
			errCh <- err
			return
		}

		errCh <- nil
	}()

	if err := conn.handshake(); err != nil {
		t.Fatalf("handshake failed: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("handshake goroutine error: %v", err)
	}

	if conn.serverVersion != 131 {
		t.Fatalf("expected serverVersion 131, got %d", conn.serverVersion)
	}

	if conn.connTime != "20250922 12:34:56" {
		t.Fatalf("expected connTime '20250922 12:34:56', got %q", conn.connTime)
	}
}

func TestHandshakeAcceptsAsciiServerResponse(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	conn := &Connection{
		config: &ConnectionConfig{ClientID: 7},
		conn:   client,
		reader: bufio.NewReader(client),
		writer: bufio.NewWriter(client),
	}

	errCh := make(chan error, 1)
	go func() {
		expected := buildHandshakeFrame("v100..203")
		buf := make([]byte, len(expected))
		if _, err := io.ReadFull(server, buf); err != nil {
			errCh <- err
			return
		}
		if !bytes.Equal(buf, expected) {
			errCh <- fmt.Errorf("unexpected handshake payload: got %x", buf)
			return
		}

		if _, err := server.Write([]byte("176\x0020250922 09:00:00\x00")); err != nil {
			errCh <- err
			return
		}

		errCh <- nil
	}()

	if err := conn.handshake(); err != nil {
		t.Fatalf("handshake failed: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("handshake goroutine error: %v", err)
	}

	if conn.serverVersion != 176 {
		t.Fatalf("expected serverVersion 176, got %d", conn.serverVersion)
	}
	if conn.connTime != "20250922 09:00:00" {
		t.Fatalf("expected connTime '20250922 09:00:00', got %q", conn.connTime)
	}
}

func TestHandshakeRejectsOldServerVersion(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	conn := &Connection{
		config: &ConnectionConfig{ClientID: 3},
		conn:   client,
		reader: bufio.NewReader(client),
		writer: bufio.NewWriter(client),
	}

	errCh := make(chan error, 1)
	go func() {
		expected := buildHandshakeFrame("v100..203")
		buf := make([]byte, len(expected))
		if _, err := io.ReadFull(server, buf); err != nil {
			errCh <- err
			return
		}
		if !bytes.Equal(buf, expected) {
			errCh <- fmt.Errorf("unexpected handshake payload: got %x", buf)
			return
		}

		if _, err := server.Write([]byte("80\x0020250922 09:00:00\x00")); err != nil {
			errCh <- err
			return
		}
		server.Close()
		errCh <- nil
	}()

	err := conn.handshake()
	if err == nil {
		t.Fatalf("expected handshake failure for old server version")
	}
	if !strings.Contains(err.Error(), "too old") {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("handshake goroutine error: %v", err)
	}
}

func TestHandshakeFailsWhenServerSilentEvenWithPermissiveEnv(t *testing.T) {
	t.Setenv("IBKR_HANDSHAKE_PERMISSIVE", "true")

	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	conn := &Connection{
		config: &ConnectionConfig{ClientID: 9},
		conn:   client,
		reader: bufio.NewReader(client),
		writer: bufio.NewWriter(client),
	}

	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		expected := buildHandshakeFrame("v100..203")
		buf := make([]byte, len(expected))
		if _, err := io.ReadFull(server, buf); err != nil {
			errCh <- err
			return
		}
		if !bytes.Equal(buf, expected) {
			errCh <- fmt.Errorf("unexpected handshake payload: got %x", buf)
			return
		}
		if err := server.Close(); err != nil {
			errCh <- err
			return
		}
		errCh <- io.EOF
	}()

	err := conn.handshake()
	if err == nil {
		t.Fatalf("expected handshake failure when server provides no response")
	}
	if !strings.Contains(err.Error(), "no response") && !strings.Contains(err.Error(), "read/write on closed pipe") {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := <-errCh; err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("handshake goroutine error: %v", err)
	}
}

func buildHandshakeFrame(version string) []byte {
	descriptorBytes := append([]byte(version), '\x00')
	frame := make([]byte, 0, 4+4+len(descriptorBytes))
	frame = append(frame, 'A', 'P', 'I', '\x00')
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(descriptorBytes)))
	frame = append(frame, lenBuf[:]...)
	frame = append(frame, descriptorBytes...)
	return frame
}

func buildHandshakeAck(fields ...string) []byte {
	var payload bytes.Buffer
	for _, field := range fields {
		payload.WriteString(field)
		payload.WriteByte('\x00')
	}

	body := payload.Bytes()
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	return frame
}
