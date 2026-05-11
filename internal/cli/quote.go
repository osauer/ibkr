package cli

import (
	"context"
	"encoding/json"
	"errors"
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
	// Numeric headers right-align over their right-aligned data columns;
	// text headers left-align. Built from the same widths as the data row
	// so any future width tweak only touches one verb instead of a hand-
	// spaced label string.
	//
	// PREV CLOSE / CHG / CHG% sit after LAST so the reader's eye lands
	// on the price first then the move — same left-to-right priority you
	// see on every retail platform. Em-dash placeholders preserve column
	// alignment when ticks haven't arrived yet (frozen, dead pre-market).
	fmt.Fprintf(out, "  %-9s  %10s  %-6s  %10s  %-6s  %10s  %10s  %8s  %8s  %-7s  %7s  %s\n",
		"SYMBOL", "BID", "BID_SZ", "ASK", "ASK_SZ", "LAST", "PREV CLOSE", "CHG", "CHG%", "VOLUME", "IV", "DATA")
	for _, q := range qs {
		// Tint the data-type column yellow when not live so a row of
		// frozen/delayed quotes is obvious at a glance — same policy as
		// the table-header badge.
		dt := q.DataType
		if dt != "" && dt != "live" {
			dt = env.yellow(dt)
		}
		fmt.Fprintf(out, "  %-9s  %s  %-6s  %s  %-6s  %s  %s  %s  %s  %-7s  %s  %s\n",
			q.Symbol,
			orDash(q.Bid, 10),
			formatSize(q.BidSize),
			orDash(q.Ask, 10),
			formatSize(q.AskSize),
			orDash(q.Last, 10),
			orDash(q.PrevClose, 10),
			env.formatChange(q.Change, 8),
			env.formatChangePct(q.ChangePct, 8),
			formatSize(q.Volume),
			ivStatus(q.IV),
			dt,
		)
	}
	fmt.Fprintln(out)
	return 0
}

// formatChange renders a signed dollar change right-aligned to width w,
// colored green/red by sign (dim em-dash when nil). Padding lives outside
// the ANSI wrap so the column width matches whether or not color is on.
func (e *Env) formatChange(p *float64, w int) string {
	if p == nil {
		return padDash(w)
	}
	s := fmt.Sprintf("%+*.2f", w, *p)
	switch {
	case *p > 0:
		return e.green(s)
	case *p < 0:
		return e.red(s)
	default:
		return e.dim(s)
	}
}

// formatChangePct renders a signed percentage right-aligned to width w
// (the % sign counts inside the width), colored by sign. Same nil → em-
// dash policy as formatChange so both columns disappear together.
func (e *Env) formatChangePct(p *float64, w int) string {
	if p == nil {
		return padDash(w)
	}
	s := fmt.Sprintf("%+*.2f%%", w-1, *p)
	switch {
	case *p > 0:
		return e.green(s)
	case *p < 0:
		return e.red(s)
	default:
		return e.dim(s)
	}
}

// watchDataTypeBanner renders the per-stream data-type hint shown above
// the next tick row. Frozen mode is the load-bearing case: the gateway
// only delivers one snapshot, so we tell the user explicitly instead of
// leaving them watching a dead stream. Live mode renders nothing
// extra — the badge would be noise on the happy path. Tinted yellow
// when color is enabled so the banner stands out from the tick rows.
func (e *Env) watchDataTypeBanner(dt string) string {
	switch dt {
	case "frozen":
		return e.yellow("data=frozen ⚠  · markets closed; only the last-recorded quote is available — no further updates expected")
	case "delayed-frozen":
		return e.yellow("data=delayed-frozen ⚠  · markets closed; showing yesterday's close — no further updates expected")
	case "delayed":
		return e.yellow("data=delayed ⚠  · 15-20 min delayed quotes (entitlement-limited)")
	case "live":
		return ""
	default:
		return ""
	}
}

// ivStatus renders implied volatility as a fixed-width 7-column string
// so the IV column lines up regardless of whether IBKR delivered a value.
// Width matches the snapshot table header (`%7s "IV"`); the percent sign
// already takes one column inside the width.
func ivStatus(iv *float64) string {
	if iv == nil {
		return padDash(7)
	}
	return fmt.Sprintf("%6.1f%%", *iv*100)
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
	if !jsonOut {
		fmt.Fprintln(env.Stdout)
		fmt.Fprintf(env.Stdout, "%s · streaming · render every %s · Ctrl-C to stop\n", sym, rate)
		fmt.Fprintf(env.Stdout, "  %-8s  %10s  %-6s  %10s  %-6s  %10s\n",
			"TIME", "BID", "BID_SZ", "ASK", "ASK_SZ", "LAST")
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	frames := make(chan rpc.Frame, 16)
	done := make(chan error, 1)

	go func() {
		err := env.Conn.Stream(streamCtx, rpc.MethodQuoteSubscribe, params, func(raw json.RawMessage) error {
			var f rpc.Frame
			if err := json.Unmarshal(raw, &f); err != nil {
				return err
			}
			select {
			case frames <- f:
			case <-streamCtx.Done():
			}
			return nil
		})
		done <- err
		close(frames)
	}()

	err := runQuoteRenderer(env, frames, done, rate, jsonOut)
	if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		return fail(env, "quote --watch: %v", err)
	}
	return 0
}

