package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// gammaZeroCache holds the current and most-recent zero-gamma compute
// for SPX, indexed by NY trading-session date. Two concerns intersect
// here that the existing ttlMap-style caches don't handle:
//
//  1. Singleflight. The compute is a multi-minute fan-out across
//     hundreds of option legs against a shared market-data slot pool.
//     Two concurrent callers must share one in-flight job, not run
//     duplicate fan-outs that compete for the same gateway slots.
//
//  2. Background lifetime. The compute outlives any single RPC
//     context — the first caller of the day kicks off a job that the
//     daemon completes regardless of whether the original client
//     hangs around. Subsequent pollers see the in-flight job's
//     progress and pick up the result when it's done.
//
// Session key is derived in America/New_York: the result is cached
// for the rest of the same NY trading day and rolls over at midnight
// NY. DST is handled by time.LoadLocation; if the zone fails to load
// the cache falls back to UTC date, which is safe but slightly less
// useful for international callers.
//
// Soft-TTL refresh-while-stale: when a kickOrJoin caller hits a
// cached successful result older than gammaSoftTTL, the cache serves
// the stale value immediately AND kicks a refresh in the background.
// The refresh is stored in `refresh`, distinct from `current`, so
// further callers during the refresh keep seeing the stable served
// value rather than blocking on the new in-flight job. On completion
// the refresh promotes to current; on error it's discarded so a
// transient compute failure can't poison a known-good cached value.
type gammaZeroCache struct {
	mu      sync.Mutex
	current *gammaComputation // nil until first kickOrJoin
	refresh *gammaComputation // soft-TTL refresh in flight behind current; nil otherwise
	// lastErr / lastErrAt / lastErrSummary retain the prior failure
	// context across the gammaErrorRetryTTL boundary. Without this, a
	// caller polling during a retry window sees Status=Computing with
	// no indication that the previous compute failed — the error was
	// silently discarded the moment kickOrJoin kicked a fresh attempt.
	// Cleared on a successful compute landing.
	lastErr        error
	lastErrAt      time.Time
	lastErrSummary string // shortened single-line summary for rendering
}

// gammaComputation is one zero-gamma run from kickoff through result
// retrieval. The done channel is closed exactly once when the result
// (success or error) is finalised; readers Select on it to wait.
//
// progress is updated atomically by the compute goroutine; readers
// load it without holding any cache lock so a poller during a long
// compute doesn't block on the cache mutex.
type gammaComputation struct {
	sessionKey string        // e.g., "2026-05-16"
	startedAt  time.Time     // kickoff wall-clock
	done       chan struct{} // closed once result or err is set
	result     *rpc.GammaZeroComputed
	err        error
	cancel     context.CancelFunc // bounds the bg goroutine; called on superseding compute
	progress   atomic.Int32       // 0–100, best-effort
	etaSeconds int                // static estimate captured at kickoff
}

// gammaErrorRetryTTL is the minimum age of a cached error before
// kickOrJoin re-attempts. Before this fix a single transient
// gateway-side timeout (e.g. cold-start SPX contract-details race)
// would stick in cache and poison every regime/gamma call for the
// rest of the NY trading session — confirmed observed at v0.22.0.
//
// "Age" is measured against startedAt rather than the deferred
// finishedAt: the cache's now parameter and startedAt share a clock,
// so the TTL check stays testable with synthetic times, and the
// production semantic ("60 s since we kicked the failing attempt") is
// the right one — long error paths shouldn't have an extra 60-s
// quiet period on top of their own duration.
//
// 60 s is long enough to dampen retry storms against a genuinely down
// gateway while short enough that a one-shot blip clears on the
// user's next normal poll.
const gammaErrorRetryTTL = 60 * time.Second

// gammaSoftTTL is the age above which a cached successful compute
// triggers a background refresh on the next kickOrJoin. The served
// value is still returned immediately; the refresh runs behind it
// and the next caller picks up the new result.
//
// 5 min trades off cost against drift. Dealer positioning shifts
// slowly intraday (positions reshuffle on the order of hours, not
// minutes), but the spot the GEX curve is anchored on does move
// minute-to-minute, and a stale γ-zero relative to current spot is
// the part users care about. 5 min keeps the cached answer within
// roughly one ATR-style move for liquid index ETFs without spinning
// the gateway's option-quote slots continuously.
//
// Distinct from gammaErrorRetryTTL (60 s): that one re-attempts a
// FAILED compute on the next call; this one rolls a SUCCESSFUL
// compute forward while still serving the prior value.
const gammaSoftTTL = 5 * time.Minute

// isDone reports whether the compute has finished (success or error).
// Safe for concurrent readers — the done channel close is the
// happens-before signal that the result / err fields are stable.
func (g *gammaComputation) isDone() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

