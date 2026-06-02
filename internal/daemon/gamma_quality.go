package daemon

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

const (
	gammaRankableRTHMaxAge       = 60 * time.Minute
	gammaRankableExtendedMaxAge  = 30 * time.Minute
	gammaContextClosedMaxAge     = 24 * time.Hour
	gammaMinPricedLegs           = 100
	gammaMinGEXLegs              = 25
	gammaMinSPXOIObservedPct     = 95.0
	gammaMinDefaultOIObservedPct = 75.0
	gammaMinOIPositivePct        = 5.0
	gammaBlockOIPositivePct      = 1.0
	gammaContextDerivedIVPct     = 40.0
	gammaBlockDerivedIVPct       = 80.0
	gammaContextConcentrationPct = 50.0
	gammaBlockConcentrationPct   = 90.0
	gammaContextSkewRSquared     = 0.75
	gammaBlockSkewRSquared       = 0.50
)

func annotateGammaQuality(c *rpc.GammaZeroComputed, now time.Time) {
	if c == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	for _, sub := range c.PerIndex {
		annotateGammaQuality(sub, now)
	}
	q := buildGammaSignalQuality(c, now)
	c.Quality = &q
}

func refreshGammaSummaries(c *rpc.GammaZeroComputed) {
	if c == nil {
		return
	}
	for _, sub := range c.PerIndex {
		refreshGammaSummaries(sub)
	}
	c.Summary = buildGammaSummary(c)
}

func buildGammaSignalQuality(c *rpc.GammaZeroComputed, now time.Time) rpc.GammaSignalQuality {
	q := rpc.GammaSignalQuality{
		Rankability: rpc.GammaRankabilityRankable,
		AsOf:        time.Time{},
		Session:     rpc.ClassifySession(now).String(),
	}
	if c == nil {
		q.Rankability = rpc.GammaRankabilityUnavailable
		q.RankabilityReason = "gamma payload is missing"
		return q
	}
	q.AsOf = c.AsOf
	q.SessionKey = gammaQualitySessionKey(c.AsOf)
	q.CurrentSessionKey = nySessionKey(now)
	q.Coverage = gammaQualityCoverage(c)
	if !c.AsOf.IsZero() && now.After(c.AsOf) {
		q.AgeSeconds = int64(now.Sub(c.AsOf).Seconds())
	}

	gammaQualityFreshnessGate(&q, c, now)
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		gammaQualityCombinedGates(&q, c)
	} else {
		gammaQualitySingleGates(&q, c)
	}
	gammaQualityWarningGates(&q, c)
	gammaFinalizeRankability(&q)
	return q
}

func gammaQualityFreshnessGate(q *rpc.GammaSignalQuality, c *rpc.GammaZeroComputed, now time.Time) {
	if c.AsOf.IsZero() {
		gammaQualityAddGate(q, "freshness", rpc.GammaQualityGateBlock, "compute timestamp missing")
		q.Freshness = "missing"
		return
	}
	session := rpc.ClassifySession(now)
	switch session {
	case rpc.SessionClosed:
		q.MaxAgeSeconds = int64(gammaContextClosedMaxAge.Seconds())
		switch {
		case now.Before(c.AsOf):
			q.Freshness = "future"
			gammaQualityAddGate(q, "freshness", rpc.GammaQualityGateBlock, "compute timestamp is in the future")
		case now.Sub(c.AsOf) > gammaContextClosedMaxAge:
			q.Freshness = "stale"
			gammaQualityAddGate(q, "freshness", rpc.GammaQualityGateBlock, "closed-session cache is older than 24h")
		default:
			q.Freshness = "closed_session_context"
			gammaQualityAddGate(q, "freshness", rpc.GammaQualityGateContext, "market is closed; cached gamma is context only")
		}
	case rpc.SessionRTH:
		q.MaxAgeSeconds = int64(gammaRankableRTHMaxAge.Seconds())
		gammaQualityActiveSessionFreshnessGate(q, c, now, gammaRankableRTHMaxAge)
	default:
		q.MaxAgeSeconds = int64(gammaRankableExtendedMaxAge.Seconds())
		gammaQualityActiveSessionFreshnessGate(q, c, now, gammaRankableExtendedMaxAge)
	}
}

