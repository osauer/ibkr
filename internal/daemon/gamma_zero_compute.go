package daemon

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// Default calibration window for the zero-gamma compute. Tuned for
// the trader-side review: 6 expirations beats the SpotGamma 4-expiry
// default in nominal coverage; ±10 % strike width keeps the leg count
// reasonable; ±15 % sweep range comfortably brackets the typical zero
// crossing without inflating the profile point count.
//
// WorkerCount 4 matches the documented safe gateway throttle elsewhere
// in this package (handleChainFetch, around handlers.go:1628). Bumping
// it requires retuning AcquireMarketDataSlot and is a deliberate
// follow-up, not a v1 knob.
const (
	defaultExpiryCount    = 6
	defaultStrikeWidthPct = 0.10
	defaultSweepRangePct  = 0.15
	defaultWorkerCount    = 4

	// nearDTECutoffYears is the boundary between near and term gamma
	// buckets — 7 days in fractional years. Picked to capture 0DTE
	// through end-of-week (the high-velocity flow segment that's now
	// ~59 % of 2025 SPX volume per Cboe Aug 2025), separating it from
	// the monthly OPEX dynamics that dominate the term bucket.
	// Hardcoded rather than parameterised on GammaZeroParams — it's a
	// regime-meaningful boundary, not a tunable knob.
	nearDTECutoffYears = 7.0 / 365.0

	// sweepPoints is the number of (spot, GEX) samples in the profile.
	// 60 points across [0.85, 1.15] × spot ≈ 0.5 % per point, which
	// fits the precision the methodology can defensibly claim.
	sweepPoints = 60

	// topStrikesK is the number of concentration rows on the result —
	// enough for a renderer to draw the "call wall / put wall" view
	// without flooding the JSON payload.
	topStrikesK = 8

	// Throttle-signal abort. The option-chain fan-out makes hundreds
	// of reqContractDetails calls in close succession; the gateway can
	// rate-limit by returning empty contractDataEnd responses ("no
	// security definition") instead of real details. Continuing the
	// fan-out under that condition just deepens the rate-limit hole.
	//
	// Rule: after at least throttleSampleSize completions, if the
	// observed contract-resolve failure ratio exceeds throttleAbortPct,
	// stop launching new fetches and surface a "throttled" warning on
	// whatever we managed to collect.
	//
	// 50 / 5 % numbers are conservative — 50 is a meaningful sample
	// floor without delaying the abort, and 5 % is well above the
	// expected baseline of zero (every strike we enqueue came from the
	// gateway's own list, so a healthy session should hit ~0 % resolve
	// failures).
	throttleSampleSize = 50
	throttleAbortPct   = 0.05

	// computeETA is the static initial seconds-to-complete estimate the
	// cache stamps on a fresh kickoff. Calibration after the v0.24.x
	// IV-source fix:
	//   6 expirations × ~80 strikes × 2 sides ≈ 960 legs (worst case)
	//   actual landing rate ≈ 1-2 s/leg on warm contract cache
	//   960 / 4 workers × 1.5 s/leg ≈ 6 min worst case
	//   typical wall-clock 2-4 min.
	// 240s is the new conservative midpoint.
	computeETA = 240

	// earlyAbortAfter is how long the fan-out runs before checking
	// whether any leg has landed in the aggregator. Healthy runs see
	// their first usable leg within seconds; 30 s of total silence
	// means the gateway is not delivering the OPTION_COMPUTATION / OI
	// ticks the compute needs, and the right thing is to fail fast
	// with an actionable error instead of grinding for minutes.
	earlyAbortAfter = 30 * time.Second

	// MinLegCoverageFraction is the persist-or-not threshold: a
	// compute whose successful-leg fraction falls below this is
	// surfaced as an error (not a warning-flagged result), so the
	// existing gammaErrorRetryTTL machinery in gamma_zero_cache
	// re-attempts on the next call within the same NY trading
	// session. Mirrors breadth's MinCoverageFraction = 0.80 pattern
	// at internal/breadth/spx/types.go: "did not converge" runs are
	// not stored as session truth.
	//
	// Why 0.5 (vs breadth's 0.8): the OI-weighted gamma compute
	// concentrates near ATM, so missing far-OTM legs has small
	// impact on the zero-gamma estimate. Below 50 % of expected legs,
	// however, the ATM coverage itself is likely partial — and a
	// modelled zero-gamma level computed off half the chain is no
	// better than a guess. Pre-v0.27.9 this same 0.5 inline literal
	// merely emitted a "low_leg_coverage" warning while persisting
	// the result for the rest of the session.
	MinLegCoverageFraction = 0.5
)

// checkLegCoverage returns nil if the fan-out's leg-landing fraction
// passes MinLegCoverageFraction; otherwise an error describing the
// shortfall. Throttle-attribution is folded into the message when the
// gateway visibly throttled the fan-out, so the diagnostic names the
// likely cause without the caller having to combine two signals.
//
// Extracted so the persistence gate has a unit-testable surface
// independent of the full compute fixture (which requires a live
// connector). Pattern mirror of breadth's MinCoverageFraction at
// internal/breadth/spx/types.go: do not persist runs that did not
// converge.
func checkLegCoverage(landed, total int, throttled bool) error {
	if total == 0 {
		// Defensive zero-divide guard. The only way to reach here
		// with total==0 is a misconfigured request (no expirations
		// × strikes), which normalizeGammaParams already prevents —
		// but treating "no jobs" as an error rather than letting
		// coverage = NaN through is the right posture.
		return fmt.Errorf("low leg coverage: empty jobs list — no chain to compute over")
	}
	coverage := float64(landed) / float64(total)
	if coverage >= MinLegCoverageFraction {
		return nil
	}
	throttledHint := ""
	if throttled {
		throttledHint = " (gateway throttled the fan-out)"
	}
	return fmt.Errorf("low leg coverage: %d/%d legs landed (%.0f%%), below minimum %.0f%%%s — not persisting; gammaErrorRetryTTL will let the next call re-attempt", landed, total, coverage*100, MinLegCoverageFraction*100, throttledHint)
}

