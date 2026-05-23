package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// optionOffHoursBanner is the one-line disclosure shown by chain renderers
// when rpc.IsOptionRTH(now) reports the U.S. equity-option session is
// closed. Phrasing names the actual state, the signal source (IBKR's model-
// computation engine), what the user sees missing (bid/ask), and when the
// session resumes — mirrors the gamma command's "X/Y legs back-solved via
// Black-Scholes from prior-session prices" disclosure tone.
const optionOffHoursBanner = "Options markets closed · IV is model-computed by IBKR from prior-session prices; bid/ask resume 09:30 ET"

func runChain(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "chain")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	expiry := fs.String("expiry", "", "expiry YYYY-MM-DD (omit to list available expiries)")
	width := fs.Int("width", 5, "ATM ± width strikes")
	side := fs.String("side", "both", "calls | puts | both")
	// ATM IV per expiry is on by default now — it's the answer to "which
	// expiry should I pick" and pays its own way via the daemon-side
	// cache. --no-iv returns the fast skeleton when the user just wants
	// the list of available dates.
	noIV := fs.Bool("no-iv", false, "skip ATM IV per expiry (faster; default fetches IV)")
	allExpiries := fs.Bool("all-expiries", false, "fetch IV for every listed expiry (default: nearest 12)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail(env, "chain: usage: ibkr chain SYM [--expiry YYYY-MM-DD]")
	}
	symbol := strings.ToUpper(rest[0])

	// No --expiry: list available expiries (with ATM IV by default).
	if *expiry == "" {
		withIV := !*noIV
		params := rpc.ChainExpiriesParams{
			Symbol:      symbol,
			WithIV:      withIV,
			AllExpiries: *allExpiries,
		}
		var res rpc.ChainExpiriesResult
		if err := env.Conn.Call(ctx, rpc.MethodChainExpiries, params, &res); err != nil {
			return fail(env, "chain: %v", err)
		}
		if *jsonOut {
			return printJSON(env, res)
		}
		return renderChainExpiriesText(env, &res, withIV)
	}

	if *noIV || *allExpiries {
		return fail(env, "chain: --no-iv and --all-expiries only apply when --expiry is omitted")
	}

	// Format-validate --expiry locally so a typo like "tomorrow" or
	// "2099-99-99" fails fast instead of burning the full RPC deadline
	// against a doomed strike-fan that has no contract to fetch.
	if _, err := time.Parse("2006-01-02", *expiry); err != nil {
		return fail(env, "chain: --expiry must be YYYY-MM-DD (got %q)", *expiry)
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
		c.Symbol, formatMoney(c.Spot), c.Expiry, c.DTE, env.suffixBadge(c.DataType))
	fmt.Fprintln(out)
	// Two-line header: line 1 spans CALLS over the five call columns and
	// PUTS over the five put columns; line 2 right-aligns each label over
	// its right-aligned data column. Both lines built from the same field
	// widths as the data row so labels stay glued to the columns.
	//
	// The 34-char span covers 5 × 6-wide fields plus 4 single-space
	// separators (6+1+6+1+6+1+6+1+6 = 34); CALLS/PUTS are right-padded
	// inside that span so they sit roughly above the call/put leg blocks.
	const groupSpan = 34
	groupHeader := func(label string) string {
		pad := (groupSpan - len(label)) / 2
		return strings.Repeat(" ", pad) + label + strings.Repeat(" ", groupSpan-len(label)-pad)
	}
	groupLine := fmt.Sprintf("  %s   %s   %s", groupHeader("CALLS"), strings.Repeat(" ", 8), groupHeader("PUTS"))
	colLine := fmt.Sprintf("  %6s %6s %6s %6s %6s   %8s   %6s %6s %6s %6s %6s",
		"BID", "ASK", "LAST", "IV", "OI", "STRIKE", "BID", "ASK", "LAST", "IV", "OI")
	fmt.Fprintln(out, env.dim(groupLine))
	fmt.Fprintln(out, env.dim(colLine))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(colLine))))
	for _, s := range c.Strikes {
		// Bold the ATM strike: it's the single hero number per strike grid —
		// the anchor a reader uses to navigate the wings. Other strikes stay
		// plain so the eye picks ATM out without competing emphasis.
		strike := fmt.Sprintf("%8.2f", s.Strike)
		marker := ""
		if s.IsATM {
			strike = env.bold(strike)
			marker = " ← ATM"
		}
		fmt.Fprintf(out, "  %s %s %s %s %s   %s   %s %s %s %s %s%s\n",
			fmt2(s.CallBid), fmt2(s.CallAsk), fmt2(s.CallLast), fmtPct(s.CallIV), fmtOICell(s.CallOI),
			strike,
			fmt2(s.PutBid), fmt2(s.PutAsk), fmt2(s.PutLast), fmtPct(s.PutIV), fmtOICell(s.PutOI),
			marker)
	}
	fmt.Fprintln(out)
	// Caption is intentionally about IV (the model-computation cell that
	// can be unavailable); Bid/Ask/Last are exchange ticks and a missing
	// value there means an illiquid leg with no quotes, not "unavailable".
	fmt.Fprintln(out, env.dim("  IV is delivered as a model-computation tick. Empty cells = unavailable, never derived."))
	if !rpc.IsOptionRTH(time.Now()) {
		fmt.Fprintln(out, env.yellow("  "+optionOffHoursBanner))
	}
	return 0
}