func newGammaZeroCache() *gammaZeroCache {
	return &gammaZeroCache{}
}

// IsComputing reports whether a gamma compute is currently in flight.
// Used by Server.isBusy() so the daemon's idle watcher doesn't shut
// down while a multi-minute compute is still running (a fresh
// `ibkr regime` call kicks gamma and returns immediately; the user
// can walk away and the compute should still complete). Safe for
// concurrent callers — read under c.mu, follows the same lock
// discipline as kickOrJoin.
//
// A soft-TTL refresh counts as in-flight even when current is done:
// the user can see "computing dealer zero-gamma" in `ibkr status`
// whenever any compute is happening, so they understand why the
// next call may briefly carry a stale AsOf.
func (c *gammaZeroCache) IsComputing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil && !c.current.isDone() {
		return true
	}
	if c.refresh != nil && !c.refresh.isDone() {
		return true
	}
	return false
}

// nySessionKey returns the NY-tz date string that identifies the
// trading session a compute belongs to. Computed at every cache
// lookup so DST transitions and timezone surprises don't poison a
// cache key. Falls back to UTC date if the zone fails to load —
// guarantees a stable string under all conditions.
func nySessionKey(now time.Time) string {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return now.In(loc).Format("2006-01-02")
	}
	return now.UTC().Format("2006-01-02")
}

// computeFn is the contract the cache calls when it needs to kick a
// fresh compute. It runs on the daemon's goroutine, not the caller's,
// and gets a context bounded by the bg goroutine's lifetime (cancelled
// when the compute is superseded or the daemon shuts down).
//
// Implementations should update progress periodically (the cache
// stamps the etaSeconds once at kickoff but doesn't touch progress —
// the compute owns it).
type computeFn func(ctx context.Context, progress *atomic.Int32) (*rpc.GammaZeroComputed, error)

