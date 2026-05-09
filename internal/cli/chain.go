package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/ibkr/internal/rpc"
)

func runChain(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "chain")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	expiry := fs.String("expiry", "", "expiry YYYY-MM-DD (omit to list available expiries)")
	width := fs.Int("width", 5, "ATM ± width strikes")
	side := fs.String("side", "both", "calls | puts | both")
	withIV := fs.Bool("with-iv", false, "include ATM IV per expiry (slow, requires --expiry empty)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail(env, "chain: usage: ibkr chain SYM [--expiry YYYY-MM-DD]")
	}
	symbol := strings.ToUpper(rest[0])

	// No --expiry: list available expiries (optionally with ATM IV).
	if *expiry == "" {
		params := rpc.ChainExpiriesParams{Symbol: symbol, WithIV: *withIV}
		var res rpc.ChainExpiriesResult
		if err := env.Conn.Call(ctx, rpc.MethodChainExpiries, params, &res); err != nil {
			return fail(env, "chain: %v", err)
		}
		if *jsonOut {
			return printJSON(env, res)
		}
		return renderChainExpiriesText(env, &res, *withIV)
	}

	if *withIV {
		return fail(env, "chain: --with-iv only applies when --expiry is omitted")
	}

	params := rpc.ChainFetchParams{
		Symbol: symbol,
		Expiry: *expiry,
		Width:  *width,
		Side:   strings.ToLower(*side),
	}
	var res rpc.ChainResult
	if err := env.Conn.Call(ctx, rpc.MethodChainFetch, params, &res); err != nil {
		return fail(env, "chain: %v", err)
	}

	if *jsonOut {
		return printJSON(env, res)
	}
	return renderChainText(env, &res)
}

func renderChainText(env *Env, c *rpc.ChainResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s  spot %s  ·  expiry %s  ·  %d DTE%s\n",
		c.Symbol, formatMoney(c.Spot), c.Expiry, c.DTE, suffixBadge(c.DataType))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "                  CALLS                                              PUTS")
	fmt.Fprintln(out, "   BID     ASK    LAST     IV          STRIKE          BID     ASK    LAST     IV")
	for _, s := range c.Strikes {
		marker := ""
		if s.IsATM {
			marker = " ← ATM"
		}
		fmt.Fprintf(out, "  %-6s %-6s %-6s %-6s   %8.2f   %-6s %-6s %-6s %-6s%s\n",
			fmt2(s.CallBid), fmt2(s.CallAsk), fmt2(s.CallLast), fmtPct(s.CallIV),
			s.Strike,
			fmt2(s.PutBid), fmt2(s.PutAsk), fmt2(s.PutLast), fmtPct(s.PutIV),
			marker)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Greeks shown only when IBKR delivers tick 106. Empty cells = unavailable, never derived.")
	return 0
}

// renderChainExpiriesText prints the expiry list. Single column by default,
// two columns when --with-iv is set so users can see the ATM IV term
// structure at a glance. Empty list → guidance, not silence.
func renderChainExpiriesText(env *Env, r *rpc.ChainExpiriesResult, withIV bool) int {
	out := env.Stdout
	fmt.Fprintln(out)
	if len(r.Expiries) == 0 {
		fmt.Fprintf(out, "%s  no option expiries available\n", r.Symbol)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  Symbol may be non-optionable, or the gateway's market-data farm")
		fmt.Fprintln(out, "  is not delivering security definitions. Try `ibkr status`.")
		return 0
	}
	fmt.Fprintf(out, "%s  %d expiries available\n", r.Symbol, len(r.Expiries))
	fmt.Fprintln(out)
	if withIV {
		fmt.Fprintln(out, "  EXPIRY        ATM IV")
		for _, e := range r.Expiries {
			fmt.Fprintf(out, "  %-10s    %s\n", e.Date, fmtIVRow(e.IV, e.IVStatus))
		}
	} else {
		fmt.Fprintln(out, "  EXPIRY")
		for _, e := range r.Expiries {
			fmt.Fprintf(out, "  %s\n", e.Date)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Pick one with `ibkr chain %s --expiry YYYY-MM-DD`.\n", r.Symbol)
	return 0
}

// fmtIVRow renders a per-expiry IV cell. Status disambiguates an empty cell
// so users know whether the expiry is non-optionable, the IV fetch timed out,
// or the data farm hasn't delivered yet.
func fmtIVRow(iv *float64, status string) string {
	if iv != nil && *iv > 0 {
		return fmt.Sprintf("%5.1f%%", *iv*100)
	}
	switch status {
	case "timeout":
		return "  —    (timeout)"
	case "unavailable":
		return "  —    (unavailable)"
	default:
		return "  —"
	}
}

func fmt2(p *float64) string {
	if p == nil || *p == 0 {
		return "  —  "
	}
	return fmt.Sprintf("%6.2f", *p)
}

func fmtPct(p *float64) string {
	if p == nil || *p == 0 {
		return "  —  "
	}
	return fmt.Sprintf("%5.1f%%", *p*100)
}
