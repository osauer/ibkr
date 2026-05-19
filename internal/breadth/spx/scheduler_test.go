package spx

import (
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
