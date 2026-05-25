package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	"github.com/osauer/ibkr/internal/watchlist"
)

var watchlistDefaultPath = watchlist.DefaultPath

func runWatchlist(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "watch")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	add := fs.Bool("add", false, "add symbol(s) to the local watchlist")
	remove := fs.Bool("remove", false, "remove symbol(s) from the local watchlist")
	clear := fs.Bool("clear", false, "remove every symbol from the local watchlist")
	list := fs.Bool("list", false, "list saved symbols")
	quotes := fs.Bool("quotes", false, "show saved symbols with enriched quote context")
	live := fs.Bool("watch", false, "quote saved symbols repeatedly until Ctrl-C")
	rate := fs.Duration("rate", time.Second, "poll interval for --watch")
	timeout := fs.Duration("timeout", 5*time.Second, "per-symbol quote timeout for --quotes/--watch")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	rest := fs.Args()
	symbols := watchlist.NormalizeSymbols(strings.Join(rest, ","))
	actions := boolCount(*add, *remove, *clear, *list, *quotes, *live)
	if actions == 0 {
		*list = true
	}
	if actions > 1 {
		return fail(env, "watch: choose one of --add, --remove, --clear, --list, --quotes, or --watch")
	}
	if (*add || *remove) && len(symbols) == 0 {
		return fail(env, "watch: symbol required with --add or --remove")
	}
	if (*clear || *list || *quotes || *live) && len(symbols) > 0 && !*add && !*remove {
		return fail(env, "watch: unexpected symbol(s) with --list, --clear, --quotes, or --watch")
	}
	if *live && *jsonOut {
		return fail(env, "watch: --watch and --json are mutually exclusive")
	}
	if *quotes && env.Conn == nil {
		return fail(env, "watch: --quotes requires the ibkr daemon")
	}

	store, err := openWatchlistStore()
	if err != nil {
		return fail(env, "watch: %v", err)
	}

	switch {
	case *add:
		snap, err := store.Add(symbols)
		return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
	case *remove:
		snap, err := store.Remove(symbols)
		return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
	case *clear:
		snap, err := store.Clear()
		return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
	case *live:
		if env.Conn == nil {
			return fail(env, "watch: --watch requires the ibkr daemon")
		}
		fetchAndRender := func(out io.Writer) int {
			snap, err := store.Snapshot()
			if err != nil {
				return fail(env, "watch: %v", err)
			}
			if len(snap.Symbols) == 0 {
				return renderWatchlistTextTo(env, out, snap)
			}
			return renderWatchlistQuotes(ctx, env, out, snap, *timeout, false)
		}
		return runWatch(ctx, env, *rate, "watch", fetchAndRender)
	case *quotes:
		snap, err := store.Snapshot()
		if err != nil {
			return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
		}
		if len(snap.Symbols) == 0 {
			if *jsonOut {
				return printJSON(env, rpc.WatchlistResult{Name: snap.Name, Symbols: []string{}, Rows: []rpc.WatchlistRow{}, AsOf: time.Now()})
			}
			return renderWatchlistResult(env, env.Stdout, snap, nil, *jsonOut)
		}
		return renderWatchlistQuotes(ctx, env, env.Stdout, snap, *timeout, *jsonOut)
	default:
		snap, err := store.Snapshot()
		return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
	}
}

func openWatchlistStore() (*watchlist.Store, error) {
	path, err := watchlistDefaultPath()
	if err != nil {
		return nil, err
	}
	return watchlist.New(path), nil
}

func renderWatchlistResult(env *Env, out io.Writer, snap *watchlist.Snapshot, err error, jsonOut bool) int {
	if err != nil {
		return fail(env, "watch: %v", err)
	}
	if jsonOut {
		return printJSONTo(env, out, snap)
	}
	return renderWatchlistTextTo(env, out, snap)
}

