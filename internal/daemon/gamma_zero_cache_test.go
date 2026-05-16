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
		return &rpc.GammaZeroComputed{SpotSPX: 5000}, nil
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
	if first.result == nil || first.result.SpotSPX != 5000 {
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
		return &rpc.GammaZeroComputed{SpotSPX: 5000}, nil
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
			return &rpc.GammaZeroComputed{SpotSPX: 5000}, nil
		}
	}
	fastCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return &rpc.GammaZeroComputed{SpotSPX: 5050}, nil
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
	if job2.result == nil || job2.result.SpotSPX != 5050 {
		t.Errorf("force job result mismatch: %+v", job2.result)
	}
}

// TestGammaZeroCache_SnapshotStates exhaustively covers the three
// envelope states (computing / ready / error) the snapshot helper
// must produce — the dashboard generator branches on Status, so a
// regression that loses one of these states silently breaks the UI.
func TestGammaZeroCache_SnapshotStates(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now.Add(10 * time.Second) }

	// nil job — happens only before the first kickoff. Status is the
	// computing sentinel so renderers can show "spinning up."
	env := c.snapshot(nil, nowFn)
	if env.Status != rpc.GammaZeroStatusComputing {
		t.Errorf("nil job snapshot: got %q, want %q", env.Status, rpc.GammaZeroStatusComputing)
	}

	// Computing
	block := make(chan struct{})
	computingCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		p.Store(42)
		<-block
		return &rpc.GammaZeroComputed{SpotSPX: 5000}, nil
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
	if env.Result == nil || env.Result.SpotSPX != 5000 {
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

// TestRemainingEta floors the ETA at 5 seconds and decrements it
// correctly as the job ages. Used by the polling UX to avoid
// flicker near the end of a long compute.
func TestRemainingEta(t *testing.T) {
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	job := &gammaComputation{startedAt: now, etaSeconds: 300}

	if got := remainingEta(job, now); got != 300 {
		t.Errorf("at start: got %d, want 300", got)
	}
	if got := remainingEta(job, now.Add(60*time.Second)); got != 240 {
		t.Errorf("at +60s: got %d, want 240", got)
	}
	if got := remainingEta(job, now.Add(299*time.Second)); got != 5 {
		t.Errorf("near end: got %d, want 5 (floor)", got)
	}
	if got := remainingEta(job, now.Add(10*time.Minute)); got != 5 {
		t.Errorf("past initial estimate: got %d, want 5 (floor)", got)
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
