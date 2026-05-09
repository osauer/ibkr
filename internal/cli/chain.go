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
	expiry := fs.String("expiry", "", "expiry YYYY-MM-DD (required)")
	width := fs.Int("width", 5, "ATM ± width strikes")
	side := fs.String("side", "both", "calls | puts | both")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail(env, "chain: usage: ibkr chain SYM --expiry YYYY-MM-DD")
	}
	if *expiry == "" {
		return fail(env, "chain: --expiry is required")
	}

	params := rpc.ChainFetchParams{
		Symbol: strings.ToUpper(rest[0]),
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