// legData carries the per-leg inputs the aggregator needs from the
// fan-out into the sweep. Captured at fetch time; iv stays fixed
// during the spot sweep (a documented v1 limitation — sticky-strike
// skew is on the deferred backlog).
type legData struct {
	expiryYMD string
	dte       float64 // years; positive
	strike    float64
	right     string // "C" | "P"
	isCall    bool
	iv        float64
	oi        int64
	// gamma is the gateway-supplied model-computation gamma at the
	// snapshot spot; used for the at-spot aggregate. The sweep
	// recomputes gamma via Black-Scholes for each scenario spot.
	gammaAtSnapshot float64
}

// legResult is the per-leg payload returned by a legFetcher. Bundled as
// a struct because adding the BS-IV-derived flag pushed the original
// 5-tuple into "what do these positional booleans mean" territory.
//
// OK reports whether OI + IV both landed (whether from the gateway's
// model tick or the BS-IV fallback) within budget — the aggregator
// only counts a leg when OK is true.
//
// Throttle reports a contract-resolve failure on a strike that came
// from the gateway's own enumeration. The aggregator counts these in
// the noContract tally that drives the throttle abort. Soft drops
// (subscribed but data didn't land) leave Throttle false: they're
// skip-this-leg without raising the throttle alarm.
//
// IVDerived is true when the IV was back-solved from an observed
// option price (Black-Scholes Newton-Raphson) rather than pushed by
// the gateway as a model-computation tick. Pre-market the model
// engine doesn't fire because there's no live option flow; the
// fallback path lets the compute still produce a number, at the cost
// of using yesterday's close as the price anchor. Surfaced to the
// caller so the result envelope can disclose how many legs used the
// fallback.
type legResult struct {
	OI        int64
	IV        float64
	Gamma     float64
	IVDerived bool
	OK        bool
	Throttle  bool
}

// legFetcher abstracts the per-leg subscribe-collect-unsubscribe so
// tests can drive computeGammaZeroSPX with a fake. The fetcher is
// expected to block for at most the budget the caller passes via ctx.
//
// snapshotSpot + snapshotAt are passed so the fetcher can compute a BS
// back-solve when the gateway doesn't deliver a model tick (the typical
// pre-market state). Both are captured by computeGammaZeroSPX before
// the fan-out begins; the fetcher does NOT take its own spot snapshot.
type legFetcher func(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying, expiryYMD string,
	strike float64,
	right string,
	snapshotSpot float64,
	snapshotAt time.Time,
) legResult

// productionLegFetcher is the live-gateway implementation. It mirrors
// the data-collection pattern in handlers.go's fillOptionLeg (the chain
// command's per-strike fill): subscribe the option, wait for the
// open-interest tick to land in the MarketData cache, then read the
// per-strike IV from GetOptionIV and the Greeks from GetOptionGreeks.
//
// Three-stage data collection:
//
//	Stage 1  — OI gate. Tick 27 (callOpenInterest) / 28
//	           (putOpenInterest), per-subscription cache.
//	Stage 2  — gateway model tick. Tick 21 (OPTION_COMPUTATION,
//	           tickType=13) routes into optIV[key] / optGreeks[key];
//	           fastest path with the gateway's own σ.
//	Stage 2b — BS-IV fallback. When the gateway never pushed a model
//	           tick (typical pre-market: the IBKR model-computation
//	           engine only fires when option order flow is active),
//	           solve for σ via Newton-Raphson against the option's
//	           prior-session close (tick 9, always pushed on subscribe
//	           regardless of trading state). The leg's gamma is then
//	           computed via bsGamma using the derived σ.
//
// Without Stage 2b, the pre-market compute aborts because zero legs
// land IV — exactly the behaviour the v0.25.0 release notes call out as
// a known limitation. With Stage 2b, the compute produces a result
// anchored on yesterday's close prices; the result envelope's
// Quality.Source disclosure makes the use-of-prior-prices honest.
//
// Per-leg budget is 1.5 s, shared by Stage 1 (OI gate) AND Stage 2
// (model-tick gate). Model ticks for actively-quoted strikes arrive
// within ~500 ms during RTH, so 1.5 s is 3× the typical-arrival
// headroom and dead deep-OTM strikes drop without holding a worker.
// Pre-market the model tick never arrives at all and the budget is
// pure wait before Stage 2b's BS-IV fallback solves from the option's
// prior close — shrinking from the prior 5 s collapses pre-market
// wall-clock from ~20 min to ~6 min on 4 workers (960 legs × 1.5 s /
// 4).
func productionLegFetcher(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying, expiryYMD string,
	strike float64,
	right string,
	snapshotSpot float64,
	snapshotAt time.Time,
) legResult {
	if c == nil {
		return legResult{Throttle: true}
	}
	key, _, err := c.SubscribeOption(ctx, underlying, expiryYMD, strike, right)
	if err != nil {
		// SubscribeOption's error path has two distinct shapes:
		//
		//   - "contract details unavailable for option …": the gateway
		//     responded definitively that no listed contract matches
		//     this (expiry, strike, right) triple. Common on
		//     multi-class chains where the secDefOptParams strike
		//     superset includes candidates that don't exist on every
		//     expiry. These aren't throttle signals.
		//
		//   - ErrContractDetailsTimeout / "timeout waiting for option
		//     contract details": the gateway didn't respond within the
		//     5 s budget. This IS the canonical throttle signal —
		//     reqContractDetails is queueing.
		msg := err.Error()
		throttle := !strings.Contains(msg, "contract details unavailable")
		return legResult{Throttle: throttle}
	}
	defer func() { _ = c.UnsubscribeMarketData(key) }()

	// Stage 1: open-interest gate. OI ticks (27 callOpenInterest,
	// 28 putOpenInterest) arrive via the standard tick-size handler
	// in handleTickSize and write to subscriptions[key].OpenInt; the
	// MarketData cache read is the right surface here because OI is
	// per-subscription, not per-OPRA-key.
	//
	// Genuine zero-OI strikes do exist (newly listed lines on far-OTM
	// rungs) and shouldn't block the leg fetch indefinitely — but they
	// contribute exactly zero to dealer GEX in either the at-spot
	// aggregate or the sweep, so the upstream caller already drops
	// them on `if !OK`. The wait predicate therefore short-circuits on
	// OI > 0 only; OI == 0 falls through to the model-tick poll and
	// reports the leg as failed if neither arrives, which is the right
	// thing — a leg with no OI and no IV is dead.
	var oi int64
	deadline := time.Now().Add(1500 * time.Millisecond)
	err = pollMarketData(ctx, c, key, deadline, func(d *ibkrlib.MarketData) bool {
		if d.OpenInt > 0 {
			oi = d.OpenInt
			return true
		}
		return false
	})
	if IsSubscriptionRejected(err) {
		// Gateway pushed a terminal error for this reqID (200 "no
		// security definition", 354 "not subscribed", 10197 "competing
		// session", …). The subscription will never produce ticks, so
		// abort the leg immediately — both for OI (already polled) and
		// for the model tick (would block another 5 s). Throttle: false
		// because this is the gateway being authoritative, not a sign
		// the fan-out is overloading the wire.
		return legResult{}
	}

	// Stage 2: model-tick gate. handleOptionComputation only commits
	// optIV[key] and optGreeks[key] once IBKR sends a non-sentinel
	// model row (see saneGreek), so the presence of either is the
	// authoritative signal that the contract has been priced.
	// pollUntilWithReject shares the leg's overall deadline AND the
	// subscription's reject channel — model ticks usually arrive within
	// the first second once OI lands, but the budget covers them both,
	// and a late-arriving terminal error still aborts fast.
	var iv, gamma float64
	_ = pollUntilWithReject(ctx, deadline, c.SubscriptionRejectCh(key), key, func() bool {
		if v, found := c.GetOptionIV(key); found && v > 0 {
			iv = v
		}
		if g, found := c.GetOptionGreeks(key); found {
			gamma = g.Gamma
		}
		return iv > 0
	})

	if oi == 0 {
		return legResult{}
	}
	if iv > 0 {
		return legResult{OI: oi, IV: iv, Gamma: gamma, OK: true}
	}
	// Stage 2b: gateway pushed OI but no model tick (typical pre-market).
	// Back-solve σ from the option's bid/ask mid or prior-session close.
	bid, ask, hasQuote := c.GetOptionQuoteBidAsk(key)
	var price float64
	if hasQuote && bid > 0 && ask > 0 {
		price = (bid + ask) / 2
	} else if px, ok := c.GetOptionPrevClose(key); ok && px > 0 {
		price = px
	}
	return bsIVFallback(snapshotSpot, snapshotAt, expiryYMD, strike, right, oi, price)
}

