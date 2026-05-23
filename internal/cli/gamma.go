package cli

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func runGamma(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "gamma")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	noWait := fs.Bool("no-wait", false, "return immediately with current status; don't block on the compute")
	force := fs.Bool("force", false, "ignore the cached result and start a fresh compute (diagnostics)")
	only := fs.String("only", "", "restrict to a single underlying: 'spy' or 'spx' (default: combined when both reachable, see docs/design/gamma-spx-coverage.md)")
	explain := fs.Bool("explain", false, "show methodology, citations, skew/source/compute metadata, per-bucket breakdown")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "gamma: takes no positional args (got %v)", fs.Args())
	}

	// Default: block up to 3 s for the result. Cached runs return
	// instantly (the ceiling never kicks in); in-flight runs come back
	// almost immediately with a "computing N%" envelope the renderer
	// turns into a progress row. The earlier 50 s default was sized for
	// a one-shot synchronous feel ("block once, get the answer if the
	// compute is fast"), but the combined SPY+SPX compute runs 5–20
	// minutes — well past 50 s — so every call paid the full wait and
	// then returned "still computing." Polling especially suffered:
	// each iteration burned 50 s of wall-clock waiting for `<-job.done`
	// or the timer, regardless of how recently progress had moved.
	//
	// 3 s is the new default — short enough that polling doesn't feel
	// chained to the wire, long enough to absorb a snappy cache-hit
	// or a near-end-of-compute landing without an extra round trip.
	// --no-wait still returns immediately (WaitMs=0) for callers that
	// want zero blocking.
	waitMs := 3_000
	if *noWait {
		waitMs = 0
	}
	// Map --only to the RPC scope. Empty (no flag) falls through to
	// the daemon's empty-Scope default — today: SPY-only; step 7 of
	// the SPX coverage arc switches it to combined with SPX-skipped
	// fallback.
	scope := ""
	switch strings.ToLower(strings.TrimSpace(*only)) {
	case "":
		// pass through
	case "spy":
		scope = rpc.GammaZeroScopeSPY
	case "spx":
		scope = rpc.GammaZeroScopeSPX
	default:
		return fail(env, "gamma: --only must be 'spy' or 'spx' (got %q)", *only)
	}
	params := rpc.GammaZeroSPXParams{WaitMs: waitMs, Force: *force, Scope: scope}

	var res rpc.GammaZeroSPXResult
	if err := env.Conn.Call(ctx, rpc.MethodGammaZeroSPX, params, &res); err != nil {
		return fail(env, "gamma: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderGammaText(env, &res, *explain)
}

func renderGammaText(env *Env, r *rpc.GammaZeroSPXResult, explain bool) int {
	out := env.Stdout

	// Hero. For Computing/Error states we still need a header but skip
	// the spot anchor (no Result payload yet).
	title := gammaHeaderForScope(r)
	timestamp := gammaHeroTimestamp(r)
	anchor := gammaHeroAnchor(r)
	summary := gammaHeroSummary(r)
	renderCommandHero(out, title, timestamp, anchor, summary)

	// Top-of-output banner for entitlement-degraded states. Surfaces
	// the SPX-skipped fallback per design §8.2 above the headline
	// numbers so the reader catches it before acting on the SPY-only
	// view. Pre-status-check so even an in-flight "computing" envelope
	// can carry the banner from the prior session's warning list.
	if r.Result != nil {
		renderGammaSkippedBanner(env, r.Result)
	}

	switch r.Status {
	case rpc.GammaZeroStatusComputing:
		fmt.Fprintf(out, "  Status      computing\n")
		if r.StartedAt != nil {
			fmt.Fprintf(out, "  Started     %s\n", r.StartedAt.Format("15:04:05 MST"))
		}
		if r.EtaSeconds > 0 {
			fmt.Fprintf(out, "  ETA         %s remaining\n", formatDuration(r.EtaSeconds))
		}
		if r.Progress > 0 {
			fmt.Fprintf(out, "  Progress    %d %%\n", r.Progress)
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  Compute runs once per NY trading session (typical 2-4 min on a warm"))
		fmt.Fprintln(out, env.dim("  contract cache); subsequent calls within the day return cached."))
		fmt.Fprintln(out, env.dim("  Re-run `ibkr gamma` to block again, or add --no-wait to poll."))
		fmt.Fprintln(out)
		return 0

	case rpc.GammaZeroStatusError:
		fmt.Fprintf(out, "  Status      error\n")
		if r.Error != "" {
			fmt.Fprintf(out, "  Reason      %s\n", r.Error)
		}
		fmt.Fprintln(out)
		return 1
	}

	if r.Result == nil {
		// Unknown status with no result — defensive renderer fallback.
		return fail(env, "gamma: daemon returned status %q without a result payload", r.Status)
	}

	c := r.Result

	// Compact per-index lines. In combined mode, one line per index;
	// in single-underlying mode, one line for that index. The line
	// either reports the γ-zero crossing or the no-crossing regime
	// statement plus the leg count.
	renderGammaPerIndexLines(env, c)

	// Magnitude co-primary — surfaced as a peer line rather than
	// buried under "Method" metadata. Convention label comes from the
	// wire so the renderer doesn't re-derive methodology.
	if c.GammaTotalAbs > 0 {
		conv := c.GammaTotalAbsConvention
		if conv == "" {
			conv = "sign-agnostic"
		}
		fmt.Fprintf(out, "  Magnitude   %s per 1%% move  (%s)\n", formatGEX(c.GammaTotalAbs), conv)
	}

	// Monthly OPEX is the third Friday of the month in NY time. Surfaced
	// as a factual calendar line so a reader can spot it without separately
	// reaching for a calendar; the front-week γ-zero/concentration figures
	// move quickly as expiring contracts unwind on the morning of OPEX.
	if isMonthlyOPEXNow() {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  Calendar    monthly OPEX today — front-week reading is distorted by expiring contracts")
	}

	if len(c.Warnings) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  Warnings:"))
		for _, w := range c.Warnings {
			fmt.Fprintf(out, "    · %s\n", w)
		}
	}

	if len(c.TopStrikes) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  Top strikes by |Γ|·OI (regime-robust positioning signal):"))
		// Combined-scope renders an INDEX column per the user-interview
		// choice (single sorted list with the underlying labelled per
		// row); single-underlying mode keeps the original shape so
		// today's SPY-only output is unchanged.
		showIndex := c.Scope == rpc.GammaZeroScopeCombined
		var header string
		if showIndex {
			header = fmt.Sprintf("    %-5s  %-12s  %8s  %5s  %12s  %12s  %10s",
				"INDEX", "EXPIRY", "STRIKE", "RIGHT", "|GEX|", "NOTIONAL", "OI")
		} else {
			header = fmt.Sprintf("    %-12s  %8s  %5s  %12s  %12s  %10s",
				"EXPIRY", "STRIKE", "RIGHT", "|GEX|", "NOTIONAL", "OI")
		}
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim("    "+strings.Repeat("─", visibleLen(header)-4)))
		for _, ts := range c.TopStrikes {
			notional := float64(ts.OI) * ts.Strike * 100
			if showIndex {
				idx := ts.Underlying
				if idx == "" {
					idx = "—"
				}
				fmt.Fprintf(out, "    %-5s  %-12s  %8.0f  %5s  %12s  %12s  %10d\n",
					idx, ts.Expiry, ts.Strike, ts.Right, formatGEX(ts.AbsGEX), formatGEX(notional), ts.OI)
			} else {
				fmt.Fprintf(out, "    %-12s  %8.0f  %5s  %12s  %12s  %10d\n",
					ts.Expiry, ts.Strike, ts.Right, formatGEX(ts.AbsGEX), formatGEX(notional), ts.OI)
			}
		}
	}

	if explain {
		renderGammaExplain(env, c)
	}

	fmt.Fprintln(out)
	return 0
}

