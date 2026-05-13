package ibkr

import (
	"testing"
)

// convertIBKRPositions must pass UnrealizedPNL straight through from the
// wire. A previous draft synthesised a value when IBKR reported zero, using
// (currentPrice - AverageCost) * qty * multiplier — fine for stocks but
// catastrophically wrong for options (IBKR's averageCost on OPT is
// per-contract, while currentPrice is per-share, so the formula was 100× off
// on any option with multiplier 100). The wire-reported PNL is authoritative;
// a genuine zero (e.g. a position just opened at exactly the current price)
// must stay zero rather than be replaced by a synthesised value.
func TestConvertIBKRPositionsPassesUnrealizedPNLThrough(t *testing.T) {
	t.Parallel()
	c := &Connector{}
	cases := []struct {
		name       string
		raw        RawPosition
		wantUnreal float64
	}{
		{
			name: "stock with non-zero unrealized passes through",
			raw: RawPosition{
				Contract:      Contract{ConID: 123, Symbol: "AAPL", SecType: "STK", Multiplier: 1, Exchange: "SMART"},
				Position:      100,
				MarketPrice:   207.42,
				AverageCost:   192.10,
				UnrealizedPNL: 1532.00,
			},
			wantUnreal: 1532.00,
		},
		{
			name: "option with non-zero unrealized passes through",
			raw: RawPosition{
				Contract:      Contract{ConID: 456, Symbol: "AAPL", SecType: "OPT", Multiplier: 100, Expiry: "20251219", Strike: 210, Right: "C", Exchange: "SMART"},
				Position:      2,
				MarketPrice:   7.85,
				AverageCost:   510.00, // per-contract; would synthesise nonsense P&L if combined with per-share price
				UnrealizedPNL: 550.00,
			},
			wantUnreal: 550.00,
		},
		{
			name: "option with zero unrealized stays zero (no synthesis)",
			raw: RawPosition{
				Contract:      Contract{ConID: 789, Symbol: "MSFT", SecType: "OPT", Multiplier: 100, Expiry: "20260117", Strike: 400, Right: "P", Exchange: "SMART"},
				Position:      1,
				MarketPrice:   3.50,
				AverageCost:   350.00, // legitimate per-contract cost
				UnrealizedPNL: 0,      // wire says zero — keep it
			},
			wantUnreal: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rp := tc.raw
			got := c.convertIBKRPositions(map[string]*RawPosition{"key": &rp})
			if len(got) != 1 {
				t.Fatalf("convertIBKRPositions: got %d positions, want 1", len(got))
			}
			if got[0].UnrealizedPnL != tc.wantUnreal {
				t.Errorf("UnrealizedPnL = %v, want %v (no synthesis allowed)", got[0].UnrealizedPnL, tc.wantUnreal)
			}
		})
	}
}
