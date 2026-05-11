package daemon

import (
	"sync"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// greeksCache memoises per-option model-computation Greeks so the
// positions handler doesn't churn a fresh option subscription for every
// held leg on every invocation. The keys are the OPRA-style option
// market-data keys (the same shape SubscribeOption returns), so callers
// in handlePositionsList build the same key from the position's contract
// fields and look up directly.
//
// TTL is tuned for actionability — Greeks shift slowly relative to spot
// (delta drifts on the order of minutes for liquid names), and a stale
// cached value is far better than nil for the portfolio-aggregate
// rendering. 60 s strikes the balance: short enough that an aggressive
// `watch -n 60 ibkr positions` re-warms each cycle, long enough that
// back-to-back invocations during a decision pause cost zero gateway
// round trips.
//
// Negative caching is essential. The model-computation tick (msg 21
// tickType 13) silently drops for far-OTM and illiquid OOH legs; we
// still want to remember "we tried and got nothing" so we don't re-
// poll a dead stream on the next call.
type greeksCache struct {
	mu      sync.RWMutex
	entries map[string]greeksEntry
}

type greeksEntry struct {
	value      ibkrlib.Greeks
	underlying float64 // model-computation underlying price, 0 if unavailable
	ok         bool    // false → negative cache: we tried and got nothing valid
	asOf       time.Time
}

const greeksTTL = 60 * time.Second

func newGreeksCache() *greeksCache {
	return &greeksCache{entries: map[string]greeksEntry{}}
}

func (c *greeksCache) get(key string, now time.Time) (greeksEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok {
		return greeksEntry{}, false
	}
	if now.Sub(e.asOf) > greeksTTL {
		return greeksEntry{}, false
	}
	return e, true
}

func (c *greeksCache) put(key string, e greeksEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = e
}
