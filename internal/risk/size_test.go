package risk

import (
	"math"
	"strings"
	"testing"
)

// ComputeSize is the only place math + validation lives. Tests are
// table-driven against the pure function — no daemon, no mocks of
// daemon-internal data, no live gateway.
func TestComputeSize(t *testing.T) {
	t.Parallel()

	// base: NLV 100k EUR, 1% risk → 1000 EUR risk budget; fx=1 keeps math in
	// EUR throughout. Long AAPL entry 207.50, stop 202.50 → per-share risk 5
	// → 1000 / 5 = 200 shares.
	base := SizeInput{
		Symbol: "AAPL", Side: "long",
		Entry: 207.50, Stop: 202.50,
		RiskPct: 1.0, Lot: 1, FX: 1.0,
		NLV: 100_000, BuyingPower: 400_000, Currency: "EUR",
	}

	tests := []struct {
		name       string
		mutate     func(*SizeInput)
		wantErr    string // substring; empty = expect success
		wantShares int
		wantStatus string
	}{
		{
			name:       "long happy path",
			mutate:     func(_ *SizeInput) {},
			wantShares: 200,
			wantStatus: "ok",
		},
		{
			name: "short happy path inverts stop>entry",
			mutate: func(in *SizeInput) {
				in.Side = "short"
				in.Entry = 202.50
				in.Stop = 207.50
			},
			wantShares: 200,
			wantStatus: "ok",
		},
		{
			name: "long with stop >= entry rejected",
			mutate: func(in *SizeInput) {
				in.Stop = 210
			},
			wantErr: "long trade requires stop",
		},
		{
			name: "short with stop <= entry rejected",
			mutate: func(in *SizeInput) {
				in.Side = "short"
				in.Stop = 200 // < entry
			},
			wantErr: "short trade requires stop",
		},
		{
			name: "lot 100 rounds shares down (200 → 200, exact multiple)",
			mutate: func(in *SizeInput) {
				in.Lot = 100
			},
			wantShares: 200,
			wantStatus: "ok",
		},
		{
			name: "lot 100 rounds 250 down to 200",
			mutate: func(in *SizeInput) {
				// raw = 250 → floor(250/100)*100 = 200
				in.Stop = 203.50 // per-share risk 4 → raw 1000/4 = 250
				in.Lot = 100
			},
			wantShares: 200,
			wantStatus: "ok",
		},
		{
			name: "tight risk: shares < 1 returns shares=0 with status",
			mutate: func(in *SizeInput) {
				// budget 0.004% of 100k = 4 EUR < 5 EUR per-share risk → 0 shares
				in.RiskPct = 0.004
			},
			wantShares: 0,
			wantStatus: "tight_risk",
		},
		{
			name: "exceeds buying power flagged",
			mutate: func(in *SizeInput) {
				// 200 shares × 207.50 = 41,500. Cap BP at 30k → flag.
				in.BuyingPower = 30_000
			},
			wantShares: 200,
			wantStatus: "exceeds_buying_power",
		},
		{
			name: "fx applied to risk budget",
			mutate: func(in *SizeInput) {
				// EUR account, USD trade. 1% of 100k EUR = 1000 EUR ≈ 1085 USD.
				// per-share risk 5 USD → 1085/5 = 217 shares.
				in.FX = 1.085
			},
			wantShares: 217,
			wantStatus: "ok",
		},
		{
			name: "risk-pct out of bounds (zero) rejected",
			mutate: func(in *SizeInput) {
				in.RiskPct = 0
			},
			wantErr: "risk-pct must be in",
		},
		{
			name: "risk-pct out of bounds (>100) rejected",
			mutate: func(in *SizeInput) {
				in.RiskPct = 101
			},
			wantErr: "risk-pct must be in",
		},
		{
			name: "lot 0 rejected",
			mutate: func(in *SizeInput) {
				in.Lot = 0
			},
			wantErr: "lot must be >= 1",
		},
		{
			name: "fx 0 rejected",
			mutate: func(in *SizeInput) {
				in.FX = 0
			},
			wantErr: "fx must be > 0",
		},
		{
			name: "nlv 0 surfaces gateway-not-connected hint",
			mutate: func(in *SizeInput) {
				in.NLV = 0
			},
			wantErr: "nlv must be > 0",
		},
		{
			name: "missing symbol rejected",
			mutate: func(in *SizeInput) {
				in.Symbol = ""
			},
			wantErr: "symbol is required",
		},
		{
			name: "invalid side rejected",
			mutate: func(in *SizeInput) {
				in.Side = "sideways"
			},
			wantErr: "side must be long or short",
		},
		{
			name: "long with target <= entry rejected",
			mutate: func(in *SizeInput) {
				in.Target = 205 // < entry 207.50
			},
			wantErr: "long trade requires target",
		},
		{
			name: "short with target >= entry rejected",
			mutate: func(in *SizeInput) {
				in.Side = "short"
				in.Entry = 202.50
				in.Stop = 207.50
				in.Target = 205 // > entry 202.50
			},
			wantErr: "short trade requires target",
		},
		{
			name: "negative target rejected",
			mutate: func(in *SizeInput) {
				in.Target = -10
			},
			wantErr: "target must be > 0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			res, err := ComputeSize(in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil; res=%+v", tc.wantErr, res)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q missing substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Shares != tc.wantShares {
				t.Errorf("shares = %d, want %d (per-share risk %.4f, riskQuote %.4f)",
					res.Shares, tc.wantShares, res.PerShareRisk, res.RiskQuote)
			}
			if res.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", res.Status, tc.wantStatus)
			}
			// Cross-checks on the happy paths.
			if tc.wantStatus == "ok" {
				wantNotional := float64(res.Shares) * in.Entry
				if res.Notional != wantNotional {
					t.Errorf("notional = %v, want %v", res.Notional, wantNotional)
				}
				wantMaxLoss := float64(res.Shares) * res.PerShareRisk
				if res.MaxLoss != wantMaxLoss {
					t.Errorf("max_loss = %v, want %v", res.MaxLoss, wantMaxLoss)
				}
			}
		})
	}
}

