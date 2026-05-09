package ibkr

import (
	"os"
	"strings"
	"testing"
)

// setServerVersionReady configures a connection with the given server version
// and marks the handshake channel as ready so unit tests can exercise outbound
// encoding paths without blocking on the async handshake lifecycle.
func setServerVersionReady(c *Connection, version int) {
	c.serverVersion = version
	c.signalHandshakeReady()
}

// skipIfLiveTrading skips integration tests that would touch live IBKR endpoints
// whenever the environment indicates live trading mode.
func skipIfLiveTrading(t *testing.T) {
	t.Helper()
	if isLiveTradingEnv() {
		t.Skip("Skipping IBKR integration test while FEATURES_LIVE_MODE indicates live trading")
	}
}

func isLiveTradingEnv() bool {
	if strings.EqualFold(os.Getenv("FEATURES_LIVE_MODE"), "true") {
		return true
	}
	cfg := os.Getenv("REGIME_CONFIG_FILE")
	if strings.Contains(strings.ToLower(cfg), "live") {
		return true
	}
	cfg = os.Getenv("CONFIG_FILE")
	return strings.Contains(strings.ToLower(cfg), "live")
}
