package ibkr

import (
	"net"
	"testing"
	"time"
)

// TestGatewayConnectivity tests basic TCP connectivity to the Gateway
func TestGatewayConnectivity(t *testing.T) {
	// Try to establish a TCP connection
	conn, err := net.DialTimeout("tcp", "127.0.0.1:4001", 2*time.Second)
	if err != nil {
		t.Skipf("Cannot connect to Gateway on port 4001: %v (skipping)", err)
	}
	defer conn.Close()

	t.Log("✅ TCP connection to Gateway successful")

	// Try to send the API version string
	apiVersion := "v176"
	_, err = conn.Write([]byte(apiVersion + "\x00"))
	if err != nil {
		t.Fatalf("Failed to write API version: %v", err)
	}

	t.Log("✅ Sent API version to Gateway")

	// Set a read timeout
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	// Try to read response
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Logf("⚠️  Gateway did not respond (might need API access enabled): %v", err)
		t.Log("Make sure IB Gateway has:")
		t.Log("  1. API access enabled")
		t.Log("  2. 'Read-Only API' disabled")
		t.Log("  3. Trusted IP set to 127.0.0.1")
		t.Log("  4. Socket port set to 4001")
		return
	}

	t.Logf("✅ Gateway responded with %d bytes", n)
	t.Logf("Response (hex): %x", buf[:n])
}
