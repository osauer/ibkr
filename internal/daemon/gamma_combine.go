package daemon

import (
	"context"
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
//     gamma regime (both long-γ / both short-γ / both flipping) or
//     disagree (one stabilising, one amplifying). The disagree case
//     is the actionable signal — it flags institutional vs retail/ETF
//     positioning divergence that the per-index breakdown otherwise
//     buries.
//   - GammaTotalAbs: the sum across both indices, framed as "total
//     dealer-book size" — a diagnostic, not a regime headline.
//   - TopStrikes: merged + sorted + top-K. With the 100× per-contract
//     scaling SPX rows will dominate; the renderer's INDEX column
//     makes the imbalance visible rather than hidden.
//   - Profile / ProfileNear / ProfileTerm: per-bucket sum of GEX on
//     the SHARED spot grid via combineProfileBuckets. SPY (~540) and
//     SPX (~5400) sit on different absolute spot scales today so the
//     helper bails with a `combined_profile_grid_mismatch` warning
//     and leaves the field nil — consumers needing a real curve must
//     recurse on PerIndex. The summed path exists for future
//     same-grid combinations.
//   - Expirations, LegCount, DerivedIVLegs: unioned/summed for the
//     diagnostic footer.
//   - Warnings: unioned across spy + spx then deduped; the profile
//     mismatch warnings noted above land here. Entitlement-graceful
//     path may append "spx_unavailable:<reason>" before we get here
//     (in which case this function never runs and computeGammaCombined
//     returns the SPY-only result directly).
//
// SPY-only fields that come along by shallow copy and are NOT
// re-derived for the combined headline (SpotUnderlying, SpotAt,
// ZeroGamma, GapPct, GammaSign*, SweepLow/HighAbs, Zero/Sign*Near,
// Near/TermLegCount, Zero/Sign*Term, SkewModel, SkewFitQuality,
// Params, Method, PartialClasses) are documented in the field-by-
// field intent map immediately above the shallow copy below.
// Renderers reading the combined envelope should pull these from
// PerIndex["SPY"] / PerIndex["SPX"] rather than trusting the
// top-level scalars.
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

	// Shallow-copy SPY onto out, then override the combined-specific
	// fields below. Field-by-field intent map for any future reader:
	//
	//   COMBINED (overridden below — safe to read off `out`):
	//     Scope, GammaTotalAbs, TopStrikes, TopConcentrationPct,
	//     LegCount, DerivedIVLegs, Expirations, Warnings, Source,
	//     RegimeAgreement, PerIndex, DurationMS, AsOf,
	//     Profile, ProfileNear, ProfileTerm.
	//
	//   SPY-ONLY (carried from `out := *spy` shallow copy — DO NOT
	//   consume these as "combined" values; they are the SPY-half
	//   reading and a future post-1.0 CombinedGammaZeroComputed type
	//   will hide them entirely from this shape):
	//     SpotUnderlying, SpotAt, ZeroGamma, GapPct, GammaSign,
	//     SweepLowAbs, SweepHighAbs,
	//     ZeroGammaNear, GammaSignNear, NearLegCount,
	//     ZeroGammaTerm, GammaSignTerm, TermLegCount,
	//     SkewModel, SkewFitQuality, Params, Method, PartialClasses.
	//
	// Renderers reading the combined headline should pull the
	// per-index numbers from PerIndex["SPY"] / PerIndex["SPX"]
	// instead of trusting SpotUnderlying / ZeroGamma / GammaSign on
	// the envelope.
	out := *spy
	out.Scope = rpc.GammaZeroScopeCombined
	// Mark the shallow-copy on the wire so consumers can detect that
	// SpotUnderlying / ZeroGamma / GammaSign / Profile / the per-bucket
	// triples are SPY-anchored rather than truly combined. See
	// rpc.GammaZeroComputed doc-comment for the full field-by-field map
	// and consumer guidance.
	out.SpotAnchor = "SPY"
	out.GammaTotalAbs = combinedAbs
	out.TopStrikes = allTop
	out.TopConcentrationPct = topConcPct
	out.LegCount = spy.LegCount + spx.LegCount
	out.DerivedIVLegs = spy.DerivedIVLegs + spx.DerivedIVLegs
	out.Expirations = dedupeStrings(append(append([]string{}, spy.Expirations...), spx.Expirations...))
	sort.Strings(out.Expirations)

	// Profile combination: replace the shallow-copied SPY-half
	// profiles with a per-bucket sum across the shared spot grid.
	// SPY (spot ~540) and SPX (spot ~5400) sit on radically different
	// absolute spot scales, so in practice the grids almost always
	// differ — combineProfileBuckets bails with a warning in that
	// case and returns nil, which is the honest answer (a renderer
	// that needs a per-index profile can recurse on PerIndex). The
	// near/term profiles get the same treatment.
	combinedWarnings := append([]string{}, spy.Warnings...)
	combinedWarnings = append(combinedWarnings, spx.Warnings...)
	out.Profile, combinedWarnings = combineProfileBuckets(spy.Profile, spx.Profile, "combined_profile_grid_mismatch", combinedWarnings)
	out.ProfileNear, combinedWarnings = combineProfileBuckets(spy.ProfileNear, spx.ProfileNear, "combined_profile_near_grid_mismatch", combinedWarnings)
	out.ProfileTerm, combinedWarnings = combineProfileBuckets(spy.ProfileTerm, spx.ProfileTerm, "combined_profile_term_grid_mismatch", combinedWarnings)
	out.Warnings = dedupeStrings(combinedWarnings)

	out.Source = "computed from IBKR SPY+SPX option chains"
	out.RegimeAgreement = classifyRegimeAgreement(spy, spx)
	out.PerIndex = map[string]*rpc.GammaZeroComputed{
		"SPY": spy,
		"SPX": spx,
	}
	out.DurationMS = spy.DurationMS + spx.DurationMS
	if spx.AsOf.After(spy.AsOf) {
		out.AsOf = spx.AsOf
	}
	return &out
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
		return nil, append(warnings, mismatchWarn)
	}
	for i := range a {
		if a[i].Spot != b[i].Spot {
			return nil, append(warnings, mismatchWarn)
		}
	}
	out := make([]rpc.GammaProfilePoint, len(a))
	for i := range a {
		out[i] = rpc.GammaProfilePoint{Spot: a[i].Spot, GEX: a[i].GEX + b[i].GEX}
	}
	return out, warnings
}