// bsIVFallback assembles a leg result from Black-Scholes back-solving when
// the gateway didn't deliver a model-computation tick. Inputs: pre-fetched
// OI and the best-available option price (bid/ask mid OR prior-session
// close — the caller selects). Returns an empty legResult on any refusal
// (degenerate DTE, dead price, solver bounds violation).
//
// Extracted from productionLegFetcher so the BS-solve composition can be
// tested without a live gateway — productionLegFetcher's SubscribeOption +
// poll path requires a real connection, but bsIVFallback is pure
// composition: optionPriceForBSIV is the only connector read it depends
// on, and that's done by the caller. Tests drive bsIVFallback directly
// with canned (spot, strike, dte, oi, price) tuples.
func bsIVFallback(snapshotSpot float64, snapshotAt time.Time, expiryYMD string, strike float64, right string, oi int64, price float64) legResult {
	dte := dteYears(expiryYMD, snapshotAt)
	if dte <= 0 || price <= 0 {
		return legResult{}
	}
	iv := bsImpliedVolatility(price, snapshotSpot, strike, dte, 0, 0, right == "C")
	if iv <= 0 {
		return legResult{}
	}
	gamma := bsGamma(snapshotSpot, strike, dte, iv, 0, 0)
	return legResult{OI: oi, IV: iv, Gamma: gamma, IVDerived: true, OK: true}
}

