// Package spx computes S&P 500 breadth measurements locally from a validated
// constituent universe and daily closes obtained through the daemon's broker
// connector.
//
// The compute is a sliding window over a stream: for each S&P-500 name
// keep the last 50 daily closes, count names where the most recent
// close is ≥ the window mean, divide by member count, multiply by 100.
// The engine owns refresh concurrency and its in-memory view. In normal daemon
// operation, snapshots, rolling windows, history, and refreshed membership are
// persisted as typed daemon.db state and observations. Embedded membership is
// the cold fallback; JSON file paths remain only for explicit legacy import and
// isolated codec tests.
package spx

import "time"

// Method is the methodology token stamped on every snapshot so renderers
// can disclose provenance without parsing free-form notes. The constant
// is deliberately unexported — callers should compare against Snapshot.Method
// rather than rebuilding the string.
//
// The token changes whenever the compute methodology or snapshot payload shape
// becomes incompatible. LoadSnapshot treats a mismatch as a cold start rather
// than publishing partially decoded values.
const methodConstituentFanout = "constituent-fanout-50/200dma+nh-v2"

// MethodConstituentFanout is the exported form of the current breadth
// methodology token for daemon wire envelopes and documentation.
const MethodConstituentFanout = methodConstituentFanout

// MinCoverageFraction is the minimum fraction of MemberCount that a
// refresh must cover before the engine will persist its result.
// Refreshes below this threshold are treated as "did not converge"
// — typical causes: a connector-not-ready race at cold-start (where
// every fetch returns "no gateway connector"), an outage mid-fan-out,
// or a pacing-induced abort. Persisting a below-threshold snapshot
// would mislead any consumer that reads the cached value, and would
// poison the scheduler's "today's snapshot exists, skip the next
// bootstrap" check; refusing to persist forces a retry on the next
// tick instead. The 0.80 threshold tolerates ordinary per-name fetch
// errors (e.g. a few delisted-but-not-yet-removed tickers) while
// rejecting catastrophic fan-out failures.
const MinCoverageFraction = 0.80

// WindowSize is the 50-day SMA lookback (S&P DJI's S5FI is the
// 50-day variant). The window holds the 50 most recent daily closes
// chronologically; the most recent close is window[len-1]. SMA =
// mean(window). A name is "above 50DMA" when window[len-1] >=
// mean(window). Today's close participates in its own SMA — this
// matches the convention used by $SPXA50R / StockCharts and S&P
// DJI's published S5FI methodology.
const WindowSize = 50

// WindowSize200 is the 200-day SMA lookback ($SPXA200R). Catches
// cyclical tops cleanly (1999, 2021) — slower-moving than the 50-day
// reading but a meaningful complement. Computed in the same pass over
// each constituent's daily bars, so the cold-start cost is unchanged
// (IBKR's pacing limit is per-request, not per-bar; pulling 200 days
// instead of 50 doesn't cost more requests).
const WindowSize200 = 200

// RollingMaxBars is the lookback for the per-constituent rolling max/min
// of close used for the new-52-week-highs/lows count. 252 trading bars
// approximates one calendar year of US sessions (252 = 252 weekday
// sessions per year, leaving the 9-10 US market holidays out — close
// enough to "52 weeks" for the reading the renderer wants). A name
// "makes a new 52-week high today" when today's close strictly exceeds
// the max of the previous 251 closes.
const RollingMaxBars = 252

// Snapshot is one breadth reading: the computed values, represented trading
// session, and provenance carried by the breadth.spx RPC envelope and runtime
// persistence record.
type Snapshot struct {
	// Value is the 50-DMA reading: percentage of constituents trading
	// above their own 50-day SMA, in [0, 100].
	Value float64 `json:"value"`
	// PctAbove50DMA is the 50-day reading exposed under the canonical
	// long-form name (renderer-friendly). Equal to Value; kept
	// alongside Value so the wire shape is self-documenting.
	PctAbove50DMA float64 `json:"pct_above_50dma"`
	// PctAbove200DMA is the 200-day reading. Below 40% = red /
	// 40–60% = yellow / above 60% = green per the locked plan
	// (calibrated to the post-Mag-7 era; StockCharts' 70/30 default
	// would have read red far too often through 2024-2025).
	PctAbove200DMA float64 `json:"pct_above_200dma"`
	// NewHighsToday is the count of S&P 500 constituents whose latest
	// close strictly exceeded their rolling 252-bar max (~1 year of
	// trading sessions ≈ "52-week high"). Coverage-aware: a name with
	// < RollingMaxBars history is skipped, not counted as either.
	NewHighsToday int `json:"new_highs_today"`
	// NewLowsToday is the symmetric count for new 252-bar lows.
	NewLowsToday int `json:"new_lows_today"`
	// NetNewHighsPct is (NewHighs - NewLows) / coverage × 100. A
	// positive number means more names making new highs than new
	// lows; a deeply-negative one is the textbook divergence pattern
	// (SPX near highs but few constituents leading it).
	NetNewHighsPct float64 `json:"net_new_highs_pct"`
	// AsOf is the wall-clock instant the compute finished. Distinct
	// from SessionKey: a snapshot may be refreshed multiple times
	// against the same trading session as late prints settle.
	AsOf time.Time `json:"as_of"`
	// SessionKey is the New-York date of the trading session the
	// snapshot represents (YYYY-MM-DD). Resilient to UTC vs local
	// timezone confusion when the daemon runs outside the US.
	SessionKey string `json:"session_key"`
	// Method is a stable token identifying the compute methodology.
	// Renderers and tests pin this string to detect silent algorithm
	// changes.
	Method string `json:"method"`
	// MemberCount is the size of the membership list used in the
	// compute. Should track the S&P-500 cardinality (~500–505 with
	// the dual-class names).
	MemberCount int `json:"member_count"`
	// Coverage is the count of members that had enough 50-DMA history
	// (≥ WindowSize closes) to contribute to the headline. The
	// denominator in Value is Coverage, not MemberCount, so a recent
	// listing with thin history doesn't push the percentage downward.
	Coverage int `json:"coverage"`
	// Coverage200 is the analogous denominator for PctAbove200DMA —
	// names with ≥ WindowSize200 closes. Smaller than Coverage when
	// some constituents have between 50 and 200 days of history (post-
	// IPO names, recent index additions).
	Coverage200 int `json:"coverage_200"`
	// CoverageHighsLows is the denominator for the new-highs/lows
	// count — names with ≥ RollingMaxBars closes. Smaller than
	// Coverage200 when some constituents have between 200 and 252
	// days of history. Used as the denominator in NetNewHighsPct.
	CoverageHighsLows int `json:"coverage_highs_lows"`
	// Excluded lists members dropped from the compute and the reason
	// — useful when verifying against $SPXA50R divergence. Empty in
	// the steady state.
	Excluded []ExcludedMember `json:"excluded,omitempty"`
}

