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

// TestSelectExpirations pins the 0DTE-post-settlement filter at the
// per-trading-class NY-time cutoff plus the slot-anchored picker:
//
//	[front-week-1, front-week-2, EOW, next-monthly, next-quarterly, fill]
//
// Together they make sure (a) past-settlement same-day expiries fall
// out and (b) the basket reaches monthly + quarterly horizons rather
// than picking only weeklies inside ~2 weeks (which the lexicographic-N
// predecessor did — see docs/design/gamma-adaptive-strike-window.md).
func TestSelectExpirations(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	chain := map[string][]float64{
		"2026-05-15": {5000}, // yesterday
		"2026-05-16": {5000}, // today (Sat — synthetic)
		"2026-05-17": {5000}, // tomorrow (Sun)
		"2026-05-19": {5000}, // Tue next week
		"2026-05-26": {5000}, // Tue week+1
		"2026-06-19": {5000}, // 3rd Friday of June — monthly
		"2026-09-18": {5000}, // 3rd Friday of September — quarterly
		"2026-12-18": {5000}, // 3rd Friday of December — quarterly
	}

	t.Run("morning_today_included_with_monthly_anchor", func(t *testing.T) {
		// Sat 2026-05-16 10:00 ET → this week's Friday = 05-22 (NOT in
		// chain). EOW anchor falls through; monthly anchor lands on
		// 06-19 and quarterly on 09-18, leaving room for two front-week
		// slots.
		now := time.Date(2026, 5, 16, 10, 0, 0, 0, loc)
		got := selectExpirations(chain, "", now, 4)
		// Slot 1 = 05-16, Slot 2 = 05-17, Slot 4 (monthly) = 06-19,
		// Slot 5 (quarterly) = 09-18.
		want := []string{"2026-05-16", "2026-05-17", "2026-06-19", "2026-09-18"}
		if !equalSlice(got, want) {
			t.Errorf("morning slot basket: got %v, want %v", got, want)
		}
	})

	t.Run("post_settlement_today_excluded", func(t *testing.T) {
		// 17:00 ET on 2026-05-16 → today (Sat) excluded by the past-cutoff
		// rule even though it's pre-PM-settle for a Saturday listing
		// (16:15 ET buffer applied uniformly).
		now := time.Date(2026, 5, 16, 17, 0, 0, 0, loc)
		got := selectExpirations(chain, "", now, 4)
		// Slot 1 = 05-17, Slot 2 = 05-19, Slot 4 monthly = 06-19,
		// Slot 5 quarterly = 09-18.
		want := []string{"2026-05-17", "2026-05-19", "2026-06-19", "2026-09-18"}
		if !equalSlice(got, want) {
			t.Errorf("post-settlement: got %v, want %v", got, want)
		}
	})

	t.Run("yesterday_always_excluded", func(t *testing.T) {
		now := time.Date(2026, 5, 16, 10, 0, 0, 0, loc)
		got := selectExpirations(chain, "", now, 10)
		for _, d := range got {
			if d == "2026-05-15" {
				t.Errorf("selectExpirations included expired date 2026-05-15: got %v", got)
			}
		}
	})

	t.Run("count_caps_result", func(t *testing.T) {
		// Count=2 only fills slots 1 + 2 (front-week-1, front-week-2);
		// every later slot is skipped.
		now := time.Date(2026, 5, 16, 10, 0, 0, 0, loc)
		got := selectExpirations(chain, "", now, 2)
		want := []string{"2026-05-16", "2026-05-17"}
		if !equalSlice(got, want) {
			t.Errorf("count=2: got %v, want %v", got, want)
		}
	})

	// SPX-class third-Friday is AM-settled (09:30 ET cash-settle). At
	// 10:00 ET on the third Friday the SPX-class listing is already
	// past its settlement window (09:30 + 15-min buffer = 09:45), so
	// the SPX-class filter must exclude it. The SPXW-class filter on
	// the same date+time keeps the listing (PM-settle 16:00, still
	// hours away).
	//
	// Without the trading-class qualifier on selectExpirations, both
	// classes' third-Friday listings would inherit the same 16:15
	// cutoff and the SPX-AM book would be priced with ~6 hours of
	// nonexistent time-to-expiry.
	thirdFridayChain := map[string][]float64{
		"2026-06-19": {5400}, // third Friday — listed under both SPX (AM) and SPXW (PM)
		"2026-06-20": {5400}, // Saturday — synthetic; included to confirm count truncation works
		"2026-06-26": {5400}, // next week
	}
	t.Run("spx_class_third_friday_post_AM_settle_excluded", func(t *testing.T) {
		now := time.Date(2026, 6, 19, 10, 0, 0, 0, loc) // 10:00 ET — past 09:45 AM-settle buffer
		got := selectExpirations(thirdFridayChain, "SPX", now, 4)
		for _, d := range got {
			if d == "2026-06-19" {
				t.Errorf("SPX-class selectExpirations included AM-settled third Friday post-09:45: got %v", got)
			}
		}
	})
	t.Run("spxw_class_third_friday_pre_PM_settle_included", func(t *testing.T) {
		now := time.Date(2026, 6, 19, 10, 0, 0, 0, loc) // 10:00 ET — well before 16:15 PM-settle buffer
		got := selectExpirations(thirdFridayChain, "SPXW", now, 4)
		found := false
		for _, d := range got {
			if d == "2026-06-19" {
				found = true
			}
		}
		if !found {
			t.Errorf("SPXW-class selectExpirations dropped PM-settled third Friday pre-16:00: got %v", got)
		}
	})
	t.Run("spx_class_pre_AM_settle_morning_included", func(t *testing.T) {
		now := time.Date(2026, 6, 19, 8, 30, 0, 0, loc) // 08:30 ET — before 09:30 AM-settle
		got := selectExpirations(thirdFridayChain, "SPX", now, 4)
		found := false
		for _, d := range got {
			if d == "2026-06-19" {
				found = true
			}
		}
		if !found {
			t.Errorf("SPX-class selectExpirations dropped pre-AM-settle morning listing: got %v", got)
		}
	})
}

