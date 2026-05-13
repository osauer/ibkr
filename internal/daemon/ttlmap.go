package daemon

import (
	"sync"
	"time"
)

// ttlMap is a generic in-memory TTL cache. The ttlOf callback decides how
// long each entry lives — supports both constant TTL (prev-close), per-
// entry asymmetric TTL (greeks: short on negative, long on positive), and
// time-of-day TTL (expiry-IV: short during US trading hours, long outside).
// All access is guarded by a single RWMutex; concurrent get/put is safe.
type ttlMap[K comparable, V any] struct {
	mu      sync.RWMutex
	entries map[K]ttlEntry[V]
	ttlOf   func(V, time.Time) time.Duration
}

type ttlEntry[V any] struct {
	value V
	asOf  time.Time
}

func newTTLMap[K comparable, V any](ttlOf func(V, time.Time) time.Duration) *ttlMap[K, V] {
	return &ttlMap[K, V]{
		entries: map[K]ttlEntry[V]{},
		ttlOf:   ttlOf,
	}
}

// get returns (value, true) when an entry exists for k and is not yet
// stale per ttlOf(value, now). Returns the zero value and false otherwise.
func (c *ttlMap[K, V]) get(k K, now time.Time) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[k]
	if !ok {
		var zero V
		return zero, false
	}
	if now.Sub(e.asOf) > c.ttlOf(e.value, now) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// put records v at time now. Subsequent gets within ttlOf(v, getNow) return it.
func (c *ttlMap[K, V]) put(k K, v V, now time.Time) {
	c.mu.Lock()
	c.entries[k] = ttlEntry[V]{value: v, asOf: now}
	c.mu.Unlock()
}
