package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// fakeDeps captures pre-canned answers for the three quote+history
// regime fetchers (VIX, HYG/SPY, USD/JPY). Tests construct one with
// the bits they need and the closures pull from these maps —
// missing keys yield zeros, which mirrors the production "no tick"
// path the fetchers gate on.
//
// warnLog accumulates log lines a fetcher emits on partial failure
// (history error, insufficient bars). Tests assert against it to
// verify the operator-visible diagnostic landed; nil here means the
// fetcher had no failures worth logging.
type fakeDeps struct {
	snapshots map[string]fakeQuote
	// rich holds the Week52High value the snapshotWith52WHigh dep
	// returns for a given symbol. Tests that don't set an entry simulate
	// the "Misc Stats tick 165 didn't land in the budget" case.
	rich    map[string]fakeRichQuote
	bars    map[string]fakeHistory
	warnLog *[]string
}

type fakeQuote struct {
	price    float64
	dataType string
}

type fakeRichQuote struct {
	price      float64
	week52High float64
	dataType   string
}

type fakeHistory struct {
	bars []ibkrlib.HistoricalBar
	err  error
}

func (f *fakeDeps) build() *regimeDeps {
	return &regimeDeps{
		snapshot: func(_ context.Context, sym string, _ time.Duration) (float64, string) {
			q := f.snapshots[sym]
			return q.price, q.dataType
		},
		snapshotWith52WHigh: func(_ context.Context, sym string, _ time.Duration) (float64, float64, string) {
			r := f.rich[sym]
			return r.price, r.week52High, r.dataType
		},
		history: func(sym string, _ int, _ time.Duration) ([]ibkrlib.HistoricalBar, error) {
			h := f.bars[sym]
			return h.bars, h.err
		},
		logWarnf: func(format string, args ...any) {
			if f.warnLog != nil {
				*f.warnLog = append(*f.warnLog, fmt.Sprintf(format, args...))
			}
		},
	}
}

// makeBars synthesises N daily bars with a constant close. Oldest-
// first, matching what FetchHistoricalDailyBars returns.
func makeBars(n int, closePrice float64) []ibkrlib.HistoricalBar {
	out := make([]ibkrlib.HistoricalBar, n)
	for i := range n {
		out[i] = ibkrlib.HistoricalBar{Close: closePrice}
	}
	return out
}

func TestFetchRegimeVIXTerm(t *testing.T) {
	ctx := context.Background()
	t.Run("happy_live", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, dataType: rpc.MarketDataLive},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataLive},
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Fatalf("status=%q, want ok", got.Status)
		}
		if got.Ratio == nil || *got.Ratio < 0.857 || *got.Ratio > 0.858 {
			t.Errorf("ratio=%v, want ≈0.857", got.Ratio)
		}
		if len(got.FieldsMissing) != 0 {
			t.Errorf("fields_missing=%v, want empty", got.FieldsMissing)
		}
	})

	t.Run("stale_data_type", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, dataType: rpc.MarketDataFrozen},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataFrozen},
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusStale {
			t.Errorf("frozen tick: status=%q, want stale", got.Status)
		}
	})

	t.Run("vix3m_missing", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX": {price: 18.0, dataType: rpc.MarketDataLive},
				// VIX3M absent → snapshot returns 0
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Fatalf("status=%q, want error", got.Status)
		}
		if got.VIX == nil || *got.VIX != 18.0 {
			t.Errorf("VIX should still be populated for partial-error case, got %v", got.VIX)
		}
		if got.Ratio != nil {
			t.Errorf("ratio must be nil when VIX3M missing, got %v", *got.Ratio)
		}
	})

	t.Run("vix_missing", func(t *testing.T) {
		deps := (&fakeDeps{}).build() // no snapshots
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Errorf("both missing: status=%q, want error", got.Status)
		}
		if got.VIX != nil || got.VIX3M != nil {
			t.Errorf("no measurements should be populated: got VIX=%v VIX3M=%v", got.VIX, got.VIX3M)
		}
	})
}

