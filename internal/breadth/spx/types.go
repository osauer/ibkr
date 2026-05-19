// Package spx computes the S&P-500 stocks-above-50DMA breadth index
// (S&P DJI's S5FI) locally from constituent daily closes. IBKR does not
// redistribute S&P DJI's breadth-index family on retail subscriptions
// (verified via reqContractDetails probe — see pkg/ibkr/symbols.go), so
// the daemon reproduces the math from data it already has access to.
//
// The compute is a sliding window over a stream: for each S&P-500 name
// keep the last 50 daily closes, count names where the most recent
// close is ≥ the window mean, divide by member count, multiply by 100.
// This reproduces S&P DJI's published S5FI value bit-identically when
// the membership list and constituent close data are both current.
//
// State footprint is ≈ 200 KB on disk (500 names × 50 floats + a
// membership list) — small enough that a single JSON file with
// temp-rename atomic writes is the right persistence primitive. No
// database, no schema migration, no out-of-process cache.
package spx

import "time"

// Method is the methodology token stamped on every snapshot so renderers
// can disclose provenance without parsing free-form notes. The constant
// is deliberately unexported — callers should compare against Snapshot.Method
// rather than rebuilding the string.
const methodConstituentFanout = "constituent-fanout-50dma"

// WindowSize is the SMA lookback (S&P DJI's S5FI is the 50-day variant).
// The window holds the 50 most recent daily closes chronologically; the
// most recent close is window[len-1]. SMA = mean(window). A name is
// "above 50DMA" when window[len-1] >= mean(window). Today's close
// participates in its own SMA — this matches the convention used by
// $SPXA50R / StockCharts and S&P DJI's published S5FI methodology.
const WindowSize = 50

// Snapshot is one S5FI reading: the computed value, when it was
// computed, which trading session it represents, and provenance for
// every consumer to read without re-deriving anything. Persisted as
// snapshot.json in the cache dir; serialised over the wire inside the
// breadth.spx RPC envelope.
type Snapshot struct {
	// Value is the S5FI percentage in [0, 100].
	Value float64 `json:"value"`
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
	// Coverage is the count of members that had enough history
	// (≥ WindowSize closes) to contribute to the compute. The
	// denominator in Value is Coverage, not MemberCount, so a recent
	// listing with thin history doesn't push the percentage downward.
	Coverage int `json:"coverage"`
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
// window is full Len(Closes) == WindowSize. LastBarAt is the date
// string of the most recent close in YYYY-MM-DD form — used to decide
// whether the next refresh needs to fetch new bars for this name.
type ConstituentWindow struct {
	Symbol    string    `json:"symbol"`
	Closes    []float64 `json:"closes"`
	LastBarAt string    `json:"last_bar_at"`
}

// WindowSet is the persistence shape for windows.json. The version
// field is a migration handle reserved for the day the on-disk format
// has to change incompatibly. Today it should always be 1.
type WindowSet struct {
	Version int                          `json:"version"`
	AsOf    time.Time                    `json:"as_of"`
	Windows map[string]ConstituentWindow `json:"windows"`
}

// CurrentWindowSetVersion is the schema version the engine writes. On
// load, files with an unknown version are treated as no-cache so the
// engine cold-starts rather than mis-interprets a future format.
const CurrentWindowSetVersion = 1

// HistoryPoint is one day's S5FI reading on the rolling history file.
// The renderer pulls the trailing N for the dashboard sparkline.
// Date is the NY-tz session key (YYYY-MM-DD); Value is the S5FI
// percentage [0, 100] computed for that session.
type HistoryPoint struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
}

// HistorySet is the persistence shape for history.json. Points are
// stored in chronological order, oldest first, capped at
// MaxHistoryPoints to bound the wire payload and file size.
type HistorySet struct {
	Version int            `json:"version"`
	Points  []HistoryPoint `json:"points"`
}

// CurrentHistorySetVersion is the schema version the engine writes.
// On load, files with an unknown version are treated as no-cache so
// a future bump triggers a graceful rebuild rather than a parse
// error.
const CurrentHistorySetVersion = 1

// MaxHistoryPoints caps how many days of S5FI history the engine
// retains on disk. The dashboard CLI shows ~30 by default; we keep
// twice that so a daemon that's been down for a month still ships a
// useful sparkline on its first call after restart. After cap, oldest
// points roll off.
const MaxHistoryPoints = 60
