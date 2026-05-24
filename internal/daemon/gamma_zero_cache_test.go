package daemon

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// TestGammaZeroCache_SingleflightWithinSession proves the cache's
// singleflight invariant: two concurrent kickOrJoin calls in the same
// NY session share one compute, even if both arrive while the first
// is still in flight. This is load-bearing — without it, a dashboard
// generator + a CLI poll at the same moment would launch duplicate
// fan-outs and double the gateway market-data slot pressure.
func TestGammaZeroCache_SingleflightWithinSession(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	block := make(chan struct{}) // hold the compute until both callers have joined

	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		<-block
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}

	var first, second *gammaComputation
	var firstFresh, secondFresh bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		first, firstFresh = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	}()
	// Brief gap to make sure the first caller has acquired the mutex
	// and registered c.current before the second hits.
	time.Sleep(20 * time.Millisecond)
	go func() {
		defer wg.Done()
		second, secondFresh = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	}()
	// Let the second caller observe in-flight state, then unblock.
	time.Sleep(20 * time.Millisecond)
	close(block)
	wg.Wait()

	if first != second {
		t.Fatalf("singleflight broken: got two distinct jobs %p vs %p", first, second)
	}
	if !firstFresh {
		t.Errorf("first caller should report fresh=true, got false")
	}
	if secondFresh {
		t.Errorf("second caller should report fresh=false (joined existing), got true")
	}
	if got := computeRuns.Load(); got != 1 {
		t.Errorf("compute ran %d times, want exactly 1", got)
	}

	<-first.done
	if first.result == nil || first.result.SpotUnderlying != 5000 {
		t.Errorf("first.result mismatch: %+v", first.result)
	}
}

// TestGammaZeroCache_SessionRollover verifies a new compute kicks off
// when the NY session date changes — yesterday's cached result must
// not satisfy today's caller.
func TestGammaZeroCache_SessionRollover(t *testing.T) {
	c := newGammaZeroCache()
	day1 := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}

	job1, fresh1 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, day1, 300, compute)
	<-job1.done
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, day2, 300, compute)
	<-job2.done // wait for the second compute to record its run

	if job1 == job2 {
		t.Fatalf("session rollover not detected — got same job pointer")
	}
	if !fresh1 || !fresh2 {
		t.Errorf("both day-1 and day-2 should be fresh kickoffs: got fresh1=%v fresh2=%v",
			fresh1, fresh2)
	}
	if got := computeRuns.Load(); got != 2 {
		t.Errorf("expected 2 compute runs across the rollover, got %d", got)
	}
}

// TestGammaZeroCache_ForceSupersedesInflight proves force() cancels
// the in-flight compute and starts a new one, distinct from the
// superseded job. The old job's done channel may never close (the
// goroutine returns without recording a result on cancel); callers
// that were waiting on it are responsible for their own timeouts.
func TestGammaZeroCache_ForceSupersedesInflight(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	cancelled := make(chan struct{})
	slowCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		select {
		case <-ctx.Done():
			close(cancelled)
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
		}
	}
	fastCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return &rpc.GammaZeroComputed{SpotUnderlying: 5050}, nil
	}

	job1, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, slowCompute)
	time.Sleep(30 * time.Millisecond) // let slowCompute enter its select

	job2 := c.force(context.Background(), rpc.GammaZeroScopeCombined, now, 300, fastCompute)

	if job1 == job2 {
		t.Fatal("force should produce a new job distinct from the superseded one")
	}

	select {
	case <-cancelled:
		// expected: the slow compute saw ctx.Done() and exited
	case <-time.After(500 * time.Millisecond):
		t.Fatal("superseded compute was not cancelled within budget")
	}

	<-job2.done
	if job2.result == nil || job2.result.SpotUnderlying != 5050 {
		t.Errorf("force job result mismatch: %+v", job2.result)
	}
}