// gammaHeroTimestamp returns the formatted local-time stamp for the
// hero, sourced from Result.AsOf (compute finish). Empty when no
// payload exists yet (Computing / Error / pre-Result states).
func gammaHeroTimestamp(r *rpc.GammaZeroSPXResult) string {
	if r == nil || r.Result == nil || r.Result.AsOf.IsZero() {
		return ""
	}
	return r.Result.AsOf.Local().Format("15:04 MST")
}

// gammaHeroAnchor returns the one-line market anchor for the gamma
// hero: spot price + compute freshness. Gamma doesn't pull VIX, so the
// anchor stays focused on the underlying's spot and the result age.
// Empty for Computing/Error states with no Result payload.
func gammaHeroAnchor(r *rpc.GammaZeroSPXResult) string {
	if r == nil || r.Result == nil {
		return ""
	}
	c := r.Result
	parts := []string{fmt.Sprintf("%s %.2f", gammaSpotLabelForScope(c), c.SpotUnderlying)}
	if !c.AsOf.IsZero() {
		age := max(time.Since(c.AsOf).Truncate(time.Second), 0)
		parts = append(parts,
			fmt.Sprintf("computed %s · %s ago", c.AsOf.Local().Format("15:04 MST"), age))
	}
	return strings.Join(parts, "  ·  ")
}

