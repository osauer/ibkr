package daemon

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// decoupledCorrThreshold is the SPY/SPX 20-day daily-close correlation
// below which the renderer promotes per-index headlines to primary and
// badges the combined γ-zero as "decoupled." Calibrated per design
// §5.3.1: 0.90 catches the post-2020 fast-decoupling events while
// keeping false positives near zero in calm regimes (≥ 0.97 typical).
const decoupledCorrThreshold = 0.90

// correlationLookback is the daily-close window used for the
// SPY/SPX decorrelation gate. 20 sessions matches the breadth engine's
// existing rolling-window convention; long enough to smooth noise,
// short enough that single-event decouplings (2020-03, 2024-08-05)
// show up in the gate.
const correlationLookback = 20

// compute20DaySPYSPXCorrelation returns the Pearson r over the last N
// daily closes of SPY and SPX. Nil + err on fetch failure; nil + nil
// when one side has too few bars to compute (degenerate). The
// caller's decision: a nil correlation means "couldn't compute" —
// surface as a warning but don't gate the combined headline on it.
//
// Lookback is correlationLookback (20 sessions). The fetcher
// transparently uses the existing FetchHistoricalDailyBars path
// breadth already drives, so the historical-data pacing budget is
// shared and a warm cache absorbs the cost on repeat runs.
func compute20DaySPYSPXCorrelation(c *ibkrlib.Connector) (*float64, error) {
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	spyBars, err := c.FetchHistoricalDailyBars("SPY", correlationLookback, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("fetch SPY daily bars: %w", err)
	}
	spxBars, err := c.FetchHistoricalDailyBars("SPX", correlationLookback, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("fetch SPX daily bars: %w", err)
	}
	r, ok := pearsonR(barCloses(spyBars), barCloses(spxBars))
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func barCloses(bars []ibkrlib.HistoricalBar) []float64 {
	out := make([]float64, 0, len(bars))
	for _, b := range bars {
		out = append(out, b.Close)
	}
	return out
}

// pearsonR computes the Pearson correlation coefficient over two equal-
// length series. Returns (r, true) on success; (0, false) when either
// series has fewer than 2 points, the lengths mismatch, or either
// series has zero variance (degenerate — undefined r).
//
// Slices are truncated to min length so a one-bar lookback mismatch
// (gateway returned 19 SPY bars but 20 SPX bars on a holiday-adjacent
// fetch) doesn't drop the whole correlation; the trailing-aligned
// truncation preserves the most-recent shared window.
func pearsonR(x, y []float64) (float64, bool) {
	n := min(len(x), len(y))
	if n < 2 {
		return 0, false
	}
	// Trailing-align: take the LAST n entries of each.
	x = x[len(x)-n:]
	y = y[len(y)-n:]

	var sumX, sumY float64
	for i := range n {
		sumX += x[i]
		sumY += y[i]
	}
	meanX := sumX / float64(n)
	meanY := sumY / float64(n)

	var num, denomX, denomY float64
	for i := range n {
		dx := x[i] - meanX
		dy := y[i] - meanY
		num += dx * dy
		denomX += dx * dx
		denomY += dy * dy
	}
	if denomX <= 0 || denomY <= 0 {
		return 0, false
	}
	return num / math.Sqrt(denomX*denomY), true
}

// combineGammaResults builds a combined SPY+SPX result envelope from
// two single-underlying GammaZeroComputed payloads.
//
// Aggregation rules (per design §5.3):
//   - Combined sweep at scenario index i: sum SPY_profile[i].GEX +
//     SPX_profile[i].GEX. Both indices' sweeps are in dollars over the
//     same relative-percent x-axis, so the sum is the dollar dealer
//     GEX of the combined book at the matching % move from current
//     spots.
//   - CombinedGapPct: the spot-percent at which the combined sweep
//     crosses zero. Nil when no crossing or when either profile is
//     empty.
//   - Top-level scalars are SPY-anchored (per design §12.1) — keeps
//     `zero_gamma` typed as a price level for JSON consumers.
//   - GammaTotalAbs sums across both indices in dollars.
//   - TopStrikes merges both indices' top-K, sorts by AbsGEX, takes
//     overall top-K. Per user-interview choice: single sorted list with
//     INDEX column; SPX rows dominate (10× dollar gamma per contract).
//   - Expirations is the union of both indices' picked dates.
//   - Warnings unions both halves; "decoupled" added when corr < threshold.
//
// decorrCorr is the SPY/SPX 20-day Pearson r; nil when not computable.
// When non-nil and below decoupledCorrThreshold, a "decoupled" warning
// is added so the renderer can promote per-index headlines.
func combineGammaResults(spy, spx *rpc.GammaZeroComputed, decorrCorr *float64) *rpc.GammaZeroComputed {
	if spy == nil && spx == nil {
		return nil
	}
	// If one side is missing, the caller should be using the
	// entitlement-graceful path (step 8). Defensive: return whichever
	// side we have, tagged as single-scope.
	if spy == nil {
		return spx
	}
	if spx == nil {
		return spy
	}

	combinedProfile, combinedGapPct := buildCombinedSweep(spy.Profile, spx.Profile, spy.SpotUnderlying)
	combinedProfileNear, _ := buildCombinedSweep(spy.ProfileNear, spx.ProfileNear, spy.SpotUnderlying)
	combinedProfileTerm, _ := buildCombinedSweep(spy.ProfileTerm, spx.ProfileTerm, spy.SpotUnderlying)

	// Combined warnings — union with stable order: SPY first, then SPX,
	// dedupe across the merge. Decorrelation badge appended last so the
	// renderer can pop it for top-of-output presentation.
	warnings := dedupeStrings(append(append([]string{}, spy.Warnings...), spx.Warnings...))
	if decorrCorr != nil && *decorrCorr < decoupledCorrThreshold {
		warnings = append(warnings, "decoupled")
	}

	// Top strikes: merge, sort by AbsGEX descending, take top-K.
	allTop := append(append([]rpc.StrikeConcentration{}, spy.TopStrikes...), spx.TopStrikes...)
	sort.SliceStable(allTop, func(i, j int) bool {
		return allTop[i].AbsGEX > allTop[j].AbsGEX
	})
	if len(allTop) > topStrikesK {
		allTop = allTop[:topStrikesK]
	}
	combinedAbs := spy.GammaTotalAbs + spx.GammaTotalAbs
	var topConcPct float64
	if combinedAbs > 0 && len(allTop) > 0 {
		topConcPct = allTop[0].AbsGEX / combinedAbs * 100
	}

	// SPY-anchored top-level fields (per design §12.1). Combined-
	// specific fields layer on top.
	out := *spy // shallow copy preserves SPY's per-index scalars
	out.Scope = rpc.GammaZeroScopeCombined
	out.Profile = combinedProfile
	out.ProfileNear = combinedProfileNear
	out.ProfileTerm = combinedProfileTerm
	out.GammaTotalAbs = combinedAbs
	out.TopStrikes = allTop
	out.TopConcentrationPct = topConcPct
	out.LegCount = spy.LegCount + spx.LegCount
	out.DerivedIVLegs = spy.DerivedIVLegs + spx.DerivedIVLegs
	out.Expirations = dedupeStrings(append(append([]string{}, spy.Expirations...), spx.Expirations...))
	sort.Strings(out.Expirations)
	out.Warnings = warnings
	out.Source = "computed from IBKR SPY+SPX option chains"
	out.CombinedGapPct = combinedGapPct
	out.DecoupledCorr = decorrCorr
	out.PerIndex = map[string]*rpc.GammaZeroComputed{
		"SPY": spy,
		"SPX": spx,
	}
	// DurationMS sums the two halves' wall clocks since they ran
	// serially. AsOf takes the later of the two.
	out.DurationMS = spy.DurationMS + spx.DurationMS
	if spx.AsOf.After(spy.AsOf) {
		out.AsOf = spx.AsOf
	}
	return &out
}

// buildCombinedSweep aggregates two single-underlying sweep profiles
// by matching scenario-percent index. Returns the combined profile
// and the spot-percent at which it crosses zero (nil if no crossing,
// nil if either input is empty, nil if length mismatch).
//
// The x-axis on each input profile is in price (each index's own
// spot × scenario percent). We discard the absolute spots and align
// by index because the sweep step is identical (60 points across
// [0.85, 1.15] × spot). The output GammaProfilePoint.Spot is the
// SPY-anchored spot at index i — chosen as the renderer-friendly
// anchor since SPY is the headline scope's spot.
//
// combinedGapPct is computed against spy_spot's anchor — the
// spot-percent at the crossing relative to the renderer's anchor.
func buildCombinedSweep(spy, spx []rpc.GammaProfilePoint, spySpot float64) ([]rpc.GammaProfilePoint, *float64) {
	if len(spy) == 0 || len(spx) == 0 {
		return nil, nil
	}
	n := min(len(spy), len(spx))
	combined := make([]rpc.GammaProfilePoint, n)
	for i := range n {
		combined[i] = rpc.GammaProfilePoint{
			Spot: spy[i].Spot,
			GEX:  spy[i].GEX + spx[i].GEX,
		}
	}
	zero, _ := findZeroCrossing(combined)
	if zero == nil || spySpot <= 0 {
		return combined, nil
	}
	gap := (*zero - spySpot) / spySpot * 100
	return combined, &gap
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// computeGammaCombined runs SPY then SPX serially under separate
// underlying-holds, computes their 20-day correlation, and combines
// the two halves into one envelope.
//
// Underlying-hold transition (per design §7.1 audit checklist item 6):
// each half's Hold is scoped to its own function call so a panic or
// error in the SPX half cannot leak the SPY hold. The two halves'
// holds DO NOT overlap — SPY releases at SPY-phase end before SPX
// acquires. This bounds market-data subscription footprint to one
// underlying at a time.
func computeGammaCombined(
	bgCtx context.Context,
	s *Server,
	c *ibkrlib.Connector,
	params rpc.GammaZeroParams,
	prog *atomic.Int32,
) (*rpc.GammaZeroComputed, error) {
	// Phase 1: SPY.
	spyRes, err := runUnderlyingPhase(bgCtx, s, c, "SPY", params, prog, 0)
	if err != nil {
		return nil, fmt.Errorf("zero-gamma: SPY phase: %w", err)
	}

	// Phase 2: SPX. Entitlement-graceful degradation per design §8.2:
	// if SPX errors (most commonly 354 "not subscribed"; or 30s
	// early-abort with no legs), we DON'T fail the combined run.
	// Instead, we degrade to SPY-only with a structured warning and
	// flip the result's Scope back to "spy" so the renderer surfaces
	// the SPX-skipped banner at the top of the output. SPY-only users
	// must not be regressed by the SPX path.
	spxRes, spxErr := runUnderlyingPhase(bgCtx, s, c, "SPX", params, prog, 50)
	if spxErr != nil {
		if s != nil && s.logger != nil {
			s.logger.Warnf("gamma.combine.spx_unavailable err=%v (degrading to SPY-only)", spxErr)
		}
		spyRes.Warnings = append(spyRes.Warnings, "spx_unavailable:"+summarizeSPXFailure(spxErr))
		// Keep Scope as the single-underlying "spy" — the combined
		// envelope hasn't been built. SPY headline numbers stand.
		return spyRes, nil
	}

	// Correlation gate. Failure to compute is non-fatal (nil corr +
	// nil err pair means "couldn't compute"; a real error logs but
	// doesn't block the headline).
	corr, corrErr := compute20DaySPYSPXCorrelation(c)
	if corrErr != nil {
		if s != nil && s.logger != nil {
			s.logger.Warnf("gamma.combine.corr_fetch_failed err=%v (proceeding without decoupled gate)", corrErr)
		}
		corr = nil
	}

	combined := combineGammaResults(spyRes, spxRes, corr)
	if combined == nil {
		return nil, fmt.Errorf("zero-gamma: combine produced nil result")
	}
	return combined, nil
}

// summarizeSPXFailure turns an SPX-phase error into the short token
// the warning-list embeds. Strips the verbose context that's helpful
// in logs but noisy in the renderer banner. Looks for the canonical
// IBKR error code in the message; falls back to "unavailable" for
// non-IBKR errors (gateway disconnect, ctx cancel).
//
// Token formats:
//
//	354       → entitlement gap (most common)
//	200       → contract not found / SPX chain restricted
//	timeout   → 30s early-abort with no legs
//	<other>   → truncated error message, ≤ 60 chars
func summarizeSPXFailure(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "354"):
		return "354"
	case strings.Contains(msg, " 200 ") || strings.Contains(msg, "no security definition"):
		return "200"
	case strings.Contains(msg, "no option data landed"):
		return "no_data"
	case strings.Contains(msg, "throttled"):
		return "throttled"
	}
	// Trim leading "zero-gamma: " jargon and cap length.
	msg = strings.TrimPrefix(msg, "zero-gamma: ")
	if len(msg) > 60 {
		msg = msg[:57] + "..."
	}
	// Replace any embedded colon to keep the warning token parseable.
	msg = strings.ReplaceAll(msg, ":", "·")
	return msg
}

