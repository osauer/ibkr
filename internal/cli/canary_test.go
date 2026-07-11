package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
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
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch for ambiguous all-unranked market", res.Direction, res.Severity)
	}
	if res.Action != canaryActionConfirmInputs || res.MarketConfirmation != canaryMarketBlocked || res.InputHealth != canaryInputDegraded {
		t.Fatalf("decision = action %s market %s input %s, want confirm_inputs/blocked/degraded", res.Action, res.MarketConfirmation, res.InputHealth)
	}
	if res.PlannerModeHint != risk.PlannerModeConfirmData || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("planner = %s/%s, want confirm_data/blocked", res.PlannerModeHint, res.PlannerReadiness)
	}
	if !rowContains(res.Rows, "Ambiguity filter", "Refresh or verify incomplete market inputs") {
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
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch until degraded inputs are clean", res.Direction, res.Severity)
	}
	if res.Action != canaryActionWatch || res.PlannerReadiness != risk.PlannerReadinessPrestage {
		t.Fatalf("action/readiness = %s/%s, want watch/prestage", res.Action, res.PlannerReadiness)
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
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch because market inputs are blocked", res.Direction, res.Severity)
	}
	if res.Action != canaryActionConfirmInputs {
		t.Fatalf("action = %s, want confirm_inputs for account-only margin danger in canary", res.Action)
	}
}

func TestComputeCanaryZeroExcessLiquidityLiquidatesWhenMarginContextPresent(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.Cushion = 0
	acct.ExcessLiquidity = 0
	acct.MaintenanceMargin = 80_000
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  healthyCanaryRegime(),
	})
	if res.Direction != "" || res.Severity != risk.SeverityObserve {
		t.Fatalf("state = %s/%s, want observe because account-only margin danger is outside canary scope", res.Direction, res.Severity)
	}
	if res.Action != canaryActionStandDown {
		t.Fatalf("action = %s, want stand_down without market pressure", res.Action)
	}
	if !rowContainsEvidence(res.Rows, "Immediate margin safety", "cushion 0%") {
		t.Fatalf("expected zero-cushion evidence, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryLookAheadMarginDangerLiquidates(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.LookAheadExcess = 8_000
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  healthyCanaryRegime(),
	})
	if res.Direction != "" || res.Severity != risk.SeverityObserve {
		t.Fatalf("state = %s/%s, want observe because look-ahead margin alone is outside canary scope", res.Direction, res.Severity)
	}
	if !rowContainsEvidence(res.Rows, "Immediate margin safety", "look-ahead cushion 8%") {
		t.Fatalf("expected look-ahead cushion evidence, rows: %+v", res.Rows)
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
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch for one red cluster plus yellow", res.Direction, res.Severity)
	}
	if res.PlannerModeHint != risk.PlannerModeStage {
		t.Fatalf("planner_mode_hint = %s, want stage", res.PlannerModeHint)
	}
	if !rowContains(res.Rows, "Early stress filtered", "second independent red cluster") {
		t.Fatalf("expected early-stress filter row, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryLowExposureMarketWatchDoesNotClaimPortfolioExposed(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.GrossPositionValue = 0
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterYellowCount: 1, ClusterRankedCount: 6}
	r.VIXTermStructure.Band = "red"
	r.VolOfVol.Band = "red"
	r.Breadth.Band = "yellow"
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Action != canaryActionWatch || res.PortfolioFit != canaryPortfolioFitLow {
		t.Fatalf("decision = action %s portfolio_fit %s, want watch/low", res.Action, res.PortfolioFit)
	}
	summary := strings.ToLower(res.Summary)
	if strings.Contains(summary, "portfolio is exposed") || strings.Contains(summary, "stage reductions") {
		t.Fatalf("summary should not claim low-exposure portfolio is exposed: %q", res.Summary)
	}
	if !strings.Contains(summary, "portfolio exposure is low") {
		t.Fatalf("summary should explain low exposure watch posture, got %q", res.Summary)
	}
}

func TestComputeCanarySingleGammaRedIsNotLifecycleConfirmation(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	spyPct := -0.1
	vixPct := 5.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.MarketConfirmation == canaryMarketConfirmed {
		t.Fatalf("market_confirmation = %s, want not confirmed for one red gamma cluster", res.MarketConfirmation)
	}
	if res.Market.RegimePosture.Label != "Stress signal present" || res.Market.RegimePosture.Tone != rpc.RegimeToneWatch {
		t.Fatalf("market posture = %+v, want Stress signal present/watch", res.Market.RegimePosture)
	}
	if !hasSignal(res.Signals, risk.SignalGammaRed) {
		t.Fatalf("missing gamma red watch signal, signals: %+v", res.Signals)
	}
	marketEvidence := canaryMarketEvidence(res.Market)
	if strings.Contains(marketEvidence, "SPY") || strings.Contains(marketEvidence, "VIX") {
		t.Fatalf("market cluster evidence should not mix tape indicators: %q", marketEvidence)
	}
	if got := canaryTapeEvidence(res.Market); !strings.Contains(got, "SPY") || !strings.Contains(got, "VIX") {
		t.Fatalf("tape evidence should remain separately available, got %q", got)
	}
}

func TestComputeCanaryContextOnlyGammaDoesNotConfirmStress(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	r.GammaZero.Status = rpc.RegimeStatusOK
	r.GammaZero.Envelope = rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope:         rpc.GammaZeroScopeCombined,
			GammaSign:     "negative",
			GammaTotalAbs: 10_000_000_000,
			Quality: &rpc.GammaSignalQuality{
				Rankability:       rpc.GammaRankabilityContextOnly,
				RankabilityReason: "freshness: market is closed; cached gamma is context only",
			},
		},
	}

	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})

	if slices.Contains(res.Market.RedClusterNames, "gamma") {
		t.Fatalf("red clusters = %+v, want context-only gamma excluded", res.Market.RedClusterNames)
	}
	if slices.Contains(res.Market.DegradedClusters, "gamma") {
		t.Fatalf("degraded clusters = %+v, want context-only gamma excluded", res.Market.DegradedClusters)
	}
	if slices.Contains(res.Market.AmbiguousClusters, "gamma") {
		t.Fatalf("ambiguous clusters = %+v, want context-only gamma excluded", res.Market.AmbiguousClusters)
	}
	if hasSignal(res.Signals, risk.SignalGammaRed) {
		t.Fatalf("signals include gamma_red despite context-only gamma: %+v", res.Signals)
	}
	if res.InputHealth != canaryInputOK {
		t.Fatalf("input_health = %s, want ok for context-only gamma", res.InputHealth)
	}
	for _, h := range res.SourceHealth {
		if h.Source == "regime" && h.Status != rpc.RegimeStatusOK {
			t.Fatalf("regime source health = %+v, want ok when only gamma is context-only", h)
		}
	}
	if hasSignal(res.Signals, risk.SignalRiskDataDegraded) {
		t.Fatalf("signals include data degraded despite context-only gamma: %+v", res.Signals)
	}
}