// gammaHeroSummary returns the one-line regime statement for the hero.
// Combined mode uses formatRegimeAgreement; single-underlying mode
// names the single index's regime in the same compact shape. Empty
// when no Result is available.
func gammaHeroSummary(r *rpc.GammaZeroSPXResult) string {
	if r == nil || r.Result == nil {
		return ""
	}
	c := r.Result
	if c.Scope == rpc.GammaZeroScopeCombined {
		return formatRegimeAgreement(c)
	}
	label := gammaSpotLabelForScope(c)
	switch {
	case c.ZeroGamma != nil:
		return fmt.Sprintf("%s γ-zero at %s (flipping regime)", label, formatSpotPrice(*c.ZeroGamma))
	case c.GammaSign == "positive":
		return fmt.Sprintf("%s long-γ (stabilizing)", label)
	case c.GammaSign == "negative":
		return fmt.Sprintf("%s short-γ (amplifying)", label)
	}
	return ""
}

// renderGammaPerIndexLines emits the compact per-index summary lines.
// Combined mode iterates SPY then SPX; single-underlying mode emits one
// line for that index. When the result's HorizonAgreement carries
// "diverge", the line expands to a per-bucket near/0DTE/1-7/term
// breakdown so the disagreement isn't hidden behind a one-line summary.
//
// Per-index format examples:
//
//	SPY  no crossing · long-γ · 1052 legs
//	SPY  γ-zero $735.00 (+0.5% from spot) · 1052 legs
func renderGammaPerIndexLines(env *Env, c *rpc.GammaZeroComputed) {
	out := env.Stdout
	if c.Scope == rpc.GammaZeroScopeCombined {
		for _, key := range []string{"SPY", "SPX"} {
			if sub := c.PerIndex[key]; sub != nil {
				fmt.Fprintf(out, "  %-5s %s\n", key, formatGammaPerIndexCompact(sub))
				if shouldShowDivergedBuckets(sub) {
					renderGammaBucketBreakdown(env, "    ", sub)
				}
			}
		}
		return
	}
	label := gammaSpotLabelForScope(c)
	fmt.Fprintf(out, "  %-5s %s\n", label, formatGammaPerIndexCompact(c))
	if shouldShowDivergedBuckets(c) {
		renderGammaBucketBreakdown(env, "    ", c)
	}
}

// formatGammaPerIndexCompact returns the per-index single-line summary:
// either the γ-zero crossing + signed distance, or a no-crossing regime
// label, followed by " · N legs".
func formatGammaPerIndexCompact(c *rpc.GammaZeroComputed) string {
	legCount := c.LegCount
	if c.ZeroGamma != nil {
		dist := ""
		if c.SpotUnderlying > 0 {
			dist = fmt.Sprintf(" (%+.1f%% from spot)", (*c.ZeroGamma-c.SpotUnderlying)/c.SpotUnderlying*100)
		}
		return fmt.Sprintf("γ-zero %s%s · %d legs", formatSpotPrice(*c.ZeroGamma), dist, legCount)
	}
	regime := "no signed profile"
	switch c.GammaSign {
	case "positive":
		regime = "long-γ"
	case "negative":
		regime = "short-γ"
	}
	return fmt.Sprintf("no crossing · %s · %d legs", regime, legCount)
}

