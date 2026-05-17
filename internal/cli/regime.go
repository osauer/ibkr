package cli

import (
	"context"
	"fmt"

	"github.com/osauer/ibkr/internal/rpc"
)

func runRegime(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "regime")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON (the canonical surface for renderers + LLMs)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "regime: takes no positional args (got %v)", fs.Args())
	}

	var res rpc.RegimeSnapshotResult
	if err := env.Conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &res); err != nil {
		return fail(env, "regime: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderRegimeText(env, &res)
}

// renderRegimeText prints a deliberately plain dump of the five
// indicators. Threshold derivation (green/yellow/red) is the
// consumer's job — see the `notes` field on each row for the spec
// thresholds verbatim. v1 keeps the renderer minimal so the wire
// shape stays the canonical surface; a richer renderer can read the
// same JSON via `--json` and apply its own thresholds.
func renderRegimeText(env *Env, r *rpc.RegimeSnapshotResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Risk Regime Snapshot  ·  %s\n", r.AsOf.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(out, "Spec doc: %s\n", r.SpecDoc)
	fmt.Fprintln(out)

	// 1. VIX term structure
	fmt.Fprintln(out, "1. VIX/VIX3M ratio")
	switch {
	case r.VIXTermStructure.Ratio != nil:
		fmt.Fprintf(out, "   VIX %.2f / VIX3M %.2f = %.3f (%s)\n",
			derefF(r.VIXTermStructure.VIX), derefF(r.VIXTermStructure.VIX3M),
			*r.VIXTermStructure.Ratio, r.VIXTermStructure.Status)
	default:
		fmt.Fprintf(out, "   %s — %s\n", r.VIXTermStructure.Status, r.VIXTermStructure.ErrorMessage)
	}
	fmt.Fprintf(out, "   %s\n", env.dim(r.VIXTermStructure.Notes))
	fmt.Fprintln(out)

	// 2. HYG vs SPY divergence
	fmt.Fprintln(out, "2. HYG vs SPY divergence")
	if r.HYGSPYDivergence.Status == rpc.RegimeStatusError {
		fmt.Fprintf(out, "   error — %s\n", r.HYGSPYDivergence.ErrorMessage)
	} else {
		fmt.Fprintf(out, "   HYG %s  50-DMA %s  (%s)\n",
			fmtF2(r.HYGSPYDivergence.HYGPrice),
			fmtF2(r.HYGSPYDivergence.HYG50DMA),
			r.HYGSPYDivergence.Status)
		fmt.Fprintf(out, "   SPY %s  52-w high %s\n",
			fmtF2(r.HYGSPYDivergence.SPYPrice),
			fmtF2(r.HYGSPYDivergence.SPY52WHigh))
	}
	fmt.Fprintf(out, "   %s\n", env.dim(r.HYGSPYDivergence.Notes))
	fmt.Fprintln(out)

	// 3. USD/JPY
	fmt.Fprintln(out, "3. USD/JPY")
	switch r.USDJPY.Status {
	case rpc.RegimeStatusOK, rpc.RegimeStatusStale:
		fmt.Fprintf(out, "   %s last %s  7d-ago %s  weekly %s%% (%s)\n",
			r.USDJPY.Symbol, fmtF4(r.USDJPY.Last),
			fmtF4(r.USDJPY.Close7DAgo), fmtF2(r.USDJPY.WeeklyChange),
			r.USDJPY.Status)
	default:
		fmt.Fprintf(out, "   %s — %s\n", r.USDJPY.Status, r.USDJPY.ErrorMessage)
	}
	fmt.Fprintf(out, "   %s\n", env.dim(r.USDJPY.Notes))
	fmt.Fprintln(out)

	// 4. SPX zero-gamma
	fmt.Fprintln(out, "4. SPX dealer zero-gamma")
	switch r.GammaZero.Status {
	case rpc.RegimeStatusOK:
		c := r.GammaZero.Envelope.Result
		if c == nil {
			fmt.Fprintln(out, "   ready but envelope missing payload")
		} else {
			fmt.Fprintf(out, "   spot %.2f", c.SpotSPX)
			if c.ZeroGamma != nil {
				fmt.Fprintf(out, "  zero-gamma %.2f", *c.ZeroGamma)
				if c.GapPct != nil {
					fmt.Fprintf(out, "  gap %+.2f%%", *c.GapPct)
				}
			} else {
				fmt.Fprintf(out, "  no crossing in sweep (%s gamma)", c.GammaSign)
			}
			fmt.Fprintf(out, "  legs=%d\n", c.LegCount)
			if len(c.Warnings) > 0 {
				fmt.Fprintf(out, "   warnings: %v\n", c.Warnings)
			}
		}
	case rpc.RegimeStatusComputing:
		eta := r.GammaZero.Envelope.EtaSeconds
		fmt.Fprintf(out, "   computing (eta %ds, progress %d%%)\n",
			eta, r.GammaZero.Envelope.Progress)
		fmt.Fprintln(out, "   re-run `ibkr regime` later to read the cached result")
	default:
		fmt.Fprintf(out, "   %s — %s\n", r.GammaZero.Status, r.GammaZero.Envelope.Error)
	}
	fmt.Fprintf(out, "   %s\n", env.dim(r.GammaZero.Notes))
	fmt.Fprintln(out)

	// 5. SPX breadth
	fmt.Fprintln(out, "5. SPX breadth (% above 50-DMA)")
	switch r.Breadth.Status {
	case rpc.RegimeStatusOK, rpc.RegimeStatusStale:
		fmt.Fprintf(out, "   %.1f%% (%s)\n",
			r.Breadth.Envelope.Value, r.Breadth.Status)
	default:
		fmt.Fprintf(out, "   %s — see notes\n", r.Breadth.Status)
	}
	fmt.Fprintf(out, "   %s\n", env.dim(r.Breadth.Notes))
	fmt.Fprintln(out)

	return 0
}

// fmtF2 / fmtF4 format pointer-floats with N decimal places, or "—"
// when nil. Keeps the table tidy without sprinkling nil-guards.
func fmtF2(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *p)
}

func fmtF4(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.4f", *p)
}

func derefF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
