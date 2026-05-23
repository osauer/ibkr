package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// gammaZeroCache holds the current and most-recent zero-gamma compute
// for each scope (combined / SPY / SPX), indexed by NY trading-session
// date. Three concerns intersect here that the existing ttlMap-style
// caches don't handle:
//
//  1. Singleflight per scope. The compute is a multi-minute fan-out
//     across hundreds of option legs against a shared market-data
//     slot pool. Two concurrent callers requesting the SAME scope
//     must share one in-flight job; two concurrent callers requesting
//     DIFFERENT scopes (e.g. combined + --only=spy) must NOT collide
//     on the cache and silently overwrite each other's results.
//
//  2. Background lifetime. The compute outlives any single RPC
//     context — the first caller of the day kicks off a job that the
//     daemon completes regardless of whether the original client
//     hangs around. Subsequent pollers see the in-flight job's
//     progress and pick up the result when it's done.
//
//  3. Scope isolation. The result envelope shape differs across
//     scopes (combined carries PerIndex / RegimeAgreement; SPY-only
//     and SPX-only carry single-underlying-shaped payloads). A
//     scope-mixed cache slot would surface the wrong shape to the
//     wrong caller.
//
// Session key is derived in America/New_York: the result is cached
// for the rest of the same NY trading day and rolls over at midnight
// NY. DST is handled by time.LoadLocation; if the zone fails to load
// the cache falls back to UTC date, which is safe but slightly less
// useful for international callers.
//
// Soft-TTL refresh-while-stale: when a kickOrJoin caller hits a
// cached successful result that's either older than the current
// session class's softTTL (60 min RTH, 30 min pre/post, ∞ closed)
// OR was computed in a different active session class than now,
// the cache serves the stale value immediately AND kicks a refresh
// in the background. The refresh is stored in the slot's `refresh`
// field, distinct from `current`, so further callers during the
// refresh keep seeing the stable served value rather than blocking
// on the new in-flight job. On completion the refresh promotes to
// current; on error it's discarded so a transient compute failure
// can't poison a known-good cached value.
type gammaZeroCache struct {
	mu sync.Mutex
	// slots holds one entry per scope. Key is the scope string
	// (rpc.GammaZeroScope* constants). A nil/missing entry means no
	// compute has ever been kicked for that scope this session.
	// Each slot owns its own current/refresh/lastErr lifecycle so
	// the three scopes can't step on each other's state.
	slots map[string]*gammaSlot

	// store is the optional on-disk persistence layer. nil = pure
	// in-memory (tests use this). Set once at construction by
	// newGammaZeroCacheWithStore — never modified after, so reads
	// from the spawnJob goroutine don't need the cache mutex.
	store *gammaZeroStore
	// log is the logger used for persistence warnings. nil-safe via
	// gammaLogf wrapper.
	log gammaLogger
}

// gammaSlot is the per-scope cache cell. Mirrors the original
// single-slot fields of gammaZeroCache: current, refresh, plus the
// last-error retention that powers the "retry of X" rendering across
// the gammaErrorRetryTTL boundary. Each scope owns one slot.
type gammaSlot struct {
	current *gammaComputation // nil until first kickOrJoin for this scope
	refresh *gammaComputation // soft-TTL refresh in flight behind current; nil otherwise
	// lastErr / lastErrAt / lastErrSummary retain the prior failure
	// context across the gammaErrorRetryTTL boundary. Without this,
	// a caller polling during a retry window sees Status=Computing
	// with no indication that the previous compute failed — the
	// error was silently discarded the moment kickOrJoin kicked a
	// fresh attempt. Cleared on a successful compute landing.
	lastErr        error
	lastErrAt      time.Time
	lastErrSummary string // shortened single-line summary for rendering
}

// getOrCreateSlotLocked returns the slot for scope, creating an empty
// one on first access. Caller must hold c.mu.
func (c *gammaZeroCache) getOrCreateSlotLocked(scope string) *gammaSlot {
	if c.slots == nil {
		c.slots = make(map[string]*gammaSlot, 3)
	}
	if s, ok := c.slots[scope]; ok {
		return s
	}
	s := &gammaSlot{}
	c.slots[scope] = s
	return s
}

