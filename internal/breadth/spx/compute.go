package spx

import (
	"fmt"
	"math"
	"time"
)

// Compute reduces a set of constituent windows to a single snapshot
// carrying the 50-DMA reading, the 200-DMA reading, and the
// new-highs/lows counts. Pure: no I/O, no clock dependency beyond the
// wall-clock stamp the caller supplies. Deterministic — same inputs
// in, same outputs out — which is what makes verification against
// public breadth indices meaningful.
//
// members is the authoritative S&P-500 list for this session. Only
// names appearing in members count; windows for delisted names are
// silently ignored. Names in members but missing from windows are
// excluded with reason "no_window". Names with thin history are
// counted toward whichever readings their history supports (a name
// with 75 bars contributes to the 50-DMA reading and is excluded
// from the 200-DMA and new-highs/lows counts).
//
// sessionKey is the New-York trading-day date string the snapshot
// represents (YYYY-MM-DD). The caller derives it; Compute does not
// inspect clocks. asOf is the wall-clock stamp that goes into
// Snapshot.AsOf — typically time.Now() at the call site, but
// injectable so tests can pin it.
func Compute(members []string, windows map[string]ConstituentWindow, sessionKey string, asOf time.Time) Snapshot {
	snap := Snapshot{
		AsOf:        asOf,
		SessionKey:  sessionKey,
		Method:      methodConstituentFanout,
		MemberCount: len(members),
	}

	above50 := 0
	above200 := 0
	coverage50 := 0
	coverage200 := 0
	coverageHL := 0
	newHighs := 0
	newLows := 0

	// Iterating in member order makes the exclusion list stable for
	// tests and easy to diff in verify.log entries.
	for _, sym := range members {
		w, ok := windows[sym]
		if !ok {
			snap.Excluded = append(snap.Excluded, ExcludedMember{Symbol: sym, Reason: "no_window"})
			continue
		}
		if len(w.Closes) < WindowSize {
			snap.Excluded = append(snap.Excluded, ExcludedMember{
				Symbol: sym,
				Reason: fmt.Sprintf("thin_history(%d)", len(w.Closes)),
			})
			continue
		}
		coverage50++
		// 50-DMA: slice the last WindowSize closes (Closes is
		// chronological, oldest first). SMA includes today's close per
		// $SPXA50R / S&P DJI convention.
		w50 := w.Closes[len(w.Closes)-WindowSize:]
		var sum50 float64
		for _, c := range w50 {
			sum50 += c
		}
		sma50 := sum50 / float64(WindowSize)
		if w50[WindowSize-1] >= sma50 {
			above50++
		}

		// 200-DMA: only contributes when the name has enough history.
		if len(w.Closes) >= WindowSize200 {
			w200 := w.Closes[len(w.Closes)-WindowSize200:]
			var sum200 float64
			for _, c := range w200 {
				sum200 += c
			}
			sma200 := sum200 / float64(WindowSize200)
			coverage200++
			if w200[WindowSize200-1] >= sma200 {
				above200++
			}
		}

		// New-highs/lows: only contributes when the rolling max/min
		// has accumulated enough bars. The slide step maintained the
		// rolling max over the previous RollingMaxBars closes
		// (excluding today's), so today's close vs HighRollingMax is
		// the test for "made a new 52-week high today".
		if w.HighRollingBarsHad >= RollingMaxBars {
			coverageHL++
			today := w.Closes[len(w.Closes)-1]
			if w.HighRollingMax > 0 && today > w.HighRollingMax {
				newHighs++
			}
			if w.LowRollingMin > 0 && today < w.LowRollingMin {
				newLows++
			}
		}
	}

	snap.Coverage = coverage50
	snap.Coverage200 = coverage200
	snap.CoverageHighsLows = coverageHL
	if coverage50 > 0 {
		snap.Value = 100.0 * float64(above50) / float64(coverage50)
		snap.PctAbove50DMA = snap.Value
	}
	if coverage200 > 0 {
		snap.PctAbove200DMA = 100.0 * float64(above200) / float64(coverage200)
	}
	snap.NewHighsToday = newHighs
	snap.NewLowsToday = newLows
	if coverageHL > 0 {
		snap.NetNewHighsPct = 100.0 * float64(newHighs-newLows) / float64(coverageHL)
	}
	return snap
}

