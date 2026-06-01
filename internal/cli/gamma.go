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
	force := fs.Bool("force", false, "start a diagnostic refresh; preserve a good served cache unless the refresh succeeds")
	only := fs.String("only", "", "restrict to a single underlying: 'spy' or 'spx' (default: combined when both reachable)")
	explain := fs.Bool("explain", false, "show methodology, citations, skew/source/compute metadata, per-bucket breakdown")
	profiles := fs.Bool("profiles", false, "include full gamma-profile arrays in --json output")
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
	// the daemon's default combined run with SPX-skipped fallback.
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
		if !*profiles {
			rpc.StripGammaProfiles(&res)
		}
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
	renderCommandHeroStyled(env, out, title, timestamp, anchor, summary, gammaHeroSummaryStyle(r))

	// Top-of-output banner for entitlement-degraded states. Surfaces
	// the SPX-skipped fallback per design §8.2 above the headline
	// numbers so the reader catches it before acting on the SPY-only
	// view. Pre-status-check so even an in-flight "computing" envelope
	// can carry the banner from the prior session's warning list.
	spxSkippedBanner := false
	if r.Result != nil {
		spxSkippedBanner = renderGammaSkippedBanner(env, r.Result)
	}

	switch r.Status {
	case rpc.GammaZeroStatusCold:
		// No compute has run this NY session and none is in flight. This is
		// the common off-hours state: the daemon never recomputes on a closed
		// market, so a stale or invalidated cache leaves us with no value to
		// serve until the next U.S. equity-options session open. Friendly
		// explainer beats a bare "without a result payload" error.
		fmt.Fprintf(out, "  Status      no data yet (cold cache)\n")
		if r.ColdReason != "" {
			fmt.Fprintf(out, "  Reason      %s\n", r.ColdReason)
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  The compute runs automatically on the first call of each NY"))
		fmt.Fprintln(out, env.dim("  trading session (09:30 ET, Mon-Fri). Outside session hours the"))
		fmt.Fprintln(out, env.dim("  daemon does not run heavy option-chain fans against a closed"))
		if r.ColdAction != "" {
			fmt.Fprintln(out, env.dim("  market. "+r.ColdAction))
		} else {
			fmt.Fprintln(out, env.dim("  market. To force a compute now (mostly useful when troubleshooting"))
			fmt.Fprintln(out, env.dim("  or testing): ibkr gamma --force"))
		}
		fmt.Fprintln(out)
		return 0

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

	renderGammaDataNotes(env, c, explain, spxSkippedBanner)

	if len(c.TopStrikes) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  Top strikes by |Γ|·OI (regime-robust positioning signal):"))
		// Combined-scope renders an INDEX column per the user-interview
		// choice (single sorted list with the underlying labelled per
		// row); single-underlying mode keeps the narrower table.
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
	var parts []string
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		for _, key := range []string{"SPY", "SPX"} {
			if sub := c.PerIndex[key]; sub != nil && sub.SpotUnderlying > 0 {
				parts = append(parts, fmt.Sprintf("%s %.2f", key, sub.SpotUnderlying))
			}
		}
	} else if c.SpotUnderlying > 0 {
		parts = append(parts, fmt.Sprintf("%s %.2f", gammaSpotLabelForScope(c), c.SpotUnderlying))
	}
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
	if c.Summary != nil && c.Summary.PrimaryStatement != "" {
		return c.Summary.PrimaryStatement
	}
	if c.Scope == rpc.GammaZeroScopeCombined {
		return formatRegimeAgreement(c)
	}
	label := gammaSpotLabelForScope(c)
	switch {
	case c.ZeroGamma != nil:
		return fmt.Sprintf("%s γ-zero at %s (%s)", label, formatSpotPrice(*c.ZeroGamma), gammaRegimeWord(c))
	case c.GammaSign == "positive":
		return fmt.Sprintf("%s long-γ (stabilizing)", label)
	case c.GammaSign == "negative":
		return fmt.Sprintf("%s short-γ (amplifying)", label)
	}
	return ""
}

