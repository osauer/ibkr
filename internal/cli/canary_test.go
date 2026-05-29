package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestComputeCanaryAmbiguityDoesNotLookSafe(t *testing.T) {
	t.Parallel()
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime: rpc.RegimeSnapshotResult{
			Composite: rpc.RegimeComposite{ClusterRankedCount: 0, ClusterUnrankedCount: 6},
			GammaZero: rpc.RegimeGammaZero{
				Status: rpc.RegimeStatusComputing,
			},
			Breadth: rpc.RegimeBreadth{
				Status: rpc.RegimeStatusComputing,
			},
		},
		Now: time.Date(2026, 5, 28, 21, 55, 0, 0, time.UTC),
	})
	if res.Decision != canaryDecisionWatch {
		t.Fatalf("decision = %s, want WATCH for ambiguous all-unranked market", res.Decision)
	}
	if !rowContains(res.Rows, "Data quality gate", "wait for at least four ranked") {
		t.Fatalf("expected data-quality ambiguity row, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryConfirmedStressWithIncompleteGammaBreadthStillDelevers(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.GrossPositionValue = 130_000
	delta := 95_000.0
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			DollarDeltaBase: &delta,
			ExposureBase: []rpc.UnderlyingExposure{{
				Underlying: "SPY", MarketValueBase: 60_000, MarketValuePctNLV: new(60.0),
			}},
			GreeksCoverage: 2,
			GreeksTotal:    2,
		}},
		Regime: redVolCreditRegimeWithComputingSlowRows(),
	})
	if res.Decision != canaryDecisionDelever {
		t.Fatalf("decision = %s, want DE-LEVER on confirmed vol+credit stress with high exposure", res.Decision)
	}
	if !rowContains(res.Rows, "Ambiguity filter", "do not suppress confirmed independent red signals") {
		t.Fatalf("expected ambiguity filter disclosure, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryImmediateMarginDangerLiquidatesDespiteAmbiguousMarket(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.Cushion = 0.07
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime: rpc.RegimeSnapshotResult{
			Composite: rpc.RegimeComposite{ClusterRankedCount: 1, ClusterUnrankedCount: 5},
			GammaZero: rpc.RegimeGammaZero{
				Status: rpc.RegimeStatusComputing,
			},
		},
	})
	if res.Decision != canaryDecisionLiquidate {
		t.Fatalf("decision = %s, want LIQUIDATE on margin cushion below 10%%", res.Decision)
	}
}

