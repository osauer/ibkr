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
		err := rl.SubmitWithPriority(RequestTypeGeneral, func() error { return sendErr }, 0, 0)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "error 100") {
			t.Fatalf("expected rate limit error, got %v", err)
		}
	}

	err := rl.SubmitWithPriority(RequestTypeGeneral, func() error { return sendErr }, 0, 0)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "circuit") {
		t.Fatalf("expected circuit breaker error, got %v", err)
	}

	time.Sleep(rl.circuitCooldown + 50*time.Millisecond)

	if err := rl.SubmitWithPriority(RequestTypeGeneral, func() error { return nil }, 0, 0); err != nil {
		t.Fatalf("expected successful request after cooldown, got %v", err)
	}

	metrics := rl.GetMetrics()
	if metrics.ConsecutiveErrors != 0 {
		t.Fatalf("expected consecutive errors reset, got %d", metrics.ConsecutiveErrors)
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
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 50 {
					_ = rl.SubmitWithPriority(RequestTypeGeneral, func() error { return nil }, 0, 1)
				}
			}()
		}

		time.Sleep(2 * time.Millisecond)
		rl.Stop()
		wg.Wait()
	}
}
