package daemon

import (
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// TestStreakStore_FirstCall starts the counter at 1 with today's session.
func TestStreakStore_FirstCall(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	now := mustParseNY(t, "2026-05-20 10:00 EST")
	info := s.Tick(StreakKeyVIXTerm, 0.85, "green", now)
	if info == nil {
		t.Fatal("nil info from first Tick")
	}
	if info.Sessions != 1 || info.Band != "green" || info.Since != "2026-05-20" {
		t.Errorf("got Sessions=%d Band=%q Since=%q, want 1/green/2026-05-20",
			info.Sessions, info.Band, info.Since)
	}
}

// TestStreakStore_SameSessionNoIncrement: multiple calls on the same NY
// session with the same band leave Sessions at 1.
func TestStreakStore_SameSessionNoIncrement(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	now := mustParseNY(t, "2026-05-20 10:00 EST")
	s.Tick(StreakKeyVIXTerm, 0.85, "green", now)
	later := mustParseNY(t, "2026-05-20 14:00 EST")
	info := s.Tick(StreakKeyVIXTerm, 0.86, "green", later)
	if info.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1 (same session, no increment)", info.Sessions)
	}
}

// TestStreakStore_NextSessionIncrements: a same-band call on a later NY
// session ticks Sessions up by 1.
func TestStreakStore_NextSessionIncrements(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	day1 := mustParseNY(t, "2026-05-20 10:00 EST")
	day2 := mustParseNY(t, "2026-05-21 10:00 EST")
	s.Tick(StreakKeyVIXTerm, 0.85, "green", day1)
	info := s.Tick(StreakKeyVIXTerm, 0.86, "green", day2)
	if info.Sessions != 2 || info.Since != "2026-05-20" {
		t.Errorf("got Sessions=%d Since=%q, want 2/2026-05-20", info.Sessions, info.Since)
	}
}

// TestStreakStore_BandChangeResets: a different band on a later day
// resets Sessions to 1 and Since to today.
func TestStreakStore_BandChangeResets(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	day1 := mustParseNY(t, "2026-05-20 10:00 EST")
	day2 := mustParseNY(t, "2026-05-21 10:00 EST")
	s.Tick(StreakKeyVIXTerm, 0.85, "green", day1)
	info := s.Tick(StreakKeyVIXTerm, 0.95, "yellow", day2)
	if info.Sessions != 1 || info.Band != "yellow" || info.Since != "2026-05-21" {
		t.Errorf("got Sessions=%d Band=%q Since=%q, want 1/yellow/2026-05-21",
			info.Sessions, info.Band, info.Since)
	}
}

// TestStreakStore_EmptyBandFreezes: an unavailable indicator (empty band)
// returns the previous state without mutating the counter.
func TestStreakStore_EmptyBandFreezes(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	day1 := mustParseNY(t, "2026-05-20 10:00 EST")
	day2 := mustParseNY(t, "2026-05-21 10:00 EST")
	s.Tick(StreakKeyVIXTerm, 0.85, "green", day1)
	info := s.Tick(StreakKeyVIXTerm, 0, "", day2)
	if info == nil || info.Sessions != 1 || info.Band != "green" {
		t.Errorf("freeze returned %+v, want green sessions=1", info)
	}
	// Now a real tick on day 2 should still see day1 as Since.
	info = s.Tick(StreakKeyVIXTerm, 0.86, "green", day2)
	if info.Sessions != 2 {
		t.Errorf("after freeze + real tick, Sessions = %d, want 2", info.Sessions)
	}
}

func TestPopulateStreaksDoesNotAttachFrozenStreakToUnrankedRow(t *testing.T) {
	store := NewStreakStore(t.TempDir())
	now := mustParseNY(t, "2026-05-20 10:00 EST")
	store.Tick(StreakKeyVIXTerm, 0.85, "green", now)

	srv := &Server{streaks: store}
	res := &rpc.RegimeSnapshotResult{
		VIXTermStructure: rpc.RegimeVIXTerm{Status: rpc.RegimeStatusError, ErrorMessage: "no tick"},
	}
	srv.populateStreaks(res)
	if res.VIXTermStructure.Streak != nil {
		t.Fatalf("unranked VIX row should not expose frozen prior streak, got %+v", res.VIXTermStructure.Streak)
	}
	if got := store.Get(StreakKeyVIXTerm); got == nil || got.Band != "green" {
		t.Fatalf("store should still retain the frozen streak internally, got %+v", got)
	}
}

// TestStreakStore_PersistAcrossInstances: a store written by one instance
// should be loaded by a fresh instance pointed at the same dir.
func TestStreakStore_PersistAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s1 := NewStreakStore(dir)
	day1 := mustParseNY(t, "2026-05-20 10:00 EST")
	day2 := mustParseNY(t, "2026-05-21 10:00 EST")
	s1.Tick(StreakKeyVIXTerm, 0.85, "green", day1)
	s1.Tick(StreakKeyVIXTerm, 0.86, "green", day2)

	s2 := NewStreakStore(dir)
	info := s2.Get(StreakKeyVIXTerm)
	if info == nil || info.Sessions != 2 || info.Since != "2026-05-20" {
		t.Errorf("reload got %+v, want sessions=2 since=2026-05-20", info)
	}
}

