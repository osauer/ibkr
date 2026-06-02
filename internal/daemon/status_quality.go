package daemon

import (
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

func (s *Server) statusDataQuality() []rpc.DataQualityHealth {
	out := []rpc.DataQualityHealth{}
	if s.zeroGamma != nil {
		if q, ok := gammaStatusQuality(s.zeroGamma.snapshotCurrent(rpc.GammaZeroScopeCombined, time.Now)); ok {
			out = append(out, q)
		}
	}
	s.lastRegimeQualityMu.Lock()
	out = append(out, s.lastRegimeQuality...)
	s.lastRegimeQualityMu.Unlock()
	return out
}

func (s *Server) updateRegimeStatusQuality(r *rpc.RegimeSnapshotResult) {
	q := regimeStatusQuality(r)
	s.lastRegimeQualityMu.Lock()
	s.lastRegimeQuality = q
	s.lastRegimeQualityMu.Unlock()
}

func statusDataFarms(farms []ibkrlib.DataFarmStatus) []rpc.DataFarmHealth {
	out := make([]rpc.DataFarmHealth, 0, len(farms))
	for _, farm := range farms {
		if !dataFarmNeedsAttention(farm.Status) {
			continue
		}
		out = append(out, rpc.DataFarmHealth{
			Name:    farm.Name,
			Type:    farm.Type,
			Status:  farm.Status,
			Code:    farm.Code,
			Message: farm.Message,
			AsOf:    farm.AsOf,
		})
	}
	return out
}

func dataFarmNeedsAttention(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "broken", "disconnected":
		return true
	default:
		return false
	}
}

func regimeSnapshotDataQuality(r *rpc.RegimeSnapshotResult) []rpc.DataQualityHealth {
	if r == nil {
		return nil
	}
	out := []rpc.DataQualityHealth{}
	if q, ok := gammaStatusQuality(r.GammaZero.Envelope); ok {
		out = append(out, q)
	}
	out = append(out, regimeStatusQuality(r)...)
	return out
}

func regimeStatusQuality(r *rpc.RegimeSnapshotResult) []rpc.DataQualityHealth {
	if r == nil {
		return nil
	}
	stale := staleRegimeClusters(r)
	partial := partialRegimeClusters(r)
	if len(stale) == 0 && len(partial) == 0 {
		return nil
	}
	status := "stale"
	var summary []string
	if len(partial) > 0 {
		status = "partial"
		summary = append(summary, "partial: "+strings.Join(partial, ", "))
	}
	if len(stale) > 0 {
		summary = append(summary, "stale: "+strings.Join(stale, ", "))
	}
	q := rpc.DataQualityHealth{
		Surface:         "regime",
		Status:          status,
		StaleClusters:   stale,
		PartialClusters: partial,
		AsOf:            r.AsOf,
	}
	q.Summary = strings.Join(summary, "; ")
	return []rpc.DataQualityHealth{q}
}

