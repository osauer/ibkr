package risk

import (
	"strings"
	"testing"
	"time"
)

func etDate(y int, m time.Month, d int) time.Time {
	loc, _ := time.LoadLocation("America/New_York")
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// healthyInputs models a compact version of a real book: an oversized NOW
// complex into earnings, a BB option line over the premium cap, a SPY hedge,
// and an MSFT covered call across its print.
func healthyInputs() RuleInputs {
	now := etDate(2026, 7, 7)
	nowEarnings := EarningsInput{Known: true, Date: etDate(2026, 7, 22), TimeOfDay: "amc", SessionsUntil: new(11), Source: "fetched"}
	msftEarnings := EarningsInput{Known: true, Date: etDate(2026, 7, 29), TimeOfDay: "amc", SessionsUntil: new(16), Source: "fetched"}
	return RuleInputs{
		AsOf:            now,
		BaseCurrency:    "EUR",
		Positions:       SourceState{Healthy: true},
		Account:         SourceState{Healthy: true},
		NLVBase:         new(245000.0),
		CashBase:        new(-62000.0),
		DailyPnLBase:    new(9700.0),
		SessionOpen:     true,
		SPYDayChangePct: new(1.0),
		Names: []NameInput{
			{
				Symbol: "NOW", ExposureBase: 380000, MarketValueBase: 120000, HasStockLeg: true,
				StockDayChangePct: new(1.6),
				Legs: []LegInput{
					{Desc: "NOW 20260717 C 130", Right: "C", Strike: 130, Expiry: etDate(2026, 7, 17), DTE: 10,
						Quantity: 35, Multiplier: 100, Mark: 0.44, Underlying: new(108.0), Delta: new(0.08),
						MarketValueBase: 1400, ExtrinsicBase: new(1400.0)},
					{Desc: "NOW 20260821 C 115", Right: "C", Strike: 115, Expiry: etDate(2026, 8, 21), DTE: 45,
						Quantity: 50, Multiplier: 100, Mark: 7.86, Underlying: new(108.0), Delta: new(0.46),
						MarketValueBase: 36000, ExtrinsicBase: new(36000.0)},
				},
			},
			{
				Symbol: "BB", ExposureBase: 45000, MarketValueBase: 45000, HasStockLeg: true,
				StockDayChangePct: new(-1.7),
				Legs: []LegInput{
					{Desc: "BB 20260821 C 12", Right: "C", Strike: 12, Expiry: etDate(2026, 8, 21), DTE: 45,
						Quantity: 300, Multiplier: 100, Mark: 1.28, Underlying: new(11.3), Delta: new(0.50),
						MarketValueBase: 34000, ExtrinsicBase: new(34000.0)},
				},
			},
			{
				Symbol: "MSFT", ExposureBase: 30000, MarketValueBase: 12000, HasStockLeg: true,
				StockDayChangePct: new(0.3),
				Legs: []LegInput{
					{Desc: "MSFT 20260821 C 400", Right: "C", Strike: 400, Expiry: etDate(2026, 8, 21), DTE: 45,
						Quantity: -3, Multiplier: 100, Mark: 5, Underlying: new(386.0), Delta: new(-0.3),
						MarketValueBase: -1400},
				},
			},
			{
				Symbol: "SPY", ExposureBase: -80000, MarketValueBase: 38000, HasStockLeg: false,
				Legs: []LegInput{
					{Desc: "SPY 20261016 P 710", Right: "P", Strike: 710, Expiry: etDate(2026, 10, 16), DTE: 101,
						Quantity: 40, Multiplier: 100, Mark: 10.4, Underlying: new(752.0), Delta: new(-0.24),
						MarketValueBase: 38000, ExtrinsicBase: new(38000.0), HedgeListed: true},
				},
			},
		},
		Earnings: map[string]EarningsInput{"NOW": nowEarnings, "MSFT": msftEarnings, "BB": {Known: false}},
	}
}

func rowByID(t *testing.T, ev Evaluation, id string) RuleRow {
	t.Helper()
	for _, r := range ev.Rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("row %s missing", id)
	return RuleRow{}
}

func TestEvaluateRulebookHealthyBook(t *testing.T) {
	ev := EvaluateRulebook(healthyInputs(), DefaultRulebookPolicy())
	if len(ev.Rows) != 12 {
		t.Fatalf("rows = %d, want 12", len(ev.Rows))
	}
	cases := map[string]string{
		RuleSingleNameExposure: RuleStatusAct,   // NOW 155% of NLV
		RuleOptionLinePremium:  RuleStatusAct,   // NOW 115C 14.7%, BB 13.9%
		RuleCashSellOnly:       RuleStatusAct,   // -25.3%
		RuleExtrinsicBudget:    RuleStatusAct,   // ~44.7% of NLV
		RuleExpiryRunway:       RuleStatusWatch, // NOW Jul 130C at 10 DTE
		RuleCatalystCoverage:   RuleStatusWatch, // NOW 130C dies before Jul 22
		RuleOverwriteEarnings:  RuleStatusAct,   // MSFT short call through Jul 29
		RuleEarningsSizeFreeze: RuleStatusPass,  // 11 sessions out
		RuleRedOnGreen:         RuleStatusWatch, // BB -1.7% on SPY +1.0%
		RuleWinnerTrim:         RuleStatusPass,  // nothing +4%
		RuleGreenDayAction:     RuleStatusInfo,  // green day, act rules open
		RuleHedgeIntegrity:     RuleStatusWatch, // hedge ~16% of gross long
	}
	for id, want := range cases {
		if got := rowByID(t, ev, id).Status; got != want {
			t.Errorf("%s = %s, want %s (evidence: %s)", id, got, want, rowByID(t, ev, id).Evidence)
		}
	}
}

// TestNeverFalsePass is the acceptance test for the design's safety
// invariant: strip each input dimension and assert the affected rows report
// unknown/not_evaluated — never pass (docs/design/trading-rulebook.md).
func TestNeverFalsePass(t *testing.T) {
	pol := DefaultRulebookPolicy()
	portfolioRules := []string{
		RuleSingleNameExposure, RuleOptionLinePremium, RuleExtrinsicBudget,
		RuleExpiryRunway, RuleCatalystCoverage, RuleOverwriteEarnings,
		RuleEarningsSizeFreeze, RuleHedgeIntegrity,
	}
	assertNoPass := func(t *testing.T, ev Evaluation, ids ...string) {
		t.Helper()
		for _, id := range ids {
			r := rowByID(t, ev, id)
			if r.Status == RuleStatusPass {
				t.Errorf("%s = pass on degraded inputs (evidence: %s)", id, r.Evidence)
			}
		}
	}

	t.Run("positions pending", func(t *testing.T) {
		in := healthyInputs()
		in.Positions = SourceState{Healthy: false, Reason: "positions_pending"}
		in.Names = nil // a boot race serves an empty book
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, append(portfolioRules, RuleRedOnGreen, RuleWinnerTrim)...)
	})

	t.Run("account absent", func(t *testing.T) {
		in := healthyInputs()
		in.Account = SourceState{Healthy: false, Reason: "account_unavailable"}
		in.NLVBase, in.CashBase, in.DailyPnLBase = nil, nil, nil
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, append(portfolioRules, RuleCashSellOnly, RuleGreenDayAction)...)
	})

	t.Run("greeks stripped", func(t *testing.T) {
		in := healthyInputs()
		for i := range in.Names {
			gap := 0.0
			for j := range in.Names[i].Legs {
				in.Names[i].Legs[j].Delta = nil
				in.Names[i].Legs[j].ExtrinsicBase = nil
				gap += abs(in.Names[i].Legs[j].MarketValueBase)
			}
			in.Names[i].GreeksGapNotionalBase = gap
		}
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, RuleSingleNameExposure, RuleExtrinsicBudget, RuleHedgeIntegrity)
	})

	t.Run("earnings unknown", func(t *testing.T) {
		in := healthyInputs()
		in.Earnings = map[string]EarningsInput{}
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, RuleCatalystCoverage, RuleOverwriteEarnings)
	})

	t.Run("earnings stale", func(t *testing.T) {
		in := healthyInputs()
		for k, e := range in.Earnings {
			e.Stale = true
			in.Earnings[k] = e
		}
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, RuleCatalystCoverage, RuleOverwriteEarnings)
	})

	t.Run("off session", func(t *testing.T) {
		in := healthyInputs()
		in.SessionOpen = false
		ev := EvaluateRulebook(in, pol)
		for _, id := range []string{RuleRedOnGreen, RuleWinnerTrim} {
			if got := rowByID(t, ev, id).Status; got != RuleStatusNotEvaluated {
				t.Errorf("%s = %s off-session, want not_evaluated", id, got)
			}
		}
	})

	t.Run("no spy tape", func(t *testing.T) {
		in := healthyInputs()
		in.SPYDayChangePct = nil
		ev := EvaluateRulebook(in, pol)
		if got := rowByID(t, ev, RuleRedOnGreen).Status; got != RuleStatusUnknown {
			t.Errorf("red_on_green = %s without SPY tape, want unknown", got)
		}
	})
}

