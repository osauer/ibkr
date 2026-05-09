package ibkr

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/osauer/ibkr/pkg/ibkr/internal/logging"
)

var prewarmLogger = logging.Component("IBKR Prewarm")

// PrewarmConfig configures contract pre-warming behavior
type PrewarmConfig struct {
	Enabled          bool          // Enable/disable pre-warming
	FailureMode      string        // "fatal" or "warning"
	Timeout          time.Duration // Per-symbol timeout
	MaxConcurrency   int           // Max concurrent contract detail requests (1 = sequential)
	MaxRetries       int           // Max retries per symbol (0 = no retries)
	RetryDelay       time.Duration // Initial retry delay (doubles on each retry)
	LateArrivalGrace time.Duration // Extra wait for late contract details before retrying
}

// prewarmResult tracks the result of a single contract pre-warming attempt
type prewarmResult struct {
	symbol string
	conID  int
	err    error
}

// PrewarmContracts fetches contract details for all symbols and waits for completion.
// This ensures contract IDs are cached before historical data requests are made,
// preventing race conditions that cause error 320 (conID=0).
//
// The function respects the MaxConcurrency limit to avoid overwhelming IBKR's API.
// When FailureMode is "fatal", any symbol that fails to resolve will cause the
// function to return an error. When FailureMode is "warning", failures are logged
// but the function continues.
func (c *Connector) PrewarmContracts(ctx context.Context, symbols []string, cfg PrewarmConfig) error {
	if !cfg.Enabled {
		prewarmLogger.Debugf("Pre-warming disabled by config")
		return nil
	}
	if !c.isConnected() {
		prewarmLogger.Warnf("Skipping contract pre-warm: IBKR connector not connected (degraded mode)")
		return nil
	}

	// Default timeout if not configured
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}

	if cfg.LateArrivalGrace <= 0 {
		cfg.LateArrivalGrace = contractDetailsLateGrace
	}

	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 1
	}

	if len(symbols) == 0 {
		prewarmLogger.Debugf("No symbols to pre-warm")
		return nil
	}

	prewarmLogger.Debugf("Pre-warming contract details for %d symbols (timeout=%s, concurrency=%d, failureMode=%s)",
		len(symbols), cfg.Timeout, cfg.MaxConcurrency, cfg.FailureMode)

	results := make(chan prewarmResult, len(symbols))
	var wg sync.WaitGroup

	// Semaphore for concurrency control (1 = sequential, 5 = parallel, etc.)
	sem := make(chan struct{}, cfg.MaxConcurrency)

	fetch := c.fetchContractDetails
	if fetch == nil {
		fetch = c.FetchContractDetails
	}

	for _, sym := range symbols {
		wg.Add(1)
		go func(symbol string) {
			defer wg.Done()

			// Acquire semaphore slot
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }() // Release slot
			case <-ctx.Done():
				results <- prewarmResult{symbol: symbol, err: ctx.Err()}
				return
			}

			// Fetch contract details with retry
			var details []ContractDetailsLite
			var err error
			maxRetries := cfg.MaxRetries
			if maxRetries == 0 {
				maxRetries = 2 // Default: 2 retries
			}
			retryDelay := cfg.RetryDelay
			if retryDelay == 0 {
				retryDelay = 2 * time.Second // Default: 2s initial delay
			}

			for attempt := 0; attempt <= maxRetries; attempt++ {
				if _, inactive := c.InactiveReason(symbol); inactive {
					results <- prewarmResult{symbol: symbol, err: ErrSymbolInactive}
					return
				}

				select {
				case <-ctx.Done():
					results <- prewarmResult{symbol: symbol, err: ctx.Err()}
					return
				default:
				}

				if attempt > 0 {
					delay := retryDelay * time.Duration(1<<uint(attempt-1)) // Exponential backoff
					prewarmLogger.Debugf("Retrying %s (attempt %d/%d) after %v", symbol, attempt+1, maxRetries+1, delay)
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						results <- prewarmResult{symbol: symbol, err: ctx.Err()}
						return
					}
				}

				if c.conn == nil || !c.conn.IsConnected() {
					results <- prewarmResult{symbol: symbol, err: fmt.Errorf("ibkr connection unavailable")}
					return
				}

				if cached := c.cachedContractDetail(symbol); cached != nil && cached.ConID != 0 {
					results <- prewarmResult{symbol: symbol, conID: cached.ConID, err: nil}
					return
				}

				attemptStart := time.Now()
				details, err = fetch(symbol, cfg.Timeout)
				if err == nil && len(details) > 0 {
					latency := time.Since(attemptStart)
					prewarmLogger.Debugf("Pre-warmed %s (attempt %d/%d, conID=%d, latency=%s)", symbol, attempt+1, maxRetries+1, details[0].ConID, latency)
					results <- prewarmResult{symbol: symbol, conID: details[0].ConID, err: nil}
					return
				}

				if attempt < maxRetries {
					prewarmLogger.Infof("Pre-warm attempt %d/%d for %s failed: %v", attempt+1, maxRetries+1, symbol, err)
				}

				if err != nil {
					lateDetail := c.awaitContractDetail(symbol, cfg.LateArrivalGrace)
					if lateDetail != nil && lateDetail.ConID != 0 {
						latency := time.Since(attemptStart)
						prewarmLogger.Infof("Pre-warm for %s recovered after timeout (conID=%d, latency=%s)", symbol, lateDetail.ConID, latency)
						results <- prewarmResult{symbol: symbol, conID: lateDetail.ConID, err: nil}
						return
					}
				}
			}

			// All retries exhausted
			if err == nil && len(details) == 0 {
				err = fmt.Errorf("no contract details returned after %d attempts", maxRetries+1)
			}
			results <- prewarmResult{symbol: symbol, err: fmt.Errorf("failed after %d attempts: %w", maxRetries+1, err)}
		}(sym)
	}

	// Wait for all goroutines to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	successes := 0
	failures := 0
	var firstError error

	for res := range results {
		if res.err != nil {
			prewarmLogger.Warnf("Failed to pre-warm contract for %s: %v", res.symbol, res.err)
			failures++
			if firstError == nil {
				firstError = fmt.Errorf("pre-warm failed for %s: %w", res.symbol, res.err)
			}
		} else {
			prewarmLogger.Debugf("Pre-warmed contract for %s (conID=%d)", res.symbol, res.conID)
			successes++
		}
	}

	prewarmLogger.Infof("Pre-warming complete: %d succeeded, %d failed", successes, failures)

	// Handle failure mode
	if failures > 0 {
		if cfg.FailureMode == "fatal" {
			return fmt.Errorf("pre-warming failed for %d/%d symbols (first error: %w)", failures, len(symbols), firstError)
		}
		// Warning mode - log but continue
		prewarmLogger.Warnf("Pre-warming incomplete: %d/%d symbols failed (continuing anyway)", failures, len(symbols))
	}

	if successes == 0 {
		if cfg.FailureMode == "fatal" {
			return fmt.Errorf("all %d contract pre-warming attempts failed", failures)
		}
		prewarmLogger.Warnf("Contract pre-warm yielded no successes; continuing in warning mode")
		return nil
	}

	return nil
}