// computeGammaZeroSPX runs the full Phase 2 compute. The caller (the
// cache's background goroutine) supplies a ctx bounded only by daemon
// shutdown — not RPC deadlines — and an atomic progress counter the
// fan-out updates as it advances. Returns a populated result on
// success or a classified error on failure (stale spot, no usable
// legs, gateway disconnect).
//
// Underlying: SPY (the S&P 500 ETF), not SPX. SPY has continuous
// extended-hours quoting on SMART/ARCA, a single trading class (so the
// secDefOptParams response is a clean per-expiry strike grid rather
// than a multi-class superset that triggers spurious "no security
// definition" errors), and active dealer hedging flow that produces
// real IV ticks pre-market. SPX (the index) by contrast has no spot
// trading outside RTH, so IBKR's model-computation engine doesn't push
// IV ticks for SPX options off-hours, and the compute consistently
// failed to land a single leg. The regime signal is unchanged — SPY
// dealer gamma tracks SPX dealer gamma closely (both are dominated by
// the same dealer-positioning regime) — but the underlying number is
// SPY-scale (~SPX/10).
//
// Methodology (perfiliev-bs-sweep-v1):
//
//  1. Snapshot SPY spot. Refuse on stale (data_type != live and not
//     empty-pending) — the compute is anchored on a single spot and a
//     known-bad spot poisons everything downstream.
//
//  2. Enumerate expirations + listed strikes via FetchOptionExpiryStrikes.
//
//  3. Pick the nearest N non-0DTE-post-settlement expirations. The
//     0DTE filter is the *evening* of expiration day in NY: at 09:30
//     ET on a 3rd Friday expiry, we still include it; at 16:15 ET
//     on any expiry day, we drop it.
//
//  4. Per expiry, filter listed strikes to those within ±StrikeWidthPct
//     of spot. Far-OTM strikes contribute negligibly to dealer GEX
//     and just inflate the leg count.
//
//  5. Fan-out per-leg subscriptions at WorkerCount concurrency. Each
//     worker captures OI + IV + gateway-Γ for one (expiry, strike,
//     right). Failures (no OI, no IV, gateway dropout) are dropped
//     from the aggregate; the leg count surfaces on the result so
//     consumers can flag low-coverage runs.
//
//  6. Aggregate at spot:
//     dealer GEX = Σ sign(right) × Γ_leg × OI_leg × 100 × spot² × 0.01
//     |gex|      = Σ          |Γ_leg| × OI_leg × 100 × spot² × 0.01
//     The sign convention assumes the 2018 Perfiliev default
//     (long calls, short puts) — documented as a regime hint, not a
//     dollar-precise level.
//
//  7. Sweep spot ∈ [1−SweepRangePct, 1+SweepRangePct] × snapshot_spot
//     in sweepPoints steps. For each scenario spot, recompute Γ_leg
//     via bsGamma with the leg's captured IV and DTE (sticky-IV
//     during sweep; documented v1 limitation). Sum signed
//     contributions to build the profile.
//
//  8. Find the zero crossing on the swept profile via linear interp;
//     compute GapPct from spot.
//
//  9. Rank legs by |Γ × OI| at snapshot spot; surface the top
//     topStrikesK as the magnitude signal (sign-agnostic, robust to
//     the dealer-positioning assumption).
//
// On any step's failure the function returns a classified error;
// step-internal partial failures (e.g., 50/960 legs dropped) attach a
// structured warning instead and continue.
func computeGammaZeroSPX(
	ctx context.Context,
	c *ibkrlib.Connector,
	params rpc.GammaZeroParams,
	fetch legFetcher,
	now func() time.Time,
	progress *atomic.Int32,
) (*rpc.GammaZeroComputed, error) {
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	if fetch == nil {
		fetch = productionLegFetcher
	}
	if now == nil {
		now = time.Now
	}
	params = normalizeGammaParams(params)
	startWall := now()

	// 1. SPY spot snapshot.
	progress.Store(2)
	const sym = "SPY"
	spot, bid, ask, last, dataType := snapshotUnderlyingForGamma(ctx, c, sym, 5*time.Second)
	if spot <= 0 {
		return nil, fmt.Errorf("zero-gamma: no %s spot available (gateway returned no live tick)", sym)
	}
	if !isAcceptableDataType(dataType) {
		return nil, fmt.Errorf("zero-gamma: %s spot is %s; refusing to compute on stale data", sym, dataType)
	}
	spotAt := now()
	_ = bid
	_ = ask
	_ = last

	// 2. Expirations + strikes. The secDefOptParams response is large
	// and streams in over several seconds. 30 s mirrors the per-method
	// budget the existing handleChainExpiries handler runs with for
	// the same call (server.go's unaryDeadline for MethodChainExpiries
	// is 50 s).
	progress.Store(5)
	allStrikes, err := c.FetchOptionExpiryStrikes(sym, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("zero-gamma: fetch %s expiries: %w", sym, err)
	}
	if len(allStrikes) == 0 {
		return nil, fmt.Errorf("zero-gamma: gateway returned no %s expirations", sym)
	}

	// 3. Select expirations.
	pickedExp := selectExpirations(allStrikes, spotAt, params.ExpiryCount)
	if len(pickedExp) == 0 {
		return nil, fmt.Errorf("zero-gamma: no usable %s expirations after 0DTE filtering", sym)
	}
	progress.Store(10)

	// 4. Build the per-expiry strike grids, ordered ATM-outward.
	//
	// Iteration order matters: secDefOptParams returns a dedupe SUPERSET
	// of strikes across exchanges, so the strike list contains
	// candidates that don't exist as listed contracts on every expiry
	// (especially far-OTM strikes that exist only for select events).
	// Far-OTM legs also rarely have IV ticks flowing pre-market — the
	// model-computation engine only fires for actively-quoted strikes.
	// Processing strikes nearest-ATM first means the compute hits
	// liquid, listed strikes quickly and accumulates legs while the
	// worker pool drains the long tail of dead candidates in the
	// background. With the empirical 5 % throttle threshold, this also
	// avoids a worst-case where the first 50 attempts are all far-OTM
	// failures and the compute aborts before ever reaching ATM.
	type legSpec struct {
		expiryYMD string
		strike    float64
		right     string
	}
	var jobs []legSpec
	for _, expDate := range pickedExp {
		strikes := filterStrikesAroundSpot(allStrikes[expDate], spot, params.StrikeWidthPct)
		ordered := sortStrikesATMOutward(strikes, spot)
		expYMD := compactExpiry(expDate)
		for _, k := range ordered {
			jobs = append(jobs, legSpec{expiryYMD: expYMD, strike: k, right: "C"})
			jobs = append(jobs, legSpec{expiryYMD: expYMD, strike: k, right: "P"})
		}
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("zero-gamma: no %s strikes within ±%.0f%% of spot %.2f",
			sym, params.StrikeWidthPct*100, spot)
	}

	// Switch the connection's MarketDataType to 1 (live) for the fan-out.
	// Empirical (wire-interceptor) finding 2026-05-18: with the daemon
	// default of type=2 (frozen-aware), the IBKR gateway delivers
	// OPTION_COMPUTATION model ticks (msg 21, with IV/greeks) for OPT
	// contracts but does NOT deliver tick types 27/28 (callOpenInterest /
	// putOpenInterest). In type=1 it delivers OI but not the model ticks
	// pre-market. The gamma compute needs both. Switching to type=1 for
	// the fan-out gets us OI; the legFetcher tolerates missing
	// model-tick IV by falling back to GetOptionIV which is also
	// populated from the connector-level snapshot path.
	//
	// The defer restores type=2 even on panic/error. Type changes apply
	// only to *future* reqMktData calls on the same connection, so
	// concurrently-running regime/chain subscriptions made before this
	// point keep their original type. Subscriptions made by this fan-out
	// will all be type=1.
	if err := c.SetMarketDataType(1); err != nil {
		return nil, fmt.Errorf("zero-gamma: switch to live data type: %w", err)
	}
	defer func() {
		_ = c.SetMarketDataType(2)
	}()

	// 5. Fan-out. Mutex around shared aggregation slice; the contention
	// is bounded at one append per completed leg (cheap relative to the
	// per-leg roundtrip).
	var (
		legs           []legData
		mu             sync.Mutex
		done           atomic.Int32
		noContract     atomic.Int32
		derivedIVs     atomic.Int32
		throttledAbort atomic.Bool
		earlyAbort     atomic.Bool
		total          = int32(len(jobs))
	)

	// Early-abort watchdog. After earlyAbortAfter elapses, if zero legs
	// have landed in the aggregator, the gateway is not delivering the
	// ticks we need (entitlement gap, model-computation queue idle,
	// session-boundary feed pause, …). Aborting fast surfaces a precise
	// error rather than running the full fan-out against a feed that
	// won't produce. With the post-fix per-leg budget of 5 s and 4
	// workers, healthy runs see their first usable leg in <2 s; 30 s of
	// silence is the right threshold.
	abortTimer := time.AfterFunc(earlyAbortAfter, func() {
		mu.Lock()
		landed := len(legs)
		mu.Unlock()
		if landed == 0 {
			earlyAbort.Store(true)
		}
	})
	defer abortTimer.Stop()

	runBounded(jobs, params.WorkerCount, func(j legSpec) {
		if ctx.Err() != nil || throttledAbort.Load() || earlyAbort.Load() {
			return
		}
		r := fetch(ctx, c, sym, j.expiryYMD, j.strike, j.right, spot, spotAt)
		// Always increment the progress counter — failed legs still
		// represent work attempted. 10 % is consumed by spot+expiries
		// stages above; the fan-out scales linearly from 10 → 85.
		d := done.Add(1)
		if r.Throttle {
			nc := noContract.Add(1)
			if throttleDetected(d, nc) {
				throttledAbort.Store(true)
			}
		}
		if total > 0 {
			progress.Store(10 + int32(75*float64(d)/float64(total)))
		}
		if !r.OK {
			return
		}
		dte := dteYears(j.expiryYMD, spotAt)
		if dte <= 0 || r.IV <= 0 {
			// Belt-and-suspenders: skip legs whose DTE/IV degenerate
			// after fetch (in flight expiry rollover, or a partial OI
			// tick that snuck past the fetcher's gate).
			return
		}
		if r.IVDerived {
			derivedIVs.Add(1)
		}
		mu.Lock()
		legs = append(legs, legData{
			expiryYMD:       j.expiryYMD,
			dte:             dte,
			strike:          j.strike,
			right:           j.right,
			isCall:          j.right == "C",
			iv:              r.IV,
			oi:              r.OI,
			gammaAtSnapshot: r.Gamma,
		})
		mu.Unlock()
	})

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(legs) == 0 {
		switch {
		case earlyAbort.Load():
			// Both the model-tick path AND the BS-IV fallback failed
			// to produce a single usable leg in 30 s. With the v0.26.0
			// BS-IV fallback the pre-market case is supposed to
			// recover; if we land here it usually means the gateway
			// dropped the OI ticks too (entitlement gap, feed-farm
			// outage, or session-boundary pause).
			return nil, fmt.Errorf("zero-gamma: no option data landed in first %ds (neither model ticks nor prior-session prices for BS-IV fallback). Check gateway entitlement and farm-connection notices in the daemon log",
				int(earlyAbortAfter.Seconds()))
		case throttledAbort.Load():
			return nil, fmt.Errorf("zero-gamma: gateway throttled (%d of %d first-wave legs failed contract resolution); aborted to avoid compounding rate-limit pressure",
				noContract.Load(), done.Load())
		default:
			return nil, fmt.Errorf("zero-gamma: all %d legs failed to return OI+IV", len(jobs))
		}
	}
	progress.Store(85)

	// 6-7. Sweep + aggregate.
	//
	// Build the per-expiry skew curves first — the sweep evaluates σ at
	// each scenario spot's moneyness rather than holding the snapshot
	// IV fixed. Curves are fitted on calls AND puts pooled (put-call
	// parity makes them lie on the same surface). A curve marked !ok
	// (< 3 IV points, or degenerate solve) falls back to sticky-IV for
	// that expiry alone — surfaced as "skew_fallback:YYYYMMDD" warning.
	skewByExpiry, skewFitQuality, skewFallbacks := buildSkewCurves(legs, spot)
	// Partition legs into near (DTE ≤ 7) and term (DTE > 7) buckets.
	// The combined sweep stays the headline; the two bucket sweeps run
	// alongside so the regime row can surface horizon agreement.
	var nearLegs, termLegs []legData
	for _, l := range legs {
		if l.dte <= nearDTECutoffYears {
			nearLegs = append(nearLegs, l)
		} else {
			termLegs = append(termLegs, l)
		}
	}

	profile := sweepProfile(legs, spot, params.SweepRangePct, skewByExpiry)
	profileNear := sweepProfile(nearLegs, spot, params.SweepRangePct, skewByExpiry)
	profileTerm := sweepProfile(termLegs, spot, params.SweepRangePct, skewByExpiry)
	// At-spot aggregates: re-use the profile's snapshot bucket OR
	// compute directly. We compute directly so the headline numbers
	// don't depend on the sweep grid alignment.
	gammaTotalAbs := 0.0
	for _, l := range legs {
		gammaTotalAbs += absGEX(l.gammaAtSnapshot, float64(l.oi), 100, spot)
	}
	progress.Store(90)

	// 8. Zero crossings: combined + near + term.
	zg, gammaSign := findZeroCrossing(profile)
	var gapPct *float64
	if zg != nil {
		v := (spot - *zg) / *zg * 100
		gapPct = &v
	}
	zgNear, signNear := findZeroCrossing(profileNear)
	zgTerm, signTerm := findZeroCrossing(profileTerm)

	// 9. Top strikes by magnitude.
	topStrikes := rankTopStrikesByAbsGEX(legs, spot, topStrikesK)

	// Coverage gate. A compute whose successful-leg fraction falls
	// below MinLegCoverageFraction is surfaced as an error so the
	// existing gammaErrorRetryTTL retry machinery applies — without
	// this, a single throttled fan-out at NY-session start poisons
	// the cache for the rest of the day (same shape as the v0.27.0
	// breadth poison-cache bug, mitigated for breadth at v0.27.3).
	if err := checkLegCoverage(len(legs), len(jobs), throttledAbort.Load()); err != nil {
		return nil, err
	}

	// Warnings. Ordered "throttled" first because it explains why
	// coverage is low, not the other way around.
	var warnings []string
	if throttledAbort.Load() {
		warnings = append(warnings, "throttled")
	}
	if zg == nil {
		warnings = append(warnings, "no_crossing_in_window")
	}
	if len(nearLegs) == 0 {
		warnings = append(warnings, "near_no_legs")
	}
	if len(termLegs) == 0 {
		warnings = append(warnings, "term_no_legs")
	}
	// Surface per-expiry skew-fit fallbacks so a renderer can show
	// "skew curve fell back to sticky-IV for 20260620" rather than
	// silently using the legacy recipe for that expiry. Each fallback
	// expiry contributes one warning of the form "skew_fallback:YYYYMMDD".
	for _, expYMD := range skewFallbacks {
		warnings = append(warnings, "skew_fallback:"+expYMD)
	}

	derivedCount := int(derivedIVs.Load())
	if derivedCount > 0 && derivedCount == len(legs) {
		// All legs used the BS-IV fallback — useful signal for the
		// renderer, since the resulting flip level reflects prior-
		// session prices rather than live model ticks.
		warnings = append(warnings, "all_iv_derived")
	}

	// Empty-bucket sign normalisation. findZeroCrossing returns
	// "no_data" on empty/single-point profiles; surface that as
	// the explicit "no_data" sign on the row so the consumer can
	// distinguish "no crossing but the sweep had legs" (positive /
	// negative) from "no legs to sweep" (no_data).
	if len(nearLegs) == 0 {
		signNear = "no_data"
	}
	if len(termLegs) == 0 {
		signTerm = "no_data"
	}

	skewModel := ""
	if len(skewFitQuality) > 0 {
		skewModel = "sticky-moneyness-v1"
	}

	res := &rpc.GammaZeroComputed{
		SpotUnderlying: spot,
		SpotAt:         spotAt,
		ZeroGamma:      zg,
		GapPct:         gapPct,
		GammaSign:      gammaSign,
		Profile:        profile,
		ZeroGammaNear:  zgNear,
		ProfileNear:    profileNear,
		GammaSignNear:  signNear,
		NearLegCount:   len(nearLegs),
		ZeroGammaTerm:  zgTerm,
		ProfileTerm:    profileTerm,
		GammaSignTerm:  signTerm,
		TermLegCount:   len(termLegs),
		SkewModel:      skewModel,
		SkewFitQuality: skewFitQuality,
		GammaTotalAbs:  gammaTotalAbs,
		TopStrikes:     topStrikes,
		Expirations:    pickedExp,
		LegCount:       len(legs),
		DerivedIVLegs:  derivedCount,
		Warnings:       warnings,
		Params:         params,
		Source:         "computed from IBKR SPY option chain",
		Method:         "perfiliev-bs-sweep-v2-stickymoneyness",
		AsOf:           now(),
		DurationMS:     now().Sub(startWall).Milliseconds(),
	}
	progress.Store(100)
	return res, nil
}