func gammaStatusQuality(env rpc.GammaZeroSPXResult) (rpc.DataQualityHealth, bool) {
	switch env.Status {
	case rpc.GammaZeroStatusReady:
		if env.Result == nil {
			return rpc.DataQualityHealth{
				Surface:         "gamma",
				Status:          "partial",
				Summary:         "partial: gamma ready envelope missing result",
				PartialClusters: []string{"gamma"},
				AsOf:            gammaEnvelopeAsOf(env),
			}, true
		}
	case rpc.GammaZeroStatusComputing:
		summary := "partial: gamma computing"
		return rpc.DataQualityHealth{
			Surface:         "gamma",
			Status:          "partial",
			Summary:         summary,
			PartialClusters: []string{"gamma"},
			AsOf:            gammaEnvelopeAsOf(env),
		}, true
	case rpc.GammaZeroStatusCold:
		summary := "partial: gamma cold"
		if env.ColdReason != "" {
			summary = "partial: " + env.ColdReason
		}
		return rpc.DataQualityHealth{
			Surface:         "gamma",
			Status:          "partial",
			Summary:         summary,
			PartialClusters: []string{"gamma"},
			AsOf:            gammaEnvelopeAsOf(env),
		}, true
	case rpc.GammaZeroStatusError:
		summary := "partial: gamma error"
		if strings.TrimSpace(env.Error) != "" {
			summary = "partial: gamma error: " + strings.TrimSpace(env.Error)
		}
		return rpc.DataQualityHealth{
			Surface:         "gamma",
			Status:          "partial",
			Summary:         summary,
			PartialClusters: []string{"gamma"},
			AsOf:            gammaEnvelopeAsOf(env),
		}, true
	default:
		return rpc.DataQualityHealth{}, false
	}
	if !gammaResultDegraded(env.Result) {
		return rpc.DataQualityHealth{}, false
	}
	summary := "degraded"
	if gammaHasSPYUnavailable(env.Result) {
		summary = "degraded: SPY excluded"
	} else if gammaHasSPXUnavailable(env.Result) {
		summary = "degraded: SPX excluded"
	} else if gammaHasSPXCacheFallback(env.Result) {
		summary = "degraded: SPX cache fallback"
	} else if gammaHasOIMissing(env.Result) {
		switch gammaOIMissingSummaryClass(env.Result, env.Result.AsOf) {
		case "spx":
			summary = "degraded: partial SPX option OI (unexpected: SPX OI should be session-stable)"
		case "rth":
			summary = "degraded: partial option OI (unexpected: sampled during RTH)"
		case "spy_off_hours":
			summary = "degraded: partial SPY option OI (expected: sampled outside RTH)"
		default:
			summary = "degraded: partial option OI"
		}
	}
	return rpc.DataQualityHealth{
		Surface:          "gamma",
		Status:           "degraded",
		Summary:          summary,
		DegradedClusters: []string{"gamma"},
		AsOf:             env.Result.AsOf,
	}, true
}

func gammaEnvelopeAsOf(env rpc.GammaZeroSPXResult) time.Time {
	if env.Result != nil && !env.Result.AsOf.IsZero() {
		return env.Result.AsOf
	}
	if env.StartedAt != nil {
		return *env.StartedAt
	}
	return time.Time{}
}

func gammaResultDegraded(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	if c.Summary != nil && strings.EqualFold(c.Summary.Confidence, "degraded") {
		return true
	}
	for _, rawCode := range gammaWarningCodes(c) {
		code := strings.ToLower(strings.TrimSpace(rawCode))
		switch {
		case code == "throttled", code == "all_iv_derived", code == "cache_stale_off_hours", code == "oi_missing":
			return true
		case strings.HasPrefix(code, "spy_unavailable:"),
			strings.HasPrefix(code, "spx_unavailable:"),
			strings.HasPrefix(code, "spx_cache_fallback"),
			strings.HasPrefix(code, "skew_fallback:"):
			return true
		}
	}
	for _, sub := range c.PerIndex {
		if gammaResultDegraded(sub) {
			return true
		}
	}
	return false
}

func gammaHasSPYUnavailable(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	for _, w := range c.WarningDetails {
		if strings.HasPrefix(w.Code, "spy_unavailable:") {
			return true
		}
	}
	for _, sub := range c.PerIndex {
		if gammaHasSPYUnavailable(sub) {
			return true
		}
	}
	return false
}

func gammaHasSPXUnavailable(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	for _, w := range c.WarningDetails {
		if strings.HasPrefix(w.Code, "spx_unavailable:") {
			return true
		}
	}
	for _, sub := range c.PerIndex {
		if gammaHasSPXUnavailable(sub) {
			return true
		}
	}
	return false
}