func gammaQualityActiveSessionFreshnessGate(q *rpc.GammaSignalQuality, c *rpc.GammaZeroComputed, now time.Time, maxAge time.Duration) {
	switch {
	case q.SessionKey != q.CurrentSessionKey:
		q.Freshness = "session_mismatch"
		gammaQualityAddGate(q, "freshness", rpc.GammaQualityGateBlock,
			fmt.Sprintf("computed for session %s; current session is %s", q.SessionKey, q.CurrentSessionKey))
	case now.Sub(c.AsOf) > maxAge:
		q.Freshness = "stale"
		gammaQualityAddGate(q, "freshness", rpc.GammaQualityGateBlock,
			fmt.Sprintf("cache age %s exceeds %s", formatGammaQualityDuration(now.Sub(c.AsOf)), formatGammaQualityDuration(maxAge)))
	default:
		q.Freshness = "fresh"
		gammaQualityAddGate(q, "freshness", rpc.GammaQualityGatePass, "same session and inside freshness TTL")
	}
}

func gammaQualityCombinedGates(q *rpc.GammaSignalQuality, c *rpc.GammaZeroComputed) {
	q.ByUnderlying = make(map[string]rpc.GammaSignalQuality, len(c.PerIndex))
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		if sub == nil || sub.Quality == nil {
			continue
		}
		q.ByUnderlying[key] = *sub.Quality
	}
	spy := c.PerIndex["SPY"]
	spx := c.PerIndex["SPX"]
	switch {
	case spx == nil:
		gammaQualityAddGate(q, "spx_coverage", rpc.GammaQualityGateBlock, "SPX slice missing; combined gamma cannot rank")
	case spx.Quality == nil:
		gammaQualityAddGate(q, "spx_coverage", rpc.GammaQualityGateBlock, "SPX quality missing")
	case spx.Quality.Rankability == rpc.GammaRankabilityRankable:
		gammaQualityAddGate(q, "spx_coverage", rpc.GammaQualityGatePass, "SPX slice rankable")
	case spx.Quality.Rankability == rpc.GammaRankabilityContextOnly:
		gammaQualityAddGate(q, "spx_coverage", rpc.GammaQualityGateContext, "SPX slice is context only")
	default:
		gammaQualityAddGate(q, "spx_coverage", rpc.GammaQualityGateBlock, "SPX slice is not rankable")
	}
	switch {
	case spy == nil:
		gammaQualityAddGate(q, "spy_coverage", rpc.GammaQualityGateContext, "SPY slice missing; combined view is SPX-only context")
	case spy.Quality == nil:
		gammaQualityAddGate(q, "spy_coverage", rpc.GammaQualityGateContext, "SPY quality missing")
	case spy.Quality.Rankability == rpc.GammaRankabilityRankable:
		gammaQualityAddGate(q, "spy_coverage", rpc.GammaQualityGatePass, "SPY slice rankable")
	default:
		gammaQualityAddGate(q, "spy_coverage", rpc.GammaQualityGateContext, "SPY slice is not rankable")
	}
	gammaQualityCommonModelGates(q, c)
}