func TestComputeCanaryGammaRequiresExplicitRankableQuality(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		status         string
		envelopeStatus string
		quality        *rpc.GammaSignalQuality
	}{
		{name: "nil_quality", status: rpc.RegimeStatusOK, envelopeStatus: rpc.GammaZeroStatusReady},
		{name: "blocked_quality", status: rpc.RegimeStatusOK, envelopeStatus: rpc.GammaZeroStatusReady, quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked, RankabilityReason: "OI coverage blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := healthyCanaryRegime()
			r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
			r.GammaZero.Band = "red"
			r.GammaZero.Status = tc.status
			r.GammaZero.Envelope.Status = tc.envelopeStatus
			r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
				Quality: tc.quality,
			}

			res := ComputeCanary(CanaryInput{
				Account: baseCanaryAccount(),
				Regime:  r,
			})

			if slices.Contains(res.Market.RedClusterNames, "gamma") {
				t.Fatalf("red clusters = %+v, want gamma excluded", res.Market.RedClusterNames)
			}
			if res.Market.RankedClusters != 5 || res.Market.UnrankedClusters != 1 {
				t.Fatalf("cluster coverage = ranked %d unranked %d, want 5/1", res.Market.RankedClusters, res.Market.UnrankedClusters)
			}
			if !slices.Contains(res.Market.AmbiguousClusters, "gamma") {
				t.Fatalf("ambiguous clusters = %+v, want gamma ambiguous", res.Market.AmbiguousClusters)
			}
			if !slices.Contains(res.Market.DegradedClusters, "gamma") {
				t.Fatalf("degraded clusters = %+v, want gamma degraded", res.Market.DegradedClusters)
			}
			if hasSignal(res.Signals, risk.SignalGammaRed) {
				t.Fatalf("signals include gamma_red despite non-rankable gamma: %+v", res.Signals)
			}
		})
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
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch for unconfirmed premarket SPY drop", res.Direction, res.Severity)
	}
	if res.PlannerModeHint != risk.PlannerModeStage {
		t.Fatalf("planner_mode_hint = %s, want stage", res.PlannerModeHint)
	}
	if !rowContains(res.Rows, "Index tape shock", "second pass") {
		t.Fatalf("expected direct tape second-pass row, rows: %+v", res.Rows)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarketSelloffViolent)
	if !ok {
		t.Fatalf("missing selloff signal, signals: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityWatch || !containsString(sig.BlockedBy, "confirmation") {
		t.Fatalf("selloff signal = severity %q blocked_by %+v, want watch blocked by confirmation", sig.Severity, sig.BlockedBy)
	}
}

func TestComputeCanaryProvisionalRedDoesNotConfirmHardTapeStress(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.GrossPositionValue = 180_000
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	spyPct := -2.6
	vixPct := 2.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			ExposureBase: []rpc.UnderlyingExposure{{
				Underlying: "SPY", DollarDeltaBase: new(45_000.0),
			}},
		}},
		Regime: r,
		Now:    time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.MarketConfirmation == canaryMarketConfirmed || res.Market.EligibleRedClusters != 0 {
		t.Fatalf("market = confirmation %s eligible %d, want provisional/non-confirmed", res.MarketConfirmation, res.Market.EligibleRedClusters)
	}
	gross, ok := findSignal(res.Signals, risk.SignalGrossExposureHigh)
	if !ok {
		t.Fatalf("missing gross exposure signal, signals: %+v", res.Signals)
	}
	if gross.Severity != risk.SeverityWatch || gross.Direction != risk.DirectionRebalance {
		t.Fatalf("gross exposure signal = %+v, want watch/rebalance for provisional red plus hard tape", gross)
	}
	concentration, ok := findSignal(res.Signals, risk.SignalSingleNameDeltaHigh)
	if !ok {
		t.Fatalf("missing concentration signal, signals: %+v", res.Signals)
	}
	if concentration.Severity != risk.SeverityWatch || concentration.Direction != risk.DirectionRebalance {
		t.Fatalf("concentration signal = %+v, want watch/rebalance for provisional red plus hard tape", concentration)
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
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch for confirmed SPY/VIX context without high portfolio fit", res.Direction, res.Severity)
	}
	if res.Action != canaryActionWatch || res.PlannerReadiness != risk.PlannerReadinessPrestage {
		t.Fatalf("action/readiness = %s/%s, want watch/prestage", res.Action, res.PlannerReadiness)
	}
	if !rowContains(res.Rows, "Index tape shock", "direct SPY stress is confirmed") {
		t.Fatalf("expected confirmed direct tape row, rows: %+v", res.Rows)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarketSelloffViolent)
	if !ok {
		t.Fatalf("missing selloff signal, signals: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityAct || len(sig.BlockedBy) > 0 {
		t.Fatalf("selloff signal = severity %q blocked_by %+v, want act without blocked_by", sig.Severity, sig.BlockedBy)
	}
}

func TestComputeCanaryGrossDollarDeltaCatchesOffsettingOptionBook(t *testing.T) {
	t.Parallel()
	net := 0.0
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			DollarDeltaBase: &net,
			ExposureBase: []rpc.UnderlyingExposure{
				{Underlying: "AAPL", DollarDeltaBase: new(90_000.0)},
				{Underlying: "MSFT", DollarDeltaBase: new(-90_000.0)},
			},
		}},
		Regime: redVolCreditRegimeWithComputingSlowRows(),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch until degraded inputs are clean", res.Direction, res.Severity)
	}
	if !rowContainsEvidence(res.Rows, "US equity/options exposure", "gross delta 180% NLV") {
		t.Fatalf("expected gross delta evidence, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryStressedExposureDeleverHasMatchingSignal(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.GrossPositionValue = 110_000
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  redVolCreditRegimeWithComputingSlowRows(),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch until degraded inputs are clean", res.Direction, res.Severity)
	}
	sig, ok := findSignal(res.Signals, risk.SignalGrossExposureHigh)
	if !ok {
		t.Fatalf("missing gross exposure signal, signals: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityAct || sig.Threshold == nil || *sig.Threshold != canaryPolicy.GrossExposureStressActPct {
		t.Fatalf("gross exposure signal = severity %q threshold %v, want act at stress threshold", sig.Severity, sig.Threshold)
	}
}

func TestComputeCanaryLargestDeltaConcentrationWatchesWithoutMarketStress(t *testing.T) {
	t.Parallel()
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			ExposureBase: []rpc.UnderlyingExposure{
				{Underlying: "AAPL", DollarDeltaBase: new(45_000.0)},
			},
		}},
		Regime: healthyCanaryRegime(),
	})
	if res.Direction != risk.DirectionRebalance || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want rebalance/watch on largest dollar-delta concentration", res.Direction, res.Severity)
	}
	if res.Action != canaryActionRebalance || res.PortfolioFit != canaryPortfolioFitHigh {
		t.Fatalf("decision = action %s fit %s, want rebalance/high", res.Action, res.PortfolioFit)
	}
	if res.PlannerModeHint != risk.PlannerModeRebalance || res.PlannerReadiness != risk.PlannerReadinessReady {
		t.Fatalf("planner = %s/%s, want rebalance/ready", res.PlannerModeHint, res.PlannerReadiness)
	}
	if !rowContainsEvidence(res.Rows, "Largest concentration", "AAPL delta 45% NLV") {
		t.Fatalf("expected largest-delta evidence, rows: %+v", res.Rows)
	}
	if !hasSignal(res.Signals, risk.SignalSingleNameDeltaHigh) {
		t.Fatalf("expected single-name delta signal, signals: %+v", res.Signals)
	}
	sig, ok := findSignal(res.Signals, risk.SignalSingleNameDeltaHigh)
	if !ok || sig.Direction != risk.DirectionRebalance || sig.Posture != risk.PortfolioPostureRebalance {
		t.Fatalf("single-name delta signal = %+v, want rebalance direction/posture", sig)
	}
}

func TestComputeCanaryProvisionalRedsDoNotCreateShortConvexityActSignal(t *testing.T) {
	t.Parallel()
	gamma := -1.0
	r := healthyCanaryRegime()
	r.FundingStress.Band = "red"
	r.GammaZero.Band = "red"
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			Gamma:          &gamma,
			GreeksCoverage: 1,
			GreeksTotal:    1,
		}},
		Regime: r,
		Now:    time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Market.EligibleRedClusters != 0 || res.Market.RedClusters < 2 {
		t.Fatalf("market reds = %d eligible = %d, want multiple provisional reds", res.Market.RedClusters, res.Market.EligibleRedClusters)
	}
	if sig, ok := findSignal(res.Signals, risk.SignalShortConvexityHigh); ok {
		t.Fatalf("short-convexity signal = %+v, want no act-grade convexity signal from provisional reds", sig)
	}
	if res.Severity != risk.SeverityWatch {
		t.Fatalf("severity = %s, want watch for provisional reds", res.Severity)
	}
}