// TestComputeSizeRMultiple covers the optional reward-side math. R-multiple
// is the desk-standard "is this trade worth taking" filter; breakeven win
// rate is its dual. Pure derivation from entry/stop/target — once shares
// are sized, RewardQuote is just shares × per-share reward.
func TestComputeSizeRMultiple(t *testing.T) {
	t.Parallel()

	base := SizeInput{
		Symbol: "AAPL", Side: "long",
		Entry: 200, Stop: 195, // per-share risk 5
		RiskPct: 1.0, Lot: 1, FX: 1.0,
		NLV: 100_000, BuyingPower: 400_000, Currency: "EUR",
	}
	t.Run("no target -> reward fields stay nil", func(t *testing.T) {
		res, err := ComputeSize(base)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if res.R != nil || res.RewardQuote != nil || res.BreakevenWinRate != nil || res.Target != nil {
			t.Errorf("expected reward fields nil without target, got R=%v reward=%v be=%v target=%v",
				res.R, res.RewardQuote, res.BreakevenWinRate, res.Target)
		}
	})
	t.Run("long 2R target -> R=2 and 33.3% breakeven", func(t *testing.T) {
		in := base
		in.Target = 210 // reward 10, risk 5 → R=2
		res, err := ComputeSize(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if res.R == nil || math.Abs(*res.R-2.0) > 1e-9 {
			t.Fatalf("R = %v, want 2.0", res.R)
		}
		if res.BreakevenWinRate == nil || math.Abs(*res.BreakevenWinRate-(1.0/3.0)) > 1e-9 {
			t.Fatalf("breakeven = %v, want 0.3333", res.BreakevenWinRate)
		}
		// 200 shares × 10 reward = 2000 quote ccy.
		if res.RewardQuote == nil || math.Abs(*res.RewardQuote-2000) > 1e-9 {
			t.Fatalf("reward_quote = %v, want 2000", res.RewardQuote)
		}
	})
	t.Run("short 3R target -> R=3 and 25% breakeven", func(t *testing.T) {
		in := SizeInput{
			Symbol: "AAPL", Side: "short",
			Entry: 200, Stop: 205, Target: 185, // risk 5, reward 15
			RiskPct: 1.0, Lot: 1, FX: 1.0,
			NLV: 100_000, BuyingPower: 400_000, Currency: "EUR",
		}
		res, err := ComputeSize(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if res.R == nil || math.Abs(*res.R-3.0) > 1e-9 {
			t.Fatalf("R = %v, want 3.0", res.R)
		}
		if res.BreakevenWinRate == nil || math.Abs(*res.BreakevenWinRate-0.25) > 1e-9 {
			t.Fatalf("breakeven = %v, want 0.25", res.BreakevenWinRate)
		}
	})
}
