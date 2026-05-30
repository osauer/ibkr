package daemon

import (
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
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
	if len(stale) == 0 {
		return nil
	}
	q := rpc.DataQualityHealth{
		Surface:       "regime",
		Status:        "stale",
		StaleClusters: stale,
		AsOf:          r.AsOf,
	}
	q.Summary = "stale: " + strings.Join(stale, ", ")
	return []rpc.DataQualityHealth{q}
}

func gammaStatusQuality(env rpc.GammaZeroSPXResult) (rpc.DataQualityHealth, bool) {
	if env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		return rpc.DataQualityHealth{}, false
	}
	if !gammaResultDegraded(env.Result) {
		return rpc.DataQualityHealth{}, false
	}
	summary := "degraded"
	if gammaHasSPXUnavailable(env.Result) {
		summary = "degraded: SPX excluded"
	}
	return rpc.DataQualityHealth{
		Surface:          "gamma",
		Status:           "degraded",
		Summary:          summary,
		DegradedClusters: []string{"gamma"},
		AsOf:             env.Result.AsOf,
	}, true
}

func gammaResultDegraded(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	if c.Summary != nil && strings.EqualFold(c.Summary.Confidence, "degraded") {
		return true
	}
	return gammaHasSPXUnavailable(c)
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

func hasRegimeStatus(statuses []string, want string) bool {
	for _, status := range statuses {
		if strings.EqualFold(strings.TrimSpace(status), want) {
			return true
		}
	}
	return false
}
