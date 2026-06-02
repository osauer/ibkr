package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestGammaQualityRankableCombinedSPYSPX(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	combined := rankableCombinedGammaFixture(now.Add(-10 * time.Minute))

	annotateGammaQuality(combined, now)
	refreshGammaSummaries(combined)

	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable: %+v", got, combined.Quality)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "red" {
		t.Fatalf("bandForGamma = %q, want red for rankable short-gamma fixture", got)
	}
}

func TestGammaQualitySingleScopesCanRankIndependently(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		scope string
	}{
		{"spy", rpc.GammaZeroScopeSPY},
		{"spx", rpc.GammaZeroScopeSPX},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := rankableGammaFixture(tc.scope, now.Add(-5*time.Minute))
			annotateGammaQuality(c, now)
			if got := c.Quality.Rankability; got != rpc.GammaRankabilityRankable {
				t.Fatalf("rankability = %q, want rankable: %+v", got, c.Quality)
			}
		})
	}
}

func TestGammaQualitySPXCanonicalRanksWithSPYUnavailable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.Warnings = []string{"spy_unavailable:throttled"}

	annotateGammaQuality(spx, now)

	if got := spx.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable canonical SPX despite SPY unavailability: %+v", got, spx.Quality)
	}
	for _, blocker := range append(spx.Quality.Blockers, spx.Quality.Context...) {
		if strings.Contains(blocker, "spy_coverage") {
			t.Fatalf("SPY unavailability should not downgrade canonical SPX quality: %+v", spx.Quality)
		}
	}
	var sawPass bool
	for _, gate := range spx.Quality.Gates {
		if gate.Name == "spy_coverage" && gate.Status == rpc.GammaQualityGatePass {
			sawPass = true
		}
	}
	if !sawPass {
		t.Fatalf("quality gates did not record SPY unavailability as non-blocking pass: %+v", spx.Quality.Gates)
	}
}

func TestGammaQualityStaleActiveCacheBlocksRanking(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	combined := rankableCombinedGammaFixture(now.Add(-2 * time.Hour))

	annotateGammaQuality(combined, now)
	refreshGammaSummaries(combined)

	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("rankability = %q, want blocked: %+v", got, combined.Quality)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "" {
		t.Fatalf("bandForGamma = %q, want unranked for stale gamma", got)
	}
}

func TestGammaQualityColdEnvelopeIsDataQualityPartial(t *testing.T) {
	t.Parallel()
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status:         rpc.GammaZeroStatusCold,
		ColdReasonCode: "session_closed_no_cache",
		ColdReason:     "no gamma cache is available",
	})
	if !ok {
		t.Fatal("gammaStatusQuality returned ok=false for cold envelope")
	}
	if got.Status != "partial" || !strings.Contains(got.Summary, "no gamma cache") {
		t.Fatalf("quality = %+v, want partial cold-cache summary", got)
	}
}

func TestGammaQualityForcedRefreshKeepsCurrentCacheRankable(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	c := newGammaZeroCache()
	current := rankableCombinedGammaFixture(now.Add(-5 * time.Minute))
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(current, rpc.GammaZeroScopeCombined, now)},
	}
	block := make(chan struct{})
	job := c.force(context.Background(), rpc.GammaZeroScopeCombined, now, computeETA, func(ctx context.Context, progress *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		<-block
		return rankableCombinedGammaFixture(now), nil
	})
	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now })
	close(block)
	<-job.done

	if env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		t.Fatalf("snapshot = %+v, want served current cache while force runs", env)
	}
	if got := env.Result.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("rankability = %q, want rankable served cache: %+v", got, env.Result.Quality)
	}
}

func TestGammaQualityFailedRefreshBlocksServedCache(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	c := newGammaZeroCache()
	current := rankableCombinedGammaFixture(now.Add(-70 * time.Minute))
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(current, rpc.GammaZeroScopeCombined, now)},
	}
	computeErr := func(ctx context.Context, progress *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return nil, errors.New("farm timeout")
	}
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, computeETA, computeErr)
	refresh := c.slots[rpc.GammaZeroScopeCombined].refresh
	if refresh == nil {
		t.Fatal("expected soft-TTL refresh to start")
	}
	<-refresh.done
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now.Add(time.Second), computeETA, computeErr)

	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now.Add(time.Second) })
	if env.Result == nil || env.Result.Quality == nil {
		t.Fatalf("snapshot missing quality: %+v", env)
	}
	if got := env.Result.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("rankability = %q, want blocked after failed refresh: %+v", got, env.Result.Quality)
	}
	if !hasGammaWarning(env.Result.WarningDetails, "refresh_failed:farm_timeout") {
		t.Fatalf("warning_details missing refresh failure: %+v", env.Result.WarningDetails)
	}
}

