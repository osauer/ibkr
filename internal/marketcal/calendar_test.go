package marketcal

import (
	"testing"
	"time"
)

func TestUSEquityMemorialDay2026(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "America/New_York")
	cal := NewWithClock(func() time.Time { return time.Date(2026, 5, 25, 10, 0, 0, 0, loc) })

	res, err := cal.Query(Query{Market: MarketUSEquity, Date: "2026-05-25", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session.State != StateHoliday {
		t.Fatalf("state = %s, want holiday", res.Session.State)
	}
	if res.Session.Reason != "Memorial Day" {
		t.Fatalf("reason = %q, want Memorial Day", res.Session.Reason)
	}
	if res.Session.NextOpen == nil || res.Session.NextOpen.Format(time.RFC3339) != "2026-05-26T09:30:00-04:00" {
		t.Fatalf("next_open = %v, want 2026-05-26 09:30 ET", res.Session.NextOpen)
	}
}

func TestDEXetraWhitMonday2026IsOpen(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "Europe/Berlin")
	cal := NewWithClock(func() time.Time { return time.Date(2026, 5, 25, 12, 0, 0, 0, loc) })

	res, err := cal.Query(Query{Market: MarketDEXetra, Date: "2026-05-25", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session.State != StateRegular || !res.Session.IsOpen {
		t.Fatalf("session = %+v, want regular open", res.Session)
	}
	if got := res.Session.Open.Format("15:04 MST"); got != "09:00 CEST" {
		t.Fatalf("open = %s, want 09:00 CEST", got)
	}
	if got := res.Session.Close.Format("15:04 MST"); got != "17:30 CEST" {
		t.Fatalf("close = %s, want 17:30 CEST", got)
	}
}

func TestUSEquityEarlyCloses2026(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "America/New_York")
	cal := NewWithClock(func() time.Time { return time.Date(2026, 11, 27, 12, 0, 0, 0, loc) })

	for _, date := range []string{"2026-11-27", "2026-12-24"} {
		res, err := cal.Query(Query{Market: MarketUSEquity, Date: date, Days: 1})
		if err != nil {
			t.Fatal(err)
		}
		if res.Session.State != StateEarlyClose {
			t.Fatalf("%s state = %s, want early_close", date, res.Session.State)
		}
		if got := res.Session.Close.Format("15:04 MST"); got != "13:00 EST" {
			t.Fatalf("%s close = %s, want 13:00 EST", date, got)
		}
	}
}

func TestDEXetraYearEndClosures2026(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "Europe/Berlin")
	cal := NewWithClock(func() time.Time { return time.Date(2026, 12, 24, 12, 0, 0, 0, loc) })

	for _, date := range []string{"2026-12-24", "2026-12-25", "2026-12-31"} {
		res, err := cal.Query(Query{Market: MarketDEXetra, Date: date, Days: 1})
		if err != nil {
			t.Fatal(err)
		}
		if res.Session.State != StateHoliday {
			t.Fatalf("%s state = %s, want holiday", date, res.Session.State)
		}
	}
}

func TestSessionOpenInclusiveCloseExclusive(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "America/New_York")
	cal := NewWithClock(func() time.Time { return time.Date(2026, 5, 26, 9, 30, 0, 0, loc) })

	open, err := cal.SessionAt(MarketUSEquity, time.Date(2026, 5, 26, 9, 30, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if !open.IsOpen {
		t.Fatalf("open boundary should be inclusive: %+v", open)
	}
	closeT, err := cal.SessionAt(MarketUSEquity, time.Date(2026, 5, 26, 16, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if closeT.IsOpen {
		t.Fatalf("close boundary should be exclusive: %+v", closeT)
	}
	if closeT.NextOpen == nil || closeT.NextOpen.Format(time.RFC3339) != "2026-05-27T09:30:00-04:00" {
		t.Fatalf("close boundary next_open = %v, want next day 09:30 ET", closeT.NextOpen)
	}
}

func TestOvernightNextOpenIsSameDay(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "America/New_York")
	cal := NewWithClock(func() time.Time { return time.Date(2026, 5, 26, 1, 0, 0, 0, loc) })

	s, err := cal.SessionAt(MarketUSEquity, time.Date(2026, 5, 26, 1, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if s.IsOpen {
		t.Fatalf("1am should not be open: %+v", s)
	}
	if s.NextOpen == nil || s.NextOpen.Format(time.RFC3339) != "2026-05-26T09:30:00-04:00" {
		t.Fatalf("next_open = %v, want same-day 09:30 ET", s.NextOpen)
	}
}

func TestDSTLocalHoursStayStable(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	berlin := mustLoc(t, "Europe/Berlin")
	cal := NewWithClock(func() time.Time { return time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC) })

	us, err := cal.Query(Query{Market: MarketUSEquity, Date: "2026-03-20", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	de, err := cal.Query(Query{Market: MarketDEXetra, Date: "2026-03-20", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := us.Session.Open.In(ny).Format("15:04 MST"); got != "09:30 EDT" {
		t.Fatalf("US open = %s, want 09:30 EDT", got)
	}
	if got := de.Session.Open.In(berlin).Format("15:04 MST"); got != "09:00 CET" {
		t.Fatalf("DE open = %s, want 09:00 CET", got)
	}
}

func TestOutsideCoverageUnknown(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "America/New_York")
	cal := NewWithClock(func() time.Time { return time.Date(2026, 5, 25, 12, 0, 0, 0, loc) })

	res, err := cal.Query(Query{Market: MarketUSEquity, Date: "2029-01-02", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session.State != StateUnknown {
		t.Fatalf("outside coverage state = %s, want unknown", res.Session.State)
	}
}

func TestUSOptionsRegularSession(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "America/New_York")
	cal := NewWithClock(func() time.Time { return time.Date(2026, 5, 26, 16, 10, 0, 0, loc) })

	s, err := cal.SessionAt(MarketUSOptions, time.Date(2026, 5, 26, 16, 10, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsOpen {
		t.Fatalf("US options should still be open at 16:10 ET under v1 regular options calendar: %+v", s)
	}
	if got := s.Close.Format("15:04 MST"); got != "16:15 EDT" {
		t.Fatalf("close = %s, want 16:15 EDT", got)
	}
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
