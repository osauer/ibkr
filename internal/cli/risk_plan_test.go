package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
)

func TestRiskPlanCurrentPortfolioFixtureRanksNuancedReductions(t *testing.T) {
	t.Parallel()
	in := currentPortfolioRiskPlanFixture()
	plan := ComputeRiskPlan(in)
	if plan.Kind != rpc.RiskPlanKind || plan.SchemaVersion != rpc.RiskPlanSchemaVersion {
		t.Fatalf("shape = %s/%s", plan.Kind, plan.SchemaVersion)
	}
	if plan.PolicyProfile != "active-v1" || plan.PolicyVersion != "risk-policy-v1" || plan.PolicyFingerprint.Key == "" {
		t.Fatalf("policy fields not populated: %+v", plan.PolicyFingerprint)
	}
	if plan.ResponseMode != rpc.RiskPlanModeDefend {
		t.Fatalf("response mode = %q, want defend due margin/P&L act signals", plan.ResponseMode)
	}
	if len(plan.Candidates) < 3 {
		t.Fatalf("got %d candidates, want BB/NOW/CRWV", len(plan.Candidates))
	}
	if plan.Candidates[0].Subject != "BB" {
		t.Fatalf("first candidate = %s, want BB concentration first", plan.Candidates[0].Subject)
	}
	bb := plan.Candidates[0]
	if len(bb.Legs) != 2 {
		t.Fatalf("BB legs = %d, want 2 legs (10C then 9C)", len(bb.Legs))
	}
	if bb.Legs[0].Contract.Strike != 10 || bb.Legs[0].Quantity != 300 {
		t.Fatalf("first BB leg = %+v, want sell 300 10C", bb.Legs[0])
	}
	if bb.Legs[1].Contract.Strike != 9 || bb.Legs[1].Quantity != 20 {
		t.Fatalf("second BB leg = %+v, want sell 20 9C", bb.Legs[1])
	}
	if bb.EstimatedRiskAfter.LargestExposurePctNLV == nil || *bb.EstimatedRiskAfter.LargestExposurePctNLV > 26 {
		t.Fatalf("BB after exposure = %v, want near 25%%", bb.EstimatedRiskAfter.LargestExposurePctNLV)
	}
	nowFront := findCandidateByIntent(plan, "reduce_front_dte_loss_and_gamma")
	if nowFront == nil {
		t.Fatalf("missing NOW front-DTE candidate: %+v", plan.Candidates)
	}
	if len(nowFront.Legs) != 1 || nowFront.Legs[0].Contract.Symbol != "NOW" || nowFront.Legs[0].Contract.Expiry != "20260618" {
		t.Fatalf("NOW front candidate = %+v", nowFront.Legs)
	}
	if dte := nowFront.Legs[0].DTE; dte == nil || *dte != 15 {
		t.Fatalf("NOW front DTE = %v, want 15", nowFront.Legs[0].DTE)
	}
	if findCandidateBySubject(plan, "SPY") != nil {
		t.Fatalf("SPY hedge should be preserved, candidates: %+v", plan.Candidates)
	}
	if !containsText(plan.Warnings, "mark_outside_bid_ask") {
		t.Fatalf("warnings should retain SPY mark_outside_bid_ask, got %+v", plan.Warnings)
	}
	if findCandidateBySubject(plan, "HGENQ") != nil {
		t.Fatalf("zero-value HGENQ must not produce a candidate")
	}
}

func TestRiskPlanCandidateIDsStable(t *testing.T) {
	t.Parallel()
	in := currentPortfolioRiskPlanFixture()
	a := ComputeRiskPlan(in)
	b := ComputeRiskPlan(in)
	if a.PlanID != b.PlanID {
		t.Fatalf("plan IDs differ: %s vs %s", a.PlanID, b.PlanID)
	}
	for i := range a.Candidates {
		if a.Candidates[i].ID != b.Candidates[i].ID {
			t.Fatalf("candidate %d IDs differ: %s vs %s", i, a.Candidates[i].ID, b.Candidates[i].ID)
		}
	}
}

