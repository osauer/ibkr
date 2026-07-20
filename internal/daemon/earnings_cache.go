package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
)

// earningsCache serves per-symbol next-earnings dates for the trading
// rulebook (rules 6-8). It mirrors fxRateCache's last-known-good shape:
// fetches NEVER run on the snapshot path — readers get whatever is cached
// (possibly stale, flagged as such) and a bounded async refresher fills
// gaps. api.nasdaq.com resets connections from Go's default user agent
// (verified 2026-07-07), so the fetcher identifies as a browser — a
// deliberate, documented choice. Any parse ambiguity records a miss, never
// a guessed date: unknown ≠ pass is the rulebook's core invariant.

const (
	earningsStoreFilename     = "earnings-dates.json"
	earningsPersistVersion    = 1
	earningsFreshWindow       = 24 * time.Hour
	earningsTTL               = 45 * 24 * time.Hour
	earningsFetchTimeout      = 8 * time.Second
	earningsFailureRetry      = 15 * time.Minute
	earningsFetchConcurrency  = 4
	earningsAuthorityScope    = "market/events/earnings"
	earningsStateKind         = "earnings_dates.current.v1"
	earningsObservationKind   = "earnings_dates.snapshot.v1"
	earningsObservationSource = "nasdaq.earnings_calendar"
)

