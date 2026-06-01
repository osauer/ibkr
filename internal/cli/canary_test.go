package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/risk"
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
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch for ambiguous all-unranked market", res.Direction, res.Severity)
	}
	if res.PortfolioPosture != risk.PortfolioPostureConfirmData {
		t.Fatalf("portfolio_posture = %s, want confirm_data", res.PortfolioPosture)
	}
	if res.PlannerModeHint != risk.PlannerModeConfirmData || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("planner = %s/%s, want confirm_data/blocked", res.PlannerModeHint, res.PlannerReadiness)
	}
	if !rowContains(res.Rows, "Ambiguity filter", "incomplete or stale") {
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
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityAct {
		t.Fatalf("state = %s/%s, want defensive/act on confirmed vol+credit stress with high exposure", res.Direction, res.Severity)
	}
	if res.PlannerModeHint != risk.PlannerModeDefend || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("planner = %s/%s, want defend/blocked until degraded inputs are confirmed", res.PlannerModeHint, res.PlannerReadiness)
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
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityUrgent {
		t.Fatalf("state = %s/%s, want defensive/urgent on margin cushion below 10%%", res.Direction, res.Severity)
	}
	if res.PlannerModeHint != risk.PlannerModeDefend || res.PlannerReadiness != risk.PlannerReadinessReady {
		t.Fatalf("planner = %s/%s, want defend/ready for immediate margin danger", res.PlannerModeHint, res.PlannerReadiness)
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
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityUrgent {
		t.Fatalf("state = %s/%s, want defensive/urgent when active margin account has zero cushion", res.Direction, res.Severity)
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
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityUrgent {
		t.Fatalf("state = %s/%s, want defensive/urgent when look-ahead cushion is below 10%%", res.Direction, res.Severity)
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
	})
	if len(res.Lifecycle.ConfirmedBy) != 0 {
		t.Fatalf("confirmed_by = %+v, want none for one red gamma cluster", res.Lifecycle.ConfirmedBy)
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
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityAct {
		t.Fatalf("state = %s/%s, want defensive/act for confirmed SPY/VIX shock plus red cluster", res.Direction, res.Severity)
	}
	if res.PlannerModeHint != risk.PlannerModeDefend || res.PlannerReadiness != risk.PlannerReadinessReady {
		t.Fatalf("planner = %s/%s, want defend/ready", res.PlannerModeHint, res.PlannerReadiness)
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
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityUrgent {
		t.Fatalf("state = %s/%s, want defensive/urgent on 180%% gross delta in confirmed stress", res.Direction, res.Severity)
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
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityAct {
		t.Fatalf("state = %s/%s, want defensive/act on stressed gross exposure", res.Direction, res.Severity)
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
	if res.PortfolioPosture != risk.PortfolioPostureRebalance {
		t.Fatalf("portfolio_posture = %s, want rebalance", res.PortfolioPosture)
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
	if res.SignalConfidence != "high" {
		t.Fatalf("signal_confidence = %q, want high", res.SignalConfidence)
	}
}

func TestComputeCanarySignalsExposureAndConfidenceReasons(t *testing.T) {
	t.Parallel()
	delta := 140_000.0
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			DollarDeltaBase: &delta,
			ExposureBase: []rpc.UnderlyingExposure{{
				Underlying: "NOW", MarketValueBase: 40_000, MarketValuePctNLV: new(40.0), DollarDeltaBase: new(140_000.0),
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
	if res.DataConfidence != "high" || res.SignalConfidence != "high" || res.PlannerReadiness != risk.PlannerReadinessReady {
		t.Fatalf("confidence profile = data %q signals %q readiness %q", res.DataConfidence, res.SignalConfidence, res.PlannerReadiness)
	}
	if res.PlannerModeHint != risk.PlannerModeRebalance {
		t.Fatalf("planner_mode_hint = %s, want rebalance", res.PlannerModeHint)
	}
	if !strings.Contains(strings.Join(res.ConfidenceReasons, "\n"), "portfolio breach lacks independent market-stress confirmation") {
		t.Fatalf("missing confidence reason, got %+v", res.ConfidenceReasons)
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
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityAct {
		t.Fatalf("state = %s/%s, want defensive/act for FX carry unwind with tape confirmation", res.Direction, res.Severity)
	}
	if res.PortfolioPosture != risk.PortfolioPostureThreat {
		t.Fatalf("portfolio_posture = %s, want threat", res.PortfolioPosture)
	}
	if res.PlannerModeHint != risk.PlannerModeDefend || res.PlannerReadiness != risk.PlannerReadinessReady {
		t.Fatalf("planner = %s/%s, want defend/ready", res.PlannerModeHint, res.PlannerReadiness)
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
	})
	if res.Direction != risk.DirectionConstructive || res.Severity != risk.SeverityAct {
		t.Fatalf("state = %s/%s, want constructive/act", res.Direction, res.Severity)
	}
	if res.PortfolioPosture != risk.PortfolioPostureOpportunity {
		t.Fatalf("portfolio_posture = %s, want opportunity", res.PortfolioPosture)
	}
	if res.PlannerModeHint != risk.PlannerModeDeploy || res.PlannerReadiness != risk.PlannerReadinessReady {
		t.Fatalf("planner = %s/%s, want deploy/ready", res.PlannerModeHint, res.PlannerReadiness)
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
	if res.Direction != risk.DirectionDefensive {
		t.Fatalf("direction = %q, want defensive", res.Direction)
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

func TestComputeCanaryStaleGreenClusterStillWatches(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
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
	if !rowContains(res.Rows, "Ambiguity filter", "incomplete or stale") {
		t.Fatalf("expected stale-data ambiguity row, rows: %+v", res.Rows)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "stale clusters: vol") {
		t.Fatalf("expected stale-cluster warning, warnings: %+v", res.Warnings)
	}
	if res.Confidence != "medium-low" {
		t.Fatalf("confidence = %q, want medium-low for stale data", res.Confidence)
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
	for _, key := range []string{"fingerprint", "source_fingerprints", "source_health", "lifecycle", "direction", "portfolio_posture", "severity", "planner_mode_hint", "planner_readiness", "signals", "rows"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("canary JSON missing %s: %s", key, b)
		}
	}
	if wire["portfolio_posture"] != string(risk.PortfolioPostureThreat) {
		t.Fatalf("portfolio_posture = %#v, want threat", wire["portfolio_posture"])
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
	lifecycle, ok := wire["lifecycle"].(map[string]any)
	if !ok || lifecycle["stage"] == "" || lifecycle["fingerprint"] == nil {
		t.Fatalf("lifecycle missing/malformed: %#v", wire["lifecycle"])
	}
	sourceHealth, ok := wire["source_health"].([]any)
	if !ok || len(sourceHealth) != 3 {
		t.Fatalf("source_health missing/malformed: %#v", wire["source_health"])
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

func TestComputeCanaryRegimeSourceHealthKeepsStaleAndDegradedNotes(t *testing.T) {
	t.Parallel()
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 4, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	r.GammaZero.Band = "red"
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
	}
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  r,
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
	if regimeHealth.Status != "degraded" {
		t.Fatalf("regime source status = %q, want degraded", regimeHealth.Status)
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

func TestRenderCanaryTextShowsRiskStateAndNextStep(t *testing.T) {
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
		"Risk state [Defensive / Act]",
		"Posture    Threat",
		"Alert ID   canary-fp-v1 sha256:",
		"Guidance   Refresh or confirm degraded inputs before planning major portfolio changes.",
		"Confidence Medium-low (data:",
		"breadth and gamma computing",
		"Next step  Confirm data before defend",
		"Title                        Risk state",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "Guidance") > strings.Index(got, "Confidence") {
		t.Fatalf("guidance should render before confidence:\n%s", got)
	}
	if strings.Index(got, "Why") > strings.Index(got, "Next step") {
		t.Fatalf("next step should render below why:\n%s", got)
	}
}

func TestCanaryConfidenceExplanationHumanizesReasons(t *testing.T) {
	t.Parallel()
	got := canaryConfidenceExplanation(&CanaryResult{
		Confidence:       "medium-low",
		DataConfidence:   "medium-low",
		SignalConfidence: "high",
		ConfidenceReasons: []string{
			"degraded clusters: gamma",
			"stale clusters: credit,fx,vol",
			"portfolio breach lacks independent market-stress confirmation",
		},
	})
	for _, want := range []string{
		"gamma degraded",
		"credit, FX, and vol stale",
		"portfolio breach lacks independent market-stress confirmation",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("explanation missing %q: %s", want, got)
		}
	}
}

func TestCanaryInlineConfidenceReasonSeparatesDataAndConfirmation(t *testing.T) {
	t.Parallel()
	got := canaryInlineConfidenceReason([]string{
		"degraded clusters: gamma",
		"stale clusters: credit,fx,vol",
		"portfolio breach lacks independent market-stress confirmation",
	})
	want := "data: gamma degraded, credit, FX, and vol stale; confirmation missing"
	if got != want {
		t.Fatalf("inline confidence reason = %q, want %q", got, want)
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

func TestRenderCanaryTextUsesStackedRowsBelowWideTerminals(t *testing.T) {
	t.Parallel()
	res := ComputeCanary(CanaryInput{
		Account: baseCanaryAccount(),
		Regime:  redVolCreditRegimeWithComputingSlowRows(),
		Now:     time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
	})
	var narrow bytes.Buffer
	renderCanaryTextWidth(&Env{}, &narrow, &res, 100)
	if !strings.Contains(narrow.String(), "  Details\n") {
		t.Fatalf("narrow canary render should use stacked details:\n%s", narrow.String())
	}
	if strings.Contains(narrow.String(), "Title                        Risk state") {
		t.Fatalf("narrow canary render should not use wide table:\n%s", narrow.String())
	}

	var wide bytes.Buffer
	renderCanaryTextWidth(&Env{}, &wide, &res, 120)
	if !strings.Contains(wide.String(), "Title                        Risk state") {
		t.Fatalf("wide canary render should use table:\n%s", wide.String())
	}
}

func TestRenderCanaryTextColorsCurrentState(t *testing.T) {
	t.Parallel()
	res := CanaryResult{
		AsOf:             time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
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
	if !strings.Contains(got, ansiBold+ansiYellow+"[Defensive / Watch]"+ansiReset+ansiReset) {
		t.Fatalf("current defensive/watch state is not bold yellow:\n%q", got)
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