func TestComputeCanaryHeldUnderlyingPnLShockRebalancesWithoutMarketConfirmation(t *testing.T) {
	t.Parallel()
	dailyLoss := -2_500.0
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{
			AsOf: time.Now(),
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "XYZ", MarketValueBase: 30_000, MarketValuePctNLV: new(30.0), DailyPnLBase: &dailyLoss,
				}},
			},
		},
		Regime: healthyCanaryRegime(),
	})
	if res.MarketConfirmation != canaryMarketNone {
		t.Fatalf("market_confirmation = %s, want none; held-name stress must not confirm market tape", res.MarketConfirmation)
	}
	if res.Direction != risk.DirectionRebalance || res.Action != canaryActionRebalance || res.PortfolioFit != canaryPortfolioFitHigh {
		t.Fatalf("decision = %s/%s fit %s, want rebalance/rebalance/high", res.Direction, res.Action, res.PortfolioFit)
	}
	sig, ok := findSignal(res.Signals, risk.SignalHeldUnderlyingPnLShock)
	if !ok {
		t.Fatalf("missing held P&L shock signal: %+v", res.Signals)
	}
	if sig.Subject != "XYZ" || sig.Direction != risk.DirectionRebalance || sig.Severity != risk.SeverityWatch {
		t.Fatalf("held P&L signal = %+v, want XYZ rebalance/watch", sig)
	}
	if len(res.Portfolio.HeldStress) != 1 || res.Portfolio.HeldStress[0].Underlying != "XYZ" {
		t.Fatalf("held_stress = %+v, want one XYZ row", res.Portfolio.HeldStress)
	}
	if !rowContainsEvidence(res.Rows, "Held-name stress", "XYZ daily P&L -2.5% NLV") {
		t.Fatalf("expected held-name evidence, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryHeldOptionExpiryConcentrationRebalances(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 4, 14, 0, 0, 0, time.UTC)
	underlying := 50.0
	delta := 0.60
	gamma := -0.03
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{
			AsOf: now,
			Options: []rpc.PositionView{{
				Symbol:     "XYZ",
				SecType:    rpc.SecTypeOption,
				Quantity:   10,
				Multiplier: 100,
				Expiry:     now.Add(5 * 24 * time.Hour).Format("20060102"),
				Delta:      &delta,
				Gamma:      &gamma,
				Underlying: &underlying,
			}},
			Portfolio: &rpc.PositionsPortfolio{
				GreeksCoverage: 1,
				GreeksTotal:    1,
			},
		},
		Regime: healthyCanaryRegime(),
		Now:    now,
	})
	sig, ok := findSignal(res.Signals, risk.SignalHeldOptionExpiryConcentration)
	if !ok {
		t.Fatalf("missing held option expiry signal: %+v", res.Signals)
	}
	if sig.Subject != "XYZ" || sig.Direction != risk.DirectionRebalance || sig.Observed == nil || *sig.Observed != 30 {
		t.Fatalf("held option signal = %+v, want XYZ rebalance at 30%% NLV", sig)
	}
	if res.Action != canaryActionRebalance || res.PortfolioFit != canaryPortfolioFitHigh {
		t.Fatalf("decision = action %s fit %s, want rebalance/high", res.Action, res.PortfolioFit)
	}
	if got := res.Portfolio.HeldStress[0].NearExpiryMinDTE; got == nil || *got != 5 {
		t.Fatalf("near_expiry_min_dte = %v, want 5", got)
	}
	if !rowContainsEvidence(res.Rows, "Held-name stress", "XYZ near-expiry delta 30% NLV at 5 DTE") {
		t.Fatalf("expected held option evidence, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryHeldLiquidityDegradedIsDataQualityOnly(t *testing.T) {
	t.Parallel()
	spread := 1.20
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{
			AsOf: time.Now(),
			Stocks: []rpc.PositionView{{
				Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: 1_000, SpreadPct: &spread, QuoteQuality: "firm",
			}},
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "XYZ", MarketValueBase: 30_000, MarketValuePctNLV: new(30.0),
				}},
			},
		},
		Regime: healthyCanaryRegime(),
	})
	if res.Direction != "" || res.Action != canaryActionStandDown {
		t.Fatalf("decision = %s/%s, want stand_down for held-liquidity-only evidence", res.Direction, res.Action)
	}
	sig, ok := findSignal(res.Signals, risk.SignalHeldLiquidityDegraded)
	if !ok || sig.Direction != risk.DirectionDataQuality {
		t.Fatalf("held liquidity signal = %+v ok=%v, want data-quality signal", sig, ok)
	}
	if !rowContains(res.Rows, "Held-name stress", "Confirm held-name quotes") {
		t.Fatalf("expected held-name data-quality row, rows: %+v", res.Rows)
	}
}

func TestComputeCanarySignalsExposureAndDecisionShape(t *testing.T) {
	t.Parallel()
	delta := 140_000.0
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			DollarDeltaBase: &delta,
			ExposureBase: []rpc.UnderlyingExposure{{
				Underlying: "LMN", MarketValueBase: 40_000, MarketValuePctNLV: new(40.0), DollarDeltaBase: new(140_000.0),
			}},
		}},
		Regime: healthyCanaryRegime(),
	})
	for _, want := range []risk.SignalID{
		risk.SignalNetDeltaHigh,
		risk.SignalSingleNameExposureHigh,
		risk.SignalSingleNameDeltaHigh,
	} {
		if !hasSignal(res.Signals, want) {
			t.Fatalf("missing signal %s in %+v", want, res.Signals)
		}
	}
	if res.Direction != risk.DirectionRebalance {
		t.Fatalf("direction = %q, want rebalance", res.Direction)
	}
	if res.Action != canaryActionRebalance || res.PortfolioFit != canaryPortfolioFitHigh || res.InputHealth != canaryInputOK {
		t.Fatalf("decision = action %s fit %s input %s, want rebalance/high/ok", res.Action, res.PortfolioFit, res.InputHealth)
	}
	if res.PlannerReadiness != risk.PlannerReadinessReady {
		t.Fatalf("planner_readiness = %q, want ready", res.PlannerReadiness)
	}
	if res.PlannerModeHint != risk.PlannerModeRebalance {
		t.Fatalf("planner_mode_hint = %s, want rebalance", res.PlannerModeHint)
	}
}

func TestComputeCanaryFastCarryUnwindActs(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 1, ClusterYellowCount: 2, ClusterGreenCount: 3, ClusterRankedCount: 6}
	r.USDJPY.Band = "red"
	r.VIXTermStructure.Band = "yellow"
	r.Breadth.Band = "yellow"
	spyPct := -2.0
	vixPct := 16.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch context without vulnerable portfolio fit", res.Direction, res.Severity)
	}
	if res.MarketConfirmation != canaryMarketConfirmed || res.PortfolioFit != canaryPortfolioFitLow || res.Action != canaryActionWatch {
		t.Fatalf("decision = market %s fit %s action %s, want confirmed/low/watch", res.MarketConfirmation, res.PortfolioFit, res.Action)
	}
	if !rowContains(res.Rows, "Fast carry unwind", "FX stress is confirmed") {
		t.Fatalf("expected fast-carry market row, rows: %+v", res.Rows)
	}
	if !hasSignal(res.Signals, risk.SignalFXCarryUnwind) {
		t.Fatalf("missing FX carry unwind signal, signals: %+v", res.Signals)
	}
}

func TestComputeCanaryConstructiveTapeShowsOpportunityPosture(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.HYGSPYDivergence.SPYChangePct = new(3.0)
	r.VIXTermStructure.VIXChangePct = new(-25.0)
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionConstructive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want constructive/watch", res.Direction, res.Severity)
	}
	if res.Action != canaryActionDeploy || res.MarketConfirmation != canaryMarketConfirmed {
		t.Fatalf("decision = action %s market %s, want deploy/confirmed", res.Action, res.MarketConfirmation)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarketRallyViolent)
	if !ok || sig.Posture != risk.PortfolioPostureOpportunity {
		t.Fatalf("market rally signal = %+v, want opportunity posture", sig)
	}
}

