package spx

import (
	"testing"
	"time"
)

// flatWindow returns a ConstituentWindow whose 50 closes are all at
// the same level — convenient for asserting the boundary condition
// "today's close == SMA" (counts as above, per the >= convention).
func flatWindow(sym string, level float64) ConstituentWindow {
	closes := make([]float64, WindowSize)
	for i := range closes {
		closes[i] = level
	}
	return ConstituentWindow{Symbol: sym, Closes: closes, LastBarAt: "2026-05-16"}
}

// risingWindow constructs a window where every close is a step higher
// than the previous: today's close is well above the SMA.
func risingWindow(sym string, start, step float64) ConstituentWindow {
	closes := make([]float64, WindowSize)
	for i := range closes {
		closes[i] = start + float64(i)*step
	}
	return ConstituentWindow{Symbol: sym, Closes: closes, LastBarAt: "2026-05-16"}
}

// fallingWindow is risingWindow's mirror — today's close is the lowest
// in the window, well below the SMA.
func fallingWindow(sym string, start, step float64) ConstituentWindow {
	closes := make([]float64, WindowSize)
	for i := range closes {
		closes[i] = start - float64(i)*step
	}
	return ConstituentWindow{Symbol: sym, Closes: closes, LastBarAt: "2026-05-16"}
}

func TestComputeAllAbove(t *testing.T) {
	members := []string{"A", "B", "C", "D"}
	windows := map[string]ConstituentWindow{
		"A": risingWindow("A", 100, 1),
		"B": risingWindow("B", 50, 0.5),
		"C": risingWindow("C", 200, 2),
		"D": risingWindow("D", 75, 0.1),
	}
	snap := Compute(members, windows, "2026-05-16", time.Now())
	if snap.Value != 100.0 {
		t.Errorf("all-rising windows: want 100, got %v", snap.Value)
	}
	if snap.Coverage != 4 {
		t.Errorf("coverage: want 4, got %d", snap.Coverage)
	}
	if len(snap.Excluded) != 0 {
		t.Errorf("excluded: want empty, got %v", snap.Excluded)
	}
}

func TestComputeAllBelow(t *testing.T) {
	members := []string{"A", "B"}
	windows := map[string]ConstituentWindow{
		"A": fallingWindow("A", 100, 1),
		"B": fallingWindow("B", 200, 2),
	}
	snap := Compute(members, windows, "2026-05-16", time.Now())
	if snap.Value != 0.0 {
		t.Errorf("all-falling windows: want 0, got %v", snap.Value)
	}
	if snap.Coverage != 2 {
		t.Errorf("coverage: want 2, got %d", snap.Coverage)
	}
}

// TestComputeFlatBoundary pins the >= convention. A flat window
// (every close equal) has its tail close == SMA. We count this as
// "above" — matches $SPXA50R / S&P DJI methodology, which uses
// "close ≥ SMA" not "close > SMA".
func TestComputeFlatBoundary(t *testing.T) {
	members := []string{"A"}
	windows := map[string]ConstituentWindow{
		"A": flatWindow("A", 100),
	}
	snap := Compute(members, windows, "2026-05-16", time.Now())
	if snap.Value != 100.0 {
		t.Errorf("close == SMA must count as above (>=); got %v", snap.Value)
	}
}

func TestComputeMixed(t *testing.T) {
	members := []string{"A", "B", "C", "D"}
	windows := map[string]ConstituentWindow{
		"A": risingWindow("A", 100, 1),  // above
		"B": fallingWindow("B", 100, 1), // below
		"C": risingWindow("C", 50, 0.5), // above
		"D": fallingWindow("D", 80, 2),  // below
	}
	snap := Compute(members, windows, "2026-05-16", time.Now())
	if snap.Value != 50.0 {
		t.Errorf("2 of 4 above: want 50.0, got %v", snap.Value)
	}
}