func gammaHeroSummaryStyle(r *rpc.GammaZeroSPXResult) func(*Env, string) string {
	return func(env *Env, s string) string {
		if env == nil || !env.Color {
			return s
		}
		band := bandUnranked
		if r != nil && r.Result != nil {
			if r.Result.Scope == rpc.GammaZeroScopeCombined && len(r.Result.PerIndex) > 0 {
				band = gammaCombinedRegimeBand(r.Result)
			} else {
				band = gammaSingleRegimeBand(r.Result)
			}
		}
		switch band {
		case bandGreen:
			return ansiBold + ansiGreen + s + ansiReset
		case bandRed:
			return ansiBold + ansiRed + s + ansiReset
		default:
			return heroSummaryStyle(env, s)
		}
	}
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
		return fmt.Sprintf("γ-zero %s%s · %d GEX legs", formatSpotPrice(*c.ZeroGamma), dist, legCount)
	}
	if c.Summary != nil {
		if idx, ok := summaryForSingleIndex(c); ok && idx.ZeroGammaStatus == "unavailable" {
			why := idx.Interpretation
			if why == "" {
				why = "no usable gamma profile"
			}
			return fmt.Sprintf("unavailable · %s · %d GEX legs", why, legCount)
		}
	}
	if c.LegCount > 0 && c.GammaTotalAbs == 0 {
		return fmt.Sprintf("unavailable · no usable gamma magnitude · %d GEX legs", legCount)
	}
	regime := "no signed profile"
	switch c.GammaSign {
	case "positive":
		regime = "long-γ"
	case "negative":
		regime = "short-γ"
	case "no_data":
		return fmt.Sprintf("unavailable · no usable gamma profile · %d GEX legs", legCount)
	}
	rangeText := ""
	if c.SweepLowAbs > 0 && c.SweepHighAbs > 0 {
		rangeText = fmt.Sprintf(" in %s-%s", formatSpotPrice(c.SweepLowAbs), formatSpotPrice(c.SweepHighAbs))
	}
	return fmt.Sprintf("no crossing%s · %s · %d GEX legs", rangeText, regime, legCount)
}

func summaryForSingleIndex(c *rpc.GammaZeroComputed) (rpc.GammaIndexSummary, bool) {
	if c == nil || c.Summary == nil {
		return rpc.GammaIndexSummary{}, false
	}
	if len(c.Summary.PerIndex) == 0 {
		return rpc.GammaIndexSummary{}, false
	}
	label := gammaSpotLabelForScope(c)
	idx, ok := c.Summary.PerIndex[label]
	return idx, ok
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
	buckets := []struct {
		name   string
		regime string
	}{
		{"0dte", gammaBucketRegime(c.SpotUnderlying, c.ZeroGamma0DTE, c.GammaSign0DTE)},
		{"1to7", gammaBucketRegime(c.SpotUnderlying, c.ZeroGamma1to7, c.GammaSign1to7)},
		{"term", gammaBucketRegime(c.SpotUnderlying, c.ZeroGammaTerm, c.GammaSignTerm)},
	}
	var usable []struct {
		name   string
		regime string
	}
	for _, b := range buckets {
		if b.regime != "" {
			usable = append(usable, b)
		}
	}
	switch len(usable) {
	case 0:
		return ""
	case 1:
		return usable[0].name + "_only"
	}
	first := usable[0].regime
	allSame := true
	for _, b := range usable[1:] {
		if b.regime != first {
			allSame = false
			break
		}
	}
	if allSame && len(usable) == 3 {
		return "all_" + strings.TrimSuffix(first, "_gamma")
	}
	if buckets[0].regime != "" && buckets[2].regime != "" && buckets[0].regime != buckets[2].regime {
		return "diverge:0dte_vs_term"
	}
	if !allSame {
		return "diverge:partial"
	}
	return strings.TrimSuffix(first, "_gamma") + "_only"
}

