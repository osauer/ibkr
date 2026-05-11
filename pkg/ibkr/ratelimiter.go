package ibkr

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/pkg/ibkr/internal/logging"
)

var rateLimiterLogger = logging.Component("IBKR RateLimiter")

// RateLimiter manages rate limiting for IBKR API requests
// Based on IBKR limits documented in docs/IBKR_limits.md
type RateLimiter struct {
	// Token buckets for different rate limits
	messageRate    *TokenBucket // General messages: 40/sec (safe from 50/sec max)
	historicalRate *TokenBucket // Historical data: 60 requests per 10 minutes

	// Semaphores for concurrent limits
	historicalConcurrent *Semaphore // Max 50 concurrent historical requests
	marketDataSubs       *Semaphore // Max 100 concurrent market data subscriptions (retail)

	// Queue for pacing requests
	requestQueue chan *RateLimitedRequest

	// Metrics
	metrics   *RateLimiterMetrics
	metricsMu sync.RWMutex

	// Circuit breaker for repeated rate limit violations
	circuitMu        sync.Mutex
	circuitOpenUntil time.Time
	circuitThreshold int
	circuitCooldown  time.Duration

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// RateLimiterMetrics tracks rate limiting statistics
type RateLimiterMetrics struct {
	TotalRequests        uint64
	ThrottledRequests    uint64
	RejectedRequests     uint64
	CurrentQueueDepth    int
	MessageRatePerSec    float64
	HistoricalRatePerMin float64
	LastRateLimitError   time.Time
	ConsecutiveErrors    int
}

// RateLimitedRequest wraps a request with rate limiting metadata
type RateLimitedRequest struct {
	Type       RequestType
	SendFunc   func() error
	ResultChan chan error
	Priority   int // Higher priority = processed first
	Timestamp  time.Time
	Retries    int
	MaxRetries int
}

// RequestType categorizes different IBKR request types for proper rate limiting
type RequestType int

const (
	RequestTypeGeneral RequestType = iota
	RequestTypeMarketData
	RequestTypeHistorical
	RequestTypeOrder
	RequestTypeAccount
	RequestTypeHeartbeat // Special priority for heartbeats
)

// TokenBucket implements token bucket algorithm for rate limiting
type TokenBucket struct {
	capacity   int     // Max tokens
	tokens     float64 // Current tokens (float for fractional refill)
	refillRate float64 // Tokens per second
	lastRefill time.Time
	mu         sync.Mutex
}

// NewTokenBucket creates a new token bucket
func NewTokenBucket(capacity int, refillRate float64) *TokenBucket {
	return &TokenBucket{
		capacity:   capacity,
		tokens:     float64(capacity), // Start full
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// TryAcquire attempts to acquire n tokens, returns true if successful
func (tb *TokenBucket) TryAcquire(n int) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	// Refill tokens based on elapsed time
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens = min(float64(tb.capacity), tb.tokens+elapsed*tb.refillRate)
	tb.lastRefill = now

	// Check if we have enough tokens
	if tb.tokens >= float64(n) {
		tb.tokens -= float64(n)
		return true
	}
	return false
}

// WaitForTokens blocks until n tokens are available
func (tb *TokenBucket) WaitForTokens(ctx context.Context, n int) error {
	ticker := time.NewTicker(10 * time.Millisecond) // Check every 10ms
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if tb.TryAcquire(n) {
				return nil
			}
		}
	}
}

// Semaphore limits concurrent operations
type Semaphore struct {
	ch chan struct{}
}

// NewSemaphore creates a semaphore with given capacity
func NewSemaphore(capacity int) *Semaphore {
	return &Semaphore{
		ch: make(chan struct{}, capacity),
	}
}

