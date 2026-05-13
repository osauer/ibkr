package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"
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
		slices.SortStableFunc(rows, func(a, b rpc.PositionView) int { return cmp.Compare(b.UnrealizedPnL, a.UnrealizedPnL) })
	case "value":
		slices.SortStableFunc(rows, func(a, b rpc.PositionView) int { return cmp.Compare(b.MarketValue, a.MarketValue) })
	default:
		slices.SortStableFunc(rows, func(a, b rpc.PositionView) int { return cmp.Compare(a.Symbol, b.Symbol) })
	}
}

func renderPositionsText(env *Env, r *rpc.PositionsResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	if len(r.Stocks) == 0 && len(r.Options) == 0 {
		fmt.Fprintf(out, "No open positions%s\n\n", env.suffixBadge(r.DataType))
		return 0
	}
	// Realized P&L is only rendered when at least one row carries a non-zero
	// value — most accounts in a same-day snapshot have all zeros, and an
	// always-on column adds dead width to the table.
	showRealized := anyRealized(r.Stocks) || anyRealized(r.Options)
	if len(r.Stocks) > 0 {
		renderStocksTable(env, r.Stocks, r.DataType, showRealized)
	}
	if len(r.Options) > 0 {
		renderOptionsTable(env, r.Options, r.DataType, showRealized)
	}
	renderPortfolioSummary(env, r)
	fmt.Fprintf(out, "  %d positions  ·  as of %s\n",
		len(r.Stocks)+len(r.Options), formatTimeShort(r.AsOf))
	return 0
}

// renderStocksTable prints the stocks block with a dim column header + rule
// and right-aligned money columns. Same layout language as the by-underlying
// view so a reader switching between the two doesn't have to re-learn where
// each value lives.
func renderStocksTable(env *Env, rows []rpc.PositionView, dataType string, showRealized bool) {
	out := env.Stdout
	// Widths fit realistic data: AvgCost/Mark hold "$ 9,999.99" (10) or
	// "$ 99,999.99" (11); MarketValue holds up to "$ 9,999,999.99" (14);
	// UNREAL P&L holds signed money to the same magnitude. DAY CHG holds
	// the composite "+$X.XX (+Y.YY%)" cell — 22 cells leaves slack for
	// 3-digit percent moves on penny stocks.
	const (
		wSymbol = 9
		wQty    = 7
		wAvg    = 11
		wMark   = 11
		wDayChg = 22
		wMkt    = 14
		wPnL    = 13
	)
	fmt.Fprintf(out, "Stocks & ETFs%s\n", env.suffixBadge(dataType))
	header := fmt.Sprintf("  %-*s  %*s  %*s  %*s  %-*s  %*s  %*s",
		wSymbol, "SYMBOL", wQty, "QTY", wAvg, "AVG COST", wMark, "MARK",
		wDayChg, "DAY CHG", wMkt, "MKT VALUE", wPnL, "UNREAL P&L")
	if showRealized {
		header += fmt.Sprintf("  %*s", wPnL, "REAL P&L")
	}
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, p := range rows {
		row := fmt.Sprintf("  %-*s  %*.0f  %s  %s  %s  %s  %s",
			wSymbol, p.Symbol, wQty, p.Quantity,
			padLeftVisible(formatMoney(avgCostPerShare(p)), wAvg),
			padLeftVisible(formatMoney(p.Mark), wMark),
			padRightVisible(env.formatDayChange(p.DayChange, p.DayChangePct, 0), wDayChg),
			padLeftVisible(formatMoney(p.MarketValue), wMkt),
			env.formatPnLRight(p.UnrealizedPnL, wPnL))
		if showRealized {
			row += "  " + env.formatPnLRight(p.RealizedPnL, wPnL)
		}
		fmt.Fprintln(out, row)
	}
	fmt.Fprintln(out)
}