// buildSkewCurves groups legs by expiry, fits a quadratic
// log-moneyness curve per expiry, and returns the three things the
// caller needs:
//
//   - skewByExpiry: per-expiryYMD curve, including unfit ones (the
//     sweep checks curve.ok before using a curve)
//   - skewFitQuality: per-expiryYMD diagnostics (points, R², fitted
//     range) for the result envelope — only fitted expiries appear here
//   - skewFallbacks: list of expiryYMDs that failed to fit and fell back
//     to sticky-IV; surfaced as warnings on the result
func buildSkewCurves(legs []legData, snapshotSpot float64) (map[string]SkewCurve, map[string]rpc.SkewFitInfo, []string) {
	byExpiry := map[string][]legData{}
	for _, l := range legs {
		byExpiry[l.expiryYMD] = append(byExpiry[l.expiryYMD], l)
	}
	curves := make(map[string]SkewCurve, len(byExpiry))
	quality := make(map[string]rpc.SkewFitInfo, len(byExpiry))
	var fallbacks []string
	// Stable iteration order so the warnings list is deterministic for
	// regression tests.
	expiryOrder := make([]string, 0, len(byExpiry))
	for k := range byExpiry {
		expiryOrder = append(expiryOrder, k)
	}
	sort.Strings(expiryOrder)
	for _, expYMD := range expiryOrder {
		expLegs := byExpiry[expYMD]
		curve := fitSkewCurve(expLegs, snapshotSpot)
		curves[expYMD] = curve
		if !curve.ok {
			fallbacks = append(fallbacks, expYMD)
			continue
		}
		quality[expYMD] = rpc.SkewFitInfo{
			Points:   curve.nPoints,
			RSquared: skewFitRSquared(curve, expLegs, snapshotSpot),
			Range:    [2]float64{curve.mLo, curve.mHi},
		}
	}
	return curves, quality, fallbacks
}

