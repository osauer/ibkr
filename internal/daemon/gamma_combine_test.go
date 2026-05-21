package daemon

import (
	"math"
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

// TestPearsonRMatchesKnownValue pins the correlation calculation
// against a hand-computed value. Two series moving exactly together
// must yield r = 1.0; perfectly inverse yields -1.0; uncorrelated
// random walks should land near 0.
func TestPearsonRMatchesKnownValue(t *testing.T) {
	cases := []struct {
		name string
		x    []float64
		y    []float64
		want float64
	}{
		{"perfect_positive", []float64{1, 2, 3, 4, 5}, []float64{2, 4, 6, 8, 10}, 1.0},
		{"perfect_negative", []float64{1, 2, 3, 4, 5}, []float64{10, 8, 6, 4, 2}, -1.0},
		{"identity", []float64{1.5, 2.5, 3.5}, []float64{1.5, 2.5, 3.5}, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pearsonR(tc.x, tc.y)
			if !ok {
				t.Fatalf("pearsonR returned !ok")
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPearsonRDegenerateInputs pins refuse-on-degenerate cases:
// length mismatch handled by trailing-align truncation; too-short
// series and zero-variance series return !ok rather than NaN/Inf.
func TestPearsonRDegenerateInputs(t *testing.T) {
	t.Run("single_point", func(t *testing.T) {
		if _, ok := pearsonR([]float64{1.0}, []float64{2.0}); ok {
			t.Error("expected !ok for single-point series")
		}
	})
	t.Run("zero_variance_x", func(t *testing.T) {
		if _, ok := pearsonR([]float64{5, 5, 5, 5}, []float64{1, 2, 3, 4}); ok {
			t.Error("expected !ok when x has zero variance")
		}
	})
	t.Run("length_mismatch_trailing_aligned", func(t *testing.T) {
		// Trailing-align truncation: SPY had 19 bars, SPX had 20. The
		// shared trailing 19 should compute cleanly.
		x := []float64{1, 2, 3, 4, 5}
		y := []float64{99, 1, 2, 3, 4, 5}
		got, ok := pearsonR(x, y)
		if !ok || math.Abs(got-1.0) > 1e-9 {
			t.Errorf("trailing-align: got (%v, %v), want (1.0, true)", got, ok)
		}
	})
}

// TestBuildCombinedSweepAggregatesAndFindsZero pins the §5.3
// aggregation rule: sum signed GEX at each scenario-percent index,
// linear-interpolate the zero crossing, return the SPY-anchored
// spot-percent headline.
func TestBuildCombinedSweepAggregatesAndFindsZero(t *testing.T) {
	// SPY profile: GEX rising from negative to positive across spot
	// 510→570. Pure SPY would cross at ~540 (1% above 535 anchor).
	spy := []rpc.GammaProfilePoint{
		{Spot: 510, GEX: -3e9},
		{Spot: 540, GEX: 0},
		{Spot: 570, GEX: +3e9},
	}
	// SPX profile: a much bigger book that drags the combined zero
	// down: starts more negative, crosses at the SECOND point too.
	// Combined will cross where SPY+SPX = 0 — between point 0 and 1.
	spx := []rpc.GammaProfilePoint{
		{Spot: 5100, GEX: -10e9},
		{Spot: 5400, GEX: -1e9},
		{Spot: 5700, GEX: +8e9},
	}

	spySpot := 535.0
	combined, gapPct := buildCombinedSweep(spy, spx, spySpot)

	if len(combined) != 3 {
		t.Fatalf("combined len = %d, want 3", len(combined))
	}
	// Combined GEX at each scenario index = SPY[i] + SPX[i].
	wantGEX := []float64{-13e9, -1e9, +11e9}
	for i, p := range combined {
		if math.Abs(p.GEX-wantGEX[i]) > 1e-9 {
			t.Errorf("combined[%d].GEX = %v, want %v", i, p.GEX, wantGEX[i])
		}
	}
	// Zero crossing lies between point 1 (GEX=-1e9 at Spot=540) and
	// point 2 (+11e9 at Spot=570). Linear interp: 540 + 30 × (1/12) ≈ 542.5.
	// Combined gap = (542.5 - 535) / 535 × 100 ≈ +1.4 %.
	if gapPct == nil {
		t.Fatal("gapPct == nil, want positive crossing")
	}
	if *gapPct < 1.0 || *gapPct > 2.0 {
		t.Errorf("combined gapPct = %v, want between 1.0 and 2.0", *gapPct)
	}
}

// TestBuildCombinedSweepReturnsNilWhenEitherEmpty pins the empty
// case — combine never invents data when one half is missing.
func TestBuildCombinedSweepReturnsNilWhenEitherEmpty(t *testing.T) {
	t.Run("spy_empty", func(t *testing.T) {
		_, gap := buildCombinedSweep(nil, []rpc.GammaProfilePoint{{Spot: 5400, GEX: 1}}, 540)
		if gap != nil {
			t.Errorf("expected nil gap on empty SPY; got %v", *gap)
		}
	})
	t.Run("spx_empty", func(t *testing.T) {
		_, gap := buildCombinedSweep([]rpc.GammaProfilePoint{{Spot: 540, GEX: 1}}, nil, 540)
		if gap != nil {
			t.Errorf("expected nil gap on empty SPX; got %v", *gap)
		}
	})
}

// TestCombineGammaResultsMergesTopStrikes pins the user-interview
// pick (single sorted list with INDEX column). SPX rows dominate by
// raw dollar gamma; the merge sorts by AbsGEX and takes overall top-K.
func TestCombineGammaResultsMergesTopStrikes(t *testing.T) {
	spy := &rpc.GammaZeroComputed{
		SpotUnderlying: 540,
		GammaTotalAbs:  5e9,
		Profile: []rpc.GammaProfilePoint{
			{Spot: 510, GEX: -1e9}, {Spot: 540, GEX: 0}, {Spot: 570, GEX: 1e9},
		},
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPY", Strike: 540, Right: "C", AbsGEX: 8e8, Expiry: "2026-06-19"},
			{Underlying: "SPY", Strike: 540, Right: "P", AbsGEX: 6e8, Expiry: "2026-06-19"},
		},
	}
	spx := &rpc.GammaZeroComputed{
		SpotUnderlying: 5400,
		GammaTotalAbs:  18e9,
		Profile: []rpc.GammaProfilePoint{
			{Spot: 5100, GEX: -3e9}, {Spot: 5400, GEX: 0}, {Spot: 5700, GEX: 3e9},
		},
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPX", TradingClass: "SPXW", Strike: 5400, Right: "C", AbsGEX: 7e9, Expiry: "2026-06-19"},
			{Underlying: "SPX", TradingClass: "SPXW", Strike: 5300, Right: "P", AbsGEX: 5e9, Expiry: "2026-06-19"},
		},
	}
	combined := combineGammaResults(spy, spx, nil)
	if combined == nil {
		t.Fatal("combined is nil")
	}
	if combined.Scope != rpc.GammaZeroScopeCombined {
		t.Errorf("Scope = %q, want %q", combined.Scope, rpc.GammaZeroScopeCombined)
	}
	if combined.PerIndex["SPY"] != spy || combined.PerIndex["SPX"] != spx {
		t.Errorf("PerIndex pointers don't match the inputs")
	}
	if combined.GammaTotalAbs != 23e9 {
		t.Errorf("combined GammaTotalAbs = %v, want 23e9", combined.GammaTotalAbs)
	}
	// Top strikes: SPX rows dominate (7e9, 5e9 > 8e8, 6e8). Order
	// must be SPX 5400 C, SPX 5300 P, SPY 540 C, SPY 540 P.
	if len(combined.TopStrikes) != 4 {
		t.Fatalf("merged top strikes len = %d, want 4: %+v", len(combined.TopStrikes), combined.TopStrikes)
	}
	wantOrder := []struct {
		underlying string
		absGEX     float64
	}{
		{"SPX", 7e9},
		{"SPX", 5e9},
		{"SPY", 8e8},
		{"SPY", 6e8},
	}
	for i, w := range wantOrder {
		if combined.TopStrikes[i].Underlying != w.underlying || combined.TopStrikes[i].AbsGEX != w.absGEX {
			t.Errorf("top[%d] = %+v, want underlying=%s absGEX=%v",
				i, combined.TopStrikes[i], w.underlying, w.absGEX)
		}
	}
}

// TestCombineGammaResultsDecoupledWarning pins the 0.90 correlation
// gate — when corr < threshold, the renderer gets a "decoupled"
// warning so it can promote per-index headlines to primary.
func TestCombineGammaResultsDecoupledWarning(t *testing.T) {
	spy := &rpc.GammaZeroComputed{
		SpotUnderlying: 540,
		Profile:        []rpc.GammaProfilePoint{{Spot: 540, GEX: 0}},
	}
	spx := &rpc.GammaZeroComputed{
		SpotUnderlying: 5400,
		Profile:        []rpc.GammaProfilePoint{{Spot: 5400, GEX: 0}},
	}
	corrLow := 0.82
	combined := combineGammaResults(spy, spx, &corrLow)
	if combined == nil {
		t.Fatal("combined is nil")
	}
	found := false
	for _, w := range combined.Warnings {
		if w == "decoupled" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings missing 'decoupled' for corr=%v: %v", corrLow, combined.Warnings)
	}

	// corr above threshold → no decoupled warning.
	corrHigh := 0.97
	combinedHi := combineGammaResults(spy, spx, &corrHigh)
	for _, w := range combinedHi.Warnings {
		if w == "decoupled" {
			t.Errorf("unexpected 'decoupled' warning at corr=%v: %v", corrHigh, combinedHi.Warnings)
		}
	}

	// nil corr → no decoupled warning (couldn't compute, don't speculate).
	combinedNil := combineGammaResults(spy, spx, nil)
	for _, w := range combinedNil.Warnings {
		if w == "decoupled" {
			t.Errorf("unexpected 'decoupled' warning at nil corr: %v", combinedNil.Warnings)
		}
	}
}

// TestGammaScopeForRequestDefaultsToCombined pins that the empty-Scope
// caller (today's `ibkr gamma` no flag) lifts to the combined
// canonical path post-step-7.
func TestGammaScopeForRequestDefaultsToCombined(t *testing.T) {
	got, err := gammaScopeForRequest("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != rpc.GammaZeroScopeCombined {
		t.Errorf("empty scope: got %q, want %q", got, rpc.GammaZeroScopeCombined)
	}
}