// kickOrJoin returns the active or most-recent computation for the
// current NY session. If a fresh compute is needed, it's started in
// a background goroutine via compute(); the returned gammaComputation
// may be in-flight or already complete. Concurrent callers always
// share — only one fan-out per session per non-force call.
//
// fresh is true when this call started the compute, false when an
// existing computation was returned (in-flight or finished).
//
// etaSeconds is the static initial estimate the cache stamps on a
// fresh kickoff. The compute reports refined progress via its
// atomic counter.
//
// Soft-TTL refresh: when the served result is older than gammaSoftTTL,
// kickOrJoin kicks a background refresh while still returning the
// stale value. fresh stays false for the caller (they got the cached
// envelope, not a new in-flight job). The next caller after the
// refresh lands sees the new value.
func (c *gammaZeroCache) kickOrJoin(parent context.Context, now time.Time, etaSeconds int, compute computeFn) (job *gammaComputation, fresh bool) {
	key := nySessionKey(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Promote a landed soft-TTL refresh; discard a failed one. A
	// failed refresh must NOT poison a known-good cached value —
	// the existing current stays in place and the next caller can
	// trigger another refresh attempt past the soft TTL.
	if c.refresh != nil && c.refresh.isDone() {
		if c.refresh.err == nil && c.refresh.sessionKey == key {
			c.current = c.refresh
		}
		c.refresh = nil
	}

	if c.current != nil && c.current.sessionKey == key {
		// Same session — but a cached error past the retry TTL must NOT
		// block fresh attempts. Without this check, a one-shot
		// gateway-side timeout poisons every regime/gamma call for the
		// rest of the NY trading session. In-flight jobs (isDone=false)
		// always pass through to the shared singleflight regardless of
		// age; success results stay sticky for the whole session.
		if c.current.isDone() && c.current.err != nil && now.Sub(c.current.startedAt) >= gammaErrorRetryTTL {
			// Retain the failure context so the next render of the
			// "computing" row can surface "retry of <error> at HH:MM:SS"
			// instead of silently switching to a clean Computing state.
			c.lastErr = c.current.err
			c.lastErrAt = c.current.startedAt
			c.lastErrSummary = summarizeGammaErr(c.current.err)
			job = c.startLocked(parent, key, now, etaSeconds, compute)
			return job, true
		}
		// Soft-TTL: if the served value is stale and no refresh is
		// already running, kick one behind it. The caller still gets
		// c.current immediately — refresh is fire-and-forget.
		if c.current.isDone() && c.current.err == nil && c.refresh == nil &&
			now.Sub(c.current.startedAt) >= gammaSoftTTL {
			c.refresh = c.spawnJob(parent, key, now, etaSeconds, compute)
		}
		// Same session: serve the in-flight or recently-completed job.
		// Callers that need to bypass cache (diagnostics) use force().
		return c.current, false
	}

	job = c.startLocked(parent, key, now, etaSeconds, compute)
	return job, true
}

// force unconditionally starts a fresh compute for the current NY
// session, superseding any in-flight job for the same key. The
// previous job's context is cancelled — its result (if any) is
// discarded; the new job becomes c.current. Use sparingly: this
// throws away work and competes for the gateway's market-data slots
// against the cancelled job's last few in-flight subscriptions.
//
// Any in-flight soft-TTL refresh is also cancelled and discarded —
// force always lands as the canonical current, not as a refresh
// behind a stale value.
func (c *gammaZeroCache) force(parent context.Context, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	key := nySessionKey(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.current != nil && !c.current.isDone() {
		// Cancel the superseded compute — it stops fanning out and the
		// next time the gateway responds to one of its in-flight legs
		// the worker returns immediately. The done channel doesn't
		// close (the goroutine returns without setting result), so
		// any caller still waiting on the old job will block until
		// its own ctx times out. That's the documented force tradeoff.
		c.current.cancel()
	}
	if c.refresh != nil && !c.refresh.isDone() {
		c.refresh.cancel()
	}
	c.refresh = nil
	return c.startLocked(parent, key, now, etaSeconds, compute)
}

// spawnJob allocates a fresh computation and launches its background
// goroutine. Caller must hold c.mu. Does NOT touch c.current — the
// caller decides whether to assign it as canonical (startLocked) or
// hold it separately as the soft-TTL refresh.
func (c *gammaZeroCache) spawnJob(parent context.Context, key string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	// Decouple the compute's lifetime from any single RPC ctx. Use the
	// daemon's parent context (typically Background) as the upstream
	// signal so daemon shutdown still cancels the compute, but a
	// client disconnect mid-compute doesn't kill a job that other
	// pollers are waiting on.
	bgCtx, cancel := context.WithCancel(parent)
	job := &gammaComputation{
		sessionKey: key,
		startedAt:  now,
		done:       make(chan struct{}),
		cancel:     cancel,
		etaSeconds: etaSeconds,
	}

	go func() {
		defer close(job.done)
		// Best-effort panic guard: a math bug or nil pointer deep in
		// the compute pipeline shouldn't take down the daemon. The
		// recovered error becomes job.err, which surfaces to callers
		// as Status=error on the next poll.
		defer func() {
			if r := recover(); r != nil {
				job.err = fmt.Errorf("zero-gamma compute panicked: %v", r)
			}
		}()
		res, err := compute(bgCtx, &job.progress)
		if err != nil {
			job.err = err
			return
		}
		job.result = res
	}()

	return job
}

// startLocked allocates and launches a fresh compute, assigning the
// new job to c.current. Caller must hold c.mu. Thin wrapper over
// spawnJob — kept distinct because most kick paths want the new job
// to become the canonical served value, while the soft-TTL refresh
// path holds the new job in c.refresh until it lands cleanly.
func (c *gammaZeroCache) startLocked(parent context.Context, key string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	job := c.spawnJob(parent, key, now, etaSeconds, compute)
	c.current = job
	return job
}

// snapshot extracts a wire-shape envelope from the current job state.
// Always returns a populated Status; Result is set only on success.
//
// g == nil means no compute has ever been kicked this NY trading
// session — distinct from Computing (compute in flight). Pre-v0.27.9
// snapshot conflated these two under Computing, so a consumer
// couldn't tell "first caller of the day must kick" from "the kick
// is already running." Mirror of the v0.27.3 breadth Cold state.
//
// nowFn is injectable for tests — production callers pass time.Now.
func (c *gammaZeroCache) snapshot(g *gammaComputation, nowFn func() time.Time) rpc.GammaZeroSPXResult {
	if g == nil {
		return rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusCold}
	}
	started := g.startedAt
	env := rpc.GammaZeroSPXResult{
		StartedAt: &started,
	}
	if g.isDone() {
		if g.err != nil {
			env.Status = rpc.GammaZeroStatusError
			env.Error = g.err.Error()
			return env
		}
		env.Status = rpc.GammaZeroStatusReady
		env.Result = g.result
		// Clear stale prior-error context — a successful compute means
		// the previous failure is no longer informative.
		c.mu.Lock()
		c.lastErr = nil
		c.lastErrSummary = ""
		c.mu.Unlock()
		return env
	}
	progress := g.progress.Load()
	env.Status = rpc.GammaZeroStatusComputing
	env.EtaSeconds = remainingEta(g, nowFn(), progress)
	env.Progress = int(progress)
	// Attach prior-failure context if the current in-flight compute is
	// a retry of a recent failure. The renderer uses this to display
	// "computing · retry of <summary> at HH:MM:SS" instead of dropping
	// the prior error silently.
	c.mu.Lock()
	if c.lastErr != nil {
		retryAt := c.lastErrAt
		env.RetryOfErrorAt = &retryAt
		env.RetryOfErrorSummary = c.lastErrSummary
	}
	c.mu.Unlock()
	return env
}

// summarizeGammaErr compresses a compute error to a single-line summary
// suitable for rendering inline with "computing · retry of <summary>".
// Strips the long-tail context ("not persisting; gammaErrorRetryTTL ...")
// that's useful in logs but not in the dashboard. Returns the original
// string when it's already short enough.
func summarizeGammaErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.Index(msg, " — "); i > 0 {
		msg = msg[:i]
	}
	if i := strings.Index(msg, ". "); i > 0 {
		msg = msg[:i]
	}
	// "zero-gamma: " prefix is daemon-internal jargon; trim it.
	msg = strings.TrimPrefix(msg, "zero-gamma: ")
	if len(msg) > 80 {
		msg = msg[:77] + "..."
	}
	return msg
}