// runUnderlyingPhase wraps one (Hold underlying → computeGammaZeroFor →
// release underlying) cycle. Progress baseline is the starting %
// (0 for SPY phase, 50 for SPX phase) so the existing 0-100 atomic
// reports cleanly across both halves.
func runUnderlyingPhase(
	bgCtx context.Context,
	s *Server,
	c *ibkrlib.Connector,
	underlying string,
	params rpc.GammaZeroParams,
	prog *atomic.Int32,
	progressBase int32,
) (*rpc.GammaZeroComputed, error) {
	if s == nil {
		return nil, fmt.Errorf("server is nil")
	}
	release, err := s.subs.Hold(bgCtx, underlying)
	if err != nil {
		return nil, fmt.Errorf("hold %s underlying: %w", underlying, err)
	}
	defer release()

	// Per-phase progress: each phase contributes 0..50 to the global
	// 0..100 atomic. Inner compute writes 0..100; we wrap and rescale.
	innerProg := &atomic.Int32{}
	go func() {
		// Poll the inner progress and rebase. Cheap (a few times per
		// second is enough) — the renderer reads progress at most once
		// per RPC call.
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-t.C:
				inner := innerProg.Load()
				if inner <= 0 {
					continue
				}
				prog.Store(progressBase + inner/2)
				if inner >= 100 {
					return
				}
			}
		}
	}()

	var logger gammaLogger
	if s != nil {
		logger = s.logger
	}
	return computeGammaZeroFor(bgCtx, c, underlying, params, productionLegFetcher, time.Now, innerProg, logger)
}

// gammaScopeForRequest maps the requested scope onto the actual
// scope the daemon will compute. Empty defaults to combined (the new
// canonical headline) once step 7 has both halves wired; consumers
// passing "spy" or "spx" get single-underlying. Unknown scopes
// surface as an error to the caller.
func gammaScopeForRequest(scope string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "":
		// Default lifts to combined now that step 7 is in place.
		return rpc.GammaZeroScopeCombined, nil
	case rpc.GammaZeroScopeSPY, rpc.GammaZeroScopeSPX, rpc.GammaZeroScopeCombined:
		return strings.ToLower(scope), nil
	default:
		return "", fmt.Errorf("unknown scope %q (want spy|spx|spy+spx)", scope)
	}
}