func TestComputeCanaryEarlyStressRequiresSecondIndependentCluster(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 1, ClusterYellowCount: 1, ClusterRankedCount: 6}
	r.VIXTermStructure.Band = "red"
	r.VolOfVol.Band = "red"
	r.Breadth.Band = "yellow"
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if res.Decision != canaryDecisionWatch {
		t.Fatalf("decision = %s, want WATCH for one red cluster plus yellow", res.Decision)
	}
	if !rowContains(res.Rows, "Early stress filtered", "second independent red cluster") {
		t.Fatalf("expected early-stress filter row, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryStandalonePremarketSPYDropWatches(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	spyPct := -2.7
	vixPct := 2.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if res.Decision != canaryDecisionWatch {
		t.Fatalf("decision = %s, want WATCH for unconfirmed premarket SPY drop", res.Decision)
	}
	if !rowContains(res.Rows, "Index tape shock", "second pass") {
		t.Fatalf("expected direct tape second-pass row, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryConfirmedSPYVIXShockDelevers(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	spyPct := -2.6
	vixPct := 12.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if res.Decision != canaryDecisionDelever {
		t.Fatalf("decision = %s, want DE-LEVER for confirmed SPY/VIX shock plus red cluster", res.Decision)
	}
	if !rowContains(res.Rows, "Index tape shock", "direct SPY stress is confirmed") {
		t.Fatalf("expected confirmed direct tape row, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryStandaloneVIXSpikeWatches(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	spyPct := -0.2
	vixPct := 18.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if res.Decision != canaryDecisionWatch {
		t.Fatalf("decision = %s, want WATCH for standalone VIX spike", res.Decision)
	}
	if !rowContains(res.Rows, "Index tape shock", "flashing early stress") {
		t.Fatalf("expected early VIX tape row, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryGreenClustersAreNotAmbiguous(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 5, ClusterUnrankedCount: 1}
	r.GammaZero.Band = "red"
	r.Breadth.Band = "unranked"
	r.Breadth.Status = rpc.RegimeStatusComputing
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.AmbiguousClusters, ","); got != "breadth" {
		t.Fatalf("ambiguous clusters = %q, want breadth only", got)
	}
}

func TestComputeCanarySurfacesDegradedGammaSeparately(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
	}
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.AmbiguousClusters, ","); got != "" {
		t.Fatalf("ambiguous clusters = %q, want none for ranked degraded gamma", got)
	}
	if got := strings.Join(res.Market.DegradedClusters, ","); got != "gamma" {
		t.Fatalf("degraded clusters = %q, want gamma", got)
	}
	if !rowContains(res.Rows, "Ambiguity filter", "verify weak clusters") {
		t.Fatalf("expected degraded-gamma disclosure row, rows: %+v", res.Rows)
	}
	if res.Confidence != "medium-low" {
		t.Fatalf("confidence = %q, want medium-low for degraded gamma", res.Confidence)
	}
}

func TestComputeCanarySeparatesPartialFromAmbiguousClusters(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 4, ClusterRedCount: 1, ClusterRankedCount: 5, ClusterUnrankedCount: 1}
	r.HYGSPYDivergence.Band = "green"
	r.HYGSPYDivergence.Status = rpc.RegimeStatusOK
	r.CreditSpreads.Band = "unranked"
	r.CreditSpreads.Status = rpc.RegimeStatusError
	r.FundingStress.Band = "unranked"
	r.FundingStress.Status = rpc.RegimeStatusError
	r.GammaZero.Band = "red"
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.AmbiguousClusters, ","); got != "funding" {
		t.Fatalf("ambiguous clusters = %q, want funding", got)
	}
	if got := strings.Join(res.Market.PartialClusters, ","); got != "credit" {
		t.Fatalf("partial clusters = %q, want credit", got)
	}
}

func TestCanaryWarningsSanitizeExternalErrors(t *testing.T) {
	t.Parallel()
	line := canaryWarningLine(rpc.RegimeWarning{
		Code:    "credit_spreads_unavailable",
		Scope:   "credit_spreads",
		Message: "HY OAS: GET https://fred.stlouisfed.org/graph/fredgraph.csv?id=BAMLH0A0HYM2: HTTP 404 Not Found",
		Impact:  "cash credit row is unranked; ETF credit proxy may still rank the credit cluster.",
		Action:  "Retry later.",
	})
	if strings.Contains(line, "https://") || strings.Contains(line, "HTTP 404") {
		t.Fatalf("warning leaked noisy transport error: %s", line)
	}
	if !strings.Contains(line, "cash credit row is unranked") {
		t.Fatalf("warning did not preserve useful impact: %s", line)
	}
}

func TestCanaryWarningsPolishUnrankedImpact(t *testing.T) {
	t.Parallel()
	line := canaryWarningLine(rpc.RegimeWarning{
		Scope:  "credit_spreads",
		Impact: "cash credit spreads is unranked; the composite has lower coverage.",
	})
	if !strings.Contains(line, "cash credit spreads row is unranked") {
		t.Fatalf("warning did not polish unranked impact: %s", line)
	}
}

func TestRenderCanaryTextShowsEscalationLadder(t *testing.T) {
	t.Parallel()
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  redVolCreditRegimeWithComputingSlowRows(),
		Now:     time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
	})
	var out bytes.Buffer
	renderCanaryText(&Env{}, &out, &res)
	got := out.String()
	for _, want := range []string{
		"Stage      [De-lever]",
		"Confidence High",
		"Escalate   Go > Watch > [De-lever] > Liquidate",
		"Title                        Stage",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q:\n%s", want, got)
		}
	}
}

func TestRenderCanaryTextColorsCurrentStage(t *testing.T) {
	t.Parallel()
	res := CanaryResult{
		AsOf:     time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
		Decision: canaryDecisionWatch,
		Action:   "Freeze new risk.",
		Rows: []CanaryRow{{
			Title:    "Portfolio canary",
			Decision: canaryDecisionWatch,
			Action:   "Freeze new risk.",
		}},
	}
	var out bytes.Buffer
	renderCanaryText(&Env{Color: true}, &out, &res)
	got := out.String()
	if !strings.Contains(got, ansiBold+ansiYellow+"[Watch]"+ansiReset+ansiReset) {
		t.Fatalf("current WATCH stage is not bold yellow:\n%q", got)
	}
	if strings.Contains(got, "CURRENT") {
		t.Fatalf("render should not repeat CURRENT:\n%q", got)
	}
}

func baseCanaryAccount() rpc.AccountResult {
	return rpc.AccountResult{
		BaseCurrency:       "USD",
		NetLiquidation:     100_000,
		ExcessLiquidity:    50_000,
		Cushion:            0.50,
		GrossPositionValue: 60_000,
	}
}

func healthyCanaryRegime() rpc.RegimeSnapshotResult {
	return rpc.RegimeSnapshotResult{
		Composite: rpc.RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		VIXTermStructure: rpc.RegimeVIXTerm{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		FundingStress: rpc.RegimeFundingStress{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		USDJPY: rpc.RegimeUSDJPY{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		GammaZero: rpc.RegimeGammaZero{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		Breadth: rpc.RegimeBreadth{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
	}
}

func redVolCreditRegimeWithComputingSlowRows() rpc.RegimeSnapshotResult {
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 2, ClusterGreenCount: 2, ClusterRankedCount: 4, ClusterUnrankedCount: 2}
	r.VIXTermStructure.Band = "red"
	r.VolOfVol.Band = "red"
	r.HYGSPYDivergence.Band = "red"
	r.CreditSpreads.Band = "red"
	r.GammaZero.Band = ""
	r.GammaZero.Status = rpc.RegimeStatusComputing
	r.Breadth.Band = ""
	r.Breadth.Status = rpc.RegimeStatusComputing
	return r
}

func rowContains(rows []CanaryRow, title, text string) bool {
	for _, row := range rows {
		if row.Title == title && strings.Contains(row.Action, text) {
			return true
		}
	}
	return false
}
