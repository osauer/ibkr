package daemon

import (
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
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