// shouldShowDivergedBuckets reports whether the per-index summary
// should expand to a per-bucket breakdown. We expand whenever the
// HorizonAgreement (re-classified locally from the same wire fields
// the daemon uses) carries a "diverge" prefix.
func shouldShowDivergedBuckets(c *rpc.GammaZeroComputed) bool {
	return strings.HasPrefix(localHorizonAgreement(c), "diverge")
}

// localHorizonAgreement mirrors the daemon's classifyHorizonAgreement
// on a per-index slice so the CLI can detect diverge cases without a
// round trip. The wire's top-level HorizonAgreement is regime-row only
// (gates on RegimeGammaZero) — per-index slices in combined mode don't
// carry it.
func localHorizonAgreement(c *rpc.GammaZeroComputed) string {
	if c == nil || c.SpotUnderlying <= 0 {
		return ""
	}
	zeroAvail := c.ZeroGamma0DTE != nil
	oneToSevenAvail := c.ZeroGamma1to7 != nil
	termAvail := c.ZeroGammaTerm != nil
	avail := 0
	for _, v := range []bool{zeroAvail, oneToSevenAvail, termAvail} {
		if v {
			avail++
		}
	}
	switch avail {
	case 0:
		return ""
	case 1:
		switch {
		case zeroAvail:
			return "0dte_only"
		case oneToSevenAvail:
			return "1to7_only"
		default:
			return "term_only"
		}
	}
	var sides []bool
	if zeroAvail {
		sides = append(sides, c.SpotUnderlying > *c.ZeroGamma0DTE)
	}
	if oneToSevenAvail {
		sides = append(sides, c.SpotUnderlying > *c.ZeroGamma1to7)
	}
	if termAvail {
		sides = append(sides, c.SpotUnderlying > *c.ZeroGammaTerm)
	}
	allAbove, allBelow := true, true
	for _, above := range sides {
		if !above {
			allAbove = false
		}
		if above {
			allBelow = false
		}
	}
	if avail == 3 && allAbove {
		return "all_above"
	}
	if avail == 3 && allBelow {
		return "all_below"
	}
	if zeroAvail && termAvail {
		spotAbove0 := c.SpotUnderlying > *c.ZeroGamma0DTE
		spotAboveT := c.SpotUnderlying > *c.ZeroGammaTerm
		if spotAbove0 != spotAboveT {
			return "diverge:0dte_vs_term"
		}
	}
	return "diverge:partial"
}

// renderGammaBucketBreakdown emits the per-bucket 0DTE / 1-7 / term
// rows used both in the default-mode diverge expansion and the
// --explain mode's full disclosure. Indent is the leading whitespace
// applied to each row so the same helper can sit under both a per-index
// summary ("    ") and the --explain block ("  ").
func renderGammaBucketBreakdown(env *Env, indent string, c *rpc.GammaZeroComputed) {
	out := env.Stdout
	fmt.Fprintf(out, "%sγ-zero 0DTE %s\n", indent,
		formatHorizonGammaLine(c.ZeroGamma0DTE, c.GammaSign0DTE, c.SpotUnderlying, c.LegCount0DTE, "DTE = 0"))
	fmt.Fprintf(out, "%sγ-zero 1-7  %s\n", indent,
		formatHorizonGammaLine(c.ZeroGamma1to7, c.GammaSign1to7, c.SpotUnderlying, c.LegCount1to7, "0 < DTE ≤ 7"))
	fmt.Fprintf(out, "%sγ-zero term %s\n", indent,
		formatHorizonGammaLine(c.ZeroGammaTerm, c.GammaSignTerm, c.SpotUnderlying, c.LegCountTerm, "DTE > 7"))
}