// Acquire blocks until a slot is available
func (s *Semaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryAcquire attempts to acquire without blocking
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees a slot
func (s *Semaphore) Release() {
	select {
	case <-s.ch:
		// Released
	default:
		// Channel was empty (shouldn't happen in normal use)
		rateLimiterLogger.Warnf("Release called on empty semaphore")
	}
}

// Count returns current number of acquired slots
func (s *Semaphore) Count() int {
	return len(s.ch)
}

// NewRateLimiter creates a rate limiter with IBKR-compliant limits
func NewRateLimiter(ctx context.Context) *RateLimiter {
	ctx, cancel := context.WithCancel(ctx)

	rl := &RateLimiter{
		// Message rate: 40/sec (safe from 50/sec max)
		messageRate: NewTokenBucket(40, 40),

		// Historical: 60 requests per 10 minutes = 0.1 requests/sec
		historicalRate: NewTokenBucket(60, 0.1),

		// Concurrent limits
		historicalConcurrent: NewSemaphore(50),  // Max 50 concurrent historical
		marketDataSubs:       NewSemaphore(100), // Max 100 market data subscriptions

		// Request queue with buffer
		requestQueue: make(chan *RateLimitedRequest, 1000),

		metrics:          &RateLimiterMetrics{},
		ctx:              ctx,
		cancel:           cancel,
		circuitThreshold: 5,
		circuitCooldown:  10 * time.Second,
	}

	// Start request processor
	rl.wg.Add(1)
	go rl.processRequests()

	// Start metrics updater
	rl.wg.Add(1)
	go rl.updateMetrics()

	return rl
}

// Stop gracefully shuts down the rate limiter
func (rl *RateLimiter) Stop() {
	rl.cancel()
	close(rl.requestQueue)
	rl.wg.Wait()
}

// Submit submits a request for rate-limited execution
func (rl *RateLimiter) Submit(reqType RequestType, sendFunc func() error) error {
	return rl.SubmitWithPriority(reqType, sendFunc, 0, 3) // Default: priority 0, max 3 retries
}

// SubmitWithPriority submits a request with custom priority and retry settings
func (rl *RateLimiter) SubmitWithPriority(reqType RequestType, sendFunc func() error, priority int, maxRetries int) error {
	if err := rl.checkCircuit(reqType); err != nil {
		return err
	}

	// For heartbeats, use highest priority
	if reqType == RequestTypeHeartbeat {
		priority = 1000
	}

	req := &RateLimitedRequest{
		Type:       reqType,
		SendFunc:   sendFunc,
		ResultChan: make(chan error, 1),
		Priority:   priority,
		Timestamp:  time.Now(),
		MaxRetries: maxRetries,
	}

	// Try to queue the request
	select {
	case rl.requestQueue <- req:
		// Wait for result with timeout
		select {
		case err := <-req.ResultChan:
			return err
		case <-time.After(30 * time.Second):
			rl.incrementRejected()
			return fmt.Errorf("request timeout after 30s")
		case <-rl.ctx.Done():
			return fmt.Errorf("rate limiter stopped")
		}
	default:
		// Queue is full
		rl.incrementRejected()
		rl.recordRateLimitError()
		return fmt.Errorf("request queue full (1000 pending)")
	}
}

// processRequests processes queued requests with rate limiting
func (rl *RateLimiter) processRequests() {
	defer rl.wg.Done()

	for {
		select {
		case <-rl.ctx.Done():
			return
		case req, ok := <-rl.requestQueue:
			if !ok {
				return
			}

			// Process the request with appropriate rate limiting
			err := rl.executeRequest(req)

			// Handle retries if needed
			if err != nil && req.Retries < req.MaxRetries {
				req.Retries++
				// Re-queue with exponential backoff. Honor rl.ctx so a
				// shutdown mid-backoff doesn't leak a goroutine sleeping
				// out the remainder of the delay.
				rl.wg.Add(1)
				go func(backoff time.Duration) {
					defer rl.wg.Done()
					select {
					case <-time.After(backoff):
					case <-rl.ctx.Done():
						req.ResultChan <- rl.ctx.Err()
						return
					}
					select {
					case rl.requestQueue <- req:
						// Re-queued successfully
					case <-rl.ctx.Done():
						req.ResultChan <- rl.ctx.Err()
					default:
						// Queue full, give up
						req.ResultChan <- fmt.Errorf("retry failed: queue full")
					}
				}(time.Duration(req.Retries) * time.Second)
			} else {
				// Send result back
				req.ResultChan <- err
			}
		}
	}
}

// executeRequest executes a single request with appropriate rate limiting
func (rl *RateLimiter) executeRequest(req *RateLimitedRequest) error {
	rl.incrementTotal()

	// Wait for general message rate limit (all requests)
	if err := rl.messageRate.WaitForTokens(rl.ctx, 1); err != nil {
		rl.incrementThrottled()
		rl.recordRateLimitError()
		return fmt.Errorf("rate limit cancelled: %w", err)
	}

	// Apply type-specific limits
	switch req.Type {
	case RequestTypeHistorical:
		// Wait for historical rate limit
		if err := rl.historicalRate.WaitForTokens(rl.ctx, 1); err != nil {
			rl.incrementThrottled()
			rl.recordRateLimitError()
			return fmt.Errorf("historical rate limit: %w", err)
		}

		// Acquire concurrent slot
		if err := rl.historicalConcurrent.Acquire(rl.ctx); err != nil {
			rl.incrementThrottled()
			rl.recordRateLimitError()
			return fmt.Errorf("historical concurrent limit: %w", err)
		}
		defer rl.historicalConcurrent.Release()

	case RequestTypeMarketData:
		// Check market data subscription limit
		if rl.marketDataSubs.Count() >= 100 {
			rl.incrementThrottled()
			rl.recordRateLimitError()
			return fmt.Errorf("market data subscription limit reached (100)")
		}
		// Note: Caller must manage subscription lifecycle with AcquireMarketDataSlot/ReleaseMarketDataSlot
	}

	// Execute the actual request
	err := req.SendFunc()
	if err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "error 100") || strings.Contains(lower, "rate limit") {
			rl.recordRateLimitError()
		} else {
			rl.resetRateLimitErrors()
		}
	} else {
		rl.resetRateLimitErrors()
	}

	return err
}