func TestEarningsGapEdges(t *testing.T) {
	expiry := etDate(2026, 7, 17)
	now := etDate(2026, 7, 7)
	cases := []struct {
		name      string
		e         EarningsInput
		wantSpans bool
		wantAmb   bool
	}{
		{"amc on expiry day dies before gap", EarningsInput{Known: true, Date: etDate(2026, 7, 17), TimeOfDay: "amc"}, false, false},
		{"bmo on expiry day spans", EarningsInput{Known: true, Date: etDate(2026, 7, 17), TimeOfDay: "bmo"}, true, false},
		{"unknown tod on expiry day conservative", EarningsInput{Known: true, Date: etDate(2026, 7, 17)}, true, true},
		{"earnings after expiry", EarningsInput{Known: true, Date: etDate(2026, 7, 24), TimeOfDay: "amc"}, false, false},
		{"earnings before today", EarningsInput{Known: true, Date: etDate(2026, 7, 1), TimeOfDay: "amc"}, false, false},
		{"earnings inside life", EarningsInput{Known: true, Date: etDate(2026, 7, 15), TimeOfDay: "bmo"}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spans, amb := spansEarningsGap(now, expiry, tc.e)
			if spans != tc.wantSpans || amb != tc.wantAmb {
				t.Fatalf("spansEarningsGap = (%v,%v), want (%v,%v)", spans, amb, tc.wantSpans, tc.wantAmb)
			}
		})
	}

	if expiresBeforeCatalyst(etDate(2026, 7, 17), EarningsInput{Known: true, Date: etDate(2026, 7, 17), TimeOfDay: "amc"}) != true {
		t.Error("amc on expiry day: option dies before the gap, rule 6 should flag")
	}
	if expiresBeforeCatalyst(etDate(2026, 7, 17), EarningsInput{Known: true, Date: etDate(2026, 7, 17), TimeOfDay: "bmo"}) != false {
		t.Error("bmo on expiry day: option lives through the gap, rule 6 should not flag")
	}
}