// renderChainExpiriesText prints the expiry list. Two columns when withIV
// is set so users can see the ATM IV term structure at a glance; single
// column otherwise. Empty list → guidance, not silence.
//
// When the daemon applied the default 12-expiry cap, the tail rows arrive
// without IVStatus while the head rows have it — that's the signal to
// render a "first N — pass --all-expiries to expand" footer hint.
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
	cappedAt := chainExpiriesCapBoundary(r.Expiries, withIV)
	header := fmt.Sprintf("%s  %d expiries available", r.Symbol, len(r.Expiries))
	if r.Spot > 0 {
		header += fmt.Sprintf("  ·  spot %s", formatMoney(r.Spot))
	}
	fmt.Fprintln(out, header)
	fmt.Fprintln(out)
	if withIV {
		// EXPIRY · DTE · ATM IV · 1-σ EXPECTED MOVE BY EXPIRY ($ + %)
		// Expected move is the canonical spot × IV × √(DTE/365) — same
		// shape CBOE's option calculator and most desk tools use. Pre-
		// computed on the daemon side; renderer just lays it out.
		fmt.Fprintln(out, "  EXPIRY        DTE   ATM IV   "+env.bold("EXPECTED MOVE"))
		for _, e := range r.Expiries {
			fmt.Fprintf(out, "  %-10s  %4s   %s   %s\n",
				e.Date,
				env.dim(fmtDTE(e.DTE)),
				fmtIVRow(e.IV, e.IVStatus),
				env.bold(fmtImpliedMove(e.ImpliedMove, e.ImpliedMovePct)))
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  spot × IV × √(DTE/365)  ·  1-σ; CBOE convention"))
	} else {
		fmt.Fprintln(out, "  EXPIRY        DTE")
		for _, e := range r.Expiries {
			fmt.Fprintf(out, "  %-10s  %4s\n", e.Date, fmtDTE(e.DTE))
		}
	}
	fmt.Fprintln(out)
	if cappedAt > 0 {
		fmt.Fprintf(out, "  IV fetched for the nearest %d expiries; pass `--all-expiries` to fetch IV for the rest.\n", cappedAt)
	}
	fmt.Fprintf(out, "  Pick one with `ibkr chain %s --expiry YYYY-MM-DD`.\n", r.Symbol)
	if withIV && !rpc.IsOptionRTH(time.Now()) {
		fmt.Fprintln(out, env.yellow("  "+optionOffHoursBanner))
	}
	return 0
}

