package ibkr

import "time"

// IBKRStatus is the high-level connection state surfaced to API/CLI layers
// so consumers can flag staleness in responses without needing to interpret
// the connection-level ConnectionStatus enum directly.
type IBKRStatus int

const (
	// IBKRStatusDisconnected indicates no usable connection: the connector is
	// not started, has no lease, or the underlying connection is in
	// Disconnected/Failed state.
	IBKRStatusDisconnected IBKRStatus = iota
	// IBKRStatusReconnecting indicates the connector is attempting to
	// (re)establish connectivity. Data served while in this state is
	// last-known and should be flagged stale.
	IBKRStatusReconnecting
	// IBKRStatusConnected indicates the connector has an established session
	// with the gateway and is ready to serve fresh requests.
	IBKRStatusConnected
	// IBKRStatusMaintenance indicates IBKR's scheduled maintenance window is
	// in effect (Saturday 00:00–04:00 US/Eastern) regardless of session
	// state. During this window the gateway is restarted and data is
	// expected to be unavailable.
	IBKRStatusMaintenance
)

// String renders the status as an upper-case label suitable for inclusion in
// JSON envelopes consumed by the agent layer.
func (s IBKRStatus) String() string {
	switch s {
	case IBKRStatusDisconnected:
		return "DISCONNECTED"
	case IBKRStatusReconnecting:
		return "RECONNECTING"
	case IBKRStatusConnected:
		return "CONNECTED"
	case IBKRStatusMaintenance:
		return "MAINTENANCE"
	default:
		return "UNKNOWN"
	}
}

// GetIBKRStatus returns a coarse-grained connection state for the connector,
// folding the underlying ConnectionStatus into the smaller surface that
// downstream APIs expose. The maintenance-window check overrides connection
// state when active, so callers can warn users that any stale data they
// observe is expected (rather than a fault) during weekly IBKR resets.
//
// The method is non-blocking and safe for concurrent use.
func (c *Connector) GetIBKRStatus() IBKRStatus {
	if isIBKRMaintenanceWindow(time.Now()) {
		return IBKRStatusMaintenance
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return IBKRStatusDisconnected
	}

	switch conn.Status() {
	case StatusConnected:
		return IBKRStatusConnected
	case StatusConnecting, StatusReconnecting:
		return IBKRStatusReconnecting
	default:
		return IBKRStatusDisconnected
	}
}

// isIBKRMaintenanceWindow reports whether the supplied instant falls inside
// IBKR's standard weekly maintenance window. IBKR documents the window as
// Saturday 00:00 to ~04:00 US/Eastern; we use a conservative 00:00–04:00 ET
// range. The function falls back to UTC if the IANA tzdata is unavailable
// (which would be unusual on a server but should not trigger a hard error).
func isIBKRMaintenanceWindow(t time.Time) bool {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	et := t.In(loc)
	if et.Weekday() != time.Saturday {
		return false
	}
	hour := et.Hour()
	return hour >= 0 && hour < 4
}
