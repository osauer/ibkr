package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// Default calibration window for the v1 zero-gamma compute. Tuned for
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

	// sweepPoints is the number of (spot, GEX) samples in the profile.
	// 60 points across [0.85, 1.15] × spot ≈ 0.5 % per point, which
	// fits the precision the methodology can defensibly claim.
	sweepPoints = 60

	// topStrikesK is the number of concentration rows on the result —
	// enough for a renderer to draw the "call wall / put wall" view
	// without flooding the JSON payload.
	topStrikesK = 8

	// Throttle-signal abort. SPX's option-chain fan-out makes hundreds
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
	// cache stamps on a fresh kickoff. Calibration:
	//   6 expirations × ~80 strikes × 2 sides = 960 legs (worst case)
	//   960 / 4 workers × 3.5 s/leg ≈ 14 min worst case
	//   typical wall-clock 5-8 min with warm contract cache.
	// 360s is a conservative midpoint that doesn't over-promise.
	computeETA = 360
)

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

// legFetcher abstracts the per-leg subscribe-collect-unsubscribe so
// tests can drive computeGammaZeroSPX with a fake. The fetcher is
// expected to block for at most the budget the caller passes via ctx.
//
// ok reports whether OI + IV both landed within budget — the
// aggregator only counts a leg when ok is true.
//
// throttleSignal reports a contract-resolve failure on a strike that
// came from the gateway's own enumeration. The aggregator counts
// these in the noContract tally that drives the throttle abort.
// Soft drops (subscribed but data didn't land) leave both flags
// false: they're skip-this-leg without raising the throttle alarm.
type legFetcher func(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying, expiryYMD string,
	strike float64,
	right string,
) (oi int64, iv, gamma float64, ok, throttleSignal bool)