// classifyRegimeAgreement labels the SPY/SPX regime relationship by
// comparing per-index γ-zero sweep outcomes. Returns one of
// "agree:long-gamma", "agree:short-gamma", "agree:flipping",
// "disagree", or "" (unknown — at least one bucket has no_data).
//
// The classification reads GammaSign + ZeroGamma rather than fetching
// any external state:
//
//	per-index regime ∈ { long-gamma, short-gamma, flipping, no-data }
//	  long-gamma:  GammaSign == "positive"    (whole sweep > 0)
//	  short-gamma: GammaSign == "negative"    (whole sweep < 0)
//	  flipping:    ZeroGamma != nil           (crossing inside window)
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
		return "flipping"
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
// Entitlement-graceful degradation (per design §8.2): on SPX-phase
// failure (354 entitlement, 200 contract, 30s no-data, etc.) the
// function returns the SPY-only result with a structured warning
// rather than failing the run.
func computeGammaCombined(
	bgCtx context.Context,
	s *Server,
	c *ibkrlib.Connector,
	params rpc.GammaZeroParams,
	prog *atomic.Int32,
) (*rpc.GammaZeroComputed, error) {
	spyRes, err := runUnderlyingPhase(bgCtx, s, c, "SPY", params, prog, 0)
	if err != nil {
		return nil, fmt.Errorf("zero-gamma: SPY phase: %w", err)
	}

	spxRes, spxErr := runUnderlyingPhase(bgCtx, s, c, "SPX", params, prog, 50)
	if spxErr != nil {
		if s != nil && s.logger != nil {
			s.logger.Warnf("gamma.combine.spx_unavailable err=%v (degrading to SPY-only)", spxErr)
		}
		spyRes.Warnings = append(spyRes.Warnings, "spx_unavailable:"+summarizeSPXFailure(spxErr))
		return spyRes, nil
	}

	combined := combineGammaResults(spyRes, spxRes)
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
