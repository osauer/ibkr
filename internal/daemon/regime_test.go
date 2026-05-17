package daemon

import (
	"context"
	"errors"
	"slices"
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
type fakeDeps struct {
	snapshots map[string]fakeQuote
	bars      map[string]fakeHistory
	misc      map[string]*ibkrlib.MarketData
}

type fakeQuote struct {
	price    float64
	dataType string
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
		history: func(sym string, _ int, _ time.Duration) ([]ibkrlib.HistoricalBar, error) {
			h := f.bars[sym]
			return h.bars, h.err
		},
		miscData: func(sym string) *ibkrlib.MarketData {
			return f.misc[sym]
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
				"SPY": {price: 530.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
			misc: map[string]*ibkrlib.MarketData{
				"SPY": {Week52High: 540.0},
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
				"SPY": {price: 530.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
			// no misc data → SPY52WHigh stays nil
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
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
				"SPY": {price: 530.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {err: errors.New("history fetch failed")},
			},
			misc: map[string]*ibkrlib.MarketData{
				"SPY": {Week52High: 540.0},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Errorf("status=%q, want ok", got.Status)
		}
		if !slices.Contains(got.FieldsMissing, "hyg_50dma") {
			t.Errorf("fields_missing=%v, want to include hyg_50dma", got.FieldsMissing)
		}
	})

	t.Run("spy_spot_missing", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
				// SPY missing
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Errorf("status=%q, want error when SPY spot missing", got.Status)
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
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: makeBars(3, 158.0)}, // < 8 bars
			},
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
