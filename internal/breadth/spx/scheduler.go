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

// errorRetryDelay is the back-off applied when Refresh returns an
// error — distinct from belowThresholdRetryDelay (which assumes a
// completed-but-partial fan-out limited by IBKR's reqContractDetails
// bucket). Refresh errors are transport-level failures (gateway down,
// bulk-connector not yet ready, ctx cancellation upstream); they
// resolve in seconds, not minutes, so a short fixed back-off is
// right. 30 s is long enough not to retry-storm a recovering gateway
// and short enough that a one-shot blip clears within one user-visible
// poll cycle.
//
// Before this distinction existed (≤v0.30.0) an errored Refresh
// incremented `retries` the same as a below-threshold result, so a
// startup-time gateway hiccup put the scheduler in a 12-min back-off
// loop for up to 3 hours (15 × 12 min) before falling through to the
// daily cadence — silent and surprising.
const errorRetryDelay = 30 * time.Second

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
//  2. The cached snapshot's AsOf wall-clock predates the most recent
//     past 16:35 ET weekday tick. Catches the case where the daemon
//     was down across yesterday's scheduled tick AND the cached
//     snapshot was a mid-session refresh from before yesterday's
//     close (SessionKey matches yesterday but data is pre-close
//     partial). Without this clause the scheduler would sit on the
//     stale partial-day snapshot until today's 16:35 ET.
//  3. The cached snapshot is for an older NY session AND today's
//     16:35 ET refresh window has already passed. We missed today's
//     scheduled wake-up (daemon was down or just installed) — run
//     now rather than wait until tomorrow.
//
// When none of these hold, the scheduler sleeps until the next 16:35
// ET tick.
//
// Weekends and US market holidays: lastTick rolls back over Sat/Sun
// so a Friday-close snapshot examined on Monday morning satisfies
// "AsOf ≥ last weekday tick" and does not force a refresh. Holidays
// aren't enumerated here — the engine's planner detects a no-new-bars
// session and the refresh is a cheap no-op anyway (per the file
// header at the refreshHourET constant).
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

	// Most recent past weekday 16:35 ET tick. Today's window if already
	// passed; otherwise yesterday's, rolling back across weekends.
	lastTick := todays
	if !lastTick.Before(localNow) {
		lastTick = lastTick.AddDate(0, 0, -1)
	}
	for lastTick.Weekday() == time.Saturday || lastTick.Weekday() == time.Sunday {
		lastTick = lastTick.AddDate(0, 0, -1)
	}
	if snap.AsOf.Before(lastTick) {
		return true
	}

	if localNow.Before(todays) {
		// We haven't reached today's window yet — yesterday's data is
		// still authoritative (and the AsOf check above confirmed it
		// post-dates the last weekday tick). No catch-up needed.
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
	defer e.setRetryPending(false)

	retries := 0
	lastErrored := false
	doRefresh := func(reason string) error {
		err := e.Refresh(ctx)
		if err != nil {
			e.warnf("breadth: %s refresh: %v", reason, err)
		}
		return err
	}
	// updateRetryState reads the post-refresh coverage signal and
	// adjusts the retry counter for the next loop iteration. Called
	// only after refreshes that COMPLETED (no transport error) so a
	// below-threshold result triggers the short retry cadence —
	// otherwise the bootstrap's below-threshold outcome would sit idle
	// until the next 16:35 ET tick, defeating the retry mechanism.
	// Refresh errors take the errorRetryDelay path instead.
	updateRetryState := func() {
		cov, mc := e.LastRefreshCoverage()
		converged := mc > 0 && cov >= int(MinCoverageFraction*float64(mc))
		switch {
		case converged:
			retries = 0
			e.setRetryPending(false)
		case retries < maxBelowThresholdRetries:
			retries++
			e.setRetryPending(true)
		default:
			// Burned through the retry budget without converging.
			// Reset and fall back to the daily cadence — the
			// operator should investigate (the warnf in finalise
			// already logged each below-threshold result).
			retries = 0
			e.setRetryPending(false)
		}
	}

	if cur, _ := e.Get(); shouldRefreshOnStartup(cur, e.clock()) {
		lastErrored = doRefresh("bootstrap") != nil
		if ctx.Err() != nil {
			return
		}
		if !lastErrored {
			updateRetryState()
		}
	}

	for {
		wait := e.nextWait(retries, lastErrored)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		lastErrored = doRefresh("scheduled") != nil
		if ctx.Err() != nil {
			return
		}
		if !lastErrored {
			updateRetryState()
		} else {
			e.setRetryPending(false)
		}
	}
}

// nextWait returns how long Run should sleep before the next refresh.
// Priority: a transport error gets the short errorRetryDelay (a
// recovering gateway clears in seconds). Below-threshold coverage
// (refresh completed but partial) gets belowThresholdRetryDelay so
// IBKR's per-account contract-details bucket has time to refill.
// Otherwise we're in the steady-state daily cadence and wait until
// 16:35 ET.
//
// The error path and coverage path are deliberately distinct: a
// startup-time gateway hiccup must not steal 12-min budget from the
// coverage retry mechanism (Bug 3 fix, v0.30.1).
func (e *Engine) nextWait(retries int, lastErrored bool) time.Duration {
	if lastErrored {
		return errorRetryDelay
	}
	if retries > 0 {
		return belowThresholdRetryDelay
	}
	next := nextRefreshAt(e.clock())
	return max(next.Sub(e.clock()), 0)
}