// TestGammaZeroCache_RetriesErrorAfterTTL pins the no-poison
// invariant: a transient compute error must NOT stick in cache for
// the rest of the NY trading session. Before this fix the cache
// returned c.current unconditionally on session-key match; a
// gateway-side cold-start timeout at 9:30 AM ET poisoned every
// regime/gamma call until midnight NY rolled the session key.
//
// Within the TTL window: same-session callers see the cached error
// (no thundering herd). Past the TTL: a fresh compute is kicked.
// In-flight jobs and successful results stay sticky in both cases.
func TestGammaZeroCache_RetriesErrorAfterTTL(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	bonk := errors.New("gateway timeout")
	errCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return nil, bonk
	}

	job1, fresh1 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, errCompute)
	<-job1.done
	if !fresh1 || job1.err == nil {
		t.Fatalf("first kickoff: fresh=%v err=%v, want fresh=true err!=nil", fresh1, job1.err)
	}

	// Same instant: same-session caller must see the cached error,
	// not trigger a new compute. This is the dampener against retry
	// storms while a gateway is genuinely flapping.
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, errCompute)
	if fresh2 || job2 != job1 {
		t.Errorf("within TTL: got fresh=%v job=%p (want fresh=false job=%p)", fresh2, job2, job1)
	}
	if got := computeRuns.Load(); got != 1 {
		t.Errorf("within TTL: compute ran %d times, want 1", got)
	}

	// Past the TTL: same session key, but the cached error is now
	// stale-on-error. kickOrJoin must launch a fresh attempt.
	past := now.Add(gammaErrorRetryTTL + time.Second)
	successCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5050}, nil
	}
	job3, fresh3 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, past, 300, successCompute)
	if !fresh3 || job3 == job1 {
		t.Errorf("past TTL: got fresh=%v job=%p (want fresh=true new job)", fresh3, job3)
	}
	<-job3.done
	if job3.err != nil || job3.result == nil {
		t.Errorf("retry compute should have succeeded: err=%v result=%v", job3.err, job3.result)
	}
	if got := computeRuns.Load(); got != 2 {
		t.Errorf("past TTL: compute ran %d times, want 2", got)
	}

	// Past the TTL again, but now the cache holds a SUCCESS — the
	// retry path must not fire on healthy state.
	future := past.Add(2 * gammaErrorRetryTTL)
	job4, fresh4 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, future, 300, errCompute)
	if fresh4 || job4 != job3 {
		t.Errorf("success cache must stay sticky regardless of age: fresh=%v job=%p want job=%p", fresh4, job4, job3)
	}
}

// TestGammaZeroCache_SoftTTLRefreshesBehindStale proves the
// refresh-while-stale invariant: when the cached successful compute
// ages past softTTLRTH, the next kickOrJoin returns the stale value
// IMMEDIATELY and kicks a background refresh. The promotion of that
// refresh to current happens on the NEXT kickOrJoin — so a single
// caller doesn't pay for the refresh, the system pays it over the
// next two calls.
//
// Load-bearing: this is what keeps `ibkr regime` fast after a
// long-idle daemon. Without it, the same-session cache served a
// 10-min-old answer with no path to freshness until midnight NY.
func TestGammaZeroCache_SoftTTLRefreshesBehindStale(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		// Distinct payload per run so we can tell first vs refresh apart.
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000 + float64(run)}, nil
	}

	// First call: kicks the initial compute.
	job1, fresh1 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	<-job1.done
	if !fresh1 || job1.result == nil || job1.result.SpotUnderlying != 5001 {
		t.Fatalf("initial kickoff: fresh=%v result=%+v", fresh1, job1.result)
	}

	// Within softTTL: same job returned, no refresh kicked.
	withinTTL := now.Add(softTTLRTH - time.Second)
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, withinTTL, 300, compute)
	if fresh2 || job2 != job1 {
		t.Errorf("within softTTL: got fresh=%v job=%p (want fresh=false job=%p)", fresh2, job2, job1)
	}
	if got := computeRuns.Load(); got != 1 {
		t.Errorf("within softTTL: compute ran %d times, want 1 (no refresh expected)", got)
	}

	// Past softTTL: STILL get the stale value, but a refresh kicks
	// behind it. The caller doesn't block — they see the cached
	// SpotUnderlying=5001 immediately.
	pastTTL := now.Add(softTTLRTH + time.Second)
	job3, fresh3 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)
	if fresh3 || job3 != job1 {
		t.Errorf("past softTTL: caller should get the stale value: got fresh=%v job=%p (want fresh=false job=%p)", fresh3, job3, job1)
	}
	if job3.result == nil || job3.result.SpotUnderlying != 5001 {
		t.Errorf("past softTTL: caller should see stale result 5001, got %+v", job3.result)
	}

	// Wait for the refresh to land. We can't reach into c.refresh
	// directly without exposing internals; instead, poll kickOrJoin
	// until we see the promoted result. The refresh compute is
	// synchronous in this test, so this resolves on the next call.
	deadline := time.Now().Add(500 * time.Millisecond)
	var job4 *gammaComputation
	for time.Now().Before(deadline) {
		job4, _ = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)
		if job4 != job1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job4 == job1 {
		t.Fatal("refresh never promoted — kickOrJoin kept returning the stale job")
	}
	if job4.result == nil || job4.result.SpotUnderlying != 5002 {
		t.Errorf("refresh promotion: got %+v, want SpotUnderlying=5002 (the refreshed compute)", job4.result)
	}
	if got := computeRuns.Load(); got != 2 {
		t.Errorf("after refresh: compute ran %d times, want exactly 2 (initial + one refresh)", got)
	}
}

