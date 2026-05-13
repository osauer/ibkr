package daemon

import (
	"sync"
	"testing"
	"time"
)

// put-then-get within TTL must return the stored entry. Sanity check that
// the lookup key is symmetric — symbol case is the caller's responsibility,
// so we don't normalise.
func TestExpiryIVCacheHit(t *testing.T) {
	t.Parallel()
	c := newExpiryIVCache()
	now := time.Date(2026, 5, 11, 10, 0, 0, 0, expiryIVNYZone)
	c.put("AAPL", "2026-06-19", expiryIVEntry{iv: 0.28, status: "ok"}, now)

	got, ok := c.get("AAPL", "2026-06-19", now.Add(30*time.Second))
	if !ok {
		t.Fatalf("get returned !ok within TTL")
	}
	if got.iv != 0.28 || got.status != "ok" {
		t.Errorf("got %+v, want iv=0.28 status=ok", got)
	}
}

// An entry older than the active TTL must miss. RTH TTL is 60 s — anything
// beyond that is stale enough to re-fetch.
func TestExpiryIVCacheMissOnStale(t *testing.T) {
	t.Parallel()
	c := newExpiryIVCache()
	tradingHour := time.Date(2026, 5, 11, 14, 0, 0, 0, expiryIVNYZone) // 2 pm ET
	c.put("AAPL", "2026-06-19", expiryIVEntry{iv: 0.30, status: "ok"}, tradingHour)

	if _, ok := c.get("AAPL", "2026-06-19", tradingHour.Add(2*time.Minute)); ok {
		t.Errorf("get returned ok after staleness window")
	}
}

// Negative-cache entries (timeout/unavailable) live just as long as
// successful fills — without that we'd spam the gateway with dead-leg
// retries on every chain refresh.
func TestExpiryIVCacheNegativeCachePersists(t *testing.T) {
	t.Parallel()
	c := newExpiryIVCache()
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, expiryIVNYZone)
	c.put("AMD", "2026-12-18", expiryIVEntry{status: "timeout"}, now)

	got, ok := c.get("AMD", "2026-12-18", now.Add(30*time.Second))
	if !ok {
		t.Fatalf("negative entry should still hit within TTL")
	}
	if got.status != "timeout" || got.iv != 0 {
		t.Errorf("got %+v, want status=timeout iv=0", got)
	}
}

// Off-hours TTL is 4 h so an entry put before close still hits during
// the overnight gap. The boundary is wall-clock NY time, which is what
// US-equities options data follows.
func TestExpiryIVTTLBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		when time.Time
		want time.Duration
	}{
		{"weekday 10am ET", time.Date(2026, 5, 11, 10, 0, 0, 0, expiryIVNYZone), 60 * time.Second},
		{"weekday 3:59pm ET", time.Date(2026, 5, 11, 15, 59, 0, 0, expiryIVNYZone), 60 * time.Second},
		{"weekday 4pm ET", time.Date(2026, 5, 11, 16, 0, 0, 0, expiryIVNYZone), 4 * time.Hour},
		{"weekday 8am ET (pre-market)", time.Date(2026, 5, 11, 8, 0, 0, 0, expiryIVNYZone), 4 * time.Hour},
		{"saturday noon ET", time.Date(2026, 5, 9, 12, 0, 0, 0, expiryIVNYZone), 4 * time.Hour},
		{"sunday noon ET", time.Date(2026, 5, 10, 12, 0, 0, 0, expiryIVNYZone), 4 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expiryIVTTL(tc.when); got != tc.want {
				t.Errorf("expiryIVTTL(%v) = %v, want %v", tc.when, got, tc.want)
			}
		})
	}
}

// Concurrent put/get must not race. -race in CI catches the failure mode
// if the RWMutex protection is dropped or reduced to a plain map.
func TestExpiryIVCacheRaceSafe(t *testing.T) {
	t.Parallel()
	c := newExpiryIVCache()
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c.put("AAPL", "2026-06-19", expiryIVEntry{iv: float64(i) / 100, status: "ok"}, now)
		}(i)
		go func() {
			defer wg.Done()
			_, _ = c.get("AAPL", "2026-06-19", now)
		}()
	}
	wg.Wait()
}
