package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestGammaZeroStatusAndRegimeUsesGapForCrossing(t *testing.T) {
	cases := []struct {
		name string
		gap  float64
		want string
	}{
		{"above_zero_long_gamma", 3.0, "long_gamma"},
		{"near_zero_transition", 0.5, "transition_gamma"},
		{"below_zero_short_gamma", -3.0, "short_gamma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zero := 100.0
			c := &rpc.GammaZeroComputed{ZeroGamma: &zero, GapPct: &tc.gap}
			status, regime := gammaZeroStatusAndRegime(c)
			if status != "crossing" {
				t.Fatalf("status = %q, want crossing", status)
			}
			if regime != tc.want {
				t.Fatalf("regime = %q, want %q", regime, tc.want)
			}
		})
	}
}

func TestSPXUnavailableWarningTextFetchCanceledIsUserFacing(t *testing.T) {
	t.Parallel()
	message, impact, action := spxUnavailableWarningText("fetch_canceled")
	for _, got := range []string{message, impact, action} {
		if strings.Contains(got, "context canceled") {
			t.Fatalf("warning text leaked raw context error: message=%q impact=%q action=%q", message, impact, action)
		}
	}
	for _, want := range []string{
		"canceled before usable data landed",
		"Showing SPY only",
		"09:30-16:15 ET",
		"--only=spy",
	} {
		if !strings.Contains(message+" "+impact+" "+action, want) {
			t.Fatalf("warning text missing %q: message=%q impact=%q action=%q", want, message, impact, action)
		}
	}
}

func TestSPYUnavailableWarningTextZeroMagnitudeIsUserFacing(t *testing.T) {
	t.Parallel()
	message, impact, action := spyUnavailableWarningText("zero_magnitude")
	for _, want := range []string{
		"SPY option chain was skipped",
		"Showing SPX only",
		"regular trading hours",
		"--only=spy --force",
	} {
		if !strings.Contains(message+" "+impact+" "+action, want) {
			t.Fatalf("warning text missing %q: message=%q impact=%q action=%q", want, message, impact, action)
		}
	}
}

func TestGammaWarningDetailSPYUnavailableScopesToSPY(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeSPX}, "spy_unavailable:zero_magnitude")
	if got.Severity != "data_quality" || got.Scope != "SPY" {
		t.Fatalf("warning detail = %+v, want SPY data_quality warning", got)
	}
	for _, want := range []string{"Showing SPX only", "SPY gamma is not included"} {
		if !strings.Contains(got.Message+" "+got.Impact, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailStrikeBudgetCapped(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeSPX}, "strike_budget_capped")
	if got.Severity != "methodology" || got.Scope != "SPX" {
		t.Fatalf("warning detail = %+v, want SPX methodology warning", got)
	}
	for _, want := range []string{"nearest 80", "gateway request budget"} {
		if !strings.Contains(got.Message+" "+got.Impact, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailOIMissingNamesAPIDiagnostic(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		AsOf:           time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC), // 08:00 ET pre-market
		PricedLegCount: 751,
		LegCount:       2,
	}, "oi_missing")
	if got.Severity != "info" || got.Scope != "SPY" {
		t.Fatalf("warning detail = %+v, want SPY info warning outside RTH", got)
	}
	for _, want := range []string{"749 priced legs", "2 had positive OI", "generic tick 101", "regular U.S. option-data surface is closed", "sparse SPY OI is expected", "unknown, not zero", "09:30-16:15 ET"} {
		if !strings.Contains(got.Message+" "+got.Impact+" "+got.Action, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailOIMissingCountsPositiveOIAsObservedForLegacyCache(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		AsOf:           time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC), // 08:00 ET pre-market
		PricedLegCount: 751,
		LegCount:       2,
		LegDiagnostics: &rpc.GammaLegDiagnostics{
			Total: rpc.GammaLegDiagnosticCounts{
				PricedLegs:       751,
				OpenInterestLegs: 2,
			},
		},
	}, "oi_missing")
	if got.Severity != "info" || got.Scope != "SPY" {
		t.Fatalf("warning detail = %+v, want SPY info warning outside RTH", got)
	}
	for _, want := range []string{"749 priced legs", "2 legs had observed OI", "2 had positive OI"} {
		if !strings.Contains(got.Message+" "+got.Impact, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailOIMissingIsDataQualityForSPXOutsideRTH(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		AsOf:           time.Date(2026, time.June, 1, 22, 0, 0, 0, time.UTC), // 18:00 ET post-market
		PricedLegCount: 927,
		LegCount:       335,
	}, "oi_missing")
	if got.Severity != "data_quality" || got.Scope != "SPX" {
		t.Fatalf("warning detail = %+v, want SPX data_quality warning outside RTH", got)
	}
	for _, want := range []string{"SPX option OI should normally be stable", "unknown, not zero", "data-farm health"} {
		if !strings.Contains(got.Message+" "+got.Impact+" "+got.Action, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailOIMissingIsDataQualityDuringRTH(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		AsOf:           time.Date(2026, time.June, 1, 15, 0, 0, 0, time.UTC), // 11:00 ET RTH
		PricedLegCount: 911,
		LegCount:       346,
	}, "oi_missing")
	if got.Severity != "data_quality" || got.Scope != "SPX" {
		t.Fatalf("warning detail = %+v, want SPX data_quality warning during RTH", got)
	}
	for _, want := range []string{"565 priced legs", "regular U.S. option hours", "should normally be available", "data-farm health"} {
		if !strings.Contains(got.Message+" "+got.Impact+" "+got.Action, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}