// normalizeGammaParams fills in defaults for unset fields. Mirrors the
// pattern handleHistoryDaily / handleChainFetch use for their own param
// defaults — keeps the wire-shape contract liberal.
func normalizeGammaParams(p rpc.GammaZeroParams) rpc.GammaZeroParams {
	if p.ExpiryCount <= 0 {
		p.ExpiryCount = defaultExpiryCount
	}
	if p.StrikeWidthPct <= 0 {
		p.StrikeWidthPct = defaultStrikeWidthPct
	}
	if p.SweepRangePct <= 0 {
		p.SweepRangePct = defaultSweepRangePct
	}
	if p.WorkerCount <= 0 {
		p.WorkerCount = defaultWorkerCount
	}
	return p
}

// snapshotUnderlyingForGamma wraps briefSnapshotFull with the gamma
// compute's spot-resolution policy. Returns (spot, bid, ask, last,
// dataType) — spot picks last → mid → bid → ask → mark → close,
// matching the briefSnapshotPrice convention. Mark (tick 37) covers
// most off-hours frozen states; close (tick 9) is the last-resort
// anchor for the rare case where the gateway hasn't even pushed a
// mark yet (cold post-restart). Without these the compute could not
// anchor a spot. Caller passes the underlying symbol (currently SPY).
func snapshotUnderlyingForGamma(ctx context.Context, c *ibkrlib.Connector, sym string, timeout time.Duration) (spot, bid, ask, last float64, dataType string) {
	var mark, closePx float64
	bid, ask, last, mark, closePx, dataType = briefSnapshotFull(ctx, c, sym, timeout)
	switch {
	case last > 0:
		spot = last
	case bid > 0 && ask > 0:
		spot = (bid + ask) / 2
	case bid > 0:
		spot = bid
	case ask > 0:
		spot = ask
	case mark > 0:
		spot = mark
	case closePx > 0:
		spot = closePx
	}
	if dataType == "" && spot > 0 {
		dataType = "live"
	}
	return spot, bid, ask, last, dataType
}