// productionLegFetcher is the live-gateway implementation. It
// subscribes the option contract, polls until OI and IV land or the
// per-leg budget expires, reads GetOptionGreeks for the gamma at the
// current spot, then unsubscribes. Designed to mirror fillOptionLeg's
// concurrency posture so the existing 4-worker rate-limit observation
// stays valid.
func productionLegFetcher(
	ctx context.Context,
	c *ibkrlib.Connector,
	underlying, expiryYMD string,
	strike float64,
	right string,
) (oi int64, iv, gamma float64, ok, throttleSignal bool) {
	if c == nil {
		return 0, 0, 0, false, true
	}
	key, _, err := c.SubscribeOption(ctx, underlying, expiryYMD, strike, right)
	if err != nil {
		// SubscribeOption's error path is dominated by
		// resolveOptionContract failing with either "contract details
		// unavailable" (empty contractDataEnd) or
		// ErrContractDetailsTimeout. Both are the canonical throttle
		// signal: we enqueued strikes that came from the gateway's
		// own enumeration, so a healthy session resolves them.
		return 0, 0, 0, false, true
	}
	defer func() { _ = c.UnsubscribeMarketData(key) }()

	// 5 s per-leg budget covers both OI and IV. OI ticks (27/28) often
	// arrive ~1 s after first bid/ask; IV (model-computation tick 21)
	// can be slower for far-OTM legs. We treat the leg as successful
	// as soon as OI is positive AND IV is positive — gamma is read
	// from GetOptionGreeks immediately after.
	deadline := time.Now().Add(5 * time.Second)
	err = pollMarketData(ctx, c, key, deadline, func(d *ibkrlib.MarketData) bool {
		if d.OpenInt > 0 && d.IV > 0 {
			oi = d.OpenInt
			iv = d.IV
			return true
		}
		return false
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return 0, 0, 0, false, false
	}
	// Read whatever final values landed before the deadline — the
	// pollMarketData inner closure already captured them in the
	// shared locals, but partial captures (OI without IV or vice
	// versa) are dropped because the aggregator needs both.
	if oi == 0 || iv == 0 {
		return 0, 0, 0, false, false
	}
	// Gateway-computed gamma at the current spot. May be 0 if the
	// model-computation tick (21) hasn't fired for this leg; that's
	// fine because the sweep recomputes gamma at every scenario spot.
	if g, gok := c.GetOptionGreeks(key); gok {
		gamma = g.Gamma
	}
	return oi, iv, gamma, true, false
}

// computeGammaZeroSPX runs the full Phase 2 compute. The caller (the
// cache's background goroutine) supplies a ctx bounded only by daemon
// shutdown — not RPC deadlines — and an atomic progress counter the
// fan-out updates as it advances. Returns a populated result on
// success or a classified error on failure (stale spot, no usable
// legs, gateway disconnect).
//
// Methodology (perfiliev-bs-sweep-v1):
//
//  1. Snapshot SPX spot. Refuse on stale (data_type != live and not
//     empty-pending) — the compute is anchored on a single spot and a
//     known-bad spot poisons everything downstream.
//
//  2. Enumerate expirations + listed strikes via FetchOptionExpiryStrikes.
//     The merge across SPX/SPXW trading classes is automatic at this
//     layer (pinned by TestFetchOptionExpiriesMergesAcrossTradingClasses).
//
//  3. Pick the nearest N non-0DTE-post-settlement expirations. The
//     0DTE filter is the *evening* of expiration day in NY: at 09:30
//     ET on a 3rd Friday SPX expiry, we still include it; at 16:15 ET
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

	// 1. SPX spot snapshot.
	progress.Store(2)
	const sym = "SPX"
	spot, bid, ask, last, dataType := snapshotSPXForGamma(ctx, c, 5*time.Second)
	if spot <= 0 {
		return nil, fmt.Errorf("zero-gamma: no SPX spot available (gateway returned no live tick)")
	}
	if !isAcceptableDataType(dataType) {
		return nil, fmt.Errorf("zero-gamma: SPX spot is %s; refusing to compute on stale data", dataType)
	}
	spotAt := now()
	_ = bid
	_ = ask
	_ = last

	// 2. Expirations + strikes. SPX has hundreds of listed expirations
	// (AM-settled SPX + PM-settled SPXW) and tens of thousands of
	// strikes — the secDefOptParams response is large and streams in
	// over several seconds. 30 s mirrors the per-method budget the
	// existing handleChainExpiries handler runs with for the same call
	// (server.go's unaryDeadline for MethodChainExpiries is 50 s).
	progress.Store(5)
	allStrikes, err := c.FetchOptionExpiryStrikes(sym, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("zero-gamma: fetch SPX expiries: %w", err)
	}
	if len(allStrikes) == 0 {
		return nil, fmt.Errorf("zero-gamma: gateway returned no SPX expirations")
	}

	// 3. Select expirations.
	pickedExp := selectExpirations(allStrikes, spotAt, params.ExpiryCount)
	if len(pickedExp) == 0 {
		return nil, fmt.Errorf("zero-gamma: no usable SPX expirations after 0DTE filtering")
	}
	progress.Store(10)

	// 4. Build the per-expiry strike grids.
	type legSpec struct {
		expiryYMD string
		strike    float64
		right     string
	}
	var jobs []legSpec
	for _, expDate := range pickedExp {
		strikes := filterStrikesAroundSpot(allStrikes[expDate], spot, params.StrikeWidthPct)
		expYMD := compactExpiry(expDate)
		for _, k := range strikes {
			jobs = append(jobs, legSpec{expiryYMD: expYMD, strike: k, right: "C"})
			jobs = append(jobs, legSpec{expiryYMD: expYMD, strike: k, right: "P"})
		}
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("zero-gamma: no SPX strikes within ±%.0f%% of spot %.2f",
			params.StrikeWidthPct*100, spot)
	}

	// 5. Fan-out. Mutex around shared aggregation slice; the contention
	// is bounded at one append per completed leg (cheap relative to the
	// per-leg roundtrip).
	var (
		legs           []legData
		mu             sync.Mutex
		done           atomic.Int32
		noContract     atomic.Int32
		throttledAbort atomic.Bool
		total          = int32(len(jobs))
	)
	runBounded(jobs, params.WorkerCount, func(j legSpec) {
		if ctx.Err() != nil || throttledAbort.Load() {
			return
		}
		oi, iv, gamma, ok, throttleSignal := fetch(ctx, c, sym, j.expiryYMD, j.strike, j.right)
		// Always increment the progress counter — failed legs still
		// represent work attempted. 10 % is consumed by spot+expiries
		// stages above; the fan-out scales linearly from 10 → 85.
		d := done.Add(1)
		if throttleSignal {
			nc := noContract.Add(1)
			if throttleDetected(d, nc) {
				throttledAbort.Store(true)
			}
		}
		if total > 0 {
			progress.Store(10 + int32(75*float64(d)/float64(total)))
		}
		if !ok {
			return
		}
		dte := dteYears(j.expiryYMD, spotAt)
		if dte <= 0 || iv <= 0 {
			// Belt-and-suspenders: skip legs whose DTE/IV degenerate
			// after fetch (in flight expiry rollover, or a partial OI
			// tick that snuck past the fetcher's gate).
			return
		}
		mu.Lock()
		legs = append(legs, legData{
			expiryYMD:       j.expiryYMD,
			dte:             dte,
			strike:          j.strike,
			right:           j.right,
			isCall:          j.right == "C",
			iv:              iv,
			oi:              oi,
			gammaAtSnapshot: gamma,
		})
		mu.Unlock()
	})

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(legs) == 0 {
		if throttledAbort.Load() {
			return nil, fmt.Errorf("zero-gamma: gateway throttled (%d of %d first-wave legs failed contract resolution); aborted to avoid compounding rate-limit pressure",
				noContract.Load(), done.Load())
		}
		return nil, fmt.Errorf("zero-gamma: all %d legs failed to return OI+IV", len(jobs))
	}
	progress.Store(85)

	// 6-7. Sweep + aggregate.
	profile := sweepProfile(legs, spot, params.SweepRangePct)
	// At-spot aggregates: re-use the profile's snapshot bucket OR
	// compute directly. We compute directly so the headline numbers
	// don't depend on the sweep grid alignment.
	gammaTotalAbs := 0.0
	for _, l := range legs {
		gammaTotalAbs += absGEX(l.gammaAtSnapshot, float64(l.oi), 100, spot)
	}
	progress.Store(90)

	// 8. Zero crossing.
	zg, gammaSign := findZeroCrossing(profile)
	var gapPct *float64
	if zg != nil {
		v := (spot - *zg) / *zg * 100
		gapPct = &v
	}

	// 9. Top strikes by magnitude.
	topStrikes := rankTopStrikesByAbsGEX(legs, spot, topStrikesK)

	// Warnings. Ordered "throttled" first because it explains why
	// coverage is low, not the other way around.
	var warnings []string
	if throttledAbort.Load() {
		warnings = append(warnings, "throttled")
	}
	coverage := float64(len(legs)) / float64(len(jobs))
	if coverage < 0.5 {
		warnings = append(warnings, "low_leg_coverage")
	}
	if zg == nil {
		warnings = append(warnings, "no_crossing_in_window")
	}

	res := &rpc.GammaZeroComputed{
		SpotSPX:       spot,
		SpotAt:        spotAt,
		ZeroGamma:     zg,
		GapPct:        gapPct,
		GammaSign:     gammaSign,
		Profile:       profile,
		GammaTotalAbs: gammaTotalAbs,
		TopStrikes:    topStrikes,
		Expirations:   pickedExp,
		LegCount:      len(legs),
		Warnings:      warnings,
		Params:        params,
		Source:        "computed from IBKR option chain (SPX + SPXW)",
		Method:        "perfiliev-bs-sweep-v1",
		AsOf:          now(),
		DurationMS:    now().Sub(startWall).Milliseconds(),
	}
	progress.Store(100)
	return res, nil
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

// snapshotSPXForGamma wraps briefSnapshotFull with the SPX-specific
// fallbacks the compute pipeline needs. Returns (spot, bid, ask,
// last, dataType) — spot picks last → mid → bid → ask in that order,
// matching the existing briefSnapshotPrice convention.
func snapshotSPXForGamma(ctx context.Context, c *ibkrlib.Connector, timeout time.Duration) (spot, bid, ask, last float64, dataType string) {
	bid, ask, last, dataType = briefSnapshotFull(ctx, c, "SPX", timeout)
	switch {
	case last > 0:
		spot = last
	case bid > 0 && ask > 0:
		spot = (bid + ask) / 2
	case bid > 0:
		spot = bid
	case ask > 0:
		spot = ask
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
// recomputes per-leg Γ via Black-Scholes with the leg's captured IV
// and DTE (fixed IV across sweep — v1 limitation).
func sweepProfile(legs []legData, snapshotSpot, sweepRangePct float64) []rpc.GammaProfilePoint {
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
			γ := bsGamma(scenarioSpot, l.strike, l.dte, l.iv, 0, 0)
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