func gammaBucketRegime(spot float64, zero *float64, sign string) string {
	if zero != nil && *zero > 0 {
		gap := (spot - *zero) / *zero * 100
		switch {
		case gap > gammaGapYellow:
			return "long_gamma"
		case gap >= -gammaGapYellow:
			return "transition_gamma"
		default:
			return "short_gamma"
		}
	}
	switch sign {
	case "positive":
		return "long_gamma"
	case "negative":
		return "short_gamma"
	}
	return ""
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

func renderGammaDataNotes(env *Env, c *rpc.GammaZeroComputed, explain bool, spxSkippedBanner bool) {
	details := gammaWarningDetailsForRender(c)
	if len(details) == 0 {
		return
	}
	out := env.Stdout
	printed := false
	seen := map[string]struct{}{}
	for _, d := range details {
		if !shouldRenderGammaWarningDetail(d, explain, spxSkippedBanner) {
			continue
		}
		key := d.Scope + "\x00" + d.Code + "\x00" + d.Message
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if !printed {
			fmt.Fprintln(out)
			fmt.Fprintln(out, env.dim("  Data notes:"))
			printed = true
		}
		scope := ""
		if d.Scope != "" {
			scope = d.Scope + ": "
		}
		line := scope + d.Message
		if d.Impact != "" {
			line += " " + d.Impact
		}
		fmt.Fprintf(out, "    · %s\n", line)
		if explain && d.Action != "" {
			fmt.Fprintf(out, "      %s\n", env.dim("Action: "+d.Action))
		}
	}
}

func shouldRenderGammaWarningDetail(d rpc.GammaWarningDetail, explain bool, spxSkippedBanner bool) bool {
	if d.Code == "no_crossing_in_window" {
		return false
	}
	if strings.HasPrefix(d.Code, "spx_unavailable:") && spxSkippedBanner {
		return false
	}
	return true
}

func gammaWarningDetailsForRender(c *rpc.GammaZeroComputed) []rpc.GammaWarningDetail {
	if c == nil {
		return nil
	}
	var out []rpc.GammaWarningDetail
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		for _, key := range []string{"SPY", "SPX"} {
			out = append(out, gammaWarningDetailsForRender(c.PerIndex[key])...)
		}
		for _, d := range warningDetailsOrFallback(c) {
			if strings.HasPrefix(d.Code, "spy_unavailable:") ||
				strings.HasPrefix(d.Code, "spx_unavailable:") ||
				strings.HasPrefix(d.Code, "spx_cache_fallback") ||
				d.Code == "cache_stale_off_hours" {
				out = append(out, d)
			}
		}
		return out
	}
	return warningDetailsOrFallback(c)
}

func warningDetailsOrFallback(c *rpc.GammaZeroComputed) []rpc.GammaWarningDetail {
	if c == nil {
		return nil
	}
	if len(c.WarningDetails) > 0 {
		return c.WarningDetails
	}
	out := make([]rpc.GammaWarningDetail, 0, len(c.Warnings))
	for _, code := range c.Warnings {
		out = append(out, fallbackGammaWarningDetail(c, code))
	}
	return out
}