func gammaQualitySingleGates(q *rpc.GammaSignalQuality, c *rpc.GammaZeroComputed) {
	cov := q.Coverage
	if cov.PricedLegs == 0 || cov.GEXLegs == 0 || c.GammaTotalAbs <= 0 {
		q.Rankability = rpc.GammaRankabilityUnavailable
		gammaQualityAddGate(q, "payload", rpc.GammaQualityGateBlock, "no usable OI-weighted gamma exposure")
		return
	}
	if cov.PricedLegs < gammaMinPricedLegs {
		gammaQualityAddGate(q, "priced_leg_coverage", rpc.GammaQualityGateBlock,
			fmt.Sprintf("%d priced legs; need at least %d", cov.PricedLegs, gammaMinPricedLegs))
	} else {
		gammaQualityAddGate(q, "priced_leg_coverage", rpc.GammaQualityGatePass, "priced leg coverage sufficient")
	}
	if cov.GEXLegs < gammaMinGEXLegs {
		gammaQualityAddGate(q, "gex_leg_coverage", rpc.GammaQualityGateBlock,
			fmt.Sprintf("%d OI-weighted GEX legs; need at least %d", cov.GEXLegs, gammaMinGEXLegs))
	} else {
		gammaQualityAddGate(q, "gex_leg_coverage", rpc.GammaQualityGatePass, "OI-weighted GEX leg count sufficient")
	}
	threshold := gammaOIObservedThreshold(c)
	if cov.OIObservedPct < threshold {
		status := rpc.GammaQualityGateBlock
		reason := fmt.Sprintf("OI observed on %.1f%% of priced legs; need %.0f%%", cov.OIObservedPct, threshold)
		if gammaQualityScope(c) == "SPY" && rpc.ClassifySession(c.AsOf) != rpc.SessionRTH && cov.GEXLegs >= gammaMinGEXLegs {
			status = rpc.GammaQualityGateContext
			reason += " for rankability; sparse SPY OI can occur outside regular option hours"
		}
		gammaQualityAddGate(q, "oi_observed_coverage", status, reason)
	} else {
		gammaQualityAddGate(q, "oi_observed_coverage", rpc.GammaQualityGatePass, "observed OI coverage sufficient")
	}
	switch {
	case cov.OIPositivePct < gammaBlockOIPositivePct:
		gammaQualityAddGate(q, "oi_positive_coverage", rpc.GammaQualityGateBlock,
			fmt.Sprintf("positive-OI GEX legs are %.1f%% of priced legs; OI-weighted magnitude is too sparse", cov.OIPositivePct))
	case cov.OIPositivePct < gammaMinOIPositivePct:
		gammaQualityAddGate(q, "oi_positive_coverage", rpc.GammaQualityGateContext,
			fmt.Sprintf("positive-OI GEX legs are %.1f%% of priced legs", cov.OIPositivePct))
	default:
		gammaQualityAddGate(q, "oi_positive_coverage", rpc.GammaQualityGatePass, "positive-OI GEX coverage sufficient")
	}
	gammaQualityHorizonGates(q, c)
	gammaQualityCommonModelGates(q, c)
}

func gammaQualityHorizonGates(q *rpc.GammaSignalQuality, c *rpc.GammaZeroComputed) {
	cov := q.Coverage
	if cov.Has0DTE {
		gammaQualityAddGate(q, "horizon_0dte", rpc.GammaQualityGatePass, "0DTE bucket present")
	} else {
		gammaQualityAddGate(q, "horizon_0dte", rpc.GammaQualityGateContext, "0DTE bucket missing")
	}
	if cov.Has1To7DTE {
		gammaQualityAddGate(q, "horizon_1to7", rpc.GammaQualityGatePass, "1-7 DTE bucket present")
	} else {
		gammaQualityAddGate(q, "horizon_1to7", rpc.GammaQualityGateContext, "1-7 DTE bucket missing")
	}
	if cov.HasTerm {
		gammaQualityAddGate(q, "horizon_term", rpc.GammaQualityGatePass, "term bucket present")
	} else {
		gammaQualityAddGate(q, "horizon_term", rpc.GammaQualityGateContext, "term bucket missing")
	}
}