// gammaComputation is one zero-gamma run from kickoff through result
// retrieval. The done channel is closed exactly once when the result
// (success or error) is finalised; readers Select on it to wait.
//
// progress is updated atomically by the compute goroutine; readers
// load it without holding any cache lock so a poller during a long
// compute doesn't block on the cache mutex.
//
// scope identifies which cache slot this computation belongs to, so
// snapshot() can look up the correct lastErr context without the
// caller having to pass it again. Mirrors sessionKey in being a
// stable property of the compute that the surrounding cache reads.
type gammaComputation struct {
	sessionKey string        // e.g., "2026-05-16"
	scope      string        // rpc.GammaZeroScope* — which slot owns this job
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

// Session-aware soft TTL: the age at which a cached successful
// compute triggers a background refresh. The served value is still
// returned immediately; the refresh runs behind it and the next
// caller picks up the new result.
//
// Per-class values:
//   - softTTLRTH (60 min): RTH dealer positioning shifts on the order
//     of hours; once per hour balances freshness against IBKR slot
//     pressure during peak load.
//   - softTTLPrePost (30 min): pre / post-market sees thinner flow
//     but real price moves around news and overnight events. Refresh
//     more often than RTH so users querying around the open/close
//     don't see a stale snapshot, but not so aggressively that we
//     burn slots on a thin tape.
//   - softTTLClosed (math.MaxInt64): no live price input → no point
//     refreshing. The persisted snapshot stays canonical until the
//     NY-midnight session-key boundary rolls. Effectively infinite
//     (math.MaxInt64 ≈ 292 years as a time.Duration).
//
// Class transitions trigger an additional refresh path in kickOrJoin:
// a snapshot computed in a different active class than `now` is
// treated as stale even if its absolute age is below softTTL — see
// the boundary-refresh block.
//
// Distinct from gammaErrorRetryTTL (60 s): that one re-attempts a
// FAILED compute on the next call; soft-TTL rolls a SUCCESSFUL
// compute forward while still serving the prior value.
const (
	softTTLRTH     = 60 * time.Minute
	softTTLPrePost = 30 * time.Minute
	softTTLClosed  = time.Duration(math.MaxInt64)
)

// softTTL returns the soft-TTL appropriate for the U.S. equity-options
// session class containing now. Caller passes the same now used for
// the age comparison so a single instant drives both the TTL choice
// and the age check, avoiding boundary-flap when the two sample times
// straddle a session edge.
func softTTL(now time.Time) time.Duration {
	switch rpc.ClassifySession(now) {
	case rpc.SessionClosed:
		return softTTLClosed
	case rpc.SessionPre, rpc.SessionPost:
		return softTTLPrePost
	default:
		return softTTLRTH
	}
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

// knownGammaScopes enumerates the scopes the cache will look for
// persisted state on startup. Adding a new scope means appending here;
// the cache treats `scope` as an opaque string at the data layer, so
// nothing outside this list breaks.
var knownGammaScopes = []string{
	rpc.GammaZeroScopeCombined,
	rpc.GammaZeroScopeSPY,
	rpc.GammaZeroScopeSPX,
}

// newGammaZeroCacheWithStore returns a cache wired to an on-disk
// store. The store is consulted immediately for each known scope: any
// scope holding a result keyed to today's NY trading session has its
// slot seeded with a synthetic already-done gammaComputation, so the
// first caller for that scope skips the multi-minute compute.
//
// Per-scope independence is the load-bearing guarantee — a stale
// SPY-only file doesn't block a combined call from kicking, and vice
// versa.
//
// Persistence errors during load are warnings, not failures —
// returning a cache with empty slots is fine, the next caller for that
// scope kicks a fresh compute as it would with a pure in-memory cache.
//
// log may be nil; gammaLogf nil-safe-wraps it.
func newGammaZeroCacheWithStore(store *gammaZeroStore, now time.Time, log gammaLogger) *gammaZeroCache {
	c := &gammaZeroCache{
		slots: make(map[string]*gammaSlot, len(knownGammaScopes)),
		store: store,
		log:   log,
	}
	if store == nil {
		return c
	}
	wrap := gammaLogf{inner: log}
	offHours := rpc.ClassifySession(now) == rpc.SessionClosed
	for _, scope := range knownGammaScopes {
		persisted, err := store.Load(scope, now)
		if err != nil {
			wrap.Warnf("gamma cache: load persisted scope=%s: %v (cold start for this scope)", scope, err)
			continue
		}
		if persisted == nil && offHours {
			// Off-hours fallback: today's session-key gate didn't
			// match, but we'd rather serve yesterday's compute
			// (flagged stale via cache_stale_off_hours when age > 24h)
			// than force the user to wait until the next session open
			// for any γ-zero answer. See kickOrJoin's SessionClosed
			// gate for the serve-only-never-kick guarantee.
			stale, stErr := store.LoadStale(scope)
			if stErr != nil {
				wrap.Warnf("gamma cache: load stale scope=%s: %v (cold start for this scope)", scope, stErr)
				continue
			}
			if stale == nil {
				continue
			}
			persisted = stale
		}
		if persisted == nil {
			continue
		}
		c.slots[scope] = &gammaSlot{current: newPersistedComputation(persisted, scope, now)}
		wrap.Infof("gamma cache: loaded persisted result scope=%s session=%s as_of=%s",
			scope, nySessionKey(now), persisted.AsOf.Format(time.RFC3339))
	}
	return c
}

// newPersistedComputation wraps a persisted result in a
// gammaComputation that looks already-done to the rest of the cache.
// The done channel is closed immediately; result is set; err and
// cancel are zero-valued (cancel is only used to abort in-flight
// computes, which this isn't).
//
// scope is stored on the gammaComputation so snapshot() can route
// lastErr reads to the right slot without needing the caller to pass
// it again.
//
// startedAt is the persisted AsOf so the soft-TTL refresh trigger
// fires on the correct elapsed window — a result persisted 40 min
// ago should be refreshed in another ~20 min under the RTH TTL,
// not start a fresh countdown from boot time. Also lets the
// boundary-refresh path detect cached snapshots whose AsOf belongs
// to a different session class than the current one (e.g. a
// persisted pre-market value reloaded at 10:00 ET).
func newPersistedComputation(r *rpc.GammaZeroComputed, scope string, now time.Time) *gammaComputation {
	job := &gammaComputation{
		sessionKey: nySessionKey(now),
		scope:      scope,
		startedAt:  r.AsOf,
		done:       make(chan struct{}),
		result:     r,
	}
	close(job.done)
	return job
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
	for _, slot := range c.slots {
		if slot.current != nil && !slot.current.isDone() {
			return true
		}
		if slot.refresh != nil && !slot.refresh.isDone() {
			return true
		}
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
// Soft-TTL refresh: when the served result is past the current
// session class's softTTL or was computed in a different active
// class (e.g. pre-market value served during RTH), kickOrJoin kicks
// a background refresh while still returning the stale value. fresh
// stays false for the caller (they got the cached envelope, not a
// new in-flight job). The next caller after the refresh lands sees
// the new value. Closed sessions never trigger refresh — see the
// kickOrJoin body comment for the gating logic.
func (c *gammaZeroCache) kickOrJoin(parent context.Context, scope string, now time.Time, etaSeconds int, compute computeFn) (job *gammaComputation, fresh bool) {
	key := nySessionKey(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	slot := c.getOrCreateSlotLocked(scope)

	// Promote a landed soft-TTL refresh; discard a failed one. A
	// failed refresh must NOT poison a known-good cached value —
	// the existing current stays in place and the next caller can
	// trigger another refresh attempt past the soft TTL.
	if slot.refresh != nil && slot.refresh.isDone() {
		if slot.refresh.err == nil && slot.refresh.sessionKey == key {
			slot.current = slot.refresh
		}
		slot.refresh = nil
	}

	// SessionClosed gate: outside U.S. equity-options trading hours
	// we never kick a fresh compute (no fresh quotes inbound; the
	// fan-out would either time out on dead model ticks or land
	// garbage IVs against prior-session prices) and we serve any
	// successful cached result we have — even one whose sessionKey
	// belongs to a prior NY trading date. The persisted Friday-RTH
	// result is the best answer we can give Saturday morning;
	// freshness is the renderer's problem (snapshot stamps
	// cache_stale_off_hours past 24h so the user sees the age).
	//
	// No usable cache + closed gateway → return (nil, false) and
	// snapshot reports Cold rather than starting a doomed compute.
	if rpc.ClassifySession(now) == rpc.SessionClosed {
		if slot.current != nil && slot.current.isDone() && slot.current.err == nil && slot.current.result != nil {
			return slot.current, false
		}
		return nil, false
	}

	if slot.current != nil && slot.current.sessionKey == key {
		// Same session — but a cached error past the retry TTL must NOT
		// block fresh attempts. Without this check, a one-shot
		// gateway-side timeout poisons every regime/gamma call for the
		// rest of the NY trading session. In-flight jobs (isDone=false)
		// always pass through to the shared singleflight regardless of
		// age; success results stay sticky for the whole session.
		if slot.current.isDone() && slot.current.err != nil && now.Sub(slot.current.startedAt) >= gammaErrorRetryTTL {
			// Retain the failure context so the next render of the
			// "computing" row can surface "retry of <error> at HH:MM:SS"
			// instead of silently switching to a clean Computing state.
			slot.lastErr = slot.current.err
			slot.lastErrAt = slot.current.startedAt
			slot.lastErrSummary = summarizeGammaErr(slot.current.err)
			job = c.startLocked(parent, scope, key, now, etaSeconds, compute)
			return job, true
		}
		// Soft-TTL: if the served value is stale and no refresh is
		// already running, kick one behind it. The caller still gets
		// slot.current immediately — refresh is fire-and-forget.
		//
		// Two triggers, both gated on the cached value being a clean
		// success with no refresh already in flight:
		//
		//  1. Boundary refresh: cached value was computed in a
		//     different active session class than `now`. E.g. a
		//     pre-market snapshot served at 09:31 ET would otherwise
		//     survive the entire first hour of RTH under the 60-min
		//     RTH TTL — users expect fresh data at the open.
		//
		//  2. Age refresh: absolute age exceeds the per-class
		//     softTTL — 60 min in RTH, 30 min in pre/post.
		//
		// Both triggers are skipped when the current class is
		// SessionClosed: no price input, no point refreshing. softTTL
		// returns math.MaxInt64 for Closed so the age check can never
		// fire there, and the explicit currentClass check below
		// skips the boundary path so a Post→Closed transition (e.g.
		// 20:01 ET) doesn't trigger a doomed refresh.
		if slot.current.isDone() && slot.current.err == nil && slot.refresh == nil {
			currentClass := rpc.ClassifySession(now)
			if currentClass != rpc.SessionClosed {
				cachedClass := rpc.ClassifySession(slot.current.startedAt)
				classChanged := cachedClass != currentClass
				if classChanged || now.Sub(slot.current.startedAt) >= softTTL(now) {
					slot.refresh = c.spawnJob(parent, scope, key, now, etaSeconds, compute)
				}
			}
		}
		// Same session: serve the in-flight or recently-completed job.
		// Callers that need to bypass cache (diagnostics) use force().
		return slot.current, false
	}

	job = c.startLocked(parent, scope, key, now, etaSeconds, compute)
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
func (c *gammaZeroCache) force(parent context.Context, scope string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	key := nySessionKey(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	slot := c.getOrCreateSlotLocked(scope)
	if slot.current != nil && !slot.current.isDone() {
		// Cancel the superseded compute — it stops fanning out and the
		// next time the gateway responds to one of its in-flight legs
		// the worker returns immediately. The done channel doesn't
		// close (the goroutine returns without setting result), so
		// any caller still waiting on the old job will block until
		// its own ctx times out. That's the documented force tradeoff.
		slot.current.cancel()
	}
	if slot.refresh != nil && !slot.refresh.isDone() {
		slot.refresh.cancel()
	}
	slot.refresh = nil
	return c.startLocked(parent, scope, key, now, etaSeconds, compute)
}

// spawnJob allocates a fresh computation and launches its background
// goroutine. Caller must hold c.mu. Does NOT assign the job into any
// slot — the caller decides whether to install it as `current`
// (startLocked) or hold it separately as the soft-TTL refresh.
//
// scope is captured on the gammaComputation so the persist call can
// route the save to the right per-scope file.
func (c *gammaZeroCache) spawnJob(parent context.Context, scope, key string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	// Decouple the compute's lifetime from any single RPC ctx. Use the
	// daemon's parent context (typically Background) as the upstream
	// signal so daemon shutdown still cancels the compute, but a
	// client disconnect mid-compute doesn't kill a job that other
	// pollers are waiting on.
	bgCtx, cancel := context.WithCancel(parent)
	job := &gammaComputation{
		sessionKey: key,
		scope:      scope,
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
		// Persist to disk on success. Failed computes do not persist
		// (no Save call on the err != nil path) — mirrors breadth's
		// MinCoverageFraction policy of "do not persist runs that
		// did not converge."
		//
		// Persistence runs off the cache mutex; c.store is set once
		// at construction and never modified, so reading it lock-
		// less is safe. Save errors degrade to warnings only — the
		// in-memory cache still serves callers correctly.
		if c.store != nil {
			if saveErr := c.store.Save(scope, key, res); saveErr != nil {
				gammaLogf{inner: c.log}.Warnf("gamma cache: persist scope=%s: %v", scope, saveErr)
			}
		}
	}()

	return job
}

// startLocked allocates and launches a fresh compute, assigning the
// new job to slot.current for the scope. Caller must hold c.mu. Thin
// wrapper over spawnJob — kept distinct because most kick paths want
// the new job to become the canonical served value for the scope,
// while the soft-TTL refresh path holds the new job in slot.refresh
// until it lands cleanly.
func (c *gammaZeroCache) startLocked(parent context.Context, scope, key string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	job := c.spawnJob(parent, scope, key, now, etaSeconds, compute)
	slot := c.getOrCreateSlotLocked(scope)
	slot.current = job
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
		// Off-hours stale tag: when we're serving a cached result
		// outside trading hours and it's more than 24h old, append
		// the `cache_stale_off_hours` warning so the renderer can
		// say "computed Nh ago" loudly instead of presenting an
		// overnight-old reading as if it were fresh. Copy-on-write
		// to avoid mutating the shared cache pointer that other
		// concurrent snapshots may still be reading.
		now := nowFn()
		if g.result != nil && rpc.ClassifySession(now) == rpc.SessionClosed && now.Sub(g.result.AsOf) > 24*time.Hour {
			r := *g.result
			r.Warnings = dedupeStrings(append(append([]string{}, g.result.Warnings...), "cache_stale_off_hours"))
			env.Result = &r
		}
		// Clear stale prior-error context for this scope — a successful
		// compute means the previous failure is no longer informative.
		c.mu.Lock()
		if slot, ok := c.slots[g.scope]; ok {
			slot.lastErr = nil
			slot.lastErrSummary = ""
		}
		c.mu.Unlock()
		return env
	}
	progress := g.progress.Load()
	env.Status = rpc.GammaZeroStatusComputing
	env.EtaSeconds = remainingEta(g, nowFn(), progress)
	env.Progress = int(progress)
	// Attach prior-failure context for THIS scope if the current
	// in-flight compute is a retry of a recent failure. The renderer
	// uses this to display "computing · retry of <summary> at
	// HH:MM:SS" instead of dropping the prior error silently. Reads
	// from the slot keyed by g.scope so the spy/spx/combined retry
	// contexts stay separate.
	c.mu.Lock()
	if slot, ok := c.slots[g.scope]; ok && slot.lastErr != nil {
		retryAt := slot.lastErrAt
		env.RetryOfErrorAt = &retryAt
		env.RetryOfErrorSummary = slot.lastErrSummary
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
