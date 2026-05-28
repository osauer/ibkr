package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestServeShutdownTerminatesWithoutInputEOF(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	done, closeInput := servePipe(t, &out)
	writeMCPLine(t, closeInput.writer, `{"jsonrpc":"2.0","id":7,"method":"shutdown"}`)

	if err := waitServeDone(t, done); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	closeInput.close()

	var resp struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      json.RawMessage  `json:"id"`
		Result  *json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode response: %v\n%s", err, out.String())
	}
	if resp.JSONRPC != "2.0" || string(resp.ID) != "7" || resp.Result == nil {
		t.Fatalf("shutdown response = %s", out.String())
	}
}

func TestServeExitNotificationTerminatesWithoutInputEOF(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	done, closeInput := servePipe(t, &out)
	writeMCPLine(t, closeInput.writer, `{"jsonrpc":"2.0","method":"exit"}`)

	if err := waitServeDone(t, done); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	closeInput.close()

	if got := strings.TrimSpace(out.String()); got != "" {
		t.Fatalf("exit notification should not produce a response, got %q", got)
	}
}

type pipeCloser struct {
	writer *io.PipeWriter
}

func (p pipeCloser) close() {
	_ = p.writer.Close()
}

func servePipe(t *testing.T, out *bytes.Buffer) (<-chan error, pipeCloser) {
	t.Helper()

	reader, writer := io.Pipe()
	srv := NewServer(nil, "test")
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(context.Background(), reader, out)
	}()
	return done, pipeCloser{writer: writer}
}

func writeMCPLine(t *testing.T, w *io.PipeWriter, line string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte(line + "\n"))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write MCP line: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("write MCP line blocked")
	}
}

func waitServeDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Serve did not exit after MCP lifecycle message while stdin stayed open")
		return nil
	}
}
