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

// belowThresholdRetryDelay is how long the scheduler waits before
// retrying a refresh that finished below MinCoverageFraction. Sized
// to give IBKR's per-account reqContractDetails bucket (~50 / 10 min,
// observed) time to refill — the dominant bottleneck during a 503-name
// cold-start fan-out. A previous refresh that resolved ~50 contracts
// drained the bucket; waiting 12 min lets the next attempt land another
// ~50 successful resolutions on top of the windows already persisted.
const belowThresholdRetryDelay = 12 * time.Minute

// maxBelowThresholdRetries caps how many back-to-back retries the
// scheduler performs before falling through to the daily cadence.
// With belowThresholdRetryDelay = 12 min and ~50 new resolutions per
// retry (IBKR's bucket math), 12 retries covers ~600 names — enough to
// converge from a cold start for the S&P 500. The cap exists so a
// genuinely broken gateway doesn't keep us in a tight retry loop
// indefinitely: after the limit, we fall through to the daily 16:35 ET
// wake-up and the operator can investigate.
const maxBelowThresholdRetries = 15

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

// Run starts the engine's scheduler. Returns when ctx is cancelled.
// Designed to be called once from the daemon's startup sequence inside
// a goroutine; multiple concurrent Run loops on the same Engine would
// compete on the refresh mutex without crashing, but the caller
// shouldn't do that.
//
// Lifecycle:
//   - On entry, check shouldRefreshOnStartup. If true, call Refresh
//     immediately so handleBreadthSPX has data ASAP.
//   - After each refresh, check coverage. If it converged (≥ threshold),
//     sleep until nextRefreshAt (daily cadence). If it didn't and
//     we're under the retry limit, sleep belowThresholdRetryDelay and
//     retry — letting accumulated windows + IBKR's refilled
//     reqContractDetails bucket push coverage higher on the next pass.
//   - On ctx.Done at any point: return cleanly. An in-flight Refresh
//     is cancelled via its context.
//
// Errors from Refresh are logged but do not stop the loop — the
// engine retries on the next tick. A transient gateway disconnect
// shouldn't take the daily cadence offline.
func (e *Engine) Run(ctx context.Context) {
	retries := 0
	doRefresh := func(reason string) {
		if err := e.Refresh(ctx); err != nil {
			e.warnf("breadth: %s refresh: %v", reason, err)
		}
	}

	if cur, _ := e.Get(); shouldRefreshOnStartup(cur, e.clock()) {
		doRefresh("bootstrap")
		if ctx.Err() != nil {
			return
		}
	}

	for {
		wait := e.nextWait(retries)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		doRefresh("scheduled")
		if ctx.Err() != nil {
			return
		}

		cov, mc := e.LastRefreshCoverage()
		converged := mc > 0 && cov >= int(MinCoverageFraction*float64(mc))
		switch {
		case converged:
			retries = 0
		case retries < maxBelowThresholdRetries:
			retries++
		default:
			// Burned through the retry budget without converging.
			// Reset and fall back to the daily cadence — the
			// operator should investigate (the warnf in finalise
			// already logged each below-threshold result).
			retries = 0
		}
	}
}

// nextWait returns how long Run should sleep before the next refresh.
// retries > 0 means the previous refresh was below threshold and we're
// in the retry phase; the wait is belowThresholdRetryDelay so IBKR's
// per-account contract-details bucket has time to refill. Otherwise
// we're in the steady-state daily cadence and wait until 16:35 ET.
func (e *Engine) nextWait(retries int) time.Duration {
	if retries > 0 {
		return belowThresholdRetryDelay
	}
	next := nextRefreshAt(e.clock())
	return max(next.Sub(e.clock()), 0)
}
