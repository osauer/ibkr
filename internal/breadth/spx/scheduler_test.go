package spx

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNextRefreshAtToday covers the simple case: it's morning ET,
// next wake is later today at 16:35.
func TestNextRefreshAtToday(t *testing.T) {
	loc := nyLocation()
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, loc) // 10:00 ET Monday
	got := nextRefreshAt(now)
	want := time.Date(2026, 5, 18, refreshHourET, refreshMinuteET, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("morning case: want %v, got %v", want, got)
	}
}

// TestNextRefreshAtTomorrow covers the post-close case: it's evening
// ET, we've passed today's 16:35, next wake is tomorrow.
func TestNextRefreshAtTomorrow(t *testing.T) {
	loc := nyLocation()
	now := time.Date(2026, 5, 18, 17, 0, 0, 0, loc) // 17:00 ET Monday
	got := nextRefreshAt(now)
	want := time.Date(2026, 5, 19, refreshHourET, refreshMinuteET, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("evening case: want %v, got %v", want, got)
	}
}

// TestNextRefreshAtExactlyOnBoundary pins the half-open interval: if
// now == today's 16:35 ET exactly, we treat that as "already past"
// and schedule tomorrow, not "right now". Avoids a tight loop if a
// daemon timer fires precisely on the boundary.
func TestNextRefreshAtExactlyOnBoundary(t *testing.T) {
	loc := nyLocation()
	now := time.Date(2026, 5, 18, refreshHourET, refreshMinuteET, 0, 0, loc)
	got := nextRefreshAt(now)
	want := time.Date(2026, 5, 19, refreshHourET, refreshMinuteET, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("boundary case: want %v, got %v", want, got)
	}
}

// TestNextRefreshAtCrossesMidnight pins behaviour around UTC midnight
// on a US date: a UTC-based daemon at 02:00 UTC Tuesday (22:00 ET
// Monday) should still pick today's-NY-date 16:35 — already past —
// so schedules tomorrow-NY 16:35.
func TestNextRefreshAtCrossesMidnight(t *testing.T) {
	now := time.Date(2026, 5, 19, 2, 0, 0, 0, time.UTC) // 22:00 ET Monday
	got := nextRefreshAt(now)
	loc := nyLocation()
	want := time.Date(2026, 5, 19, refreshHourET, refreshMinuteET, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("UTC-midnight case: want %v, got %v", want, got)
	}
}

// TestShouldRefreshOnStartupNoSnapshot is the cold-install case: no
// cache exists, so a catch-up is always wanted regardless of clock.
func TestShouldRefreshOnStartupNoSnapshot(t *testing.T) {
	loc := nyLocation()
	if !shouldRefreshOnStartup(nil, time.Date(2026, 5, 18, 9, 0, 0, 0, loc)) {
		t.Error("cold-install morning should refresh")
	}
	if !shouldRefreshOnStartup(nil, time.Date(2026, 5, 18, 22, 0, 0, 0, loc)) {
		t.Error("cold-install evening should refresh")
	}
}

// TestShouldRefreshOnStartupAlreadyToday covers the no-op startup
// case: cache already covers today's session.
func TestShouldRefreshOnStartupAlreadyToday(t *testing.T) {
	loc := nyLocation()
	now := time.Date(2026, 5, 18, 22, 0, 0, 0, loc)
	snap := &Snapshot{SessionKey: nySessionKey(now)}
	if shouldRefreshOnStartup(snap, now) {
		t.Error("cache for today's session should not trigger startup refresh")
	}
}

// TestShouldRefreshOnStartupCaughtBeforeWindow covers the morning-
// startup case: yesterday's cache, but today's 16:35 hasn't arrived
// yet — yesterday's data remains authoritative, no catch-up.
func TestShouldRefreshOnStartupCaughtBeforeWindow(t *testing.T) {
	loc := nyLocation()
	now := time.Date(2026, 5, 18, 9, 0, 0, 0, loc) // 09:00 ET
	yesterday := nySessionKey(now.AddDate(0, 0, -1))
	snap := &Snapshot{SessionKey: yesterday}
	if shouldRefreshOnStartup(snap, now) {
		t.Error("morning startup with yesterday's cache should wait for 16:35, not refresh now")
	}
}

// TestShouldRefreshOnStartupMissedYesterday covers the catch-up case:
// yesterday's cache, daemon comes up after 16:35 — we missed the
// window, run a refresh immediately rather than wait until tomorrow.
func TestShouldRefreshOnStartupMissedYesterday(t *testing.T) {
	loc := nyLocation()
	now := time.Date(2026, 5, 18, 17, 0, 0, 0, loc) // 17:00 ET, past window
	yesterday := nySessionKey(now.AddDate(0, 0, -1))
	snap := &Snapshot{SessionKey: yesterday}
	if !shouldRefreshOnStartup(snap, now) {
		t.Error("evening startup with yesterday's cache should catch up immediately")
	}
}

