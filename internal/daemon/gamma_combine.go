package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// combineGammaResults builds the SPY+SPX result envelope from the two
// single-underlying GammaZeroComputed payloads.
//
// What it DOES surface:
//
//   - PerIndex: the two single-underlying GammaZeroComputed payloads,
//     each fully formed with their own ZeroGamma / GapPct / Profile.
//     This is the load-bearing decision surface for the user.
//   - RegimeAgreement: classifies whether SPY and SPX agree on the
//     gamma regime (long-γ / short-γ / transition) or disagree
//     (one stabilising, one amplifying).
//   - GammaTotalAbs: the sum across both indices, framed as "total
//     dealer-book size" — a diagnostic, not a regime headline.
//   - TopStrikes: merged + sorted + top-K. With the 100× per-contract
//     scaling SPX rows will dominate; the renderer's INDEX column
//     makes the imbalance visible rather than hidden.
//   - Expirations, LegCount, PricedLegCount, DerivedIVLegs, and
//     LegDiagnostics: unioned / summed for the diagnostic footer.
//   - Warnings: unioned across spy + spx then deduped. They hydrate into
//     WarningDetails before the result leaves the cache.
//
// What it DOES NOT surface: a top-level spot, gamma sign, or zero-gamma
// price. SPY and SPX live on different price scales, so those fields are
// per-index only in combined scope.
func combineGammaResults(spy, spx *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
	if spy == nil && spx == nil {
		return nil
	}
	// One-sided fallbacks. The entitlement-graceful path in
	// computeGammaCombined returns the SPY-only result directly when
	// SPX errors, so these branches are defensive — they should not
	// fire on a healthy combined run.
	if spy == nil {
		return spx
	}
	if spx == nil {
		return spy
	}

	// Top strikes: merge, sort by AbsGEX descending, take top-K. SPX
	// rows will dominate per the spot² scaling; the renderer surfaces
	// the INDEX column so the imbalance is visible.
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

	asOf := spy.AsOf
	if spx.AsOf.After(asOf) {
		asOf = spx.AsOf
	}
	method := spy.Method
	if method == "" {
		method = spx.Method
	}
	convention := spy.GammaTotalAbsConvention
	if convention == "" {
		convention = spx.GammaTotalAbsConvention
	}
	citations := spy.MethodologyCitations
	if len(citations) == 0 {
		citations = spx.MethodologyCitations
	}
	params := spy.Params

	out := &rpc.GammaZeroComputed{
		Scope:                   rpc.GammaZeroScopeCombined,
		GammaTotalAbs:           combinedAbs,
		GammaTotalAbsConvention: convention,
		TopStrikes:              allTop,
		TopConcentrationPct:     topConcPct,
		LegCount:                spy.LegCount + spx.LegCount,
		PricedLegCount:          spy.PricedLegCount + spx.PricedLegCount,
		DerivedIVLegs:           spy.DerivedIVLegs + spx.DerivedIVLegs,
		LegDiagnostics:          combineGammaLegDiagnostics(spy.LegDiagnostics, spx.LegDiagnostics),
		Expirations:             dedupeStrings(append(append([]string{}, spy.Expirations...), spx.Expirations...)),
		Params:                  params,
		Source:                  "computed from IBKR SPY+SPX option chains",
		Method:                  method,
		MethodologyCitations:    citations,
		AsOf:                    asOf,
		DurationMS:              spy.DurationMS + spx.DurationMS,
		RegimeAgreement:         classifyRegimeAgreement(spy, spx),
		PerIndex: map[string]*rpc.GammaZeroComputed{
			"SPY": spy,
			"SPX": spx,
		},
	}
	sort.Strings(out.Expirations)

	// A combined SPY+SPX gamma-zero level is intentionally not
	// interpolated: the two underlyings live on different spot scales.
	// Per-index profiles remain available under PerIndex.
	if len(spy.Profile) > 0 && len(spx.Profile) > 0 && sameProfileGrid(spy.Profile, spx.Profile) {
		var combinedWarnings []string
		out.Profile, combinedWarnings = combineProfileBuckets(spy.Profile, spx.Profile, "", nil)
		out.Warnings = dedupeStrings(append(out.Warnings, combinedWarnings...))
	}
	return out
}