func TestHedgeExemptionSuppressedWhenOverHedged(t *testing.T) {
	in := healthyInputs()
	// Inflate the hedge so the band breaches high: short delta ~194k vs
	// gross long ~455k ≈ 43%.
	spy := &in.Names[3].Legs[0]
	spy.Delta = new(-0.60)
	spy.DTE = 10 // inside runway window so the exemption question is live
	spy.Expiry = etDate(2026, 7, 17)
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())

	hedge := rowByID(t, ev, RuleHedgeIntegrity)
	if hedge.Status != RuleStatusWatch || !strings.Contains(hedge.Evidence, "over") {
		t.Fatalf("hedge row = %s (%s), want over-band watch", hedge.Status, hedge.Evidence)
	}
	runway := rowByID(t, ev, RuleExpiryRunway)
	found := false
	for _, o := range runway.Offenders {
		if o.Symbol == "SPY" && strings.Contains(o.Note, "suppressed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("over-hedged SPY leg should lose its runway exemption; offenders = %+v, exempt = %+v", runway.Offenders, runway.Exempt)
	}
}

// A policy-hedge index name with net-short delta belongs to rule 12, not the
// concentration cap — but it must surface in rule 1's Exempt list, never
// silently vanish (2026-07-07 live finding: 40 SPY puts ≈ 234% of NLV in
// absolute delta-dollars outranked every real concentration offender).
func TestSingleNameExposureExemptsShortHedge(t *testing.T) {
	in := healthyInputs()
	in.Names[3].ExposureBase = -640000 // oversized SPY hedge, short delta
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	row := rowByID(t, ev, RuleSingleNameExposure)
	for _, o := range row.Offenders {
		if o.Symbol == "SPY" {
			t.Fatalf("short hedge SPY must not be a concentration offender: %+v", row.Offenders)
		}
	}
	found := false
	for _, e := range row.Exempt {
		if e.Symbol == "SPY" && strings.Contains(e.Note, "rule 12") {
			found = true
		}
	}
	if !found {
		t.Fatalf("short hedge must be disclosed in Exempt, got %+v", row.Exempt)
	}
	if row.Status != RuleStatusAct || row.Offenders[0].Symbol != "NOW" {
		t.Fatalf("real concentration offender should lead: status=%s offenders=%+v", row.Status, row.Offenders)
	}
	// A LONG position in a hedge-listed index is ordinary concentration.
	in.Names[3].ExposureBase = 640000
	ev = EvaluateRulebook(in, DefaultRulebookPolicy())
	row = rowByID(t, ev, RuleSingleNameExposure)
	if len(row.Offenders) == 0 || row.Offenders[0].Symbol != "SPY" {
		t.Fatalf("long index exposure must still count as concentration, got %+v", row.Offenders)
	}
}