// renderOptionsTable prints the options block in the same column language
// as renderStocksTable. Strike is a 2-decimal float column, right-aligned;
// AvgCost/Mark/UnrealPnL hold money right-aligned so decimal points line up
// even when magnitudes vary (single-digit premium vs four-digit underlying).
func renderOptionsTable(env *Env, rows []rpc.PositionView, dataType string, showRealized bool) {
	out := env.Stdout
	const (
		wUnder  = 10
		wSide   = 4
		wExpiry = 10
		wStrike = 8
		wQty    = 5
		wAvg    = 11
		wMark   = 11
		wPnL    = 13
	)
	fmt.Fprintf(out, "Options%s\n", env.suffixBadge(dataType))
	header := fmt.Sprintf("  %-*s  %-*s  %-*s  %*s  %*s  %*s  %*s  %*s",
		wUnder, "UNDERLYING", wSide, "SIDE", wExpiry, "EXPIRY",
		wStrike, "STRIKE", wQty, "QTY",
		wAvg, "AVG COST", wMark, "MARK", wPnL, "UNREAL P&L")
	if showRealized {
		header += fmt.Sprintf("  %*s", wPnL, "REAL P&L")
	}
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, p := range rows {
		row := fmt.Sprintf("  %-*s  %-*s  %-*s  %*.2f  %*.0f  %s  %s  %s",
			wUnder, p.Symbol, wSide, p.Right, wExpiry, formatExpiry(p.Expiry),
			wStrike, p.Strike, wQty, p.Quantity,
			padLeftVisible(formatMoney(avgCostPerShare(p)), wAvg),
			padLeftVisible(formatMoney(p.Mark), wMark),
			env.formatPnLRight(p.UnrealizedPnL, wPnL))
		if showRealized {
			row += "  " + env.formatPnLRight(p.RealizedPnL, wPnL)
		}
		fmt.Fprintln(out, row)
	}
	fmt.Fprintln(out)
}

