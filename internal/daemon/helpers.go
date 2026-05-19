package daemon

import (
	"context"
	"strings"
	"sync"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// pollCadence is the shared 75 ms cadence at which short-lived snapshot
// polls re-read the IBKR market-data / option cache. Previously inlined
// at seven call sites; now changing the cadence is a one-line edit.
const pollCadence = 75 * time.Millisecond

// pollUntil drives a polling loop on the standard cadence until fn signals
// done, the context is cancelled, or the deadline passes. Returns nil iff
// fn returned true; otherwise the cancellation reason (ctx.Err() or
// context.DeadlineExceeded).
//
// The IBKR Subscribe/Unsubscribe call is the caller's responsibility — this
// helper only owns the loop. Use ptrIfPos to lift the scalar fields the
// predicate observed.
func pollUntil(ctx context.Context, deadline time.Time, fn func() (done bool)) error {
	if fn() {
		return nil
	}
	poll := time.NewTicker(pollCadence)
	defer poll.Stop()
	for {
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
		if fn() {
			return nil
		}
	}
}

// pollMarketData is the common case of pollUntil: poll
// c.GetMarketData()[key] until predicate returns true. Predicate is invoked
// only when the cache has an entry for key.
func pollMarketData(ctx context.Context, c *ibkrlib.Connector, key string, deadline time.Time, predicate func(*ibkrlib.MarketData) bool) error {
	return pollUntil(ctx, deadline, func() bool {
		data, ok := c.GetMarketData()[key]
		if !ok {
			return false
		}
		return predicate(data)
	})
}

// ptrIfPos returns &v when v > 0 (using ordered comparison) and nil
// otherwise. Replaces ~80 instances of `if x > 0 { v := x; row.X = &v }`
// across handlers.go and subs.go. The fresh-local pattern is preserved so
// callers don't share storage across rows.
func ptrIfPos[T int | int64 | float64](v T) *T {
	if v > 0 {
		x := v
		return &x
	}
	return nil
}

// normCcy normalises a currency code: uppercase, trimmed. Centralises the
// ~18 inlined `strings.ToUpper(strings.TrimSpace(...))` calls in handlers.go
// that had already drifted (e.g. handlers.go:622 vs handlers.go:1107).
func normCcy(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// normSym is normCcy aliased for symbol normalisation — same rule, but the
// call site reads clearer.
func normSym(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// runBounded runs fn(jobs[i]) concurrently with at most workers in flight.
// Replaces hand-rolled buffered-channel + WaitGroup blocks across
// handlers.go (prewarmPrevCloses, prewarmOptionGreeks, chain expiry IV
// fetch, chain strike fetch, scan-row enrichment). The fn closure is
// responsible for ctx-cancellation observation if it does any blocking
// work; this helper only bounds parallelism.
func runBounded[T any](jobs []T, workers int, fn func(T)) {
	if len(jobs) == 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}
	ch := make(chan T, len(jobs))
	for _, j := range jobs {
		ch <- j
	}
	close(ch)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for j := range ch {
				fn(j)
			}
		})
	}
	wg.Wait()
}