// AcquireMarketDataSlot acquires a market data subscription slot
func (rl *RateLimiter) AcquireMarketDataSlot(ctx context.Context) error {
	return rl.marketDataSubs.Acquire(ctx)
}

// ReleaseMarketDataSlot releases a market data subscription slot
func (rl *RateLimiter) ReleaseMarketDataSlot() {
	rl.marketDataSubs.Release()
}

// GetMetrics returns current rate limiter metrics
func (rl *RateLimiter) GetMetrics() RateLimiterMetrics {
	rl.metricsMu.RLock()
	defer rl.metricsMu.RUnlock()

	metrics := *rl.metrics
	metrics.CurrentQueueDepth = len(rl.requestQueue)
	return metrics
}

// updateMetrics periodically updates rate metrics
func (rl *RateLimiter) updateMetrics() {
	defer rl.wg.Done()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastTotal uint64
	var lastHistorical uint64

	for {
		select {
		case <-rl.ctx.Done():
			return
		case <-ticker.C:
			rl.metricsMu.Lock()

			// Calculate rates
			currentTotal := rl.metrics.TotalRequests
			rl.metrics.MessageRatePerSec = float64(currentTotal - lastTotal)
			lastTotal = currentTotal

			// Historical rate (per minute)
			if time.Now().Unix()%60 == 0 {
				rl.metrics.HistoricalRatePerMin = float64(rl.metrics.TotalRequests - lastHistorical)
				lastHistorical = rl.metrics.TotalRequests
			}

			rl.metricsMu.Unlock()
		}
	}
}

// Helper methods for metrics
func (rl *RateLimiter) incrementTotal() {
	rl.metricsMu.Lock()
	rl.metrics.TotalRequests++
	rl.metricsMu.Unlock()
}

func (rl *RateLimiter) incrementThrottled() {
	rl.metricsMu.Lock()
	rl.metrics.ThrottledRequests++
	rl.metricsMu.Unlock()
}

func (rl *RateLimiter) incrementRejected() {
	rl.metricsMu.Lock()
	rl.metrics.RejectedRequests++
	rl.metricsMu.Unlock()
}

func (rl *RateLimiter) recordRateLimitError() {
	now := time.Now()
	rl.metricsMu.Lock()
	rl.metrics.LastRateLimitError = now
	rl.metrics.ConsecutiveErrors++
	count := rl.metrics.ConsecutiveErrors
	rl.metricsMu.Unlock()

	rateLimiterLogger.Warnf("IBKR rate limit error detected (consecutive: %d)", count)

	if rl.circuitThreshold > 0 && count >= rl.circuitThreshold {
		rl.openCircuit(now.Add(rl.circuitCooldown))
	}
}

func (rl *RateLimiter) resetRateLimitErrors() {
	rl.metricsMu.Lock()
	rl.metrics.ConsecutiveErrors = 0
	rl.metricsMu.Unlock()
}

func (rl *RateLimiter) openCircuit(openUntil time.Time) {
	rl.circuitMu.Lock()
	if rl.circuitOpenUntil.Before(openUntil) {
		rl.circuitOpenUntil = openUntil
		rateLimiterLogger.Warnf("Circuit breaker open until %s", openUntil.Format(time.RFC3339))
	}
	rl.circuitMu.Unlock()
}

func (rl *RateLimiter) checkCircuit(reqType RequestType) error {
	rl.circuitMu.Lock()
	defer rl.circuitMu.Unlock()

	if rl.circuitOpenUntil.IsZero() {
		return nil
	}

	now := time.Now()
	if now.After(rl.circuitOpenUntil) {
		rl.circuitOpenUntil = time.Time{}
		return nil
	}

	if reqType == RequestTypeHeartbeat {
		return nil
	}

	return fmt.Errorf("rate limiter circuit breaker open until %s", rl.circuitOpenUntil.Format(time.RFC3339))
}

// min returns the minimum of two float64 values
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