func TestComputeCanaryPositivePnLShockProtectsGains(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	daily := 12_000.0
	acct.DailyPnL = &daily
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  healthyCanaryRegime(),
	})
	sig, ok := findSignal(res.Signals, risk.SignalPortfolioPnLShock)
	if !ok {
		t.Fatalf("missing P&L shock signal, signals: %+v", res.Signals)
	}
	if sig.Direction != risk.DirectionDefensive || sig.Severity != risk.SeverityWatch {
		t.Fatalf("P&L signal = direction %q severity %q, want defensive/watch", sig.Direction, sig.Severity)
	}
	if sig.ConfidenceImpact == "" {
		t.Fatalf("P&L signal should explain why a gain is not directly deployable: %+v", sig)
	}
	if res.Direction != "" || res.Action != canaryActionStandDown {
		t.Fatalf("decision = direction %q action %q, want no canary action for P&L-only evidence", res.Direction, res.Action)
	}
}

func TestComputeCanaryMissingDailyPnLIsDataQuality(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.DailyPnL = nil
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  healthyCanaryRegime(),
	})
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch for missing daily P&L", res.Direction, res.Severity)
	}
	if !rowContains(res.Rows, "Portfolio P&L shock", "cannot confirm or reject") {
		t.Fatalf("expected P&L data-quality row, rows: %+v", res.Rows)
	}
	sig, ok := findSignal(res.Signals, risk.SignalRiskDataDegraded)
	if !ok || sig.Subject != "account.daily_pnl" {
		t.Fatalf("missing daily P&L data-quality signal, got %+v", res.Signals)
	}
	var accountHealth *rpc.SourceHealth
	for i := range res.SourceHealth {
		if res.SourceHealth[i].Source == "account" {
			accountHealth = &res.SourceHealth[i]
			break
		}
	}
	if accountHealth == nil || accountHealth.Status != "partial" || accountHealth.Confidence != "medium-low" {
		t.Fatalf("account source health = %+v, want partial/medium-low", accountHealth)
	}
}

func TestComputeCanaryWatchMarginSignalDoesNotPublishLowerTarget(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.Cushion = 0.30
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  healthyCanaryRegime(),
	})
	sig, ok := findSignal(res.Signals, risk.SignalMarginCushionLow)
	if !ok {
		t.Fatalf("missing margin signal, signals: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityWatch {
		t.Fatalf("margin signal severity = %q, want watch", sig.Severity)
	}
	if sig.Target != nil {
		t.Fatalf("watch margin signal target = %v, want nil because target would be below watch threshold", *sig.Target)
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
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch for standalone VIX spike", res.Direction, res.Severity)
	}
	if !rowContains(res.Rows, "Index tape shock", "flashing early stress") {
		t.Fatalf("expected early VIX tape row, rows: %+v", res.Rows)
	}
}

func TestComputeCanarySurfacesProtectionCoverage(t *testing.T) {
	t.Parallel()
	unprotected := 12_000.0
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{
			ProtectionCoverage: &rpc.ProtectionCoverageSummary{
				Status:                          "review",
				UnprotectedNotionalBase:         &unprotected,
				UnprotectedNotionalBaseCurrency: "USD",
				Counts:                          rpc.ProtectionCoverageCounts{Unprotected: 1},
				LargestUnprotected:              []rpc.ProtectionCoverageRow{{Underlying: "MSFT"}},
			},
		},
		Regime: healthyCanaryRegime(),
	})
	if res.Portfolio.ProtectionCoverage == nil {
		t.Fatal("portfolio protection coverage missing")
	}
	if !rowContains(res.Rows, "Protection coverage", "Review largest unprotected stock/ETF exposures") {
		t.Fatalf("coverage guidance missing, rows: %+v", res.Rows)
	}
	if !rowContainsEvidence(res.Rows, "Protection coverage", "unprotected $ 12,000.00") {
		t.Fatalf("coverage evidence missing, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryStaleGreenClusterStillWatches(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch when ranked market data is stale", res.Direction, res.Severity)
	}
	if res.PlannerModeHint != risk.PlannerModeConfirmData || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("planner = %s/%s, want confirm_data/blocked", res.PlannerModeHint, res.PlannerReadiness)
	}
	if got := strings.Join(res.Market.StaleClusters, ","); got != "vol" {
		t.Fatalf("stale clusters = %q, want vol", got)
	}
	if !rowContains(res.Rows, "Ambiguity filter", "Refresh or verify incomplete market inputs") {
		t.Fatalf("expected stale-data ambiguity row, rows: %+v", res.Rows)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "stale clusters: vol") {
		t.Fatalf("expected stale-cluster warning, warnings: %+v", res.Warnings)
	}
	if res.InputHealth != canaryInputDegraded {
		t.Fatalf("input_health = %q, want degraded for stale data", res.InputHealth)
	}
}

func TestComputeCanaryOffHoursVolStaleIsContext(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	r.VIXTermStructure.AsOf = &rpc.RegimeAsOfSummary{Label: "frozen", Date: "2026-06-01"}
	r.DataQuality = []rpc.DataQualityHealth{{
		Surface:       "regime",
		Status:        rpc.RegimeStatusStale,
		Summary:       "stale: vol",
		StaleClusters: []string{"vol"},
	}}
	r.WarningDetails = []rpc.RegimeWarning{{
		Code:    "vix_term_structure_stale",
		Scope:   "vix_term_structure",
		Message: "volatility term structure stale",
	}}
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	})
	if got := strings.Join(res.Market.StaleClusters, ","); got != "" {
		t.Fatalf("stale clusters = %q, want none for expected off-hours vol context", got)
	}
	if res.InputHealth != canaryInputOK {
		t.Fatalf("input_health = %q, want ok for expected off-hours vol context", res.InputHealth)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %+v, want none for expected off-hours vol context", res.Warnings)
	}
	vix, ok := findCanaryIndicator(res.MarketIndicators, "VIX/VIX3M")
	if !ok {
		t.Fatalf("missing VIX indicator: %+v", res.MarketIndicators)
	}
	if strings.Contains(strings.ToLower(vix.Comment), "stale input") || !strings.Contains(vix.Comment, "closed-session cached context") {
		t.Fatalf("VIX comment = %q, want closed-session context without stale-input action", vix.Comment)
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

func TestComputeCanaryCarriesSourceTimestamps(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.AsOf = time.Date(2026, 5, 29, 13, 1, 0, 0, time.UTC)
	posAsOf := time.Date(2026, 5, 29, 13, 2, 0, 0, time.UTC)
	regime := healthyCanaryRegime()
	regime.AsOf = time.Date(2026, 5, 29, 13, 3, 0, 0, time.UTC)
	res := ComputeCanary(CanaryInput{
		Account:   acct,
		Positions: rpc.PositionsResult{AsOf: posAsOf},
		Regime:    regime,
	})
	if !res.SourceAsOf.Account.Equal(acct.AsOf) || !res.SourceAsOf.Positions.Equal(posAsOf) || !res.SourceAsOf.Regime.Equal(regime.AsOf) {
		t.Fatalf("source_as_of = %+v, want account=%s positions=%s regime=%s", res.SourceAsOf, acct.AsOf, posAsOf, regime.AsOf)
	}
}

func TestComputeCanaryCarriesSemanticFingerprints(t *testing.T) {
	t.Parallel()
	regime := healthyCanaryRegime()
	regime.Fingerprint = rpc.BuildRegimeFingerprint(&regime)
	acct := baseCanaryAccount()
	acct.Cushion = 0.30
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  regime,
	})
	if res.Fingerprint.Version != rpc.CanaryFingerprintVersion || res.Fingerprint.Key == "" {
		t.Fatalf("canary fingerprint = %+v, want populated %s", res.Fingerprint, rpc.CanaryFingerprintVersion)
	}
	if res.SourceFingerprints.Regime == nil || *res.SourceFingerprints.Regime != regime.Fingerprint {
		t.Fatalf("source regime fingerprint = %+v, want %+v", res.SourceFingerprints.Regime, regime.Fingerprint)
	}
}