// TestGammaZeroCache_SoftTTLDoesNotStackRefreshes proves the
// refresh-while-stale path is itself singleflighted: a second caller
// arriving while a refresh is in flight does NOT kick a second
// refresh. Otherwise a burst of post-TTL pollers (e.g. an MCP agent
// hammering ibkr_regime) would launch parallel compute fan-outs,
// defeating the gateway-slot-protection that motivates the cache.
func TestGammaZeroCache_SoftTTLDoesNotStackRefreshes(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	block := make(chan struct{})
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		if run >= 2 {
			// Hold the refresh until the test signals — gives the
			// second caller a window to observe in-flight refresh.
			<-block
		}
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000 + float64(run)}, nil
	}

	// Initial kickoff completes immediately.
	job1, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	<-job1.done

	// Two callers past softTTL, back-to-back. The first kicks the
	// refresh; the second sees an in-flight refresh and does NOT
	// kick another.
	pastTTL := now.Add(softTTLRTH + time.Second)
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)

	// Give the goroutines time to settle; the second compute
	// should still be blocked.
	time.Sleep(20 * time.Millisecond)
	if got := computeRuns.Load(); got != 2 {
		t.Errorf("expected exactly 2 computes (initial + one refresh), got %d", got)
	}

	close(block) // let the refresh finish; drains the goroutine
}

// TestGammaZeroCache_SoftTTLFailedRefreshKeepsCachedSuccess proves a
// failed soft-TTL refresh must NOT poison a known-good cached value.
// Without this, a transient gateway hiccup during a refresh could
// overwrite the entire morning's stable γ-zero reading until midnight
// rolled the session key.
func TestGammaZeroCache_SoftTTLFailedRefreshKeepsCachedSuccess(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	bonk := errors.New("transient gateway timeout")
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		if run == 1 {
			return &rpc.GammaZeroComputed{SpotUnderlying: 5001}, nil
		}
		// Subsequent runs (the refresh) fail.
		return nil, bonk
	}

	// Establish the cached success.
	job1, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	<-job1.done
	if job1.result == nil || job1.result.SpotUnderlying != 5001 {
		t.Fatalf("initial kickoff failed: %+v", job1)
	}

	// Past softTTL: refresh kicks, fails.
	pastTTL := now.Add(softTTLRTH + time.Second)
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)

	// Wait for the refresh to finish. Then a follow-up call must
	// STILL return the cached success — the refresh's error gets
	// dropped, not promoted.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if computeRuns.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the goroutine's defer close(done) land

	job3, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)
	if job3.result == nil || job3.result.SpotUnderlying != 5001 {
		t.Errorf("after failed refresh: should still see cached success 5001, got %+v", job3.result)
	}
	if job3.err != nil {
		t.Errorf("after failed refresh: cached success should not have err, got %v", job3.err)
	}
}