// TestClassifyBands sanity-checks each classifier against the spec.
func TestClassifyBands(t *testing.T) {
	mkPtr := func(v float64) *float64 { return &v }

	t.Run("vix term", func(t *testing.T) {
		cases := []struct {
			ratio float64
			want  string
		}{
			{0.85, "green"},
			{0.92, "yellow"},
			{0.95, "yellow"},
			{1.00, "red"},
			{1.20, "red"},
		}
		for _, c := range cases {
			if got := classifyVIXTermBand(mkPtr(c.ratio)); got != c.want {
				t.Errorf("classifyVIXTermBand(%v) = %q, want %q", c.ratio, got, c.want)
			}
		}
		if got := classifyVIXTermBand(nil); got != "" {
			t.Errorf("nil ratio returned %q, want empty (freeze)", got)
		}
	})

	t.Run("usdjpy", func(t *testing.T) {
		cases := []struct {
			weeklyPct float64
			want      string
		}{
			{0.0, "green"},
			{0.5, "green"},  // yen weakening — green
			{-0.5, "green"}, // yen weakening little — green
			{-1.0, "yellow"},
			{-1.5, "yellow"},
			{-2.0, "red"},
			{-3.5, "red"},
		}
		for _, c := range cases {
			if got := classifyUSDJPYBand(mkPtr(c.weeklyPct)); got != c.want {
				t.Errorf("classifyUSDJPYBand(%v) = %q, want %q", c.weeklyPct, got, c.want)
			}
		}
	})

	t.Run("hyg spy", func(t *testing.T) {
		hyg := 79.0
		hyg50 := 80.0
		spy := 737.0
		nearHigh := 749.0
		farHigh := 780.0
		if got := classifyHYGSPYBand(rpc.RegimeHYGSPYDivergence{
			HYGPrice: &hyg, HYG50DMA: &hyg50, SPYPrice: &spy, SPY52WHigh: &nearHigh,
		}); got != "red" {
			t.Errorf("HYG below 50dma + SPY near highs = %q, want red", got)
		}
		if got := classifyHYGSPYBand(rpc.RegimeHYGSPYDivergence{
			HYGPrice: &hyg, HYG50DMA: &hyg50, SPYPrice: &spy, SPY52WHigh: &farHigh,
		}); got != "yellow" {
			t.Errorf("HYG below 50dma away from highs = %q, want yellow", got)
		}
	})

	t.Run("gamma", func(t *testing.T) {
		// With a crossing, band on gap_pct.
		cases := []struct {
			gap  float64
			want string
		}{
			{3.0, "green"},
			{2.5, "green"},
			{1.0, "yellow"},
			{-1.5, "yellow"},
			{-2.5, "red"},
		}
		for _, c := range cases {
			if got := classifyGammaBand(mkPtr(c.gap), ""); got != c.want {
				t.Errorf("classifyGammaBand gap=%v = %q, want %q", c.gap, got, c.want)
			}
		}
		// Without a crossing, band on sign.
		if got := classifyGammaBand(nil, "positive"); got != "green" {
			t.Errorf("no crossing + positive = %q, want green", got)
		}
		if got := classifyGammaBand(nil, "negative"); got != "red" {
			t.Errorf("no crossing + negative = %q, want red", got)
		}
		if got := classifyGammaBand(nil, "no_data"); got != "" {
			t.Errorf("no crossing + no_data = %q, want empty", got)
		}
		combined := &rpc.GammaZeroComputed{
			Scope:   rpc.GammaZeroScopeCombined,
			Quality: rankableGammaQuality(),
			PerIndex: map[string]*rpc.GammaZeroComputed{
				"SPY": {Scope: rpc.GammaZeroScopeSPY, GammaSign: "positive", Quality: rankableGammaQuality()},
				"SPX": {Scope: rpc.GammaZeroScopeSPX, GammaSign: "negative", Quality: rankableGammaQuality()},
			},
		}
		if got := classifyGammaComputedBand(combined); got != "red" {
			t.Errorf("SPX-dominant mixed combined gamma bands = %q, want red", got)
		}
	})

	t.Run("breadth", func(t *testing.T) {
		cases := []struct {
			v    float64
			want string
		}{
			{20, "red"},
			{39.9, "red"},
			{40, "yellow"},
			{50, "yellow"},
			{55, "yellow"},
			{55.1, "green"},
			{75, "green"},
		}
		for _, c := range cases {
			if got := classifyBreadthBand(c.v); got != c.want {
				t.Errorf("classifyBreadthBand(%v) = %q, want %q", c.v, got, c.want)
			}
		}
	})
}

func TestGammaStreaksRequireExplicitRankableQuality(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		quality *rpc.GammaSignalQuality
	}{
		{name: "nil_quality"},
		{name: "blocked_quality", quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gap := 5.0
			res := &rpc.RegimeSnapshotResult{
				GammaZero: rpc.RegimeGammaZero{
					Status: rpc.RegimeStatusOK,
					Envelope: rpc.GammaZeroSPXResult{
						Status: rpc.GammaZeroStatusReady,
						Result: &rpc.GammaZeroComputed{
							ZeroGamma: new(580.0),
							GapPct:    &gap,
							Quality:   tc.quality,
						},
					},
				},
			}

			band, _ := gammaZeroStreaks{}.bandAndValue(res)
			if band != "" {
				t.Fatalf("bandAndValue band = %q, want frozen/unranked", band)
			}
		})
	}
}

func mustParseNY(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load NY tz: %v", err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04 MST", s, loc)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}
