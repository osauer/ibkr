package ibkr

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRateLimiterCircuitBreakerTriggers(t *testing.T) {
	rl := NewRateLimiter(context.Background())
	t.Cleanup(rl.Stop)

	rl.circuitThreshold = 2
	rl.circuitCooldown = 200 * time.Millisecond

	sendErr := fmt.Errorf("ERROR 100: rate limit exceeded")

	for i := 0; i < rl.circuitThreshold; i++ {
		err := rl.SubmitWithRetries(RequestTypeGeneral, func() error { return sendErr }, 0)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "error 100") {
			t.Fatalf("expected rate limit error, got %v", err)
		}
	}

	err := rl.SubmitWithRetries(RequestTypeGeneral, func() error { return sendErr }, 0)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "circuit") {
		t.Fatalf("expected circuit breaker error, got %v", err)
	}

	time.Sleep(rl.circuitCooldown + 50*time.Millisecond)

	if err := rl.SubmitWithRetries(RequestTypeGeneral, func() error { return nil }, 0); err != nil {
		t.Fatalf("expected successful request after cooldown, got %v", err)
	}

	metrics := rl.GetMetrics()
	if metrics.ConsecutiveErrors != 0 {
		t.Fatalf("expected consecutive errors reset, got %d", metrics.ConsecutiveErrors)
	}
}

func TestTokenBucketReservationsStaggerWaiters(t *testing.T) {
	tb := NewTokenBucket(10, 40)
	now := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	tb.mu.Lock()
	tb.tokens = 0
	tb.lastRefill = now
	tb.mu.Unlock()

	delay, reserved := tb.reserve(1, now)
	if !reserved {
		t.Fatalf("first waiter did not reserve a future token")
	}
	if delay < 24*time.Millisecond || delay > 26*time.Millisecond {
		t.Fatalf("first delay=%s, want about 25ms", delay)
	}

	delay, reserved = tb.reserve(1, now)
	if !reserved {
		t.Fatalf("second waiter did not reserve a future token")
	}
	if delay < 49*time.Millisecond || delay > 51*time.Millisecond {
		t.Fatalf("second delay=%s, want about 50ms", delay)
	}
}

func TestTokenBucketRejectsImpossibleWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := NewTokenBucket(1, 1).WaitForTokens(ctx, 2)
	if err == nil || !strings.Contains(err.Error(), "exceeds bucket capacity") {
		t.Fatalf("WaitForTokens err=%v, want capacity error", err)
	}
}

// TestRateLimiterStopRace exercises concurrent Submit/Stop. Before the fix,
// Stop closed requestQueue while Submit and the retry goroutine could still
// send to it, occasionally panicking with "send on closed channel".
func TestRateLimiterStopRace(t *testing.T) {
	for range 25 {
		rl := NewRateLimiter(context.Background())

		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				for range 50 {
					_ = rl.SubmitWithRetries(RequestTypeGeneral, func() error { return nil }, 1)
				}
			})
		}

		time.Sleep(2 * time.Millisecond)
		rl.Stop()
		wg.Wait()
	}
}