func TestComputeCanaryJSONCarriesMonitorFields(t *testing.T) {
	t.Parallel()
	regime := healthyCanaryRegime()
	regime.Fingerprint = rpc.BuildRegimeFingerprint(&regime)
	acct := baseCanaryAccount()
	acct.Cushion = 0.30
	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  regime,
	})
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	for _, key := range []string{"fingerprint", "source_fingerprints", "source_health", "action", "market_confirmation", "portfolio_fit", "input_health", "severity", "planner_mode_hint", "planner_readiness", "signals", "rows"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("canary JSON missing %s: %s", key, b)
		}
	}
	if _, ok := wire["market_indicators"]; !ok {
		t.Fatalf("canary JSON missing market_indicators: %s", b)
	}
	if wire["action"] != canaryActionStandDown {
		t.Fatalf("action = %#v, want stand_down", wire["action"])
	}
	signals, ok := wire["signals"].([]any)
	if !ok || len(signals) == 0 {
		t.Fatalf("signals missing/malformed: %#v", wire["signals"])
	}
	firstSignal, ok := signals[0].(map[string]any)
	if !ok || firstSignal["posture"] == "" {
		t.Fatalf("signal posture missing: %#v", wire["signals"])
	}
	fp, ok := wire["fingerprint"].(map[string]any)
	if !ok || fp["version"] != rpc.CanaryFingerprintVersion || fp["key"] == "" {
		t.Fatalf("fingerprint missing/malformed: %#v", wire["fingerprint"])
	}
	sources, ok := wire["source_fingerprints"].(map[string]any)
	if !ok {
		t.Fatalf("source_fingerprints missing/malformed: %#v", wire["source_fingerprints"])
	}
	for _, key := range []string{"account", "positions", "regime"} {
		fp, ok := sources[key].(map[string]any)
		if !ok || fp["key"] == "" {
			t.Fatalf("source_fingerprints.%s missing/malformed: %#v", key, sources[key])
		}
	}
	regimeFP, ok := sources["regime"].(map[string]any)
	if !ok || regimeFP["version"] != rpc.RegimeFingerprintVersion || regimeFP["key"] != regime.Fingerprint.Key {
		t.Fatalf("source_fingerprints.regime = %#v, want %+v", sources["regime"], regime.Fingerprint)
	}
	sourceHealth, ok := wire["source_health"].([]any)
	if !ok || len(sourceHealth) != 3 {
		t.Fatalf("source_health missing/malformed: %#v", wire["source_health"])
	}
	indicators, ok := wire["market_indicators"].([]any)
	if !ok || len(indicators) == 0 {
		t.Fatalf("market_indicators missing/malformed: %#v", wire["market_indicators"])
	}
	firstIndicator, ok := indicators[0].(map[string]any)
	if !ok || firstIndicator["name"] == "" || firstIndicator["status"] == "" {
		t.Fatalf("market indicator missing required scan fields: %#v", indicators[0])
	}
}

func TestComputeCanaryJSONCarriesHeldStress(t *testing.T) {
	t.Parallel()
	dailyLoss := -2_500.0
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{
			AsOf: time.Now(),
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "XYZ", MarketValueBase: 30_000, MarketValuePctNLV: new(30.0), DailyPnLBase: &dailyLoss,
				}},
			},
		},
		Regime: healthyCanaryRegime(),
	})
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	portfolio, ok := wire["portfolio"].(map[string]any)
	if !ok {
		t.Fatalf("portfolio missing/malformed: %s", b)
	}
	held, ok := portfolio["held_stress"].([]any)
	if !ok || len(held) != 1 {
		t.Fatalf("held_stress missing/malformed: %#v", portfolio["held_stress"])
	}
	first, ok := held[0].(map[string]any)
	if !ok || first["underlying"] != "XYZ" || first["daily_pnl_pct_nlv"] == nil || first["signal_ids"] == nil {
		t.Fatalf("held_stress[0] = %#v, want XYZ stress with daily P&L and signal IDs", held[0])
	}
}

func TestComputeCanaryFingerprintIgnoresTimestampsAndRawValuesInsideBucket(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.Cushion = 0.30
	regime := healthyCanaryRegime()
	first := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  regime,
		Now:     time.Date(2026, 5, 31, 8, 30, 0, 0, time.UTC),
	})

	acct.AsOf = time.Date(2026, 5, 31, 8, 35, 0, 0, time.UTC)
	acct.Cushion = 0.29
	regime.AsOf = time.Date(2026, 5, 31, 8, 36, 0, 0, time.UTC)
	second := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  regime,
		Now:     time.Date(2026, 5, 31, 8, 37, 0, 0, time.UTC),
	})
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprint changed inside same margin bucket: %v != %v", first.Fingerprint, second.Fingerprint)
	}

	acct.Cushion = 0.19
	third := ComputeCanary(CanaryInput{Account: acct, Regime: regime})
	if first.Fingerprint == third.Fingerprint {
		t.Fatal("fingerprint did not change after crossing margin severity bucket")
	}
}

func TestComputeCanaryFingerprintIncludesSourceRegimeFingerprint(t *testing.T) {
	t.Parallel()
	regime := healthyCanaryRegime()
	regime.Fingerprint = rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:a"}
	first := ComputeCanary(CanaryInput{Account: baseCanaryAccount(), Regime: regime})

	regime.Fingerprint = rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:b"}
	second := ComputeCanary(CanaryInput{Account: baseCanaryAccount(), Regime: regime})
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("canary fingerprint did not change when source regime fingerprint changed")
	}
}

func TestComputeCanaryFingerprintIncludesSourceMarketEventsFingerprint(t *testing.T) {
	t.Parallel()
	regime := healthyCanaryRegime()
	first := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  regime,
		MarketEvents: rpc.MarketEventsResult{
			Kind:        rpc.MarketEventsKind,
			Fingerprint: rpc.Fingerprint{Version: rpc.MarketEventsFingerprintVersion, Key: "sha256:market-a"},
		},
	})
	second := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  regime,
		MarketEvents: rpc.MarketEventsResult{
			Kind:        rpc.MarketEventsKind,
			Fingerprint: rpc.Fingerprint{Version: rpc.MarketEventsFingerprintVersion, Key: "sha256:market-b"},
		},
	})
	if first.SourceFingerprints.MarketEvents == nil || first.SourceFingerprints.MarketEvents.Key != "sha256:market-a" {
		t.Fatalf("source market-events fingerprint missing: %+v", first.SourceFingerprints.MarketEvents)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("canary fingerprint did not change when source market-events fingerprint changed")
	}
}

func TestComputeCanarySurfacesDegradedGammaSeparately(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityBlocked,
			RankabilityReason: "oi_observed_coverage: OI coverage below threshold",
		},
		Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
	}
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.AmbiguousClusters, ","); got != "gamma" {
		t.Fatalf("ambiguous clusters = %q, want gamma for blocked gamma", got)
	}
	if got := strings.Join(res.Market.DegradedClusters, ","); got != "gamma" {
		t.Fatalf("degraded clusters = %q, want gamma", got)
	}
	if !rowContainsEvidence(res.Rows, "Ambiguity filter", "ambiguous gamma; degraded gamma") {
		t.Fatalf("expected degraded-gamma disclosure row, rows: %+v", res.Rows)
	}
	if res.InputHealth != canaryInputDegraded {
		t.Fatalf("input_health = %q, want degraded for degraded gamma", res.InputHealth)
	}
}

func TestComputeCanaryDoesNotDegradeRankableWarningOnlyGamma(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 2, ClusterEligibleRedCount: 2, ClusterGreenCount: 4, ClusterRankedCount: 6}
	r.HYGSPYDivergence.Band = "red"
	r.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.GammaZero.Band = "red"
	r.GammaZero.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality:        rankableCanaryGammaQuality(),
		WarningDetails: []rpc.GammaWarningDetail{{Code: "oi_missing", Severity: "data_quality"}},
	}
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.DegradedClusters, ","); got != "" {
		t.Fatalf("degraded clusters = %q, want none for rankable warning-only gamma", got)
	}
	sig, ok := findSignal(res.Signals, risk.SignalRegimeStressConfirmed)
	if !ok {
		t.Fatalf("missing regime stress signal: %+v", res.Signals)
	}
	if containsString(sig.BlockedBy, "gamma") {
		t.Fatalf("regime stress signal blocked_by = %+v, want gamma usable", sig.BlockedBy)
	}
	if res.MarketConfirmation != canaryMarketConfirmed {
		t.Fatalf("market_confirmation = %s, want confirmed for rankable gamma", res.MarketConfirmation)
	}
}

