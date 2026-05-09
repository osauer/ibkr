package ibkr

import (
	"net"
	"os"
	"testing"
	"time"
)

// TestDebugHandshake tests the exact handshake protocol
func TestDebugHandshake(t *testing.T) {
	addr := ibkrTestAddr()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Skipf("Cannot connect: %v (skipping)", err)
	}
	defer conn.Close()

	t.Log("Connected to Gateway")

	// Try different handshake approaches
	tests := []struct {
		name string
		send func() error
	}{
		{
			name: "Frame v100..203",
			send: func() error {
				_, err := conn.Write(buildHandshakeFrame("v100..203"))
				return err
			},
		},
		{
			name: "Frame v203",
			send: func() error {
				_, err := conn.Write(buildHandshakeFrame("v203"))
				return err
			},
		},
		{
			name: "Legacy raw v203",
			send: func() error {
				_, err := conn.Write([]byte("API\x00v203\x00"))
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr := ibkrTestAddr()
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Skipf("Cannot connect: %v (skipping)", err)
			}
			defer conn.Close()

			if err := tc.send(); err != nil {
				t.Fatalf("Failed to send: %v", err)
			}

			// Try to read response
			conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			buf := make([]byte, 1024)
			n, err := conn.Read(buf)
			if err != nil {
				t.Logf("No response: %v", err)
			} else {
				t.Logf("Got response: %d bytes", n)
				t.Logf("Response (hex): %x", buf[:n])
				t.Logf("Response (string): %q", buf[:n])
			}
		})
	}
}

func ibkrTestAddr() string {
	host := os.Getenv("IBKR_TEST_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("IBKR_TEST_PORT")
	if port == "" {
		port = "4002"
	}
	return net.JoinHostPort(host, port)
}