// renderGammaExplain writes the --explain disclosure: per-bucket
// breakdown, methodology/source/compute metadata block, citations, and
// the sign-convention disclosure. Sequenced so a reader scans from the
// most-actionable (per-bucket detail) down to the methodology footer.
func renderGammaExplain(env *Env, c *rpc.GammaZeroComputed) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintln(out, env.dim("  Per-bucket γ-zero (horizon split):"))
	if c.Scope == rpc.GammaZeroScopeCombined {
		for _, key := range []string{"SPY", "SPX"} {
			if sub := c.PerIndex[key]; sub != nil {
				fmt.Fprintf(out, "    %s\n", key)
				renderGammaBucketBreakdown(env, "      ", sub)
			}
		}
	} else {
		renderGammaBucketBreakdown(env, "    ", c)
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Scope       %s · ±%.0f%% strikes · %d expirations\n",
		gammaScopeLabel(c), c.Params.StrikeWidthPct*100, len(c.Expirations))
	fmt.Fprintf(out, "  Leg count   %d across %d expirations\n", c.LegCount, len(c.Expirations))
	if c.SkewModel != "" {
		fmt.Fprintf(out, "  Skew model  %s", c.SkewModel)
		if n := len(c.SkewFitQuality); n > 0 {
			var rs []float64
			for _, info := range c.SkewFitQuality {
				rs = append(rs, info.RSquared)
			}
			if len(rs) > 0 {
				fmt.Fprintf(out, "  (%d expiries fit, median R² %.2f)", n, computeMedian(rs))
			}
		}
		fmt.Fprintln(out)
	}
	if c.DerivedIVLegs > 0 {
		fmt.Fprintf(out, "  Derived IV  %d/%d legs back-solved via Black-Scholes from prior-session prices\n",
			c.DerivedIVLegs, c.LegCount)
	}
	fmt.Fprintf(out, "  Method      %s\n", c.Method)
	fmt.Fprintf(out, "  Source      %s\n", c.Source)
	if c.DurationMS > 0 {
		fmt.Fprintf(out, "  Compute     %s\n", formatDuration(int(c.DurationMS/1000)))
	}

	if len(c.MethodologyCitations) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  Citations")
		for _, ref := range c.MethodologyCitations {
			fmt.Fprintf(out, "    · %s\n", ref)
		}
	}

	// Scaling caveat — printed on every --explain run so a reader of
	// the combined view doesn't anchor on the SPY-scale headline level
	// and miss that SPX dominates the dollar-gamma sum. Two short
	// lines; the spot_anchor field on the wire is the machine-readable
	// counterpart.
	fmt.Fprintln(out)
	fmt.Fprintln(out, env.dim("  Scaling     SPY contributes ~1/100 of SPX dollar-gamma per equivalent leg (S² scaling);"))
	fmt.Fprintln(out, env.dim("              combined |Γ|·OI sum is dominated by SPX. Combined headline level uses SPY-scale"))
	fmt.Fprintln(out, env.dim("              (see spot_anchor field); read per_index entries for per-underlying levels."))

	fmt.Fprintln(out)
	fmt.Fprintln(out, env.dim("  Disclosure: the signed γ-zero assumes the 2018 \"dealers long calls,"))
	fmt.Fprintln(out, env.dim("  short puts\" convention. In regimes dominated by covered-call ETFs or"))
	fmt.Fprintln(out, env.dim("  autocall hedging the sign can invert; treat as a regime hint, not a"))
	fmt.Fprintln(out, env.dim("  level. The magnitude signal above is methodology-agnostic."))
}

func formatDuration(seconds int) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	d := time.Duration(seconds) * time.Second
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// formatHorizonGammaLine builds one row of the near/term breakdown
// — either "γ-zero $NNN.NN (+M.N% from spot · X legs · DTE ≤ 7)" or the
// no-crossing/no-data variants. The renderer wants a compact one-line
// summary per bucket; this helper keeps both lines symmetric.
func formatHorizonGammaLine(zg *float64, sign string, spot float64, legCount int, dteHint string) string {
	if legCount == 0 {
		return fmt.Sprintf("—  (no legs · %s)", dteHint)
	}
	if zg != nil {
		// Match the headline γ-zero sign convention: γ-zero distance
		// from spot (negative when below). Avoids the cognitive flip
		// between the headline row and the bucket rows.
		dist := 0.0
		if spot > 0 {
			dist = (*zg - spot) / spot * 100
		}
		return fmt.Sprintf("$%.2f  (%+.1f%% from spot · %d legs · %s)", *zg, dist, legCount, dteHint)
	}
	switch sign {
	case "positive":
		return fmt.Sprintf("no crossing — dealer long-γ (%d legs · %s)", legCount, dteHint)
	case "negative":
		return fmt.Sprintf("no crossing — dealer short-γ (%d legs · %s)", legCount, dteHint)
	}
	return fmt.Sprintf("no crossing (%d legs · %s)", legCount, dteHint)
}

