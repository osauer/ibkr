package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
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

func TestServeIdleTimeoutTerminatesWithoutInputEOF(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	reader, writer := io.Pipe()
	defer writer.Close()

	srv := NewServer(nil, "test")
	done := make(chan error, 1)
	go func() {
		done <- srv.ServeWithOptions(context.Background(), reader, &out, ServeOptions{IdleTimeout: 20 * time.Millisecond})
	}()

	if err := waitServeDone(t, done); err != nil {
		t.Fatalf("ServeWithOptions: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Fatalf("idle timeout should not produce output, got %q", got)
	}
}

func TestServeIdleTimeoutResetsAfterRequest(t *testing.T) {
	t.Parallel()

	var out lockedBuffer
	reader, writer := io.Pipe()
	defer writer.Close()

	srv := NewServer(nil, "test")
	done := make(chan error, 1)
	go func() {
		done <- srv.ServeWithOptions(context.Background(), reader, &out, ServeOptions{IdleTimeout: 80 * time.Millisecond})
	}()

	time.Sleep(40 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("ServeWithOptions exited before activity reset idle timer: %v", err)
	default:
	}

	writeMCPLine(t, writer, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	waitForOutputContains(t, &out, `"id":1`)

	time.Sleep(40 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("ServeWithOptions exited too soon after activity reset idle timer: %v", err)
	default:
	}

	if err := waitServeDone(t, done); err != nil {
		t.Fatalf("ServeWithOptions: %v", err)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

func waitForOutputContains(t *testing.T, out interface{ String() string }, want string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("output did not contain %q:\n%s", want, out.String())
}