func fallbackGammaWarningDetail(c *rpc.GammaZeroComputed, code string) rpc.GammaWarningDetail {
	scope := gammaSpotLabelForScope(c)
	if c != nil && c.Scope == rpc.GammaZeroScopeCombined {
		scope = "SPY+SPX"
	}
	if strings.HasPrefix(code, "spy_unavailable:") {
		scope = "SPY"
	}
	if strings.HasPrefix(code, "spx_unavailable:") {
		scope = "SPX"
	}
	d := rpc.GammaWarningDetail{Code: code, Scope: scope, Severity: "info"}
	switch {
	case code == "no_crossing_in_window":
		d.Message = "No signed gamma-zero crossing was found in the swept range."
		d.Impact = "Use the regime label and swept range instead of a zero-gamma level."
	case code == "0dte_no_legs":
		d.Message = "No same-day expiry legs were included."
		d.Impact = "The 0DTE horizon is unavailable for this run."
	case code == "1to7_no_legs":
		d.Message = "No 1-7 DTE legs were included."
		d.Impact = "The weekly horizon is unavailable for this run."
	case code == "term_no_legs":
		d.Message = "No >7 DTE legs were included."
		d.Impact = "The term horizon is unavailable for this run."
	case code == "throttled":
		d.Severity = "data_quality"
		d.Message = "The gateway throttled part of the option fan-out."
		d.Impact = "Coverage may be incomplete; treat this slice as lower confidence."
		d.Action = "Retry later or during regular trading hours; avoid repeated forced runs."
	case code == "oi_missing":
		d.Severity = "data_quality"
		if c != nil && c.PricedLegCount > 0 {
			d.Message = fmt.Sprintf("Open interest was missing or zero for %d priced legs.", max(c.PricedLegCount-c.LegCount, 0))
			d.Impact = fmt.Sprintf("%d priced legs landed, but only %d contributed to dealer GEX.", c.PricedLegCount, c.LegCount)
		} else {
			d.Message = "Some priced legs had no usable open interest."
		}
	case strings.HasPrefix(code, "spx_unavailable:"):
		d.Severity = "data_quality"
		reason := strings.TrimPrefix(code, "spx_unavailable:")
		d.Message = renderSPXUnavailableMessage(reason)
		d.Impact = "Showing SPY only; SPX gamma is not included."
		d.Action = spxUnavailableAction(reason)
	case code == "all_iv_derived":
		d.Severity = "data_quality"
		d.Message = "All implied volatilities were back-solved instead of supplied by the gateway model tick."
	case code == "strike_budget_capped":
		d.Severity = "methodology"
		d.Message = "The strike fan-out was capped to the nearest 80 listed strikes per expiry."
		d.Impact = "Farther out-of-money strikes inside the ±10% candidate window were skipped to keep the gateway request budget bounded."
	case code == "cache_stale_off_hours":
		d.Severity = "data_quality"
		d.Message = "The cached gamma result is older than 24 hours and markets are closed."
	case strings.HasPrefix(code, "spy_unavailable:"):
		d.Severity = "data_quality"
		reason := strings.TrimPrefix(code, "spy_unavailable:")
		d.Message = renderSPYUnavailableMessage(reason)
		d.Impact = "Showing SPX only; SPY gamma is not included."
		d.Action = spyUnavailableAction(reason)
	default:
		d.Message = code
	}
	return d
}

func renderSPYUnavailableMessage(reason string) string {
	switch normalizeSPXUnavailableReason(reason) {
	case "354":
		return "missing OPRA option market-data entitlement for SPY options (IBKR error 354)"
	case "200":
		return "SPY option contract resolution rejected (IBKR error 200)"
	case "no_data":
		return "no SPY option rows returned usable IV/OI before the fetch window ended"
	case "throttled":
		return "gateway throttled the SPY fan-out"
	case "zero_magnitude":
		return "landed legs produced zero usable gamma magnitude"
	case "fetch_canceled":
		return "the SPY option-chain fetch was canceled before usable data landed"
	case "timeout":
		return "the SPY option-chain fetch timed out before usable data landed"
	default:
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return "SPY option-chain data was unavailable"
		}
		return "SPY option-chain data was unavailable (" + reason + ")"
	}
}

func spyUnavailableAction(reason string) string {
	switch normalizeSPXUnavailableReason(reason) {
	case "354":
		return "Check the U.S. options market-data subscription in IBKR, or run `ibkr gamma --only=spx` to suppress this note."
	case "200":
		return "Retry later; if it repeats, run `ibkr gamma --only=spy --force` for diagnostics or `ibkr gamma --only=spx` to suppress this note."
	case "no_data", "fetch_canceled", "timeout":
		return "Retry during 09:30-16:00 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run `ibkr gamma --only=spx`."
	case "throttled":
		return "Wait a few minutes and retry without --force; use `ibkr gamma --only=spx` if you only want the SPX surface."
	case "zero_magnitude":
		return "Retry during 09:30-16:00 ET, or run `ibkr gamma --only=spy --force` for SPY-only diagnostics."
	default:
		return "Retry later; if it repeats, check the daemon log and TWS market-data farm messages, or run `ibkr gamma --only=spx`."
	}
}