// ExcludedMember explains why a constituent did not contribute to the
// compute. The codebase logs this so the verification scrape can
// attribute small divergences to known causes (new listing, missing
// data feed, etc.) rather than algorithm bugs.
type ExcludedMember struct {
	Symbol string `json:"symbol"`
	Reason string `json:"reason"`
}

// ConstituentWindow holds the sliding window of daily closes for one
// S&P-500 name. Closes is chronological (oldest first); when the
// window is full len(Closes) == WindowSize200. LastBarAt is
// the date string of the most recent close in YYYY-MM-DD form — used
// to decide whether the next refresh needs to fetch new bars for this
// name.
//
// The window holds the trailing 200 closes per constituent, enough to cover
// the 200-day SMA. The 50-day reading slices the
// last 50 closes; the 200-day reading uses the full window; the
// rolling-max/min for new-highs/lows uses a separate field tracked
// outside the close window because the lookback (252 bars) exceeds
// what we keep in Closes.
type ConstituentWindow struct {
	Symbol    string    `json:"symbol"`
	Closes    []float64 `json:"closes"`
	LastBarAt string    `json:"last_bar_at"`
	// HighWindow is the trailing 252-bar rolling max of close
	// (~1 year), updated each refresh from the bars merged in. Kept
	// separately from Closes so the persisted footprint stays bounded:
	// we don't need 252 floats per name; the rolling max compressed
	// into a single value + the count of bars contributing is enough
	// to detect today's-close-above-prior-252-day-max. RollingMax is
	// the max of the previous N closes (excluding today); the daemon
	// compares today's close vs RollingMax to decide whether today is
	// a new high.
	HighRollingMax     float64 `json:"high_rolling_max,omitempty"`
	HighRollingBarsHad int     `json:"high_rolling_bars_had,omitempty"`
	LowRollingMin      float64 `json:"low_rolling_min,omitempty"`
	LowRollingBarsHad  int     `json:"low_rolling_bars_had,omitempty"`
}

// WindowSet is the versioned persistence shape for constituent windows. An
// incompatible Version is treated as no state and triggers a cold rebuild.
type WindowSet struct {
	Version int                          `json:"version"`
	AsOf    time.Time                    `json:"as_of"`
	Windows map[string]ConstituentWindow `json:"windows"`
}

// CurrentWindowSetVersion is the constituent-window schema version written by
// the engine. Other versions are not projected into current state.
const CurrentWindowSetVersion = 2

// HistoryPoint is one session's breadth reading in rolling history. The
// renderer pulls the trailing N for the dashboard sparkline. Date
// is the NY-tz session key (YYYY-MM-DD); the four numbers carry the
// 50-DMA reading, the 200-DMA reading, and the constituent counts for
// new 52-week highs and lows.
type HistoryPoint struct {
	Date           string  `json:"date"`
	PctAbove50DMA  float64 `json:"pct_above_50dma"`
	PctAbove200DMA float64 `json:"pct_above_200dma,omitempty"`
	NewHighs       int     `json:"new_highs,omitempty"`
	NewLows        int     `json:"new_lows,omitempty"`
}

// HistorySet is the versioned rolling-history persistence shape. Points are
// stored chronologically, oldest first, and capped at MaxHistoryPoints.
type HistorySet struct {
	Version int            `json:"version"`
	Points  []HistoryPoint `json:"points"`
}

// CurrentHistorySetVersion is the history schema version written by the engine.
// Other versions are not projected into current state.
const CurrentHistorySetVersion = 2

// MaxHistoryPoints caps how many days of S5FI history the engine retains. The
// dashboard CLI shows ~30 by default; the engine keeps
// twice that so a daemon that's been down for a month still ships a
// useful sparkline on its first call after restart. After cap, oldest
// points roll off.
const MaxHistoryPoints = 60