type earningsEntry struct {
	// Date is the next earnings date, YYYY-MM-DD (ET calendar date).
	Date string `json:"date"`
	// TimeOfDay is "amc", "bmo", or "" when the provider did not say.
	TimeOfDay string `json:"time_of_day,omitempty"`
	// Estimated marks provider-flagged unconfirmed dates.
	Estimated  bool      `json:"estimated,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

type earningsPersistEnvelope struct {
	Version int                      `json:"version"`
	Entries map[string]earningsEntry `json:"entries"`
}

type earningsCache struct {
	mu       sync.Mutex
	entries  map[string]earningsEntry
	failures map[string]time.Time
	inflight map[string]bool
	store    *earningsStore
	client   *http.Client
	logf     func(format string, args ...any)
	clock    func() time.Time
	// fetchURL is swappable for tests; %s receives the provider symbol.
	fetchURL string
}

func newEarningsCache(dir string, logf func(string, ...any)) *earningsCache {
	c := newEarningsCacheCold(dir, logf)
	if loaded, err := c.store.load(c.clock()); err != nil {
		logf("earnings cache load failed (cold start): %v", err)
	} else if loaded != nil {
		c.entries = loaded
	}
	return c
}

// newEarningsCacheCold installs the legacy codec without reading it. Server
// construction runs before the persistence lock, so production uses this and
// leaves legacy reads exclusively to the unpublished cutover importer.
func newEarningsCacheCold(dir string, logf func(string, ...any)) *earningsCache {
	c := newEarningsCacheMemory(logf)
	c.store = &earningsStore{dir: dir}
	return c
}

func newEarningsCacheMemory(logf func(string, ...any)) *earningsCache {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &earningsCache{
		entries:  map[string]earningsEntry{},
		failures: map[string]time.Time{},
		inflight: map[string]bool{},
		client:   &http.Client{Timeout: earningsFetchTimeout},
		logf:     logf,
		clock:    time.Now,
		fetchURL: "https://api.nasdaq.com/api/analyst/%s/earnings-date",
	}
	return c
}

// UseCoreStore replaces any legacy JSON projection loaded by construction
// with the current daemon.db document. Missing state starts cold. Failure is
// returned before the authority pointer or in-memory projection changes.
func (c *earningsCache) UseCoreStore(store *corestore.Store) error {
	if c == nil {
		return errors.New("earnings cache: nil cache")
	}
	if store == nil {
		return errors.New("earnings cache: nil corestore")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		c.store = &earningsStore{}
	}
	loaded, err := c.store.useCoreStore(store, c.clock())
	if err != nil {
		return err
	}
	c.entries = loaded
	c.failures = map[string]time.Time{}
	c.inflight = map[string]bool{}
	return nil
}

// nasdaqSymbol maps IBKR symbols to the provider's spelling: share classes
// use dots there ("BRK B" → "BRK.B"). Unmappable symbols return "".
func nasdaqSymbol(sym string) string {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if sym == "" || strings.ContainsAny(sym, "/\\?%") {
		return ""
	}
	return strings.ReplaceAll(sym, " ", ".")
}

// get returns the cached entry and whether it is stale (older than the
// fresh window). ok=false when nothing usable is cached.
func (c *earningsCache) get(sym string) (entry earningsEntry, stale, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[strings.ToUpper(sym)]
	if !found {
		return earningsEntry{}, false, false
	}
	if c.clock().Sub(e.ObservedAt) > earningsTTL {
		return earningsEntry{}, false, false
	}
	return e, c.clock().Sub(e.ObservedAt) > earningsFreshWindow, true
}

// kickRefresh asynchronously refreshes any of syms that are missing or
// stale and not in failure backoff. Bounded concurrency; returns
// immediately. Safe to call from every rules.snapshot.
func (c *earningsCache) kickRefresh(ctx context.Context, syms []string) {
	now := c.clock()
	var todo []string
	c.mu.Lock()
	for _, sym := range syms {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" || nasdaqSymbol(sym) == "" || c.inflight[sym] {
			continue
		}
		if until, failed := c.failures[sym]; failed && now.Sub(until) < earningsFailureRetry {
			continue
		}
		if e, found := c.entries[sym]; found && now.Sub(e.ObservedAt) <= earningsFreshWindow {
			continue
		}
		c.inflight[sym] = true
		todo = append(todo, sym)
	}
	c.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	go c.refresh(ctx, todo)
}

func (c *earningsCache) refresh(ctx context.Context, syms []string) {
	sem := make(chan struct{}, earningsFetchConcurrency)
	var wg sync.WaitGroup
	for _, sym := range syms {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			c.refreshOne(ctx, sym)
		})
	}
	wg.Wait()
	c.persist()
}

func (c *earningsCache) refreshOne(ctx context.Context, sym string) {
	entry, err := c.fetchOne(ctx, sym)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, sym)
	if err != nil {
		c.failures[sym] = c.clock()
		c.logf("earnings fetch %s: %v", sym, err)
		return
	}
	delete(c.failures, sym)
	c.entries[sym] = entry
}

var (
	// "Earnings announcement* for NOW: Jul 22, 2026"
	earningsAnnouncementRe = regexp.MustCompile(`:\s*([A-Z][a-z]{2} \d{1,2}, \d{4})\s*$`)
)

func (c *earningsCache) fetchOne(ctx context.Context, sym string) (earningsEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, earningsFetchTimeout)
	defer cancel()
	url := fmt.Sprintf(c.fetchURL, nasdaqSymbol(sym))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return earningsEntry{}, err
	}
	// The provider's CDN resets bare Go clients at the stream level and
	// silently hangs header-sparse HTTP/1.1 (probed 2026-07-07). What it
	// actually keys on is the browser header set — Origin/Referer/
	// Accept-Language — which returns 200 in ~250ms over Go's default h2.
	for k, v := range map[string]string{
		"User-Agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 ibkr-earnings/1.0",
		"Accept":          "application/json, text/plain, */*",
		"Accept-Language": "en-US,en;q=0.9",
		"Origin":          "https://www.nasdaq.com",
		"Referer":         "https://www.nasdaq.com/",
	} {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return earningsEntry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return earningsEntry{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return earningsEntry{}, err
	}
	return parseNasdaqEarnings(body, c.clock())
}

// parseNasdaqEarnings extracts the announcement date, session half, and
// estimated flag. Strict: a payload without an unambiguous parseable date
// is an error (recorded as a miss), never a guess.
func parseNasdaqEarnings(body []byte, now time.Time) (earningsEntry, error) {
	var payload struct {
		Data struct {
			Announcement string `json:"announcement"`
			ReportText   string `json:"reportText"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return earningsEntry{}, fmt.Errorf("decode: %w", err)
	}
	m := earningsAnnouncementRe.FindStringSubmatch(strings.TrimSpace(payload.Data.Announcement))
	if m == nil {
		return earningsEntry{}, errors.New("no parseable announcement date")
	}
	t, err := time.Parse("Jan 2, 2006", m[1])
	if err != nil {
		return earningsEntry{}, fmt.Errorf("parse %q: %w", m[1], err)
	}
	report := strings.ToLower(payload.Data.ReportText)
	entry := earningsEntry{Date: t.Format("2006-01-02"), ObservedAt: now}
	switch {
	case strings.Contains(report, "after market close"):
		entry.TimeOfDay = "amc"
	case strings.Contains(report, "before market open"):
		entry.TimeOfDay = "bmo"
	}
	if strings.Contains(report, "expected*") || strings.Contains(report, "expected *") || strings.Contains(payload.Data.Announcement, "*") {
		entry.Estimated = true
	}
	return entry, nil
}

func (c *earningsCache) persist() {
	c.mu.Lock()
	snapshot := make(map[string]earningsEntry, len(c.entries))
	maps.Copy(snapshot, c.entries)
	c.mu.Unlock()
	if err := c.store.save(snapshot); err != nil {
		c.logf("earnings cache persist failed: %v", err)
	}
}

// earningsStore persists entries across restarts. Production uses atomic
// daemon.db state + observation writes; the JSON branch remains only as a
// legacy cutover codec and for isolated tests.
type earningsStore struct {
	dir       string // sealed legacy cache; unused after useCoreStore
	authority *corestore.Store
}