// renderPortfolioSummary prints the daemon-computed aggregate block when
// at least one component is populated. Empty when there are no options
// (Greeks coverage zero AND no FX rollup) so single-stock accounts don't
// see an empty header. Lines render only when their pointer is non-nil
// — never zero-substituted.
func renderPortfolioSummary(env *Env, r *rpc.PositionsResult) {
	if r.Portfolio == nil {
		return
	}
	p := r.Portfolio
	hasGreeks := p.EffectiveDelta != nil || p.DailyTheta != nil || p.Gamma != nil || p.Vega != nil
	hasFX := p.FXSensitivityPerPct != nil
	if !hasGreeks && !hasFX {
		return
	}
	out := env.Stdout
	fmt.Fprintln(out, env.bold("Portfolio"))
	// All numeric values right-align to col (labelStart + labelWidth +
	// space + valueWidth) so the unit text (share-equivalents, USD,
	// / day, etc.) lands at a single column regardless of value
	// magnitude. labelWidth covers the widest label ("FX sensitivity /
	// 1%" = 19); valueWidth covers a 5-digit gamma with commas and a
	// sign (e.g. "+12,584.6938" = 12).
	const labelWidth = 22
	const valueWidth = 14
	rightPad := func(s string, w int) string {
		if pad := w - len(s); pad > 0 {
			return strings.Repeat(" ", pad) + s
		}
		return s
	}
	if p.EffectiveDelta != nil {
		val := rightPad(formatSignedGrouped(*p.EffectiveDelta, 1), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s  share-equivalents\n", labelWidth, "Effective delta", env.bold(val))
	}
	if p.DollarDelta != nil {
		ccy := p.DollarDeltaCurrency
		if ccy == "" {
			ccy = "?"
		}
		val := rightPad(formatMoneyBare(*p.DollarDelta), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s  %s\n", labelWidth, "Dollar delta", env.bold(val), ccy)
	}
	if p.DailyTheta != nil {
		fmt.Fprintf(out, "  %-*s  %s  / day\n", labelWidth, "Daily theta", env.formatPnLRight(*p.DailyTheta, valueWidth))
	}
	if p.Gamma != nil {
		val := rightPad(formatSignedGrouped(*p.Gamma, 4), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s\n", labelWidth, "Gamma", val)
	}
	if p.Vega != nil {
		val := rightPad(formatSignedGrouped(*p.Vega, 2), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s  / 1 vol pt\n", labelWidth, "Vega", val)
	}
	if p.GreeksTotal > 0 {
		cov := rightPad(fmt.Sprintf("%d / %d", p.GreeksCoverage, p.GreeksTotal), valueWidth)
		if p.GreeksCoverage < p.GreeksTotal {
			fmt.Fprintf(out, "  %-*s  %s  legs (partial — model abstained or OOH)\n",
				labelWidth, "Greeks coverage", cov)
		} else {
			fmt.Fprintf(out, "  %-*s  %s  legs  %s\n",
				labelWidth, "Greeks coverage", cov, env.green("✓"))
		}
	}
	if p.FXSensitivityPerPct != nil {
		base := p.FXBaseCurrency
		if base == "" {
			base = "base"
		}
		val := rightPad(formatMoneyBare(*p.FXSensitivityPerPct), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s  %s per +1%% FX\n",
			labelWidth, "FX sensitivity", val, base)
	}
	fmt.Fprintln(out)
}

func anyRealized(rows []rpc.PositionView) bool {
	for _, p := range rows {
		if p.RealizedPnL != 0 {
			return true
		}
	}
	return false
}

// renderPositionsByUnderlying prints one block per underlying with the
// stock leg (if any), the option legs (with inline Greeks), and a
// per-underlying Total row when there's more than one leg.
//
// Every row uses the same column layout — LEG, QTY, AVG, MARK,
// CHANGE/GREEKS, MKT VALUE, UNREAL P&L — so the eye reads down each
// column instead of zigzagging across row-type-specific layouts. Money
// columns right-align so decimal points line up; sign-coloured cells
// (day change, unrealised P&L, Δ) pad before colour wrap so visible
// widths stay correct under ANSI escapes.
func renderPositionsByUnderlying(env *Env, r *rpc.PositionsResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	if len(r.ByUnderlying) == 0 {
		fmt.Fprintf(out, "No open positions%s\n\n", env.suffixBadge(r.DataType))
		return 0
	}
	fmt.Fprintf(out, "Positions by underlying%s\n", env.suffixBadge(r.DataType))
	fmt.Fprintln(out)

	// Column header.  Widths chosen to fit realistic data: identifier
	// holds "2026-06-18 C 1191.67" (~20); change/greeks holds the
	// compact greek tuple "Δ+0.62 Γ+0.31 Θ-0.01 ν+0.01" (27 cells) or
	// the stock day-change cell "+1.32 (+0.64%)" (~14 cells).
	const (
		wLeg    = 22
		wQty    = 9
		wAvg    = 10
		wMark   = 10
		wChange = 27
		wMkt    = 13
		wUnreal = 13
	)
	header := fmt.Sprintf("  %-*s  %*s  %*s  %*s  %-*s  %*s  %*s",
		wLeg, "LEG", wQty, "QTY", wAvg, "AVG", wMark, "MARK",
		wChange, "CHANGE / GREEKS", wMkt, "MKT VALUE", wUnreal, "UNREAL P&L")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))

	writeRow := func(leg, qty, avg, mark, change, mkt, unreal string) {
		fmt.Fprintf(out, "  %s  %s  %s  %s  %s  %s  %s\n",
			padRightVisible(leg, wLeg),
			padLeftVisible(qty, wQty),
			padLeftVisible(avg, wAvg),
			padLeftVisible(mark, wMark),
			padRightVisible(change, wChange),
			padLeftVisible(mkt, wMkt),
			padLeftVisible(unreal, wUnreal))
	}

	for _, g := range r.ByUnderlying {
		fmt.Fprintln(out, "  "+env.bold(g.Underlying))

		if g.Stock != nil {
			s := g.Stock
			writeRow(
				"Stock",
				fmt.Sprintf("%.0f sh", s.Quantity),
				formatMoney(avgCostPerShare(*s)),
				formatMoney(s.Mark),
				env.formatDayChange(s.DayChange, s.DayChangePct, 0),
				formatMoney(s.MarketValue),
				env.formatPnLRight(s.UnrealizedPnL, wUnreal))
		}
		for _, o := range g.Options {
			writeRow(
				fmt.Sprintf("%s %s %.2f", formatExpiry(o.Expiry), o.Right, o.Strike),
				fmt.Sprintf("%.0f ct", o.Quantity),
				formatMoney(avgCostPerShare(o)),
				formatMoney(o.Mark),
				env.formatGreeksLine(o),
				formatMoney(o.MarketValue),
				env.formatPnLRight(o.UnrealizedPnL, wUnreal))
		}

		// Total row only when there's more than one leg — for a single
		// stock or a single option the per-leg row already carries the
		// values that would otherwise be duplicated.
		legs := 0
		if g.Stock != nil {
			legs++
		}
		legs += len(g.Options)
		if legs > 1 {
			writeRow(
				env.dim("─── Total"),
				"", "", "", "",
				formatMoney(g.GroupMarketValue),
				env.formatPnLRight(g.GroupUnrealizedPnL, wUnreal))
		}
	}
	fmt.Fprintln(out)
	renderPortfolioSummary(env, r)
	fmt.Fprintf(out, "  %d underlyings  ·  as of %s\n",
		len(r.ByUnderlying), formatTimeShort(r.AsOf))
	return 0
}

// formatDayChange renders the combined "+$1.42 (+0.99%)" cell — both
// values painted by sign as a unit (you almost never want one of these
// in isolation). Em-dash placeholder of width w when either is nil so
// the column stays aligned. Padding lives outside the ANSI wrap so
// visible width matches w regardless of color state.
func (e *Env) formatDayChange(chg, pct *float64, w int) string {
	if chg == nil || pct == nil {
		return padDash(w)
	}
	s := fmt.Sprintf("%+.2f (%+.2f%%)", *chg, *pct)
	if pad := w - len(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	switch {
	case *chg > 0:
		return e.green(s)
	case *chg < 0:
		return e.red(s)
	default:
		return e.dim(s)
	}
}

// formatGreeksLine renders a per-leg Greeks tuple in the most compact
// form that stays readable: no space between symbol and number, single
// space between greeks, 2 decimals everywhere ("Δ+0.62 Γ+0.31 Θ-0.01
// ν+0.01" — 27 cells). Delta carries sign coloring (the headline risk
// component); gamma / theta / vega print with sign but no color so the
// eye is drawn to delta first.
//
// Greeks that the daemon didn't capture render as a dim "Δ  —" / "Γ  —"
// / "Θ  —" / "ν  —" placeholder so the column shape stays consistent
// across rows — a blank cell would otherwise read as "Greeks don't
// apply here" rather than the truth ("not yet captured, retry").
func (e *Env) formatGreeksLine(o rpc.PositionView) string {
	delta := e.dim("Δ —")
	if o.Delta != nil {
		s := fmt.Sprintf("Δ%+0.2f", *o.Delta)
		switch {
		case *o.Delta > 0:
			s = e.green(s)
		case *o.Delta < 0:
			s = e.red(s)
		}
		delta = s
	}
	gamma := e.dim("Γ —")
	if o.Gamma != nil {
		gamma = fmt.Sprintf("Γ%+0.2f", *o.Gamma)
	}
	theta := e.dim("Θ —")
	if o.Theta != nil {
		theta = fmt.Sprintf("Θ%+0.2f", *o.Theta)
	}
	vega := e.dim("ν —")
	if o.Vega != nil {
		vega = fmt.Sprintf("ν%+0.2f", *o.Vega)
	}
	return delta + " " + gamma + " " + theta + " " + vega
}

// avgCostPerShare normalises AvgCost to per-share units for visual
// comparison with Mark in the same row. IBKR's averageCost is per-share
// for stocks but per-contract (multiplier-inclusive) for options — so a
// $3.00 premium call comes off the wire as $300, which reads like a typo
// next to a $3 mark. Dividing by multiplier on OPT restores symmetry.
// JSON output stays IBKR-faithful; only the rendered column normalises.
func avgCostPerShare(p rpc.PositionView) float64 {
	if p.SecType == "OPT" && p.Multiplier > 0 {
		return p.AvgCost / float64(p.Multiplier)
	}
	return p.AvgCost
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
