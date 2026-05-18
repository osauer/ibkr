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

	if c.ZeroGamma != nil {
		fmt.Fprintf(out, "  Zero-gamma  %.2f", *c.ZeroGamma)
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
		fmt.Fprintf(out, "  Zero-gamma  no crossing in sweep window (all %s gamma)\n", c.GammaSign)
	}

	fmt.Fprintf(out, "  |Γ|·OI sum  %.3e (sign-agnostic magnitude)\n", c.GammaTotalAbs)
	fmt.Fprintf(out, "  Leg count   %d across %d expirations\n", c.LegCount, len(c.Expirations))
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
	fmt.Fprintln(out, env.dim("  Disclosure: the signed zero-gamma assumes the 2018 \"dealers long calls,"))
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