func TestComputeNoWindow(t *testing.T) {
	members := []string{"A", "B", "MISSING"}
	windows := map[string]ConstituentWindow{
		"A": risingWindow("A", 100, 1),
		"B": risingWindow("B", 50, 1),
	}
	snap := Compute(members, windows, "2026-05-16", time.Now())
	if snap.Coverage != 2 {
		t.Errorf("coverage: want 2, got %d", snap.Coverage)
	}
	if snap.Value != 100.0 {
		t.Errorf("value computed from coverage (not MemberCount): want 100, got %v", snap.Value)
	}
	if len(snap.Excluded) != 1 || snap.Excluded[0].Symbol != "MISSING" {
		t.Errorf("excluded: want [MISSING/no_window], got %v", snap.Excluded)
	}
	if snap.Excluded[0].Reason != "no_window" {
		t.Errorf("reason: want no_window, got %q", snap.Excluded[0].Reason)
	}
}

func TestComputeThinHistory(t *testing.T) {
	members := []string{"A", "NEW"}
	thin := ConstituentWindow{
		Symbol:    "NEW",
		Closes:    []float64{50, 51, 52}, // only 3 closes
		LastBarAt: "2026-05-16",
	}
	windows := map[string]ConstituentWindow{
		"A":   risingWindow("A", 100, 1),
		"NEW": thin,
	}
	snap := Compute(members, windows, "2026-05-16", time.Now())
	if snap.Coverage != 1 {
		t.Errorf("coverage: want 1 (NEW excluded), got %d", snap.Coverage)
	}
	if snap.Value != 100.0 {
		t.Errorf("value: want 100 (A above, NEW excluded), got %v", snap.Value)
	}
	if len(snap.Excluded) != 1 || snap.Excluded[0].Symbol != "NEW" {
		t.Errorf("excluded: want [NEW/thin_history], got %v", snap.Excluded)
	}
	if snap.Excluded[0].Reason != "thin_history(3)" {
		t.Errorf("reason: want thin_history(3), got %q", snap.Excluded[0].Reason)
	}
}

func TestComputeExcludesPriorSessionWindows(t *testing.T) {
	members := []string{"CURRENT", "PRIOR"}
	current := risingWindow("CURRENT", 100, 1)
	current.LastBarAt = "2026-05-18"
	prior := risingWindow("PRIOR", 100, 1)
	prior.LastBarAt = "2026-05-15"

	snap := Compute(members, map[string]ConstituentWindow{
		"CURRENT": current,
		"PRIOR":   prior,
	}, "2026-05-18", time.Now())

	if snap.Coverage != 1 {
		t.Fatalf("coverage=%d, want only the current-session window", snap.Coverage)
	}
	if len(snap.Excluded) != 1 || snap.Excluded[0] != (ExcludedMember{Symbol: "PRIOR", Reason: "session_mismatch"}) {
		t.Fatalf("excluded=%+v, want stable prior-session exclusion", snap.Excluded)
	}
}

func TestComputeStampsMethodAndKeys(t *testing.T) {
	asOf := time.Date(2026, 5, 17, 20, 35, 0, 0, time.UTC)
	snap := Compute([]string{"A"}, map[string]ConstituentWindow{"A": risingWindow("A", 100, 1)}, "2026-05-16", asOf)
	if snap.Method != methodConstituentFanout {
		t.Errorf("method: want %q, got %q", methodConstituentFanout, snap.Method)
	}
	if snap.SessionKey != "2026-05-16" {
		t.Errorf("session key: want 2026-05-16, got %q", snap.SessionKey)
	}
	if !snap.AsOf.Equal(asOf) {
		t.Errorf("asOf: want %v, got %v", asOf, snap.AsOf)
	}
	if snap.MemberCount != 1 {
		t.Errorf("member count: want 1, got %d", snap.MemberCount)
	}
}

// TestComputeAllExcluded pins the divide-by-zero guard: when no member
// has enough history, Value stays at 0 and Coverage stays at 0.
// Renderers distinguish "no data" from "zero breadth" by reading
// Coverage, not Value.
func TestComputeAllExcluded(t *testing.T) {
	members := []string{"A", "B"}
	snap := Compute(members, map[string]ConstituentWindow{}, "2026-05-16", time.Now())
	if snap.Value != 0 || snap.Coverage != 0 {
		t.Errorf("all-excluded: want value=0, coverage=0; got value=%v coverage=%d", snap.Value, snap.Coverage)
	}
}