// The exemption covers only what rule 12 can size (long puts with delta).
// Short stock or short calls in a hedge symbol are directional shorts: the
// residual beyond the sized legs stays a concentration offender.
func TestSingleNameExposureResidualBeyondSizedHedge(t *testing.T) {
	in := healthyInputs()
	// Fixture SPY puts size to 0.24*40*100*752 = 721,920 short delta-dollars.
	// Net short 900k leaves a 178,080 residual = 72.7% of the 245k NLV.
	in.Names[3].ExposureBase = -900000
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	row := rowByID(t, ev, RuleSingleNameExposure)
	var spy *RuleOffender
	for i := range row.Offenders {
		if row.Offenders[i].Symbol == "SPY" {
			spy = &row.Offenders[i]
		}
	}
	if spy == nil || spy.Observed < 72 || spy.Observed > 73 {
		t.Fatalf("residual short beyond sized hedge legs must be an offender near 72.7%%, got %+v", row.Offenders)
	}
	if len(row.Exempt) == 0 || row.Exempt[0].Symbol != "SPY" {
		t.Fatalf("sized portion must still be disclosed in Exempt, got %+v", row.Exempt)
	}

	// A hedge-symbol short with NO rule-12-sizeable legs (e.g. short stock,
	// puts stripped) is pure concentration: no Exempt row at all.
	in = healthyInputs()
	in.Names[3].ExposureBase = -640000
	in.Names[3].Legs = nil
	ev = EvaluateRulebook(in, DefaultRulebookPolicy())
	row = rowByID(t, ev, RuleSingleNameExposure)
	if len(row.Exempt) != 0 {
		t.Fatalf("unsized hedge-symbol short must not be exempted, got %+v", row.Exempt)
	}
	found := false
	for _, o := range row.Offenders {
		if o.Symbol == "SPY" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unsized hedge-symbol short must be a concentration offender, got %+v", row.Offenders)
	}
}

func TestRankingHardestFirst(t *testing.T) {
	ev := EvaluateRulebook(healthyInputs(), DefaultRulebookPolicy())
	if len(ev.Ranked) != 12 {
		t.Fatalf("ranked = %d, want 12", len(ev.Ranked))
	}
	weight := map[string]int{RuleStatusAct: 5, RuleStatusWatch: 4, RuleStatusUnknown: 3, RuleStatusInfo: 2, RuleStatusNotEvaluated: 1, RuleStatusPass: 0}
	prev := 6
	prevImpact := 0.0
	for i, ix := range ev.Ranked {
		r := ev.Rows[ix]
		w := weight[r.Status]
		if w > prev {
			t.Fatalf("ranked[%d] %s (%s) outranks a lighter status", i, r.ID, r.Status)
		}
		if w == prev && r.ImpactBase > prevImpact {
			t.Fatalf("ranked[%d] %s impact %.0f should precede %.0f", i, r.ID, r.ImpactBase, prevImpact)
		}
		prev, prevImpact = w, r.ImpactBase
	}
	first := ev.Rows[ev.Ranked[0]]
	if first.Status != RuleStatusAct {
		t.Fatalf("hardest-first head = %s, want an act row", first.Status)
	}
}

func TestOptionMathHelpers(t *testing.T) {
	if got := OptionIntrinsicPerShare("C", 108, 130); got != 0 {
		t.Errorf("OTM call intrinsic = %v, want 0", got)
	}
	if got := OptionIntrinsicPerShare("P", 700, 710); got != 10 {
		t.Errorf("ITM put intrinsic = %v, want 10", got)
	}
	if _, ok := OptionExtrinsicPerShare("C", nil, 130, 0.44); ok {
		t.Error("extrinsic with nil underlying must be uncomputable, not zero")
	}
	if ext, ok := OptionExtrinsicPerShare("C", new(140.0), 130, 8.0); !ok || ext != 0 {
		t.Errorf("stale mark below intrinsic: extrinsic = %v ok=%v, want 0 true", ext, ok)
	}
	if _, ok := OptionSpreadPct(new(1.3), new(1.2)); ok {
		t.Error("crossed quote must not produce a spread")
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
