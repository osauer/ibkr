package daemon

import (
	"context"
	"fmt"
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
type gammaZeroCache struct {
	mu      sync.Mutex
	current *gammaComputation // nil until first kickOrJoin
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
func (c *gammaZeroCache) kickOrJoin(parent context.Context, now time.Time, etaSeconds int, compute computeFn) (job *gammaComputation, fresh bool) {
	key := nySessionKey(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.current != nil && c.current.sessionKey == key {
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
	return c.startLocked(parent, key, now, etaSeconds, compute)
}

// startLocked allocates and launches a fresh compute. Caller must
// hold c.mu. The compute goroutine owns the gammaComputation pointer
// post-return; callers only read fields after the done channel
// closes.
func (c *gammaZeroCache) startLocked(parent context.Context, key string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
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
	c.current = job

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

// snapshot extracts a wire-shape envelope from the current job state.
// Always returns a populated Status; Result is set only on success.
//
// nowFn is injectable for tests — production callers pass time.Now.
func (c *gammaZeroCache) snapshot(g *gammaComputation, nowFn func() time.Time) rpc.GammaZeroSPXResult {
	if g == nil {
		return rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusComputing}
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
		return env
	}
	progress := g.progress.Load()
	env.Status = rpc.GammaZeroStatusComputing
	env.EtaSeconds = remainingEta(g, nowFn(), progress)
	env.Progress = int(progress)
	return env
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
