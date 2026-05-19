package spx

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"
)

// Logger is the minimal logging surface the engine needs. The daemon
// passes its standard logger; tests can pass nil to silence output.
type Logger interface {
	Warnf(format string, args ...any)
	Infof(format string, args ...any)
}

// Options configures Engine construction. All fields are optional —
// the zero value picks sensible defaults documented per-field.
type Options struct {
	// Logger receives non-fatal refresh events (per-symbol fetch
	// errors, persistence failures). nil silences all logging — fine
	// for tests, not recommended for production.
	Logger Logger
	// Clock injects a synthetic time source for tests. Production
	// callers pass nil and get time.Now.
	Clock func() time.Time
	// Workers caps refresh concurrency. Each worker calls
	// BarFetcher.FetchDaily for one symbol at a time. Defaults to 6,
	// matching the IBKR-side historical-data pacing headroom. Setting
	// to 1 serialises fetches — useful in tests that want
	// deterministic ordering.
	Workers int
	// ColdLookbackDays is how many trailing daily bars to fetch for
	// a name with no cached history. Defaults to WindowSize + 10 to
	// absorb holiday gaps in the trailing 50 trading days.
	ColdLookbackDays int
	// WarmLookbackDays is how many trailing daily bars to fetch for
	// a name whose cached window is current except for today.
	// Defaults to 2 — today's bar plus one for duplicate-detection
	// during the same-session retry path.
	WarmLookbackDays int
}

// Engine is the breadth-spx state machine: it loads the on-disk
// cache, drives a background refresh against a BarFetcher when
// asked, and serves the most recent Snapshot to callers. Safe for
// concurrent use.
//
// Lifecycle:
//   - New() loads persisted state. If the cache is fresh, Get()
//     returns it immediately and no fetch is needed.
//   - Refresh(ctx) is the long-running operation. Serialised against
//     concurrent calls (the second caller waits behind the first).
//   - Get() / Status() are fast read-only views; safe to call during
//     a Refresh in progress.
//
// State is held in memory and persisted after every successful
// refresh. A crash mid-refresh just means the next refresh
// re-fetches from the last persisted point — no transactional
// guarantees beyond "either the file is the new state or it's the
// old state".
type Engine struct {
	store   *Store
	fetcher BarFetcher
	logger  Logger
	clock   func() time.Time

	workers      int
	coldLookback int
	warmLookback int

	// mu protects the in-memory state below. Held briefly for read
	// (Get) or for the swap-after-refresh; never held during a
	// long-running fetch.
	mu       sync.RWMutex
	snapshot *Snapshot
	windows  map[string]ConstituentWindow
	history  []HistoryPoint
	members  []string

	// refreshMu serialises concurrent Refresh() calls. The second
	// caller waits behind the first rather than launching a
	// duplicate fan-out, which would just compete for the same
	// fetcher slots.
	refreshMu sync.Mutex
	// refreshing is set true while a Refresh is in flight. Readers
	// take mu.RLock to inspect — distinct from refreshMu so a poller
	// during a long refresh doesn't block on the fetch loop.
	refreshing bool
}

// New constructs an Engine. Loads any persisted state from store
// (best effort — a corrupted or missing cache results in a cold
// start, not an error). Members come from the checked-in list in
// members_data.go; the engine never re-reads members at runtime.
func New(store *Store, fetcher BarFetcher, opts Options) *Engine {
	if store == nil {
		panic("spx.New: store is required")
	}
	if fetcher == nil {
		panic("spx.New: fetcher is required")
	}
	members, _ := MemberList()
	e := &Engine{
		store:        store,
		fetcher:      fetcher,
		logger:       opts.Logger,
		clock:        opts.Clock,
		workers:      opts.Workers,
		coldLookback: opts.ColdLookbackDays,
		warmLookback: opts.WarmLookbackDays,
		windows:      map[string]ConstituentWindow{},
		members:      members,
	}
	if e.clock == nil {
		e.clock = time.Now
	}
	if e.workers <= 0 {
		e.workers = 6
	}
	if e.coldLookback <= 0 {
		// 50 + 10 trading-day pad covers the ~9 US market holidays
		// per year so we land WindowSize closes from a single fetch.
		e.coldLookback = WindowSize + 10
	}
	if e.warmLookback <= 0 {
		e.warmLookback = 2
	}

	// Best-effort load. Errors are logged but never propagated —
	// the engine remains useful on cold start.
	if snap, err := store.LoadSnapshot(); err != nil {
		e.warnf("breadth: load snapshot: %v", err)
	} else if snap != nil {
		e.snapshot = snap
	}
	if windows, err := store.LoadWindows(); err != nil {
		e.warnf("breadth: load windows: %v", err)
	} else if windows != nil {
		e.windows = windows
	}
	if hist, err := store.LoadHistory(); err != nil {
		e.warnf("breadth: load history: %v", err)
	} else {
		e.history = hist
	}
	return e
}