func TestComputeCanaryTreatsContextOnlyGammaAsContextNotDegraded(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRankedCount: 5, ClusterUnrankedCount: 1}
	r.GammaZero.Band = "red"
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityContextOnly,
			RankabilityReason: "freshness: market is closed; cached gamma is context only",
		},
		Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
		WarningDetails: []rpc.GammaWarningDetail{{
			Code:     "spx_cache_fallback:no_data",
			Scope:    "SPX",
			Severity: "data_quality",
			Message:  "SPX live refresh was unavailable; using the last successful cached SPX slice.",
		}},
	}
	r.WarningDetails = []rpc.RegimeWarning{{
		Code:     "gamma_zero_context_only",
		Scope:    "gamma_zero",
		Severity: "info",
		Message:  "dealer gamma context_only: freshness: market is closed; cached gamma is context only",
		Impact:   "dealer gamma is displayed as context but is not ranked or used as independent stress confirmation.",
	}}
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.DegradedClusters, ","); got != "" {
		t.Fatalf("degraded clusters = %q, want none for context-only gamma", got)
	}
	if got := strings.Join(res.Market.AmbiguousClusters, ","); got != "" {
		t.Fatalf("ambiguous clusters = %q, want none for context-only gamma", got)
	}
	var out bytes.Buffer
	renderCanaryTextWidthDetails(&Env{}, &out, &res, 120, true)
	rendered := out.String()
	for _, want := range []string{
		"Market indicators",
		"STATE",
		"context",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
	for _, unwanted := range []string{
		"warning: degraded clusters: gamma",
		"Degraded input   gamma",
		"degraded gamma",
		"ambiguous clusters: gamma",
		"verify: gamma cannot confirm",
		"Gamma is after-hours/context-only. Action:",
		"Input checks",
	} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("render should not contain %q:\n%s", unwanted, rendered)
		}
	}
}

func TestComputeCanaryMarketIndicatorsCarryDecisionContext(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.VIXTermStructure.AsOf = &rpc.RegimeAsOfSummary{Date: "2026-05-29"}
	r.VIXTermStructure.Ratio = new(0.91)
	r.VIXTermStructure.VIX = new(18.0)
	r.VIXTermStructure.VIX3M = new(19.8)
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Scope:           rpc.GammaZeroScopeCombined,
		SpotUnderlying:  500.0,
		GammaSign:       "negative",
		GammaTotalAbs:   10_000_000_000,
		ZeroGamma:       nil,
		RegimeAgreement: "agree:short-gamma",
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityContextOnly,
			RankabilityReason: "freshness: market is closed; cached gamma is context only",
		},
	}

	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 5, 29, 14, 0, 0, 0, time.UTC),
	})

	vix, ok := findCanaryIndicator(res.MarketIndicators, "VIX/VIX3M")
	if !ok {
		t.Fatalf("missing VIX indicator: %+v", res.MarketIndicators)
	}
	if vix.Status != "green" || vix.AsOf != "2026-05-29" || !strings.Contains(vix.Reading, "0.910") || !strings.Contains(vix.Comment, "contango") {
		t.Fatalf("VIX indicator = %+v, want green dated reading with contango comment", vix)
	}
	gamma, ok := findCanaryIndicator(res.MarketIndicators, "γ-zero (SPY+SPX)")
	if !ok {
		t.Fatalf("missing combined gamma indicator: %+v", res.MarketIndicators)
	}
	if gamma.Status != "context" || !strings.Contains(gamma.Reading, "short-γ") || !strings.Contains(gamma.Comment, "context only") {
		t.Fatalf("gamma indicator = %+v, want context decision context", gamma)
	}
	if strings.Contains(gamma.Comment, rpc.GammaRankabilityContextOnly+"; "+rpc.GammaRankabilityContextOnly) {
		t.Fatalf("gamma comment duplicates state note: %+v", gamma)
	}
}

func TestComputeCanaryMarketSummaryCarriesTapeLevels(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.HYGSPYDivergence.SPYPrice = new(512.34)
	r.HYGSPYDivergence.SPYChangePct = new(-0.75)
	r.VIXTermStructure.VIX = new(16.34)
	r.VIXTermStructure.VIXChangePct = new(3.61)

	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
	})
	if res.Market.SPYPrice == nil || *res.Market.SPYPrice != 512.34 {
		t.Fatalf("spy_price = %v, want 512.34", res.Market.SPYPrice)
	}
	if res.Market.VIX == nil || *res.Market.VIX != 16.34 {
		t.Fatalf("vix = %v, want 16.34", res.Market.VIX)
	}
	if res.Market.SPYChangePct == nil || *res.Market.SPYChangePct != -0.75 {
		t.Fatalf("spy_change_pct = %v, want -0.75", res.Market.SPYChangePct)
	}
	if res.Market.VIXChangePct == nil || *res.Market.VIXChangePct != 3.61 {
		t.Fatalf("vix_change_pct = %v, want 3.61", res.Market.VIXChangePct)
	}
}

func TestComputeCanaryMarginPressureDoesNotActWithoutMarketConfirmation(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.Cushion = 0.12
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRankedCount: 5, ClusterUnrankedCount: 1}
	r.GammaZero.Band = "red"
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityContextOnly,
			RankabilityReason: "freshness: cached gamma is context only",
		},
	}

	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  r,
	})

	if res.Direction != "" || res.Severity != risk.SeverityObserve {
		t.Fatalf("state = %s/%s, want observe because context gamma does not block canary", res.Direction, res.Severity)
	}
	if res.Action != canaryActionStandDown || res.InputHealth != canaryInputOK {
		t.Fatalf("decision = action %s input %s, want stand_down/ok", res.Action, res.InputHealth)
	}
	if got := strings.Join(res.Market.DegradedClusters, ","); got != "" {
		t.Fatalf("degraded clusters = %q, want none for context-only gamma", got)
	}
	if hasSignal(res.Signals, risk.SignalRiskDataDegraded) {
		t.Fatalf("context gamma should not produce risk data degraded signal: %+v", res.Signals)
	}
}

func TestComputeCanaryDailyLossDoesNotActWithoutMarketConfirmation(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	daily := -11_000.0
	acct.DailyPnL = &daily
	r := healthyCanaryRegime()
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityContextOnly,
			RankabilityReason: "freshness: cached gamma is context only",
		},
	}

	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  r,
	})

	if res.Direction != "" || res.Severity != risk.SeverityObserve {
		t.Fatalf("state = %s/%s, want observe because context gamma does not turn daily P&L into canary action", res.Direction, res.Severity)
	}
	if res.Action != canaryActionStandDown || res.InputHealth != canaryInputOK {
		t.Fatalf("decision = action %s input %s, want stand_down/ok", res.Action, res.InputHealth)
	}
}

func TestComputeCanaryMarginPressureDoesNotConfirmMarketTape(t *testing.T) {
	t.Parallel()
	acct := baseCanaryAccount()
	acct.Cushion = 0.12
	r := healthyCanaryRegime()
	spyPct := -2.6
	vixPct := 2.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct

	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  r,
	})

	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch from partial market pressure plus margin exposure", res.Direction, res.Severity)
	}
	if res.Action != canaryActionWatch || res.PlannerReadiness != risk.PlannerReadinessPrestage {
		t.Fatalf("action/readiness = %s/%s, want watch/prestage", res.Action, res.PlannerReadiness)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarketSelloffViolent)
	if !ok {
		t.Fatalf("missing market selloff signal: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityWatch || !containsString(sig.BlockedBy, "confirmation") {
		t.Fatalf("selloff signal = severity %q blocked_by %+v, want watch blocked by market confirmation", sig.Severity, sig.BlockedBy)
	}
	if !rowContains(res.Rows, "Index tape shock", "needs confirmation") {
		t.Fatalf("expected tape row to require confirmation, rows: %+v", res.Rows)
	}
}

