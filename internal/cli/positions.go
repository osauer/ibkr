package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/osauer/ibkr/internal/rpc"
)

func runPositions(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "positions")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	symbol := fs.String("symbol", "", "filter by symbol")
	typeF := fs.String("type", "", "filter by type: stk | opt")
	sortBy := fs.String("sort", "alpha", "sort: alpha | pnl | value")
	by := fs.String("by", "", "group view: underlying (default = flat stocks/options tables)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *by != "" && *by != "underlying" {
		return fail(env, "positions: --by must be 'underlying' (got %q)", *by)
	}

	params := rpc.PositionsListParams{Symbol: *symbol, Type: *typeF}
	var res rpc.PositionsResult
	if err := env.Conn.Call(ctx, rpc.MethodPositionsList, params, &res); err != nil {
		return fail(env, "positions: %v", err)
	}

	applySort(res.Stocks, *sortBy)
	applySort(res.Options, *sortBy)

	if *jsonOut {
		return printJSON(env, res)
	}
	if *by == "underlying" {
		return renderPositionsByUnderlying(env, &res)
	}
	return renderPositionsText(env, &res)
}

func applySort(rows []rpc.PositionView, by string) {
	switch by {
	case "pnl":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].UnrealizedPnL > rows[j].UnrealizedPnL })
	case "value":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].MarketValue > rows[j].MarketValue })
	default:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Symbol < rows[j].Symbol })
	}
}

func renderPositionsText(env *Env, r *rpc.PositionsResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	if len(r.Stocks) == 0 && len(r.Options) == 0 {
		fmt.Fprintf(out, "No open positions%s\n\n", suffixBadge(r.DataType))
		return 0
	}
	// Realized P&L is only rendered when at least one row carries a non-zero
	// value — most accounts in a same-day snapshot have all zeros, and an
	// always-on column adds dead width to the table.
	showRealized := anyRealized(r.Stocks) || anyRealized(r.Options)
	if len(r.Stocks) > 0 {
		fmt.Fprintf(out, "Stocks & ETFs%s\n", suffixBadge(r.DataType))
		if showRealized {
			fmt.Fprintln(out, "  SYMBOL     QTY        AVG COST       MARK         MKT VALUE       UNREAL P&L    REAL P&L")
		} else {
			fmt.Fprintln(out, "  SYMBOL     QTY        AVG COST       MARK         MKT VALUE       UNREAL P&L")
		}
		for _, p := range r.Stocks {
			if showRealized {
				fmt.Fprintf(out, "  %-9s %7.0f   %-12s %-11s %-15s %-12s  %s\n",
					p.Symbol, p.Quantity, formatMoney(p.AvgCost), formatMoney(p.Mark),
					formatMoney(p.MarketValue), formatMoney(p.UnrealizedPnL), formatMoney(p.RealizedPnL))
			} else {
				fmt.Fprintf(out, "  %-9s %7.0f   %-12s %-11s %-15s %-12s\n",
					p.Symbol, p.Quantity, formatMoney(p.AvgCost), formatMoney(p.Mark),
					formatMoney(p.MarketValue), formatMoney(p.UnrealizedPnL))
			}
		}
		fmt.Fprintln(out)
	}
	if len(r.Options) > 0 {
		fmt.Fprintf(out, "Options%s\n", suffixBadge(r.DataType))
		if showRealized {
			fmt.Fprintln(out, "  UNDERLYING  SIDE  EXPIRY      STRIKE   QTY      AVG COST     MARK         UNREAL P&L    REAL P&L")
		} else {
			fmt.Fprintln(out, "  UNDERLYING  SIDE  EXPIRY      STRIKE   QTY      AVG COST     MARK         UNREAL P&L")
		}
		for _, p := range r.Options {
			if showRealized {
				fmt.Fprintf(out, "  %-10s  %-4s  %-10s  %7.2f  %5.0f    %-12s %-11s %-12s  %s\n",
					p.Symbol, p.Right, formatExpiry(p.Expiry), p.Strike, p.Quantity,
					formatMoney(p.AvgCost), formatMoney(p.Mark), formatMoney(p.UnrealizedPnL), formatMoney(p.RealizedPnL))
			} else {
				fmt.Fprintf(out, "  %-10s  %-4s  %-10s  %7.2f  %5.0f    %-12s %-11s %-12s\n",
					p.Symbol, p.Right, formatExpiry(p.Expiry), p.Strike, p.Quantity,
					formatMoney(p.AvgCost), formatMoney(p.Mark), formatMoney(p.UnrealizedPnL))
			}
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "  %d positions  ·  as of %s\n",
		len(r.Stocks)+len(r.Options), formatTimeShort(r.AsOf))
	return 0
}

func anyRealized(rows []rpc.PositionView) bool {
	for _, p := range rows {
		if p.RealizedPnL != 0 {
			return true
		}
	}
	return false
}

// renderPositionsByUnderlying prints one block per underlying with the stock
// leg (if any) followed by the option legs and a group P&L line.
func renderPositionsByUnderlying(env *Env, r *rpc.PositionsResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	if len(r.ByUnderlying) == 0 {
		fmt.Fprintf(out, "No open positions%s\n\n", suffixBadge(r.DataType))
		return 0
	}
	fmt.Fprintf(out, "Positions by underlying%s\n", suffixBadge(r.DataType))
	for _, g := range r.ByUnderlying {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  %s\n", g.Underlying)
		if g.Stock != nil {
			s := g.Stock
			fmt.Fprintf(out, "    Stock     %7.0f sh   avg %-12s mark %-11s mkt %s   unreal %s\n",
				s.Quantity, formatMoney(s.AvgCost), formatMoney(s.Mark),
				formatMoney(s.MarketValue), formatMoney(s.UnrealizedPnL))
		}
		if len(g.Options) > 0 {
			fmt.Fprintln(out, "    Options")
			for _, o := range g.Options {
				fmt.Fprintf(out, "      %s %s %7.2f   %5.0f ct   avg %-12s mark %-11s unreal %s\n",
					formatExpiry(o.Expiry), o.Right, o.Strike, o.Quantity,
					formatMoney(o.AvgCost), formatMoney(o.Mark), formatMoney(o.UnrealizedPnL))
			}
		}
		fmt.Fprintf(out, "    Group     mkt %-15s unreal %s\n",
			formatMoney(g.GroupMarketValue), formatMoney(g.GroupUnrealizedPnL))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %d underlyings  ·  as of %s\n",
		len(r.ByUnderlying), formatTimeShort(r.AsOf))
	return 0
}

func formatExpiry(s string) string {
	// IBKR returns YYYYMMDD; render YYYY-MM-DD if length matches
	if len(s) == 8 {
		return s[:4] + "-" + s[4:6] + "-" + s[6:8]
	}
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}