// combineProfileBuckets sums the GEX values of two sweep profiles
// bucket-by-bucket on the assumption they share the same Spot grid.
// Returns nil + a warning appended to warnings when:
//   - the two lengths differ;
//   - any pair of corresponding Spot values is not exactly equal;
//   - either side is empty (no useful sum is possible).
//
// The exact-equality spot check is intentional: dealer GEX has no
// natural interpretation across spot scales, so any drift means we
// can't be sure the buckets represent the same scenario, and an
// incorrect sum is worse than a missing one. In SPY+SPX production
// the grids will always differ (SPY anchors ~540, SPX anchors ~5400)
// so this path is effectively "bail with a warning" today. The
// summed path exists for future per-index combinations where the
// grids align by construction (e.g. two trading classes of the same
// underlying).
func combineProfileBuckets(a, b []rpc.GammaProfilePoint, mismatchWarn string, warnings []string) ([]rpc.GammaProfilePoint, []string) {
	if len(a) == 0 || len(b) == 0 {
		return nil, warnings
	}
	if len(a) != len(b) {
		if mismatchWarn == "" {
			return nil, warnings
		}
		return nil, append(warnings, mismatchWarn)
	}
	for i := range a {
		if a[i].Spot != b[i].Spot {
			if mismatchWarn == "" {
				return nil, warnings
			}
			return nil, append(warnings, mismatchWarn)
		}
	}
	out := make([]rpc.GammaProfilePoint, len(a))
	for i := range a {
		out[i] = rpc.GammaProfilePoint{Spot: a[i].Spot, GEX: a[i].GEX + b[i].GEX}
	}
	return out, warnings
}

func sameProfileGrid(a, b []rpc.GammaProfilePoint) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Spot != b[i].Spot {
			return false
		}
	}
	return true
}

// classifyRegimeAgreement labels the SPY/SPX regime relationship by
// comparing per-index γ-zero sweep outcomes. Returns one of
// "agree:long-gamma", "agree:short-gamma", "agree:transition-gamma",
// "disagree", or "" (unknown — at least one bucket has no_data).
//
// The classification reads GammaSign plus the zero-gamma gap rather
// than fetching any external state:
//
//	per-index regime ∈ { long-gamma, short-gamma, transition-gamma, no-data }
//	  long-gamma:  GammaSign == "positive"    (whole sweep > 0)
//	  short-gamma: GammaSign == "negative"    (whole sweep < 0)
//	  transition:  ZeroGamma != nil and spot is within ±2% of it
//	  no-data:     GammaSign == "no_data" or anything else
//
// disagree fires whenever the two indices land in different non-no_data
// regimes — the actionable case where one book is amplifying while the
// other is stabilizing, regardless of whether the underlying prices
// happen to be correlated.
func classifyRegimeAgreement(spy, spx *rpc.GammaZeroComputed) string {
	spyR := perIndexRegime(spy)
	spxR := perIndexRegime(spx)
	if spyR == "" || spxR == "" {
		return ""
	}
	if spyR != spxR {
		return "disagree"
	}
	return "agree:" + spyR
}