// Get returns the most recent successful snapshot, or (nil, false) if
// the engine hasn't computed one yet (cold start). Fast: holds only
// a read lock; safe during an in-flight Refresh.
//
// The returned snapshot is a defensive copy — the Excluded slice is
// cloned so a caller iterating its result cannot race against an
// in-flight refresh that's appending exclusions to the engine's
// canonical state.
func (e *Engine) Get() (*Snapshot, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.snapshot == nil {
		return nil, false
	}
	snap := *e.snapshot
	snap.Excluded = slices.Clone(e.snapshot.Excluded)
	return &snap, true
}

// IsRefreshing reports whether a Refresh is currently in flight. The
// daemon's handleBreadthSPX uses this to decide between returning a
// cached snapshot and surfacing status="computing".
func (e *Engine) IsRefreshing() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.refreshing
}

// Refresh runs one pass of the constituent-fanout compute: decide
// which names need new bars, fetch them in parallel, slide each
// window forward, recompute S5FI, persist.
//
// Cold start is ~60 min wall-clock: IBKR's historical-data pacing
// limit caps each gateway connection at 60 requests per 10-minute
// sliding window, so 503 constituents land at ~6 names/min sustained
// after the initial 60-name burst. Adding workers above the default
// 6 doesn't help — the gateway throttles the second any pacing
// budget is exceeded. Warm refresh (same daemon, populated cache) is
// ~1–10 min: only today's bar per name needs fetching.
//
// Concurrent calls serialise via refreshMu — the second caller waits
// behind the first and then sees the updated snapshot. A returned
// error means the compute didn't complete; partial fetch failures
// (some names succeeded, some didn't) do NOT return an error — they
// surface as Excluded entries in the resulting Snapshot.
func (e *Engine) Refresh(ctx context.Context) error {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()

	e.mu.Lock()
	e.refreshing = true
	members := slices.Clone(e.members)
	cached := maps.Clone(e.windows)
	if cached == nil {
		cached = map[string]ConstituentWindow{}
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.refreshing = false
		e.mu.Unlock()
	}()

	plan := e.planFetches(members, cached)
	if len(plan) == 0 {
		// Nothing to fetch — recompute against cached windows so the
		// snapshot timestamp moves forward even on a no-op refresh.
		return e.finalise(members, cached)
	}

	updated, fetchErrs := e.execute(ctx, plan)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	for sym, bars := range updated {
		cached[sym] = mergeBars(cached[sym], bars, sym)
	}
	for sym, err := range fetchErrs {
		e.warnf("breadth: fetch %s: %v", sym, err)
	}

	return e.finalise(members, cached)
}

// fetchPlan is the per-symbol decision the refresh planner makes.
type fetchPlan struct {
	Symbol       string
	LookbackDays int
}

// planFetches walks the membership list and decides what to fetch
// for each name based on the cached window. A name absent from the
// cache or with no closes triggers a cold fetch (ColdLookbackDays);
// a name whose latest bar is today is skipped entirely; everything
// else gets the warm two-bar increment.
//
// Date comparison uses NY session keying so the daemon doesn't
// re-fetch every name when its host clock drifts across UTC midnight
// but the US session hasn't rolled over yet.
func (e *Engine) planFetches(members []string, cached map[string]ConstituentWindow) []fetchPlan {
	today := nySessionKey(e.clock())
	plan := make([]fetchPlan, 0, len(members))
	for _, sym := range members {
		w, ok := cached[sym]
		if !ok || len(w.Closes) == 0 {
			plan = append(plan, fetchPlan{Symbol: sym, LookbackDays: e.coldLookback})
			continue
		}
		if w.LastBarAt == today {
			// Already have today's close — nothing to fetch.
			continue
		}
		plan = append(plan, fetchPlan{Symbol: sym, LookbackDays: e.warmLookback})
	}
	return plan
}

// execute runs the fetch plan in parallel with bounded concurrency.
// Returns one map of (symbol → bars) for successful fetches and
// another of (symbol → error) for failures. Per-symbol failures are
// non-fatal: the caller continues with whatever data landed.
func (e *Engine) execute(ctx context.Context, plan []fetchPlan) (map[string][]Bar, map[string]error) {
	results := make(map[string][]Bar, len(plan))
	errs := make(map[string]error)

	var mu sync.Mutex
	sem := make(chan struct{}, e.workers)
	var wg sync.WaitGroup

dispatch:
	for _, item := range plan {
		// Acquire one slot or bail if ctx fires first. Labelled break
		// because plain `break` would only exit the select.
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			bars, err := e.fetcher.FetchDaily(ctx, item.Symbol, item.LookbackDays)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[item.Symbol] = err
				return
			}
			results[item.Symbol] = bars
		})
	}
	wg.Wait()
	return results, errs
}

