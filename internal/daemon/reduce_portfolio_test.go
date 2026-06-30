package daemon

import (
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestReduceBasketCandidates(t *testing.T) {
	pos := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AMD", SecType: "STOCK", ConID: 1, Quantity: 20},
		},
		Options: []rpc.PositionView{
			{Symbol: "AMD", SecType: "OPTION", ConID: 2, Quantity: 2, Right: "C"},  // long call → actionable
			{Symbol: "SPY", SecType: "OPTION", ConID: 3, Quantity: 30, Right: "P"}, // long put → hedge
			{Symbol: "X", SecType: "OPTION", ConID: 4, Quantity: -5, Right: "C"},   // short call → out of scope
		},
	}

	t.Run("protect hedges excludes the SPY puts", func(t *testing.T) {
		cands, actionable := reduceBasketCandidates(pos, true)
		if len(cands) != 3 { // AMD stock, AMD call, SPY put (short call dropped)
			t.Fatalf("len(cands)=%d, want 3", len(cands))
		}
		if actionable != 2 {
			t.Fatalf("actionable=%d, want 2", actionable)
		}
		var spy *reduceCandidate
		for i := range cands {
			if cands[i].row.ConID == 3 {
				spy = &cands[i]
			}
		}
		if spy == nil || !spy.hedgeExcluded {
			t.Fatalf("SPY put should be hedge-excluded, got %+v", spy)
		}
	})

	t.Run("include hedges makes the SPY puts actionable", func(t *testing.T) {
		cands, actionable := reduceBasketCandidates(pos, false)
		if len(cands) != 3 || actionable != 3 {
			t.Fatalf("len=%d actionable=%d, want 3/3", len(cands), actionable)
		}
		for _, c := range cands {
			if c.hedgeExcluded {
				t.Fatalf("no candidate should be hedge-excluded with include-hedges, got %+v", c)
			}
		}
	})
}

func TestAggregateBasketPreview(t *testing.T) {
	nb := 100.0
	res := &rpc.TradeProposalReducePortfolioResult{BaseCurrency: "EUR"}
	res.Legs = []rpc.TradeProposalReduceLeg{
		{Symbol: "AMD", SubmitEligible: true, NotionalBase: &nb},
		{Symbol: "IBM", SubmitEligible: true}, // eligible but no base notional → FX incomplete
		{Symbol: "NOW", SubmitEligible: false, Blockers: []rpc.TradingBlocker{{Code: "preview_failed"}}},
		{Symbol: "SPY", HedgeLike: true, Blockers: []rpc.TradingBlocker{{Code: "hedge_excluded"}}},
	}
	aggregateBasket(res, false)
	if res.LegCount != 4 || res.EligibleCount != 2 || res.BlockedCount != 1 || res.HedgeExcludedCount != 1 {
		t.Fatalf("counts wrong: %+v", res)
	}
	if !res.FXIncomplete {
		t.Fatalf("FXIncomplete should be true (IBM lacked base notional)")
	}
	if res.TotalNotional != 100 {
		t.Fatalf("TotalNotional=%v, want 100", res.TotalNotional)
	}
	if res.Accepted {
		t.Fatalf("Accepted must be false when a leg is blocked")
	}
}

func TestAggregateBasketSubmitAccepted(t *testing.T) {
	nb := 50.0
	res := &rpc.TradeProposalReducePortfolioResult{BaseCurrency: "EUR"}
	res.Legs = []rpc.TradeProposalReduceLeg{
		{Symbol: "AMD", Placed: true, NotionalBase: &nb},
		{Symbol: "SPY", HedgeLike: true, Blockers: []rpc.TradingBlocker{{Code: "hedge_excluded"}}},
	}
	aggregateBasket(res, true)
	if res.EligibleCount != 1 || res.BlockedCount != 0 || res.HedgeExcludedCount != 1 {
		t.Fatalf("counts wrong: %+v", res)
	}
	if !res.Accepted {
		t.Fatalf("Accepted should be true: one placed, zero blocked (hedge-excluded is not blocked)")
	}
	if res.FXIncomplete || res.TotalNotional != 50 {
		t.Fatalf("FX wrong: incomplete=%v total=%v", res.FXIncomplete, res.TotalNotional)
	}
}

func TestReduceBasketDedupe(t *testing.T) {
	cur := time.Unix(1000, 0).UTC()
	s := &Server{now: func() time.Time { return cur }}
	res := &rpc.TradeProposalReducePortfolioResult{EligibleCount: 3}

	if _, ok := s.reduceBasketReplay("ref1"); ok {
		t.Fatal("empty cache should miss")
	}
	s.reduceBasketStore("ref1", res)
	got, ok := s.reduceBasketReplay("ref1")
	if !ok || !got.Replayed || got.EligibleCount != 3 {
		t.Fatalf("replay should hit with Replayed=true: ok=%v got=%+v", ok, got)
	}
	// The stored result must not be mutated by the replay clone.
	if res.Replayed {
		t.Fatal("replay must not mutate the cached result")
	}
	// Empty ref never dedupes.
	if _, ok := s.reduceBasketReplay(""); ok {
		t.Fatal("empty ref must miss")
	}
	s.reduceBasketStore("", res) // no-op, must not panic
	// TTL expiry sweeps the entry.
	cur = cur.Add(reduceBasketDedupeTTL + time.Second)
	if _, ok := s.reduceBasketReplay("ref1"); ok {
		t.Fatal("expired entry should be swept")
	}
}