// perIndexRegime maps a single-underlying GammaZeroComputed to a
// regime label. Returns "" on no-data or nil input.
func perIndexRegime(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	if c.ZeroGamma != nil {
		return strings.ReplaceAll(gammaRegimeFromGap(c.GapPct), "_", "-")
	}
	switch c.GammaSign {
	case "positive":
		return "long-gamma"
	case "negative":
		return "short-gamma"
	}
	return ""
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
// underlying-holds and combines the two halves into one envelope.
//
// Underlying-hold transition (per design §7.1 audit checklist item 6):
// each half's Hold is scoped to its own function call so a panic or
// error in the SPX half cannot leak the SPY hold. The two halves'
// holds DO NOT overlap — SPY releases at SPY-phase end before SPX
// acquires. This bounds market-data subscription footprint to one
// underlying at a time.
//
// Availability-graceful degradation (per design §8.2): if one side
// fails because the gateway could not produce a usable OI/IV/GEX slice
// (354 entitlement, 200 contract, no-data, zero magnitude, etc.) but
// the other side succeeds, return the successful side with a structured
// warning rather than failing the whole default/regime gamma row.
func computeGammaCombined(
	bgCtx context.Context,
	s *Server,
	c *ibkrlib.Connector,
	params rpc.GammaZeroParams,
	prog *atomic.Int32,
) (*rpc.GammaZeroComputed, error) {
	spyRes, err := runUnderlyingPhase(bgCtx, s, c, "SPY", params, prog, 0)
	if err != nil {
		if s != nil && s.logger != nil {
			s.logger.Warnf("gamma.combine.spy_unavailable err=%v (trying SPX-only)", err)
		}
		spxRes, spxErr := runUnderlyingPhase(bgCtx, s, c, "SPX", params, prog, 50)
		if spxErr != nil {
			return nil, fmt.Errorf("zero-gamma: SPY phase: %w; SPX phase: %w", err, spxErr)
		}
		spxRes.Warnings = append(spxRes.Warnings, "spy_unavailable:"+summarizeGammaPhaseFailure(err))
		return hydrateGammaComputed(spxRes), nil
	}

	spxRes, spxErr := runUnderlyingPhase(bgCtx, s, c, "SPX", params, prog, 50)
	if spxErr != nil {
		if s != nil && s.logger != nil {
			s.logger.Warnf("gamma.combine.spx_unavailable err=%v (degrading to SPY-only)", spxErr)
		}
		spyRes.Warnings = append(spyRes.Warnings, "spx_unavailable:"+summarizeGammaPhaseFailure(spxErr))
		return hydrateGammaComputed(spyRes), nil
	}

	combined := combineGammaResults(spyRes, spxRes)
	if combined == nil {
		return nil, fmt.Errorf("zero-gamma: combine produced nil result")
	}
	return hydrateGammaComputed(combined), nil
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
//	fetch_canceled → compute context was cancelled before SPX landed
//	timeout        → deadline / timeout before SPX landed
//	zero_magnitude → legs landed but all gamma magnitude was zero
//	<other>   → truncated error message, ≤ 60 chars
func summarizeSPXFailure(err error) string {
	return summarizeGammaPhaseFailure(err)
}

func summarizeGammaPhaseFailure(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "354"):
		return "354"
	case strings.Contains(msg, " 200 ") || strings.Contains(msg, "no security definition"):
		return "200"
	case errors.Is(err, context.Canceled), strings.Contains(lower, "context canceled"), strings.Contains(lower, "context cancelled"):
		return "fetch_canceled"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(lower, "context deadline exceeded"),
		strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"):
		return "timeout"
	case strings.Contains(msg, "no option data landed"):
		return "no_data"
	case strings.Contains(msg, "throttled"):
		return "throttled"
	case strings.Contains(msg, "no usable GEX legs"), strings.Contains(msg, "zero gamma_total_abs/profile/top_strikes"):
		return "zero_magnitude"
	}
	// Trim leading "zero-gamma: " jargon and cap length.
	msg = strings.TrimPrefix(msg, "zero-gamma: ")
	if len(msg) > 60 {
		msg = msg[:57] + "..."
	}
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

	innerProg := &atomic.Int32{}
	go func() {
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
// scope the daemon will compute. Empty defaults to combined.
// Unknown scopes surface as an error to the caller.
func gammaScopeForRequest(scope string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "":
		return rpc.GammaZeroScopeCombined, nil
	case rpc.GammaZeroScopeSPY, rpc.GammaZeroScopeSPX, rpc.GammaZeroScopeCombined:
		return strings.ToLower(scope), nil
	default:
		return "", fmt.Errorf("unknown scope %q (want spy|spx|spy+spx)", scope)
	}
}
