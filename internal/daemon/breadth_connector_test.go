package daemon

import (
	"testing"
)

// TestBreadthGatewayConnector_NilUnstarted pins the contract that
// breadthGatewayConnector returns nil before the bulk-historical
// handshake has landed (or if it failed). The breadth fetcher
// relies on this nil to surface "no gateway" gracefully — a
// regression that returned a stub or fell through to the primary
// connector would mask bulk-connector startup failures and silently
// re-introduce the slot-pool contention this layer exists to avoid.
func TestBreadthGatewayConnector_NilUnstarted(t *testing.T) {
	t.Parallel()
	s := &Server{}
	if got := s.breadthGatewayConnector(); got != nil {
		t.Errorf("breadthGatewayConnector on zero-value Server = %v, want nil", got)
	}
}

// TestBreadthFetcher_WiredToBulkConnector documents the routing
// invariant: installBreadthEngine wires the fetcher's getConn thunk
// to breadthGatewayConnector, not the primary gatewayConnector. The
// fetcher's getConn must return nil here (the bulk connector hasn't
// been started) — proving the closure resolves through the bulk
// path. If a future change accidentally re-binds the fetcher to
// s.gatewayConnector, this test won't catch it (the primary is also
// nil here); the smoke test is the canonical guarantee of correct
// routing against a live gateway. This unit test exists as a
// nil-safety pin for the bulk-path closure specifically.
func TestBreadthFetcher_WiredToBulkConnector(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.installSubs()
	s.installBreadthEngine()
	if s.breadth == nil {
		t.Fatal("breadth engine not installed")
	}
	// Engine refuses a refresh when the fetcher's getConn returns nil,
	// surfacing a clean error rather than calling into a nil connector.
	// We can't expose the fetcher's getConn from spx.Engine, but we can
	// assert breadthGatewayConnector — the actual closure target — is
	// nil-safe here, which means the engine will see nil for every leg
	// of the first refresh attempt until startBreadthConnector lands.
	if got := s.breadthGatewayConnector(); got != nil {
		t.Errorf("breadthGatewayConnector after installBreadthEngine = %v, want nil (no bulk handshake yet)", got)
	}
}
