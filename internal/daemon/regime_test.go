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
	price     float64
	prevClose float64 // tick 9 (previous regular-session close); 0 = "didn't arrive"
	dataType  string
}

type fakeRichQuote struct {
	price      float64
	prevClose  float64 // tick 9 (previous regular-session close); 0 = "didn't arrive"
	week52High float64
	dataType   string
}

type fakeHistory struct {
	bars []ibkrlib.HistoricalBar
	err  error
}

func (f *fakeDeps) build() *regimeDeps {
	return &regimeDeps{
		snapshot: func(_ context.Context, sym string, _ time.Duration) (float64, float64, string) {
			q := f.snapshots[sym]
			return q.price, q.prevClose, q.dataType
		},
		snapshotWith52WHigh: func(_ context.Context, sym string, _ time.Duration) (float64, float64, float64, string) {
			r := f.rich[sym]
			return r.price, r.prevClose, r.week52High, r.dataType
		},
		history: func(_ context.Context, sym string, _ int) ([]ibkrlib.HistoricalBar, error) {
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

	// Mixed-freshness pair: the snapshot helper returns the previous
	// regular-session close for VIX3M (with dataType=frozen) when no
	// live tick lands. The row ranks at stale status — both legs are
	// usable for the ratio, but the UX must indicate that the value
	// isn't live so the renderer dims it.
	t.Run("vix3m_frozen_leg_downgrades_to_stale", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, dataType: rpc.MarketDataLive},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataFrozen},
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusStale {
			t.Fatalf("status=%q, want stale (live VIX + frozen VIX3M leg)", got.Status)
		}
		if got.Ratio == nil || *got.Ratio < 0.857 || *got.Ratio > 0.858 {
			t.Errorf("ratio=%v, want ≈0.857", got.Ratio)
		}
		if got.DataType != rpc.MarketDataFrozen {
			t.Errorf("data_type=%q, want %q (staler leg wins)", got.DataType, rpc.MarketDataFrozen)
		}
	})

	// Day-change anchor: when tick 9 lands alongside VIX, the fetcher
	// computes VIXChangePct as ((vix - prev) / prev) * 100. Dashboard
	// header reads this to color-code the VIX cell (red on up, green on
	// down — vol expanding is risk-off).
	t.Run("vix_prev_close_populates_change_pct", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, prevClose: 20.0, dataType: rpc.MarketDataLive},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataLive},
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.VIXPrevClose == nil || *got.VIXPrevClose != 20.0 {
			t.Fatalf("VIXPrevClose=%v, want 20.0", got.VIXPrevClose)
		}
		// (18 - 20) / 20 * 100 = -10
		if got.VIXChangePct == nil || *got.VIXChangePct < -10.01 || *got.VIXChangePct > -9.99 {
			t.Errorf("VIXChangePct=%v, want ≈-10", got.VIXChangePct)
		}
	})

	// The dashboard header is useful even when VIX3M fails — partial
	// envelope must still surface the VIX day-change anchor so the
	// reader sees "VIX 18.4 −10.0%" even with no term-structure ratio.
	t.Run("vix_prev_close_survives_vix3m_failure", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX": {price: 18.0, prevClose: 20.0, dataType: rpc.MarketDataLive},
				// VIX3M absent
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Fatalf("status=%q, want error", got.Status)
		}
		if got.VIXPrevClose == nil || *got.VIXPrevClose != 20.0 {
			t.Errorf("VIXPrevClose=%v, want 20.0 (header anchor must survive partial-error row)", got.VIXPrevClose)
		}
		if got.VIXChangePct == nil {
			t.Errorf("VIXChangePct must populate even when ratio leg fails")
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

	// Day-change anchor: when tick 9 lands alongside SPY's price triple,
	// the fetcher derives both the dollar and percent change. Dashboard
	// header reads these to render "SPY 530.00 +5.00 (+0.95%)" above the
	// indicator rows.
	t.Run("spy_prev_close_populates_change_and_pct", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, prevClose: 525.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPYPrevClose == nil || *got.SPYPrevClose != 525.0 {
			t.Fatalf("SPYPrevClose=%v, want 525.0", got.SPYPrevClose)
		}
		if got.SPYChange == nil || *got.SPYChange < 4.99 || *got.SPYChange > 5.01 {
			t.Errorf("SPYChange=%v, want 5.00", got.SPYChange)
		}
		// (530 - 525) / 525 * 100 ≈ 0.9524
		if got.SPYChangePct == nil || *got.SPYChangePct < 0.95 || *got.SPYChangePct > 0.96 {
			t.Errorf("SPYChangePct=%v, want ≈0.95", got.SPYChangePct)
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

// TestRegime_CallSequence_HonestUnavailable pins the contract for what
// happens when a regime fetcher's data lands on call N and then doesn't
// land on call N+1. The daemon does NOT keep a last-known-good cache;
// the affected field becomes nil and the row's status honestly reports
// error or unavailable. A renderer can detect the transition by diffing
// the wire shape between two calls — exactly what release-verify.sh's
// regime-twice step does against a live gateway.
//
// The four sub-cases cover the live-gateway risk surface called out by
// the senior verification review: VIX3M timing out (RegimeStatusError),
// USD.JPY FX tick missing (RegimeStatusUnavailable), SPY tick 165
// missing (row stays ok, field moves into FieldsMissing), and the
// SPY 52w-high fallback collapsing when history thins (advisory field
// stays missing rather than holding the prior bar).
func TestRegime_CallSequence_HonestUnavailable(t *testing.T) {
	ctx := context.Background()

	t.Run("vix3m_drops_on_call_2_flips_ok_to_error", func(t *testing.T) {
		deps := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, dataType: rpc.MarketDataLive},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataLive},
			},
		}
		built := deps.build()

		first := fetchRegimeVIXTerm(ctx, built)
		if first.Status != rpc.RegimeStatusOK {
			t.Fatalf("call 1 status=%q, want ok (precondition)", first.Status)
		}

		// VIX3M tick stops arriving on call 2 — simulate the thin-CBOE
		// off-hours case the daemon already handles inline.
		delete(deps.snapshots, "VIX3M")

		second := fetchRegimeVIXTerm(ctx, built)
		if second.Status != rpc.RegimeStatusError {
			t.Errorf("call 2 status=%q, want error (rule 12: honest report when tick drops, no stale-cache layer)", second.Status)
		}
		if second.Ratio != nil {
			t.Errorf("call 2 ratio=%v, want nil (rule 12: must not carry over the call-1 value)", *second.Ratio)
		}
		if second.VIX == nil || *second.VIX != 18.0 {
			t.Errorf("call 2 VIX=%v, want 18.0 (the surviving leg still surfaces)", second.VIX)
		}
	})

	t.Run("usdjpy_drops_on_call_2_flips_ok_to_unavailable", func(t *testing.T) {
		deps := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 149.5, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: makeBars(14, 149.0)},
			},
		}
		built := deps.build()

		first := fetchRegimeUSDJPY(ctx, built)
		if first.Status != rpc.RegimeStatusOK {
			t.Fatalf("call 1 status=%q, want ok (precondition)", first.Status)
		}

		// FX tick stops on call 2 — the IDEALPRO entitlement case.
		delete(deps.snapshots, "USD.JPY")

		second := fetchRegimeUSDJPY(ctx, built)
		if second.Status != rpc.RegimeStatusUnavailable {
			t.Errorf("call 2 status=%q, want unavailable (rule 12: FX-tick miss must surface honestly)", second.Status)
		}
		if second.Last != nil {
			t.Errorf("call 2 last=%v, want nil (rule 12: must not carry over)", *second.Last)
		}
	})

	t.Run("spy_tick165_drops_on_call_2_field_moves_to_missing", func(t *testing.T) {
		deps := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}
		built := deps.build()

		first := fetchRegimeHYGSPY(ctx, built)
		if first.Status != rpc.RegimeStatusOK || first.SPY52WHigh == nil {
			t.Fatalf("call 1: want ok with SPY52WHigh populated, got status=%q SPY52WHigh=%v", first.Status, first.SPY52WHigh)
		}

		// Tick 165 (Misc Stats) stops landing on call 2; price still
		// lands, but the 52w-high reads zero. With no history fallback
		// configured, the advisory field stays missing.
		deps.rich["SPY"] = fakeRichQuote{price: 530.0, dataType: rpc.MarketDataLive}

		second := fetchRegimeHYGSPY(ctx, built)
		if second.Status != rpc.RegimeStatusOK {
			t.Errorf("call 2 status=%q, want ok (primary measurements landed; advisory missing doesn't downgrade row)", second.Status)
		}
		if second.SPY52WHigh != nil {
			t.Errorf("call 2 SPY52WHigh=%v, want nil (rule 12: no carry-over from call 1)", *second.SPY52WHigh)
		}
		if !slices.Contains(second.FieldsMissing, "spy_52w_high") {
			t.Errorf("call 2 fields_missing=%v, want to include spy_52w_high", second.FieldsMissing)
		}
	})

	t.Run("spy_52w_fallback_collapses_when_history_thins", func(t *testing.T) {
		// Call 1: tick 165 missing but the daily-bars fallback supplies
		// the 52w high. Call 2: history returns <50 bars, fallback bails,
		// the advisory field stays missing rather than echoing call 1.
		//
		// The fallback reads HistoricalBar.High (see maxHigh in regime.go);
		// makeBars sets only Close, so bars are populated by hand here.
		spyBars := make([]ibkrlib.HistoricalBar, 200)
		for i := range spyBars {
			spyBars[i] = ibkrlib.HistoricalBar{Close: 530.0, High: 540.0}
		}
		deps := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, dataType: rpc.MarketDataLive}, // tick 165 absent
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {bars: spyBars}, // fallback succeeds
			},
		}
		built := deps.build()

		first := fetchRegimeHYGSPY(ctx, built)
		if first.SPY52WHigh == nil {
			t.Fatalf("call 1: SPY 252d max(High) fallback should have populated SPY52WHigh")
		}

		// History fallback collapses on call 2 (gateway returns a
		// truncated bar set). Both primary and fallback paths fail.
		deps.bars["SPY"] = fakeHistory{bars: spyBars[:30]}

		second := fetchRegimeHYGSPY(ctx, built)
		if second.SPY52WHigh != nil {
			t.Errorf("call 2 SPY52WHigh=%v, want nil (rule 12: when both primary and fallback drop, the field disappears honestly)", *second.SPY52WHigh)
		}
		if !slices.Contains(second.FieldsMissing, "spy_52w_high") {
			t.Errorf("call 2 fields_missing=%v, want spy_52w_high", second.FieldsMissing)
		}
	})
}

