package daemon

import (
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

func TestRegimeStatusQualityClustersStaleInputs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	res := &rpc.RegimeSnapshotResult{
		AsOf: now,
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusStale,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusOK,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status: rpc.RegimeStatusStale,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			Status: rpc.RegimeStatusOK,
		},
		FundingStress: rpc.RegimeFundingStress{
			Status: rpc.RegimeStatusOK,
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status: rpc.RegimeStatusStale,
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					AsOf:    now,
					Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
					WarningDetails: []rpc.GammaWarningDetail{{
						Code:     "spx_unavailable:354",
						Severity: "data_quality",
					}},
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK,
		},
	}

	got := regimeStatusQuality(res)
	if len(got) != 1 {
		t.Fatalf("regimeStatusQuality len=%d, want 1: %+v", len(got), got)
	}
	q := got[0]
	if q.Surface != "regime" || q.Status != "stale" {
		t.Fatalf("quality header = %+v, want regime/stale", q)
	}
	wantSummary := "stale: vol, credit, FX"
	if q.Summary != wantSummary {
		t.Fatalf("summary = %q, want %q", q.Summary, wantSummary)
	}
	if got, want := q.StaleClusters, []string{"vol", "credit", "FX"}; !equalStrings(got, want) {
		t.Fatalf("stale clusters = %#v, want %#v", got, want)
	}
	if len(q.DegradedClusters) != 0 {
		t.Fatalf("degraded clusters = %#v, want none", q.DegradedClusters)
	}
}

func TestStatusDataFarmsKeepsOnlyUnhealthyFarms(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 8, 20, 0, 0, time.UTC)
	got := statusDataFarms([]ibkrlib.DataFarmStatus{
		{Name: "usopt", Type: "market", Status: "ok", Code: 2104, AsOf: now},
		{Name: "euhmds", Type: "historical", Status: "inactive", Code: 2107, AsOf: now},
		{Name: "secdefeu", Type: "security_definition", Status: "disconnected", Code: 2157, Message: "Sec-def data farm connection is broken:secdefeu", AsOf: now},
		{Name: "tws-server", Type: "connectivity", Status: "broken", Code: 2110, Message: "Connectivity between TWS and server is broken", AsOf: now},
	})
	if len(got) != 2 {
		t.Fatalf("data farms len=%d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "secdefeu" || got[0].Status != "disconnected" {
		t.Fatalf("first farm = %+v, want secdefeu disconnected", got[0])
	}
	if got[1].Name != "tws-server" || got[1].Status != "broken" {
		t.Fatalf("second farm = %+v, want tws-server broken", got[1])
	}
}

func TestGammaStatusQualityReportsSPXExcluded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			AsOf:    now,
			Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
			WarningDetails: []rpc.GammaWarningDetail{{
				Code: "spx_unavailable:354",
			}},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: SPX excluded" {
		t.Fatalf("quality = %+v, want gamma degraded SPX excluded", got)
	}
}

func TestGammaStatusQualityReportsSPYExcluded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			AsOf:    now,
			Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
			WarningDetails: []rpc.GammaWarningDetail{{
				Code: "spy_unavailable:zero_magnitude",
			}},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: SPY excluded" {
		t.Fatalf("quality = %+v, want gamma degraded SPY excluded", got)
	}
}

func TestGammaStatusQualityReportsSPXCacheFallbackWithoutSummary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			AsOf: now,
			WarningDetails: []rpc.GammaWarningDetail{{
				Code: "spx_cache_fallback:timeout",
			}},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: SPX cache fallback" {
		t.Fatalf("quality = %+v, want gamma degraded SPX cache fallback", got)
	}
}

func TestGammaStatusQualityReportsPartialOptionOI(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeCombined,
			AsOf:  now,
			Summary: &rpc.GammaZeroSummary{
				Confidence: "degraded",
			},
			PerIndex: map[string]*rpc.GammaZeroComputed{
				"SPY": {
					Scope: rpc.GammaZeroScopeSPY,
					WarningDetails: []rpc.GammaWarningDetail{{
						Code: "oi_missing",
					}},
				},
				"SPX": {
					Scope: rpc.GammaZeroScopeSPX,
				},
			},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: partial option OI" {
		t.Fatalf("quality = %+v, want gamma degraded partial option OI", got)
	}
}

func TestRegimeSnapshotDataQualityCombinesGammaAndRegime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	res := &rpc.RegimeSnapshotResult{
		AsOf: now,
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusStale,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusOK,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status: rpc.RegimeStatusOK,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			Status: rpc.RegimeStatusOK,
		},
		FundingStress: rpc.RegimeFundingStress{
			Status: rpc.RegimeStatusOK,
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status: rpc.RegimeStatusOK,
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					AsOf:    now,
					Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
					WarningDetails: []rpc.GammaWarningDetail{{
						Code: "spx_unavailable:354",
					}},
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK,
		},
	}

	got := regimeSnapshotDataQuality(res)
	if len(got) != 2 {
		t.Fatalf("regimeSnapshotDataQuality len=%d, want 2: %+v", len(got), got)
	}
	if got[0].Surface != "gamma" || got[0].Summary != "degraded: SPX excluded" {
		t.Fatalf("first quality = %+v, want gamma degraded", got[0])
	}
	if got[1].Surface != "regime" || got[1].Summary != "stale: vol" {
		t.Fatalf("second quality = %+v, want regime stale vol", got[1])
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
