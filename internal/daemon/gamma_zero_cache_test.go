package daemon

import (
	"context"
	"errors"
	"math"
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
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)

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
		first, firstFresh = c.kickOrJoin(context.Background(), now, 300, compute)
	}()
	// Brief gap to make sure the first caller has acquired the mutex
	// and registered c.current before the second hits.
	time.Sleep(20 * time.Millisecond)
	go func() {
		defer wg.Done()
		second, secondFresh = c.kickOrJoin(context.Background(), now, 300, compute)
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
	day1 := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}

	job1, fresh1 := c.kickOrJoin(context.Background(), day1, 300, compute)
	<-job1.done
	job2, fresh2 := c.kickOrJoin(context.Background(), day2, 300, compute)
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
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)

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

	job1, _ := c.kickOrJoin(context.Background(), now, 300, slowCompute)
	time.Sleep(30 * time.Millisecond) // let slowCompute enter its select

	job2 := c.force(context.Background(), now, 300, fastCompute)

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
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	bonk := errors.New("gateway timeout")
	errCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return nil, bonk
	}

	job1, fresh1 := c.kickOrJoin(context.Background(), now, 300, errCompute)
	<-job1.done
	if !fresh1 || job1.err == nil {
		t.Fatalf("first kickoff: fresh=%v err=%v, want fresh=true err!=nil", fresh1, job1.err)
	}

	// Same instant: same-session caller must see the cached error,
	// not trigger a new compute. This is the dampener against retry
	// storms while a gateway is genuinely flapping.
	job2, fresh2 := c.kickOrJoin(context.Background(), now, 300, errCompute)
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
	job3, fresh3 := c.kickOrJoin(context.Background(), past, 300, successCompute)
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
	job4, fresh4 := c.kickOrJoin(context.Background(), future, 300, errCompute)
	if fresh4 || job4 != job3 {
		t.Errorf("success cache must stay sticky regardless of age: fresh=%v job=%p want job=%p", fresh4, job4, job3)
	}
}

// TestGammaZeroCache_SoftTTLRefreshesBehindStale proves the
// refresh-while-stale invariant: when the cached successful compute
// ages past gammaSoftTTL, the next kickOrJoin returns the stale value
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
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		// Distinct payload per run so we can tell first vs refresh apart.
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000 + float64(run)}, nil
	}

	// First call: kicks the initial compute.
	job1, fresh1 := c.kickOrJoin(context.Background(), now, 300, compute)
	<-job1.done
	if !fresh1 || job1.result == nil || job1.result.SpotUnderlying != 5001 {
		t.Fatalf("initial kickoff: fresh=%v result=%+v", fresh1, job1.result)
	}

	// Within softTTL: same job returned, no refresh kicked.
	withinTTL := now.Add(gammaSoftTTL - time.Second)
	job2, fresh2 := c.kickOrJoin(context.Background(), withinTTL, 300, compute)
	if fresh2 || job2 != job1 {
		t.Errorf("within softTTL: got fresh=%v job=%p (want fresh=false job=%p)", fresh2, job2, job1)
	}
	if got := computeRuns.Load(); got != 1 {
		t.Errorf("within softTTL: compute ran %d times, want 1 (no refresh expected)", got)
	}

	// Past softTTL: STILL get the stale value, but a refresh kicks
	// behind it. The caller doesn't block — they see the cached
	// SpotUnderlying=5001 immediately.
	pastTTL := now.Add(gammaSoftTTL + time.Second)
	job3, fresh3 := c.kickOrJoin(context.Background(), pastTTL, 300, compute)
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
		job4, _ = c.kickOrJoin(context.Background(), pastTTL, 300, compute)
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
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)

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
	job1, _ := c.kickOrJoin(context.Background(), now, 300, compute)
	<-job1.done

	// Two callers past softTTL, back-to-back. The first kicks the
	// refresh; the second sees an in-flight refresh and does NOT
	// kick another.
	pastTTL := now.Add(gammaSoftTTL + time.Second)
	c.kickOrJoin(context.Background(), pastTTL, 300, compute)
	c.kickOrJoin(context.Background(), pastTTL, 300, compute)

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
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)

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
	job1, _ := c.kickOrJoin(context.Background(), now, 300, compute)
	<-job1.done
	if job1.result == nil || job1.result.SpotUnderlying != 5001 {
		t.Fatalf("initial kickoff failed: %+v", job1)
	}

	// Past softTTL: refresh kicks, fails.
	pastTTL := now.Add(gammaSoftTTL + time.Second)
	c.kickOrJoin(context.Background(), pastTTL, 300, compute)

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

	job3, _ := c.kickOrJoin(context.Background(), pastTTL, 300, compute)
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
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
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
	job, _ := c.kickOrJoin(context.Background(), now, 300, computingCompute)
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
	errJob, _ := c2.kickOrJoin(context.Background(), now, 300, errCompute)
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
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
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