// formatGEX renders a dollar gamma value in human-readable form: $X.XXB
// for ≥1B, $X.XXM for ≥1M, $XXXk for ≥1k, else $X. Used for |Γ|·OI sums
// and per-strike notionals, where SPY chain magnitudes span k → high-B
// across the sum vs. tail-strike axis and a unit suffix reads more
// cleanly than scientific notation. Negative values get a leading minus
// from %f.
func formatGEX(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1e9:
		return fmt.Sprintf("$%.2fB", v/1e9)
	case abs >= 1e6:
		return fmt.Sprintf("$%.2fM", v/1e6)
	case abs >= 1e3:
		return fmt.Sprintf("$%.0fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

// formatSpotPrice renders a per-share dollar price with 2 decimals. Reserved
// for spot levels, γ-zero, and the absolute sweep window — values that
// live in the $10–$10000 range and read cleanly with two decimals.
func formatSpotPrice(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}

// isMonthlyOPEXNow reports whether the current NY-local date falls on the
// third Friday of the month — the canonical monthly OPEX day for U.S.
// listed options. The third Friday is the unique Friday with day-of-month
// in [15, 21]: the first Friday is in [1, 7] and the third Friday is
// exactly two weeks later.
func isMonthlyOPEXNow() bool {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	t := time.Now().In(loc)
	if t.Weekday() != time.Friday {
		return false
	}
	day := t.Day()
	return day >= 15 && day <= 21
}

// computeMedian returns the median of a small slice. Sorts in place.
// Used only by the gamma renderer's median-R² display; if performance
// ever matters we can pick from "math.Floor((n-1)/2)" without a full
// sort.
func computeMedian(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sortedCopy := append([]float64(nil), xs...)
	// Simple insertion sort — n is the expiry count (≤ 8 in practice).
	for i := 1; i < len(sortedCopy); i++ {
		for j := i; j > 0 && sortedCopy[j-1] > sortedCopy[j]; j-- {
			sortedCopy[j-1], sortedCopy[j] = sortedCopy[j], sortedCopy[j-1]
		}
	}
	mid := len(sortedCopy) / 2
	if len(sortedCopy)%2 == 1 {
		return sortedCopy[mid]
	}
	return (sortedCopy[mid-1] + sortedCopy[mid]) / 2
}

// renderGammaSkippedBanner surfaces an entitlement-degraded banner at
// the top of the output when the daemon's combined-mode compute
// degraded to SPY-only (SPX 354 / 200 / timeout / etc). The banner
// is read from the result's Warnings list, which carries
// "spx_unavailable:<reason>" tokens emitted by computeGammaCombined.
//
// Cases (per design §8.2):
//   - spx_unavailable:354     — entitlement gap, most common
//   - spx_unavailable:200     — contract not found / SPX chain restricted
//   - spx_unavailable:no_data — fan-out landed 0 legs in 30s
//   - spx_unavailable:<short> — other (timeout, gateway error)
//
// No banner when warnings list is empty or contains only non-skip
// codes. The "decoupled" warning is surfaced separately via the
// headline badge — kept distinct from entitlement-degraded states.
func renderGammaSkippedBanner(env *Env, c *rpc.GammaZeroComputed) {
	out := env.Stdout
	for _, w := range c.Warnings {
		if !strings.HasPrefix(w, "spx_unavailable:") {
			continue
		}
		reason := strings.TrimPrefix(w, "spx_unavailable:")
		var detail string
		switch reason {
		case "354":
			detail = "entitlement missing (IBKR error 354)"
		case "200":
			detail = "SPX option contract resolution rejected (IBKR error 200)"
		case "no_data":
			detail = "no option data landed within the 30s window"
		case "throttled":
			detail = "gateway throttled the SPX fan-out"
		default:
			detail = reason
		}
		fmt.Fprintf(out, "  ⚠ SPX skipped — %s. Showing SPY only.\n", detail)
		fmt.Fprintln(out, env.dim("    Re-run with --only=spy to suppress this banner, or check your"))
		fmt.Fprintln(out, env.dim("    CBOE OPRA subscription in IBKR's Market Data Subscriptions."))
		fmt.Fprintln(out)
		return
	}
}

// formatRegimeAgreement renders the RegimeAgreement classifier into a
// one-line summary. The disagree case is the actionable signal —
// flagged loudly so the reader doesn't skim past institutional/retail
// positioning divergence.
func formatRegimeAgreement(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "—"
	}
	switch c.RegimeAgreement {
	case "agree:long-gamma":
		return "SPY and SPX both long-γ (stabilizing regime · agreement)"
	case "agree:short-gamma":
		return "SPY and SPX both short-γ (amplifying regime · agreement)"
	case "agree:flipping":
		spy := c.PerIndex["SPY"]
		spx := c.PerIndex["SPX"]
		if spy != nil && spy.ZeroGamma != nil && spx != nil && spx.ZeroGamma != nil {
			return fmt.Sprintf("SPY γ-zero %s · SPX γ-zero %s (both flipping · agreement)",
				formatSpotPrice(*spy.ZeroGamma), formatSpotPrice(*spx.ZeroGamma))
		}
		return "SPY and SPX both flipping (agreement)"
	case "disagree":
		return formatRegimeDisagreement(c)
	}
	return "indeterminate (insufficient per-index data)"
}

// formatRegimeDisagreement renders the actionable case — one index
// stabilising while the other is amplifying. Names both sides so the
// reader knows which book sits where.
func formatRegimeDisagreement(c *rpc.GammaZeroComputed) string {
	spy := perIndexRegimeWord(c.PerIndex["SPY"])
	spx := perIndexRegimeWord(c.PerIndex["SPX"])
	return fmt.Sprintf("SPY %s · SPX %s (DISAGREEMENT — institutional/retail divergence; use per-index below as primary)",
		spy, spx)
}

// perIndexRegimeWord turns a per-index result into a short label
// for the disagreement summary. Mirrors the RegimeAgreement classifier
// on the daemon side.
func perIndexRegimeWord(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "—"
	}
	if c.ZeroGamma != nil {
		return fmt.Sprintf("flipping @ %s", formatSpotPrice(*c.ZeroGamma))
	}
	switch c.GammaSign {
	case "positive":
		return "long-γ"
	case "negative":
		return "short-γ"
	}
	return "—"
}

