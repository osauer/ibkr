package daemon

import "time"

// prevCloseCache memoises per-symbol previous-regular-session-close prices
// so positions / quote calls don't issue a fresh market-data subscribe
// for every held underlying on every invocation. The value (tick 9 in
// IBKR's protocol) is static across a full trading day — the cache TTL
// just has to be longer than typical session lengths and short enough
// that an overnight value can refresh on the next morning's first call.
//
// Negative caching is essential: an inactive symbol (delisted, halted)
// produces a zero-Close subscription that we still want to remember so
// the next 19 positions calls in the same session don't re-poll the same
// dead stream. The TTL applies symmetrically.
type prevCloseCache struct {
	inner *ttlMap[string, prevCloseEntry]
}

type prevCloseEntry struct {
	value float64 // 0 → negative cache (subscription returned no Close)
}

// prevCloseTTL is the maximum age of a cached prev-close before the next
// caller is forced to re-fetch. 12 hours covers the longest natural
// trading-session gap (Friday close ~21:00 UTC → Monday pre-market ~09:00
// UTC) while ensuring overnight values do refresh by morning. Daemons
// restarted between sessions repopulate naturally on first use.
const prevCloseTTL = 12 * time.Hour

func newPrevCloseCache() *prevCloseCache {
	return &prevCloseCache{
		inner: newTTLMap[string, prevCloseEntry](func(_ prevCloseEntry, _ time.Time) time.Duration {
			return prevCloseTTL
		}),
	}
}

func (c *prevCloseCache) get(symbol string, now time.Time) (prevCloseEntry, bool) {
	return c.inner.get(symbol, now)
}

func (c *prevCloseCache) put(symbol string, e prevCloseEntry, now time.Time) {
	c.inner.put(symbol, e, now)
}

// computePositionDayChange returns (chg, chg_pct) pointers describing how
// far the position's current mark sits from the previous regular-session
// close. Both stay nil unless we have a usable mark AND a positive
// cached prev close — no fabrication, no divide-by-zero.
func computePositionDayChange(mark, prevClose float64) (*float64, *float64) {
	if mark <= 0 || prevClose <= 0 {
		return nil, nil
	}
	chg := mark - prevClose
	pct := chg / prevClose * 100
	return &chg, &pct
}
