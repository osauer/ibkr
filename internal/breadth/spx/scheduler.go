package spx

import (
	"context"
	"time"
)

// refreshHourET and refreshMinuteET name the daily wakeup time
// (16:35 America/New_York). 16:00 is the NYSE close; the 35-minute
// pad gives the gateway time to settle late prints and busted
// trades before we sample the daily-bar feed. S&P DJI publishes
// S5FI post-close as well, so an earlier wake would either miss
// today's data or race the gateway.
const (
	refreshHourET   = 16
	refreshMinuteET = 35
)

// nyLocation returns the America/New_York time.Location, falling back
// to UTC if the zoneinfo database isn't available on this host. The
// fallback degrades cadence (a daemon on a UTC-only container would
// run at 16:35 UTC, which is mid-US-session) but never blocks the
// scheduler from running.
func nyLocation() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.UTC
}

// nextRefreshAt returns the next NY-tz refresh wakeup at or after
// `now`. If `now` is before today's 16:35 ET, the result is today's
// 16:35 ET; otherwise tomorrow's. Weekends and US market holidays
// are not skipped here — the engine's planner detects a no-new-bars
// session via the LastBarAt comparison and the refresh is a cheap
// no-op on non-trading days.
func nextRefreshAt(now time.Time) time.Time {
	loc := nyLocation()
	localNow := now.In(loc)
	candidate := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day(),
		refreshHourET, refreshMinuteET, 0, 0, loc,
	)
	if !candidate.After(localNow) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

// shouldRefreshOnStartup reports whether the engine should run a
// catch-up Refresh as soon as Run() starts, before settling into the
// daily cadence. The conditions are:
//
//  1. No snapshot has ever been computed (cold install). Always run.
//  2. The cached snapshot is for an older NY session AND today's
//     16:35 ET refresh window has already passed. We missed today's
//     scheduled wake-up (daemon was down or just installed) — run
//     now rather than wait until tomorrow.
//
// When neither condition holds, the scheduler sleeps until the next
// 16:35 ET tick.
func shouldRefreshOnStartup(snap *Snapshot, now time.Time) bool {
	if snap == nil {
		return true
	}
	loc := nyLocation()
	localNow := now.In(loc)
	todays := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day(),
		refreshHourET, refreshMinuteET, 0, 0, loc,
	)
	if localNow.Before(todays) {
		// We haven't reached today's window yet — yesterday's data is
		// still authoritative. No catch-up needed.
		return false
	}
	return snap.SessionKey != nySessionKey(now)
}

// Run starts the engine's daily scheduler. Returns when ctx is
// cancelled. Designed to be called once from the daemon's startup
// sequence inside a goroutine; multiple concurrent Run loops on the
// same Engine would compete on the refresh mutex without crashing,
// but the caller shouldn't do that.
//
// Lifecycle:
//   - On entry, check shouldRefreshOnStartup. If true, call Refresh
//     immediately so handleBreadthSPX has data ASAP.
//   - Loop: sleep until nextRefreshAt; on wake, call Refresh; repeat.
//   - On ctx.Done at any point: return cleanly. An in-flight Refresh
//     is cancelled via its context.
//
// Errors from Refresh are logged but do not stop the loop — the
// engine retries on the next tick. A transient gateway disconnect
// shouldn't take the daily cadence offline.
func (e *Engine) Run(ctx context.Context) {
	if cur, _ := e.Get(); shouldRefreshOnStartup(cur, e.clock()) {
		if err := e.Refresh(ctx); err != nil {
			e.warnf("breadth: bootstrap refresh: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
	}

	for {
		next := nextRefreshAt(e.clock())
		wait := max(next.Sub(e.clock()), 0)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := e.Refresh(ctx); err != nil {
			e.warnf("breadth: scheduled refresh: %v", err)
		}
	}
}
