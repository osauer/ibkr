package daemon

import (
	"context"
	"errors"
	"testing"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

func TestRegimeHistoryCacheFallsBackOnThinLiveBars(t *testing.T) {
	cache := newRegimeHistoryCache(t.TempDir())
	ctx := context.Background()
	good := makeBars(60, 79.5)

	calls := 0
	fetcher := func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		calls++
		switch calls {
		case 1:
			return good, nil
		default:
			return makeBars(30, 1), nil
		}
	}

	first, err := cache.fetch(ctx, "HYG", HYGLookbackDays, fetcher)
	if err != nil {
		t.Fatalf("first fetch error: %v", err)
	}
	if len(first) != len(good) {
		t.Fatalf("first fetch bars=%d, want %d", len(first), len(good))
	}

	cache.freshFor = 0
	second, err := cache.fetch(ctx, "HYG", HYGLookbackDays, fetcher)
	if err != nil {
		t.Fatalf("second fetch error: %v", err)
	}
	if len(second) != len(good) {
		t.Fatalf("second fetch bars=%d, want cached %d after thin live response", len(second), len(good))
	}
	if second[len(second)-1].Close != 79.5 {
		t.Fatalf("second close=%v, want cached good baseline", second[len(second)-1].Close)
	}
}

func TestRegimeHistoryCachePersistsFallbackAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	good := makeBars(10, 150)

	firstCache := newRegimeHistoryCache(dir)
	if _, err := firstCache.fetch(ctx, "USD.JPY", USDJPYLookbackDays, func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		return good, nil
	}); err != nil {
		t.Fatalf("seed fetch error: %v", err)
	}

	secondCache := newRegimeHistoryCache(dir)
	got, err := secondCache.fetch(ctx, "USD.JPY", USDJPYLookbackDays, func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		return nil, errors.New("hmds unavailable")
	})
	if err != nil {
		t.Fatalf("fallback fetch error: %v", err)
	}
	if len(got) != len(good) {
		t.Fatalf("bars=%d, want persisted cached %d", len(got), len(good))
	}
}
