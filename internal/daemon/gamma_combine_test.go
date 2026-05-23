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
	// SpotAnchor codifies the shallow-copy: top-level Spot/ZeroGamma/etc.
	// reflect the SPY half. Consumers branch on this to decide whether
	// the top-level scalars are "combined" or "anchored on one underlying".
	if combined.SpotAnchor != "SPY" {
		t.Errorf("SpotAnchor = %q, want %q (combined envelope shallow-copies SPY)", combined.SpotAnchor, "SPY")
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

// TestCombineGammaResultsSumsProfileOnSharedGrid covers the happy
// path of combineProfileBuckets: when both halves share an identical
// Spot grid (which is contrived for SPY+SPX but the helper's contract
// nonetheless), the combined Profile carries the bucket-wise GEX sum.
//
// SPY-only field passthrough is also documented here: the shallow
// copy means SpotUnderlying / ZeroGamma / GammaSign on the combined
// envelope are SPY's, not "combined" — see the field-by-field intent
// map above combineGammaResults' shallow copy.
func TestCombineGammaResultsSumsProfileOnSharedGrid(t *testing.T) {
	// Contrived shared grid (real SPY/SPX never align this way —
	// SPY anchors ~540, SPX anchors ~5400 — so the mismatch path
	// is the production case; see TestCombineGammaResultsProfileGridMismatch).
	spy := &rpc.GammaZeroComputed{
		SpotUnderlying: 110,
		ZeroGamma:      new(108.0),
		GammaSign:      "negative",
		Profile: []rpc.GammaProfilePoint{
			{Spot: 100, GEX: 10},
			{Spot: 110, GEX: 20},
			{Spot: 120, GEX: 30},
		},
		ProfileNear: []rpc.GammaProfilePoint{
			{Spot: 100, GEX: 1},
			{Spot: 110, GEX: 2},
			{Spot: 120, GEX: 3},
		},
	}
	spx := &rpc.GammaZeroComputed{
		SpotUnderlying: 110,
		Profile: []rpc.GammaProfilePoint{
			{Spot: 100, GEX: -5},
			{Spot: 110, GEX: 100},
			{Spot: 120, GEX: -7},
		},
		ProfileNear: []rpc.GammaProfilePoint{
			{Spot: 100, GEX: -1},
			{Spot: 110, GEX: 4},
			{Spot: 120, GEX: -2},
		},
	}
	combined := combineGammaResults(spy, spx)
	if combined == nil {
		t.Fatal("combined is nil")
	}
	wantProfile := []rpc.GammaProfilePoint{
		{Spot: 100, GEX: 5},
		{Spot: 110, GEX: 120},
		{Spot: 120, GEX: 23},
	}
	if len(combined.Profile) != len(wantProfile) {
		t.Fatalf("combined.Profile len = %d, want %d (Profile=%+v)", len(combined.Profile), len(wantProfile), combined.Profile)
	}
	for i, w := range wantProfile {
		if combined.Profile[i] != w {
			t.Errorf("combined.Profile[%d] = %+v, want %+v", i, combined.Profile[i], w)
		}
	}
	wantNear := []rpc.GammaProfilePoint{
		{Spot: 100, GEX: 0},
		{Spot: 110, GEX: 6},
		{Spot: 120, GEX: 1},
	}
	for i, w := range wantNear {
		if combined.ProfileNear[i] != w {
			t.Errorf("combined.ProfileNear[%d] = %+v, want %+v", i, combined.ProfileNear[i], w)
		}
	}
	// SPY-only field passthrough — these are SPY's values shallow-
	// copied onto the combined envelope; renderers must pull true
	// per-index numbers from PerIndex.
	t.Logf("SPY-only on combined envelope (carried by shallow copy): SpotUnderlying=%v ZeroGamma=%v GammaSign=%q — consume per-index detail via PerIndex",
		combined.SpotUnderlying, combined.ZeroGamma, combined.GammaSign)
	if combined.SpotUnderlying != spy.SpotUnderlying {
		t.Errorf("SPY passthrough broken: combined.SpotUnderlying=%v, want %v", combined.SpotUnderlying, spy.SpotUnderlying)
	}
	// Grid silence check — no mismatch warning should fire when
	// grids actually match.
	for _, w := range combined.Warnings {
		if w == "combined_profile_grid_mismatch" {
			t.Errorf("grids matched but combined_profile_grid_mismatch fired anyway; warnings=%v", combined.Warnings)
		}
	}
}

// TestCombineGammaResultsProfileGridMismatch covers the bail-with-
// warning branch — realistic SPY (~540) + SPX (~5400) profiles sit
// on different spot scales, so combineProfileBuckets returns nil and
// stamps `combined_profile_grid_mismatch`. The user sees the warning
// in the rendered envelope; per-index curves are still available via
// PerIndex for any consumer that needs a real profile.
func TestCombineGammaResultsProfileGridMismatch(t *testing.T) {
	spy := &rpc.GammaZeroComputed{
		Profile: []rpc.GammaProfilePoint{
			{Spot: 540, GEX: 100},
			{Spot: 550, GEX: -100},
		},
	}
	spx := &rpc.GammaZeroComputed{
		Profile: []rpc.GammaProfilePoint{
			{Spot: 5400, GEX: 1000},
			{Spot: 5500, GEX: -1000},
		},
	}
	combined := combineGammaResults(spy, spx)
	if combined == nil {
		t.Fatal("combined is nil")
	}
	if combined.Profile != nil {
		t.Errorf("grid mismatch: combined.Profile should be nil, got %+v", combined.Profile)
	}
	var sawMismatch bool
	for _, w := range combined.Warnings {
		if w == "combined_profile_grid_mismatch" {
			sawMismatch = true
		}
	}
	if !sawMismatch {
		t.Errorf("grid mismatch warning missing from combined.Warnings: %v", combined.Warnings)
	}
}

// TestCombineGammaResultsWarningsUnion proves Warnings on the combined
// envelope is the dedup'd union of spy.Warnings and spx.Warnings,
// not just the SPY-half slice that came along on the shallow copy.
// A regression here would silently hide SPX-side warnings
// (skew_fallback, throttled, etc.) from the rendered headline.
func TestCombineGammaResultsWarningsUnion(t *testing.T) {
	spy := &rpc.GammaZeroComputed{
		Warnings: []string{"all_iv_derived", "no_crossing_in_window"},
	}
	spx := &rpc.GammaZeroComputed{
		Warnings: []string{"throttled", "no_crossing_in_window"}, // last entry dupes SPY
	}
	combined := combineGammaResults(spy, spx)
	if combined == nil {
		t.Fatal("combined is nil")
	}
	want := map[string]bool{
		"all_iv_derived":        false,
		"no_crossing_in_window": false,
		"throttled":             false,
	}
	for _, w := range combined.Warnings {
		if _, ok := want[w]; ok {
			want[w] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("warnings union missing %q; got %v", k, combined.Warnings)
		}
	}
	// Dedup check — "no_crossing_in_window" appears in both inputs;
	// it must appear exactly once in the union.
	var dupes int
	for _, w := range combined.Warnings {
		if w == "no_crossing_in_window" {
			dupes++
		}
	}
	if dupes != 1 {
		t.Errorf("union should dedup duplicates: got %d copies of no_crossing_in_window in %v", dupes, combined.Warnings)
	}
}
