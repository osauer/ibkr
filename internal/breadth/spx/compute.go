package spx

import (
	"fmt"
	"time"
)

// Compute reduces a set of constituent windows to a single S5FI
// snapshot. Pure: no I/O, no clock dependency beyond the wall-clock
// stamp the caller supplies. The compute is deterministic — same
// inputs in, same outputs out — which is what makes verification
// against $SPXA50R meaningful.
//
// members is the authoritative S&P-500 list for this session. Only
// names appearing in members count; windows for delisted names are
// silently ignored. Names in members but missing from windows are
// excluded with reason "no_window". Names with fewer than WindowSize
// closes are excluded with reason "thin_history".
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

	above := 0
	coverage := 0

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
		coverage++
		// Window is chronological, oldest first. Most recent close is
		// the comparison point; SMA covers the full window — including
		// today's close, per the convention $SPXA50R / S&P DJI follow.
		window := w.Closes[len(w.Closes)-WindowSize:]
		var sum float64
		for _, c := range window {
			sum += c
		}
		sma := sum / float64(WindowSize)
		if window[WindowSize-1] >= sma {
			above++
		}
	}

	snap.Coverage = coverage
	if coverage > 0 {
		snap.Value = 100.0 * float64(above) / float64(coverage)
	}
	return snap
}

// SlideWindow appends today's close to a constituent window and trims
// it to WindowSize+1 entries (50 for the SMA, 1 for "today" — though
// since today participates in the SMA, we actually keep WindowSize
// entries total). LastBarAt is updated to the supplied date string.
//
// If today's close repeats a date already at the tail (idempotent
// rerun of a daily refresh, common during gateway flakiness), the
// existing entry is overwritten rather than duplicated.
//
// Returns the updated window. The input is not mutated; callers
// assign the result back if they want persistence.
func SlideWindow(w ConstituentWindow, close float64, barDate string) ConstituentWindow {
	out := ConstituentWindow{
		Symbol:    w.Symbol,
		Closes:    append([]float64(nil), w.Closes...),
		LastBarAt: barDate,
	}
	if w.LastBarAt == barDate && len(out.Closes) > 0 {
		// Same trading day appearing twice — overwrite the tail to
		// reflect the corrected close. Don't grow the window.
		out.Closes[len(out.Closes)-1] = close
		return out
	}
	out.Closes = append(out.Closes, close)
	if len(out.Closes) > WindowSize {
		// Drop oldest. Keep exactly WindowSize entries in the steady
		// state — older closes carry no information for the 50-DMA.
		out.Closes = out.Closes[len(out.Closes)-WindowSize:]
	}
	return out
}