// TestGammaZeroCache_SnapshotStates exhaustively covers the four
// envelope states (cold / computing / ready / error) the snapshot helper
// must produce — the dashboard generator branches on Status, so a
// regression that loses one of these states silently breaks the UI.
func TestGammaZeroCache_SnapshotStates(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now.Add(10 * time.Second) }

	// nil job — happens only before the first kickoff this NY trading
	// session. Status is Cold, distinct from Computing so a consumer
	// can tell "first caller must kick" from "kick already in flight."
	// Pre-v0.27.9 these were both Computing; the v0.27.3 breadth
	// State enum retired the same side-channel-inference pattern.
	env := c.snapshot(nil, nowFn)
	if env.Status != rpc.GammaZeroStatusCold {
		t.Errorf("nil job snapshot: got %q, want %q (no compute kicked this session must be Cold, not Computing)", env.Status, rpc.GammaZeroStatusCold)
	}

	// Computing
	block := make(chan struct{})
	computingCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		p.Store(42)
		<-block
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}
	job, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, computingCompute)
	time.Sleep(20 * time.Millisecond)
	env = c.snapshot(job, nowFn)
	if env.Status != rpc.GammaZeroStatusComputing {
		t.Errorf("computing snapshot: got %q, want %q", env.Status, rpc.GammaZeroStatusComputing)
	}
	if env.Progress != 42 {
		t.Errorf("progress not propagated: got %d, want 42", env.Progress)
	}
	if env.EtaSeconds <= 0 {
		t.Errorf("eta_seconds should be positive while computing, got %d", env.EtaSeconds)
	}

	// Ready
	close(block)
	<-job.done
	env = c.snapshot(job, nowFn)
	if env.Status != rpc.GammaZeroStatusReady {
		t.Errorf("ready snapshot: got %q, want %q", env.Status, rpc.GammaZeroStatusReady)
	}
	if env.Result == nil || env.Result.SpotUnderlying != 5000 {
		t.Errorf("ready result missing: %+v", env.Result)
	}

	// Error — separate cache to avoid same-session singleflight reuse.
	c2 := newGammaZeroCache()
	bonk := errors.New("compute failed: gateway disconnected")
	errCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return nil, bonk
	}
	errJob, _ := c2.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, errCompute)
	<-errJob.done
	env = c2.snapshot(errJob, nowFn)
	if env.Status != rpc.GammaZeroStatusError {
		t.Errorf("error snapshot: got %q, want %q", env.Status, rpc.GammaZeroStatusError)
	}
	if env.Error != bonk.Error() {
		t.Errorf("error message not propagated: got %q, want %q", env.Error, bonk.Error())
	}
}

// TestFindZeroCrossing covers the four documented outcomes of the
// zero-crossing search: clean crossing, all-positive sweep,
// all-negative sweep, and the no-data degenerate. Linear interpolation
// is asserted against an analytic anchor to catch off-by-one errors.
func TestFindZeroCrossing(t *testing.T) {
	t.Run("clean_crossing", func(t *testing.T) {
		// Construct a profile that's strictly decreasing through zero
		// between spot 4980 (+100) and spot 5000 (-100). Linear interp
		// should land exactly at 4990.
		profile := []rpc.GammaProfilePoint{
			{Spot: 4900, GEX: 1000},
			{Spot: 4980, GEX: 100},
			{Spot: 5000, GEX: -100},
			{Spot: 5100, GEX: -1000},
		}
		zg, sign := findZeroCrossing(profile)
		if zg == nil {
			t.Fatalf("expected non-nil zero gamma, got sign=%q", sign)
		}
		if math.Abs(*zg-4990) > 0.01 {
			t.Errorf("zero crossing at %.4f, want 4990 (±0.01)", *zg)
		}
		if sign != "" {
			t.Errorf("clean crossing should report empty sign, got %q", sign)
		}
	})

	t.Run("all_positive", func(t *testing.T) {
		profile := []rpc.GammaProfilePoint{
			{Spot: 4900, GEX: 100},
			{Spot: 5000, GEX: 200},
			{Spot: 5100, GEX: 300},
		}
		zg, sign := findZeroCrossing(profile)
		if zg != nil {
			t.Errorf("all-positive should return nil zero gamma, got %v", *zg)
		}
		if sign != "positive" {
			t.Errorf("all-positive sign: got %q, want positive", sign)
		}
	})

	t.Run("all_negative", func(t *testing.T) {
		profile := []rpc.GammaProfilePoint{
			{Spot: 4900, GEX: -100},
			{Spot: 5000, GEX: -200},
			{Spot: 5100, GEX: -300},
		}
		zg, sign := findZeroCrossing(profile)
		if zg != nil {
			t.Errorf("all-negative should return nil zero gamma, got %v", *zg)
		}
		if sign != "negative" {
			t.Errorf("all-negative sign: got %q, want negative", sign)
		}
	})

	t.Run("no_data", func(t *testing.T) {
		zg, sign := findZeroCrossing(nil)
		if zg != nil || sign != "no_data" {
			t.Errorf("empty profile: got (%v, %q), want (nil, no_data)", zg, sign)
		}
		zg, sign = findZeroCrossing([]rpc.GammaProfilePoint{{Spot: 5000, GEX: 100}})
		if zg != nil || sign != "no_data" {
			t.Errorf("single-point profile: got (%v, %q), want (nil, no_data)", zg, sign)
		}
	})

	t.Run("exact_zero_at_sample", func(t *testing.T) {
		profile := []rpc.GammaProfilePoint{
			{Spot: 4900, GEX: 100},
			{Spot: 5000, GEX: 0},
			{Spot: 5100, GEX: -100},
		}
		zg, sign := findZeroCrossing(profile)
		if zg == nil {
			t.Fatalf("expected non-nil zero gamma at exact sample, got sign=%q", sign)
		}
		// Exact 0 at index 1: interpolation between (4900,+100) and
		// (5000,0) lands at 5000 exactly.
		if math.Abs(*zg-5000) > 0.01 {
			t.Errorf("exact-zero sample: got %.4f, want 5000", *zg)
		}
	})
}