func renderWatchlistTextTo(env *Env, out io.Writer, snap *watchlist.Snapshot) int {
	fmt.Fprintln(out)
	if len(snap.Symbols) == 0 {
		fmt.Fprintln(out, "No symbols in watchlist")
		fmt.Fprintf(out, "  as of %s\n", formatTimeShort(snap.AsOf))
		return 0
	}
	const wSymbol = 9
	fmt.Fprintln(out, "Watchlist")
	header := fmt.Sprintf("  %-*s", wSymbol, "SYMBOL")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, sym := range snap.Symbols {
		fmt.Fprintf(out, "  %-*s\n", wSymbol, sym)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %d symbols  ·  as of %s\n", len(snap.Symbols), formatTimeShort(snap.AsOf))
	return 0
}

func renderWatchlistQuotes(ctx context.Context, env *Env, out io.Writer, snap *watchlist.Snapshot, timeout time.Duration, jsonOut bool) int {
	res, err := fetchWatchlistQuotes(ctx, env, snap, timeout)
	if err != nil {
		return fail(env, "watch: %v", err)
	}
	if jsonOut {
		return printJSONTo(env, out, res)
	}
	wenv := *env
	wenv.Stdout = out
	return renderWatchlistQuoteText(&wenv, out, res)
}

func fetchWatchlistQuotes(ctx context.Context, env *Env, snap *watchlist.Snapshot, timeout time.Duration) (*rpc.WatchlistResult, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	res := &rpc.WatchlistResult{
		Name:    snap.Name,
		Symbols: append([]string(nil), snap.Symbols...),
		Rows:    make([]rpc.WatchlistRow, 0, len(snap.Symbols)),
		AsOf:    time.Now(),
	}
	holdings := fetchWatchlistStockHoldings(ctx, env)
	for _, sym := range snap.Symbols {
		var q rpc.Quote
		params := rpc.QuoteSnapshotParams{
			Contract:  watchlistQuoteContract(sym, holdings[sym]),
			TimeoutMs: int(timeout.Milliseconds()),
		}
		row := rpc.WatchlistRow{}
		if err := env.Conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
			if isGatewayUnavailable(err.Error()) {
				return nil, err
			}
			row.Quote = rpc.Quote{Symbol: sym}
			row.Error = err.Error()
		} else {
			row.Quote = q
		}
		if h, ok := holdings[sym]; ok {
			row.Holding = h
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

func fetchWatchlistStockHoldings(ctx context.Context, env *Env) map[string]*rpc.WatchlistHolding {
	out := map[string]*rpc.WatchlistHolding{}
	if env == nil || env.Conn == nil {
		return out
	}
	var pos rpc.PositionsResult
	if err := env.Conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{Type: "stk"}, &pos); err != nil {
		return out
	}
	for _, p := range pos.Stocks {
		out[strings.ToUpper(p.Symbol)] = &rpc.WatchlistHolding{
			Quantity:      p.Quantity,
			AvgCost:       p.AvgCost,
			Mark:          p.Mark,
			MarketValue:   p.MarketValue,
			UnrealizedPnL: p.UnrealizedPnL,
			DailyPnL:      p.DailyPnL,
			Exchange:      p.Exchange,
			Currency:      p.Currency,
		}
	}
	return out
}

func watchlistQuoteContract(sym string, h *rpc.WatchlistHolding) rpc.ContractParams {
	c := rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: "USD"}
	if h == nil {
		return c
	}
	if h.Currency != "" {
		c.Currency = h.Currency
	}
	if h.Exchange != "" {
		c.Exchange = h.Exchange
	}
	return c
}

func renderWatchlistQuoteText(env *Env, out io.Writer, r *rpc.WatchlistResult) int {
	fmt.Fprintln(out)
	if r == nil || len(r.Rows) == 0 {
		fmt.Fprintln(out, "No symbols in watchlist")
		if r != nil {
			fmt.Fprintf(out, "  as of %s\n", formatTimeShort(r.AsOf))
		}
		return 0
	}
	fmt.Fprintln(out, "Watchlist")
	const (
		wSymbol = 9
		wPos    = 9
		wPrice  = 11
		wChange = 10
		wPrev   = 11
		wRange  = 21
		wVol    = 17
	)
	header := fmt.Sprintf("  %-*s  %*s  %*s  %*s  %8s  %*s  %-*s  %-*s  %-*s  %s",
		wSymbol, "SYMBOL", wPos, "POS", wPrice, "PRICE", wChange, "CHG", "CHG%",
		wPrev, "PREV", wRange, "DAY RANGE", wRange, "52W RANGE", wVol, "VOL / AVG", "DATA")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, row := range r.Rows {
		if row.Error != "" {
			fmt.Fprintf(out, "  %-*s  %s\n", wSymbol, row.Symbol, env.red(row.Error))
			continue
		}
		ccy := row.Contract.Currency
		if ccy == "" {
			ccy = "USD"
		}
		dataType := row.DataType
		if rpc.IsLiveDataType(dataType) {
			dataType = rpc.MarketDataLive
		} else {
			dataType = env.yellow(dataType)
		}
		fmt.Fprintf(out, "  %-*s  %s  %s  %s  %s  %s  %s  %s  %s  %s\n",
			wSymbol, row.Symbol,
			padLeftVisible(formatWatchlistHolding(row.Holding), wPos),
			formatPrice(row.Price, wPrice, ccy),
			formatWatchlistChange(env, row.Change, ccy, wChange),
			env.formatChangePct(row.ChangePct, 8),
			formatPrice(row.PrevClose, wPrev, ccy),
			formatWatchlistRange(env, row.DayLow, row.DayHigh, ccy, wRange),
			formatWatchlistRange(env, row.Week52Low, row.Week52High, ccy, wRange),
			formatWatchlistVolume(row.Volume, row.AvgVolume, wVol),
			dataType,
		)
		hints := watchlistQuoteHints(env, row)
		for _, hint := range hints {
			fmt.Fprintf(out, "    %s\n", hint)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %d symbols  ·  as of %s\n", len(r.Symbols), formatTimeShort(r.AsOf))
	return 0
}

func formatWatchlistHolding(h *rpc.WatchlistHolding) string {
	if h == nil || h.Quantity == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f sh", h.Quantity)
}

func formatWatchlistRange(env *Env, lo, hi *float64, ccy string, w int) string {
	sym := strings.TrimSpace(moneyPrefix(ccy))
	loS := "—"
	if lo != nil {
		loS = fmt.Sprintf("%s%.2f", sym, *lo)
	}
	hiS := "—"
	if hi != nil {
		hiS = fmt.Sprintf("%s%.2f", sym, *hi)
	}
	return env.dim(fmt.Sprintf("%*s..%-*s", (w-2)/2, loS, w-(w-2)/2-2, hiS))
}

func formatWatchlistChange(env *Env, p *float64, ccy string, w int) string {
	if p == nil {
		return padDash(w)
	}
	sym := strings.TrimSpace(moneyPrefix(ccy))
	abs := *p
	sign := "+"
	if abs < 0 {
		sign = "-"
		abs = -abs
	}
	s := fmt.Sprintf("%*s", w, fmt.Sprintf("%s%s%.2f", sign, sym, abs))
	switch {
	case *p > 0:
		return env.green(s)
	case *p < 0:
		return env.red(s)
	default:
		return env.dim(s)
	}
}

func formatWatchlistVolume(volume, avg *int64, w int) string {
	left := "—"
	if volume != nil && *volume > 0 {
		left = strings.TrimSpace(formatVolumeCompact(volume, 8))
	}
	right := "—"
	if avg != nil && *avg > 0 {
		right = strings.TrimSpace(formatVolumeCompact(avg, 8))
	}
	return fmt.Sprintf("%*s", w, left+" / "+right)
}

func watchlistQuoteHints(env *Env, row rpc.WatchlistRow) []string {
	var hints []string
	if row.PriceAsOf != "" {
		hint := row.PriceAsOf
		if row.PriceSource != "" {
			hint += " · source=" + row.PriceSource
		}
		hints = append(hints, env.dim(hint))
	}
	if row.StaleReason != "" {
		hints = append(hints, env.yellow(row.StaleReason))
	}
	if hint := quoteSessionHint(env, row.SessionContext); hint != "" {
		hints = append(hints, hint)
	}
	return hints
}

func boolCount(values ...bool) int {
	var n int
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}
