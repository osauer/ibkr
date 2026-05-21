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
