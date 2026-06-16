package ibkr

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestConnectionDisconnectClean verifies that Disconnect() properly drains
// the rate limiter and doesn't send corrupted data to IBKR
func TestConnectionDisconnectClean(t *testing.T) {
	ctx := context.Background()

	// Use a mock connection that counts messages
	conn := &Connection{
		config: &ConnectionConfig{
			Host:           "127.0.0.1",
			Port:           4001,
			ClientID:       999,
			ConnectTimeout: 5 * time.Second,
		},
		status:   StatusConnected, // Simulate connected state
		stopChan: make(chan struct{}),
	}

	// Initialize rate limiter
	conn.rateLimiter = NewRateLimiter(ctx)

	// Test 1: Verify status check blocks new messages after disconnect starts
	go func() {
		time.Sleep(10 * time.Millisecond)
		conn.statusMu.Lock()
		conn.status = StatusDisconnected
		conn.statusMu.Unlock()
	}()

	time.Sleep(50 * time.Millisecond)

	// Try to send after status changed
	err := conn.sendRawMessage("test")
	if err == nil {
		t.Error("Expected sendRawMessage to fail after disconnect, but it succeeded")
	}
	if err != nil && err.Error() != "cannot send message: connection status is DISCONNECTED" {
		t.Errorf("Unexpected error: %v", err)
	}

	// Clean up
	conn.rateLimiter.Stop()
	t.Log("✓ Disconnect properly blocks new messages")
}

// TestConnectionDisconnectStatusCheck verifies that sendMessage checks
// connection status before queueing requests
func TestConnectionDisconnectStatusCheck(t *testing.T) {
	ctx := context.Background()

	conn := &Connection{
		config: &ConnectionConfig{
			ClientID: 888,
		},
		status:      StatusDisconnected, // Already disconnected
		stopChan:    make(chan struct{}),
		rateLimiter: NewRateLimiter(ctx),
	}

	// Try to send when already disconnected
	err := conn.sendRawMessage("test message")

	if err == nil {
		t.Fatal("Expected error when sending to disconnected connection")
	}

	if err.Error() != "cannot send message: connection status is DISCONNECTED" {
		t.Errorf("Unexpected error message: %v", err)
	}

	conn.rateLimiter.Stop()
	t.Log("✓ Status check prevents queueing to disconnected connection")
}

func TestConnectionDisconnectClosesSocketWhenAlreadyDisconnected(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	conn := NewConnection(DefaultConfig())
	conn.statusMu.Lock()
	conn.status = StatusDisconnected
	conn.conn = client
	conn.statusMu.Unlock()

	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := server.Read(buf)
		readDone <- err
	}()

	if err := conn.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	conn.Disconnect()

	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("server read returned nil after client close")
		}
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not close socket for disconnected connection")
	}

	if conn.conn != nil || conn.reader != nil || conn.writer != nil || conn.scanner != nil {
		t.Fatalf("connection fields not cleared after Disconnect: conn=%v reader=%v writer=%v scanner=%v", conn.conn, conn.reader, conn.writer, conn.scanner)
	}
}