// TestRunBootstrapBelowThresholdSchedulesRetry pins the contract that
// when Run's bootstrap refresh finishes below MinCoverageFraction, the
// next wake is the short retry delay — NOT the daily 16:35 ET cadence.
// Before the fix, Run only consulted LastRefreshCoverage after each
// scheduled iteration of the main loop, so a below-threshold bootstrap
// would sleep ~24 hours before retrying. With ~50 contract resolutions
// per IBKR-bucket cycle and 503 SPX names, that translated to "cache
// never converges in any reasonable timeframe."
//
// The test runs Run in a goroutine with a fake clock pinned just past
// today's 16:35 ET, so nextRefreshAt resolves to tomorrow's 16:35 —
// any "we didn't notice the bootstrap signal" bug would manifest as
// the scheduler sleeping until tomorrow. Cancellation after the bootstrap
// + a short grace window proves the loop entered the retry phase
// (belowThresholdRetryDelay) rather than the daily phase.
func TestRunBootstrapBelowThresholdSchedulesRetry(t *testing.T) {
	loc := nyLocation()
	// 17:00 ET — past today's 16:35 ET window — so nextRefreshAt would
	// resolve to tomorrow's 16:35 if Run mistakenly entered the daily
	// path.
	now := time.Date(2026, 5, 18, 17, 0, 0, 0, loc)

	// 10 members, only 5 succeed → 50% coverage, well below threshold.
	members := []string{"OK1", "OK2", "OK3", "OK4", "OK5", "F1", "F2", "F3", "F4", "F5"}
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"OK1": makeSeries(100, 1, WindowSize, now),
			"OK2": makeSeries(50, 1, WindowSize, now),
			"OK3": makeSeries(75, 1, WindowSize, now),
			"OK4": makeSeries(60, 1, WindowSize, now),
			"OK5": makeSeries(80, 1, WindowSize, now),
		},
		Errors: map[string]error{
			"F1": errors.New("gateway: pacing"),
			"F2": errors.New("gateway: pacing"),
			"F3": errors.New("gateway: pacing"),
			"F4": errors.New("gateway: pacing"),
			"F5": errors.New("gateway: pacing"),
		},
	}
	dir := t.TempDir()
	store := NewStore(dir)
	e := New(store, fake, Options{Clock: frozenClock(now), Workers: 4})
	e.members = members

	// Run() in a goroutine, cancel after enough time for bootstrap to
	// complete and the next-wait timer to be installed. The bootstrap
	// runs synchronously in Run before the timer; cancelling here
	// proves either (a) bootstrap completed AND retry-state was
	// updated, OR (b) bootstrap completed but Run sleeps for ~24h —
	// the latter is the bug we're pinning against.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(done)
	}()

	// Wait briefly for bootstrap, then cancel. A second is plenty for
	// a 10-member fake fetcher with no latency; if Run gets stuck
	// pre-cancel for longer, the test fails — that's the kind of
	// regression we want to surface.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit within 1s of cancel — likely blocked in nextWait")
	}

	// Sanity: bootstrap ran and recorded below-threshold coverage.
	cov, mc := e.LastRefreshCoverage()
	if cov != 5 || mc != 10 {
		t.Errorf("bootstrap coverage: want (5, 10), got (%d, %d)", cov, mc)
	}
	// Windows persisted (the engine-side accumulation contract).
	if got := len(e.windows); got != 5 {
		t.Errorf("in-memory windows after bootstrap: want 5, got %d", got)
	}
}

// TestAppendHistorySameSessionOverwrites covers the re-refresh case:
// running Refresh twice in the same NY session shouldn't grow the
// history series — the second point overwrites the first.
func TestAppendHistorySameSessionOverwrites(t *testing.T) {
	existing := []HistoryPoint{
		{Date: "2026-05-17", Value: 58.4},
		{Date: "2026-05-18", Value: 60.1},
	}
	got := appendHistory(existing, HistoryPoint{Date: "2026-05-18", Value: 61.2})
	if len(got) != 2 {
		t.Errorf("length: want 2, got %d", len(got))
	}
	if got[1].Value != 61.2 {
		t.Errorf("tail value: want 61.2, got %v", got[1].Value)
	}
}

// TestAppendHistoryNewSessionAppends covers the steady-state daily
// case: a new date appends.
func TestAppendHistoryNewSessionAppends(t *testing.T) {
	existing := []HistoryPoint{{Date: "2026-05-17", Value: 58.4}}
	got := appendHistory(existing, HistoryPoint{Date: "2026-05-18", Value: 60.1})
	if len(got) != 2 {
		t.Errorf("length: want 2, got %d", len(got))
	}
	if got[1].Date != "2026-05-18" || got[1].Value != 60.1 {
		t.Errorf("appended point: got %+v", got[1])
	}
}

// TestAppendHistoryTrimsAtMax covers the rollover: appending past
// MaxHistoryPoints drops the oldest entries.
func TestAppendHistoryTrimsAtMax(t *testing.T) {
	existing := make([]HistoryPoint, MaxHistoryPoints)
	for i := range existing {
		existing[i] = HistoryPoint{
			Date:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02"),
			Value: float64(i),
		}
	}
	got := appendHistory(existing, HistoryPoint{Date: "2026-12-01", Value: 99.9})
	if len(got) != MaxHistoryPoints {
		t.Errorf("trim: want length %d, got %d", MaxHistoryPoints, len(got))
	}
	if got[len(got)-1].Date != "2026-12-01" {
		t.Errorf("tail: want 2026-12-01, got %s", got[len(got)-1].Date)
	}
	// The original index-0 entry (2026-01-01) must have been pushed
	// off the head.
	if got[0].Date == existing[0].Date {
		t.Errorf("oldest entry should have been trimmed, but still present at head")
	}
}