// throttleDetected reports whether the fan-out's observed
// contract-resolve failure ratio is high enough to abort. Pure helper
// so the threshold and sample-size policy can be unit-tested without
// driving the full compute pipeline.
//
// Returns false until we have at least throttleSampleSize completions
// — the ratio is meaningless on tiny samples and would cause spurious
// aborts on routine startup variance.
func throttleDetected(done, noContract int32) bool {
	if done < throttleSampleSize {
		return false
	}
	return float64(noContract)/float64(done) > throttleAbortPct
}

// isAcceptableDataType reports whether the gateway's per-reqID feed
// state is acceptable for the zero-gamma compute.
//
// Accepted:
//   - "live" — real-time ticks; obvious choice.
//   - "frozen" — IBKR's term for the last live tick captured before
//     a session boundary or feed pause. For SPX this is typically
//     yesterday's regular-session close. The spec explicitly says
//     a daily refresh is sufficient, and frozen is exactly that:
//     the official anchor for an end-of-day-style compute, just
//     labelled honestly. Renderers can dim the headline by reading
//     `data_type=frozen` from the result envelope.
//   - "" — no marketDataType notice has arrived yet (typical in the
//     first few hundred ms of a fresh subscription). Treated as
//     live per rpc.IsLiveDataType convention.
//
// Rejected:
//   - "delayed" / "delayed-frozen" — typically 15-minute-old data
//     because the account isn't entitled to live for the symbol.
//     A 15-min staleness biases every BS gamma in the sweep against
//     the spot snapshot, and we can't compensate for the lag
//     post-hoc. The renderer should surface this as a configuration
//     issue rather than an unreliable headline.
//   - Anything else (unexpected value) — stale-by-default.
func isAcceptableDataType(dt string) bool {
	switch dt {
	case "", "live", "frozen":
		return true
	default:
		return false
	}
}

// selectExpirations picks the nearest N expirations that are NOT
// already past their settlement window in NY time. Same-day expiries
// before settlement count; same-day expiries after settlement (16:15
// NY for SPXW, conservatively applied to all classes since we don't
// differentiate by tradingClass here) are dropped.
//
// Input map keys are YYYY-MM-DD; output is the picked subset in the
// same date format, ascending.
func selectExpirations(strikes map[string][]float64, now time.Time, count int) []string {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	nyNow := now.In(loc)
	today := nyNow.Format("2006-01-02")
	// 16:15 ET is the conservative "all SPX classes settled" cutoff:
	// SPX (AM-settled) settles at 09:30 on expiry; SPXW (PM) settles
	// at 16:00; tagging the same-day expiry as "past" only after
	// 16:15 covers both without needing tradingClass at this layer.
	settlementCutoff := time.Date(nyNow.Year(), nyNow.Month(), nyNow.Day(), 16, 15, 0, 0, loc)
	pastCutoff := nyNow.After(settlementCutoff)

	var candidates []string
	for date := range strikes {
		if date < today {
			continue // expired any time before today
		}
		if date == today && pastCutoff {
			continue // 0DTE post-settlement
		}
		candidates = append(candidates, date)
	}
	sort.Strings(candidates)
	if len(candidates) > count {
		candidates = candidates[:count]
	}
	return candidates
}