// chainExpiriesCapBoundary returns the index N at which the daemon stopped
// fetching IV (so rows 0..N-1 have IV/status; rows N..len-1 are bare).
// Returns 0 when no cap was applied — either the user passed
// --all-expiries, every row was fetched, or withIV was off entirely.
func chainExpiriesCapBoundary(rows []rpc.ChainExpiry, withIV bool) int {
	if !withIV || len(rows) == 0 {
		return 0
	}
	for i, e := range rows {
		if e.IVStatus == "" && e.IV == nil {
			if i == 0 {
				return 0 // nothing fetched — not a cap, just a dead farm
			}
			return i
		}
	}
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

// fmt2 renders a quote price right-aligned to 6 visible columns, or a
// 6-wide em-dash placeholder when missing/zero. Width matches the chain
// table's per-leg column width so call/put grids stay aligned even when
// some strikes have no quotes.
func fmt2(p *float64) string {
	if p == nil || *p == 0 {
		return padDash(6)
	}
	return fmt.Sprintf("%6.2f", *p)
}

// fmtPct renders an IV percentage right-aligned to 6 visible columns, or
// a 6-wide em-dash placeholder when missing/zero. The percent sign already
// fits inside the 6-wide format, so no extra column is needed.
func fmtPct(p *float64) string {
	if p == nil || *p == 0 {
		return padDash(6)
	}
	return fmt.Sprintf("%5.1f%%", *p*100)
}

// fmtDTE renders the day-to-expiry count right-aligned to a 4-char column.
// 0 is rendered as a numeric `0` (same-day expiry, intraday) — never an
// em-dash, because 0 carries information.
func fmtDTE(dte int) string {
	return fmt.Sprintf("%4d", dte)
}

// fmtOI renders an open-interest count compactly: small numbers as-is,
// thousands as "1.2K"/"45K", millions as "1.2M"/"12M". 0 (and negatives,
// which should never happen but are treated as "no data") render as an
// em-dash so empty cells match how zero bid/ask render in the same table.
// IBKR delivers OI via tick types 27/28 best-effort — off-hours or
// illiquid strikes commonly arrive as 0, and the renderer treats that
// identically to "never delivered".
func fmtOI(oi int64) string {
	if oi <= 0 {
		return "—"
	}
	switch {
	case oi < 1000:
		return fmt.Sprintf("%d", oi)
	case oi < 10_000:
		return fmt.Sprintf("%.1fK", float64(oi)/1000)
	case oi < 1_000_000:
		return fmt.Sprintf("%dK", oi/1000)
	case oi < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(oi)/1_000_000)
	default:
		return fmt.Sprintf("%dM", oi/1_000_000)
	}
}

// fmtOICell wraps fmtOI for the chain table cell: 6-col right-aligned,
// em-dash placeholder when nil. Mirrors fmt2/fmtPct's pointer-aware
// pattern so the renderer call site stays symmetric.
func fmtOICell(oi *int64) string {
	if oi == nil || *oi <= 0 {
		return padDash(6)
	}
	return fmt.Sprintf("%6s", fmtOI(*oi))
}

// fmtImpliedMove renders the 1-σ expected dollar move and its percent of
// spot in a single fixed-width cell. Empty cell (em-dashes) when the
// daemon couldn't compute the move — typically because the per-expiry IV
// fetch didn't land.
func fmtImpliedMove(move, pct *float64) string {
	if move == nil || pct == nil || *move == 0 {
		return padDash(17)
	}
	// Money portion padded to 8 visible cells so the parenthesized percent
	// column aligns regardless of single- vs three-digit dollar magnitudes
	// ("$ 7.51" vs "$ 100.04"). Width 8 covers up to "$ 999.99" which is
	// well above any realistic 1-σ move on common underlyings.
	return fmt.Sprintf("±%-8s (%4.1f%%)", strings.TrimSpace(formatMoney(*move)), *pct*100)
}
