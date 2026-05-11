package ibkr

import (
	"math"
	"testing"
)

// TestHandleOptionComputationPersistsGreeks verifies that a single model-
// computation tick (msg 21, tickType 13) populates the optGreeks /
// optUnderlyingPx maps, and that GetOptionGreeks returns what was stored.
//
// Field order matches the comment in connector.go::handleOptionComputation:
//
//	[msgID, version, reqID, tickType, impliedVol, delta, optPrice,
//	 pvDividend, gamma, vega, theta, underlyingPrice]
func TestHandleOptionComputationPersistsGreeks(t *testing.T) {
	conn := NewConnector(nil)
	key := "AAPL_260117C200"
	conn.optReqIDs[7] = key

	// tickType 13 = model computation.
	fields := []string{
		"21",     // msgID
		"6",      // version
		"7",      // reqID
		"13",     // tickType: model
		"0.275",  // impliedVol
		"0.523",  // delta
		"12.50",  // optPrice
		"0.0",    // pvDividend
		"0.0142", // gamma
		"34.21",  // vega
		"-0.083", // theta
		"199.40", // underlyingPrice
	}
	conn.handleOptionComputation(fields)

	g, ok := conn.GetOptionGreeks(key)
	if !ok {
		t.Fatalf("GetOptionGreeks(%q): not found", key)
	}
	if math.Abs(g.Delta-0.523) > 1e-9 {
		t.Errorf("delta = %v, want 0.523", g.Delta)
	}
	if math.Abs(g.Gamma-0.0142) > 1e-9 {
		t.Errorf("gamma = %v, want 0.0142", g.Gamma)
	}
	if math.Abs(g.Vega-34.21) > 1e-9 {
		t.Errorf("vega = %v, want 34.21", g.Vega)
	}
	if math.Abs(g.Theta-(-0.083)) > 1e-9 {
		t.Errorf("theta = %v, want -0.083", g.Theta)
	}
	if iv, ok := conn.GetOptionIV(key); !ok || math.Abs(iv-0.275) > 1e-9 {
		t.Errorf("optIV = %v ok=%v, want 0.275", iv, ok)
	}
	if u, ok := conn.GetOptionUnderlyingPrice(key); !ok || math.Abs(u-199.40) > 1e-9 {
		t.Errorf("underlying = %v ok=%v, want 199.40", u, ok)
	}
}

// TestHandleOptionComputationRejectsSentinelGreeks proves that IBKR's
// "model hasn't priced this row yet" sentinel values (MaxFloat or NaN
// across the Greeks fields) do not pollute the cache. This is the
// behavior the renderer relies on to decide whether to show "—" vs a
// number — we must never fabricate a zero when the model abstained.
func TestHandleOptionComputationRejectsSentinelGreeks(t *testing.T) {
	conn := NewConnector(nil)
	key := "AAPL_260117P150"
	conn.optReqIDs[3] = key

	// MaxFloat is the standard IBKR "no data" sentinel for greeks.
	const max = "1.7976931348623157E308"
	fields := []string{
		"21", "6", "3", "13",
		"0.0",   // impliedVol absent
		max,     // delta sentinel
		"0.0",   // optPrice
		"0.0",   // pvDividend
		max,     // gamma sentinel
		max,     // vega sentinel
		max,     // theta sentinel
		"199.4", // underlyingPrice still useful
	}
	conn.handleOptionComputation(fields)

	if g, ok := conn.GetOptionGreeks(key); ok {
		t.Errorf("expected no greeks cached on sentinel row, got %+v", g)
	}
	// Underlying price IS sane and should land — saneGreek doesn't gate
	// underlying price (it has its own bound check in handleOptionComputation).
	if u, ok := conn.GetOptionUnderlyingPrice(key); !ok || u <= 0 {
		t.Errorf("underlying = %v ok=%v, want a positive value", u, ok)
	}
}

// TestHandleOptionComputationIgnoresUnknownReqID verifies the early-exit
// when a model tick arrives for a reqID we never recorded (e.g. after a
// reconnect drops the map mid-stream).
func TestHandleOptionComputationIgnoresUnknownReqID(t *testing.T) {
	conn := NewConnector(nil)
	// No optReqIDs entry — the function must not panic or record anything.
	fields := []string{"21", "6", "9999", "13", "0.3", "0.5", "10", "0", "0.01", "20", "-0.05", "150"}
	conn.handleOptionComputation(fields)
	if len(conn.optGreeks) != 0 {
		t.Fatalf("expected empty optGreeks, got %d entries", len(conn.optGreeks))
	}
}

// TestHandleOptionComputationMalformedFields checks the length guard:
// short rows must be a no-op, not a panic.
func TestHandleOptionComputationMalformedFields(t *testing.T) {
	conn := NewConnector(nil)
	conn.optReqIDs[1] = "X_260117C100"
	// 11 fields when 12 are required.
	short := []string{"21", "6", "1", "13", "0.3", "0.5", "10", "0", "0.01", "20", "-0.05"}
	conn.handleOptionComputation(short)
	if _, ok := conn.GetOptionGreeks("X_260117C100"); ok {
		t.Fatalf("expected no entry on short row")
	}
}
