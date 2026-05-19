package daemon

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// TestNormalizeGammaParams fills in defaults for unset / negative
// fields without clobbering legitimate caller overrides.
func TestNormalizeGammaParams(t *testing.T) {
	cases := []struct {
		name string
		in   rpc.GammaZeroParams
		want rpc.GammaZeroParams
	}{
		{
			name: "all_defaults",
			in:   rpc.GammaZeroParams{},
			want: rpc.GammaZeroParams{
				ExpiryCount:    6,
				StrikeWidthPct: 0.10,
				SweepRangePct:  0.15,
				WorkerCount:    4,
			},
		},
		{
			name: "respects_overrides",
			in: rpc.GammaZeroParams{
				ExpiryCount: 10, StrikeWidthPct: 0.05, SweepRangePct: 0.20, WorkerCount: 8,
			},
			want: rpc.GammaZeroParams{
				ExpiryCount: 10, StrikeWidthPct: 0.05, SweepRangePct: 0.20, WorkerCount: 8,
			},
		},
		{
			name: "treats_negative_as_unset",
			in:   rpc.GammaZeroParams{ExpiryCount: -1, StrikeWidthPct: 0.05},
			want: rpc.GammaZeroParams{
				ExpiryCount:    6,
				StrikeWidthPct: 0.05,
				SweepRangePct:  0.15,
				WorkerCount:    4,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeGammaParams(tc.in)
			if got != tc.want {
				t.Errorf("normalize(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSelectExpirations pins the 0DTE-post-settlement filter at the NY
// 16:15 cutoff, and confirms that pre-cutoff same-day expiries are
// kept. The cutoff is intentionally conservative for v1 (one rule
// across SPX AM-settled + SPXW PM-settled) per the methodology doc.
func TestSelectExpirations(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	chain := map[string][]float64{
		"2026-05-15": {5000}, // yesterday
		"2026-05-16": {5000}, // today
		"2026-05-17": {5000}, // tomorrow
		"2026-05-19": {5000}, // next week
		"2026-05-26": {5000},
		"2026-06-19": {5000}, // monthly
		"2026-09-18": {5000}, // quarterly
		"2026-12-18": {5000},
	}

	t.Run("morning_today_included", func(t *testing.T) {
		now := time.Date(2026, 5, 16, 10, 0, 0, 0, loc)
		got := selectExpirations(chain, now, 4)
		want := []string{"2026-05-16", "2026-05-17", "2026-05-19", "2026-05-26"}
		if !equalSlice(got, want) {
			t.Errorf("morning: got %v, want %v", got, want)
		}
	})

	t.Run("post_settlement_today_excluded", func(t *testing.T) {
		now := time.Date(2026, 5, 16, 17, 0, 0, 0, loc) // 17:00 ET, past 16:15
		got := selectExpirations(chain, now, 4)
		want := []string{"2026-05-17", "2026-05-19", "2026-05-26", "2026-06-19"}
		if !equalSlice(got, want) {
			t.Errorf("post-settlement: got %v, want %v", got, want)
		}
	})

	t.Run("yesterday_always_excluded", func(t *testing.T) {
		now := time.Date(2026, 5, 16, 10, 0, 0, 0, loc)
		got := selectExpirations(chain, now, 10)
		for _, d := range got {
			if d == "2026-05-15" {
				t.Errorf("selectExpirations included expired date 2026-05-15: got %v", got)
			}
		}
	})

	t.Run("count_caps_result", func(t *testing.T) {
		now := time.Date(2026, 5, 16, 10, 0, 0, 0, loc)
		got := selectExpirations(chain, now, 2)
		if len(got) != 2 {
			t.Errorf("count cap not honored: got %d entries, want 2", len(got))
		}
	})
}

// TestFilterStrikesAroundSpot pins the ±widthPct window logic and the
// defensive sort. SPX chains return strikes in arbitrary order across
// exchange-keyed frames; relying on input order would silently break
// the strike-grid contiguity assumption.
func TestFilterStrikesAroundSpot(t *testing.T) {
	strikes := []float64{4700, 4900, 5500, 4500, 5050, 5000, 4950, 4800, 5100, 5200, 5400}

	got := filterStrikesAroundSpot(strikes, 5000, 0.05) // ±5% = [4750, 5250]
	want := []float64{4800, 4900, 4950, 5000, 5050, 5100, 5200}
	if !equalFloatSlice(got, want) {
		t.Errorf("±5%% around 5000: got %v, want %v", got, want)
	}

	if got := filterStrikesAroundSpot(strikes, 0, 0.10); got != nil {
		t.Errorf("zero spot: got %v, want nil", got)
	}
	if got := filterStrikesAroundSpot(strikes, 5000, 0); got != nil {
		t.Errorf("zero width: got %v, want nil", got)
	}
	if got := filterStrikesAroundSpot(nil, 5000, 0.10); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
}

// TestCompactExpiry round-trips YYYY-MM-DD into the YYYYMMDD form
// SubscribeOption expects, with best-effort behaviour on malformed
// input.
func TestCompactExpiry(t *testing.T) {
	cases := map[string]string{
		"2026-05-16": "20260516",
		"2026-12-31": "20261231",
		"20260516":   "20260516", // already compact
		"":           "",
		"not-a-date": "not-a-date",
		"2026/05/16": "2026/05/16",
		"2026-05-1":  "2026-05-1",
	}
	for in, want := range cases {
		if got := compactExpiry(in); got != want {
			t.Errorf("compactExpiry(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDTEYears computes years-to-expiry to 16:00 ET on the expiration
// date, returning 0 on parse failure or non-positive deltas (the leg
// gate filters these).
func TestDTEYears(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, loc)

	if y := dteYears("20260516", now); y <= 0 || y > 0.01 {
		// 2h to 16:00 ≈ 2 / (24·365) ≈ 0.000228; window [0, 0.01]
		t.Errorf("2h to expiry: got %v, want in (0, 0.01)", y)
	}
	// Roughly 33 days × 24h / (24·365) ≈ 0.0904 years
	y := dteYears("20260619", now)
	if y < 0.080 || y > 0.105 {
		t.Errorf("~33 days: got %v, want [0.080, 0.105]", y)
	}

	if y := dteYears("20260515", now); y != 0 {
		t.Errorf("past date: got %v, want 0", y)
	}
	if y := dteYears("bogus", now); y != 0 {
		t.Errorf("bogus input: got %v, want 0", y)
	}
}

// TestSweepProfile pins the sweep grid shape and exercises the BS
// recompute by checking that a strongly call-skewed chain produces a
// sweep where higher spots → more positive GEX (calls gain delta-
// notional from rising spot; under Perfiliev's call-long convention
// that means more positive dealer GEX).
func TestSweepProfile(t *testing.T) {
	legs := []legData{
		// 100k contracts of 30-DTE ATM calls. Big enough that a single
		// strike drives the whole signed signal.
		{
			expiryYMD: "20260619", dte: 30.0 / 365, strike: 5000,
			right: "C", isCall: true, iv: 0.20, oi: 100_000,
			gammaAtSnapshot: 0.001,
		},
	}
	profile := sweepProfile(legs, 5000, 0.15)
	if len(profile) != sweepPoints {
		t.Fatalf("sweep len = %d, want %d", len(profile), sweepPoints)
	}
	// Endpoints span 0.85 → 1.15 × 5000
	if math.Abs(profile[0].Spot-4250) > 0.5 {
		t.Errorf("first spot = %v, want ~4250", profile[0].Spot)
	}
	if math.Abs(profile[len(profile)-1].Spot-5750) > 0.5 {
		t.Errorf("last spot = %v, want ~5750", profile[len(profile)-1].Spot)
	}
	// Single-call chain: at spot = strike GEX is positive; gamma
	// decays at the tails so magnitude near 5000 should exceed the
	// extremes. (Trivially true for an ATM call's symmetric gamma
	// peak.)
	atSpotIdx := sweepPoints / 2
	if profile[atSpotIdx].GEX <= 0 {
		t.Errorf("ATM GEX should be positive for a long-call book, got %v", profile[atSpotIdx].GEX)
	}
	if profile[0].GEX > profile[atSpotIdx].GEX {
		t.Errorf("far-OTM GEX should be smaller than ATM: %v vs %v",
			profile[0].GEX, profile[atSpotIdx].GEX)
	}

	// Empty legs: the sweep still builds the spot grid (the renderer
	// charts a flat-zero curve), but every GEX point is exactly 0.
	// Documenting this rather than guarding because the compute
	// itself returns an error before reaching sweepProfile when no
	// legs are usable, so this is only a defensive shape pin.
	empty := sweepProfile(nil, 5000, 0.15)
	if len(empty) != sweepPoints {
		t.Errorf("empty legs len = %d, want %d", len(empty), sweepPoints)
	}
	for i, p := range empty {
		if p.GEX != 0 {
			t.Errorf("empty legs profile[%d].GEX = %v, want exactly 0", i, p.GEX)
			break
		}
	}
	if got := sweepProfile(legs, 0, 0.15); got != nil {
		t.Errorf("zero spot: got %v, want nil", got)
	}
}

// TestRankTopStrikesByAbsGEX pins the ranking and the
// already-format-conversion (YYYYMMDD → YYYY-MM-DD on the result).
func TestRankTopStrikesByAbsGEX(t *testing.T) {
	legs := []legData{
		{expiryYMD: "20260619", strike: 5000, right: "C", oi: 10_000, gammaAtSnapshot: 0.001},
		{expiryYMD: "20260619", strike: 5050, right: "P", oi: 50_000, gammaAtSnapshot: 0.0008},
		{expiryYMD: "20260626", strike: 5100, right: "C", oi: 5_000, gammaAtSnapshot: 0.0005},
		{expiryYMD: "20260619", strike: 4950, right: "C", oi: 0, gammaAtSnapshot: 0.001},  // dropped: OI=0
		{expiryYMD: "20260619", strike: 4900, right: "P", oi: 10_000, gammaAtSnapshot: 0}, // dropped: γ=0
	}
	top := rankTopStrikesByAbsGEX(legs, 5000, 5)

	if len(top) != 3 {
		t.Fatalf("got %d rows, want 3 (two filtered): %+v", len(top), top)
	}
	// Highest |γ|·OI = 50000 × 0.0008 = 40, 10000 × 0.001 = 10, 5000 × 0.0005 = 2.5
	// Order: 5050P > 5000C > 5100C
	wantOrder := []float64{5050, 5000, 5100}
	for i, w := range wantOrder {
		if top[i].Strike != w {
			t.Errorf("rank[%d] strike = %v, want %v (full: %+v)", i, top[i].Strike, w, top)
		}
	}
	// Expiry format conversion
	if top[0].Expiry != "2026-06-19" {
		t.Errorf("expiry format = %q, want 2026-06-19", top[0].Expiry)
	}
	// OI surfaces through
	if top[0].OI != 50_000 {
		t.Errorf("OI = %d, want 50000", top[0].OI)
	}

	// k=0 disables ranking
	if got := rankTopStrikesByAbsGEX(legs, 5000, 0); got != nil {
		t.Errorf("k=0: got %v, want nil", got)
	}
}

// TestThrottleDetected pins the throttle-abort threshold and sample-size
// policy. The fan-out short-circuits when the no-contract failure rate
// exceeds 5 % after at least 50 completions — chosen so we don't bail
// on startup noise but do react to a degraded gateway before the fan-out
// runs to completion and compounds the rate-limit pressure.
func TestThrottleDetected(t *testing.T) {
	cases := []struct {
		name             string
		done, noContract int32
		want             bool
	}{
		{"below_sample_size_high_ratio", 49, 49, false},  // 100 % but only 49 samples
		{"at_sample_size_below_threshold", 50, 2, false}, // 4 % — just under
		{"at_sample_size_at_threshold", 50, 3, true},     // 6 % — over
		{"deep_run_under_threshold", 400, 19, false},     // 4.75 %
		{"deep_run_over_threshold", 400, 21, true},       // 5.25 %
		{"zero_no_contract", 200, 0, false},              // healthy gateway
		{"zero_completions", 0, 0, false},                // pre-warmup
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := throttleDetected(tc.done, tc.noContract); got != tc.want {
				t.Errorf("throttleDetected(done=%d, nc=%d) = %v, want %v",
					tc.done, tc.noContract, got, tc.want)
			}
		})
	}
}

// TestIsAcceptableDataType pins the stale-data refusal logic. Live
// and frozen pass — frozen is "yesterday's official close" which the
// spec accepts for daily refresh. Delayed and delayed-frozen are
// rejected because 15-min lag corrupts the BS-vs-spot anchoring.
func TestIsAcceptableDataType(t *testing.T) {
	cases := map[string]bool{
		"":               true,
		"live":           true,
		"frozen":         true,
		"delayed":        false,
		"delayed-frozen": false,
		"unknown":        false, // forward-compat: unknown values are stale-by-default
	}
	for dt, want := range cases {
		if got := isAcceptableDataType(dt); got != want {
			t.Errorf("isAcceptableDataType(%q) = %v, want %v", dt, got, want)
		}
	}
}

// ---------- v0.26.0: BS-IV pre-market fallback path ----------

// TestBSIVFallback_AssemblesLegFromSyntheticPrice drives the bsIVFallback
// helper end-to-end on a realistic pre-market scenario. SPY 7-DTE strike
// 735 priced at a known σ via bsCallPrice + put-call parity, then
// passed through the same helper productionLegFetcher uses when the
// gateway didn't push a model tick. Asserts the assembled legResult
// carries IVDerived=true, OK=true, the recovered σ within 5 bps, a
// physical (positive, finite) gamma, and the OI threaded through
// unchanged.
//
// This is the regression pin the v0.26.0 CHANGELOG's "pre-market gamma
// lands a real result" claim warrants. If a future refactor breaks
// Stage 2b (wrong sign branch, wrong parity formula, drops OI, mis-
// labels the leg) this test fails loudly — even when the strict-mode
// wire-smoke gate can't observe the fallback firing.
func TestBSIVFallback_AssemblesLegFromSyntheticPrice(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	const (
		spot    = 737.0
		strike  = 735.0
		sigmaTr = 0.14
	)
	// 7-DTE: snapshot today, expiry 7 days out. Expiry settlement is
	// at 16:00 NY per dteYears.
	now := time.Date(2026, 5, 18, 9, 30, 0, 0, loc)
	expiryYMD := now.AddDate(0, 0, 7).Format("20060102")

	cases := []struct {
		name   string
		right  string
		isCall bool
	}{
		{"call_7dte_atm", "C", true},
		{"put_7dte_atm", "P", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dte := dteYears(expiryYMD, now)
			callPx := bsCallPrice(spot, strike, dte, sigmaTr, 0, 0)
			price := callPx
			if !tc.isCall {
				price = callPx - spot + strike // r=q=0 parity
			}

			r := bsIVFallback(spot, now, expiryYMD, strike, tc.right, 123, price)

			if !r.OK || !r.IVDerived {
				t.Fatalf("expected OK=true IVDerived=true, got %+v", r)
			}
			if r.OI != 123 {
				t.Errorf("OI threaded through: got %d, want 123", r.OI)
			}
			if math.Abs(r.IV-sigmaTr) > 0.0005 {
				t.Errorf("σ recovery: got %.5f, want %.5f", r.IV, sigmaTr)
			}
			if r.Gamma <= 0 || math.IsNaN(r.Gamma) || math.IsInf(r.Gamma, 0) {
				t.Errorf("expected positive finite gamma, got %v", r.Gamma)
			}
		})
	}
}

// TestBSIVFallback_RefusalCases pins the empty-legResult exits — every
// path where Stage 2b drops a leg rather than poisoning the aggregate
// with a non-physical σ.
func TestBSIVFallback_RefusalCases(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 5, 18, 9, 30, 0, 0, loc)
	future := now.AddDate(0, 0, 7).Format("20060102")
	past := now.AddDate(0, 0, -1).Format("20060102")

	cases := []struct {
		name      string
		expiryYMD string
		price     float64
		why       string
	}{
		{"zero_price", future, 0, "no model tick AND no bid/ask AND no prior close"},
		{"negative_price", future, -1, "garbage price"},
		{"expired", past, 5.0, "DTE ≤ 0 (rollover during compute)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := bsIVFallback(737.0, now, tc.expiryYMD, 735.0, "P", 100, tc.price)
			if r.OK || r.IVDerived || r.OI != 0 || r.IV != 0 {
				t.Errorf("%s should return empty legResult, got %+v", tc.why, r)
			}
		})
	}
}

// TestCheckLegCoverage pins the F-21/F-25 persist-or-not gate: a
// fan-out whose leg-landing fraction falls below
// MinLegCoverageFraction returns an error so the cache layer's
// gammaErrorRetryTTL machinery applies, mirroring breadth's
// MinCoverageFraction guard. Boundary, throttle-attribution, and the
// defensive empty-jobs guard are all exercised.
func TestCheckLegCoverage(t *testing.T) {
	t.Parallel()

	// Above-threshold: clean run.
	if err := checkLegCoverage(50, 100, false); err != nil {
		t.Errorf("50%% should pass MinLegCoverageFraction (0.5), got error: %v", err)
	}
	if err := checkLegCoverage(900, 1000, true); err != nil {
		t.Errorf("90%% even with throttle observed should pass: %v", err)
	}

	// Exactly the threshold passes (boundary is inclusive on the pass
	// side — coverage >= MinLegCoverageFraction returns nil).
	if err := checkLegCoverage(500, 1000, false); err != nil {
		t.Errorf("exactly 50%% should pass the >= boundary, got: %v", err)
	}

	// Below threshold: error, names the shortfall.
	err := checkLegCoverage(49, 100, false)
	if err == nil {
		t.Fatal("49%% should fail MinLegCoverageFraction")
	}
	if msg := err.Error(); !strings.Contains(msg, "49/100") || !strings.Contains(msg, "below minimum") {
		t.Errorf("error message should name landed/total and 'below minimum': %q", msg)
	}

	// Throttle attribution: when the gateway throttled, the message
	// names it so the operator can act on the cause, not the symptom.
	err = checkLegCoverage(0, 100, true)
	if err == nil {
		t.Fatal("0%% should fail")
	}
	if !strings.Contains(err.Error(), "gateway throttled") {
		t.Errorf("throttled-attribution missing from message: %q", err.Error())
	}

	// Defensive empty-jobs guard: would normally be unreachable
	// (normalizeGammaParams prevents it) but the helper must not
	// emit a NaN-laden message.
	err = checkLegCoverage(0, 0, false)
	if err == nil {
		t.Fatal("empty jobs list should defensively error rather than divide by zero")
	}
	if strings.Contains(err.Error(), "NaN") {
		t.Errorf("empty-jobs message should not surface NaN: %q", err.Error())
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalFloatSlice(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
