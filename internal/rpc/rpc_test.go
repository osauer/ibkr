package rpc

import (
	"testing"
	"time"
)

func TestIsOptionRTH(t *testing.T) {
	t.Parallel()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("America/New_York unavailable: %v", err)
	}

	cases := []struct {
		name string
		when time.Time
		want bool
	}{
		// Standard weekday window
		{"weekday_open_edge", time.Date(2026, 5, 21, 9, 30, 0, 0, ny), true},
		{"weekday_midday", time.Date(2026, 5, 21, 12, 0, 0, 0, ny), true},
		{"weekday_pre_close", time.Date(2026, 5, 21, 15, 59, 59, 0, ny), true},
		{"weekday_close_edge", time.Date(2026, 5, 21, 16, 0, 0, 0, ny), false},
		// Off-hours weekdays
		{"weekday_premarket", time.Date(2026, 5, 21, 7, 0, 0, 0, ny), false},
		{"weekday_just_before_open", time.Date(2026, 5, 21, 9, 29, 59, 0, ny), false},
		{"weekday_after_close", time.Date(2026, 5, 21, 16, 30, 0, 0, ny), false},
		// Weekend
		{"saturday_midday", time.Date(2026, 5, 23, 12, 0, 0, 0, ny), false},
		{"sunday_midday", time.Date(2026, 5, 24, 12, 0, 0, 0, ny), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOptionRTH(tc.when); got != tc.want {
				t.Errorf("IsOptionRTH(%s) = %v, want %v", tc.when.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

func TestIsOptionRTH_AcceptsUTC(t *testing.T) {
	t.Parallel()
	// 13:30 UTC on a weekday is 09:30 ET (during DST). Pass UTC explicitly
	// to pin that the helper converts to NY zone before evaluating.
	utc := time.Date(2026, 5, 21, 13, 30, 0, 0, time.UTC)
	if !IsOptionRTH(utc) {
		t.Errorf("13:30 UTC on a weekday should map to 09:30 ET and be open")
	}
}

func TestStripGammaProfilesRecursive(t *testing.T) {
	t.Parallel()
	res := &GammaZeroSPXResult{
		Status: GammaZeroStatusReady,
		Result: &GammaZeroComputed{
			Scope: GammaZeroScopeCombined,
			Profile: []GammaProfilePoint{
				{Spot: 100, GEX: 1},
			},
			Profile0DTE: []GammaProfilePoint{
				{Spot: 100, GEX: 2},
			},
			Profile1to7: []GammaProfilePoint{
				{Spot: 100, GEX: 3},
			},
			ProfileTerm: []GammaProfilePoint{
				{Spot: 100, GEX: 4},
			},
			Summary: &GammaZeroSummary{PrimaryStatement: "keep me"},
			PerIndex: map[string]*GammaZeroComputed{
				"SPY": {
					Scope: GammaZeroScopeSPY,
					Profile: []GammaProfilePoint{
						{Spot: 100, GEX: 5},
					},
				},
			},
		},
	}
	StripGammaProfiles(res)
	if len(res.Result.Profile) != 0 || len(res.Result.Profile0DTE) != 0 ||
		len(res.Result.Profile1to7) != 0 || len(res.Result.ProfileTerm) != 0 {
		t.Fatalf("top-level profiles were not stripped: %+v", res.Result)
	}
	if len(res.Result.PerIndex["SPY"].Profile) != 0 {
		t.Fatalf("per-index profile was not stripped: %+v", res.Result.PerIndex["SPY"].Profile)
	}
	if res.Result.Summary == nil || res.Result.Summary.PrimaryStatement != "keep me" {
		t.Fatalf("summary should survive profile stripping: %+v", res.Result.Summary)
	}
}

func TestStripRegimeGammaProfiles(t *testing.T) {
	t.Parallel()
	res := &RegimeSnapshotResult{
		GammaZero: RegimeGammaZero{
			Envelope: GammaZeroSPXResult{
				Status: GammaZeroStatusReady,
				Result: &GammaZeroComputed{
					Profile: []GammaProfilePoint{{Spot: 100, GEX: 1}},
					PerIndex: map[string]*GammaZeroComputed{
						"SPY": {Profile0DTE: []GammaProfilePoint{{Spot: 100, GEX: 2}}},
					},
				},
			},
		},
	}
	StripRegimeGammaProfiles(res)
	if len(res.GammaZero.Envelope.Result.Profile) != 0 {
		t.Fatalf("regime gamma profile was not stripped")
	}
	if len(res.GammaZero.Envelope.Result.PerIndex["SPY"].Profile0DTE) != 0 {
		t.Fatalf("regime per-index profile was not stripped")
	}
}
