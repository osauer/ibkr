package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func runGamma(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "gamma")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	noWait := fs.Bool("no-wait", false, "return immediately with current status; don't block on the compute")
	force := fs.Bool("force", false, "ignore the cached result and start a fresh compute (diagnostics)")
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
	params := rpc.GammaZeroSPXParams{WaitMs: waitMs, Force: *force}

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
	fmt.Fprintln(out, "SPY Dealer Zero-Gamma")
	fmt.Fprintln(out)

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
	fmt.Fprintf(out, "  SPY spot    %.2f", c.SpotUnderlying)
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

	if c.ZeroGamma != nil {
		fmt.Fprintf(out, "  γ-zero      %.2f", *c.ZeroGamma)
		if c.GapPct != nil {
			gap := *c.GapPct
			sign := "+"
			if gap < 0 {
				sign = ""
			}
			fmt.Fprintf(out, "  (spot %s%.2f %%)", sign, gap)
		}
		fmt.Fprintln(out)
	} else {
		// No crossing in the swept window. The signed profile is
		// one-sided; surface that as a regime statement rather than
		// "all <raw enum> gamma" which produces "all no_data gamma"
		// for the degenerate case.
		switch c.GammaSign {
		case "positive":
			fmt.Fprintln(out, "  γ-zero      no crossing — dealer long-γ across ±15% sweep (stabilizing regime, γ-zero well below spot)")
		case "negative":
			fmt.Fprintln(out, "  γ-zero      no crossing — dealer short-γ across ±15% sweep (amplifying regime, γ-zero well above spot)")
		default:
			fmt.Fprintln(out, "  γ-zero      no crossing — sweep produced no signed profile")
		}
	}

	// Near vs term breakdown. The combined headline above hides the
	// 0DTE/end-of-week vs monthly-OPEX contrast that's the most
	// information-dense aspect of dealer gamma — surface it when both
	// buckets have data.
	if c.NearLegCount > 0 || c.TermLegCount > 0 {
		fmt.Fprintf(out, "  γ-zero near %s\n", formatHorizonGammaLine(c.ZeroGammaNear, c.GammaSignNear, c.SpotUnderlying, c.NearLegCount, "DTE ≤ 7"))
		fmt.Fprintf(out, "  γ-zero term %s\n", formatHorizonGammaLine(c.ZeroGammaTerm, c.GammaSignTerm, c.SpotUnderlying, c.TermLegCount, "DTE > 7"))
	}

	fmt.Fprintf(out, "  |Γ|·OI sum  %.3e (sign-agnostic magnitude)\n", c.GammaTotalAbs)
	fmt.Fprintf(out, "  Leg count   %d across %d expirations\n", c.LegCount, len(c.Expirations))
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
		header := fmt.Sprintf("    %-12s  %8s  %5s  %12s  %10s",
			"EXPIRY", "STRIKE", "RIGHT", "|GEX|", "OI")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim("    "+strings.Repeat("─", visibleLen(header)-4)))
		for _, ts := range c.TopStrikes {
			fmt.Fprintf(out, "    %-12s  %8.0f  %5s  %12.3e  %10d\n",
				ts.Expiry, ts.Strike, ts.Right, ts.AbsGEX, ts.OI)
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
// — either "γ-zero NNN.NN (+M.N% · X legs · DTE ≤ 7)" or the
// no-crossing/no-data variants. The renderer wants a compact one-line
// summary per bucket; this helper keeps both lines symmetric.
func formatHorizonGammaLine(zg *float64, sign string, spot float64, legCount int, dteHint string) string {
	if legCount == 0 {
		return fmt.Sprintf("—  (no legs · %s)", dteHint)
	}
	if zg != nil {
		gap := (spot - *zg) / *zg * 100
		s := "+"
		if gap < 0 {
			s = ""
		}
		return fmt.Sprintf("%.2f  (spot %s%.2f %% · %d legs · %s)", *zg, s, gap, legCount, dteHint)
	}
	switch sign {
	case "positive":
		return fmt.Sprintf("no crossing — dealer long-γ (%d legs · %s)", legCount, dteHint)
	case "negative":
		return fmt.Sprintf("no crossing — dealer short-γ (%d legs · %s)", legCount, dteHint)
	}
	return fmt.Sprintf("no crossing (%d legs · %s)", legCount, dteHint)
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
