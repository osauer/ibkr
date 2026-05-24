package daemon

import "github.com/osauer/ibkr/internal/rpc"

// streakIndicator is the per-indicator surface populateStreaks iterates.
// Each implementation is a zero-state struct — pure dispatch, no fields.
// Variations between indicators (status gate, classifier inputs, value
// extraction, slot to attach the streak to) are encapsulated here so
// populateStreaks itself is one loop.
type streakIndicator interface {
	key() string
	// bandAndValue inspects res and returns the band/value the streak
	// counter should tick with. Returns ("", 0) to freeze the counter —
	// status not usable or required fields missing.
	bandAndValue(res *rpc.RegimeSnapshotResult) (band string, value float64)
	// attachStreak writes s into the indicator's slot in res.
	attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo)
}

var streakIndicators = []streakIndicator{
	vixTermStreaks{}, hygSpyStreaks{}, usdJpyStreaks{},
	gammaZeroStreaks{}, breadthStreaks{},
}

// vixTermStreaks — VIX/VIX3M term-structure ratio.
type vixTermStreaks struct{}

func (vixTermStreaks) key() string { return StreakKeyVIXTerm }

func (vixTermStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.VIXTermStructure.Status != rpc.RegimeStatusOK && res.VIXTermStructure.Status != rpc.RegimeStatusStale {
		return "", 0
	}
	band := classifyVIXTermBand(res.VIXTermStructure.Ratio)
	var value float64
	if res.VIXTermStructure.Ratio != nil {
		value = *res.VIXTermStructure.Ratio
	}
	return band, value
}

func (vixTermStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.VIXTermStructure.Streak = s
}

// hygSpyStreaks — HYG vs SPY divergence band.
type hygSpyStreaks struct{}

func (hygSpyStreaks) key() string { return StreakKeyHYGSPY }

func (hygSpyStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.HYGSPYDivergence.Status != rpc.RegimeStatusOK && res.HYGSPYDivergence.Status != rpc.RegimeStatusStale {
		return "", 0
	}
	band := classifyHYGSPYBand(res.HYGSPYDivergence)
	var value float64
	if res.HYGSPYDivergence.HYGPrice != nil {
		value = *res.HYGSPYDivergence.HYGPrice
	}
	return band, value
}

func (hygSpyStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.HYGSPYDivergence.Streak = s
}

// usdJpyStreaks — USD/JPY weekly-change band.
type usdJpyStreaks struct{}

func (usdJpyStreaks) key() string { return StreakKeyUSDJPY }

func (usdJpyStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.USDJPY.Status != rpc.RegimeStatusOK && res.USDJPY.Status != rpc.RegimeStatusStale {
		return "", 0
	}
	band := classifyUSDJPYBand(res.USDJPY.WeeklyChange)
	var value float64
	if res.USDJPY.WeeklyChange != nil {
		value = *res.USDJPY.WeeklyChange
	}
	return band, value
}

func (usdJpyStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.USDJPY.Streak = s
}

// gammaZeroStreaks gates on OK-only because the gamma envelope's Stale
// path doesn't carry a Result pointer; the nested-pointer check is
// meaningful and must precede classifier invocation.
type gammaZeroStreaks struct{}

func (gammaZeroStreaks) key() string { return StreakKeyGammaZero }

func (gammaZeroStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if res.GammaZero.Status != rpc.RegimeStatusOK || res.GammaZero.Envelope.Result == nil {
		return "", 0
	}
	c := res.GammaZero.Envelope.Result
	return classifyGammaComputedBand(c), gammaComputedStreakValue(c)
}

func (gammaZeroStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.GammaZero.Streak = s
}

// breadthStreaks — S&P 500 breadth pct-above-50DMA. Additionally gates
// on Envelope.State == BreadthStateReady; value is a plain float64
// (not a pointer) so no nil check is needed.
type breadthStreaks struct{}

func (breadthStreaks) key() string { return StreakKeyBreadth }

func (breadthStreaks) bandAndValue(res *rpc.RegimeSnapshotResult) (string, float64) {
	if (res.Breadth.Status != rpc.RegimeStatusOK && res.Breadth.Status != rpc.RegimeStatusStale) || res.Breadth.Envelope.State != rpc.BreadthStateReady {
		return "", 0
	}
	value := res.Breadth.Envelope.PctAbove50DMA
	band := classifyBreadthBand(value)
	return band, value
}

func (breadthStreaks) attachStreak(res *rpc.RegimeSnapshotResult, s *rpc.StreakInfo) {
	res.Breadth.Streak = s
}