// TestRemainingEta exercises the progress-derived ETA. Below
// progress=5 the fallback is initial-minus-elapsed; above that
// threshold the formula projects from elapsed × (100-p)/p. Floor at
// 5s, ceiling at 4× the static initial estimate.
func TestRemainingEta(t *testing.T) {
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	job := &gammaComputation{startedAt: now, etaSeconds: 300}

	// progress=0 early — fallback to initial - elapsed
	if got := remainingEta(job, now, 0); got != 300 {
		t.Errorf("at start (progress=0): got %d, want 300", got)
	}
	if got := remainingEta(job, now.Add(60*time.Second), 0); got != 240 {
		t.Errorf("at +60s (progress=0): got %d, want 240", got)
	}

	// progress=50 with elapsed=60s — project 60s remaining
	if got := remainingEta(job, now.Add(60*time.Second), 50); got != 60 {
		t.Errorf("progress=50 +60s: got %d, want 60", got)
	}

	// progress=99 — formula yields ~elapsed × 1/99; floor at 5
	if got := remainingEta(job, now.Add(300*time.Second), 99); got != 5 {
		t.Errorf("progress=99 +300s: got %d, want 5 (floor)", got)
	}

	// Runaway compute: progress=10 after 10 minutes → formula says
	// 600 × 90 / 10 = 5400s, which must be capped at 4 × 300 = 1200s.
	if got := remainingEta(job, now.Add(10*time.Minute), 10); got != 1200 {
		t.Errorf("runaway (progress=10 +10m): got %d, want 1200 (cap)", got)
	}

	// Fallback path past the initial estimate also floors at 5
	if got := remainingEta(job, now.Add(10*time.Minute), 0); got != 5 {
		t.Errorf("past initial estimate (progress=0): got %d, want 5 (floor)", got)
	}
}

// TestNYSessionKey is a sanity check that DST-relevant dates produce
// stable keys. New York observes DST; verify the key changes correctly
// across DST boundaries and stays stable within a single trading day.
func TestNYSessionKey(t *testing.T) {
	// 2026-03-08 02:30 EST → 03:30 EDT (spring forward in NY)
	// Both should produce key "2026-03-08" because they're the same
	// calendar date in NY.
	before := time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC) // 01:30 EST
	after := time.Date(2026, 3, 8, 8, 30, 0, 0, time.UTC)  // 04:30 EDT
	if k1, k2 := nySessionKey(before), nySessionKey(after); k1 != k2 {
		t.Errorf("DST spring-forward changed key within a day: %s vs %s", k1, k2)
	}

	// Same UTC moment in late NY day vs early NY next-day: keys differ.
	utcLateNY := time.Date(2026, 5, 17, 3, 0, 0, 0, time.UTC) // 23:00 EDT on May 16
	utcNextNY := time.Date(2026, 5, 17, 5, 0, 0, 0, time.UTC) // 01:00 EDT on May 17
	k1, k2 := nySessionKey(utcLateNY), nySessionKey(utcNextNY)
	if k1 == k2 {
		t.Errorf("NY date rollover not detected: both keys = %s", k1)
	}
	if k1 != "2026-05-16" {
		t.Errorf("late-NY May 16 should map to 2026-05-16, got %s", k1)
	}
	if k2 != "2026-05-17" {
		t.Errorf("early-NY May 17 should map to 2026-05-17, got %s", k2)
	}
}