func gammaHasSPXCacheFallback(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	for _, w := range c.WarningDetails {
		if strings.HasPrefix(w.Code, "spx_cache_fallback") {
			return true
		}
	}
	for _, sub := range c.PerIndex {
		if gammaHasSPXCacheFallback(sub) {
			return true
		}
	}
	return false
}

func gammaHasOIMissing(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	for _, w := range c.WarningDetails {
		if w.Code == "oi_missing" {
			return true
		}
	}
	for _, sub := range c.PerIndex {
		if gammaHasOIMissing(sub) {
			return true
		}
	}
	return false
}

func gammaOIMissingSummaryClass(c *rpc.GammaZeroComputed, inheritedAsOf time.Time) string {
	if c == nil {
		return ""
	}
	asOf := inheritedAsOf
	if !c.AsOf.IsZero() {
		asOf = c.AsOf
	}
	for _, w := range c.WarningDetails {
		if w.Code == "oi_missing" {
			if asOf.IsZero() {
				asOf = time.Now()
			}
			scope := strings.ToUpper(strings.TrimSpace(w.Scope))
			if scope == "" {
				scope = strings.ToUpper(gammaStatusQualityScope(c))
			}
			if scope == "SPX" {
				return "spx"
			}
			if rpc.ClassifySession(asOf) == rpc.SessionRTH {
				return "rth"
			}
			if scope == "SPY" {
				return "spy_off_hours"
			}
			return "unknown"
		}
	}
	best := ""
	for _, sub := range c.PerIndex {
		switch got := gammaOIMissingSummaryClass(sub, asOf); got {
		case "spx":
			return got
		case "rth":
			best = got
		case "spy_off_hours":
			if best == "" {
				best = got
			}
		case "unknown":
			if best == "" {
				best = got
			}
		}
	}
	return best
}

func gammaStatusQualityScope(c *rpc.GammaZeroComputed) string {
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

func staleRegimeClusters(r *rpc.RegimeSnapshotResult) []string {
	candidates := []struct {
		name     string
		statuses []string
	}{
		{name: "vol", statuses: []string{r.VIXTermStructure.Status, r.VolOfVol.Status}},
		{name: "credit", statuses: []string{r.HYGSPYDivergence.Status, r.CreditSpreads.Status}},
		{name: "funding", statuses: []string{r.FundingStress.Status}},
		{name: "FX", statuses: []string{r.USDJPY.Status}},
		{name: "gamma", statuses: []string{r.GammaZero.Status}},
		{name: "breadth", statuses: []string{r.Breadth.Status}},
	}
	out := []string{}
	for _, c := range candidates {
		if hasRegimeStatus(c.statuses, rpc.RegimeStatusStale) {
			out = append(out, c.name)
		}
	}
	return out
}

func partialRegimeClusters(r *rpc.RegimeSnapshotResult) []string {
	candidates := []struct {
		name     string
		statuses []string
	}{
		{name: "vol", statuses: []string{r.VIXTermStructure.Status, r.VolOfVol.Status}},
		{name: "credit", statuses: []string{r.HYGSPYDivergence.Status, r.CreditSpreads.Status}},
		{name: "funding", statuses: []string{r.FundingStress.Status}},
		{name: "FX", statuses: []string{r.USDJPY.Status}},
		{name: "gamma", statuses: []string{r.GammaZero.Status}},
		{name: "breadth", statuses: []string{r.Breadth.Status}},
	}
	out := []string{}
	for _, c := range candidates {
		if hasRegimeStatus(c.statuses, rpc.RegimeStatusComputing) ||
			hasRegimeStatus(c.statuses, rpc.RegimeStatusUnavailable) ||
			hasRegimeStatus(c.statuses, rpc.RegimeStatusError) {
			out = append(out, c.name)
		}
	}
	return out
}

func hasRegimeStatus(statuses []string, want string) bool {
	for _, status := range statuses {
		if strings.EqualFold(strings.TrimSpace(status), want) {
			return true
		}
	}
	return false
}