// renderGammaExplain writes the --explain disclosure: per-bucket
// breakdown, methodology/source/compute metadata block, citations, and
// the sign-convention disclosure. Sequenced so a reader scans from the
// most-actionable (per-bucket detail) down to the methodology footer.
func renderGammaExplain(env *Env, c *rpc.GammaZeroComputed) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintln(out, env.dim("  How to read"))
	fmt.Fprintln(out, env.dim("    · γ-zero is the signed profile crossing, when one exists inside the swept range."))
	fmt.Fprintln(out, env.dim("    · No crossing means the model stayed long-γ or short-γ throughout that range."))
	fmt.Fprintln(out, env.dim("    · This is market-structure context, not a trade recommendation."))

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
	fmt.Fprintf(out, "  Scope       %s · ±%.0f%% candidate window · up to 80 strikes/expiry · %d expirations\n",
		gammaScopeLabel(c), c.Params.StrikeWidthPct*100, len(c.Expirations))
	if c.PricedLegCount > 0 && c.PricedLegCount != c.LegCount {
		fmt.Fprintf(out, "  Leg count   %d GEX legs (%d priced) across %d expirations\n",
			c.LegCount, c.PricedLegCount, len(c.Expirations))
	} else {
		fmt.Fprintf(out, "  Leg count   %d GEX legs across %d expirations\n", c.LegCount, len(c.Expirations))
	}
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
		denom := c.PricedLegCount
		if denom == 0 {
			denom = c.LegCount
		}
		fmt.Fprintf(out, "  Derived IV  %d/%d priced legs back-solved via Black-Scholes from option quotes/closes\n",
			c.DerivedIVLegs, denom)
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
	// the combined view doesn't look for a single SPY+SPX gamma-zero
	// level. The wire shape matches this: price-level fields stay
	// under per_index.SPY / per_index.SPX.
	fmt.Fprintln(out)
	fmt.Fprintln(out, env.dim("  Scaling     SPY contributes ~1/100 of SPX dollar-gamma per equivalent leg (S² scaling);"))
	fmt.Fprintln(out, env.dim("              combined |Γ|·OI sums the books, but zero-gamma levels stay per-index"))
	fmt.Fprintln(out, env.dim("              because SPY and SPX use different price scales."))

	fmt.Fprintln(out)
	fmt.Fprintln(out, env.dim("  Disclosure: the signed γ-zero assumes the 2018 \"dealers long calls,"))
	fmt.Fprintln(out, env.dim("  short puts\" convention. In regimes dominated by covered-call ETFs or"))
	fmt.Fprintln(out, env.dim("  autocall hedging the sign can invert; treat it as a regime hint, not"))
	fmt.Fprintln(out, env.dim("  a trade level. The magnitude signal above is sign-convention agnostic."))
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
		return fmt.Sprintf("—  (no GEX legs · %s)", dteHint)
	}
	if zg != nil {
		// Match the headline γ-zero sign convention: γ-zero distance
		// from spot (negative when below). Avoids the cognitive flip
		// between the headline row and the bucket rows.
		dist := 0.0
		if spot > 0 {
			dist = (*zg - spot) / spot * 100
		}
		return fmt.Sprintf("$%.2f  (%+.1f%% from spot · %d GEX legs · %s)", *zg, dist, legCount, dteHint)
	}
	switch sign {
	case "positive":
		return fmt.Sprintf("no crossing — dealer long-γ (%d GEX legs · %s)", legCount, dteHint)
	case "negative":
		return fmt.Sprintf("no crossing — dealer short-γ (%d GEX legs · %s)", legCount, dteHint)
	case "no_data":
		return fmt.Sprintf("unavailable — no usable gamma profile (%d GEX legs · %s)", legCount, dteHint)
	}
	return fmt.Sprintf("no crossing (%d GEX legs · %s)", legCount, dteHint)
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
// degraded to SPY-only (SPX 354 / 200 / timeout / etc). The banner is
// built from warning_details on the wire, with a Warnings fallback for
// legacy/in-process payloads. Keep this as prose, not raw daemon tokens:
// users need to know whether the likely issue is entitlement, session
// timing, pacing, or a fetch cancellation before deciding what to do next.
//
// Cases (per design §8.2):
//   - spx_unavailable:354     — entitlement gap, most common
//   - spx_unavailable:200     — contract not found / SPX chain restricted
//   - spx_unavailable:no_data — fan-out landed 0 legs in 30s
//   - spx_unavailable:<short> — other (timeout, gateway error, cancellation)
//
// No banner when warnings list is empty or contains only non-skip
// codes. The "decoupled" warning is surfaced separately via the
// headline badge — kept distinct from entitlement-degraded states.
func renderGammaSkippedBanner(env *Env, c *rpc.GammaZeroComputed) bool {
	out := env.Stdout
	if notice, ok := spxUnavailableNotice(c); ok {
		fmt.Fprintf(out, "  ⚠ SPX skipped — %s. Showing SPY only.\n", trimSentencePeriod(notice.Message))
		if notice.Context != "" {
			renderGammaNoticeField(env, "Context", notice.Context)
		}
		if notice.Action != "" {
			renderGammaNoticeField(env, "Action", notice.Action)
		}
		fmt.Fprintln(out)
		return true
	}
	return false
}