func (s *earningsStore) useCoreStore(store *corestore.Store, now time.Time) (map[string]earningsEntry, error) {
	if s == nil {
		return nil, errors.New("earnings store: nil store")
	}
	raw, ok, err := loadMarketState(store, earningsAuthorityScope, earningsStateKind)
	if err != nil {
		return nil, fmt.Errorf("read earnings authority: %w", err)
	}
	loaded := map[string]earningsEntry{}
	if ok {
		loaded, err = decodeEarningsEnvelope(raw, now, true)
		if err != nil {
			return nil, fmt.Errorf("decode earnings authority: %w", err)
		}
	}
	s.authority = store
	return loaded, nil
}

func (s *earningsStore) load(now time.Time) (map[string]earningsEntry, error) {
	var data []byte
	if s.authority != nil {
		var ok bool
		var err error
		data, ok, err = loadMarketState(s.authority, earningsAuthorityScope, earningsStateKind)
		if err != nil || !ok {
			return map[string]earningsEntry{}, err
		}
		return decodeEarningsEnvelope(data, now, true)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, earningsStoreFilename))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read earnings cache: %w", err)
	}
	return decodeEarningsEnvelope(data, now, false)
}

func decodeEarningsEnvelope(data []byte, now time.Time, strict bool) (map[string]earningsEntry, error) {
	var env earningsPersistEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode earnings cache: %w", err)
	}
	if env.Version != earningsPersistVersion {
		if strict {
			return nil, fmt.Errorf("invalid earnings version %d", env.Version)
		}
		return nil, nil
	}
	if strict && env.Entries == nil {
		return nil, errors.New("earnings authority has no entries map")
	}
	entries := make(map[string]earningsEntry, len(env.Entries))
	for sym, e := range env.Entries {
		if err := validateEarningsRow(sym, e, now); err != nil {
			if strict {
				return nil, err
			}
			continue
		}
		if now.Sub(e.ObservedAt) > earningsTTL {
			continue
		}
		entries[strings.ToUpper(sym)] = e
	}
	return entries, nil
}

func validateEarningsRow(sym string, e earningsEntry, now time.Time) error {
	if err := validateEarningsRowShape(sym, e); err != nil {
		return err
	}
	if now.Before(e.ObservedAt) {
		return fmt.Errorf("invalid earnings row %q", sym)
	}
	return nil
}

func validateEarningsRowShape(sym string, e earningsEntry) error {
	canonical := strings.ToUpper(strings.TrimSpace(sym))
	if canonical == "" || canonical != sym || nasdaqSymbol(canonical) == "" || e.ObservedAt.IsZero() {
		return fmt.Errorf("invalid earnings row %q", sym)
	}
	parsed, err := time.Parse("2006-01-02", e.Date)
	if err != nil || parsed.Format("2006-01-02") != e.Date {
		return fmt.Errorf("invalid earnings date for %q", sym)
	}
	switch e.TimeOfDay {
	case "", "amc", "bmo":
		return nil
	default:
		return fmt.Errorf("invalid earnings session for %q", sym)
	}
}

func (s *earningsStore) save(entries map[string]earningsEntry) error {
	env := earningsPersistEnvelope{Version: earningsPersistVersion, Entries: entries}
	if s.authority != nil {
		for symbol, entry := range entries {
			if err := validateEarningsRowShape(symbol, entry); err != nil {
				return err
			}
		}
		payload, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("encode earnings authority: %w", err)
		}
		observedAt := latestEarningsObservation(entries)
		if observedAt.IsZero() {
			observedAt = time.Now().UTC()
		}
		metadata, err := json.Marshal(struct {
			Version    int    `json:"version"`
			EntryCount int    `json:"entry_count"`
			Method     string `json:"method"`
		}{earningsPersistVersion, len(entries), "Nasdaq earnings calendar"})
		if err != nil {
			return fmt.Errorf("encode earnings metadata: %w", err)
		}
		return saveMarketState(s.authority, earningsAuthorityScope, earningsStateKind, corestore.ObservationInput{
			ScopeKey: earningsAuthorityScope, Source: earningsObservationSource, Kind: earningsObservationKind,
			ObservedAt: observedAt, ContentType: "application/json", Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
		})
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	target := filepath.Join(s.dir, earningsStoreFilename)
	tmp, err := os.CreateTemp(s.dir, earningsStoreFilename+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("encode %s: %w", earningsStoreFilename, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename %s: %w", earningsStoreFilename, err)
	}
	return nil
}

func latestEarningsObservation(entries map[string]earningsEntry) time.Time {
	var latest time.Time
	for _, entry := range entries {
		if entry.ObservedAt.After(latest) {
			latest = entry.ObservedAt
		}
	}
	return latest
}