// remainingEta returns a refined ETA in seconds. Once enough work has
// landed (progress > 5), the estimate is projected from elapsed time:
// remaining ≈ elapsed × (100 - progress) / progress. Before then the
// projection is meaningless on tiny samples, so we fall back to the
// static initial estimate minus elapsed.
//
// Capped at 4× the static estimate so a stalled compute (progress
// frozen at 10 % after 10 minutes) doesn't surface absurd
// projections. Floor at 5s so the renderer doesn't flicker between
// "0s" and "computing" near the end of the run.
func remainingEta(g *gammaComputation, now time.Time, progress int32) int {
	elapsed := int(now.Sub(g.startedAt).Seconds())
	cap := 4 * g.etaSeconds
	var remaining int
	if progress > 5 {
		remaining = int(float64(elapsed) * float64(100-progress) / float64(progress))
	} else {
		remaining = g.etaSeconds - elapsed
	}
	remaining = min(remaining, cap)
	return max(remaining, 5)
}

// findZeroCrossing scans the sweep profile and returns the spot at
// which the signed GEX crosses zero, via linear interpolation between
// the two adjacent points that bracket the crossing. The profile is
// assumed sorted by Spot ascending — the caller is responsible for
// that invariant.
//
// Returns:
//   - (price, "") on a clean crossing.
//   - (nil, "positive") when GEX is non-negative across the entire
//     sweep (dealer book is long-gamma in every scenario considered).
//   - (nil, "negative") when GEX is non-positive across the entire
//     sweep (short-gamma regime).
//   - (nil, "no_data") when the profile is empty or has fewer than
//     two points (caller bug).
//
// The interpolation is intentionally linear: dealer GEX is smooth
// over a 30 %-wide spot sweep, but the sweep itself is sampled at
// 60 evenly-spaced points, so a finer-grain interp model would imply
// precision the sampling doesn't support. Linear is honest.
func findZeroCrossing(profile []rpc.GammaProfilePoint) (zeroGamma *float64, sign string) {
	if len(profile) < 2 {
		return nil, "no_data"
	}
	allPositive := true
	allNegative := true
	for _, p := range profile {
		if p.GEX < 0 {
			allPositive = false
		}
		if p.GEX > 0 {
			allNegative = false
		}
	}
	if allPositive {
		return nil, "positive"
	}
	if allNegative {
		return nil, "negative"
	}
	// At this point at least one pair brackets the zero. Walk and
	// interpolate on the FIRST bracketing pair — for dealer-gamma the
	// sign function is monotone across the sweep range in practice
	// (no multi-cross), but if it ever isn't, the renderer's profile
	// chart will surface the anomaly and the user can investigate.
	for i := 1; i < len(profile); i++ {
		prev := profile[i-1]
		curr := profile[i]
		if (prev.GEX > 0 && curr.GEX < 0) || (prev.GEX < 0 && curr.GEX > 0) {
			// Linear interpolation: solve GEX(x) = 0 for x on the line
			// through (prev.Spot, prev.GEX) and (curr.Spot, curr.GEX).
			//   x = prev.Spot - prev.GEX × (curr.Spot - prev.Spot) / (curr.GEX - prev.GEX)
			x := prev.Spot - prev.GEX*(curr.Spot-prev.Spot)/(curr.GEX-prev.GEX)
			return &x, ""
		}
		// Exact zero at a sample point — interpolate degenerates to
		// the sample's own spot.
		if prev.GEX == 0 {
			x := prev.Spot
			return &x, ""
		}
		if i == len(profile)-1 && curr.GEX == 0 {
			x := curr.Spot
			return &x, ""
		}
	}
	// Shouldn't reach here given the allPositive/allNegative gates
	// above, but defensive: no crossing found despite mixed signs.
	return nil, "no_data"
}