func TestComputeCanaryStaleAccountBlocksMarginAction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 29, 16, 0, 0, 0, time.UTC)
	acct := baseCanaryAccount()
	acct.AsOf = now.Add(-2 * time.Hour)
	acct.Cushion = 0.07

	res := ComputeCanary(CanaryInput{
		Account: acct,
		Regime:  healthyCanaryRegime(),
		Now:     now,
	})

	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch until account refresh", res.Direction, res.Severity)
	}
	if res.Action != canaryActionConfirmInputs || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("action/readiness = %s/%s, want confirm_inputs/blocked", res.Action, res.PlannerReadiness)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarginCushionLow)
	if !ok {
		t.Fatalf("missing margin signal: %+v", res.Signals)
	}
	if !containsString(sig.BlockedBy, "account") || sig.Confidence != "medium-low" {
		t.Fatalf("margin signal = blocked_by %+v confidence %q, want stale account block", sig.BlockedBy, sig.Confidence)
	}
	if res.InputHealth != canaryInputDegraded {
		t.Fatalf("input_health = %s, want degraded", res.InputHealth)
	}
}

func TestComputeCanaryStalePositionsBlocksRebalanceAction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 29, 16, 0, 0, 0, time.UTC)
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{
			AsOf: now.Add(-2 * time.Hour),
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{
					{Underlying: "AAPL", DollarDeltaBase: new(45_000.0)},
				},
			},
		},
		Regime: healthyCanaryRegime(),
		Now:    now,
	})

	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch from stale position exposure", res.Direction, res.Severity)
	}
	if res.Action != canaryActionConfirmInputs || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("action/readiness = %s/%s, want confirm_inputs/blocked until positions refresh", res.Action, res.PlannerReadiness)
	}
	sig, ok := findSignal(res.Signals, risk.SignalSingleNameDeltaHigh)
	if !ok {
		t.Fatalf("missing single-name delta signal: %+v", res.Signals)
	}
	if !containsString(sig.BlockedBy, "positions") || sig.Confidence != "medium-low" {
		t.Fatalf("single-name signal = blocked_by %+v confidence %q, want stale positions block", sig.BlockedBy, sig.Confidence)
	}
}

func TestComputeCanaryStalePositionsBlockHeldStressAction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 29, 16, 0, 0, 0, time.UTC)
	dailyLoss := -2_500.0
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{
			AsOf: now.Add(-2 * time.Hour),
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "XYZ", MarketValueBase: 30_000, MarketValuePctNLV: new(30.0), DailyPnLBase: &dailyLoss,
				}},
			},
		},
		Regime: healthyCanaryRegime(),
		Now:    now,
	})
	if res.Direction != risk.DirectionDataQuality || res.Action != canaryActionConfirmInputs {
		t.Fatalf("decision = %s/%s, want confirm_inputs while held-stress positions are stale", res.Direction, res.Action)
	}
	sig, ok := findSignal(res.Signals, risk.SignalHeldUnderlyingPnLShock)
	if !ok {
		t.Fatalf("missing held P&L signal: %+v", res.Signals)
	}
	if !containsString(sig.BlockedBy, "positions") || sig.Confidence != "medium-low" {
		t.Fatalf("held P&L signal = blocked_by %+v confidence %q, want stale positions block", sig.BlockedBy, sig.Confidence)
	}
}

func TestComputeCanaryStaleRedClusterDoesNotConfirmLifecycle(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 2, ClusterGreenCount: 4, ClusterRankedCount: 6}
	r.VIXTermStructure.Band = "red"
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	r.VolOfVol.Band = "red"
	r.HYGSPYDivergence.Band = "red"
	r.CreditSpreads.Band = "red"

	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})

	// Pre-eligibility this emitted an act-grade confirmed signal with a
	// BlockedBy disclosure. Under the confirmation gates a stale red is
	// not eligible at all, so the confirmed signal must not fire — the
	// visible reds warn through the early signal instead.
	if _, confirmed := findSignal(res.Signals, risk.SignalRegimeStressConfirmed); confirmed {
		t.Fatalf("stale red clusters must not emit confirmed stress: %+v", res.Signals)
	}
	if _, ok := findSignal(res.Signals, risk.SignalRegimeStressEarly); !ok {
		t.Fatalf("missing early regime stress signal: %+v", res.Signals)
	}
	if res.MarketConfirmation == canaryMarketConfirmed {
		t.Fatalf("market_confirmation = %s, want stale vol to block confirmed market stress", res.MarketConfirmation)
	}
	if res.InputHealth != canaryInputDegraded {
		t.Fatalf("input_health = %s, want degraded", res.InputHealth)
	}
}

func TestComputeCanaryRegimeSourceHealthKeepsStaleAndDegradedNotes(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 4, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	r.GammaZero.Band = "red"
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityBlocked,
			RankabilityReason: "freshness: cache age exceeds TTL",
		},
		Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
	}
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	var regimeHealth *rpc.SourceHealth
	for i := range res.SourceHealth {
		if res.SourceHealth[i].Source == "regime" {
			regimeHealth = &res.SourceHealth[i]
			break
		}
	}
	if regimeHealth == nil {
		t.Fatalf("missing regime source health: %+v", res.SourceHealth)
	}
	if regimeHealth.Status != "partial" {
		t.Fatalf("regime source status = %q, want partial", regimeHealth.Status)
	}
	notes := strings.Join(regimeHealth.Notes, "\n")
	for _, want := range []string{"stale clusters: vol", "degraded clusters: gamma"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("regime source notes = %q, want %q", notes, want)
		}
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

func TestComputeCanaryOptionsPresentWithoutGreeksIsDataQuality(t *testing.T) {
	t.Parallel()
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{
			AsOf:    time.Now(),
			Options: []rpc.PositionView{{Symbol: "SPY", SecType: rpc.SecTypeOption, Quantity: 1}},
		},
		Regime: healthyCanaryRegime(),
	})
	if !rowContains(res.Rows, "Options convexity", "greeks coverage is unavailable") {
		t.Fatalf("expected options data-quality row, rows: %+v", res.Rows)
	}
	sig, ok := findSignal(res.Signals, risk.SignalOptionGreeksDegraded)
	if !ok || sig.Direction != risk.DirectionDataQuality {
		t.Fatalf("missing option greeks degraded signal, signals: %+v", res.Signals)
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

func TestCanaryWarningLabelsAreActionOriented(t *testing.T) {
	t.Parallel()
	tests := []struct {
		warning string
		want    string
	}{
		{warning: "ambiguous clusters: funding and gamma", want: "verify"},
		{warning: "funding_stress: funding spread row is unranked; the composite has lower coverage.", want: "verify"},
		{warning: "stale clusters: vol", want: "refresh"},
		{warning: "gamma_zero: dealer gamma context_only: freshness: market is closed; cached gamma is context only", want: "context"},
		{warning: "gamma_zero: dealer gamma blocked", want: "warning"},
		{warning: "credit_spreads: source error", want: "error"},
	}
	for _, tt := range tests {
		got, _ := canaryWarningLabel(tt.warning)
		if got != tt.want {
			t.Fatalf("canaryWarningLabel(%q) = %q, want %q", tt.warning, got, tt.want)
		}
	}
}

func TestCanaryWarningsPreferScopedInputChecks(t *testing.T) {
	t.Parallel()
	warnings := canaryWarnings(CanaryMarketSummary{
		AmbiguousClusters: []string{"funding"},
		UnrankedClusters:  1,
	}, rpc.RegimeSnapshotResult{WarningDetails: []rpc.RegimeWarning{
		{Scope: "funding_stress", Message: "funding spread row is unranked; the composite has lower coverage."},
		{Scope: "gamma_zero", Message: "dealer gamma context_only: freshness: market is closed; cached gamma is context only"},
		{Scope: "vix_term_structure", Message: "volatility term structure stale"},
	}}, time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC))
	got := strings.Join(warnings, "\n")
	for _, unwanted := range []string{"ambiguous clusters:", "stale clusters:", "regime cluster(s) unranked"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("generic warning %q should be suppressed when scoped detail exists: %+v", unwanted, warnings)
		}
	}
	for _, unwanted := range []string{"gamma_zero:", "vix_term_structure:"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("context warning %q should be suppressed: %+v", unwanted, warnings)
		}
	}
	for _, want := range []string{"funding_stress:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("scoped warning %q missing: %+v", want, warnings)
		}
	}
}