func TestGammaQualitySPYOnlyDegradationMakesCombinedContextOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spy.LegCount = 2
	spy.GammaTotalAbs = 10_000
	spy.TopConcentrationPct = 99
	spy.LegCount0DTE = 0
	spy.GammaSign0DTE = "no_data"
	spy.SkewFitQuality = map[string]rpc.SkewFitInfo{
		"20260602": {Points: 100, RSquared: 0.20},
	}
	spy.LegDiagnostics = &rpc.GammaLegDiagnostics{Total: rpc.GammaLegDiagnosticCounts{
		PricedLegs:               200,
		OpenInterestObservedLegs: 2,
		OpenInterestLegs:         2,
		GammaPositiveLegs:        200,
		AbsGEXLegs:               2,
	}}
	spy.Warnings = []string{"oi_missing"}
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	combined := combineGammaResults(spy, spx)

	annotateGammaQuality(combined, now)
	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityContextOnly {
		t.Fatalf("combined rankability = %q, want context_only: %+v", got, combined.Quality)
	}
	row := rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK, Envelope: rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: combined}}
	if got := bandForGamma(row); got != "" {
		t.Fatalf("bandForGamma = %q, want unranked for SPY-degraded combined gamma", got)
	}
}

func TestGammaQualitySPXOIDegradationBlocksCombined(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spx.LegDiagnostics.Total.OpenInterestObservedLegs = 180
	spx.Warnings = []string{"oi_missing"}
	combined := combineGammaResults(spy, spx)

	annotateGammaQuality(combined, now)
	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityBlocked {
		t.Fatalf("combined rankability = %q, want blocked: %+v", got, combined.Quality)
	}
}

func TestGammaQualityCoverageReportsLiveAndCarriedOI(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spy.LegDiagnostics.Total.OpenInterestObservedLegs = 198
	spy.LegDiagnostics.Total.OILiveObservedLegs = 120
	spy.LegDiagnostics.Total.OICarriedForwardLegs = 78

	annotateGammaQuality(spy, now)
	cov := spy.Quality.Coverage
	if cov.OIObservedLegs != 198 || cov.OILiveObservedLegs != 120 || cov.OICarriedForwardLegs != 78 {
		t.Fatalf("coverage split = %+v", cov)
	}
	if cov.OILiveObservedPct != 60 || cov.OICarriedForwardPct != 39 {
		t.Fatalf("coverage percentages = live %.1f carried %.1f, want 60/39", cov.OILiveObservedPct, cov.OICarriedForwardPct)
	}
}

func TestRegimeCompositeDoesNotRankContextOnlyGamma(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	combined := rankableCombinedGammaFixture(now.Add(-2 * time.Hour))
	annotateGammaQuality(combined, now)
	res := &rpc.RegimeSnapshotResult{
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: combined,
			},
		},
	}

	got := buildRegimeComposite(res)
	if got.ClusterRedCount != 0 || got.ClusterRankedCount != 0 {
		t.Fatalf("composite = %+v, want stale gamma unranked and no red vote", got)
	}
}

func rankableCombinedGammaFixture(asOf time.Time) *rpc.GammaZeroComputed {
	return hydrateGammaComputed(combineGammaResults(
		rankableGammaFixture(rpc.GammaZeroScopeSPY, asOf),
		rankableGammaFixture(rpc.GammaZeroScopeSPX, asOf),
	))
}

func rankableGammaFixture(scope string, asOf time.Time) *rpc.GammaZeroComputed {
	label := "SPY"
	spot := 750.0
	if scope == rpc.GammaZeroScopeSPX {
		label = "SPX"
		spot = 7500.0
	}
	return &rpc.GammaZeroComputed{
		Scope:                   scope,
		SpotUnderlying:          spot,
		GammaSign:               "negative",
		GammaTotalAbs:           4_000_000_000,
		GammaTotalAbsConvention: "sign-agnostic",
		TopConcentrationPct:     10,
		TopStrikes: []rpc.StrikeConcentration{{
			Underlying: label,
			Strike:     spot,
			Expiry:     "2026-06-02",
			Right:      "P",
			AbsGEX:     400_000_000,
			OI:         10_000,
		}},
		Expirations:    []string{"2026-06-02", "2026-06-05", "2026-06-19"},
		LegCount:       180,
		PricedLegCount: 200,
		DerivedIVLegs:  10,
		LegDiagnostics: &rpc.GammaLegDiagnostics{Total: rpc.GammaLegDiagnosticCounts{
			PricedLegs:               200,
			OpenInterestObservedLegs: 198,
			OpenInterestLegs:         180,
			GammaPositiveLegs:        200,
			AbsGEXLegs:               180,
		}},
		GammaSign0DTE: "negative",
		LegCount0DTE:  40,
		GammaSign1to7: "negative",
		LegCount1to7:  100,
		GammaSignTerm: "negative",
		LegCountTerm:  40,
		SkewFitQuality: map[string]rpc.SkewFitInfo{
			"20260602": {Points: 100, RSquared: 0.92},
			"20260605": {Points: 100, RSquared: 0.90},
			"20260619": {Points: 100, RSquared: 0.88},
		},
		Params: rpc.GammaZeroParams{
			ExpiryCount:    6,
			StrikeWidthPct: 0.10,
			SweepRangePct:  0.15,
			WorkerCount:    4,
		},
		Source: "test gamma fixture " + label,
		Method: gammaMethodToken,
		AsOf:   asOf,
	}
}
