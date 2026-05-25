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
	live := fs.Bool("watch", false, "quote saved symbols repeatedly until Ctrl-C")
	rate := fs.Duration("rate", time.Second, "poll interval for --watch")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	rest := fs.Args()
	symbols := watchlist.NormalizeSymbols(strings.Join(rest, ","))
	actions := boolCount(*add, *remove, *clear, *list, *live)
	if actions == 0 {
		*list = true
	}
	if actions > 1 {
		return fail(env, "watch: choose one of --add, --remove, --clear, --list, or --watch")
	}
	if (*add || *remove) && len(symbols) == 0 {
		return fail(env, "watch: symbol required with --add or --remove")
	}
	if (*clear || *list || *live) && len(symbols) > 0 && !*add && !*remove {
		return fail(env, "watch: unexpected symbol(s) with --list, --clear, or --watch")
	}
	if *live && *jsonOut {
		return fail(env, "watch: --watch and --json are mutually exclusive")
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
			return renderWatchlistQuotes(ctx, env, out, snap.Symbols)
		}
		return runWatch(ctx, env, *rate, "watch", fetchAndRender)
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

func renderWatchlistQuotes(ctx context.Context, env *Env, out io.Writer, symbols []string) int {
	results := make([]rpc.Quote, 0, len(symbols))
	var lastErr error
	for _, sym := range symbols {
		var q rpc.Quote
		params := rpc.QuoteSnapshotParams{
			Contract:  rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: "USD"},
			TimeoutMs: int((5 * time.Second).Milliseconds()),
		}
		if err := env.Conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
			lastErr = err
			if isGatewayUnavailable(err.Error()) {
				return fail(env, "watch: %v", err)
			}
			fmt.Fprintf(env.Stderr, "ibkr: watch quote %s: %v\n", sym, err)
			continue
		}
		results = append(results, q)
	}
	if len(results) == 0 && lastErr != nil {
		return fail(env, "watch: %v", lastErr)
	}
	wenv := *env
	wenv.Stdout = out
	return renderQuoteSnapshotText(&wenv, results)
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