// sortStrikesATMOutward returns the input strike list reordered by
// absolute distance from spot, nearest first. Used by the gamma
// compute's leg-job builder so the worker pool hits liquid near-ATM
// strikes before the long tail of far-OTM candidates, most of which
// don't have IV ticks flowing pre-market and (for SPY/SPX) include
// chain-dedupe ghost strikes that aren't actually listed on every
// expiry. Stable for ties (strikes equidistant from spot keep their
// pre-sort order); strikes are float64 so exact ties on $0.50/$1
// grids are vanishingly rare in practice.
func sortStrikesATMOutward(strikes []float64, spot float64) []float64 {
	if len(strikes) <= 1 {
		return strikes
	}
	out := make([]float64, len(strikes))
	copy(out, strikes)
	sort.SliceStable(out, func(i, j int) bool {
		return math.Abs(out[i]-spot) < math.Abs(out[j]-spot)
	})
	return out
}

// filterStrikesAroundSpot returns the subset of listed strikes within
// ±widthPct of spot, sorted ascending. The input slice may not be
// sorted — sort defensively because FetchOptionExpiryStrikes only
// dedupes, it doesn't promise order.
func filterStrikesAroundSpot(strikes []float64, spot, widthPct float64) []float64 {
	if spot <= 0 || widthPct <= 0 || len(strikes) == 0 {
		return nil
	}
	lo := spot * (1 - widthPct)
	hi := spot * (1 + widthPct)
	var out []float64
	for _, k := range strikes {
		if k >= lo && k <= hi {
			out = append(out, k)
		}
	}
	sort.Float64s(out)
	return out
}

// compactExpiry converts YYYY-MM-DD to YYYYMMDD — the format
// SubscribeOption (via resolveOptionContract) and the rest of the
// option-subscription path expect.
func compactExpiry(date string) string {
	if len(date) == 10 && date[4] == '-' && date[7] == '-' {
		return date[:4] + date[5:7] + date[8:10]
	}
	return date // best-effort
}

// dteYears computes years-to-expiry from an option's YYYYMMDD expiry
// string. Uses 4 PM ET on expiration day as the conservative
// settlement reference (matches selectExpirations' cutoff). Zero on
// parse failure or non-positive deltas — the compute's per-leg gate
// filters those out.
func dteYears(expiryYMD string, now time.Time) float64 {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	day, err := time.ParseInLocation("20060102", expiryYMD, loc)
	if err != nil {
		return 0
	}
	expWall := time.Date(day.Year(), day.Month(), day.Day(), 16, 0, 0, 0, loc)
	delta := expWall.Sub(now.In(loc))
	if delta <= 0 {
		return 0
	}
	return delta.Hours() / (24 * 365.0)
}

// sweepProfile builds the (spot, signed_gex) sweep over [1−range,
// 1+range] × snapshotSpot in sweepPoints steps. Each scenario spot
// recomputes per-leg Γ via Black-Scholes.
//
// skewByExpiry maps each leg's expiryYMD to a fitted skew curve. For
// each leg in the inner loop the IV is looked up at the
// scenario-spot's moneyness (σ = curve.IVAtMoneyness(ln(K/S_scenario))),
// implementing the sticky-moneyness convention. When the curve for an
// expiry is unfit (fewer than 3 points or degenerate solve), the leg
// falls back to its captured IV — the v1 sticky-IV behaviour for that
// expiry only. Pass nil to disable skew lookups entirely (used by the
// fallback test path).
func sweepProfile(legs []legData, snapshotSpot, sweepRangePct float64, skewByExpiry map[string]SkewCurve) []rpc.GammaProfilePoint {
	if snapshotSpot <= 0 || sweepRangePct <= 0 || sweepPoints < 2 {
		return nil
	}
	loSpot := snapshotSpot * (1 - sweepRangePct)
	hiSpot := snapshotSpot * (1 + sweepRangePct)
	step := (hiSpot - loSpot) / float64(sweepPoints-1)

	out := make([]rpc.GammaProfilePoint, sweepPoints)
	for i := range sweepPoints {
		scenarioSpot := loSpot + float64(i)*step
		gex := 0.0
		for _, l := range legs {
			σ := l.iv
			if skewByExpiry != nil {
				curve, ok := skewByExpiry[l.expiryYMD]
				if ok && curve.ok {
					m := math.Log(l.strike / scenarioSpot)
					if v := curve.IVAtMoneyness(m); v > 0 {
						σ = v
					}
				}
			}
			γ := bsGamma(scenarioSpot, l.strike, l.dte, σ, 0, 0)
			gex += dealerGEX(γ, float64(l.oi), 100, scenarioSpot, l.isCall)
		}
		out[i] = rpc.GammaProfilePoint{Spot: scenarioSpot, GEX: gex}
	}
	return out
}

// rankTopStrikesByAbsGEX returns the top-k legs ranked by sign-agnostic
// dollar gamma at the snapshot spot. Used by the renderer as the
// methodology-robust positioning view (independent of the Perfiliev
// sign assumption). The slice is sorted by AbsGEX descending.
func rankTopStrikesByAbsGEX(legs []legData, spot float64, k int) []rpc.StrikeConcentration {
	if k <= 0 || len(legs) == 0 {
		return nil
	}
	type ranked struct {
		row    rpc.StrikeConcentration
		absGEX float64
	}
	rows := make([]ranked, 0, len(legs))
	for _, l := range legs {
		v := absGEX(l.gammaAtSnapshot, float64(l.oi), 100, spot)
		if v == 0 {
			// Skip legs where the gateway didn't deliver a gamma tick;
			// the BS-recomputed gamma in the sweep doesn't help here
			// because the concentration view is anchored on snapshot spot.
			continue
		}
		rows = append(rows, ranked{
			row: rpc.StrikeConcentration{
				Strike: l.strike,
				Expiry: l.expiryYMD[:4] + "-" + l.expiryYMD[4:6] + "-" + l.expiryYMD[6:8],
				Right:  l.right,
				AbsGEX: v,
				OI:     l.oi,
			},
			absGEX: v,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].absGEX > rows[j].absGEX })
	if len(rows) > k {
		rows = rows[:k]
	}
	out := make([]rpc.StrikeConcentration, len(rows))
	for i, r := range rows {
		out[i] = r.row
	}
	return out
}