func gammaQualityCommonModelGates(q *rpc.GammaSignalQuality, c *rpc.GammaZeroComputed) {
	cov := q.Coverage
	switch {
	case cov.DerivedIVPct >= gammaBlockDerivedIVPct:
		gammaQualityAddGate(q, "derived_iv_share", rpc.GammaQualityGateBlock,
			fmt.Sprintf("%.1f%% of priced legs used derived IV", cov.DerivedIVPct))
	case cov.DerivedIVPct >= gammaContextDerivedIVPct:
		gammaQualityAddGate(q, "derived_iv_share", rpc.GammaQualityGateContext,
			fmt.Sprintf("%.1f%% of priced legs used derived IV", cov.DerivedIVPct))
	default:
		gammaQualityAddGate(q, "derived_iv_share", rpc.GammaQualityGatePass, "derived-IV share acceptable")
	}
	switch {
	case cov.TopConcentrationPct >= gammaBlockConcentrationPct:
		gammaQualityAddGate(q, "top_strike_concentration", rpc.GammaQualityGateBlock,
			fmt.Sprintf("top strike concentration %.1f%% is too dominant", cov.TopConcentrationPct))
	case cov.TopConcentrationPct >= gammaContextConcentrationPct:
		gammaQualityAddGate(q, "top_strike_concentration", rpc.GammaQualityGateContext,
			fmt.Sprintf("top strike concentration %.1f%% is high", cov.TopConcentrationPct))
	default:
		gammaQualityAddGate(q, "top_strike_concentration", rpc.GammaQualityGatePass, "top-strike concentration acceptable")
	}
	if cov.SkewFitExpiries == 0 {
		gammaQualityAddGate(q, "skew_fit_quality", rpc.GammaQualityGateContext, "skew-fit diagnostics unavailable")
		return
	}
	switch {
	case cov.MedianSkewRSquared < gammaBlockSkewRSquared:
		gammaQualityAddGate(q, "skew_fit_quality", rpc.GammaQualityGateBlock,
			fmt.Sprintf("median skew-fit R2 %.2f below %.2f", cov.MedianSkewRSquared, gammaBlockSkewRSquared))
	case cov.MedianSkewRSquared < gammaContextSkewRSquared:
		gammaQualityAddGate(q, "skew_fit_quality", rpc.GammaQualityGateContext,
			fmt.Sprintf("median skew-fit R2 %.2f below %.2f", cov.MedianSkewRSquared, gammaContextSkewRSquared))
	default:
		gammaQualityAddGate(q, "skew_fit_quality", rpc.GammaQualityGatePass, "skew fit quality acceptable")
	}
}

func gammaQualityWarningGates(q *rpc.GammaSignalQuality, c *rpc.GammaZeroComputed) {
	for _, raw := range gammaWarningCodes(c) {
		code := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case code == "", code == "no_crossing_in_window":
			continue
		case code == "strike_budget_capped":
			gammaQualityAddGate(q, "strike_budget", rpc.GammaQualityGatePass, "strike fan-out cap disclosed")
		case code == "cache_stale_off_hours":
			gammaQualityAddGate(q, "cache_state", rpc.GammaQualityGateBlock, "stale off-hours cache")
		case code == "throttled":
			gammaQualityAddGate(q, "gateway_pacing", rpc.GammaQualityGateBlock, "gateway throttled option fan-out")
		case code == "all_iv_derived":
			gammaQualityAddGate(q, "model_source", rpc.GammaQualityGateBlock, "all IVs were derived from quotes/closes")
		case code == "oi_missing":
			scope := gammaQualityScope(c)
			if scope == "SPX" {
				if q.Coverage.OIObservedPct >= gammaMinSPXOIObservedPct {
					gammaQualityAddGate(q, "oi_missing", rpc.GammaQualityGatePass, "SPX OI missing was within coverage tolerance")
				}
			} else if rpc.ClassifySession(c.AsOf) != rpc.SessionRTH {
				gammaQualityAddGate(q, "oi_missing", rpc.GammaQualityGateContext, "SPY OI missing outside regular option hours")
			}
		case strings.HasPrefix(code, "refresh_failed:"):
			gammaQualityAddGate(q, "refresh_state", rpc.GammaQualityGateBlock, "latest refresh failed: "+strings.TrimPrefix(code, "refresh_failed:"))
		case strings.HasPrefix(code, "spx_unavailable:"):
			gammaQualityAddGate(q, "spx_coverage", rpc.GammaQualityGateBlock, "SPX option chain unavailable: "+strings.TrimPrefix(code, "spx_unavailable:"))
		case strings.HasPrefix(code, "spy_unavailable:"):
			gammaQualityAddGate(q, "spy_coverage", rpc.GammaQualityGateContext, "SPY option chain unavailable: "+strings.TrimPrefix(code, "spy_unavailable:"))
		case strings.HasPrefix(code, "spx_cache_fallback"):
			gammaQualityAddGate(q, "spx_cache_fallback", rpc.GammaQualityGateContext, "SPX slice came from cached fallback")
		case strings.HasPrefix(code, "skew_fallback:"):
			gammaQualityAddGate(q, "skew_fallback", rpc.GammaQualityGateContext, "one expiry fell back to sticky-IV")
		case strings.Contains(code, "timeout"), strings.Contains(code, "pacing"), strings.Contains(code, "farm"):
			gammaQualityAddGate(q, "gateway_health", rpc.GammaQualityGateBlock, "gateway/data-farm warning: "+raw)
		}
	}
}

