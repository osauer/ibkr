package cli

import (
	"context"
	"fmt"
	"math"
	"slices"
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
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "gamma: takes no positional args (got %v)", fs.Args())
	}

	// Default: block up to 50 s for the result. The daemon's per-RPC
	// deadline (55 s) caps this from above and returns a clean
	// "computing" envelope rather than a socket timeout when the
	// compute is genuinely still running.
	waitMs := 50_000
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
	return renderGammaText(env, &res)
}

func renderGammaText(env *Env, r *rpc.GammaZeroSPXResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintln(out, gammaHeaderForScope(r))
	fmt.Fprintln(out)

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
	fmt.Fprintf(out, "  %s spot    %.2f", gammaSpotLabelForScope(c), c.SpotUnderlying)
	if !c.SpotAt.IsZero() {
		fmt.Fprintf(out, "  (%s)", c.SpotAt.Format("15:04:05 MST"))
	}
	fmt.Fprintln(out)
	// Compute freshness: AsOf is when the daemon finished the GEX
	// compute (distinct from SpotAt above, which is the gateway's
	// tick time for the underlying). The daemon refreshes the cached
	// compute under a soft TTL — agents and humans both want to see
	// how old this result is, especially after a long-idle daemon
	// returned a same-session cached value.
	if !c.AsOf.IsZero() {
		age := max(time.Since(c.AsOf).Truncate(time.Second), 0)
		fmt.Fprintf(out, "  Computed    %s · %s ago\n", c.AsOf.Format("15:04:05 MST"), age)
	}

	// Combined-mode renderer: when both SPY and SPX are present,
	// surface the combined γ-zero gap-percent and the per-index
	// breakdown instead of the SPY-shaped single γ-zero line. The
	// combined path returns true; the single-underlying path falls
	// through.
	if renderCombinedHeadline(env, c) {
		// Combined block prints its own γ-zero + per-index detail;
		// skip the single-line γ-zero rendering below.
	} else if c.ZeroGamma != nil {
		// Distance is signed from spot to γ-zero: negative when γ-zero
		// is below spot, positive when above. Flipped from the wire's
		// GapPct (which is signed the other way around — spot relative
		// to γ-zero) so the value reads as "γ-zero is X% from spot."
		fmt.Fprintf(out, "  γ-zero      $%.2f", *c.ZeroGamma)
		if c.SpotUnderlying > 0 {
			dist := (*c.ZeroGamma - c.SpotUnderlying) / c.SpotUnderlying * 100
			fmt.Fprintf(out, " (%+.1f%% from spot)", dist)
		}
		fmt.Fprintln(out)
	} else {
		// No crossing in the swept window. The signed profile is
		// one-sided; surface that as a regime statement rather than
		// "all <raw enum> gamma" which produces "all no_data gamma"
		// for the degenerate case. Render the absolute sweep bounds
		// in dollars rather than "well above/below spot" — readers
		// can immediately see whether γ-zero is plausibly close to
		// the window edge.
		sweepRange := fmt.Sprintf("γ-zero outside swept range %s–%s",
			formatSpotPrice(c.SweepLowAbs), formatSpotPrice(c.SweepHighAbs))
		sweepPct := c.Params.SweepRangePct * 100
		switch c.GammaSign {
		case "positive":
			fmt.Fprintf(out, "  γ-zero      no crossing — dealer long-γ across ±%.0f%% sweep (stabilizing regime, %s)\n", sweepPct, sweepRange)
		case "negative":
			fmt.Fprintf(out, "  γ-zero      no crossing — dealer short-γ across ±%.0f%% sweep (amplifying regime, %s)\n", sweepPct, sweepRange)
		default:
			fmt.Fprintln(out, "  γ-zero      no crossing — sweep produced no signed profile")
		}
	}

	// Near vs term breakdown. The combined renderer above already
	// emits per-index near/term rows; skip this single-line shape in
	// combined mode to avoid duplicate output.
	if c.Scope != rpc.GammaZeroScopeCombined && (c.NearLegCount > 0 || c.TermLegCount > 0) {
		fmt.Fprintf(out, "  γ-zero near %s\n", formatHorizonGammaLine(c.ZeroGammaNear, c.GammaSignNear, c.SpotUnderlying, c.NearLegCount, "DTE ≤ 7"))
		fmt.Fprintf(out, "  γ-zero term %s\n", formatHorizonGammaLine(c.ZeroGammaTerm, c.GammaSignTerm, c.SpotUnderlying, c.TermLegCount, "DTE > 7"))
	}

	fmt.Fprintf(out, "  |Γ|·OI sum  %s per 1%% move (sign-agnostic magnitude)\n", formatGEX(c.GammaTotalAbs))
	if c.TopConcentrationPct > 0 && len(c.TopStrikes) > 0 {
		top := c.TopStrikes[0]
		fmt.Fprintf(out, "  Top strike  %.0f%% of total |GEX| (%.0f%s %s)\n",
			c.TopConcentrationPct, top.Strike, top.Right, top.Expiry)
	}
	fmt.Fprintf(out, "  Leg count   %d across %d expirations\n", c.LegCount, len(c.Expirations))
	if c.Params.StrikeWidthPct > 0 {
		fmt.Fprintf(out, "  Scope       %s · ±%.0f%% strikes · %d expirations\n",
			gammaScopeLabel(c), c.Params.StrikeWidthPct*100, len(c.Expirations))
	}
	if c.SkewModel != "" {
		fmt.Fprintf(out, "  Skew model  %s", c.SkewModel)
		if n := len(c.SkewFitQuality); n > 0 {
			// Pick the median R² to show fit quality across expiries
			// without overwhelming the default view; full per-expiry
			// detail lives in the JSON envelope.
			var rs []float64
			for _, info := range c.SkewFitQuality {
				rs = append(rs, info.RSquared)
			}
			if len(rs) > 0 {
				// Quick sort + median.
				medianR := computeMedian(rs)
				fmt.Fprintf(out, "  (%d expiries fit, median R² %.2f)", n, medianR)
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

	fmt.Fprintln(out)
	fmt.Fprintln(out, env.dim("  Disclosure: the signed γ-zero assumes the 2018 \"dealers long calls,"))
	fmt.Fprintln(out, env.dim("  short puts\" convention. In regimes dominated by covered-call ETFs or"))
	fmt.Fprintln(out, env.dim("  autocall hedging the sign can invert; treat as a regime hint, not a"))
	fmt.Fprintln(out, env.dim("  level. The magnitude signal above is methodology-agnostic."))
	fmt.Fprintln(out)
	return 0
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

// renderCombinedHeadline prints the SPY+SPX combined headline block —
// the combined γ-zero gap with decoupled-badge handling, plus the
// per-index detail rows (SPY then SPX) with their own near/term
// breakdowns. Returns true when the combined path was used; the
// caller falls back to the single-underlying renderer otherwise.
//
// Mirrors design §9.1's mockup. The combined number is rendered as a
// spot-percent (CombinedGapPct), not a price, because SPY and SPX
// have different absolute spot levels — a single price would be
// nonsensical. Per-index rows use their own anchored spots.
func renderCombinedHeadline(env *Env, c *rpc.GammaZeroComputed) bool {
	if c == nil || c.Scope != rpc.GammaZeroScopeCombined {
		return false
	}
	out := env.Stdout

	// Headline: combined γ-zero gap. Regime label off the SPY-half
	// sign — both halves track the same dealer-positioning regime in
	// the ≥ 0.90 correlation window (where we trust the combined
	// view); for decoupled regimes the badge tells the reader to use
	// per-index instead.
	decoupled := slices.Contains(c.Warnings, "decoupled")
	if c.CombinedGapPct != nil {
		sign := "+"
		if *c.CombinedGapPct < 0 {
			sign = ""
		}
		regime := gammaCombinedRegimeLabel(c)
		fmt.Fprintf(out, "  Combined γ-zero  spot %s%.2f %% %s\n", sign, *c.CombinedGapPct, regime)
	} else {
		fmt.Fprintf(out, "  Combined γ-zero  no crossing — %s\n", gammaCombinedRegimeLabel(c))
	}
	if decoupled && c.DecoupledCorr != nil {
		fmt.Fprintln(out, env.dim(fmt.Sprintf("    ⚠ SPY/SPX 20d corr %.2f < 0.90 — combined number unreliable; treat per-index below as primary.", *c.DecoupledCorr)))
	}

	// Per-index detail.
	spy := c.PerIndex["SPY"]
	spx := c.PerIndex["SPX"]
	fmt.Fprintln(out, "  Per-index:")
	if spy != nil {
		renderPerIndexRow(env, "SPY", spy)
	}
	if spx != nil {
		renderPerIndexRow(env, "SPX", spx)
	}
	return true
}

// renderPerIndexRow prints one per-underlying detail block inside the
// combined headline. Indented two levels so it nests visually under
// the "Per-index:" header. Near/term sub-rows mirror the SPY-only
// renderer's existing breakdown.
func renderPerIndexRow(env *Env, label string, c *rpc.GammaZeroComputed) {
	out := env.Stdout
	if c.ZeroGamma != nil {
		gap := "+"
		if c.SpotUnderlying > 0 {
			dist := (*c.ZeroGamma - c.SpotUnderlying) / c.SpotUnderlying * 100
			if dist < 0 {
				gap = ""
			}
			fmt.Fprintf(out, "    %s γ-zero       %s  (spot %s%.2f %%)\n",
				label, formatSpotPrice(*c.ZeroGamma), gap, dist)
		} else {
			fmt.Fprintf(out, "    %s γ-zero       %s\n", label, formatSpotPrice(*c.ZeroGamma))
		}
	} else {
		fmt.Fprintf(out, "    %s γ-zero       no crossing (%s)\n", label, c.GammaSign)
	}
	if c.NearLegCount > 0 || c.TermLegCount > 0 {
		fmt.Fprintf(out, "      near         %s\n", formatHorizonGammaLine(c.ZeroGammaNear, c.GammaSignNear, c.SpotUnderlying, c.NearLegCount, "DTE ≤ 7"))
		fmt.Fprintf(out, "      term         %s\n", formatHorizonGammaLine(c.ZeroGammaTerm, c.GammaSignTerm, c.SpotUnderlying, c.TermLegCount, "DTE > 7"))
	}
}

// gammaCombinedRegimeLabel infers the regime label for the combined
// headline by looking at the combined sweep's sign profile. Returns
// "(regime: long-γ, stabilizing)" or similar — matches the design §9.1
// mockup. Empty when no sign info is available.
func gammaCombinedRegimeLabel(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	switch c.GammaSign {
	case "positive":
		return "(regime: long-γ, stabilizing)"
	case "negative":
		return "(regime: short-γ, amplifying)"
	}
	return ""
}

// gammaHeaderForScope returns the renderer's section header — varies
// with Result.Scope so SPX-only and combined runs don't claim to be
// SPY. Falls back to the SPY title for empty Scope (pre-step-5 result
// envelopes) so old daemon → new CLI mixes render unchanged.
func gammaHeaderForScope(r *rpc.GammaZeroSPXResult) string {
	if r == nil || r.Result == nil {
		return "Dealer Zero-Gamma"
	}
	switch r.Result.Scope {
	case rpc.GammaZeroScopeSPX:
		return "SPX Dealer Zero-Gamma"
	case rpc.GammaZeroScopeCombined:
		return "Dealer Zero-Gamma (SPY + SPX)"
	default:
		return "SPY Dealer Zero-Gamma"
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