// TestSlideWindowAppends covers the steady-state daily increment:
// today's close arrives, oldest close drops, window stays at length
// WindowSize200 (the v2 cap).
func TestSlideWindowAppends(t *testing.T) {
	// Build a window already at the v2 cap so the oldest-drops
	// invariant fires when we add one more bar.
	w := ConstituentWindow{
		Symbol:    "AAPL",
		Closes:    make([]float64, WindowSize200),
		LastBarAt: "2026-05-16",
	}
	for i := range w.Closes {
		w.Closes[i] = float64(100 + i)
	}
	oldFirst := w.Closes[0]
	out := SlideWindow(w, 999, "2026-05-17")
	if len(out.Closes) != WindowSize200 {
		t.Errorf("length: want %d, got %d", WindowSize200, len(out.Closes))
	}
	if out.Closes[len(out.Closes)-1] != 999 {
		t.Errorf("tail: want 999, got %v", out.Closes[len(out.Closes)-1])
	}
	if out.Closes[0] == oldFirst {
		t.Errorf("oldest close should have been dropped; still saw %v", oldFirst)
	}
	if out.LastBarAt != "2026-05-17" {
		t.Errorf("LastBarAt: want 2026-05-17, got %q", out.LastBarAt)
	}
}

// TestSlideWindowIdempotent covers the gateway-flake retry path:
// the same trading day's close arriving twice (e.g. corrected print)
// should overwrite the tail, not duplicate it.
func TestSlideWindowIdempotent(t *testing.T) {
	w := risingWindow("AAPL", 100, 1)
	w.LastBarAt = "2026-05-17"
	w.Closes[len(w.Closes)-1] = 149 // simulate "first try" close
	out := SlideWindow(w, 150, "2026-05-17")
	if len(out.Closes) != WindowSize {
		t.Errorf("length must stay at %d after same-day overwrite, got %d", WindowSize, len(out.Closes))
	}
	if out.Closes[len(out.Closes)-1] != 150 {
		t.Errorf("tail must be overwritten: want 150, got %v", out.Closes[len(out.Closes)-1])
	}
}

// TestSlideWindowGrowsFromEmpty covers the cold-start path: an empty
// window receives bars one by one and grows up to WindowSize200 before
// it starts dropping (the v2 cap; v1's cap was WindowSize).
func TestSlideWindowGrowsFromEmpty(t *testing.T) {
	// Each call gets a unique date so we exercise the append path,
	// not the same-day-overwrite branch (which is covered by
	// TestSlideWindowIdempotent).
	dateFor := func(i int) string {
		return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
	}
	w := ConstituentWindow{Symbol: "NEW"}
	for i := range WindowSize200 / 2 {
		w = SlideWindow(w, float64(100+i), dateFor(i))
	}
	if len(w.Closes) != WindowSize200/2 {
		t.Errorf("growth: want %d, got %d", WindowSize200/2, len(w.Closes))
	}
	// Now fill to capacity.
	for i := WindowSize200 / 2; i < WindowSize200; i++ {
		w = SlideWindow(w, float64(100+i), dateFor(i))
	}
	if len(w.Closes) != WindowSize200 {
		t.Errorf("at capacity: want %d, got %d", WindowSize200, len(w.Closes))
	}
	// One more pushes the oldest out.
	w = SlideWindow(w, 9999, dateFor(WindowSize200))
	if len(w.Closes) != WindowSize200 {
		t.Errorf("over capacity: still want %d, got %d", WindowSize200, len(w.Closes))
	}
	if w.Closes[len(w.Closes)-1] != 9999 {
		t.Errorf("tail: want 9999, got %v", w.Closes[len(w.Closes)-1])
	}
}

// TestSlideWindowDoesNotMutateInput pins the immutability contract:
// callers passing a window should be able to keep using the original
// without seeing the new close appear in it.
func TestSlideWindowDoesNotMutateInput(t *testing.T) {
	w := risingWindow("AAPL", 100, 1)
	original := append([]float64(nil), w.Closes...)
	_ = SlideWindow(w, 999, "2026-05-17")
	for i, v := range w.Closes {
		if v != original[i] {
			t.Errorf("input mutated at index %d: was %v, now %v", i, original[i], v)
		}
	}
}

