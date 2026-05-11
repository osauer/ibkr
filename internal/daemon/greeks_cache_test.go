package daemon

import (
	"testing"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// TestGreeksCacheRoundTrip: put then get within the TTL returns the
// stored value; the second-level cache key drives the renderer's
// nil-vs-value decision, so the round-trip semantics must be precise.
func TestGreeksCacheRoundTrip(t *testing.T) {
	c := newGreeksCache()
	now := time.Now()
	g := ibkrlib.Greeks{Delta: 0.5, Theta: -0.1}
	c.put("AAPL_260117C200", greeksEntry{value: g, ok: true, asOf: now})

	got, ok := c.get("AAPL_260117C200", now)
	if !ok {
		t.Fatalf("expected cache hit, got miss")
	}
	if got.value.Delta != 0.5 || got.value.Theta != -0.1 {
		t.Errorf("value mismatch: %+v", got.value)
	}
	if !got.ok {
		t.Errorf("ok bit lost across roundtrip")
	}
}

// TestGreeksCacheTTLExpiry: an entry older than TTL must miss so the
// caller re-fetches. The TTL is deliberately short relative to a
// session so back-to-back invocations are free but mid-session drift
// still refreshes.
func TestGreeksCacheTTLExpiry(t *testing.T) {
	c := newGreeksCache()
	stale := time.Now().Add(-2 * greeksTTL)
	c.put("AAPL_260117C200", greeksEntry{value: ibkrlib.Greeks{Delta: 0.5}, ok: true, asOf: stale})

	if _, ok := c.get("AAPL_260117C200", time.Now()); ok {
		t.Fatalf("expected stale entry to miss, got hit")
	}
}

// TestGreeksCacheNegativeEntry: an ok=false entry IS a cache hit (we
// remember we tried) — the renderer treats it as "tried and got
// nothing" rather than "haven't tried yet". This is what keeps the
// next positions call from re-polling a dead leg every time.
func TestGreeksCacheNegativeEntry(t *testing.T) {
	c := newGreeksCache()
	now := time.Now()
	c.put("AAPL_260117C200", greeksEntry{ok: false, asOf: now})
	got, ok := c.get("AAPL_260117C200", now)
	if !ok {
		t.Fatalf("negative cache entry should hit within TTL")
	}
	if got.ok {
		t.Errorf("ok bit should be false on negative entry")
	}
}
