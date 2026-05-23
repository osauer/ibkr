package daemon

import "github.com/osauer/ibkr/internal/rpc"

// composite-building logic. Mirrors the CLI's tallyComposite +
// verdict() exactly so wire consumers (MCP, dashboard generators)
// don't have to recompute the rollup from per-row status. The CLI
// keeps its own renderer-local tally for layout reasons, but both
// paths read off this helper's tallying conventions:
//
//   - Bands are derived from the same spec-default classifiers used
//     for streak persistence (regime_streaks.go) so the daemon stays
//     internally consistent between "what counts as a band transition"
//     and "what counts as a ranked row in the composite".
//   - HYG/SPY caps at yellow per the CLI's documented v1 floor (the
//     spec's red trigger requires multi-session history we don't track
//     here; the streak counter exposes the consecutive-sessions count
//     instead).
//   - Rows in computing / unavailable / error state stay unranked and
//     do not contribute to the green/yellow/red tally.

// verdictFloor is the minimum ranked-row count required to claim a
// verdict above "insufficient signal." Mirrors cli.verdictFloor —
// kept in sync by hand at v1; if a third reader appears, lift to a
// shared package.
const verdictFloor = 3

// buildRegimeComposite returns the same {verdict, green, yellow, red,
// ranked, unranked} rollup the CLI renders above the indicator rows,
// computed from the daemon-side classifiers. Always non-zero shape:
// when every row is unranked the verdict still surfaces honestly
// ("No ranked indicators — see rows below for state").
func buildRegimeComposite(r *rpc.RegimeSnapshotResult) rpc.RegimeComposite {
	if r == nil {
		return rpc.RegimeComposite{Verdict: "No ranked indicators — see rows below for state"}
	}
	bands := []string{
		bandForVIX(r.VIXTermStructure),
		bandForHYGSPY(r.HYGSPYDivergence),
		bandForUSDJPY(r.USDJPY),
		bandForGamma(r.GammaZero),
		bandForBreadth(r.Breadth),
	}
	var c rpc.RegimeComposite
	for _, b := range bands {
		switch b {
		case "green":
			c.GreenCount++
			c.RankedCount++
		case "yellow":
			c.YellowCount++
			c.RankedCount++
		case "red":
			c.RedCount++
			c.RankedCount++
		default:
			c.UnrankedCount++
		}
	}
	c.Verdict = verdictFor(c, len(bands))
	return c
}

// verdictFor maps the (red, yellow, ranked, total) tally onto the
// spec's interpretation table. Mirrors cli.regimeComposite.verdict
// — same words so MCP consumers can show the CLI's headline
// verbatim. total is the indicator count (5 today).
func verdictFor(c rpc.RegimeComposite, total int) string {
	switch {
	case c.RankedCount == 0:
		return "No ranked indicators — see rows below for state"
	case c.RankedCount < verdictFloor:
		return "Insufficient signal — too few indicators ranked"
	case c.RankedCount == total && c.RedCount == total:
		return "Full risk-off conditions"
	case c.RedCount >= 3:
		return "Regime shift likely — execute pre-committed plan"
	case c.RedCount >= 1:
		return "Watch closely, prep defensive moves"
	case c.YellowCount >= 3:
		return "Elevated alert — review positioning"
	default:
		return "Normal regime"
	}
}

// bandForVIX classifies the VIX/VIX3M row. Mirrors the CLI's
// rowVIXTerm path: unranked when status is anything other than ok/stale
// or when the ratio is missing.
func bandForVIX(r rpc.RegimeVIXTerm) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	return classifyVIXTermBand(r.Ratio)
}

// bandForHYGSPY classifies the HYG vs SPY row. v1 floor: never goes
// red — the spec's red trigger requires multi-session history. Mirrors
// the CLI's rowHYGSPY exactly so the composite tally agrees with what
// the CLI shows the user. The streak counter (not this rollup)
// surfaces the consecutive-sessions count that the spec's red trigger
// references.
func bandForHYGSPY(r rpc.RegimeHYGSPYDivergence) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	if r.HYG50DMA == nil || r.HYGPrice == nil {
		return ""
	}
	if *r.HYGPrice >= *r.HYG50DMA {
		return "green"
	}
	// HYG below 50dma. CLI maps both "near highs" and "not-near-highs"
	// to yellow; the unranked case is "SPY 52w-high context missing".
	if r.SPY52WHigh == nil || r.SPYPrice == nil {
		return ""
	}
	return "yellow"
}

// bandForUSDJPY classifies the USD/JPY row. Unranked on
// unavailable/error/computing rows or when the weekly change didn't
// land.
func bandForUSDJPY(r rpc.RegimeUSDJPY) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	return classifyUSDJPYBand(r.WeeklyChange)
}

// bandForGamma classifies the gamma row. Three paths matching the
// CLI's rowGamma logic: a real crossing reads on gap distance;
// no-crossing reads on the signed-profile sign; no data stays unranked.
func bandForGamma(r rpc.RegimeGammaZero) string {
	if r.Status != rpc.RegimeStatusOK || r.Envelope.Result == nil {
		return ""
	}
	c := r.Envelope.Result
	return classifyGammaBand(c.GapPct, c.GammaSign)
}

// bandForBreadth classifies the SPX breadth row. Gated on
// status=ok/stale AND envelope state=ready — the CLI does the same
// gate before pulling the value cell.
func bandForBreadth(r rpc.RegimeBreadth) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	if r.Envelope.State != rpc.BreadthStateReady {
		return ""
	}
	return classifyBreadthBand(r.Envelope.PctAbove50DMA)
}