func gammaFinalizeRankability(q *rpc.GammaSignalQuality) {
	if q.Rankability == rpc.GammaRankabilityUnavailable {
		if q.RankabilityReason == "" {
			q.RankabilityReason = "gamma payload unavailable"
		}
		return
	}
	if len(q.Blockers) > 0 {
		q.Rankability = rpc.GammaRankabilityBlocked
		q.RankabilityReason = q.Blockers[0]
		return
	}
	if len(q.Context) > 0 {
		q.Rankability = rpc.GammaRankabilityContextOnly
		q.RankabilityReason = q.Context[0]
		return
	}
	q.Rankability = rpc.GammaRankabilityRankable
	q.RankabilityReason = "all rankability gates passed"
}

func gammaQualityCoverage(c *rpc.GammaZeroComputed) rpc.GammaQualityCoverage {
	if c == nil {
		return rpc.GammaQualityCoverage{}
	}
	priced := c.PricedLegCount
	if priced == 0 && c.LegCount > 0 {
		priced = c.LegCount
	}
	oiObserved := c.LegCount
	oiLiveObserved := c.LegCount
	oiCarried := 0
	oiPositive := c.LegCount
	gexLegs := c.LegCount
	if c.LegDiagnostics != nil {
		if c.LegDiagnostics.Total.PricedLegs > 0 {
			priced = c.LegDiagnostics.Total.PricedLegs
		}
		oiObserved = max(c.LegDiagnostics.Total.OpenInterestObservedLegs, c.LegDiagnostics.Total.OpenInterestLegs)
		oiLiveObserved = c.LegDiagnostics.Total.OILiveObservedLegs
		oiCarried = c.LegDiagnostics.Total.OICarriedForwardLegs
		if oiLiveObserved == 0 && oiCarried == 0 {
			oiLiveObserved = oiObserved
		}
		oiPositive = c.LegDiagnostics.Total.OpenInterestLegs
		if c.LegDiagnostics.Total.AbsGEXLegs > 0 {
			gexLegs = c.LegDiagnostics.Total.AbsGEXLegs
		}
	}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		has0, has1to7, hasTerm := false, false, false
		for _, sub := range c.PerIndex {
			sc := gammaQualityCoverage(sub)
			has0 = has0 || sc.Has0DTE
			has1to7 = has1to7 || sc.Has1To7DTE
			hasTerm = hasTerm || sc.HasTerm
		}
		out := gammaQualityCoverageNumbers(c, priced, oiObserved, oiPositive, gexLegs)
		out.OILiveObservedLegs = oiLiveObserved
		out.OICarriedForwardLegs = oiCarried
		out.OILiveObservedPct = percent(float64(oiLiveObserved), float64(priced))
		out.OICarriedForwardPct = percent(float64(oiCarried), float64(priced))
		out.Has0DTE = has0
		out.Has1To7DTE = has1to7
		out.HasTerm = hasTerm
		return out
	}
	out := gammaQualityCoverageNumbers(c, priced, oiObserved, oiPositive, gexLegs)
	out.OILiveObservedLegs = oiLiveObserved
	out.OICarriedForwardLegs = oiCarried
	out.OILiveObservedPct = percent(float64(oiLiveObserved), float64(priced))
	out.OICarriedForwardPct = percent(float64(oiCarried), float64(priced))
	out.Has0DTE = c.LegCount0DTE > 0 || c.ZeroGamma0DTE != nil || gammaSignUsable(c.GammaSign0DTE)
	out.Has1To7DTE = c.LegCount1to7 > 0 || c.ZeroGamma1to7 != nil || gammaSignUsable(c.GammaSign1to7)
	out.HasTerm = c.LegCountTerm > 0 || c.ZeroGammaTerm != nil || gammaSignUsable(c.GammaSignTerm)
	return out
}

