package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	regimeSeriesCacheFreshFor       = 12 * time.Hour
	regimeSeriesCacheMaxFallbackAge = 14 * 24 * time.Hour
)

// regimeSeriesCache keeps official daily public-rate series from flapping on
// routine regime reads. OAS/funding rows are slow-changing daily data; a
// recently fetched local copy is preferable to marking the indicator missing
// because one HTTP request timed out.
type regimeSeriesCache struct {
	mu             sync.Mutex
	dir            string
	mem            map[string]regimeSeriesCacheEntry
	freshFor       time.Duration
	maxFallbackAge time.Duration
}

type regimeSeriesCacheEntry struct {
	SeriesID  string              `json:"series_id"`
	FetchedAt time.Time           `json:"fetched_at"`
	Points    []regimeSeriesPoint `json:"points"`
}

func newRegimeSeriesCache(dir string) *regimeSeriesCache {
	return &regimeSeriesCache{
		dir:            dir,
		mem:            map[string]regimeSeriesCacheEntry{},
		freshFor:       regimeSeriesCacheFreshFor,
		maxFallbackAge: regimeSeriesCacheMaxFallbackAge,
	}
}

func regimeSeriesCacheDefaultDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "regime-series"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr", "regime-series"), nil
}

func (c *regimeSeriesCache) fetch(ctx context.Context, seriesID string, fetcher func(context.Context, string) ([]regimeSeriesPoint, error)) ([]regimeSeriesPoint, error) {
	if c == nil {
		return fetcher(ctx, seriesID)
	}
	now := time.Now()
	if points, ok := c.cachedUsable(seriesID, now, true); ok {
		return points, nil
	}
	points, err := fetcher(ctx, seriesID)
	if err == nil {
		c.put(seriesID, points, now)
		return points, nil
	}
	if points, ok := c.cachedUsable(seriesID, now, false); ok {
		return points, nil
	}
	return nil, err
}

func (c *regimeSeriesCache) cachedUsable(seriesID string, now time.Time, requireFreshFetch bool) ([]regimeSeriesPoint, bool) {
	entry, ok := c.get(seriesID)
	if !ok || len(entry.Points) == 0 {
		return nil, false
	}
	if requireFreshFetch && now.Sub(entry.FetchedAt) > c.freshFor {
		return nil, false
	}
	if !requireFreshFetch && latestAge(entry.Points, now) > c.maxFallbackAge {
		return nil, false
	}
	return cloneRegimeSeries(entry.Points), true
}

func (c *regimeSeriesCache) get(seriesID string) (regimeSeriesCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.mem[seriesID]; ok {
		return cloneRegimeSeriesEntry(entry), true
	}
	entry, err := c.loadLocked(seriesID)
	if err != nil {
		return regimeSeriesCacheEntry{}, false
	}
	c.mem[seriesID] = entry
	return cloneRegimeSeriesEntry(entry), true
}

func (c *regimeSeriesCache) put(seriesID string, points []regimeSeriesPoint, fetchedAt time.Time) {
	if len(points) == 0 {
		return
	}
	entry := regimeSeriesCacheEntry{
		SeriesID:  seriesID,
		FetchedAt: fetchedAt,
		Points:    cloneRegimeSeries(points),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mem[seriesID] = cloneRegimeSeriesEntry(entry)
	_ = c.saveLocked(entry)
}

func (c *regimeSeriesCache) loadLocked(seriesID string) (regimeSeriesCacheEntry, error) {
	data, err := os.ReadFile(c.path(seriesID))
	if err != nil {
		return regimeSeriesCacheEntry{}, err
	}
	var entry regimeSeriesCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return regimeSeriesCacheEntry{}, err
	}
	if entry.SeriesID != seriesID || len(entry.Points) == 0 {
		return regimeSeriesCacheEntry{}, fmt.Errorf("regime series cache: bad entry for %s", seriesID)
	}
	return entry, nil
}

func (c *regimeSeriesCache) saveLocked(entry regimeSeriesCacheEntry) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := c.path(entry.SeriesID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *regimeSeriesCache) path(seriesID string) string {
	return filepath.Join(c.dir, sanitizeRegimeSeriesID(seriesID)+".json")
}

func sanitizeRegimeSeriesID(seriesID string) string {
	var b strings.Builder
	for _, r := range seriesID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "series"
	}
	return b.String()
}

func latestAge(points []regimeSeriesPoint, now time.Time) time.Duration {
	latest, ok := latestSeriesPoint(points)
	if !ok || latest.Date.IsZero() {
		return time.Duration(1<<63 - 1)
	}
	return now.Sub(latest.Date)
}

func cloneRegimeSeriesEntry(entry regimeSeriesCacheEntry) regimeSeriesCacheEntry {
	entry.Points = cloneRegimeSeries(entry.Points)
	return entry
}

func cloneRegimeSeries(points []regimeSeriesPoint) []regimeSeriesPoint {
	if len(points) == 0 {
		return nil
	}
	return slices.Clone(points)
}