func TestRenderCanaryTextShowsActionEvidenceAndInputHealth(t *testing.T) {
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
		"Action     WATCH",
		"Guidance   Market stress is confirmed, but current portfolio exposure is low; keep watch without staging reductions.",
		"Next step  Stage defensive review",
		"Why this fired",
		"Market weather",
		"Portfolio shape",
		"Combined read",
		"Market indicators",
		"INDICATOR",
		"STATE",
		"READING / COMMENT",
		"Input checks",
		"computing",
		"breadth and gamma",
		"Alert ID   canary-fp-v1 sha256:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Posture") || strings.Contains(got, "Lifecycle") || strings.Contains(got, "Confidence") {
		t.Fatalf("render leaked deleted hero fields:\n%s", got)
	}
	if strings.Contains(got, "Title                        Risk state") {
		t.Fatalf("default render should not use wide details table:\n%s", got)
	}
}

func TestCanaryInputHealthRowsHumanizeMarketIssues(t *testing.T) {
	t.Parallel()
	res := CanaryResult{
		InputHealth: canaryInputDegraded,
		Market: CanaryMarketSummary{
			DegradedClusters: []string{"gamma"},
			StaleClusters:    []string{"credit", "fx", "vol"},
		},
	}
	rows := canaryInputHealthRows(&res)
	got := fmt.Sprint(rows)
	for _, want := range []string{
		"Degraded input gamma",
		"Stale input credit, FX, and vol",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("input health rows missing %q: %s", want, got)
		}
	}
}

func TestRenderCanaryDetailsShowsRowsWhenRequested(t *testing.T) {
	t.Parallel()
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  healthyCanaryRegime(),
	})
	var out bytes.Buffer
	renderCanaryTextWidthDetails(&Env{}, &out, &res, 100, true)
	if !strings.Contains(out.String(), "Details") || !strings.Contains(out.String(), "Immediate margin safety") {
		t.Fatalf("details render missing row evidence:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Portfolio canary ·") {
		t.Fatalf("details render should not duplicate the top-level canary row:\n%s", out.String())
	}
}

func TestRenderCanaryTextWrapsAtCommonTerminalWidths(t *testing.T) {
	t.Parallel()
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  redVolCreditRegimeWithComputingSlowRows(),
		Now:     time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
	})
	res.Warnings = append(res.Warnings,
		"vix_term_structure: volatility term structure stale",
		"breadth: breadth is still computing.",
		"long_detail: "+strings.Repeat("after-hours-market-data-limitation ", 5),
	)

	for _, width := range []int{80, 100, 120} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			for _, color := range []bool{false, true} {
				var out bytes.Buffer
				renderCanaryTextWidth(&Env{Color: color}, &out, &res, width)
				for i, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
					if got := visibleLen(line); got > width {
						t.Fatalf("line %d visible width = %d, want <= %d:\n%s\nfull output:\n%s", i+1, got, width, line, out.String())
					}
				}
			}
		})
	}
}

func TestRenderCanaryTextHidesDetailsUnlessRequested(t *testing.T) {
	t.Parallel()
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  redVolCreditRegimeWithComputingSlowRows(),
		Now:     time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
	})
	var normal bytes.Buffer
	renderCanaryTextWidth(&Env{}, &normal, &res, 120)
	if strings.Contains(normal.String(), "  Details\n") {
		t.Fatalf("default canary render should hide full details:\n%s", normal.String())
	}
	if strings.Contains(normal.String(), "Title                        Risk state") {
		t.Fatalf("default canary render should not use wide table:\n%s", normal.String())
	}

	var details bytes.Buffer
	renderCanaryTextWidthDetails(&Env{}, &details, &res, 120, true)
	if !strings.Contains(details.String(), "  Details\n") {
		t.Fatalf("details canary render should use stacked details:\n%s", details.String())
	}
	if strings.Contains(details.String(), "Title                        Risk state") {
		t.Fatalf("details canary render should not use wide table:\n%s", details.String())
	}
}

func TestRenderCanaryTextColorsCurrentState(t *testing.T) {
	t.Parallel()
	res := CanaryResult{
		AsOf:             time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
		Action:           canaryActionWatch,
		Direction:        risk.DirectionDefensive,
		Severity:         risk.SeverityWatch,
		PlannerModeHint:  risk.PlannerModeStage,
		PlannerReadiness: risk.PlannerReadinessPrestage,
		Summary:          "Freeze new risk.",
		Rows: []CanaryRow{{
			Title:     "Portfolio canary",
			Direction: risk.DirectionDefensive,
			Severity:  risk.SeverityWatch,
			Guidance:  "Freeze new risk.",
		}},
	}
	var out bytes.Buffer
	renderCanaryText(&Env{Color: true}, &out, &res)
	got := out.String()
	if !strings.Contains(got, ansiBold) || !strings.Contains(got, ansiYellow) || !strings.Contains(got, "WATCH") {
		t.Fatalf("current watch action is not bold yellow:\n%q", got)
	}
	if strings.Contains(got, "CURRENT") {
		t.Fatalf("render should not repeat CURRENT:\n%q", got)
	}
}

func baseCanaryAccount() rpc.AccountResult {
	dailyPnL := 0.0
	return rpc.AccountResult{
		BaseCurrency:       "USD",
		NetLiquidation:     100_000,
		ExcessLiquidity:    50_000,
		Cushion:            0.50,
		GrossPositionValue: 60_000,
		DailyPnL:           &dailyPnL,
		AsOf:               time.Now(),
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
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					Quality: rankableCanaryGammaQuality(),
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
	}
}

func rankableCanaryGammaQuality() *rpc.GammaSignalQuality {
	return &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable}
}

func redVolCreditRegimeWithComputingSlowRows() rpc.RegimeSnapshotResult {
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 2, ClusterEligibleRedCount: 2, ClusterGreenCount: 2, ClusterRankedCount: 4, ClusterUnrankedCount: 2}
	r.VIXTermStructure.Band = "red"
	r.VIXTermStructure.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.VolOfVol.Band = "red"
	r.VolOfVol.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.HYGSPYDivergence.Band = "red"
	r.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.CreditSpreads.Band = "red"
	r.CreditSpreads.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.GammaZero.Band = ""
	r.GammaZero.Status = rpc.RegimeStatusComputing
	r.Breadth.Band = ""
	r.Breadth.Status = rpc.RegimeStatusComputing
	return r
}

func rowContains(rows []CanaryRow, title, text string) bool {
	for _, row := range rows {
		if row.Title == title && strings.Contains(row.Guidance, text) {
			return true
		}
	}
	return false
}

func rowContainsEvidence(rows []CanaryRow, title, text string) bool {
	for _, row := range rows {
		if row.Title == title && strings.Contains(row.Evidence, text) {
			return true
		}
	}
	return false
}

func hasSignal(signals []risk.Signal, id risk.SignalID) bool {
	_, ok := findSignal(signals, id)
	return ok
}

func findSignal(signals []risk.Signal, id risk.SignalID) (risk.Signal, bool) {
	for _, sig := range signals {
		if sig.ID == id {
			return sig, true
		}
	}
	return risk.Signal{}, false
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func findCanaryIndicator(indicators []CanaryMarketIndicator, name string) (CanaryMarketIndicator, bool) {
	for _, indicator := range indicators {
		if indicator.Name == name {
			return indicator, true
		}
	}
	return CanaryMarketIndicator{}, false
}