// TestGammaZeroCache_ClosedSessionNeverRefreshes proves the closed-
// session ∞ TTL rule: outside U.S. equity-options trading hours
// (overnight + weekends) a cached successful compute is served
// indefinitely with no background refresh, regardless of how long
// it's been since the value was computed.
//
// Load-bearing: under the pre-session-aware constant TTL (5 min), a
// daemon idling overnight Saturday would re-kick a multi-minute
// option fan-out every 5 min — burning gateway slots on a market
// that won't produce fresh quotes anyway. The session-aware
// softTTLClosed = math.MaxInt64 cuts that to zero.
func TestGammaZeroCache_ClosedSessionNeverRefreshes(t *testing.T) {
	c := newGammaZeroCache()
	// Saturday 2026-05-23 10:00 EDT — weekend, SessionClosed.
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)
	if cls := rpc.ClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed for Saturday, got %v", cls)
	}

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000 + float64(run)}, nil
	}

	// Pre-seed a Saturday compute as if Friday's daemon persisted it
	// to disk and today's boot loaded it. SessionClosed callers must
	// see this cached value; they must NOT kick a fresh compute (no
	// fresh quotes inbound; the fan-out would land garbage IVs).
	persisted := &rpc.GammaZeroComputed{SpotUnderlying: 5001, AsOf: now}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(persisted, rpc.GammaZeroScopeCombined, now)},
	}
	job1 := c.slots[rpc.GammaZeroScopeCombined].current

	// 8 hours later — well past every active-session softTTL but
	// still SessionClosed and still the same NY trading date.
	later := now.Add(8 * time.Hour)
	if cls := rpc.ClassifySession(later); cls != rpc.SessionClosed {
		t.Fatalf("later instant should still be SessionClosed (Sat 18:00 EDT), got %v", cls)
	}
	if nySessionKey(later) != nySessionKey(now) {
		t.Fatalf("later instant rolled the session key; pick a smaller offset (got %s vs %s)",
			nySessionKey(later), nySessionKey(now))
	}
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, later, 300, compute)
	if fresh2 || job2 != job1 {
		t.Errorf("closed session: should serve cached job1 with no refresh, got fresh=%v job=%p (want job=%p)", fresh2, job2, job1)
	}
	if got := computeRuns.Load(); got != 0 {
		t.Errorf("closed session: compute ran %d times, want 0 (no kick, no refresh)", got)
	}
}

// TestGammaZeroCache_BoundaryRefreshOnClassTransition proves the
// session-class transition trigger: a cached value computed in one
// active session class is treated as stale on the first kickOrJoin
// in a different active class, even when its absolute age is below
// the per-class softTTL. Without this, a pre-market snapshot served
// at 09:31 ET would survive the entire first hour of RTH under the
// 60-min RTH TTL — but pre-market γ-zero is qualitatively
// different from an RTH one (thin volume, muted dealer flow), and
// users querying right after the open expect a fresh read.
func TestGammaZeroCache_BoundaryRefreshOnClassTransition(t *testing.T) {
	c := newGammaZeroCache()
	// 09:25 ET Tuesday — five minutes before the open, SessionPre.
	preMarket := time.Date(2026, 5, 19, 13, 25, 0, 0, time.UTC)
	if cls := rpc.ClassifySession(preMarket); cls != rpc.SessionPre {
		t.Fatalf("test fixture sanity check: expected SessionPre at 09:25 ET, got %v", cls)
	}
	// 09:35 ET same Tuesday — five minutes into RTH, SessionRTH.
	rth := time.Date(2026, 5, 19, 13, 35, 0, 0, time.UTC)
	if cls := rpc.ClassifySession(rth); cls != rpc.SessionRTH {
		t.Fatalf("test fixture sanity check: expected SessionRTH at 09:35 ET, got %v", cls)
	}

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000 + float64(run)}, nil
	}

	job1, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, preMarket, 300, compute)
	<-job1.done
	if job1.result == nil || job1.result.SpotUnderlying != 5001 {
		t.Fatalf("initial pre-market compute: %+v", job1.result)
	}

	// Cross the open. Age is 10 min — well below both softTTLPrePost
	// (30 min) and softTTLRTH (60 min). Without the boundary path,
	// the cached value would survive. With it, a refresh kicks
	// behind the served stale value.
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, rth, 300, compute)
	if fresh2 || job2 != job1 {
		t.Errorf("class transition: caller should still see the stale pre-market value, got fresh=%v job=%p (want job=%p)", fresh2, job2, job1)
	}

	// Poll until the refresh promotes — same idiom as the soft-TTL
	// refresh test. The refresh compute is synchronous here, so it
	// resolves on the next call.
	deadline := time.Now().Add(500 * time.Millisecond)
	var job3 *gammaComputation
	for time.Now().Before(deadline) {
		job3, _ = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, rth, 300, compute)
		if job3 != job1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job3 == job1 {
		t.Fatal("boundary refresh never promoted — kickOrJoin kept returning the pre-market job")
	}
	if job3.result == nil || job3.result.SpotUnderlying != 5002 {
		t.Errorf("boundary refresh: got %+v, want SpotUnderlying=5002 (the refreshed RTH compute)", job3.result)
	}
	if got := computeRuns.Load(); got != 2 {
		t.Errorf("after boundary refresh: compute ran %d times, want 2 (initial pre + boundary RTH)", got)
	}
}

