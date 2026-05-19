package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/osauer/ibkr/internal/breadth/spx"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// breadthFetcher adapts the daemon's gateway connector to the
// spx.BarFetcher interface. Lives in the daemon package — not in spx
// — so the engine package stays free of any pkg/ibkr import.
//
// The connector is supplied via a thunk (connector accessor) rather
// than captured at construction. The daemon may reconnect or swap
// connectors after a gateway disconnect; reading the live pointer on
// every call keeps the adapter pointing at whichever connector is
// currently authoritative without re-instantiating it.
type breadthFetcher struct {
	getConn func() *ibkrlib.Connector
	// defaultTimeout is the per-request wait when ctx has no deadline.
	// Daily bars usually arrive within ~2 s; 30 s matches the budget
	// existing daemon code uses for ad-hoc historical fetches.
	defaultTimeout time.Duration
}

// newBreadthFetcher returns a spx.BarFetcher backed by getConn. nil
// from getConn maps to a "gateway unavailable" error from FetchDaily.
func newBreadthFetcher(getConn func() *ibkrlib.Connector) *breadthFetcher {
	return &breadthFetcher{
		getConn:        getConn,
		defaultTimeout: 30 * time.Second,
	}
}

// FetchDaily satisfies spx.BarFetcher. ctx is honoured for deadline
// derivation only — the connector API takes a duration, not a context.
// A cancelled context returns immediately with the context error so a
// shutdown signal doesn't have to wait for an in-flight gateway
// request to complete.
func (f *breadthFetcher) FetchDaily(ctx context.Context, symbol string, lookbackDays int) ([]spx.Bar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c := f.getConn()
	if c == nil {
		return nil, fmt.Errorf("breadth fetcher: no gateway connector")
	}
	timeout := f.defaultTimeout
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	raw, err := c.FetchHistoricalDailyBars(symbol, lookbackDays, timeout)
	if err != nil {
		return nil, err
	}
	out := make([]spx.Bar, 0, len(raw))
	for _, b := range raw {
		date := b.Date
		if !b.Time.IsZero() {
			date = b.Time.Format("2006-01-02")
		}
		// Skip bars with no parseable date — the engine's window
		// merge relies on date strings as monotonic keys, and an
		// empty date would silently collapse multiple bars into one
		// same-day overwrite.
		if date == "" {
			continue
		}
		out = append(out, spx.Bar{Date: date, Close: b.Close})
	}
	return out, nil
}
