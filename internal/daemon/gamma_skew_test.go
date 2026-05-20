package daemon

import (
	"math"
	"testing"
)

// TestFitSkewCurve_BasicQuadratic verifies the least-squares fit recovers
// the planted (A, B, C) coefficients from a noise-free sample.
func TestFitSkewCurve_BasicQuadratic(t *testing.T) {
	// Plant σ(m) = 0.20 - 0.50·m + 2.00·m² (steep skew, classic SPX).
	spot := 5000.0
	plantA, plantB, plantC := 0.20, -0.50, 2.00
	strikes := []float64{4700, 4800, 4900, 5000, 5100, 5200, 5300}
	var legs []legData
	for _, k := range strikes {
		m := math.Log(k / spot)
		σ := plantA + plantB*m + plantC*m*m
		legs = append(legs, legData{
			expiryYMD: "20260619",
			dte:       30.0 / 365,
			strike:    k,
			right:     "C",
			isCall:    true,
			iv:        σ,
			oi:        1000,
		})
	}
	curve := fitSkewCurve(legs, spot)
	if !curve.ok {
		t.Fatalf("curve marked unfit on 7 noise-free points")
	}
	if curve.nPoints != len(strikes) {
		t.Errorf("nPoints = %d, want %d", curve.nPoints, len(strikes))
	}
	const tol = 1e-6
	if math.Abs(curve.A-plantA) > tol {
		t.Errorf("A = %v, want %v", curve.A, plantA)
	}
	if math.Abs(curve.B-plantB) > tol {
		t.Errorf("B = %v, want %v", curve.B, plantB)
	}
	if math.Abs(curve.C-plantC) > tol {
		t.Errorf("C = %v, want %v", curve.C, plantC)
	}
	// IVAtMoneyness should recover the planted IV at a known m.
	m := math.Log(5000.0 / spot) // = 0
	got := curve.IVAtMoneyness(m)
	if math.Abs(got-plantA) > tol {
		t.Errorf("IVAtMoneyness(0) = %v, want %v", got, plantA)
	}
}

// TestFitSkewCurve_TooFewPoints returns !ok with fewer than 3 IV samples.
func TestFitSkewCurve_TooFewPoints(t *testing.T) {
	legs := []legData{
		{expiryYMD: "20260619", strike: 4900, iv: 0.20},
		{expiryYMD: "20260619", strike: 5000, iv: 0.18},
	}
	curve := fitSkewCurve(legs, 5000)
	if curve.ok {
		t.Errorf("expected !ok with 2 points, got ok=true")
	}
}

// TestFitSkewCurve_ClampsExtrapolation: a curve fitted over m ∈ [-0.1, +0.1]
// should clamp evaluations outside the range to the boundary value,
// preventing the parabola from projecting wildly into untested moneyness.
func TestFitSkewCurve_ClampsExtrapolation(t *testing.T) {
	spot := 5000.0
	strikes := []float64{4750, 4900, 5000, 5100, 5250}
	var legs []legData
	for _, k := range strikes {
		m := math.Log(k / spot)
		σ := 0.18 - 0.40*m + 1.80*m*m
		legs = append(legs, legData{strike: k, iv: σ})
	}
	curve := fitSkewCurve(legs, spot)
	if !curve.ok {
		t.Fatalf("curve unfit")
	}
	// Evaluate at m far beyond the fitted range — the clamped value
	// should equal the value at the boundary, not the parabolic
	// extrapolation.
	mFar := 0.5 // way past mHi ≈ 0.05
	clamped := curve.IVAtMoneyness(mFar)
	atBoundary := curve.A + curve.B*curve.mHi + curve.C*curve.mHi*curve.mHi
	const tol = 1e-9
	if math.Abs(clamped-atBoundary) > tol {
		t.Errorf("IVAtMoneyness(0.5) = %v, want boundary value %v (no extrapolation)",
			clamped, atBoundary)
	}
}

// TestSkewFitRSquared_PerfectFit returns 1 on noise-free data.
func TestSkewFitRSquared_PerfectFit(t *testing.T) {
	spot := 5000.0
	strikes := []float64{4800, 4900, 5000, 5100, 5200}
	var legs []legData
	for _, k := range strikes {
		m := math.Log(k / spot)
		legs = append(legs, legData{strike: k, iv: 0.20 - 0.50*m + 2.0*m*m})
	}
	curve := fitSkewCurve(legs, spot)
	r2 := skewFitRSquared(curve, legs, spot)
	const tol = 1e-9
	if math.Abs(r2-1.0) > tol {
		t.Errorf("R² = %v, want 1.0 on noise-free fit", r2)
	}
}

// TestSweepProfile_StickyMoneynessReprice: with a fitted skew curve the
// sweep should produce a different IV per scenario spot than the sticky-IV
// recipe. Verifies the skew lookup is actually plumbed through.
func TestSweepProfile_StickyMoneynessReprice(t *testing.T) {
	spot := 5000.0
	// Steep skew: σ falls as m rises (calls cheaper, puts richer).
	strikes := []float64{4500, 4750, 5000, 5250, 5500}
	var legs []legData
	for _, k := range strikes {
		m := math.Log(k / spot)
		σ := 0.20 - 0.60*m + 1.5*m*m
		legs = append(legs, legData{
			expiryYMD: "20260619",
			dte:       30.0 / 365,
			strike:    k,
			right:     "C",
			isCall:    true,
			iv:        σ,
			oi:        10_000,
		})
	}
	curves := map[string]SkewCurve{
		"20260619": fitSkewCurve(legs, spot),
	}
	withSkew := sweepProfile(legs, spot, 0.10, curves)
	noSkew := sweepProfile(legs, spot, 0.10, nil)
	if len(withSkew) != len(noSkew) {
		t.Fatalf("length mismatch: %d vs %d", len(withSkew), len(noSkew))
	}
	// The endpoints of the sweep should differ — that's where the skew
	// reprice has the most impact (moneyness moves the most).
	diffEdge := math.Abs(withSkew[0].GEX - noSkew[0].GEX)
	if diffEdge < 1e-9 {
		t.Errorf("sweep endpoints identical between sticky-IV and sticky-moneyness — skew lookup not wired through")
	}
}