type gammaSPXUnavailableNotice struct {
	Reason  string
	Message string
	Context string
	Action  string
}

func renderGammaNoticeField(env *Env, label, text string) {
	const maxWidth = 96
	prefix := label + ": "
	indent := strings.Repeat(" ", visibleLen(prefix))
	for i, line := range wrapVisibleText(text, maxWidth-visibleLen(prefix)) {
		if i == 0 {
			fmt.Fprintln(env.Stdout, env.dim("    "+prefix+line))
			continue
		}
		fmt.Fprintln(env.Stdout, env.dim("    "+indent+line))
	}
}

func spxUnavailableNotice(c *rpc.GammaZeroComputed) (gammaSPXUnavailableNotice, bool) {
	if c == nil {
		return gammaSPXUnavailableNotice{}, false
	}
	for _, d := range c.WarningDetails {
		if reason, ok := strings.CutPrefix(d.Code, "spx_unavailable:"); ok {
			notice := gammaSPXNoticeForReason(c, reason)
			if msg := spxUnavailableMessageFromDetail(d, reason); msg != "" {
				notice.Message = msg
			}
			if action := spxUnavailableActionFromDetail(d, reason); action != "" {
				notice.Action = action
			}
			return notice, true
		}
	}
	for _, w := range c.Warnings {
		if reason, ok := strings.CutPrefix(w, "spx_unavailable:"); ok {
			return gammaSPXNoticeForReason(c, reason), true
		}
	}
	return gammaSPXUnavailableNotice{}, false
}

func gammaSPXNoticeForReason(c *rpc.GammaZeroComputed, reason string) gammaSPXUnavailableNotice {
	return gammaSPXUnavailableNotice{
		Reason:  normalizeSPXUnavailableReason(reason),
		Message: renderSPXUnavailableMessage(reason),
		Context: spxUnavailableContext(c, reason),
		Action:  spxUnavailableAction(reason),
	}
}

func spxUnavailableMessageFromDetail(d rpc.GammaWarningDetail, reason string) string {
	msg := strings.TrimSpace(d.Message)
	msg = strings.TrimPrefix(msg, "SPX option chain was skipped: ")
	msg = strings.TrimPrefix(msg, "SPX option-chain fetch skipped: ")
	msg = trimSentencePeriod(msg)
	if msg == "" || spxUnavailableMessageIsRaw(msg, reason) {
		return renderSPXUnavailableMessage(reason)
	}
	return msg
}

func spxUnavailableActionFromDetail(d rpc.GammaWarningDetail, reason string) string {
	action := trimSentencePeriod(strings.TrimSpace(d.Action))
	if action == "" || spxUnavailableActionIsTooGeneric(action, reason) {
		return spxUnavailableAction(reason)
	}
	return qualifyGammaAction(action)
}

func spxUnavailableMessageIsRaw(msg, reason string) bool {
	lower := strings.ToLower(strings.TrimSpace(msg))
	if lower == "" {
		return true
	}
	switch normalizeSPXUnavailableReason(reason) {
	case "fetch_canceled":
		return strings.Contains(lower, "context canceled") ||
			strings.Contains(lower, "context cancelled") ||
			strings.Contains(lower, "context deadline exceeded")
	case "timeout":
		return strings.Contains(lower, "context deadline exceeded")
	}
	return lower == strings.ToLower(strings.TrimSpace(reason))
}

