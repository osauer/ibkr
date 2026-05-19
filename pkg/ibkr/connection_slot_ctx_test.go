package ibkr

import (
	"bufio"
	"context"
	"errors"
	"testing"
	"time"
)

// TestRequestMarketDataWithContract_HonoursCallerCtxOnSlotSaturation pins
// F-26: when the market-data slot pool is saturated, RequestMarketDataWithContract
// must honour the caller's ctx deadline rather than Connection.ctx.
//
// Lineage:
//
//   - v0.27.5 fixed a hard hang in this code path.
//   - v0.27.6 stopped a 45s envelope-level timeout from clobbering single-row
//     errors in the regime fetcher.
//   - v0.27.9 added boundedSnapshot/boundedSnapshotWith52WHigh in
//     internal/daemon/regime.go as orchestrator-level defense — a goroutine
//     race that returned zeros after `budget + 1s` regardless of whether the
//     inner SubscribeMarketData honoured ctx.
//   - F-26 (this commit) closes the structural gap: acquireMarketDataSlot now
//     receives the caller's ctx, so the inner per-fetcher budget is enforced
//     at the slot layer instead of being silently absorbed by Connection.ctx
//     (daemon lifetime).
//
// Pre-F-26 the call below would block until either the connection's lifetime
// ctx fired (never, in this test setup) or a slot was released (never, the
// pool is intentionally saturated). The test would therefore hang past its
// own deadline. Post-F-26 the caller's 1 s ctx deadline returns a wrapped
// context.DeadlineExceeded in roughly 1 s.
func TestRequestMarketDataWithContract_HonoursCallerCtxOnSlotSaturation(t *testing.T) {
	t.Parallel()

	var out safeBuffer
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(&out)

	// Saturate the market-data slot pool. The semaphore capacity is 100
	// (NewRateLimiter sets it; the value isn't a public API so we fill it
	// by repeated Acquire calls rather than hard-coding it). Acquire of a
	// fresh semaphore is a non-blocking channel send so this is fast.
	bg := context.Background()
	for i := range 100 {
		if err := conn.rateLimiter.AcquireMarketDataSlot(bg); err != nil {
			t.Fatalf("priming slot %d: %v", i, err)
		}
	}
	// One more should now block — verify the pool really is saturated. We
	// give it a tight 50 ms ctx so the test fails fast if the priming was
	// off.
	probeCtx, probeCancel := context.WithTimeout(bg, 50*time.Millisecond)
	if err := conn.rateLimiter.AcquireMarketDataSlot(probeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected saturated pool to time out the probe acquire, got %v", err)
	}
	probeCancel()

	contract := Contract{
		Symbol:      "SPY",
		SecType:     "STK",
		Exchange:    "SMART",
		Currency:    "USD",
		LocalSymbol: "SPY",
	}

	// 1 s ctx deadline — the structural fix should surface this here, not
	// the Connection.ctx (which is Background in this test setup and would
	// hang forever pre-fix).
	ctx, cancel := context.WithTimeout(bg, 1*time.Second)
	defer cancel()

	start := time.Now()
	_, err := conn.RequestMarketDataWithContract(ctx, contract, "100,101,104", false, false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error from saturated slot pool, got reqID with no error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected wrapped context.DeadlineExceeded, got %v", err)
	}
	// Should fire roughly at the ctx deadline. Generous upper bound for
	// CI jitter; the critical invariant is that we did NOT wait
	// indefinitely on Connection.ctx, which is the pre-F-26 behaviour.
	if elapsed > 2*time.Second {
		t.Fatalf("acquire took %v with 1s ctx deadline — caller ctx not honoured (regression of F-26)", elapsed)
	}
	// Symmetric lower bound: should not return suspiciously early, which
	// would suggest the ctx deadline was bypassed entirely.
	if elapsed < 800*time.Millisecond {
		t.Fatalf("acquire returned in %v — suspiciously fast for a 1s ctx", elapsed)
	}
}