// runQuoteRenderer owns all rendering state on the calling goroutine; the
// caller delivers frames over a channel and closes the channel after the
// stream goroutine has deposited its error in done. Closing frames (not
// done) is the EOF signal so any buffered frames are rendered before exit.
// Single ownership eliminates the data race the previous shared-state
// design carried, and the ticker is selected on directly so it can't leak
// after Stop.
//
// DataType handling: prints a one-line banner the first time we learn the
// gateway's data type AND on every subsequent change. After-hours, the
// gateway reports `frozen` (a single static snapshot — no streaming, ever
// — per IBKR docs), so the renderer auto-exits cleanly after rendering
// that snapshot rather than leaving the user staring at a dead stream.
// Pre-fix: silent hang. Commit 4-A: clear banner. Commit 4-B (here):
// banner + clean auto-exit so the user doesn't have to Ctrl+C an
// already-finished session.
//
// Error frames (rpc.Frame.Error != nil) are rendered as a final structured
// message and trigger autoExit. They are the terminal frame on the stream:
// the daemon will not send anything after.
func runQuoteRenderer(env *Env, frames <-chan rpc.Frame, done <-chan error, rate time.Duration, jsonOut bool) error {
	var pending *rpc.Frame
	last := time.Time{}
	var lastDataType string
	// prevLast tracks the most recently rendered Last across all flushes so
	// successive ticks can be colored by direction (green up, red down,
	// dim unchanged). We only paint Last because bid/ask churn constantly
	// on most names — coloring those would be flicker, not signal.
	var prevLast *float64
	autoExit := false

	flush := func() {
		if pending == nil {
			return
		}
		if pending.Error != nil {
			if jsonOut {
				_ = printJSON(env, pending)
			} else {
				fmt.Fprintln(env.Stdout)
				fmt.Fprintf(env.Stdout, "  stream ended — %s: %s\n", pending.Error.Code, pending.Error.Message)
			}
			autoExit = true
			pending = nil
			last = time.Now()
			return
		}
		if jsonOut {
			_ = printJSON(env, pending)
		} else {
			if pending.DataType != "" && pending.DataType != lastDataType {
				fmt.Fprintln(env.Stdout, "  "+env.watchDataTypeBanner(pending.DataType))
				lastDataType = pending.DataType
			}
			lastStr := orDash(pending.Last, 10)
			if env.Color && pending.Last != nil && prevLast != nil {
				switch {
				case *pending.Last > *prevLast:
					lastStr = env.green(lastStr)
				case *pending.Last < *prevLast:
					lastStr = env.red(lastStr)
				default:
					lastStr = env.dim(lastStr)
				}
			}
			fmt.Fprintf(env.Stdout, "  %-8s  %s  %-6s  %s  %-6s  %s\n",
				pending.T.Format("15:04:05"),
				orDash(pending.Bid, 10),
				formatSize(pending.BidSize),
				orDash(pending.Ask, 10),
				formatSize(pending.AskSize),
				lastStr)
			if pending.Last != nil {
				v := *pending.Last
				prevLast = &v
			}
		}
		// Frozen / delayed-frozen are snapshot-only on the IBKR side: the
		// gateway sends one tick and goes silent. Render that snapshot
		// (above), then signal the loop to exit so the user gets a clean
		// session end instead of a "Ctrl-C to stop" hint that does
		// nothing useful.
		if pending.DataType == "frozen" || pending.DataType == "delayed-frozen" {
			autoExit = true
		}
		pending = nil
		last = time.Now()
	}

	var tickCh <-chan time.Time
	if rate > 0 {
		t := time.NewTicker(rate)
		defer t.Stop()
		tickCh = t.C
	}

	for {
		select {
		case f, ok := <-frames:
			if !ok {
				flush()
				return <-done
			}
			pending = &f
			if rate == 0 {
				flush()
			}
		case <-tickCh:
			if pending != nil && time.Since(last) >= rate {
				flush()
			}
		}
		if autoExit {
			if !jsonOut {
				fmt.Fprintln(env.Stdout)
				fmt.Fprintln(env.Stdout, "  stream ended — frozen data is snapshot-only. Use `ibkr quote SYM` for one-shots.")
			}
			return nil
		}
	}
}