// gammaHeaderForScope returns the renderer's section header — varies
// with Result.Scope so SPX-only and combined runs don't claim to be
// SPY. Falls back to the SPY title for empty Scope (pre-step-5 result
// envelopes) so old daemon → new CLI mixes render unchanged.
func gammaHeaderForScope(r *rpc.GammaZeroSPXResult) string {
	if r == nil || r.Result == nil {
		return "Dealer γ-zero"
	}
	switch r.Result.Scope {
	case rpc.GammaZeroScopeSPX:
		return "Dealer γ-zero · SPX"
	case rpc.GammaZeroScopeCombined:
		return "Dealer γ-zero · SPY+SPX"
	default:
		return "Dealer γ-zero · SPY"
	}
}

// gammaSpotLabelForScope returns the underlying symbol to print next
// to the headline spot. Uses Result.Scope; combined runs anchor on
// SPY for the headline spot label (per design §12.1).
func gammaSpotLabelForScope(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "SPY"
	}
	switch c.Scope {
	case rpc.GammaZeroScopeSPX:
		return "SPX"
	default:
		return "SPY"
	}
}

// gammaScopeLabel returns the "Scope" row's left-hand label. Mirrors
// the design §9.x mockups for the various single-underlying and
// combined cases.
func gammaScopeLabel(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "SPY only"
	}
	switch c.Scope {
	case rpc.GammaZeroScopeSPX:
		return "SPX only"
	case rpc.GammaZeroScopeCombined:
		return "SPY + SPX"
	default:
		return "SPY only"
	}
}
