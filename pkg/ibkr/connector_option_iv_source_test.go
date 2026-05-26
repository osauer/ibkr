package ibkr

import (
	"strconv"
	"testing"
)

// TestModelTickPopulatesOptionIVNotSubscriptionIV pins the per-strike IV
// data model that the daemon's zero-gamma compute depends on.
//
// When the IBKR gateway delivers an OPTION_COMPUTATION model tick for an
// option subscription (msg 21, tickType 13), the per-strike IV lands in
// optIV[OPRA_key] — readable via GetOptionIV — and NOT in
// subscriptions[OPRA_key].IV (which is exposed through GetMarketData's
// MarketData.IV field). subscriptions[…].IV is only written by
// handleTickGeneric when generic tick 106 fires; tick 106 is documented
// for STK/IND ("30-day chain-averaged IV of the underlying") and is not
// reliably delivered for individual OPT subscriptions, regardless of
// whether the generic-tick list requests it.
//
// productionLegFetcher (internal/daemon/gamma_zero_compute.go) reads the
// per-strike IV via GetOptionIV for exactly this reason. A previous
// revision polled d.IV (i.e. MarketData.IV) instead and timed out every
// leg waiting for a value the gateway never sent — the v0.24.x gamma-
// compute "all N legs failed" bug. Anyone tempted to "simplify" the
// fetcher by collapsing the two reads into a single MarketData poll will
// fail this test and re-introduce the regression.
func TestModelTickPopulatesOptionIVNotSubscriptionIV(t *testing.T) {
	c := NewConnector(nil)
	const (
		reqID   = 42
		opraKey = "SPY_260619P00500000"
	)

	// Mirror the state SubscribeOption sets up: a subscription keyed by
	// OPRA chain key, with both the generic-tick (reqIDMap) and
	// option-computation (optReqIDs) routes pointing to that key.
	c.subMu.Lock()
	c.reqIDMap[reqID] = opraKey
	c.subscriptions[opraKey] = &Subscription{Symbol: opraKey, ReqID: reqID}
	c.subMu.Unlock()
	c.optMu.Lock()
	c.optReqIDs[reqID] = opraKey
	c.optMu.Unlock()

	// Model-computation tick. Wire layout for serverVersion >=
	// MIN_SERVER_VER_PRICE_BASED_VOLATILITY (=165):
	//   [msgID, reqID, tickType, tickAttrib, IV, delta, optPrice,
	//    pvDividend, gamma, vega, theta, underlyingPx]
	c.handleOptionComputation([]string{
		"21", strconv.Itoa(reqID), "13", "0",
		"0.275", "0.42", "8.30", "0", "0.0123", "30.0", "-0.08", "499.5",
	})

	// optIV is the per-strike IV source the gamma compute reads.
	iv, ok := c.GetOptionIV(opraKey)
	if !ok || iv != 0.275 {
		t.Fatalf("GetOptionIV(%q) = (%v, %v); want (0.275, true) — model tick should populate optIV",
			opraKey, iv, ok)
	}
	g, ok := c.GetOptionGreeks(opraKey)
	if !ok || g.Gamma != 0.0123 {
		t.Fatalf("GetOptionGreeks(%q).Gamma = (%v, %v); want (0.0123, true)",
			opraKey, g.Gamma, ok)
	}

	// The bug surface: anyone reading d.IV via GetMarketData sees zero
	// because handleOptionComputation does not write into sub.IV. Only
	// handleTickGeneric for tick type 106 does — and that tick is not
	// delivered for OPT subscriptions in practice. This assertion is
	// the regression pin: if a future refactor makes the model tick
	// also write to sub.IV, this test fails loudly and the maintainer
	// has to decide whether the two surfaces should converge.
	md, ok := c.GetMarketData()[opraKey]
	if !ok || md == nil {
		t.Fatalf("GetMarketData()[%q] missing", opraKey)
	}
	if md.IV != 0 {
		t.Errorf("MarketData.IV = %v; want 0 (sub.IV is the underlying-IV path, not a per-strike source)", md.IV)
	}
}

func TestOptionMarketDataKeyDistinguishesExpiries(t *testing.T) {
	t.Parallel()
	front := OptionMarketDataKey("asts", "2026-09-18", "c", 55)
	back := OptionMarketDataKey("ASTS", "2027-01-15", "C", 55)
	if front == back {
		t.Fatalf("keys collide across expiries: %q", front)
	}
	if front != "ASTS_260918C55" {
		t.Fatalf("front key = %q, want ASTS_260918C55", front)
	}
}

func TestKeyedOptionIVRoutesSameUnderlyingToSeparateSlots(t *testing.T) {
	c := NewConnector(nil)
	const (
		frontReqID = 101
		backReqID  = 102
	)
	frontKey := OptionMarketDataKey("ASTS", "20260918", "C", 55)
	backKey := OptionMarketDataKey("ASTS", "20270115", "C", 55)

	c.optReqIDs[frontReqID] = frontKey
	c.optReqIDs[backReqID] = backKey

	c.handleOptionComputation([]string{
		"21", strconv.Itoa(frontReqID), "13", "0",
		"0.80", "0.50", "10.0", "0", "0.01", "20.0", "-0.05", "55",
	})
	c.handleOptionComputation([]string{
		"21", strconv.Itoa(backReqID), "13", "0",
		"1.10", "0.45", "12.0", "0", "0.01", "22.0", "-0.04", "55",
	})

	frontIV, frontOK := c.GetOptionIV(frontKey)
	backIV, backOK := c.GetOptionIV(backKey)
	if !frontOK || frontIV != 0.80 {
		t.Fatalf("front IV = %v ok=%v, want 0.80 true", frontIV, frontOK)
	}
	if !backOK || backIV != 1.10 {
		t.Fatalf("back IV = %v ok=%v, want 1.10 true", backIV, backOK)
	}
}