func spxUnavailableActionIsTooGeneric(action, reason string) bool {
	if normalizeSPXUnavailableReason(reason) != "fetch_canceled" &&
		normalizeSPXUnavailableReason(reason) != "timeout" &&
		normalizeSPXUnavailableReason(reason) != "no_data" {
		return false
	}
	lower := strings.ToLower(action)
	return lower == "retry later or run --only=spy" ||
		lower == "retry later, or re-run with --only=spy to suppress this banner" ||
		lower == "retry later or run --only=spy to suppress this banner"
}

func qualifyGammaAction(action string) string {
	replacements := []struct {
		from string
		to   string
	}{
		{"run --only=spy", "run `ibkr gamma --only=spy`"},
		{"run --only=spx --force", "run `ibkr gamma --only=spx --force`"},
		{"re-run with --only=spy", "run `ibkr gamma --only=spy`"},
	}
	for _, r := range replacements {
		action = strings.ReplaceAll(action, r.from, r.to)
	}
	return action
}

func renderSPXUnavailableMessage(reason string) string {
	switch normalizeSPXUnavailableReason(reason) {
	case "354":
		return "missing CBOE OPRA/SPX option market-data entitlement (IBKR error 354)"
	case "200":
		return "SPX option contract resolution rejected (IBKR error 200)"
	case "no_data":
		return "no SPX option rows returned usable IV/OI before the fetch window ended"
	case "throttled":
		return "gateway throttled the SPX fan-out"
	case "zero_magnitude":
		return "landed legs produced zero usable gamma magnitude"
	case "fetch_canceled":
		return "the SPX option-chain fetch was canceled before usable data landed"
	case "timeout":
		return "the SPX option-chain fetch timed out before usable data landed"
	default:
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return "SPX option-chain data was unavailable"
		}
		return "SPX option-chain data was unavailable (" + reason + ")"
	}
}

func spxUnavailableAction(reason string) string {
	switch normalizeSPXUnavailableReason(reason) {
	case "354":
		return "Check the CBOE OPRA/SPX option data subscription in IBKR, or run `ibkr gamma --only=spy` to suppress the banner."
	case "200":
		return "Retry later; if it repeats, run `ibkr gamma --only=spx --force` for diagnostics or `ibkr gamma --only=spy` to suppress the fallback banner."
	case "no_data", "fetch_canceled", "timeout":
		return "Retry during 09:30-16:00 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run `ibkr gamma --only=spy`."
	case "throttled":
		return "Wait a few minutes and retry without --force; use `ibkr gamma --only=spy` if you only want the SPY surface."
	case "zero_magnitude":
		return "Retry during 09:30-16:00 ET, or run `ibkr gamma --only=spx --force` for SPX-only diagnostics."
	default:
		return "Retry later; if it repeats, check the daemon log and TWS market-data farm messages, or run `ibkr gamma --only=spy`."
	}
}

func spxUnavailableContext(c *rpc.GammaZeroComputed, reason string) string {
	switch normalizeSPXUnavailableReason(reason) {
	case "354":
		return "IBKR error 354 points to a missing market-data entitlement, not an after-hours condition."
	case "200":
		return "IBKR error 200 is a contract-resolution/routing rejection; it is not enough by itself to blame after-hours."
	case "throttled":
		return "Gateway pacing limited the SPX fan-out; repeated forced runs can make this worse."
	case "zero_magnitude":
		return "SPX rows landed, but not enough usable gamma magnitude survived the quality gates."
	}

	at, hasTime := gammaSPXReferenceTime(c)
	session := rpc.ClassifySession(at)
	sessionLabel := gammaSessionLabel(session)
	when := "Current timestamp"
	if hasTime {
		when = "Compute timestamp"
	}
	prefix := fmt.Sprintf("%s %s is %s.", when, gammaSPXSessionStamp(at), sessionLabel)
	switch normalizeSPXUnavailableReason(reason) {
	case "no_data", "fetch_canceled", "timeout":
		if session == rpc.SessionRTH {
			return prefix + " During regular option hours this is more likely a gateway/fetch failure unless daemon logs show IBKR 354/200."
		}
		return prefix + " Outside regular U.S. option hours, SPX option/model/OI ticks can be sparse; that may be a contributor, not a confirmed root cause."
	default:
		return prefix + " The warning does not by itself identify entitlement vs gateway vs session timing."
	}
}