// TestClassifySession exercises the four-way classifier on
// representative instants spanning the daily and weekly boundaries.
// Kept here rather than rpc/rpc_test.go because the cache is the
// only caller today and its tests are where any regression would
// show up first.
func TestClassifySession(t *testing.T) {
	// Tuesday 2026-05-19.
	tests := []struct {
		name string
		t    time.Time
		want rpc.SessionClass
	}{
		{"weekday 03:59 ET → closed (before pre open)", time.Date(2026, 5, 19, 7, 59, 0, 0, time.UTC), rpc.SessionClosed},
		{"weekday 04:00 ET → pre", time.Date(2026, 5, 19, 8, 0, 0, 0, time.UTC), rpc.SessionPre},
		{"weekday 09:29 ET → pre", time.Date(2026, 5, 19, 13, 29, 0, 0, time.UTC), rpc.SessionPre},
		{"weekday 09:30 ET → rth", time.Date(2026, 5, 19, 13, 30, 0, 0, time.UTC), rpc.SessionRTH},
		{"weekday 15:59 ET → rth", time.Date(2026, 5, 19, 19, 59, 0, 0, time.UTC), rpc.SessionRTH},
		{"weekday 16:00 ET → post", time.Date(2026, 5, 19, 20, 0, 0, 0, time.UTC), rpc.SessionPost},
		{"weekday 19:59 ET → post", time.Date(2026, 5, 19, 23, 59, 0, 0, time.UTC), rpc.SessionPost},
		{"weekday 20:00 ET → closed", time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), rpc.SessionClosed},
		{"saturday 10:00 ET → closed", time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC), rpc.SessionClosed},
		{"sunday 10:00 ET → closed", time.Date(2026, 5, 24, 14, 0, 0, 0, time.UTC), rpc.SessionClosed},
	}
	for _, tc := range tests {
		if got := rpc.ClassifySession(tc.t); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGammaZeroCache_OffHoursServesFreshCacheWithoutWarning pins the
// SessionClosed serve-cached-no-kick contract for a fresh persisted
// value: a daemon that's been running through a session boundary (or
// rebooted with today's cache on disk) must serve the cached result
// to off-hours callers and must NOT append cache_stale_off_hours when
// the result is under 24h old.
func TestGammaZeroCache_OffHoursServesFreshCacheWithoutWarning(t *testing.T) {
	c := newGammaZeroCache()
	// Saturday 2026-05-23 10:00 EDT — SessionClosed, weekend.
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)
	if cls := rpc.ClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed, got %v", cls)
	}

	// Cache holds a result computed 6h ago — fresh for off-hours
	// (well under the 24h stale gate).
	cached := &rpc.GammaZeroComputed{SpotUnderlying: 5001, AsOf: now.Add(-6 * time.Hour)}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(cached, rpc.GammaZeroScopeCombined, now)},
	}
	cachedJob := c.slots[rpc.GammaZeroScopeCombined].current

	var kicked atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		kicked.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 9999}, nil
	}
	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	if fresh || job != cachedJob {
		t.Errorf("off-hours w/ fresh cache: got fresh=%v job=%p, want fresh=false job=%p", fresh, job, cachedJob)
	}
	if kicked.Load() != 0 {
		t.Errorf("off-hours must not kick a fresh compute, kicked=%d", kicked.Load())
	}

	env := c.snapshot(job, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("snapshot status = %q, want ready", env.Status)
	}
	for _, w := range env.Result.Warnings {
		if w == "cache_stale_off_hours" {
			t.Errorf("fresh off-hours cache should not carry cache_stale_off_hours; warnings=%v", env.Result.Warnings)
		}
	}
}

func TestGammaZeroCache_InvalidReadyResultRejected(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 22, 15, 0, 0, 0, time.UTC)
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return &rpc.GammaZeroComputed{
			SpotUnderlying: 743.73,
			LegCount:       1060,
			Profile: []rpc.GammaProfilePoint{
				{Spot: 740, GEX: 0},
				{Spot: 745, GEX: 0},
			},
			AsOf: now,
		}, nil
	}

	job := c.force(context.Background(), rpc.GammaZeroScopeSPY, now, 300, compute)
	<-job.done

	env := c.snapshot(job, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusError {
		t.Fatalf("snapshot status = %q, want error", env.Status)
	}
	if !strings.Contains(env.Error, "zero gamma_total_abs/profile/top_strikes") {
		t.Fatalf("snapshot error = %q, want invalid-result explanation", env.Error)
	}
}