// TestPickExpirationSlots pins the slot-anchored picker against
// realistic SPY-like chains. The slot rule is:
//
//	[front-week-1, front-week-2, EOW, next-monthly, next-quarterly, fill]
//
// where unfilled anchored slots (e.g. quarterly listing not on chain)
// roll forward to the fill rule. Goals of this block:
//
//   - Confirm the basket reaches into the monthly and quarterly
//     horizons rather than staying inside ~2 weeks (the bug this PR
//     fixes).
//   - Lock the EOW collision behaviour (today is a Friday → EOW
//     candidate is "today", already picked as front-week-1, so the
//     slot rolls to fill).
//   - Confirm anchors degrade gracefully on chains missing a quarterly
//     listing.
func TestPickExpirationSlots(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}

	// A realistic SPY-style chain on 2026-05-22 (Friday): daily M/W/F
	// weeklies, the next monthly 3rd-Friday (Jun 19), and the next two
	// quarterly 3rd-Fridays (Sep 18, Dec 18).
	spyChain := []string{
		"2026-05-22", // Fri (today)
		"2026-05-25", // Mon
		"2026-05-27", // Wed
		"2026-05-29", // Fri
		"2026-06-01", // Mon
		"2026-06-03", // Wed
		"2026-06-05", // Fri
		"2026-06-08", // Mon
		"2026-06-10", // Wed
		"2026-06-12", // Fri
		"2026-06-15", // Mon
		"2026-06-17", // Wed
		"2026-06-19", // 3rd Friday Jun — monthly
		"2026-06-22", // Mon (post-OPEX)
		"2026-06-24", // Wed
		"2026-06-26", // Fri
		"2026-07-17", // 3rd Friday Jul — monthly
		"2026-08-21", // 3rd Friday Aug — monthly
		"2026-09-18", // 3rd Friday Sep — quarterly
		"2026-12-18", // 3rd Friday Dec — quarterly
	}

	t.Run("friday_today_anchors_to_monthly_and_quarterly", func(t *testing.T) {
		// 09:30 ET, 2026-05-22 — markets open, today's expiry still
		// pre-settle. Verifies the documented basket on a Friday: EOW
		// rolls to fill because today is its own EOW.
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(spyChain, now, 6)
		want := []string{
			"2026-05-22", // front-week-1 (today)
			"2026-05-25", // front-week-2
			"2026-05-27", // fill (EOW collided with front-week-1)
			"2026-05-29", // fill
			"2026-06-19", // monthly
			"2026-09-18", // quarterly
		}
		if !equalSlice(got, want) {
			t.Errorf("Fri 2026-05-22 basket: got %v, want %v", got, want)
		}
	})

	t.Run("wednesday_today_eow_fills_to_this_weeks_friday", func(t *testing.T) {
		// Cut the chain to a 2026-05-27 (Wed) view — today's expiry
		// (Wed) is front-week-1, next nearest is Fri 05-29, and the
		// EOW anchor is also Fri 05-29, so EOW collides with
		// front-week-2 and rolls to fill.
		now := time.Date(2026, 5, 27, 9, 30, 0, 0, loc)
		candidates := []string{
			"2026-05-27", "2026-05-29", "2026-06-01", "2026-06-03",
			"2026-06-05", "2026-06-19", "2026-09-18", "2026-12-18",
		}
		got := pickExpirationSlots(candidates, now, 6)
		want := []string{
			"2026-05-27", // front-week-1
			"2026-05-29", // front-week-2 (also EOW collision target)
			"2026-06-01", // fill (EOW rolled)
			"2026-06-03", // fill
			"2026-06-19", // monthly
			"2026-09-18", // quarterly
		}
		if !equalSlice(got, want) {
			t.Errorf("Wed 2026-05-27 basket: got %v, want %v", got, want)
		}
	})

	t.Run("tuesday_today_eow_picks_this_weeks_friday", func(t *testing.T) {
		// On a Tuesday this week's Friday is not picked as front-week,
		// so the EOW anchor lands on it cleanly.
		now := time.Date(2026, 5, 26, 9, 30, 0, 0, loc) // Tue
		candidates := []string{
			"2026-05-26", "2026-05-27", "2026-05-29", "2026-06-01",
			"2026-06-03", "2026-06-19", "2026-09-18", "2026-12-18",
		}
		got := pickExpirationSlots(candidates, now, 6)
		want := []string{
			"2026-05-26", // front-week-1 (today)
			"2026-05-27", // front-week-2
			"2026-05-29", // EOW (this Friday)
			"2026-06-01", // fill
			"2026-06-19", // monthly
			"2026-09-18", // quarterly
		}
		if !equalSlice(got, want) {
			t.Errorf("Tue 2026-05-26 basket: got %v, want %v", got, want)
		}
	})

	t.Run("monthly_and_quarterly_same_date_picks_distinct_quarter", func(t *testing.T) {
		// When today is itself a 3rd Friday of a quarterly month
		// (09-18 = quarterly), it's picked as front-week-1. The
		// monthly slot then rolls to the next 3rd Friday (10-16) and
		// the quarterly slot to Dec 18. EOW collides with today and
		// rolls to fill.
		now := time.Date(2026, 9, 18, 9, 30, 0, 0, loc)
		candidates := []string{
			"2026-09-18", // today (Fri, 3rd Fri Sep, quarterly)
			"2026-09-21", // Mon
			"2026-09-23", // Wed
			"2026-09-25", // Fri
			"2026-10-16", // 3rd Fri Oct (monthly)
			"2026-11-20", // 3rd Fri Nov (monthly)
			"2026-12-18", // 3rd Fri Dec (quarterly)
		}
		got := pickExpirationSlots(candidates, now, 6)
		// Output is sorted ascending. Picked: 09-18 (slot1), 09-21
		// (slot2), 10-16 (slot4 monthly), 12-18 (slot5 quarterly),
		// 09-23 + 09-25 (fills since EOW collided with today).
		want := []string{
			"2026-09-18", "2026-09-21", "2026-09-23", "2026-09-25",
			"2026-10-16", "2026-12-18",
		}
		if !equalSlice(got, want) {
			t.Errorf("Fri-quarterly basket: got %v, want %v", got, want)
		}
	})

	t.Run("missing_quarterly_rolls_to_fill", func(t *testing.T) {
		// Chain has weeklies + a monthly but no quarterly listing.
		// Quarterly slot rolls to fill so the basket still produces N
		// dates rather than 5.
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc) // Fri
		candidates := []string{
			"2026-05-22", "2026-05-25", "2026-05-27", "2026-05-29",
			"2026-06-01", "2026-06-19", // monthly only
		}
		got := pickExpirationSlots(candidates, now, 6)
		want := []string{
			"2026-05-22", "2026-05-25", "2026-05-27", "2026-05-29",
			"2026-06-01", "2026-06-19",
		}
		if !equalSlice(got, want) {
			t.Errorf("missing-quarterly basket: got %v, want %v", got, want)
		}
	})

	t.Run("count_2_only_front_weeks", func(t *testing.T) {
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(spyChain, now, 2)
		want := []string{"2026-05-22", "2026-05-25"}
		if !equalSlice(got, want) {
			t.Errorf("count=2: got %v, want %v", got, want)
		}
	})

	t.Run("count_4_prefers_anchored_slots_over_fill", func(t *testing.T) {
		// Count=4. The anchored slots (front-week-1, front-week-2,
		// monthly, quarterly) consume the full budget; EOW collides
		// with today (Friday) and rolls but the count is already met
		// by the monthly + quarterly anchors. No nearby weeklies make
		// the basket — that's the intent: at small N, prefer horizon
		// coverage over weeklies density.
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(spyChain, now, 4)
		want := []string{
			"2026-05-22", // front-week-1
			"2026-05-25", // front-week-2
			"2026-06-19", // monthly
			"2026-09-18", // quarterly
		}
		if !equalSlice(got, want) {
			t.Errorf("count=4 basket: got %v, want %v", got, want)
		}
	})

	t.Run("empty_candidates_returns_nil", func(t *testing.T) {
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(nil, now, 6)
		if got != nil {
			t.Errorf("nil candidates: got %v, want nil", got)
		}
	})

	t.Run("count_zero_returns_nil", func(t *testing.T) {
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(spyChain, now, 0)
		if got != nil {
			t.Errorf("count=0: got %v, want nil", got)
		}
	})
}