func TestFetchRegimeHYGSPY(t *testing.T) {
	ctx := context.Background()

	t.Run("happy_all_fields", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Fatalf("status=%q, want ok", got.Status)
		}
		if got.HYG50DMA == nil || *got.HYG50DMA != 79.5 {
			t.Errorf("HYG50DMA=%v, want 79.5", got.HYG50DMA)
		}
		if got.SPY52WHigh == nil || *got.SPY52WHigh != 540.0 {
			t.Errorf("SPY52WHigh=%v, want 540", got.SPY52WHigh)
		}
		if len(got.FieldsMissing) != 0 {
			t.Errorf("fields_missing=%v, want empty", got.FieldsMissing)
		}
	})

	t.Run("52w_high_missing", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				// SPY tick arrived but Misc-Stats 165 did not — the
				// cold-start case the new helper surfaces honestly
				// instead of returning the stale cache value.
				"SPY": {price: 530.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Errorf("status=%q, want ok (advisory missing field doesn't downgrade)", got.Status)
		}
		if !slices.Contains(got.FieldsMissing, "spy_52w_high") {
			t.Errorf("fields_missing=%v, want to include spy_52w_high", got.FieldsMissing)
		}
		if slices.Contains(got.FieldsMissing, "hyg_50dma") {
			t.Errorf("fields_missing=%v, should NOT include hyg_50dma", got.FieldsMissing)
		}
	})

	t.Run("50dma_history_error", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {err: errors.New("history fetch failed")},
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Errorf("status=%q, want ok", got.Status)
		}
		if !slices.Contains(got.FieldsMissing, "hyg_50dma") {
			t.Errorf("fields_missing=%v, want to include hyg_50dma", got.FieldsMissing)
		}
		// Operator-visible diagnostic: the silent-swallow that hid this
		// failure pre-fix is now a warn-level log line carrying the
		// underlying error. Without this the renderer's "hyg_50dma
		// missing" is uninterpretable — was it a timeout? entitlement?
		// transient gateway hiccup? The log answers.
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "HYG 50DMA") && strings.Contains(s, "history fetch failed")
		}) {
			t.Errorf("expected warn log for HYG history error, got %v", warns)
		}
	})

	t.Run("50dma_insufficient_bars_logged", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(30, 79.5)}, // < 50
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.HYG50DMA != nil {
			t.Errorf("HYG50DMA must be nil when bars insufficient, got %v", *got.HYG50DMA)
		}
		if !slices.Contains(got.FieldsMissing, "hyg_50dma") {
			t.Errorf("fields_missing=%v, want hyg_50dma", got.FieldsMissing)
		}
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "insufficient bars")
		}) {
			t.Errorf("expected warn log for insufficient-bars case, got %v", warns)
		}
	})

	t.Run("spy_spot_missing", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			// SPY's rich snapshot didn't land at all
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Errorf("status=%q, want error when SPY spot missing", got.Status)
		}
	})

	t.Run("spy_52w_high_falls_back_to_daily_bars_when_tick_missing", func(t *testing.T) {
		// Off-hours / frozen-mode reproducer. The gateway delivers SPY's
		// bid/ask/last as a single snapshot then goes silent — tick 165
		// (Misc Stats) never lands no matter how long we wait, so
		// snapshotWith52WHigh returns week52High=0. The indicator must
		// derive 52w high from daily bars rather than surface a null
		// field and drop to a 2-state signal at any hour the market is
		// closed.
		bars := make([]ibkrlib.HistoricalBar, 252)
		for i := range bars {
			bars[i] = ibkrlib.HistoricalBar{Close: 500.0, High: 500.0 + float64(i)}
		}
		// The peak is at the last bar (251) by construction; the helper
		// must scan the full 252-bar window, not just the latest.
		bars[100].High = 999.0 // peak somewhere in the middle
		const wantMaxHigh = 999.0

		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataFrozen},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 0, dataType: rpc.MarketDataFrozen},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {bars: bars},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPY52WHigh == nil {
			t.Fatalf("SPY52WHigh must fall back to daily-bar max(High) when tick 165 is absent, got nil")
		}
		if *got.SPY52WHigh != wantMaxHigh {
			t.Errorf("SPY52WHigh=%v, want %v (peak in synthetic bars)", *got.SPY52WHigh, wantMaxHigh)
		}
		if slices.Contains(got.FieldsMissing, "spy_52w_high") {
			t.Errorf("fields_missing=%v, must NOT include spy_52w_high once fallback supplied it", got.FieldsMissing)
		}
	})

	t.Run("spy_52w_high_live_tick_takes_precedence_over_fallback", func(t *testing.T) {
		// When the gateway *did* deliver tick 165, that value is the
		// truth — even if our daily-bar derivation would compute
		// something larger (e.g. an intraday spike not yet reflected in
		// the closing-day bar set). Don't second-guess the gateway.
		bars := make([]ibkrlib.HistoricalBar, 252)
		for i := range bars {
			bars[i] = ibkrlib.HistoricalBar{Close: 500.0, High: 999.0}
		}
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 600.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {bars: bars},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPY52WHigh == nil || *got.SPY52WHigh != 600.0 {
			t.Errorf("SPY52WHigh=%v, want 600 (live gateway value, not the 999 we'd derive)", got.SPY52WHigh)
		}
	})

	t.Run("spy_52w_high_fallback_history_error_logged", func(t *testing.T) {
		// Symmetric with the HYG 50DMA history-error case: if the
		// fallback's history fetch fails, the renderer learns *why*
		// spy_52w_high is missing from the daemon log rather than
		// staring at a silent null field. Operator-visible diagnostic,
		// not just an advisory missing-field note.
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataFrozen},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 0, dataType: rpc.MarketDataFrozen},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {err: errors.New("history fetch failed")},
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPY52WHigh != nil {
			t.Errorf("SPY52WHigh must be nil when both live tick and fallback fail, got %v", *got.SPY52WHigh)
		}
		if !slices.Contains(got.FieldsMissing, "spy_52w_high") {
			t.Errorf("fields_missing=%v, want spy_52w_high", got.FieldsMissing)
		}
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "SPY") && strings.Contains(s, "52w") && strings.Contains(s, "history fetch failed")
		}) {
			t.Errorf("expected warn log for SPY 52w fallback history error, got %v", warns)
		}
	})
}

