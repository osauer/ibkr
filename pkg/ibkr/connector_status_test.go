package ibkr

import (
	"testing"
	"time"
)

func TestIBKRStatus_String(t *testing.T) {
	cases := []struct {
		s    IBKRStatus
		want string
	}{
		{IBKRStatusDisconnected, "DISCONNECTED"},
		{IBKRStatusReconnecting, "RECONNECTING"},
		{IBKRStatusConnected, "CONNECTED"},
		{IBKRStatusMaintenance, "MAINTENANCE"},
		{IBKRStatus(99), "UNKNOWN"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("IBKRStatus(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestGetIBKRStatus_NilConn(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	// c.conn is nil (connector never started)
	// Skip maintenance window: test runs at non-maintenance time most of the
	// time, but when it doesn't we should still get MAINTENANCE — adjust by
	// using a wrapper that goes through the unexported helper directly.
	if isIBKRMaintenanceWindow(time.Now()) {
		t.Skip("test running during IBKR maintenance window; covered by isIBKRMaintenanceWindow tests")
	}
	if got := c.GetIBKRStatus(); got != IBKRStatusDisconnected {
		t.Fatalf("GetIBKRStatus = %v, want DISCONNECTED", got)
	}
}

func TestGetIBKRStatus_ConnectedConnection(t *testing.T) {
	if isIBKRMaintenanceWindow(time.Now()) {
		t.Skip("running during maintenance window")
	}
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	c.conn = conn

	if got := c.GetIBKRStatus(); got != IBKRStatusConnected {
		t.Fatalf("GetIBKRStatus = %v, want CONNECTED", got)
	}
}

func TestGetIBKRStatus_ConnectingMapsToReconnecting(t *testing.T) {
	if isIBKRMaintenanceWindow(time.Now()) {
		t.Skip("running during maintenance window")
	}
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnecting
	c.conn = conn

	if got := c.GetIBKRStatus(); got != IBKRStatusReconnecting {
		t.Fatalf("GetIBKRStatus = %v, want RECONNECTING", got)
	}
}

func TestGetIBKRStatus_ReconnectingMapsToReconnecting(t *testing.T) {
	if isIBKRMaintenanceWindow(time.Now()) {
		t.Skip("running during maintenance window")
	}
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusReconnecting
	c.conn = conn

	if got := c.GetIBKRStatus(); got != IBKRStatusReconnecting {
		t.Fatalf("GetIBKRStatus = %v, want RECONNECTING", got)
	}
}

func TestGetIBKRStatus_FailedMapsToDisconnected(t *testing.T) {
	if isIBKRMaintenanceWindow(time.Now()) {
		t.Skip("running during maintenance window")
	}
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusFailed
	c.conn = conn

	if got := c.GetIBKRStatus(); got != IBKRStatusDisconnected {
		t.Fatalf("GetIBKRStatus = %v, want DISCONNECTED for StatusFailed", got)
	}
}

func TestIsIBKRMaintenanceWindow(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}

	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"saturday 02:00 ET — inside", time.Date(2026, 4, 25, 2, 0, 0, 0, loc), true},
		{"saturday 00:00 ET — boundary inclusive", time.Date(2026, 4, 25, 0, 0, 0, 0, loc), true},
		{"saturday 03:59 ET — inside", time.Date(2026, 4, 25, 3, 59, 0, 0, loc), true},
		{"saturday 04:00 ET — boundary exclusive", time.Date(2026, 4, 25, 4, 0, 0, 0, loc), false},
		{"saturday 12:00 ET — outside window", time.Date(2026, 4, 25, 12, 0, 0, 0, loc), false},
		{"friday 02:00 ET — wrong day", time.Date(2026, 4, 24, 2, 0, 0, 0, loc), false},
		{"sunday 02:00 ET — wrong day", time.Date(2026, 4, 26, 2, 0, 0, 0, loc), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isIBKRMaintenanceWindow(c.t); got != c.want {
				t.Errorf("isIBKRMaintenanceWindow(%s) = %v, want %v", c.t, got, c.want)
			}
		})
	}
}

func TestIsIBKRMaintenanceWindow_TimeZoneCrossover(t *testing.T) {
	// UTC noon Saturday is 08:00 ET — outside maintenance.
	tUTC := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	if got := isIBKRMaintenanceWindow(tUTC); got != false {
		t.Errorf("UTC noon Saturday should not be ET maintenance, got %v", got)
	}
	// Saturday 06:00 UTC is 02:00 ET (EDT in April) — inside maintenance.
	tUTCMaint := time.Date(2026, 4, 25, 6, 0, 0, 0, time.UTC)
	if got := isIBKRMaintenanceWindow(tUTCMaint); got != true {
		t.Errorf("06:00 UTC Saturday should map to 02:00 EDT — inside maintenance window, got %v", got)
	}
}