// TestRunRegimeFanout_ReturnsOnCtxDoneWithPartialEnvelope pins the
// v0.27.6 fix for the regression where a single stuck fetcher hung
// the whole regime call past the daemon's deadline. The contract:
// when ctx fires, the fan-out returns within a few milliseconds with
// a fully-populated envelope where any not-yet-completed row carries
// TestBoundedSnapshot_ReturnsWithinBudgetUnderInnerBlock pins the
// per-fetcher wall-time bound. Without this, a deps.snapshot that
// blocks past its declared budget (e.g. inner SubscribeMarketData
// stalls on a saturated market-data semaphore using Connection.ctx
// instead of the caller's ctx) hangs the fetcher past the
// orchestrator's 45 s deadline and the partial-envelope path fires
// for rows that should have errored cleanly at 5 s. Production
// signature from 2026-05-19: "regime fan-out exceeded handler
// deadline" for three rows that historically returned cleanly.
func TestBoundedSnapshot_ReturnsWithinBudgetUnderInnerBlock(t *testing.T) {
	t.Parallel()

	// A snapshot dep that blocks forever — simulates the inner code
	// not honouring ctx. The bounded wrapper must STILL return at
	// budget + 1 s slack, with zero values.
	block := make(chan struct{})
	defer close(block)
	deps := &regimeDeps{
		snapshot: func(_ context.Context, _ string, _ time.Duration) (float64, float64, string) {
			<-block
			return 99.99, 99.99, rpc.MarketDataLive
		},
		snapshotWith52WHigh: func(_ context.Context, _ string, _ time.Duration) (float64, float64, float64, string) {
			<-block
			return 99.99, 99.99, 99.99, rpc.MarketDataLive
		},
	}

	start := time.Now()
	price, prev, dt := boundedSnapshot(context.Background(), deps, "VIX", 100*time.Millisecond)
	elapsed := time.Since(start)

	// budget 100ms + 1s slack = 1.1s; assert <1.5s to leave room for
	// scheduler jitter without being so loose the test loses its
	// meaning.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("boundedSnapshot took %v with inner block; want <1.5s (the helper must bound regardless of inner-code behaviour)", elapsed)
	}
	// Zero values returned when budget fires before the inner returns.
	if price != 0 || prev != 0 || dt != "" {
		t.Errorf("boundedSnapshot returned (%v, %v, %q); want zeros on budget timeout", price, prev, dt)
	}

	// Same contract for snapshotWith52WHigh.
	start = time.Now()
	p2, pc2, w52, dt2 := boundedSnapshotWith52WHigh(context.Background(), deps, "SPY", 100*time.Millisecond)
	elapsed = time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Errorf("boundedSnapshotWith52WHigh took %v with inner block; want <1.5s", elapsed)
	}
	if p2 != 0 || pc2 != 0 || w52 != 0 || dt2 != "" {
		t.Errorf("boundedSnapshotWith52WHigh returned (%v, %v, %v, %q); want zeros on budget timeout", p2, pc2, w52, dt2)
	}
}