// TestCompute_200DMA_NeedsLongerHistory: a window with 150 closes
// contributes to the 50-DMA reading but is excluded from the 200-DMA
// reading.
func TestCompute_200DMA_NeedsLongerHistory(t *testing.T) {
	// 150 monotonically rising closes — today is above any SMA window.
	closes := make([]float64, 150)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	members := []string{"NEW"}
	windows := map[string]ConstituentWindow{
		"NEW": {Symbol: "NEW", Closes: closes, LastBarAt: "2026-05-16"},
	}
	snap := Compute(members, windows, "2026-05-16", time.Now())
	if snap.Coverage != 1 {
		t.Errorf("coverage (50-DMA): want 1, got %d", snap.Coverage)
	}
	if snap.Coverage200 != 0 {
		t.Errorf("coverage_200: want 0 (only 150 closes), got %d", snap.Coverage200)
	}
	if snap.Value != 100.0 {
		t.Errorf("pct above 50-DMA: want 100, got %v", snap.Value)
	}
	if snap.PctAbove200DMA != 0 {
		t.Errorf("pct above 200-DMA: want 0 (no coverage), got %v", snap.PctAbove200DMA)
	}
}

// TestCompute_200DMA_FullHistory: a window with 200+ closes
// contributes to both SMA readings.
func TestCompute_200DMA_FullHistory(t *testing.T) {
	closes := make([]float64, WindowSize200)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	members := []string{"OLD"}
	windows := map[string]ConstituentWindow{
		"OLD": {Symbol: "OLD", Closes: closes, LastBarAt: "2026-05-16"},
	}
	snap := Compute(members, windows, "2026-05-16", time.Now())
	if snap.Coverage != 1 || snap.Coverage200 != 1 {
		t.Errorf("coverage: 50=%d 200=%d, want 1/1", snap.Coverage, snap.Coverage200)
	}
	if snap.Value != 100.0 || snap.PctAbove200DMA != 100.0 {
		t.Errorf("pct: 50=%v 200=%v, want 100/100", snap.Value, snap.PctAbove200DMA)
	}
}

// TestCompute_NewHighs counts a constituent that's at a new 252-bar
// high today. The window's HighRollingMax is set so today's close
// strictly exceeds it.
func TestCompute_NewHighs(t *testing.T) {
	closes := make([]float64, WindowSize200)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	w := ConstituentWindow{
		Symbol:             "HIGHER",
		Closes:             closes,
		LastBarAt:          "2026-05-16",
		HighRollingMax:     closes[len(closes)-2], // yesterday's close
		HighRollingBarsHad: RollingMaxBars,
		LowRollingMin:      100,
		LowRollingBarsHad:  RollingMaxBars,
	}
	snap := Compute([]string{"HIGHER"}, map[string]ConstituentWindow{"HIGHER": w}, "2026-05-16", time.Now())
	if snap.NewHighsToday != 1 {
		t.Errorf("new highs: want 1, got %d", snap.NewHighsToday)
	}
	if snap.NewLowsToday != 0 {
		t.Errorf("new lows: want 0, got %d", snap.NewLowsToday)
	}
}

// TestSlideWindow_RollingMaxAdvances: after seeding a 252-bar window
// and sliding two new bars in, HighRollingMax should reflect the
// trailing 251 closes excluding today's.
func TestSlideWindow_RollingMaxAdvances(t *testing.T) {
	dateFor := func(i int) string {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
	}
	w := ConstituentWindow{Symbol: "NEW"}
	// Seed RollingMaxBars + 1 bars of monotonically rising closes.
	for i := 0; i <= RollingMaxBars; i++ {
		w = SlideWindow(w, float64(100+i), dateFor(i))
	}
	if w.HighRollingBarsHad != RollingMaxBars {
		t.Errorf("HighRollingBarsHad: want %d, got %d", RollingMaxBars, w.HighRollingBarsHad)
	}
	// After seeding, HighRollingMax should be the second-to-last close
	// (since the slide step folds the prior close into the rolling max
	// before today's close lands).
	if w.HighRollingMax <= 0 {
		t.Errorf("HighRollingMax should be > 0 after seeding, got %v", w.HighRollingMax)
	}
}
