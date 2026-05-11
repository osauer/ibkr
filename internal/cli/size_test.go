package cli

import (
	"bytes"
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

// renderSizeText must surface the user-facing fields and only mention status
// when it isn't ok (avoid noise on the happy path).
func TestRenderSizeText(t *testing.T) {
	t.Parallel()

	t.Run("happy path stays quiet on status", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		r := &SizeResult{
			Symbol: "AAPL", Side: "long", Entry: 207.50, Stop: 202.50,
			RiskPct: 1.0, Lot: 1, FX: 1.0, NLV: 100_000, BaseCurrency: "EUR",
			RiskBase: 1000, RiskQuote: 1000, PerShareRisk: 5,
			Shares: 200, Notional: 41500, MaxLoss: 1000, Status: "ok",
		}
		if code := renderSizeText(env, r); code != 0 {
			t.Fatalf("code = %d", code)
		}
		out := stdout.String()
		for _, want := range []string{"AAPL", "long", "EUR", "200", "Net liquidation", "Risk budget", "Per-share risk", "Notional", "Max loss"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q\n%s", want, out)
			}
		}
		if strings.Contains(out, "status:") {
			t.Errorf("happy path should not surface status line:\n%s", out)
		}
		// fx=1.0 → suppress the fx-specific row to keep output clean.
		if strings.Contains(out, "Risk in quote ccy") {
			t.Errorf("fx=1.0 should suppress Risk-in-quote-ccy row:\n%s", out)
		}
	})

	t.Run("fx != 1 surfaces quote-ccy row", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		r := &SizeResult{
			Symbol: "AAPL", Side: "long", Entry: 207.50, Stop: 202.50,
			RiskPct: 1.0, Lot: 1, FX: 1.085, NLV: 100_000, BaseCurrency: "EUR",
			RiskBase: 1000, RiskQuote: 1085, PerShareRisk: 5,
			Shares: 217, Notional: 45028, MaxLoss: 1085, Status: "ok",
		}
		_ = renderSizeText(env, r)
		out := stdout.String()
		if !strings.Contains(out, "Risk in quote ccy") || !strings.Contains(out, "1.0850") {
			t.Errorf("expected Risk-in-quote-ccy row with fx 1.0850:\n%s", out)
		}
	})

	t.Run("non-ok status surfaced with hint", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		r := &SizeResult{
			Symbol: "AAPL", Side: "long", Entry: 5, Stop: 4.99,
			RiskPct: 1.0, Lot: 1, FX: 1.0, NLV: 100, BaseCurrency: "EUR",
			RiskBase: 1, RiskQuote: 1, PerShareRisk: 0.01,
			Shares: 0, Notional: 0, MaxLoss: 0, Status: "tight_risk",
		}
		_ = renderSizeText(env, r)
		out := stdout.String()
		if !strings.Contains(out, "tight_risk") || !strings.Contains(out, "widen the stop") {
			t.Errorf("expected tight_risk hint in output:\n%s", out)
		}
	})
}
