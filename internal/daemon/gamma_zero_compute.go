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
// default in nominal coverage; ±10 % strike width defines the candidate
// window and the nearest-80-strikes cap keeps the leg count reasonable;
// ±15 % sweep range comfortably brackets the typical zero crossing
// without inflating the profile point count.
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

	// maxGammaStrikesPerExpiry caps the listed strikes walked for each
	// expiry after the ±StrikeWidthPct filter and ATM-outward ordering.
	// This keeps the default fan-out at 6 × 80 × 2 = 960 option legs,
	// matching the compute budget below. It is especially important for
	// SPX/SPXW, where 5-point strike grids inside ±10% can otherwise
	// expand to 3k+ subscriptions outside RTH with little extra signal.
	maxGammaStrikesPerExpiry = 80

	// Horizon bucket boundaries in fractional years.
	//
	// v3 split (M4): three buckets instead of v2's two —
	//
	//   0DTE      DTE == 0 in trading-class settlement years  ≈ 0
	//   1-7 DTE   0 < DTE ≤ 7 days
	//   term      DTE > 7 days
	//
	// Cboe 2025 data shows 0DTE = ~59% of SPX volume, so isolating it
	// from the 1-7 weekly flow is the regime-meaningful split — lumping
	// them together (v2's "near" bucket) muddied the signal.
	//
	// Hardcoded rather than parameterised on GammaZeroParams — these
	// are regime-meaningful boundaries, not tunable knobs.
	zeroDTECutoffYears = 1.0 / 365.0 // strictly-less-than: anything inside one calendar day is "0DTE"
	nearDTECutoffYears = 7.0 / 365.0 // upper bound on the 1-7 bucket; >7 falls in term

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

	// optionOpenInterestGrace is the short post-IV wait before a gamma
	// worker unsubscribes an option leg. IV/model ticks can arrive before
	// the one-shot OI tick (27/28); reading OI immediately after IV lands
	// turns a healthy subscription into a zero-OI leg. Keep this non-
	// gating and short so missing OI remains a data-quality warning, not
	// a new fan-out bottleneck.
	optionOpenInterestGrace = 250 * time.Millisecond

	// gammaMethodToken is the stable wire token consumers (renderers,
	// cache method-mismatch gate) compare against to confirm the
	// methodology. v3 carries two breaking changes from v2: horizon
	// split is now 0DTE / 1-7 / >7 (was ≤7 / >7), and the per-leg
	// snapshot gamma is BS-recomputed from captured IV rather than
	// read from the gateway's optional Greeks tick (fixes a v2 race
	// where IV-but-no-Greeks legs contributed 0 to GammaTotalAbs).
	gammaMethodToken = "bs-gamma-profile-v3-stickymoneyness-0dte-split"

	// MinLegCoverageFraction is the persist-or-not threshold: a
	// compute whose successful-leg fraction falls below this is
	// surfaced as an error (not a warning-flagged result), so the
	// existing gammaErrorRetryTTL machinery in gamma_zero_cache
	// re-attempts on the next call within the same NY trading
	// session. Mirrors breadth's MinCoverageFraction = 0.80 pattern
	// at internal/breadth/spx/types.go: "did not converge" runs are
	// not stored as session truth.
	//
	// Why 0.2 (vs breadth's 0.8): the OI-weighted gamma compute
	// concentrates near ATM, so missing far-OTM legs has small
	// impact on the zero-gamma estimate. ATM strikes are the most
	// liquid and resolve first; 20% coverage typically captures the
	// ATM ±5% band that dominates the gamma profile. Below 20% the
	// signal is too thin to band reliably.
	//
	// Lowered from 0.5 (v0.28.x) to 0.2 (v0.29.0): empirically the
	// IBKR gateway's OPT model-tick delivery is bursty during RTH —
	// landing 20-40% of legs within the per-leg budget is typical,
	// not a degraded run. The previous 0.5 threshold was discarding
	// usable results and forcing a 60s retry cooldown that left the
	// dashboard "computing" for 5-10 minutes. 0.2 is more honest
	// about what the gateway will deliver while still gating on
	// enough signal to compute a meaningful γ-zero.
	MinLegCoverageFraction = 0.2
)

// gammaMethodologyCitations is the short bibliography the compute
// stamps on every result envelope. Surfaced via
// GammaZeroComputed.MethodologyCitations so renderers can show the
// citations alongside the headline numbers without consulting
// out-of-band documentation. Order is significant: most relevant to
// the headline first.
var gammaMethodologyCitations = []string{
	"Perfiliev (2022) — BS-sweep baseline",
	"Derman / Daglish-Hull-Suo — sticky-moneyness skew dynamics",
	"SqueezeMetrics (2017) — naive-sign GEX, deprecated 2022+",
	"Cboe 2025 — 0DTE = ~59% of SPX volume",
}

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
//
// tradingClass disambiguates SPX-class AM-monthlies from SPXW-class
// PM-weeklies on shared third-Friday dates. For single-class
// underlyings (SPY) the field equals the symbol. The settlement
// instant in dteYears branches on it.
type legData struct {
	expiryYMD    string
	dte          float64 // years; positive
	strike       float64
	right        string // "C" | "P"
	tradingClass string // "SPY" | "SPX" | "SPXW" | …
	isCall       bool
	iv           float64
	oi           int64
	oiObserved   bool
	// gamma is the gateway-supplied model-computation gamma at the
	// snapshot spot; used for the at-spot aggregate. The sweep
	// recomputes gamma via Black-Scholes for each scenario spot.
	gammaAtSnapshot float64
}

