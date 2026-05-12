package daemon

import (
	"math"
	"testing"
	"time"
)

// TestDTEFromDate covers the day-count helper that feeds the implied-move
// formula. Pure date arithmetic — no clock dependency beyond what the
// caller passes in.
func TestDTEFromDate(t *testing.T) {
	t.Parallel()

	loc := time.UTC
	today := time.Date(2026, 5, 12, 0, 0, 0, 0, loc)

	tests := []struct {
		name   string
		expiry string
		want   int
	}{
		{"same day", "2026-05-12", 0},
		{"one day out", "2026-05-13", 1},
		{"two weeks out", "2026-05-26", 14},
		{"sixty days out", "2026-07-11", 60},
		{"expired (yesterday) clamped to zero", "2026-05-11", 0},
		{"unparseable returns zero", "not-a-date", 0},
		{"empty returns zero", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dteFromDate(today, tc.expiry)
			if got != tc.want {
				t.Errorf("dteFromDate(today, %q) = %d, want %d", tc.expiry, got, tc.want)
			}
		})
	}
}

// TestComputeImpliedMove covers the σ-T expected-move formula
// spot × IV × √(DTE/365). Reference values cross-checked against the CBOE
// option calculator and the standard Black-Scholes 1-σ closed form.
func TestComputeImpliedMove(t *testing.T) {
	t.Parallel()

	f := func(v float64) *float64 { return &v }

	t.Run("happy path: spot 200, IV 30%, 30 DTE", func(t *testing.T) {
		// expected move ≈ 200 × 0.30 × √(30/365) ≈ 17.19
		mv, pct, ok := computeImpliedMove(200, f(0.30), 30)
		if !ok {
			t.Fatal("expected ok=true")
		}
		want := 200.0 * 0.30 * math.Sqrt(30.0/365.0)
		if math.Abs(mv-want) > 1e-9 {
			t.Errorf("move = %v, want %v", mv, want)
		}
		if math.Abs(pct-(mv/200)) > 1e-9 {
			t.Errorf("pct = %v, want %v", pct, mv/200)
		}
	})

	t.Run("DTE 0 -> move is 0 but ok=true", func(t *testing.T) {
		mv, _, ok := computeImpliedMove(100, f(0.50), 0)
		if !ok {
			t.Fatal("expected ok=true for DTE=0")
		}
		if mv != 0 {
			t.Errorf("move at DTE=0 = %v, want 0", mv)
		}
	})

	t.Run("nil IV rejected", func(t *testing.T) {
		_, _, ok := computeImpliedMove(100, nil, 30)
		if ok {
			t.Error("expected ok=false for nil IV")
		}
	})

	t.Run("zero spot rejected", func(t *testing.T) {
		_, _, ok := computeImpliedMove(0, f(0.30), 30)
		if ok {
			t.Error("expected ok=false for spot=0")
		}
	})

	t.Run("negative IV rejected", func(t *testing.T) {
		_, _, ok := computeImpliedMove(100, f(-0.10), 30)
		if ok {
			t.Error("expected ok=false for negative IV")
		}
	})

	t.Run("negative DTE rejected", func(t *testing.T) {
		_, _, ok := computeImpliedMove(100, f(0.30), -5)
		if ok {
			t.Error("expected ok=false for negative DTE")
		}
	})

	t.Run("scales correctly: 4× DTE doubles 1-σ move", func(t *testing.T) {
		mv1, _, _ := computeImpliedMove(100, f(0.20), 30)
		mv2, _, _ := computeImpliedMove(100, f(0.20), 120) // 4× DTE
		// √(4) = 2, so mv2 should be 2× mv1
		ratio := mv2 / mv1
		if math.Abs(ratio-2.0) > 1e-9 {
			t.Errorf("ratio of 4×-DTE move to base = %v, want 2.0", ratio)
		}
	})
}