func TestValidateGammaComputedAllowsLegsWithMagnitude(t *testing.T) {
	err := validateGammaComputed(&rpc.GammaZeroComputed{
		LegCount:       2,
		GammaTotalAbs:  1,
		TopStrikes:     []rpc.StrikeConcentration{{Strike: 740, AbsGEX: 1}},
		Profile:        []rpc.GammaProfilePoint{{Spot: 740, GEX: 1}},
		SpotUnderlying: 743.73,
		AsOf:           time.Now(),
	})
	if err != nil {
		t.Fatalf("validateGammaComputed returned %v, want nil", err)
	}
}

// TestGammaZeroCache_OffHoursStaleCacheGetsWarning proves the
// >24h-old branch of the SessionClosed serve path stamps the
// cache_stale_off_hours warning on the served envelope (via copy-on-
// write — the underlying cached pointer's Warnings must not be
// mutated, so concurrent snapshots don't see drift).
func TestGammaZeroCache_OffHoursStaleCacheGetsWarning(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 24, 14, 0, 0, 0, time.UTC) // Sunday 10:00 EDT, SessionClosed
	if cls := rpc.ClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed, got %v", cls)
	}

	// Cache result is 36h old — past the 24h stale gate.
	cached := &rpc.GammaZeroComputed{
		SpotUnderlying: 5001,
		AsOf:           now.Add(-36 * time.Hour),
		Warnings:       []string{"all_iv_derived"},
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(cached, rpc.GammaZeroScopeCombined, now)},
	}
	cachedJob := c.slots[rpc.GammaZeroScopeCombined].current

	env := c.snapshot(cachedJob, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("snapshot status = %q, want ready", env.Status)
	}
	var seen bool
	for _, w := range env.Result.Warnings {
		if w == "cache_stale_off_hours" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("stale off-hours cache must carry cache_stale_off_hours warning; got %v", env.Result.Warnings)
	}
	// Existing warning must survive the dedup'd union.
	var hasOld bool
	for _, w := range env.Result.Warnings {
		if w == "all_iv_derived" {
			hasOld = true
		}
	}
	if !hasOld {
		t.Errorf("existing warning lost during stale-tagging: got %v", env.Result.Warnings)
	}
	// Cache pointer must NOT have been mutated — copy-on-write
	// guarantees concurrent snapshots see the original.
	for _, w := range cached.Warnings {
		if w == "cache_stale_off_hours" {
			t.Errorf("snapshot mutated the cached Warnings slice: %v", cached.Warnings)
		}
	}
}

// TestGammaZeroCache_OffHoursForceAllowsPollToJoin proves that once
// force() kicks an in-flight compute on a closed session, a follow-up
// non-force kickOrJoin returns the same job rather than reporting Cold.
// Without this, a user running `ibkr gamma --force` off-hours and then
// polling with `ibkr gamma` would see a fresh Cold message between
// progress updates — confusing and wrong, since the compute IS running.
func TestGammaZeroCache_OffHoursForceAllowsPollToJoin(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC) // Saturday, SessionClosed

	block := make(chan struct{})
	defer close(block)
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		<-block
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}

	forced := c.force(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	if forced == nil {
		t.Fatal("force() returned nil on closed session")
	}

	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	if job == nil {
		t.Fatal("off-hours poll after force: expected to join in-flight job, got nil")
	}
	if job != forced {
		t.Errorf("off-hours poll after force: returned a different job (%p) than the in-flight one (%p)", job, forced)
	}
	if fresh {
		t.Errorf("off-hours poll after force: fresh must be false (join, not kick), got true")
	}
}

// TestGammaZeroCache_OffHoursColdReturnsEmpty proves the
// SessionClosed-no-cache branch: with no usable persisted result on
// hand, kickOrJoin must return (nil, false) and snapshot must report
// Cold rather than starting a doomed compute against a closed
// gateway. The dashboard renderer then surfaces "no data yet; try
// after the open."
func TestGammaZeroCache_OffHoursColdReturnsEmpty(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC) // Saturday, SessionClosed
	if cls := rpc.ClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed, got %v", cls)
	}

	var kicked atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		kicked.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}
	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	if job != nil {
		t.Errorf("off-hours cold cache: expected nil job, got %p", job)
	}
	if fresh {
		t.Errorf("off-hours cold cache: fresh must be false, got true")
	}
	if kicked.Load() != 0 {
		t.Errorf("off-hours cold cache: must not kick compute, kicked=%d", kicked.Load())
	}
	env := c.snapshot(job, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusCold {
		t.Errorf("snapshot status = %q, want cold", env.Status)
	}
}