// TestBoundedSnapshot_PassesThroughOnFastReturn confirms the helper
// adds no observable latency or value mutation when the inner snapshot
// returns inside the budget — guards against accidentally introducing
// budget-slack lag on the warm path.
func TestBoundedSnapshot_PassesThroughOnFastReturn(t *testing.T) {
	t.Parallel()
	deps := &regimeDeps{
		snapshot: func(_ context.Context, sym string, _ time.Duration) (float64, float64, string) {
			if sym == "VIX" {
				return 18.5, 19.2, rpc.MarketDataLive
			}
			return 0, 0, ""
		},
	}
	start := time.Now()
	price, prev, dt := boundedSnapshot(context.Background(), deps, "VIX", 5*time.Second)
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("fast snapshot took %v; want <100ms (the helper must not introduce slack on the warm path)", elapsed)
	}
	if price != 18.5 || prev != 19.2 || dt != rpc.MarketDataLive {
		t.Errorf("boundedSnapshot returned (%v, %v, %q); want (18.5, 19.2, %q)", price, prev, dt, rpc.MarketDataLive)
	}
}

// Status=error and an explanatory ErrorMessage.
//
// This is the gate the v0.27.5 test suite was missing — Rule 12
// covered "individual indicator drops" but didn't drive the
// orchestration layer where wg.Wait used to block.
func TestRunRegimeFanout_ReturnsOnCtxDoneWithPartialEnvelope(t *testing.T) {
	// HYG is the stuck fetcher; it blocks until the test closes the
	// channel. The other four return immediately so they should land
	// before the 200 ms deadline.
	block := make(chan struct{})
	defer close(block) // unblock the stuck goroutine on test exit so it doesn't leak

	stuckHYG := func(_ context.Context) rpc.RegimeHYGSPYDivergence {
		<-block
		return rpc.RegimeHYGSPYDivergence{Status: rpc.RegimeStatusOK}
	}
	fastVIX := func(_ context.Context) rpc.RegimeVIXTerm {
		return rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK}
	}
	fastUSDJPY := func(_ context.Context) rpc.RegimeUSDJPY {
		return rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, Symbol: "USD.JPY"}
	}
	fastGamma := func(_ context.Context) rpc.RegimeGammaZero {
		return rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK}
	}
	fastBreadth := func(_ context.Context) rpc.RegimeBreadth {
		return rpc.RegimeBreadth{Status: rpc.RegimeStatusOK}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := runRegimeFanout(ctx, fastVIX, stuckHYG, fastUSDJPY, fastGamma, fastBreadth)
	elapsed := time.Since(start)

	// The whole point of the v0.27.6 fix: the handler returns when its
	// ctx fires, not when the stuck fetcher unblocks. 400 ms is a
	// generous ceiling (200 ms ctx + slack); pre-v0.27.6 this would
	// have blocked indefinitely on wg.Wait.
	if elapsed > 400*time.Millisecond {
		t.Errorf("runRegimeFanout returned after %v; want <400ms (must respect ctx deadline, not wait for stuck fetcher)", elapsed)
	}

	// The stuck row must surface as Status=error with the partial-envelope
	// message, NOT as a zero-valued struct (which would let the renderer
	// mistake "stuck" for "no data ever requested").
	if res.HYGSPYDivergence.Status != rpc.RegimeStatusError {
		t.Errorf("HYG row Status=%q, want error (stuck fetcher must surface as error after ctx fires)", res.HYGSPYDivergence.Status)
	}
	if !strings.Contains(res.HYGSPYDivergence.ErrorMessage, "fan-out exceeded handler deadline") {
		t.Errorf("HYG row ErrorMessage=%q, want substring 'fan-out exceeded handler deadline'", res.HYGSPYDivergence.ErrorMessage)
	}
	if res.HYGSPYDivergence.Notes == "" {
		t.Errorf("HYG row Notes must be populated even on partial-envelope path so renderer has spec context")
	}

	// The four fast fetchers landed before the deadline; their status
	// must come through unchanged.
	if res.VIXTermStructure.Status != rpc.RegimeStatusOK {
		t.Errorf("VIX row Status=%q, want ok (fetcher returned before deadline)", res.VIXTermStructure.Status)
	}
	if res.USDJPY.Status != rpc.RegimeStatusOK {
		t.Errorf("USD.JPY row Status=%q, want ok", res.USDJPY.Status)
	}
	if res.GammaZero.Status != rpc.RegimeStatusOK {
		t.Errorf("gamma row Status=%q, want ok", res.GammaZero.Status)
	}
	if res.Breadth.Status != rpc.RegimeStatusOK {
		t.Errorf("breadth row Status=%q, want ok", res.Breadth.Status)
	}
}