func gammaSPXReferenceTime(c *rpc.GammaZeroComputed) (time.Time, bool) {
	if c != nil {
		if !c.AsOf.IsZero() {
			return c.AsOf, true
		}
		if !c.SpotAt.IsZero() {
			return c.SpotAt, true
		}
	}
	return time.Now(), false
}

func gammaSPXSessionStamp(t time.Time) string {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func gammaSessionLabel(session rpc.SessionClass) string {
	switch session {
	case rpc.SessionPre:
		return "pre-market, outside regular U.S. option hours"
	case rpc.SessionRTH:
		return "regular U.S. option hours"
	case rpc.SessionPost:
		return "post-market, outside regular U.S. option hours"
	default:
		return "closed, outside regular U.S. option hours"
	}
}

func normalizeSPXUnavailableReason(reason string) string {
	r := strings.ToLower(strings.TrimSpace(reason))
	r = strings.ReplaceAll(r, "_", " ")
	r = strings.ReplaceAll(r, "-", " ")
	switch {
	case r == "354", strings.Contains(r, "error 354"):
		return "354"
	case r == "200", strings.Contains(r, "error 200"), strings.Contains(r, "no security definition"):
		return "200"
	case r == "no data", strings.Contains(r, "no option data landed"):
		return "no_data"
	case strings.Contains(r, "throttl"):
		return "throttled"
	case strings.Contains(r, "zero magnitude"), strings.Contains(r, "no usable gex"):
		return "zero_magnitude"
	case r == "context canceled", r == "context cancelled", r == "canceled", r == "cancelled", r == "fetch canceled", r == "fetch cancelled":
		return "fetch_canceled"
	case r == "context deadline exceeded", r == "deadline exceeded", strings.Contains(r, "timeout"), strings.Contains(r, "timed out"):
		return "timeout"
	default:
		return strings.TrimSpace(reason)
	}
}

func trimSentencePeriod(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ".")
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
	case "agree:transition-gamma":
		spy := c.PerIndex["SPY"]
		spx := c.PerIndex["SPX"]
		if spy != nil && spy.ZeroGamma != nil && spx != nil && spx.ZeroGamma != nil {
			return fmt.Sprintf("SPY γ-zero %s · SPX γ-zero %s (both near transition · agreement)",
				formatSpotPrice(*spy.ZeroGamma), formatSpotPrice(*spx.ZeroGamma))
		}
		return "SPY and SPX both near γ-zero transition (agreement)"
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
	return fmt.Sprintf("SPY %s · SPX %s (DISAGREEMENT — model regimes differ; use per-index below as primary)",
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
		return fmt.Sprintf("%s @ %s", gammaRegimeWord(c), formatSpotPrice(*c.ZeroGamma))
	}
	switch c.GammaSign {
	case "positive":
		return "long-γ"
	case "negative":
		return "short-γ"
	}
	return "—"
}

func gammaRegimeWord(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "unavailable"
	}
	if c.GapPct != nil {
		switch {
		case *c.GapPct > gammaGapYellow:
			return "long-γ"
		case *c.GapPct >= -gammaGapYellow:
			return "transition"
		default:
			return "short-γ"
		}
	}
	switch c.GammaSign {
	case "positive":
		return "long-γ"
	case "negative":
		return "short-γ"
	}
	return "transition"
}

// gammaHeaderForScope returns the renderer's section header — varies
// with Result.Scope so SPX-only and combined runs don't claim to be
// SPY. Falls back to the SPY title for empty Scope (pre-step-5 result
// envelopes) so old daemon → new CLI mixes render unchanged.
func gammaHeaderForScope(r *rpc.GammaZeroSPXResult) string {
	if r == nil || r.Result == nil {
		return "Dealer gamma"
	}
	switch r.Result.Scope {
	case rpc.GammaZeroScopeSPX:
		return "Dealer gamma · SPX"
	case rpc.GammaZeroScopeCombined:
		return "Dealer gamma · SPY+SPX"
	default:
		return "Dealer gamma · SPY"
	}
}

// gammaSpotLabelForScope returns the underlying symbol to print next
// to a single spot value. Combined rendering usually prints separate
// SPY/SPX spot labels from PerIndex instead.
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