func TestFetchRegimeUSDJPY(t *testing.T) {
	ctx := context.Background()

	t.Run("happy_live", func(t *testing.T) {
		// Construct 10 bars with closes 150..159 — most-recent is 159,
		// 7-trading-days-ago bar is index len-8 = 2, close 152.
		bars := make([]ibkrlib.HistoricalBar, 10)
		for i := range 10 {
			bars[i] = ibkrlib.HistoricalBar{Close: float64(150 + i)}
		}
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: bars},
			},
		}).build()
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Fatalf("status=%q, want ok", got.Status)
		}
		if got.Close7DAgo == nil || *got.Close7DAgo != 152.0 {
			t.Errorf("Close7DAgo=%v, want 152", got.Close7DAgo)
		}
		if got.WeeklyChange == nil {
			t.Fatalf("WeeklyChange should be populated")
		}
		// (160 - 152) / 152 * 100 ≈ 5.263
		if *got.WeeklyChange < 5.26 || *got.WeeklyChange > 5.27 {
			t.Errorf("WeeklyChange=%v, want ≈5.263", *got.WeeklyChange)
		}
		if len(got.FieldsMissing) != 0 {
			t.Errorf("fields_missing=%v, want empty", got.FieldsMissing)
		}
	})

	t.Run("thin_history", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: makeBars(3, 158.0)}, // < 8 bars
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Errorf("status=%q, want ok (live FX, advisory history miss)", got.Status)
		}
		if !slices.Contains(got.FieldsMissing, "close_7d_ago") {
			t.Errorf("fields_missing=%v, want close_7d_ago", got.FieldsMissing)
		}
		if !slices.Contains(got.FieldsMissing, "weekly_change_pct") {
			t.Errorf("fields_missing=%v, want weekly_change_pct", got.FieldsMissing)
		}
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "USD.JPY") && strings.Contains(s, "insufficient")
		}) {
			t.Errorf("expected warn log for thin USD.JPY history, got %v", warns)
		}
	})

	t.Run("fx_history_error_logged", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {err: errors.New("contract details unresolved")},
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Close7DAgo != nil {
			t.Errorf("Close7DAgo must be nil on history error")
		}
		// Operator-visible: this is the FX-race case that bit us in
		// review — pre-fix the daemon silently swallowed the
		// contract-resolution timeout, leaving users to guess at the
		// missing weekly_change_pct.
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "USD.JPY") && strings.Contains(s, "contract details")
		}) {
			t.Errorf("expected warn log for FX history error, got %v", warns)
		}
	})

	t.Run("fx_missing", func(t *testing.T) {
		deps := (&fakeDeps{}).build() // no snapshots
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Status != rpc.RegimeStatusUnavailable {
			t.Errorf("status=%q, want unavailable when FX tick absent", got.Status)
		}
	})

	t.Run("stale_data_type", func(t *testing.T) {
		bars := make([]ibkrlib.HistoricalBar, 10)
		for i := range 10 {
			bars[i] = ibkrlib.HistoricalBar{Close: float64(150 + i)}
		}
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataDelayed},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: bars},
			},
		}).build()
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Status != rpc.RegimeStatusStale {
			t.Errorf("delayed tick: status=%q, want stale", got.Status)
		}
	})
}