// TestThisWeekFriday pins the EOW-anchor helper: for any weekday, it
// returns the YYYY-MM-DD of the calendar Friday >= today.
func TestThisWeekFriday(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		// 2026-05-22 is Friday. Walk the surrounding week.
		{"sun", time.Date(2026, 5, 17, 12, 0, 0, 0, loc), "2026-05-22"},
		{"mon", time.Date(2026, 5, 18, 12, 0, 0, 0, loc), "2026-05-22"},
		{"tue", time.Date(2026, 5, 19, 12, 0, 0, 0, loc), "2026-05-22"},
		{"wed", time.Date(2026, 5, 20, 12, 0, 0, 0, loc), "2026-05-22"},
		{"thu", time.Date(2026, 5, 21, 12, 0, 0, 0, loc), "2026-05-22"},
		{"fri", time.Date(2026, 5, 22, 12, 0, 0, 0, loc), "2026-05-22"},
		{"sat", time.Date(2026, 5, 23, 12, 0, 0, 0, loc), "2026-05-29"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := thisWeekFriday(tc.now); got != tc.want {
				t.Errorf("thisWeekFriday(%s): got %q, want %q", tc.now.Format("Mon"), got, tc.want)
			}
		})
	}
}

// TestIsThirdFridayDate / TestIsQuarterlyThirdFridayDate pin the two
// predicates used by the monthly / quarterly slot anchors. 3rd Fridays
// fall on days 15-21 (the unique Friday in that span each month);
// quarterlies are 3rd Fridays of Mar / Jun / Sep / Dec.
func TestIsThirdFridayDate(t *testing.T) {
	cases := map[string]bool{
		"2026-05-22": false, // Friday but day 22, not 3rd Friday of May (3rd Fri = May 15)
		"2026-05-15": true,  // Fri, day 15 — 3rd Friday of May
		"2026-06-19": true,  // Fri, day 19 — 3rd Friday of June
		"2026-06-26": false, // Fri, day 26 — 4th Friday
		"2026-09-18": true,  // Fri, day 18 — 3rd Friday of Sep
		"2026-09-21": false, // Mon
		"":           false,
		"bogus":      false,
	}
	for in, want := range cases {
		if got := isThirdFridayDate(in); got != want {
			t.Errorf("isThirdFridayDate(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsQuarterlyThirdFridayDate(t *testing.T) {
	cases := map[string]bool{
		"2026-03-20": true,  // 3rd Fri Mar
		"2026-06-19": true,  // 3rd Fri Jun
		"2026-09-18": true,  // 3rd Fri Sep
		"2026-12-18": true,  // 3rd Fri Dec
		"2026-05-15": false, // 3rd Fri May (not quarterly)
		"2026-07-17": false, // 3rd Fri Jul (not quarterly)
		"2026-06-26": false, // 4th Fri Jun
		"2026-06-22": false, // Mon
	}
	for in, want := range cases {
		if got := isQuarterlyThirdFridayDate(in); got != want {
			t.Errorf("isQuarterlyThirdFridayDate(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestDTEYearsSPXClassUsesAMInstant pins the trading-class branch on
// dteYears. SPX-class third-Friday options settle at 09:30 ET; at 10:00
// ET on expiry day they are PAST settlement (dte=0), not 6 hours away
// (which the legacy 16:00 PM-settle instant would say). The aggregate
// gamma error from a 6-hour TTE mis-attribution on third-Friday SPX is
// dollar-significant — SPX dealer gamma concentrates on expiring books.
func TestDTEYearsSPXClassUsesAMInstant(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	// 10:00 ET on the third Friday — past 09:30 SPX-class settle.
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, loc)

	// SPX class → AM-settle → already expired.
	if y := dteYears("20260619", "SPX", now); y != 0 {
		t.Errorf("SPX-class dteYears post-AM-settle: got %v years, want 0 (already cash-settled at 09:30)", y)
	}
	// SPXW class → PM-settle → still ~6 hours of TTE.
	y := dteYears("20260619", "SPXW", now)
	if y <= 0 || y > 0.001 {
		t.Errorf("SPXW-class dteYears pre-PM-settle: got %v years, want 0 < y < 0.001 (~6h)", y)
	}
	// Empty class falls back to PM-settle convention (today's SPY behaviour).
	if dteYears("20260619", "", now) <= 0 {
		t.Errorf("empty-class dteYears must match SPXW (PM-settle) for back-compat")
	}
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

	if y := dteYears("20260516", "", now); y <= 0 || y > 0.01 {
		// 2h to 16:00 ≈ 2 / (24·365) ≈ 0.000228; window [0, 0.01]
		t.Errorf("2h to expiry: got %v, want in (0, 0.01)", y)
	}
	// Roughly 33 days × 24h / (24·365) ≈ 0.0904 years
	y := dteYears("20260619", "", now)
	if y < 0.080 || y > 0.105 {
		t.Errorf("~33 days: got %v, want [0.080, 0.105]", y)
	}

	if y := dteYears("20260515", "", now); y != 0 {
		t.Errorf("past date: got %v, want 0", y)
	}
	if y := dteYears("bogus", "", now); y != 0 {
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
	profile := sweepProfile(legs, 5000, 0.15, nil)
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
	empty := sweepProfile(nil, 5000, 0.15, nil)
	if len(empty) != sweepPoints {
		t.Errorf("empty legs len = %d, want %d", len(empty), sweepPoints)
	}
	for i, p := range empty {
		if p.GEX != 0 {
			t.Errorf("empty legs profile[%d].GEX = %v, want exactly 0", i, p.GEX)
			break
		}
	}
	if got := sweepProfile(legs, 0, 0.15, nil); got != nil {
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
	top := rankTopStrikesByAbsGEX(legs, 5000, 5, "TEST")

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
	if got := rankTopStrikesByAbsGEX(legs, 5000, 0, "TEST"); got != nil {
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
			dte := dteYears(expiryYMD, "", now)
			callPx := bsCallPrice(spot, strike, dte, sigmaTr, 0, 0)
			price := callPx
			if !tc.isCall {
				price = callPx - spot + strike // r=q=0 parity
			}

			r := bsIVFallback(spot, now, expiryYMD, "", strike, tc.right, 123, price)

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
			r := bsIVFallback(737.0, now, tc.expiryYMD, "", 735.0, "P", 100, tc.price)
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
		t.Errorf("50%% should pass MinLegCoverageFraction (0.2), got error: %v", err)
	}
	if err := checkLegCoverage(900, 1000, true); err != nil {
		t.Errorf("90%% even with throttle observed should pass: %v", err)
	}

	// Exactly the threshold passes (boundary is inclusive on the pass
	// side — coverage >= MinLegCoverageFraction returns nil).
	if err := checkLegCoverage(200, 1000, false); err != nil {
		t.Errorf("exactly 20%% should pass the >= boundary, got: %v", err)
	}

	// Below threshold: error, names the shortfall.
	err := checkLegCoverage(19, 100, false)
	if err == nil {
		t.Fatal("19%% should fail MinLegCoverageFraction")
	}
	if msg := err.Error(); !strings.Contains(msg, "19/100") || !strings.Contains(msg, "below minimum") {
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