// legResult is the per-leg payload returned by a legFetcher. Bundled as
// a struct because adding the BS-IV-derived flag pushed the original
// 5-tuple into "what do these positional booleans mean" territory.
//
// OK reports whether IV landed (from the gateway's model tick or the
// BS-IV fallback) within budget — the aggregator only counts a leg when
// OK is true. OI is opportunistic (may be 0 for inactive strikes off-
// hours) and does not gate OK; a leg with γ but no OI contributes 0 to
// dealer GEX and still enriches the IV surface for skew fitting.
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
	OI         int64
	OIObserved bool
	IV         float64
	Gamma      float64
	IVDerived  bool
	OK         bool
	Throttle   bool
}

// gammaLogger is the minimal logging surface computeGammaZeroFor uses to
// emit kickoff / progress / abort lines. Defined as an interface so tests
// can drive the compute with a no-op recorder; production passes the
// daemon's *Logger. Nil is accepted and treated as no-op.
type gammaLogger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// gammaLogfWrap returns a struct that turns nil-safe Infof/Warnf into
// no-ops. Lets every log call site stay free of nil checks without
// repeating boilerplate.
type gammaLogf struct{ inner gammaLogger }

func (g gammaLogf) Infof(format string, args ...any) {
	if g.inner != nil {
		g.inner.Infof(format, args...)
	}
}
func (g gammaLogf) Warnf(format string, args ...any) {
	if g.inner != nil {
		g.inner.Warnf(format, args...)
	}
}

