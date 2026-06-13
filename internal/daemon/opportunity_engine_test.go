package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestOptionExerciseOpportunityCallUsesExecutableClose(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	policy := defaultOpportunityPolicy()
	status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", now)
	bid, ask, optionBid := 103.0, 103.20, 2.0
	row := opportunityTestOption(now, "C", 100, &optionBid)
	stock := opportunityTestStock(now, -100, &bid, &ask)

	opp, ok := optionExerciseOpportunity(policy, status, row, stock, rpc.OpportunitySourceFingerprints{}, now)
	if !ok {
		t.Fatal("call exercise opportunity missing")
	}
	if len(opp.Blockers) != 0 {
		t.Fatalf("blockers=%+v, want none", opp.Blockers)
	}
	if opp.PositionEffect != rpc.ExercisePositionEffectClose {
		t.Fatalf("position effect=%q, want close", opp.PositionEffect)
	}
	if opp.UnderlyingQuantityBefore != -100 || opp.UnderlyingQuantityAfter != 0 || opp.UnderlyingShareChange != 100 {
		t.Fatalf("underlying effect before/after/change = %.0f/%.0f/%.0f", opp.UnderlyingQuantityBefore, opp.UnderlyingQuantityAfter, opp.UnderlyingShareChange)
	}
	if opp.IntrinsicValue != 300 || opp.CloseValue != 200 || opp.ExpectedGain != 100 {
		t.Fatalf("economics intrinsic=%.2f close=%.2f gain=%.2f, want 300/200/100", opp.IntrinsicValue, opp.CloseValue, opp.ExpectedGain)
	}
	if opp.Reason != "exercise value exceeds executable option close value" {
		t.Fatalf("reason=%q, want positive-gain exercise wording", opp.Reason)
	}
}

func TestOptionExerciseOpportunityPutUsesUnderlyingAsk(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	policy := defaultOpportunityPolicy()
	status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", now)
	bid, ask, optionBid := 101.80, 102.0, 2.5
	row := opportunityTestOption(now, "P", 105, &optionBid)
	stock := opportunityTestStock(now, 100, &bid, &ask)

	opp, ok := optionExerciseOpportunity(policy, status, row, stock, rpc.OpportunitySourceFingerprints{}, now)
	if !ok {
		t.Fatal("put exercise opportunity missing")
	}
	if len(opp.Blockers) != 0 {
		t.Fatalf("blockers=%+v, want none", opp.Blockers)
	}
	if opp.PositionEffect != rpc.ExercisePositionEffectClose {
		t.Fatalf("position effect=%q, want close", opp.PositionEffect)
	}
	if opp.IntrinsicValue != 300 || opp.CloseValue != 250 || opp.ExpectedGain != 50 {
		t.Fatalf("economics intrinsic=%.2f close=%.2f gain=%.2f, want 300/250/50", opp.IntrinsicValue, opp.CloseValue, opp.ExpectedGain)
	}
}

func TestOptionExerciseOpportunityNegativeGainReasonDoesNotOverstate(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	policy := defaultOpportunityPolicy()
	status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", now)
	bid, ask, optionBid := 103.0, 103.20, 4.0
	row := opportunityTestOption(now, "C", 100, &optionBid)
	stock := opportunityTestStock(now, -100, &bid, &ask)

	opp, ok := optionExerciseOpportunity(policy, status, row, stock, rpc.OpportunitySourceFingerprints{}, now)
	if ok {
		t.Fatalf("negative-gain row surfaced as opportunity: %+v", opp)
	}
}