func gammaQualityCoverageNumbers(c *rpc.GammaZeroComputed, priced, oiObserved, oiPositive, gexLegs int) rpc.GammaQualityCoverage {
	out := rpc.GammaQualityCoverage{
		PricedLegs:          priced,
		OIObservedLegs:      oiObserved,
		OIPositiveLegs:      oiPositive,
		GEXLegs:             gexLegs,
		DerivedIVPct:        percent(float64(c.DerivedIVLegs), float64(max(priced, 0))),
		TopConcentrationPct: c.TopConcentrationPct,
		ExpirationCount:     len(c.Expirations),
	}
	out.OIObservedPct = percent(float64(oiObserved), float64(priced))
	out.OIPositivePct = percent(float64(oiPositive), float64(priced))
	rs := gammaSkewRSquaredValues(c)
	if len(rs) > 0 {
		out.SkewFitExpiries = len(rs)
		slices.Sort(rs)
		out.MinSkewRSquared = rs[0]
		out.MedianSkewRSquared = medianSorted(rs)
	}
	return out
}

func gammaSkewRSquaredValues(c *rpc.GammaZeroComputed) []float64 {
	if c == nil {
		return nil
	}
	var out []float64
	for _, info := range c.SkewFitQuality {
		if info.Points > 0 {
			out = append(out, info.RSquared)
		}
	}
	if c.Scope == rpc.GammaZeroScopeCombined {
		for _, sub := range c.PerIndex {
			out = append(out, gammaSkewRSquaredValues(sub)...)
		}
	}
	return out
}

func gammaQualityAddGate(q *rpc.GammaSignalQuality, name, status, reason string) {
	q.Gates = append(q.Gates, rpc.GammaQualityGate{Name: name, Status: status, Reason: reason})
	text := name
	if reason != "" {
		text += ": " + reason
	}
	switch status {
	case rpc.GammaQualityGateBlock:
		if !slices.Contains(q.Blockers, text) {
			q.Blockers = append(q.Blockers, text)
		}
	case rpc.GammaQualityGateContext:
		if !slices.Contains(q.Context, text) {
			q.Context = append(q.Context, text)
		}
	}
}

func gammaOIObservedThreshold(c *rpc.GammaZeroComputed) float64 {
	if gammaQualityScope(c) == "SPX" {
		return gammaMinSPXOIObservedPct
	}
	return gammaMinDefaultOIObservedPct
}

func gammaQualityScope(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	switch c.Scope {
	case rpc.GammaZeroScopeSPX:
		return "SPX"
	case rpc.GammaZeroScopeCombined:
		return "SPY+SPX"
	default:
		return "SPY"
	}
}

func gammaQualitySessionKey(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return nySessionKey(t)
}

func gammaSignUsable(sign string) bool {
	return sign == "positive" || sign == "negative"
}

func percent(num, den float64) float64 {
	if den <= 0 {
		return 0
	}
	return num / den * 100
}

func medianSorted(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return values[mid]
	}
	return (values[mid-1] + values[mid]) / 2
}

func formatGammaQualityDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Truncate(time.Second).String()
}