// legFetcher abstracts the per-leg subscribe-collect-unsubscribe so
// tests can drive computeGammaZeroFor with a fake. The fetcher is
// expected to block for at most the budget the caller passes via ctx.
//
// snapshotSpot + snapshotAt are passed so the fetcher can compute a BS
// back-solve when the gateway doesn't deliver a model tick (the typical
// pre-market state). Both are captured by computeGammaZeroFor before
// the fan-out begins; the fetcher does NOT take its own spot snapshot.
//
// tradingClass is the option's listed class — load-bearing for SPX
// because the SPX-class AM-settled and SPXW-class PM-settled contracts
// share (expiry, strike, right) on third-Fridays. The fetcher passes it
// through to SubscribeOption so the cache lookup hits the right
// pre-warmed entry. For SPY-class single-class underlyings the field is
// just the symbol; empty falls back to underlying.
type legFetcher func(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying, tradingClass, expiryYMD string,
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
// Two-stage data collection:
//
//	Stage 1  — gateway model tick. Tick 21 (OPTION_COMPUTATION,
//	           tickType=13) routes into optIV[key] / optGreeks[key];
//	           fastest path with the gateway's own σ. Verified to fire
//	           off-hours under the daemon's default MarketDataType=2 —
//	           same path `ibkr chain SPY` relies on for ATM IV.
//	Stage 2  — BS-IV fallback. When the gateway never pushed a model
//	           tick, solve for σ via Newton-Raphson against the option's
//	           bid/ask mid or prior-session close (tick 9, always pushed
//	           on subscribe regardless of trading state). Gamma is then
//	           computed via bsGamma using the derived σ.
//
// Open interest (ticks 27/28) is read opportunistically from the per-
// subscription cache at the end — never as a gate. Missing OI is
// unknown, not zero: the leg can enrich IV/skew fitting, but it is
// omitted from OI-weighted dealer GEX until an OI tick is observed.
// SPY OI may be absent outside regular option hours; SPX OI should be
// stable across session phases, so missing SPX OI is a data-quality
// finding rather than expected off-hours sparsity.
//
// Per-leg budget is 1.5 s for the model-tick poll. Active strikes
// produce a model tick within ~500 ms; dead deep-OTM strikes time out
// and fall through to Stage 2 which back-solves σ from cached prices.
func productionLegFetcher(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying, tradingClass, expiryYMD string,
	strike float64,
	right string,
	snapshotSpot float64,
	snapshotAt time.Time,
) legResult {
	if c == nil {
		return legResult{Throttle: true}
	}
	key, _, err := c.SubscribeOption(ctx, underlying, tradingClass, expiryYMD, strike, right)
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

	// Stage 1: model-tick poll. handleOptionComputation commits
	// optIV[key] / optGreeks[key] once IBKR sends a non-sentinel model
	// row (see saneGreek); their presence is the authoritative signal
	// that the gateway priced the contract. Empirically (see `ibkr chain
	// SPY` working off-hours via the same handleOptionComputation path),
	// model ticks DO arrive for OPT contracts under the daemon's default
	// MarketDataType=2 — the prior v0.28 release switched the connection
	// to type=1 to chase OI ticks but suppressed model ticks system-wide
	// for the duration of the fan-out, regressing landing rate to ~1%.
	//
	// OI (ticks 27/28) is intentionally NOT gated. A leg with IV but no
	// observed OI still enriches the IV surface for skew fitting, but
	// its dealer-GEX contribution is omitted because OI is unknown, not
	// zero. SPY OI can be sparse outside regular option hours; missing
	// SPX OI remains a data-quality finding.
	deadline := time.Now().Add(1500 * time.Millisecond)
	var iv, gamma float64
	err = pollUntilWithReject(ctx, deadline, c.SubscriptionRejectCh(key), key, func() bool {
		if v, found := c.GetOptionIV(key); found && v > 0 {
			iv = v
		}
		if g, found := c.GetOptionGreeks(key); found {
			gamma = g.Gamma
		}
		return iv > 0
	})
	if IsSubscriptionRejected(err) {
		// Gateway pushed a terminal error for this reqID (200 "no
		// security definition", 354 "not subscribed", 10197 "competing
		// session", …). The subscription will never produce ticks.
		// Throttle: false — gateway is being authoritative, not a sign
		// the fan-out is overloading the wire.
		return legResult{}
	}

	if iv > 0 {
		// Opportunistic OI read. May be 0 for strikes the gateway never
		// pushed a 27/28 tick for — that's fine, the leg still lands.
		//
		// Do not read only once: IV/model ticks often arrive before the
		// one-shot OI tick. A short grace materially improves OI capture
		// without making OI a hard gate.
		oi, oiObserved := waitForOptionOpenInterest(ctx, time.Now().Add(optionOpenInterestGrace), func() (int64, bool) {
			return optionOpenInterest(c, key)
		})
		return legResult{OI: oi, OIObserved: oiObserved, IV: iv, Gamma: gamma, OK: true}
	}
	// Stage 2: BS-IV fallback when model tick never arrived.
	// Back-solve σ from the option's bid/ask mid or prior-session close.
	oi, oiObserved := optionOpenInterest(c, key)
	bid, ask, hasQuote := c.GetOptionQuoteBidAsk(key)
	var price float64
	if hasQuote && bid > 0 && ask > 0 {
		price = (bid + ask) / 2
	} else if px, ok := c.GetOptionPrevClose(key); ok && px > 0 {
		price = px
	}
	return bsIVFallback(snapshotSpot, snapshotAt, expiryYMD, tradingClass, strike, right, oi, oiObserved, price)
}

func optionOpenInterest(c *ibkrlib.Connector, key string) (int64, bool) {
	if c == nil || key == "" {
		return 0, false
	}
	if d, ok := c.GetMarketData()[key]; ok {
		return d.OpenInt, d.OpenIntObserved
	}
	return 0, false
}

func waitForOptionOpenInterest(ctx context.Context, deadline time.Time, read func() (int64, bool)) (int64, bool) {
	if read == nil {
		return 0, false
	}
	var oi int64
	var observed bool
	_ = pollUntil(ctx, deadline, func() bool {
		oi, observed = read()
		return observed
	})
	return oi, observed
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
//
// tradingClass plumbs through so the BS-IV fallback uses the same
// settlement instant as the live-tick path — an SPX-class AM-monthly
// solving from yesterday's close at 14:00 ET on expiry day must use
// the 09:30 ET instant (already past) and yield dte=0, not over-state
// gamma against a 16:00 instant that doesn't apply to this class.
func bsIVFallback(snapshotSpot float64, snapshotAt time.Time, expiryYMD, tradingClass string, strike float64, right string, oi int64, oiObserved bool, price float64) legResult {
	dte := dteYears(expiryYMD, tradingClass, snapshotAt)
	if dte <= 0 || price <= 0 {
		return legResult{}
	}
	iv := bsImpliedVolatility(price, snapshotSpot, strike, dte, 0, 0, right == "C")
	if iv <= 0 {
		return legResult{}
	}
	gamma := bsGamma(snapshotSpot, strike, dte, iv, 0, 0)
	return legResult{OI: oi, OIObserved: oiObserved, IV: iv, Gamma: gamma, IVDerived: true, OK: true}
}

// computeGammaZeroFor runs the full Phase 2 compute for one underlying.
// The caller (the cache's background goroutine) supplies a ctx bounded
// only by daemon shutdown — not RPC deadlines — and an atomic progress
// counter the fan-out updates as it advances. Returns a populated
// result on success or a classified error on failure (stale spot, no
// usable legs, gateway disconnect).
//
// `underlying` is the symbol whose option chain drives the compute —
// "SPY" or "SPX" today. The function is structurally
// single-underlying: callers that want SPY+SPX run it once per
// underlying and aggregate at a higher layer.
//
// Underlying choice notes (carried forward from the SPY-only era):
// SPY has continuous extended-hours quoting on SMART/ARCA, a single
// trading class (so the secDefOptParams response is a clean per-expiry
// strike grid rather than a multi-class superset that triggers spurious
// "no security definition" errors), and active dealer hedging flow
// that produces real IV ticks pre-market. SPX (the index) by contrast
// has no spot trading outside RTH, so IBKR's model-computation engine
// doesn't push IV ticks for SPX options off-hours, and an SPX-only
// off-hours compute will land few legs. The BS-IV fallback and a
// permissive MinLegCoverageFractionSPX (~0.05) are the off-hours
// posture.
//
// Methodology (bs-gamma-profile-v3-stickymoneyness-0dte-split):
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
//     of spot, then cap to the nearest strikes by moneyness. Far-OTM
//     strikes contribute negligibly to dealer GEX and just inflate the
//     leg count / gateway pressure.
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
func computeGammaZeroFor(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying string,
	params rpc.GammaZeroParams,
	fetch legFetcher,
	now func() time.Time,
	progress *atomic.Int32,
	logger gammaLogger,
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
	sym := strings.TrimSpace(strings.ToUpper(underlying))
	if sym == "" {
		return nil, fmt.Errorf("zero-gamma: empty underlying symbol")
	}
	log := gammaLogf{inner: logger}
	params = normalizeGammaParams(params)
	startWall := now()
	log.Infof("gamma.kickoff underlying=%s workers=%d expiry_count=%d strike_width_pct=%.2f sweep_range_pct=%.2f",
		sym, params.WorkerCount, params.ExpiryCount, params.StrikeWidthPct, params.SweepRangePct)

	// 1. Underlying spot snapshot.
	progress.Store(2)
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
	//
	// Branch on underlying: SPX is multi-class (SPX-AM monthlies +
	// SPXW-PM weeklies share third-Friday dates as distinct contracts),
	// so it pulls the classed strike grid and applies a per-class
	// settlement cutoff. SPY-style single-class underlyings keep the
	// existing merged-across-classes path; tradingClass on each leg is
	// just the symbol.
	progress.Store(5)
	picked, err := buildPickedExpirations(c, sym, spotAt, params.ExpiryCount)
	if err != nil {
		return nil, fmt.Errorf("zero-gamma: fetch %s expiries: %w", sym, err)
	}
	if len(picked) == 0 {
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
		expiryYMD    string
		strike       float64
		right        string
		tradingClass string // SPY for single-class; "SPX" / "SPXW" on the SPX classed path
	}
	var jobs []legSpec
	strikeBudgetCapped := false
	for _, p := range picked {
		strikes := filterStrikesAroundSpot(p.strikes, spot, params.StrikeWidthPct)
		ordered := sortStrikesATMOutward(strikes, spot)
		if capped, ok := capStrikesATMOutward(ordered, maxGammaStrikesPerExpiry); ok {
			ordered = capped
			strikeBudgetCapped = true
		}
		for _, k := range ordered {
			jobs = append(jobs, legSpec{expiryYMD: p.expiryYMD, strike: k, right: "C", tradingClass: p.tradingClass})
			jobs = append(jobs, legSpec{expiryYMD: p.expiryYMD, strike: k, right: "P", tradingClass: p.tradingClass})
		}
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("zero-gamma: no %s strikes within ±%.0f%% of spot %.2f",
			sym, params.StrikeWidthPct*100, spot)
	}
	log.Infof("gamma.jobs total=%d picked=%d spot=%.2f", len(jobs), len(picked), spot)

	// Bulk-prewarm option contracts before the worker fan-out. This is the
	// load-bearing optimization: without it, each of the ~1600 legs would
	// independently pay a reqContractDetails round-trip with up-to-4-exchange
	// retry loop, which the IBKR per-account throttle caps at ~50 attempts
	// before aborting the whole fan-out. The bulk prewarm issues one
	// partial-Contract reqContractDetails per expiration (no Strike, no
	// Right) and the gateway streams every listed strike × C/P back in one
	// burst — same primitive TWS uses internally to populate a chain
	// instantly. Round-trip count drops from ~1600 to len(picked) (~6).
	//
	// TradingClass is load-bearing: omitting it interleaves multi-class
	// listings (SPY+SPYW, SPX+SPXW) and cache entries shadow each other.
	// SPY+weeklies all share class "SPY"; SPX has two distinct classes
	// (SPX-AM + SPXW-PM) which require independent prewarm passes.
	//
	// Errors per expiry are localised — one timed-out expiry doesn't fail
	// the others, and the per-leg fetcher still has its own
	// resolveOptionContract fallback for cache misses. The prewarm is a
	// fast path, not a hard dependency.
	expsByClass := map[string][]string{}
	for _, p := range picked {
		expsByClass[p.tradingClass] = append(expsByClass[p.tradingClass], p.expiryYMD)
	}
	prewarmStart := now()
	prewarmTotal := 0
	for class, ymds := range expsByClass {
		prewarmResults := c.PrewarmOptionChain(ctx, sym, ymds, class, 30*time.Second)
		for _, r := range prewarmResults {
			if r.Err != nil {
				log.Warnf("gamma.prewarm class=%s expiry=%s cached=%d elapsed=%s err=%v",
					class, r.Expiry, r.Cached, r.Elapsed.Round(time.Millisecond), r.Err)
				continue
			}
			log.Infof("gamma.prewarm class=%s expiry=%s cached=%d elapsed=%s",
				class, r.Expiry, r.Cached, r.Elapsed.Round(time.Millisecond))
			prewarmTotal += r.Cached
		}
	}
	log.Infof("gamma.prewarm.done total_cached=%d wall_clock=%s",
		prewarmTotal, time.Since(prewarmStart).Round(time.Millisecond))

	// Filter jobs to only those whose (symbol, expiry, strike, right)
	// is cached. secDefOptParams returns a SUPERSET of strikes across
	// exchanges, so the original enumeration includes strikes that
	// don't exist as listed contracts on every expiry. Those cache
	// misses would force the per-leg fetcher to call
	// resolveOptionContract → fetchOptionContractDetail, which under
	// burst load times out at 5s × 4-exchange-attempts and counts as
	// Throttle=true. Even 5% of such failures trip the throttle-abort
	// detector and kill the fan-out before legitimate legs complete.
	//
	// The prewarm response IS the gateway's authoritative list of
	// listed contracts; if a strike isn't in the cache after prewarm,
	// no contract exists for it on this expiry. Skip those jobs.
	beforeFilter := len(jobs)
	filteredJobs := jobs[:0]
	for _, j := range jobs {
		if c.IsOptionContractCached(sym, j.tradingClass, j.expiryYMD, j.strike, j.right) {
			filteredJobs = append(filteredJobs, j)
		}
	}
	jobs = filteredJobs
	if len(jobs) < beforeFilter {
		log.Infof("gamma.filter dropped=%d from=%d to=%d (strikes not in prewarm cache)",
			beforeFilter-len(jobs), beforeFilter, len(jobs))
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("zero-gamma: no cached option contracts after prewarm (prewarm landed %d total)",
			prewarmTotal)
	}

	// Keep the connection's default MarketDataType (type=2, frozen-aware).
	// Verified 2026-05-21: `ibkr chain SPY` works off-hours via the same
	// handleOptionComputation routing the gamma fan-out depends on — both
	// run under type=2 and chain reliably gets model ticks per leg.
	//
	// The prior v0.28 switch to type=1 was meant to chase OI ticks
	// (27/28) which type=2 doesn't push, but empirically type=1 also
	// fails to deliver OI off-hours (10/1260 in the last failed run) AND
	// suppresses the model ticks the IV path depends on. The legFetcher
	// now treats OI as opportunistic — see the productionLegFetcher
	// comment — so this trade-off goes away.

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
		r := fetch(ctx, c, sym, j.tradingClass, j.expiryYMD, j.strike, j.right, spot, spotAt)
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
		dte := dteYears(j.expiryYMD, j.tradingClass, spotAt)
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
			tradingClass:    j.tradingClass,
			isCall:          j.right == "C",
			iv:              r.IV,
			oi:              r.OI,
			oiObserved:      r.OIObserved,
			gammaAtSnapshot: r.Gamma,
		})
		mu.Unlock()
	})

	fanoutElapsed := time.Since(startWall).Round(time.Millisecond)
	if ctx.Err() != nil {
		log.Warnf("gamma.abort reason=ctx_cancelled landed=%d/%d elapsed=%s err=%v",
			len(legs), len(jobs), fanoutElapsed, ctx.Err())
		return nil, ctx.Err()
	}
	if len(legs) == 0 {
		switch {
		case earlyAbort.Load():
			log.Warnf("gamma.abort reason=early_abort landed=%d/%d elapsed=%s no_contract=%d",
				len(legs), len(jobs), fanoutElapsed, noContract.Load())
			// Both the model-tick path AND the BS-IV fallback failed
			// to produce a single usable leg in 30 s. With the v0.26.0
			// BS-IV fallback the pre-market case is supposed to
			// recover; if we land here it usually means the gateway
			// dropped model/price ticks entirely (entitlement gap,
			// feed-farm outage, or session-boundary pause).
			return nil, fmt.Errorf("zero-gamma: no option data landed in first %ds (neither model ticks nor prior-session prices for BS-IV fallback). Check gateway entitlement and farm-connection notices in the daemon log",
				int(earlyAbortAfter.Seconds()))
		case throttledAbort.Load():
			log.Warnf("gamma.abort reason=throttled landed=%d/%d elapsed=%s no_contract=%d",
				len(legs), len(jobs), fanoutElapsed, noContract.Load())
			return nil, fmt.Errorf("zero-gamma: gateway throttled (%d of %d first-wave legs failed contract resolution); aborted to avoid compounding rate-limit pressure",
				noContract.Load(), done.Load())
		default:
			log.Warnf("gamma.abort reason=no_legs landed=%d/%d elapsed=%s",
				len(legs), len(jobs), fanoutElapsed)
			return nil, fmt.Errorf("zero-gamma: all %d legs failed to return usable IV/pricing", len(jobs))
		}
	}
	log.Infof("gamma.fanout.done landed=%d/%d derived_iv=%d elapsed=%s",
		len(legs), len(jobs), derivedIVs.Load(), fanoutElapsed)
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
	// Partition legs into the v3 three-bucket horizon split:
	//   0DTE   — DTE strictly inside one calendar day (the 09:30/16:00
	//            settlement instant has not yet passed; selectExpirations
	//            already filters past-settlement same-day listings).
	//   1to7   — 0 < DTE ≤ 7 days (overnight through end-of-week).
	//   term   — DTE > 7 days (monthly OPEX and quarterly horizons).
	//
	// Each bucket runs its own sweep; the regime row uses bucket
	// agreement/divergence to surface horizon signal.
	var zeroDTELegs, oneToSevenLegs, termLegs []legData
	// At-spot aggregate: Σ |Γ_i| × OI_i × multiplier × spot² over all
	// legs, sign-agnostic by construction. v3 fix (B1): derive the
	// per-leg gamma via Black-Scholes from the captured IV at snapshot
	// spot rather than reading the gateway's optional Greeks tick. The
	// v2 code path read r.Gamma which the productionLegFetcher only
	// populated when GetOptionGreeks returned a non-empty row — but
	// the IV-poll loop exits as soon as IV > 0, so an IV-arrived-but-
	// Greeks-haven't race left gammaAtSnapshot = 0 and every such leg
	// contributed 0 to the magnitude. Off-hours, where the model-tick
	// engine is bursty, this cancelled out most legs and the sum
	// landed at $0. Recomputing via bsGamma matches the sweep's recipe
	// at the snapshot point (internally consistent) and is non-zero
	// whenever IV > 0 — which is the OK-leg invariant.
	// Carry the snapshot-recomputed gamma onto each leg so
	// rankTopStrikesByAbsGEX picks up the same value; otherwise the
	// top-strikes table and the magnitude row would diverge for the
	// same race-affected legs.
	legDiagnostics := buildGammaLegDiagnostics(sym, legs, spot)
	gexLegs, gammaTotalAbs := prepareGEXLegs(legs, spot)
	if len(gexLegs) == 0 {
		return nil, fmt.Errorf("zero-gamma: no usable GEX legs: %d priced legs landed, but none had non-zero open-interest-weighted gamma (%s)",
			len(legs), formatGammaLegDiagnostics(legDiagnostics))
	}

	for _, l := range gexLegs {
		switch {
		case l.dte < zeroDTECutoffYears:
			zeroDTELegs = append(zeroDTELegs, l)
		case l.dte <= nearDTECutoffYears:
			oneToSevenLegs = append(oneToSevenLegs, l)
		default:
			termLegs = append(termLegs, l)
		}
	}

	profile := sweepProfile(gexLegs, spot, params.SweepRangePct, skewByExpiry)
	profile0DTE := sweepProfile(zeroDTELegs, spot, params.SweepRangePct, skewByExpiry)
	profile1to7 := sweepProfile(oneToSevenLegs, spot, params.SweepRangePct, skewByExpiry)
	profileTerm := sweepProfile(termLegs, spot, params.SweepRangePct, skewByExpiry)
	progress.Store(90)

	// 8. Zero crossings: combined + 0DTE + 1-7 + term.
	zg, gammaSign := findZeroCrossing(profile)
	var gapPct *float64
	if zg != nil {
		v := (spot - *zg) / *zg * 100
		gapPct = &v
	}
	zg0DTE, sign0DTE := findZeroCrossing(profile0DTE)
	zg1to7, sign1to7 := findZeroCrossing(profile1to7)
	zgTerm, signTerm := findZeroCrossing(profileTerm)

	// 9. Top strikes by magnitude.
	topStrikes := rankTopStrikesByAbsGEX(gexLegs, spot, topStrikesK, sym)

	// Coverage gate. A compute whose successful-leg fraction falls
	// below MinLegCoverageFraction is surfaced as an error so the
	// existing gammaErrorRetryTTL retry machinery applies — without
	// this, a single throttled fan-out at NY-session start poisons
	// the cache for the rest of the day (same shape as the v0.27.0
	// breadth poison-cache bug, mitigated for breadth at v0.27.3).
	if err := checkLegCoverage(len(legs), len(jobs), throttledAbort.Load()); err != nil {
		log.Warnf("gamma.abort reason=low_coverage landed=%d/%d elapsed=%s err=%v",
			len(legs), len(jobs), time.Since(startWall).Round(time.Millisecond), err)
		return nil, err
	}

	// Warnings. Ordered "throttled" first because it explains why
	// coverage is low, not the other way around.
	var warnings []string
	if throttledAbort.Load() {
		warnings = append(warnings, "throttled")
	}
	if gammaOIMissingCount(legDiagnostics) > 0 {
		warnings = append(warnings, "oi_missing")
	}
	if strikeBudgetCapped {
		warnings = append(warnings, "strike_budget_capped")
	}
	if zg == nil {
		warnings = append(warnings, "no_crossing_in_window")
	}
	if len(zeroDTELegs) == 0 {
		warnings = append(warnings, "0dte_no_legs")
	}
	if len(oneToSevenLegs) == 0 {
		warnings = append(warnings, "1to7_no_legs")
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
	if len(zeroDTELegs) == 0 {
		sign0DTE = "no_data"
	}
	if len(oneToSevenLegs) == 0 {
		sign1to7 = "no_data"
	}
	if len(termLegs) == 0 {
		signTerm = "no_data"
	}

	skewModel := ""
	if len(skewFitQuality) > 0 {
		skewModel = "sticky-moneyness-v1"
	}

	// Concentration ratio: share of the sign-agnostic |Γ|·OI sum parked
	// at the single largest strike. Zero-guard for the empty-table case
	// (every leg failed) and for a degenerate sum-of-zeros.
	var topConcentrationPct float64
	if len(topStrikes) > 0 && gammaTotalAbs > 0 {
		topConcentrationPct = topStrikes[0].AbsGEX / gammaTotalAbs * 100
	}

	res := &rpc.GammaZeroComputed{
		SpotUnderlying:          spot,
		SpotAt:                  spotAt,
		ZeroGamma:               zg,
		GapPct:                  gapPct,
		GammaSign:               gammaSign,
		Profile:                 profile,
		ZeroGamma0DTE:           zg0DTE,
		Profile0DTE:             profile0DTE,
		GammaSign0DTE:           sign0DTE,
		LegCount0DTE:            len(zeroDTELegs),
		ZeroGamma1to7:           zg1to7,
		Profile1to7:             profile1to7,
		GammaSign1to7:           sign1to7,
		LegCount1to7:            len(oneToSevenLegs),
		ZeroGammaTerm:           zgTerm,
		ProfileTerm:             profileTerm,
		GammaSignTerm:           signTerm,
		LegCountTerm:            len(termLegs),
		SkewModel:               skewModel,
		SkewFitQuality:          skewFitQuality,
		GammaTotalAbs:           gammaTotalAbs,
		GammaTotalAbsConvention: "sign-agnostic",
		TopStrikes:              topStrikes,
		TopConcentrationPct:     topConcentrationPct,
		SweepLowAbs:             spot * (1 - params.SweepRangePct),
		SweepHighAbs:            spot * (1 + params.SweepRangePct),
		Expirations:             pickedDatesFromPicked(picked),
		LegCount:                len(gexLegs),
		PricedLegCount:          len(legs),
		DerivedIVLegs:           derivedCount,
		LegDiagnostics:          legDiagnostics,
		Warnings:                warnings,
		Params:                  params,
		Scope:                   strings.ToLower(sym),
		Source:                  fmt.Sprintf("computed from IBKR %s option chain", sym),
		Method:                  gammaMethodToken,
		MethodologyCitations:    gammaMethodologyCitations,
		AsOf:                    now(),
		DurationMS:              now().Sub(startWall).Milliseconds(),
	}
	progress.Store(100)
	zeroGammaStr := "—"
	if zg != nil {
		zeroGammaStr = fmt.Sprintf("%.2f", *zg)
	}
	log.Infof("gamma.done gex_legs=%d priced_legs=%d/%d derived_iv=%d spot=%.2f zero_gamma=%s sign=%s elapsed=%s",
		len(gexLegs), len(legs), len(jobs), derivedCount, spot, zeroGammaStr, gammaSign,
		time.Since(startWall).Round(time.Millisecond))
	if err := validateGammaComputed(res); err != nil {
		return nil, err
	}
	return hydrateGammaComputed(res), nil
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

// snapshotUnderlyingForGamma polls the connector's market-data cache for
// the underlying spot. Returns (spot, bid, ask, last, dataType) — spot
// picks last → mid → bid → ask → mark → close, matching the
// briefSnapshotPrice convention. Mark (tick 37) covers most off-hours
// frozen states; close (tick 9) is the last-resort anchor for the rare
// case where the gateway hasn't even pushed a mark yet.
//
// Caller MUST hold an active market-data subscription for sym for the
// duration of this call AND for the lifetime of the option fan-out that
// follows. IBKR requires the underlying to be subscribed for the model
// engine to push OPTION_COMPUTATION (msg 21) ticks on the OPT
// subscriptions — without a held underlying the gateway suppresses the
// IV/Greeks tick stream and the leg fan-out lands ~0% useful data.
// Acquire via subManager.Hold at the call site so refcounting is honest
// and the briefSnapshotFull subscribe/unsubscribe race that previously
// tore the line down mid-fan-out is structurally excluded.
func snapshotUnderlyingForGamma(ctx context.Context, c *ibkrlib.Connector, sym string, timeout time.Duration) (spot, bid, ask, last float64, dataType string) {
	if c == nil {
		return 0, 0, 0, 0, ""
	}
	sym = normSym(sym)
	var mark, closePx float64
	_ = pollMarketData(ctx, c, sym, time.Now().Add(timeout), func(d *ibkrlib.MarketData) bool {
		bid, ask, last, mark, closePx = d.Bid, d.Ask, d.Last, d.MarkPrice, d.Close
		if dataType == "" && (bid > 0 || ask > 0 || last > 0 || mark > 0 || closePx > 0) {
			dataType = marketDataTypeName(c.GetMarketDataTypeForSymbol(sym))
		}
		return bid > 0 || ask > 0 || last > 0 || mark > 0
	})
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

// classSettlementInstant returns the NY-time settlement instant for an
// option of the given trading class on the supplied day. SPX-class
// monthlies are AM-settled at 09:30 ET (cash-settled against the SET
// special opening quotation); SPXW weeklies and everything else are
// PM-settled at 16:00 ET. Empty class is treated as the PM default for
// back-compat with single-class callers (SPY, equities).
//
// Used by dteYears to compute time-to-expiry under the correct
// settlement convention, and by selectExpirations to decide whether a
// same-day listing is already past its cash-settlement window.
func classSettlementInstant(tradingClass string, year int, month time.Month, day int, loc *time.Location) time.Time {
	if strings.EqualFold(strings.TrimSpace(tradingClass), "SPX") {
		return time.Date(year, month, day, 9, 30, 0, 0, loc)
	}
	return time.Date(year, month, day, 16, 0, 0, 0, loc)
}

// classSettlementBuffer is the post-settlement grace window the
// expiry-filter uses before tagging a same-day listing as "expired."
// Mirrors the original 15-minute buffer on the unified 16:15 cutoff;
// applied symmetrically to AM-settled and PM-settled classes so the
// boundary semantics stay consistent across the SPX/SPXW split.
const classSettlementBuffer = 15 * time.Minute

// selectExpirations picks up to N expirations using a slot-anchored
// policy. Same-day expiries before their class's settlement cutoff
// count; same-day expiries after settlement are dropped (see
// classSettlementInstant + classSettlementBuffer).
//
// The slot policy is fixed:
//
//	1  front-week-1    nearest unused candidate
//	2  front-week-2    next nearest unused candidate
//	3  EOW             this calendar week's Friday (rolls to fill if already used)
//	4  next-monthly    next 3rd-Friday (any month)
//	5  next-quarterly  next 3rd-Friday of Mar/Jun/Sep/Dec
//	6+ fill            nearest unused candidate, until count is reached
//
// Each anchored slot (3–5) is allowed to find no candidate, in which
// case the slot is skipped and the fill rule covers the remaining
// count. The output is sorted ascending for stable downstream
// iteration.
//
// Why the slot policy: lexicographic-N picked only weeklies inside
// ~2 weeks on the SPY chain (max DTE = 13), so the term gamma bucket
// (DTE > 7) was computed over almost-weeklies. Anchoring monthly +
// quarterly slots guarantees the basket reaches the OPEX and
// institutional-collar horizons that dominate dealer term-gamma.
//
// tradingClass is the option class whose settlement instant defines
// the today-cutoff (same semantics as before this refactor): "SPX"
// cuts off at 09:45 ET (AM-settle + 15-min buffer); "SPXW", "SPY",
// and empty default to 16:15 ET (PM-settle + 15-min buffer).
//
// Input map keys are YYYY-MM-DD; output is the picked subset in the
// same date format, sorted ascending.
func selectExpirations(strikes map[string][]float64, tradingClass string, now time.Time, count int) []string {
	if count <= 0 {
		return nil
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	nyNow := now.In(loc)
	today := nyNow.Format("2006-01-02")
	settlementCutoff := classSettlementInstant(tradingClass, nyNow.Year(), nyNow.Month(), nyNow.Day(), loc).Add(classSettlementBuffer)
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
	return pickExpirationSlots(candidates, nyNow, count)
}

// pickExpirationSlots applies the slot policy documented on
// selectExpirations to an already-filtered, ascending-sorted candidate
// list. Exposed at package scope (lowercase but distinct from
// selectExpirations) so tests can drive the picker independently of
// the trading-class settlement-filter logic.
//
// candidates: sorted ascending, must already be non-expired.
// nyNow:      NY-localised wall-clock; used by the EOW anchor.
// count:      maximum slots to fill (typical 6).
func pickExpirationSlots(candidates []string, nyNow time.Time, count int) []string {
	if count <= 0 || len(candidates) == 0 {
		return nil
	}
	used := make(map[string]struct{}, count)
	picks := make([]string, 0, count)

	// attempt tries to add the first candidate matching predicate that
	// hasn't been used yet. Returns true when the slot was filled.
	attempt := func(predicate func(string) bool) bool {
		if len(picks) >= count {
			return false
		}
		for _, d := range candidates {
			if _, ok := used[d]; ok {
				continue
			}
			if predicate(d) {
				used[d] = struct{}{}
				picks = append(picks, d)
				return true
			}
		}
		return false
	}

	always := func(string) bool { return true }

	// Slots 1-2: front-week-1, front-week-2 — nearest two unused.
	attempt(always)
	attempt(always)

	// Slot 3: EOW — this calendar week's Friday from nyNow (>= today).
	// Falls through silently when that Friday isn't in candidates or
	// has already been picked as a front-week slot.
	thisFri := thisWeekFriday(nyNow)
	attempt(func(d string) bool { return d == thisFri })

	// Slot 4: next-monthly — next 3rd-Friday in candidates.
	attempt(isThirdFridayDate)

	// Slot 5: next-quarterly — next 3rd-Friday of Mar/Jun/Sep/Dec.
	// Always runs even if the monthly slot already picked the quarter's
	// 3rd-Friday; the predicate skips used candidates so the quarterly
	// slot lands on the NEXT quarter, not the same one.
	attempt(isQuarterlyThirdFridayDate)

	// Fill: nearest unused until count is reached.
	for _, d := range candidates {
		if len(picks) >= count {
			break
		}
		if _, ok := used[d]; ok {
			continue
		}
		used[d] = struct{}{}
		picks = append(picks, d)
	}

	sort.Strings(picks)
	return picks
}

// thisWeekFriday returns the YYYY-MM-DD of the calendar Friday >= nyNow's
// date. When nyNow falls on a Friday the date returned is nyNow's own
// date; on Saturday/Sunday it rolls forward to the upcoming Friday.
//
// Used by the EOW anchor in pickExpirationSlots. Pure helper, no
// dependency on the strike map — that way the slot rule's "end of
// this week" semantics are independent of which specific Fridays the
// chain happens to list.
func thisWeekFriday(nyNow time.Time) string {
	daysToFri := (int(time.Friday) - int(nyNow.Weekday()) + 7) % 7
	fri := time.Date(nyNow.Year(), nyNow.Month(), nyNow.Day()+daysToFri, 0, 0, 0, 0, nyNow.Location())
	return fri.Format("2006-01-02")
}

// isThirdFridayDate reports whether a YYYY-MM-DD string is the 3rd
// Friday of its month. 3rd Fridays fall on days 15-21 — the only
// Friday in that span each month. Used by the monthly anchor.
func isThirdFridayDate(yyyyMMdd string) bool {
	t, err := time.Parse("2006-01-02", yyyyMMdd)
	if err != nil {
		return false
	}
	if t.Weekday() != time.Friday {
		return false
	}
	d := t.Day()
	return d >= 15 && d <= 21
}

// isQuarterlyThirdFridayDate reports whether a YYYY-MM-DD is the 3rd
// Friday of a quarterly month (Mar / Jun / Sep / Dec). Used by the
// quarterly anchor.
func isQuarterlyThirdFridayDate(yyyyMMdd string) bool {
	if !isThirdFridayDate(yyyyMMdd) {
		return false
	}
	t, _ := time.Parse("2006-01-02", yyyyMMdd)
	switch t.Month() {
	case time.March, time.June, time.September, time.December:
		return true
	}
	return false
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

func capStrikesATMOutward(strikes []float64, maxCount int) ([]float64, bool) {
	if maxCount <= 0 || len(strikes) <= maxCount {
		return strikes, false
	}
	return strikes[:maxCount], true
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
// string under the correct settlement-instant for the option's trading
// class. SPX-class monthlies expire at 09:30 ET (AM SET); SPXW
// weeklies, SPY, and equities expire at 16:00 ET (PM close). Empty
// tradingClass falls back to 16:00 ET — back-compat for the SPY-only
// path before the SPX coverage arc.
//
// Zero on parse failure or non-positive deltas — the compute's per-leg
// gate filters those out.
//
// Why this matters: an SPX-class third-Friday option at 10:00 ET on
// expiry day has already settled at 09:30; pricing it with 6.5 extra
// hours of TTE under the legacy 16:00 instant would over-state its
// gamma. The aggregate is dollar-significant — third-Friday SPX gamma
// dominates the day-of-expiry book.
func dteYears(expiryYMD, tradingClass string, now time.Time) float64 {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	day, err := time.ParseInLocation("20060102", expiryYMD, loc)
	if err != nil {
		return 0
	}
	expWall := classSettlementInstant(tradingClass, day.Year(), day.Month(), day.Day(), loc)
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
//
// underlying is stamped onto each row's StrikeConcentration so the
// combined-scope merge (step 7) can keep SPY vs SPX rows
// distinguishable in the merged top-K table without re-deriving the
// information from the leg's tradingClass.
func rankTopStrikesByAbsGEX(legs []legData, spot float64, k int, underlying string) []rpc.StrikeConcentration {
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
				Underlying:   underlying,
				TradingClass: l.tradingClass,
				Strike:       l.strike,
				Expiry:       l.expiryYMD[:4] + "-" + l.expiryYMD[4:6] + "-" + l.expiryYMD[6:8],
				Right:        l.right,
				AbsGEX:       v,
				OI:           l.oi,
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

func prepareGEXLegs(legs []legData, spot float64) ([]legData, float64) {
	gexLegs := make([]legData, 0, len(legs))
	total := 0.0
	for _, l := range legs {
		l.gammaAtSnapshot = bsGamma(spot, l.strike, l.dte, l.iv, 0, 0)
		v := absGEX(l.gammaAtSnapshot, float64(l.oi), 100, spot)
		if v == 0 {
			continue
		}
		total += v
		gexLegs = append(gexLegs, l)
	}
	return gexLegs, total
}
