package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func runQuote(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "quote")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	watch := fs.Bool("watch", false, "stream ticks until Ctrl-C")
	rate := fs.Duration("rate", 250*time.Millisecond, "render throttle window for --watch (0 = every tick)")
	timeout := fs.Duration("timeout", 5*time.Second, "snapshot timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fail(env, "quote: at least one symbol required")
	}

	// Two surface forms:
	//   ibkr quote AAPL,MSFT[,...]            → list of stock snapshots
	//   ibkr quote AAPL YYMMDD C|P STRIKE     → single option snapshot
	if len(rest) == 4 {
		return runQuoteOption(ctx, env, rest, *jsonOut, *watch, *rate, *timeout)
	}
	if len(rest) > 1 {
		return fail(env, "quote: unexpected positional args; use comma-separated symbols")
	}

	symbols := splitSymbols(rest[0])
	if *watch {
		if len(symbols) != 1 {
			return fail(env, "quote --watch: only one symbol may be streamed at a time")
		}
		return runQuoteWatch(ctx, env, symbols[0], *jsonOut, *rate)
	}
	return runQuoteSnapshotList(ctx, env, symbols, *jsonOut, *timeout)
}

func splitSymbols(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToUpper(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runQuoteSnapshotList(ctx context.Context, env *Env, syms []string, jsonOut bool, timeout time.Duration) int {
	results := make([]rpc.Quote, 0, len(syms))
	var lastErr error
	for _, sym := range syms {
		var q rpc.Quote
		params := rpc.QuoteSnapshotParams{
			Contract:  rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: "USD"},
			TimeoutMs: int(timeout.Milliseconds()),
		}
		if err := env.Conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
			lastErr = err
			// Gateway-down errors apply to every symbol — abort the loop so we
			// don't burn timeout × N waiting for the same failure each call.
			if isGatewayUnavailable(err.Error()) {
				return fail(env, "quote: %v", err)
			}
			if !jsonOut {
				fmt.Fprintf(env.Stderr, "ibkr: quote %s: %v\n", sym, err)
			}
			continue
		}
		results = append(results, q)
	}
	if len(results) == 0 && lastErr != nil {
		return fail(env, "quote: %v", lastErr)
	}
	if jsonOut {
		if len(syms) == 1 && len(results) == 1 {
			return printJSON(env, results[0])
		}
		return printJSON(env, results)
	}
	return renderQuoteSnapshotText(env, results)
}

func renderQuoteSnapshotText(env *Env, qs []rpc.Quote) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  SYMBOL     BID         BID_SZ  ASK         ASK_SZ  LAST        VOLUME   IV       DATA")
	for _, q := range qs {
		fmt.Fprintf(out, "  %-9s  %-10s  %-6s  %-10s  %-6s  %-10s  %-7s  %-7s  %s\n",
			q.Symbol,
			orDash(q.Bid, "%10.2f"),
			formatSize(q.BidSize),
			orDash(q.Ask, "%10.2f"),
			formatSize(q.AskSize),
			orDash(q.Last, "%10.2f"),
			formatSize(q.Volume),
			ivStatus(q.IV, q.IVStatus),
			q.DataType,
		)
	}
	fmt.Fprintln(out)
	return 0
}

func ivStatus(iv *float64, status string) string {
	if iv == nil {
		if status == "real" {
			return "  —"
		}
		return "  —"
	}
	return fmt.Sprintf("%5.1f%%", *iv*100)
}

func runQuoteOption(ctx context.Context, env *Env, rest []string, jsonOut, watch bool, rate, timeout time.Duration) int {
	symbol := strings.ToUpper(rest[0])
	expiry := strings.TrimSpace(rest[1]) // YYMMDD
	right := strings.ToUpper(rest[2])    // C | P
	strikeStr := strings.TrimSpace(rest[3])
	strike, err := strconv.ParseFloat(strikeStr, 64)
	if err != nil {
		return fail(env, "quote: invalid strike %q", strikeStr)
	}
	if right != "C" && right != "P" {
		return fail(env, "quote: option side must be C or P")
	}
	if len(expiry) != 6 {
		return fail(env, "quote: expiry must be YYMMDD")
	}
	full := fmt.Sprintf("%s_%s%s%.0f", symbol, expiry, right, strike)
	if watch {
		return runQuoteWatch(ctx, env, full, jsonOut, rate)
	}
	return runQuoteSnapshotList(ctx, env, []string{full}, jsonOut, timeout)
}

func runQuoteWatch(ctx context.Context, env *Env, sym string, jsonOut bool, rate time.Duration) int {
	params := rpc.QuoteSubscribeParams{
		Contract: rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: "USD"},
	}
	out := env.Stdout
	if !jsonOut {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "%s · streaming · render every %s · Ctrl-C to stop\n", sym, rate)
		fmt.Fprintln(out, "  TIME       BID         BID_SZ  ASK         ASK_SZ  LAST")
	}

	last := time.Time{}
	var pending *rpc.Frame
	tick := time.NewTicker(rateOrMin(rate))
	defer tick.Stop()

	flush := func() {
		if pending == nil {
			return
		}
		if jsonOut {
			_ = printJSON(env, pending)
		} else {
			fmt.Fprintf(out, "  %-8s  %-10s  %-6s  %-10s  %-6s  %-10s\n",
				pending.T.Format("15:04:05"),
				orDash(pending.Bid, "%10.2f"),
				formatSize(pending.BidSize),
				orDash(pending.Ask, "%10.2f"),
				formatSize(pending.AskSize),
				orDash(pending.Last, "%10.2f"))
		}
		pending = nil
		last = time.Now()
	}

	go func() {
		for range tick.C {
			if rate > 0 && time.Since(last) >= rate {
				flush()
			}
		}
	}()

	err := env.Conn.Stream(ctx, rpc.MethodQuoteSubscribe, params, func(raw json.RawMessage) error {
		var f rpc.Frame
		if err := json.Unmarshal(raw, &f); err != nil {
			return err
		}
		pending = &f
		if rate == 0 {
			flush()
		}
		return nil
	})
	flush()
	if err != nil && err != context.Canceled && !strings.Contains(err.Error(), "context canceled") {
		return fail(env, "quote --watch: %v", err)
	}
	return 0
}

func rateOrMin(d time.Duration) time.Duration {
	if d <= 0 {
		return 50 * time.Millisecond
	}
	return d
}
