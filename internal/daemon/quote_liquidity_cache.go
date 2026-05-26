package daemon

import "time"

type quoteLiquidityCache struct {
	inner *ttlMap[quoteLiquidityKey, quoteLiquidityEntry]
}

type quoteLiquidityKey struct {
	symbol   string
	market   string
	exchange string
	primary  string
	currency string
}

type quoteLiquidityEntry struct {
	avgVolume       int64
	avgDollarVolume float64
	status          string
	source          string
	sampleDays      int
	asOf            time.Time
}

func newQuoteLiquidityCache() *quoteLiquidityCache {
	return &quoteLiquidityCache{
		inner: newTTLMap[quoteLiquidityKey, quoteLiquidityEntry](func(e quoteLiquidityEntry, _ time.Time) time.Duration {
			if e.status == "ok" || e.status == "partial" {
				return 4 * time.Hour
			}
			return 5 * time.Minute
		}),
	}
}

func (c *quoteLiquidityCache) get(key quoteLiquidityKey, now time.Time) (quoteLiquidityEntry, bool) {
	if c == nil || c.inner == nil {
		return quoteLiquidityEntry{}, false
	}
	return c.inner.get(key, now)
}

func (c *quoteLiquidityCache) put(key quoteLiquidityKey, e quoteLiquidityEntry, now time.Time) {
	if c == nil || c.inner == nil {
		return
	}
	c.inner.put(key, e, now)
}
