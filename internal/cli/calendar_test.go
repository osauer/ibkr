package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestRenderCalendarTextShowsHolidayNextOpenAndCoverage(t *testing.T) {
	t.Parallel()
	loc := mustLoadLocation(t, "America/New_York")
	nextOpen := time.Date(2026, 5, 26, 9, 30, 0, 0, loc)
	nextClose := time.Date(2026, 5, 26, 16, 0, 0, 0, loc)
	res := &rpc.MarketCalendarResult{
		Market:        "us_equity",
		Label:         "US equities",
		Timezone:      "America/New_York",
		CoverageStart: "2026-01-01",
		CoverageEnd:   "2028-12-31",
		Session: rpc.MarketSession{
			Market:   "us_equity",
			Label:    "US equities",
			Date:     "2026-05-25",
			Timezone: "America/New_York",
			State:    "holiday",
			Reason:   "Memorial Day",
			NextOpen: &nextOpen,
		},
		Sessions: []rpc.MarketSession{
			{Date: "2026-05-25", State: "holiday", Reason: "Memorial Day"},
			{Date: "2026-05-26", State: "regular", Open: nextOpen, Close: nextClose},
		},
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderCalendarText(env, res); code != 0 {
		t.Fatalf("renderCalendarText code = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"US equities calendar",
		"Session:       2026-05-25  holiday",
		"Reason:        Memorial Day",
		"Next open:     2026-05-26 09:30 EDT",
		"Coverage:      2026-01-01 to 2028-12-31",
		"2026-05-26",
		"09:30-16:00 EDT",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("calendar output missing %q:\n%s", want, out)
		}
	}
}

func TestQuoteSessionHintSummarizesCalendarContext(t *testing.T) {
	t.Parallel()
	loc := mustLoadLocation(t, "Europe/Berlin")
	nextOpen := time.Date(2026, 12, 28, 9, 0, 0, 0, loc)
	env := &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}

	got := quoteSessionHint(env, &rpc.MarketSession{
		Label:    "Xetra",
		State:    "holiday",
		Reason:   "Christmas Day",
		NextOpen: &nextOpen,
	})

	for _, want := range []string{"Xetra", "holiday", "Christmas Day", "next open 2026-12-28 09:00 CET"} {
		if !strings.Contains(got, want) {
			t.Fatalf("quoteSessionHint missing %q in %q", want, got)
		}
	}
}

func TestMarketSessionHoursUsesResponseTimezoneName(t *testing.T) {
	t.Parallel()
	open, err := time.Parse(time.RFC3339, "2026-11-27T09:30:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	closeT, err := time.Parse(time.RFC3339, "2026-11-27T13:00:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	got := marketSessionHours(rpc.MarketSession{
		Timezone: "America/New_York",
		Open:     open,
		Close:    closeT,
	})
	if got != "09:30-13:00 EST" {
		t.Fatalf("marketSessionHours = %q, want EST zone name", got)
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}