// SlideWindow folds today's close into a constituent window. It does
// three things in one pass:
//
//  1. Append today's close to the chronological Closes slice and trim
//     to the v2 cap of WindowSize200 entries.
//  2. Update the rolling max/min over the previous RollingMaxBars
//     closes (excluding today's), so the next Compute can detect
//     "today made a new 252-bar high".
//  3. Track HighRollingBarsHad / LowRollingBarsHad so a name with
//     fewer than RollingMaxBars of history doesn't get counted as
//     making a new high on its 30th day of trading.
//
// Same-day idempotency: if barDate matches w.LastBarAt, the existing
// tail close is overwritten and counters are not double-bumped. The
// rolling-max state for that name doesn't change on a same-day
// re-fetch — late prints settling shouldn't kick a new-high.
//
// The input is not mutated; callers assign the result back if they
// want persistence.
func SlideWindow(w ConstituentWindow, close float64, barDate string) ConstituentWindow {
	out := ConstituentWindow{
		Symbol:             w.Symbol,
		Closes:             append([]float64(nil), w.Closes...),
		LastBarAt:          barDate,
		HighRollingMax:     w.HighRollingMax,
		HighRollingBarsHad: w.HighRollingBarsHad,
		LowRollingMin:      w.LowRollingMin,
		LowRollingBarsHad:  w.LowRollingBarsHad,
	}
	if w.LastBarAt == barDate && len(out.Closes) > 0 {
		// Same trading day appearing twice — overwrite the tail to
		// reflect the corrected close. Don't grow the window. Rolling
		// max/min stays as-is: a late-print correction to today's
		// close shouldn't kick a new-high vs the prior-251-day max
		// that's already locked in.
		out.Closes[len(out.Closes)-1] = close
		return out
	}
	// Roll the prior close (if any) into the rolling max/min. The
	// rolling max is defined as max(close over previous N bars
	// excluding today), so we fold yesterday's close into the rolling
	// state BEFORE today's close lands in Closes.
	if len(out.Closes) > 0 {
		prevClose := out.Closes[len(out.Closes)-1]
		out.HighRollingBarsHad = min(out.HighRollingBarsHad+1, RollingMaxBars)
		out.LowRollingBarsHad = out.HighRollingBarsHad
		if out.HighRollingMax == 0 || prevClose > out.HighRollingMax {
			out.HighRollingMax = prevClose
		}
		if out.LowRollingMin == 0 || prevClose < out.LowRollingMin {
			out.LowRollingMin = prevClose
		}
		// Once we've seen RollingMaxBars bars, the simple "max-so-far"
		// formulation no longer matches the rolling-window semantics —
		// the oldest bar should fall out of the rolling set. We don't
		// keep a long-enough close history to recompute the max
		// exactly on every slide; instead, after we've seen the full
		// trailing year we periodically rebuild from the Closes slice
		// (which holds 200 bars; the rolling max over 200 of the last
		// 252 sessions is a close approximation; specifically, the
		// max over the trailing 200 is the same as the max over the
		// trailing 252 unless the historical peak was 200-252 days
		// ago — rare and surfaces as a slightly-stale rolling-max
		// reading, but within the v1 calibration window).
		if out.HighRollingBarsHad == RollingMaxBars && len(out.Closes) >= WindowSize200 {
			out.HighRollingMax = sliceMax(out.Closes)
			out.LowRollingMin = sliceMin(out.Closes)
		}
	}
	out.Closes = append(out.Closes, close)
	if len(out.Closes) > WindowSize200 {
		// Drop oldest. Keep at most WindowSize200 entries — older
		// closes carry no information for the 200-DMA and the
		// rolling-max state already integrated them.
		out.Closes = out.Closes[len(out.Closes)-WindowSize200:]
	}
	return out
}

// sliceMax / sliceMin are the rolling-max / rolling-min helpers used
// on the close-slice path. Return 0 for empty input — the caller
// guards before calling.
func sliceMax(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := math.Inf(-1)
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

func sliceMin(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := math.Inf(1)
	for _, x := range xs {
		if x < m {
			m = x
		}
	}
	return m
}
