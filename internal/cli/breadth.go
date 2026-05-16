package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/ibkr/internal/rpc"
)

func runBreadth(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "breadth")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	days := fs.Int("days", 30, "trailing daily-series length (1-90)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "breadth: takes no positional args (got %v)", fs.Args())
	}
	params := rpc.BreadthSPXParams{HistoryDays: *days}
	var res rpc.BreadthSPXResult
	if err := env.Conn.Call(ctx, rpc.MethodBreadthSPX, params, &res); err != nil {
		return fail(env, "breadth: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderBreadthText(env, &res)
}

func renderBreadthText(env *Env, r *rpc.BreadthSPXResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "S&P 500 Breadth  ·  %% above 50-day SMA%s\n", env.suffixBadge(r.DataType))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Headline    %.1f %%\n", r.Value)
	if !r.SpotAt.IsZero() {
		fmt.Fprintf(out, "  Observed    %s\n", r.SpotAt.Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintf(out, "  Source      %s\n", r.Source)
	fmt.Fprintf(out, "  Method      %s\n", r.Method)

	if len(r.History) == 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  (no history bars returned)")
		fmt.Fprintln(out)
		return 0
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Sparkline   %s\n", breadthSparkline(r.History))
	fmt.Fprintln(out)

	header := fmt.Sprintf("  %-12s  %8s", "DATE", "VALUE")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, h := range r.History {
		fmt.Fprintf(out, "  %-12s  %7.1f%%\n", h.Date, h.Value)
	}
	fmt.Fprintln(out)
	return 0
}

// breadthSparkline renders the trailing series as Unicode block glyphs.
// Eight-step granularity matches the typical 0-100 % breadth range
// without inventing precision the data doesn't have.
func breadthSparkline(h []rpc.BreadthDailyValue) string {
	if len(h) == 0 {
		return ""
	}
	const glyphs = "▁▂▃▄▅▆▇█"
	runes := []rune(glyphs)
	// Map [0, 100] linearly onto the 8 glyph bins. The breadth domain is
	// fixed by construction; no min/max normalization needed.
	var b strings.Builder
	b.Grow(len(h) * 3)
	for _, v := range h {
		x := v.Value
		if x < 0 {
			x = 0
		}
		if x > 100 {
			x = 100
		}
		idx := int(x / 12.5)
		if idx >= len(runes) {
			idx = len(runes) - 1
		}
		b.WriteRune(runes[idx])
	}
	return b.String()
}
