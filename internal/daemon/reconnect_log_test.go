package daemon

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/discover"
)

// These tests pin the reconnect-retry log-volume fix: the connect-failure
// verdicts in connectWithFailover / tryOneHandshake now log once on the
// transition and demote identical repeats to Debug. While the gateway is down
// the daemon rebuilds the connector every cycle; each cycle used to re-emit the
// same "gateway not connected" / "no endpoint usable" WARN lines, flooding
// ibkr-daemon.log (~50k lines over a 13.5h off-hours window). Follow-up #3 to
// the order-status log dedupe (project_daily_pnl_freeze_2026_07_01).
//
// NewLogger installs a process-global pkg/ibkr sink, so these do not call
// t.Parallel(); they assert on their own daemon-logger buffer, which the fake
// attempterFactory keeps free of any pkg/ibkr connector output.

// countLevelLines counts slog text lines at the given level containing needle.
func countLevelLines(buf *bytes.Buffer, level, needle string) int {
	n := 0
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "level="+level) && strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

func failingFailoverServer(buf *bytes.Buffer) *Server {
	srv := &Server{
		logger:  NewLogger(buf, "debug"),
		streams: map[string]context.CancelFunc{},
	}
	srv.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		return &fakeAttempter{
			port:      ep.Port,
			connectOk: false,
			lastError: "dial tcp 127.0.0.1:7496: connect: connection refused",
		}
	}
	return srv
}

// TestConnectWithFailover_UnreachableVerdictDedupes drives three reconnect
// cycles against a down single-endpoint gateway and asserts each verdict
// surfaces once at WARN and then rides at Debug.
func TestConnectWithFailover_UnreachableVerdictDedupes(t *testing.T) {
	var buf bytes.Buffer
	srv := failingFailoverServer(&buf)
	primary := discover.Endpoint{Host: "127.0.0.1", Port: 7496, ClientID: 15}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for range 3 {
		srv.connectWithFailover(ctx, primary)
	}

	const gwVerdict = "Daemon up but gateway not connected"
	if got := countLevelLines(&buf, "WARN", gwVerdict); got != 1 {
		t.Fatalf("gateway-unreachable verdict logged %d WARN lines over 3 cycles, want exactly 1\n%s", got, buf.String())
	}
	if got := countLevelLines(&buf, "DEBUG", gwVerdict); got != 2 {
		t.Fatalf("gateway-unreachable verdict logged %d DEBUG repeats over 3 cycles, want 2\n%s", got, buf.String())
	}

	// The single-endpoint exhaustion verdict dedupes the same way.
	const exhaustion = "Daemon up but no endpoint usable"
	if got := countLevelLines(&buf, "WARN", exhaustion); got != 1 {
		t.Fatalf("no-endpoint-usable verdict logged %d WARN lines over 3 cycles, want exactly 1\n%s", got, buf.String())
	}
}

// TestConnectWithFailover_RecoveryResetsAndReArms pins the episode lifecycle:
// an outage logs one WARN, a successful handshake logs the recovery bookend and
// clears the dedupe, and a fresh outage then logs a new WARN transition.
func TestConnectWithFailover_RecoveryResetsAndReArms(t *testing.T) {
	var buf bytes.Buffer
	down := true
	srv := &Server{
		logger:  NewLogger(&buf, "debug"),
		streams: map[string]context.CancelFunc{},
	}
	srv.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		return &fakeAttempter{
			port:      ep.Port,
			connectOk: !down,
			lastError: "dial tcp 127.0.0.1:7496: connect: connection refused",
		}
	}
	primary := discover.Endpoint{Host: "127.0.0.1", Port: 7496, ClientID: 15}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Outage: two failing cycles → one WARN transition.
	srv.connectWithFailover(ctx, primary)
	srv.connectWithFailover(ctx, primary)
	if got := countLevelLines(&buf, "WARN", "Daemon up but gateway not connected"); got != 1 {
		t.Fatalf("outage should log exactly 1 WARN transition, got %d\n%s", got, buf.String())
	}

	// Gateway returns: success logs the recovery bookend and resets the latch.
	down = false
	srv.connectWithFailover(ctx, primary)
	if !strings.Contains(buf.String(), "Gateway reachable again") {
		t.Fatalf("recovery must log the bookend after an outage:\n%s", buf.String())
	}

	// A second, distinct outage must log a fresh WARN — the reset re-armed it.
	down = true
	buf.Reset()
	srv.connectWithFailover(ctx, primary)
	if got := countLevelLines(&buf, "WARN", "Daemon up but gateway not connected"); got != 1 {
		t.Fatalf("second outage after recovery must log a fresh WARN, got %d\n%s", got, buf.String())
	}
}

// TestPostConnectSetup_NoBookendOnCleanFirstConnect guards against a spurious
// "reachable again" line when the daemon connects without any prior outage.
func TestPostConnectSetup_NoBookendOnCleanFirstConnect(t *testing.T) {
	var buf bytes.Buffer
	srv := &Server{
		logger:  NewLogger(&buf, "debug"),
		streams: map[string]context.CancelFunc{},
	}
	ep := discover.Endpoint{Host: "127.0.0.1", Port: 7496, ClientID: 15}

	srv.postConnectSetup(&fakeAttempter{port: 7496, connectOk: true}, ep)

	if strings.Contains(buf.String(), "Gateway reachable again") {
		t.Fatalf("clean first connect must not log a recovery bookend:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Connected to IB Gateway") {
		t.Fatalf("postConnectSetup must still log the connect line:\n%s", buf.String())
	}
}