func TestOptionExerciseOpportunityBlockersFailClosed(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	bid, ask, optionBid := 103.0, 103.20, 2.0

	tests := []struct {
		name       string
		policy     func(opportunityPolicy) opportunityPolicy
		row        func(rpc.PositionView) rpc.PositionView
		stock      func(rpc.PositionView) rpc.PositionView
		at         time.Time
		wantCode   string
		wantEffect string
		wantSkip   bool
	}{
		{
			name: "missing option bid when policy requires bid",
			policy: func(p opportunityPolicy) opportunityPolicy {
				p.Buckets.OptionExercise.AllowNoOptionBid = false
				return p
			},
			row:      func(r rpc.PositionView) rpc.PositionView { r.OptionBid = nil; return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { return s },
			at:       now,
			wantCode: "option_bid_unavailable",
		},
		{
			name:     "stale option quote",
			policy:   func(p opportunityPolicy) opportunityPolicy { return p },
			row:      func(r rpc.PositionView) rpc.PositionView { r.PriceAt = now.Add(-time.Minute); return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { return s },
			at:       now,
			wantCode: "option_quote_stale",
		},
		{
			name:     "outside RTH",
			policy:   func(p opportunityPolicy) opportunityPolicy { return p },
			row:      func(r rpc.PositionView) rpc.PositionView { return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { return s },
			at:       time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC),
			wantCode: "options_rth_required",
		},
		{
			name:     "unsupported style",
			policy:   func(p opportunityPolicy) opportunityPolicy { return p },
			row:      func(r rpc.PositionView) rpc.PositionView { return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { s.SecType = rpc.SecTypeIndex; return s },
			at:       now,
			wantCode: "exercise_style_unknown_or_unsupported",
		},
		{
			name:     "underlying NBBO missing",
			policy:   func(p opportunityPolicy) opportunityPolicy { return p },
			row:      func(r rpc.PositionView) rpc.PositionView { return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { s.Bid = nil; return s },
			at:       now,
			wantSkip: true,
		},
		{
			name:       "exercise can increase stock exposure",
			policy:     func(p opportunityPolicy) opportunityPolicy { return p },
			row:        func(r rpc.PositionView) rpc.PositionView { return r },
			stock:      func(s rpc.PositionView) rpc.PositionView { s.Quantity = 100; return s },
			at:         now,
			wantEffect: rpc.ExercisePositionEffectIncrease,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy := tc.policy(defaultOpportunityPolicy())
			status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", tc.at)
			row := tc.row(opportunityTestOption(tc.at, "C", 100, &optionBid))
			stock := tc.stock(opportunityTestStock(tc.at, -100, &bid, &ask))

			opp, ok := optionExerciseOpportunity(policy, status, row, stock, rpc.OpportunitySourceFingerprints{}, tc.at)
			if tc.wantSkip {
				if ok {
					t.Fatalf("row surfaced as opportunity: %+v", opp)
				}
				return
			}
			if !ok {
				t.Fatal("expected opportunity to be surfaced")
			}
			if tc.wantCode == "" {
				if len(opp.Blockers) != 0 {
					t.Fatalf("blockers=%+v, want none", opp.Blockers)
				}
				if opp.State != rpc.OpportunityStateGenerated {
					t.Fatalf("state=%q, want generated", opp.State)
				}
			} else {
				if !hasBlocker(opp.Blockers, tc.wantCode) {
					t.Fatalf("blockers=%+v, want %q", opp.Blockers, tc.wantCode)
				}
				if opp.State != rpc.OpportunityStateBlocked {
					t.Fatalf("state=%q, want blocked", opp.State)
				}
			}
			if tc.wantEffect != "" && opp.PositionEffect != tc.wantEffect {
				t.Fatalf("position effect=%q, want %q", opp.PositionEffect, tc.wantEffect)
			}
		})
	}
}

func TestClassifyExercisePositionEffect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		before float64
		after  float64
		want   string
	}{
		{before: 0, after: 100, want: rpc.ExercisePositionEffectOpen},
		{before: -100, after: 0, want: rpc.ExercisePositionEffectClose},
		{before: -200, after: -100, want: rpc.ExercisePositionEffectReduce},
		{before: 100, after: 200, want: rpc.ExercisePositionEffectIncrease},
		{before: -100, after: 100, want: rpc.ExercisePositionEffectFlip},
		{before: 100, after: 100, want: rpc.ExercisePositionEffectUnknown},
	}
	for _, tc := range tests {
		if got := classifyExercisePositionEffect(tc.before, tc.after); got != tc.want {
			t.Fatalf("classifyExercisePositionEffect(%v, %v)=%q, want %q", tc.before, tc.after, got, tc.want)
		}
	}
}

func TestOpportunityPreviewParamsForSubmitPreservesOrigin(t *testing.T) {
	t.Parallel()
	got := opportunityPreviewParamsForSubmit(rpc.OpportunityExerciseSubmitParams{
		Key:       "opportunity",
		Revision:  "rev",
		Quantity:  2,
		TimeoutMs: 5000,
		Origin:    rpc.OrderOriginPairedDevice,
	})
	if got.Key != "opportunity" || got.Revision != "rev" || got.Quantity != 2 || got.TimeoutMs != 5000 || got.Origin != rpc.OrderOriginPairedDevice {
		t.Fatalf("preview params = %+v, want submit fields including origin", got)
	}
}

func opportunityTestRTH() time.Time {
	return time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
}

func opportunityTestOption(now time.Time, right string, strike float64, bid *float64) rpc.PositionView {
	spot := 103.10
	return rpc.PositionView{
		Symbol:       "AAPL",
		SecType:      rpc.SecTypeOption,
		ConID:        12345,
		Exchange:     "SMART",
		Currency:     "USD",
		LocalSymbol:  "AAPL  260619C00100000",
		TradingClass: "AAPL",
		Quantity:     1,
		Multiplier:   100,
		PriceAt:      now,
		Expiry:       "20260619",
		Strike:       strike,
		Right:        strings.ToUpper(right),
		OptionBid:    bid,
		Underlying:   &spot,
	}
}

func opportunityTestStock(now time.Time, quantity float64, bid, ask *float64) rpc.PositionView {
	return rpc.PositionView{
		Symbol:     "AAPL",
		SecType:    rpc.SecTypeStock,
		Currency:   "USD",
		Quantity:   quantity,
		Multiplier: 1,
		Bid:        bid,
		Ask:        ask,
		PriceAt:    now,
	}
}
