package daemon

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/rpc"
)

// handleGammaZeroSPX returns the current dealer zero-gamma estimate for
// SPX (Indicator 4 of the risk-regime dashboard). The compute is heavy
// (multi-minute fan-out across hundreds of option legs) and runs on a
// daemon-internal goroutine — the RPC always returns within budget,
// carrying a Status of "computing" while the work is in flight and
// "ready" once a same-NY-session result is cached.
//
// Concurrency contract:
//
//   - The first caller of an NY trading session kicks the compute and
//     gets Status="computing" + an EtaSeconds hint. They can either
//     return immediately or set WaitMs > 0 to block for the result.
//   - Concurrent callers within the same session share the in-flight
//     job (singleflight) — no duplicate fan-outs against the same
//     gateway slot pool.
//   - Callers after the compute finishes get Status="ready" with the
//     cached payload until the next NY midnight, regardless of WaitMs.
//   - Force=true on the request supersedes the cached/in-flight result
//     and starts fresh. Diagnostics only; the cache handles freshness.
//
// Methodology lives in docs/specs/risk-regime-dashboard.md. The result
// envelope's Method field is "perfiliev-bs-sweep-v1"; renderers can
// deep-link to the disclosure.
func (s *Server) handleGammaZeroSPX(ctx context.Context, req *rpc.Request) (*rpc.GammaZeroSPXResult, error) {
	var p rpc.GammaZeroSPXParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}

	// Scope: which underlying(s) to compute. Empty defaults to combined
	// (the new canonical headline post-step-7). Single-underlying paths
	// (--only=spy / --only=spx) bypass the canonical cache via force()
	// since they're diagnostic shapes.
	scope, scopeErr := gammaScopeForRequest(p.Scope)
	if scopeErr != nil {
		return nil, fmt.Errorf("zero-gamma: %w", scopeErr)
	}

	// Background ctx for the compute goroutine — independent of the
	// per-RPC ctx because the compute outlives any single client call.
	// serverCtx is set on Start and matches the daemon's lifetime, so
	// daemon shutdown cancels the compute cleanly.
	s.mu.Lock()
	parent := s.serverCtx
	s.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}

	// Build the compute closure. The cache layer owns goroutine
	// lifecycle; we hand it a function that closes over the gateway
	// connector + params.
	//
	// The closure acquires a refcounted Hold on the underlying for
	// the entire lifetime of the compute. IBKR's TWS API requires a
	// market-data subscription on the option's underlying to push
	// OPTION_COMPUTATION (msg 21) ticks for OPT subscriptions; without
	// it the model engine has no live spot anchor and the per-leg fan-
	// out lands ~0% IV/greeks (observed: 12/1256 legs at 1% coverage
	// pre-market). subManager.Hold is refcounted, so a concurrent
	// regime snapshot on the same symbol is safe — the line stays
	// open until the compute releases.
	//
	// Per-scope compute selection:
	//   combined  → SPY phase then SPX phase, with separate Holds
	//               (computeGammaCombined enforces the underlying-hold
	//               transition audit checklist item from design §7.1).
	//   spy / spx → single-underlying phase under one Hold.
	params := normalizeGammaParams(rpc.GammaZeroParams{})
	compute := func(bgCtx context.Context, prog *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		switch scope {
		case rpc.GammaZeroScopeCombined:
			return computeGammaCombined(bgCtx, s, c, params, prog)
		case rpc.GammaZeroScopeSPX:
			return runUnderlyingPhase(bgCtx, s, c, "SPX", params, prog, 0)
		default: // GammaZeroScopeSPY
			return runUnderlyingPhase(bgCtx, s, c, "SPY", params, prog, 0)
		}
	}

	var job *gammaComputation
	if p.Force {
		job = s.zeroGamma.force(parent, time.Now(), computeETA, compute)
	} else {
		job, _ = s.zeroGamma.kickOrJoin(parent, time.Now(), computeETA, compute)
	}

	// Optional wait: bounded by both the caller's WaitMs and the per-
	// method deadline (which itself sits under the CLI's 60 s ceiling
	// — see unaryDeadline). The RPC ctx provides the upper bound; we
	// don't need a separate timer.
	if p.WaitMs > 0 {
		// Cap the wait at the RPC deadline so we always return before
		// the dispatcher times us out. The per-method deadline for
		// GammaZeroSPX is intentionally long enough to make WaitMs
		// usable but shorter than the bg compute itself, so a high
		// WaitMs still returns "computing" if the compute hasn't
		// finished.
		waitCtx, waitCancel := context.WithTimeout(ctx, time.Duration(p.WaitMs)*time.Millisecond)
		defer waitCancel()
		select {
		case <-job.done:
			// compute finished — fall through to snapshot
		case <-waitCtx.Done():
			// either WaitMs elapsed or the RPC deadline fired —
			// either way, return current state
		}
	}

	env := s.zeroGamma.snapshot(job, time.Now)
	return &env, nil
}
