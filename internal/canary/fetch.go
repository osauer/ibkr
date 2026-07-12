package canary

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// FetchCanary reads the three existing snapshots needed by ComputeCanary.
// dial.Conn serializes calls internally, so this stays sequential and avoids
// hidden socket contention in scheduled MCP runs.
func FetchCanary(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (CanaryResult, error) {
	res, _, err := FetchCanarySnapshot(ctx, conn)
	return res, err
}

func FetchCanarySnapshot(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (CanaryResult, rpc.PositionsResult, error) {
	res, positions, _, err := FetchCanarySnapshotWithRegime(ctx, conn)
	return res, positions, err
}

func FetchCanarySnapshotWithRegime(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (CanaryResult, rpc.PositionsResult, rpc.RegimeSnapshotResult, error) {
	var acct rpc.AccountResult
	if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
		return CanaryResult{}, rpc.PositionsResult{}, rpc.RegimeSnapshotResult{}, fmt.Errorf("account: %w", err)
	}
	var pos rpc.PositionsResult
	if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &pos); err != nil {
		return CanaryResult{}, rpc.PositionsResult{}, rpc.RegimeSnapshotResult{}, fmt.Errorf("positions: %w", err)
	}
	var regime rpc.RegimeSnapshotResult
	if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &regime); err != nil {
		return CanaryResult{}, rpc.PositionsResult{}, rpc.RegimeSnapshotResult{}, fmt.Errorf("regime: %w", err)
	}
	marketEvents := fetchCanaryMarketEvents(ctx, conn, pos)
	if acct.DailyPnL == nil {
		var refreshed rpc.AccountResult
		if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &refreshed); err == nil && refreshed.DailyPnL != nil {
			acct = refreshed
		}
	}
	canary := ComputeCanary(CanaryInput{Account: acct, Positions: pos, Regime: regime, MarketEvents: marketEvents})
	rpc.CompactRegimeSnapshot(&regime)
	return canary, pos, regime, nil
}

func fetchCanaryMarketEvents(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, pos rpc.PositionsResult) rpc.MarketEventsResult {
	symbols := canaryMarketEventSymbols(pos)
	if len(symbols) == 0 {
		return rpc.MarketEventsResult{}
	}
	var out rpc.MarketEventsResult
	if err := conn.Call(ctx, rpc.MethodMarketEventsSnapshot, rpc.MarketEventsParams{Symbols: symbols}, &out); err != nil {
		now := time.Now().UTC()
		out = rpc.MarketEventsResult{
			Kind:          rpc.MarketEventsKind,
			SchemaVersion: rpc.MarketEventsSchemaVersion,
			AsOf:          now,
			Symbols:       symbols,
			SourceHealth: []rpc.SourceHealth{{
				Source:               "market_events",
				Status:               rpc.MarketEventStatusUnknown,
				AsOf:                 now,
				Confidence:           "low",
				FingerprintStability: rpc.FingerprintStabilitySemanticBuckets,
				Notes:                []string{err.Error()},
			}},
			WarningDetails: []rpc.DataWarning{{
				Code:     "market_events_unavailable",
				Scope:    "market_events",
				Severity: "data_quality",
				Message:  "Market-event snapshot unavailable: " + err.Error(),
				Impact:   "Held-name market-event flags remain unknown, not inactive.",
				Action:   "Retry market-events before relying on absence of halt, LULD, Reg SHO, or borrow pressure tags.",
			}},
			NotExecution: "Market-event flags are observed context and daemon safety gates; no orders are placed by ibkr.",
		}
		out.Fingerprint = rpc.BuildMarketEventsFingerprint(&out)
	}
	return out
}

func canaryMarketEventSymbols(pos rpc.PositionsResult) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(value string) {
		sym := strings.ToUpper(strings.TrimSpace(value))
		if sym == "" || seen[sym] {
			return
		}
		seen[sym] = true
		out = append(out, sym)
	}
	for _, stock := range pos.Stocks {
		add(stock.Symbol)
	}
	for _, group := range pos.ByUnderlying {
		add(group.Underlying)
		if group.Stock != nil {
			add(group.Stock.Symbol)
		}
		for _, opt := range group.Options {
			add(opt.Symbol)
		}
	}
	slices.Sort(out)
	return out
}