// finalise computes a snapshot from the (possibly updated) windows
// and persists everything: windows, snapshot, and rolling history.
// History is appended idempotently — a re-refresh in the same NY
// session overwrites the existing point rather than duplicating it,
// matching the SlideWindow same-day semantics.
//
// Persistence failures are logged but not fatal — the in-memory state
// still updates so subsequent Get() calls see the new value. The
// on-disk cache will be re-tried on the next refresh.
//
// A snapshot whose coverage is below MinCoverageFraction × MemberCount
// is treated as "did not converge" and is NEVER persisted. The
// degenerate Coverage == 0 case (cold-start race against the gateway
// connector, a total outage) is the most extreme failure mode, but
// the same logic must apply to partial fan-outs — a snapshot computed
// over 200 of 503 names is not representative of the underlying market
// and would still poison the scheduler's "today's snapshot exists,
// skip next bootstrap" check. The threshold is 80%: tolerates ordinary
// per-name fetch errors (delisted tickers, transient pacing) while
// rejecting catastrophic fan-outs. finalise returns nil on a
// below-threshold result — the engine logs, the existing on-disk
// state is preserved, and the next tick retries.
func (e *Engine) finalise(members []string, windows map[string]ConstituentWindow) error {
	now := e.clock()
	sessionKey := nySessionKey(now)
	snap := Compute(members, windows, sessionKey, now)

	minCoverage := int(MinCoverageFraction * float64(snap.MemberCount))
	if snap.Coverage < minCoverage {
		e.warnf("breadth: refresh coverage %d/%d below threshold %d (%.0f%% of %d); not persisting, will retry next tick",
			snap.Coverage, snap.MemberCount, minCoverage, MinCoverageFraction*100, snap.MemberCount)
		return nil
	}

	// Build the new history series. Lock briefly to read the existing
	// slice, then release while we shuffle bytes — we don't want to
	// hold the lock through three disk writes below.
	e.mu.Lock()
	history := appendHistory(e.history, HistoryPoint{Date: sessionKey, Value: snap.Value})
	e.mu.Unlock()

	if err := e.store.SaveWindows(windows, now); err != nil {
		e.warnf("breadth: save windows: %v", err)
	}
	if err := e.store.SaveSnapshot(snap); err != nil {
		e.warnf("breadth: save snapshot: %v", err)
	}
	if err := e.store.SaveHistory(history); err != nil {
		e.warnf("breadth: save history: %v", err)
	}

	e.mu.Lock()
	e.windows = windows
	e.snapshot = &snap
	e.history = history
	e.mu.Unlock()
	return nil
}

// appendHistory adds today's point to the rolling series, collapsing
// the same-session retry case (already saw this date, overwrite the
// existing entry) and trimming to MaxHistoryPoints from the head.
// Idempotent — calling twice with the same point leaves the series
// unchanged.
func appendHistory(existing []HistoryPoint, point HistoryPoint) []HistoryPoint {
	out := slices.Clone(existing)
	if n := len(out); n > 0 && out[n-1].Date == point.Date {
		// Same-session re-refresh: overwrite the tail rather than
		// appending. Late prints or a forced re-run shouldn't widen
		// the series.
		out[n-1] = point
		return out
	}
	out = append(out, point)
	if len(out) > MaxHistoryPoints {
		out = out[len(out)-MaxHistoryPoints:]
	}
	return out
}

// mergeBars folds a list of fetched bars into an existing window.
// Each bar slides the window forward one day (or overwrites the
// tail if same-day). Duplicate dates between the fetched series and
// the existing window are collapsed — the cache stays clean even
// when the daily warm-fetch overlaps yesterday's bar.
func mergeBars(w ConstituentWindow, bars []Bar, symbol string) ConstituentWindow {
	if w.Symbol == "" {
		w.Symbol = symbol
	}
	for _, b := range bars {
		if w.LastBarAt != "" && b.Date <= w.LastBarAt && b.Date != w.LastBarAt {
			// Older than the last cached bar — ignore. The cache is
			// the source of truth for historical closes; a fetcher
			// that re-emits past dates shouldn't rewrite history.
			continue
		}
		w = SlideWindow(w, b.Close, b.Date)
	}
	return w
}

// nySessionKey returns the NY-tz date string identifying today's
// trading session. Mirrors gammaZeroCache.nySessionKey to keep
// session keys consistent across both indicator caches — they
// should agree on "what session am I in?" for the same wall-clock
// time. Falls back to UTC date if the zone fails to load.
func nySessionKey(now time.Time) string {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return now.In(loc).Format("2006-01-02")
	}
	return now.UTC().Format("2006-01-02")
}

// warnf is a nil-safe Logger.Warnf wrapper. The engine's logger is
// optional; nil silences all output (used in tests).
func (e *Engine) warnf(format string, args ...any) {
	if e.logger != nil {
		e.logger.Warnf(format, args...)
	}
}

// History returns up to `limit` trailing history points, oldest first.
// A non-positive limit returns the full retained series (bounded by
// MaxHistoryPoints). The returned slice is a defensive copy — callers
// can mutate freely without affecting engine state.
func (e *Engine) History(limit int) []HistoryPoint {
	e.mu.RLock()
	defer e.mu.RUnlock()
	src := e.history
	if limit > 0 && limit < len(src) {
		src = src[len(src)-limit:]
	}
	return slices.Clone(src)
}
