package daemon

import (
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

// TestClassifyRegimeAgreement pins the four classifier outcomes plus
// the no-data fallback. Replaces the earlier 20-day price-correlation
// gate — that gate fired ~never because SPY/SPX prices stay > 0.99
// correlated essentially always, and missed the actual case worth
// flagging (gamma regimes that decouple while prices stay tightly
// correlated, which IS the actionable signal).
func TestClassifyRegimeAgreement(t *testing.T) {
	cases := []struct {
		name string
		spy  *rpc.GammaZeroComputed
		spx  *rpc.GammaZeroComputed
		want string
	}{
		{
			name: "both_short_gamma",
			spy:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "agree:short-gamma",
		},
		{
			name: "both_long_gamma",
			spy:  &rpc.GammaZeroComputed{GammaSign: "positive"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "positive"},
			want: "agree:long-gamma",
		},
		{
			name: "both_flipping",
			spy:  &rpc.GammaZeroComputed{ZeroGamma: new(545.0)},
			spx:  &rpc.GammaZeroComputed{ZeroGamma: new(5450.0)},
			want: "agree:flipping",
		},
		{
			name: "disagree_long_vs_short",
			spy:  &rpc.GammaZeroComputed{GammaSign: "positive"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "disagree",
		},
		{
			name: "disagree_flipping_vs_short",
			spy:  &rpc.GammaZeroComputed{ZeroGamma: new(545.0)},
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "disagree",
		},
		{
			name: "no_data_spy",
			spy:  &rpc.GammaZeroComputed{GammaSign: "no_data"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "",
		},
		{
			name: "both_no_data",
			spy:  &rpc.GammaZeroComputed{GammaSign: "no_data"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "no_data"},
			want: "",
		},
		{
			name: "nil_one_side",
			spy:  nil,
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRegimeAgreement(tc.spy, tc.spx); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCombineGammaResultsMergesTopStrikes — SPX rows dominate by raw
// dollar gamma (the 100× per-contract scaling makes this structural).
// The merge produces a single sorted list; INDEX column on the renderer
// makes the imbalance visible rather than hidden.
func TestCombineGammaResultsMergesTopStrikes(t *testing.T) {
	spy := &rpc.GammaZeroComputed{
		SpotUnderlying: 540,
		GammaTotalAbs:  5e9,
		GammaSign:      "negative",
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPY", Strike: 540, Right: "C", AbsGEX: 8e8, Expiry: "2026-06-19"},
			{Underlying: "SPY", Strike: 540, Right: "P", AbsGEX: 6e8, Expiry: "2026-06-19"},
		},
	}
	spx := &rpc.GammaZeroComputed{
		SpotUnderlying: 5400,
		GammaTotalAbs:  18e9,
		GammaSign:      "negative",
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPX", TradingClass: "SPXW", Strike: 5400, Right: "C", AbsGEX: 7e9, Expiry: "2026-06-19"},
			{Underlying: "SPX", TradingClass: "SPXW", Strike: 5300, Right: "P", AbsGEX: 5e9, Expiry: "2026-06-19"},
		},
	}
	combined := combineGammaResults(spy, spx)
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
	// Regime classifier fires on the inputs (both short-γ → agree).
	if combined.RegimeAgreement != "agree:short-gamma" {
		t.Errorf("RegimeAgreement = %q, want agree:short-gamma", combined.RegimeAgreement)
	}
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

// TestCombineGammaResultsDisagreementSurfaces pins the actionable
// case — SPY long-γ + SPX short-γ — produces RegimeAgreement="disagree"
// so the renderer can flag the institutional/retail divergence.
func TestCombineGammaResultsDisagreementSurfaces(t *testing.T) {
	spy := &rpc.GammaZeroComputed{SpotUnderlying: 540, GammaSign: "positive"}
	spx := &rpc.GammaZeroComputed{SpotUnderlying: 5400, GammaSign: "negative"}
	combined := combineGammaResults(spy, spx)
	if combined.RegimeAgreement != "disagree" {
		t.Errorf("RegimeAgreement = %q, want disagree", combined.RegimeAgreement)
	}
}

// TestGammaScopeForRequestDefaultsToCombined pins that an empty Scope
// (today's `ibkr gamma` no flag) lands on the combined path.
func TestGammaScopeForRequestDefaultsToCombined(t *testing.T) {
	got, err := gammaScopeForRequest("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != rpc.GammaZeroScopeCombined {
		t.Errorf("empty scope: got %q, want %q", got, rpc.GammaZeroScopeCombined)
	}
}
