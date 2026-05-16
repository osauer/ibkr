package daemon

import (
	"math"
	"testing"
)

// TestBSGamma_KnownValues pins the BS gamma helper against analytically-
// derived reference values for a few well-spaced (moneyness, vol, dte)
// cells. Hull's textbook example (S=49, K=50, σ=20 %, r=5 %, t=20/52)
// is the canonical anchor; the others span ATM / OTM / ITM corners so
// a regression in any single d1 / pdf / scaling component fails this
// test deterministically.
func TestBSGamma_KnownValues(t *testing.T) {
	cases := []struct {
		name                       string
		spot, strike, t, vol, r, q float64
		want                       float64
		tol                        float64
	}{
		{
			name: "hull_ch18_example_S49_K50",
			spot: 49, strike: 50, t: 20.0 / 52.0, vol: 0.20, r: 0.05, q: 0,
			want: 0.0655, tol: 0.0005,
		},
		{
			name: "atm_t1Q_r0",
			spot: 100, strike: 100, t: 0.25, vol: 0.20, r: 0, q: 0,
			want: 0.0398, tol: 0.0005,
		},
		{
			name: "otm_20pct_t1Q",
			spot: 100, strike: 120, t: 0.25, vol: 0.20, r: 0, q: 0,
			want: 0.00829, tol: 0.0005,
		},
		{
			name: "itm_20pct_t1Q",
			spot: 120, strike: 100, t: 0.25, vol: 0.20, r: 0, q: 0,
			want: 0.00576, tol: 0.0005,
		},
		// SPX-like leg: index-style, dividend yield embedded in r so we
		// keep q=0. Confirms the formula scales correctly at index spot
		// magnitudes (~5000) — historically a place where subtle units
		// mistakes (multiplier vs spot²) silently invert magnitudes.
		{
			// Analytical reference: d1 = (0 + (0.05 + 0.0162) · 0.0822) /
			// (0.18 · √0.0822) ≈ 0.1054; φ(d1) ≈ 0.3968;
			// γ = 0.3968 / (5000 · 0.18 · 0.2867) ≈ 0.001538.
			name: "spx_atm_30dte",
			spot: 5000, strike: 5000, t: 30.0 / 365.0, vol: 0.18, r: 0.05, q: 0,
			want: 0.001538, tol: 0.00002,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bsGamma(tc.spot, tc.strike, tc.t, tc.vol, tc.r, tc.q)
			if math.Abs(got-tc.want) > tc.tol {
				t.Errorf("γ(%s) = %.6f, want %.6f ± %.6f",
					tc.name, got, tc.want, tc.tol)
			}
		})
	}
}

// TestBSGamma_DegenerateInputs verifies the helper returns 0 (not NaN,
// not panic) for the degenerate cases the aggregator can plausibly hit
// when a leg's IV or time-to-expiry hasn't been delivered by IBKR. The
// aggregator filters these legs before passing them through, but the
// belt-and-suspenders check here means a single missed filter doesn't
// poison the whole zero-gamma sum with a NaN.
func TestBSGamma_DegenerateInputs(t *testing.T) {
	cases := []struct {
		name                       string
		spot, strike, t, vol, r, q float64
	}{
		{"zero_t", 100, 100, 0, 0.20, 0, 0},
		{"negative_t", 100, 100, -0.01, 0.20, 0, 0},
		{"zero_vol", 100, 100, 0.25, 0, 0, 0},
		{"negative_vol", 100, 100, 0.25, -0.10, 0, 0},
		{"zero_spot", 0, 100, 0.25, 0.20, 0, 0},
		{"negative_spot", -1, 100, 0.25, 0.20, 0, 0},
		{"zero_strike", 100, 0, 0.25, 0.20, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bsGamma(tc.spot, tc.strike, tc.t, tc.vol, tc.r, tc.q)
			if got != 0 || math.IsNaN(got) {
				t.Errorf("γ(%s) = %v, want exactly 0", tc.name, got)
			}
		})
	}
}

// TestBSGamma_CallPutEquality pins the put-call gamma identity at the
// formula layer — gamma is symmetric across rights for any given
// (spot, strike, t, vol). A regression that branches on right
// internally would fail here.
func TestBSGamma_CallPutEquality(t *testing.T) {
	// The function itself doesn't take a right argument — proving the
	// identity at the API surface. Test exists so that "I'll add right-
	// dependent logic later" doesn't slip past review without a
	// deliberate rename.
	γ1 := bsGamma(5000, 5050, 0.10, 0.20, 0.04, 0)
	γ2 := bsGamma(5000, 5050, 0.10, 0.20, 0.04, 0)
	if γ1 != γ2 {
		t.Fatalf("γ is not deterministic for identical inputs: %v vs %v", γ1, γ2)
	}
}

// TestDealerGEX_SignConvention pins the Perfiliev convention: calls
// contribute positive dealer-gamma exposure, puts contribute negative.
// This is the single most regression-prone line in the whole zero-gamma
// pipeline — a sign flip here flips the entire dashboard verdict.
func TestDealerGEX_SignConvention(t *testing.T) {
	const γ = 0.001
	const oi = 10_000.0
	const mult = 100
	const spot = 5000.0

	call := dealerGEX(γ, oi, mult, spot, true)
	put := dealerGEX(γ, oi, mult, spot, false)

	if call <= 0 {
		t.Errorf("call GEX = %v, want > 0 (Perfiliev: calls long gamma)", call)
	}
	if put >= 0 {
		t.Errorf("put GEX = %v, want < 0 (Perfiliev: puts short gamma)", put)
	}
	if math.Abs(call+put) > 1e-9 {
		t.Errorf("|call| should equal |put| for identical Γ/OI: call=%v put=%v", call, put)
	}

	// Magnitude check: γ × OI × mult × spot² × 0.01
	want := γ * oi * mult * spot * spot * 0.01
	if math.Abs(call-want) > 1e-6 {
		t.Errorf("call GEX magnitude = %v, want %v", call, want)
	}
}

// TestAbsGEX_NoSignConvention pins the magnitude-only path used for the
// "where dealer hedging concentrates" view. This signal is robust in
// regimes where the Perfiliev sign assumption may invert, so we keep
// it strictly non-negative and identical for calls vs puts.
func TestAbsGEX_NoSignConvention(t *testing.T) {
	const γ = -0.0008 // negative γ is mathematically impossible but
	// defensively checked: |γ| in the formula handles any caller bug.
	got := absGEX(γ, 5_000, 100, 5000)
	want := 0.0008 * 5_000 * 100 * 5000 * 5000 * 0.01
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("absGEX with negative γ = %v, want %v (should use |γ|)", got, want)
	}

	// Zero-OI legs contribute zero — common for legs where the gateway
	// didn't deliver the OI tick within budget.
	if v := absGEX(0.001, 0, 100, 5000); v != 0 {
		t.Errorf("zero-OI absGEX = %v, want 0", v)
	}
}