func TestOrderPreviewFromPlanBlocksSubmitWithoutBrokerWhatIf(t *testing.T) {
	t.Parallel()
	plan := ComputeRiskPlan(currentPortfolioRiskPlanFixture())
	path := filepath.Join(t.TempDir(), "plan.json")
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := runOrderPreviewFromPlan(context.Background(), env, []string{"--from-plan", path, "--candidate", plan.Candidates[0].ID, "--json"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 blocked preview; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var res rpc.RiskPlanOrderPreviewResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("preview JSON: %v\n%s", err, stdout.String())
	}
	if res.SubmitEligible || res.TokenMinted || res.Executable {
		t.Fatalf("preview should not be submit eligible: %+v", res)
	}
	if !containsText(res.Blockers, "broker_whatif_unavailable") {
		t.Fatalf("missing what-if blocker: %+v", res.Blockers)
	}
}

func currentPortfolioRiskPlanFixture() RiskPlanInput {
	now := time.Date(2026, 6, 3, 21, 42, 0, 0, time.FixedZone("CEST", 2*60*60))
	nlv := 255998.0
	base := "EUR"
	dailyPnL := -42051.0
	cushion := 11.378604520347816
	dailyPct := -16.427390359493423
	netDeltaPct := 150.6044945272435
	grossDeltaPct := 629.6746813305166
	bbPct := 44.04658352214047
	crwvPct := 26.48931500368287
	nowPct := 22.643820964713647
	rddtPct := 11.89574611807858
	spyPct := 8.795841090723872
	spyDeltaPct := 239.53509340163657
	acct := rpc.AccountResult{
		AccountID:            "U123",
		BaseCurrency:         base,
		NetLiquidation:       nlv,
		ExcessLiquidity:      nlv * cushion / 100,
		MaintenanceMargin:    200000,
		GrossPositionValue:   nlv * 1.1387081149071476,
		Cushion:              cushion / 100,
		LookAheadExcess:      nlv * cushion / 100,
		LookAheadMaintMargin: 200000,
		DailyPnL:             &dailyPnL,
		AsOf:                 now,
	}
	pos := rpc.PositionsResult{
		AsOf:      now,
		AccountID: "U123",
		Stocks: []rpc.PositionView{
			stock("CRWV", 700, 96.87445231875438, 67812.11662312807),
			stock("HGENQ", 1000, 0, 0),
		},
		Options: []rpc.PositionView{
			opt("BB", "20260717", "C", 8, 50, 11664.981959573925, 0.787045147407975, 2.65, 2.83, 22338.73901835519*11664.981959573925/112756.91466432162),
			opt("BB", "20260717", "C", 9, 300, 55947.033998767125, 0.6880497446169536, 2.07, 2.27, 22338.73901835519*55947.033998767125/112756.91466432162),
			opt("BB", "20260717", "C", 10, 300, 45144.412640799375, 0.5940819519258875, 1.73, 1.83, 22338.73901835519*45144.412640799375/112756.91466432162),
			opt("NOW", "20260618", "C", 144, 30, 2952.126399372357, 0.12716340082256106, 1.00, 1.30, -6669.35823632792),
			opt("NOW", "20260717", "C", 130, 50, 40463.836166204695, 0.3951910443396816, 6.70, 7.00, -21415.322132482645),
			opt("NOW", "20260821", "C", 115, 10, 14551.516344151838, 0.6094004345260057, 16.80, 17.20, -13020.720860117422),
			opt("RDDT", "20260717", "C", 175, 15, 30452.872147358805, 0.49018640625080406, 13.85, 15.00, -3801.4122028813085),
			spyPut(),
		},
	}
	dollarDelta := 383490.675692331
	pos.Portfolio = &rpc.PositionsPortfolio{
		DollarDeltaBase:    &dollarDelta,
		BaseCurrency:       base,
		NetLiquidationBase: &nlv,
		GreeksCoverage:     8,
		GreeksTotal:        8,
		ExposureBase: []rpc.UnderlyingExposure{
			exposure("BB", 112756.91466432162, bbPct, 367772.4022388838, 4720.159849450321),
			exposure("CRWV", 67812.11662312807, crwvPct, 67812.11662312807, -3125.2946714976774),
			exposure("NOW", 57967.72879324764, nowPct, 383522.2779114541, -41742.26244504438),
			exposure("RDDT", 30452.872147358805, rddtPct, 177580.99719756594, 1284.4459086342854),
			exposure("SPY", 22517.177275431295, spyPct, -613197.1182787009, 299.7095707440487),
			exposure("HGENQ", 0, 0, 0, 0),
		},
	}
	canary := rpc.CanaryResult{
		AsOf:        now,
		Fingerprint: rpc.Fingerprint{Version: rpc.CanaryFingerprintVersion, Key: "sha256:98ef36792f354611b74c995503e40c1b871b34c0abaad16223411a0a26c24baf"},
		SourceFingerprints: rpc.CanarySourceFingerprints{
			Account:   &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:acct"},
			Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:pos"},
			Regime:    &rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:regime"},
		},
		Policy:             risk.DefaultPolicy().PolicyProfile(),
		PolicyProfile:      risk.DefaultPolicy().PolicyProfile(),
		PolicyVersion:      risk.DefaultPolicy().PolicyVersion(),
		PolicyFingerprint:  rpc.Fingerprint{Version: risk.PolicyFingerprintVersion, Key: risk.DefaultPolicy().FingerprintKey()},
		Action:             "watch",
		MarketConfirmation: "partial",
		PortfolioFit:       "high",
		InputHealth:        "ok",
		Direction:          risk.DirectionDefensive,
		Severity:           risk.SeverityWatch,
		PlannerModeHint:    risk.PlannerModeStage,
		PlannerReadiness:   risk.PlannerReadinessPrestage,
		Summary:            "Market pressure is developing and the portfolio is exposed; freeze new risk and stage reductions.",
		PrimaryDrivers:     []risk.SignalID{risk.SignalMarginCushionLow, risk.SignalLookAheadCushionLow, risk.SignalPortfolioPnLShock, risk.SignalNetDeltaHigh, risk.SignalGrossDeltaHigh},
		Signals: []risk.Signal{
			{ID: risk.SignalMarginCushionLow, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Observed: &cushion, Threshold: new(20.0), Target: new(25.0)},
			{ID: risk.SignalLookAheadCushionLow, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Observed: &cushion, Threshold: new(20.0), Target: new(25.0)},
			{ID: risk.SignalPortfolioPnLShock, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Observed: &dailyPct, Threshold: new(10.0)},
			{ID: risk.SignalNetDeltaHigh, Direction: risk.DirectionRebalance, Severity: risk.SeverityWatch, Observed: &netDeltaPct, Threshold: new(125.0)},
			{ID: risk.SignalGrossDeltaHigh, Direction: risk.DirectionRebalance, Severity: risk.SeverityWatch, Observed: &grossDeltaPct, Threshold: new(150.0)},
			{ID: risk.SignalSingleNameExposureHigh, Direction: risk.DirectionRebalance, Severity: risk.SeverityWatch, Subject: "BB", Observed: &bbPct, Threshold: new(35.0), Target: new(25.0)},
			{ID: risk.SignalSingleNameDeltaHigh, Direction: risk.DirectionRebalance, Severity: risk.SeverityWatch, Subject: "SPY", Observed: &spyDeltaPct, Threshold: new(35.0), Target: new(25.0)},
		},
		Portfolio: rpc.CanaryPortfolioSummary{
			BaseCurrency:         base,
			NetLiquidation:       nlv,
			CushionPct:           &cushion,
			LookAheadCushionPct:  &cushion,
			GrossExposurePctNLV:  new(113.87081149071476),
			NetDeltaPctNLV:       &netDeltaPct,
			GrossDeltaPctNLV:     &grossDeltaPct,
			LargestExposure:      "BB",
			LargestExposurePct:   &bbPct,
			LargestDeltaExposure: "SPY",
			LargestDeltaPctNLV:   &spyDeltaPct,
			DailyPnLPct:          &dailyPct,
			OptionGreeks:         "8/8 legs",
		},
	}
	return RiskPlanInput{Account: acct, Positions: pos, Canary: canary, RequestedMode: rpc.RiskPlanModeAuto, Now: now}
}

func stock(symbol string, qty, mark, mvBase float64) rpc.PositionView {
	return rpc.PositionView{Symbol: symbol, SecType: rpc.SecTypeStock, Currency: "USD", Quantity: qty, Multiplier: 1, Mark: mark, MarketValue: mvBase / 0.862, MarketValueBase: &mvBase, FXRate: new(0.862)}
}

func opt(symbol, expiry, right string, strike, qty, mvBase, delta, bid, ask, pnlBase float64) rpc.PositionView {
	underlying := mvBase / 0.862 / qty / 100
	theta := -0.1
	gamma := 0.01
	vega := 0.1
	return rpc.PositionView{
		Symbol:            symbol,
		SecType:           rpc.SecTypeOption,
		Currency:          "USD",
		Quantity:          qty,
		Multiplier:        100,
		MarketValue:       mvBase / 0.862,
		MarketValueBase:   &mvBase,
		FXRate:            new(0.862),
		UnrealizedPnLBase: &pnlBase,
		Expiry:            expiry,
		Right:             right,
		Strike:            strike,
		Delta:             &delta,
		Gamma:             &gamma,
		Theta:             &theta,
		Vega:              &vega,
		OptionBid:         &bid,
		OptionAsk:         &ask,
		Underlying:        &underlying,
		TradingClass:      symbol,
	}
}

func spyPut() rpc.PositionView {
	mvBase := 22517.177275431295
	delta := -0.06279493776398423
	bid := 1.73
	ask := 1.74
	pnl := -9609.937149212426
	leg := opt("SPY", "20260717", "P", 670, 150, mvBase, delta, bid, ask, pnl)
	underlying := 650.69
	leg.Underlying = &underlying
	leg.MarkOutsideBidAsk = true
	return leg
}

func exposure(symbol string, mv, pct, delta, daily float64) rpc.UnderlyingExposure {
	return rpc.UnderlyingExposure{
		Underlying:        symbol,
		MarketValueBase:   mv,
		MarketValuePctNLV: &pct,
		DollarDeltaBase:   &delta,
		DailyPnLBase:      &daily,
		BaseCurrency:      "EUR",
	}
}

func findCandidateByIntent(plan rpc.RiskPlanResult, intent string) *rpc.RiskPlanCandidate {
	for i := range plan.Candidates {
		if plan.Candidates[i].Intent == intent {
			return &plan.Candidates[i]
		}
	}
	return nil
}

func findCandidateBySubject(plan rpc.RiskPlanResult, subject string) *rpc.RiskPlanCandidate {
	for i := range plan.Candidates {
		if plan.Candidates[i].Subject == subject {
			return &plan.Candidates[i]
		}
	}
	return nil
}

func containsText(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
