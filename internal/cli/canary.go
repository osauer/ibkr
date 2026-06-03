package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
)

var canaryPolicy = risk.DefaultPolicy()

type CanaryInput = rpc.CanaryInput
type CanaryResult = rpc.CanaryResult
type CanarySourceAsOf = rpc.CanarySourceAsOf
type CanarySourceFingerprints = rpc.CanarySourceFingerprints
type CanaryRow = rpc.CanaryRow
type CanaryMarketIndicator = rpc.CanaryMarketIndicator
type CanaryPortfolioSummary = rpc.CanaryPortfolioSummary
type CanaryMarketSummary = rpc.CanaryMarketSummary

func ComputeCanary(in CanaryInput) CanaryResult {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	accountFingerprint := rpc.BuildAccountFingerprint(&in.Account)
	positionsFingerprint := rpc.BuildPositionsFingerprint(&in.Positions, in.Account.NetLiquidation)
	regimeFingerprint := in.Regime.Fingerprint
	if regimeFingerprint.Key == "" {
		regimeFingerprint = rpc.BuildRegimeFingerprint(&in.Regime)
	}
	res := CanaryResult{
		AsOf:               now,
		SourceAsOf:         CanarySourceAsOf{Account: in.Account.AsOf, Positions: in.Positions.AsOf, Regime: in.Regime.AsOf},
		SourceFingerprints: CanarySourceFingerprints{Account: &accountFingerprint, Positions: &positionsFingerprint, Regime: &regimeFingerprint},
		Policy:             canaryPolicy.PolicyProfile(),
		PolicyProfile:      canaryPolicy.PolicyProfile(),
		PolicyVersion:      canaryPolicy.PolicyVersion(),
		PolicyFingerprint:  rpc.Fingerprint{Version: risk.PolicyFingerprintVersion, Key: canaryPolicy.FingerprintKey()},
		Portfolio:          summarizeCanaryPortfolio(in.Account, in.Positions),
		Market:             summarizeCanaryMarket(in.Regime, now),
		MarketIndicators:   canaryMarketIndicators(in.Regime, now),
		NotExecution:       "Read-only canary snapshot; no orders are placed by ibkr.",
	}
	sourceIssues := canarySourceIssues(in, now)

	rows := []CanaryRow{
		canaryMarginRow(res.Portfolio),
		canaryPnLShockRow(res.Portfolio),
		canaryTapeShockRow(res.Portfolio, res.Market),
		canaryMarketRow(res.Market),
		canaryExposureRow(res.Portfolio, res.Market),
		canaryConcentrationRow(res.Portfolio, res.Market),
		canaryOptionsRow(res.Portfolio, in.Positions, res.Market),
		canaryDataQualityRow(res.Market, in.Regime),
	}
	res.Signals = canarySignals(res.Portfolio, in.Positions, res.Market, in.Regime)
	res.Signals = canaryApplySourceBlocks(res.Signals, sourceIssues)
	res.Signals = append(res.Signals, canarySourceDataQualitySignals(sourceIssues)...)
	res.MarketConfirmation = canaryMarketConfirmation(res.Market)
	res.PortfolioFit = canaryPortfolioFit(res.Portfolio, res.Signals)
	res.InputHealth = canaryInputHealth(in, res.Market, sourceIssues)
	res.Direction, res.Severity = canaryDecisionState(res.MarketConfirmation, res.PortfolioFit, res.InputHealth, res.Market, res.Signals)
	res.Action = canaryAction(res.Direction, res.Severity, res.MarketConfirmation, res.PortfolioFit, res.InputHealth)
	res.PlannerModeHint = canaryPlannerModeFromAction(res.Action)
	res.PlannerReadiness = canaryPlannerReadinessFromAction(res.Action, res.Severity, res.InputHealth)
	res.PrimaryDrivers = canaryPrimaryDrivers(res.Signals)
	res.Summary = canaryDecisionSummary(res)
	overall := canaryOverallRow(res.Direction, res.Severity, res.Summary, res.Market, res.Portfolio)
	res.Rows = append([]CanaryRow{overall}, rows...)
	res.Warnings = canaryWarnings(res.Market, in.Regime, now)
	res.SourceHealth = canarySourceHealth(in, now, accountFingerprint, positionsFingerprint, regimeFingerprint, res.InputHealth, res.Market)
	res.Fingerprint = rpc.BuildCanaryFingerprint(&res)
	return res
}

func summarizeCanaryPortfolio(acct rpc.AccountResult, pos rpc.PositionsResult) CanaryPortfolioSummary {
	out := CanaryPortfolioSummary{
		BaseCurrency:   acct.BaseCurrency,
		NetLiquidation: acct.NetLiquidation,
	}
	if acct.NetLiquidation > 0 {
		out.CushionPct = canaryCurrentCushionPct(acct)
		out.LookAheadCushionPct = canaryLookAheadCushionPct(acct)
		if acct.GrossPositionValue > 0 {
			pct := acct.GrossPositionValue / acct.NetLiquidation * 100
			out.GrossExposurePctNLV = &pct
		}
		if acct.DailyPnL != nil {
			pct := *acct.DailyPnL / acct.NetLiquidation * 100
			out.DailyPnLPct = &pct
		}
	}
	if pos.Portfolio != nil {
		if pos.Portfolio.DollarDeltaBase != nil && acct.NetLiquidation > 0 {
			pct := math.Abs(*pos.Portfolio.DollarDeltaBase) / acct.NetLiquidation * 100
			out.NetDeltaPctNLV = &pct
		}
		if pos.Portfolio.GreeksTotal > 0 {
			out.OptionGreeks = fmt.Sprintf("%d/%d legs", pos.Portfolio.GreeksCoverage, pos.Portfolio.GreeksTotal)
		}
		for _, e := range pos.Portfolio.ExposureBase {
			if e.MarketValuePctNLV != nil {
				if out.LargestExposurePct == nil || math.Abs(*e.MarketValuePctNLV) > math.Abs(*out.LargestExposurePct) {
					pct := *e.MarketValuePctNLV
					out.LargestExposurePct = &pct
					out.LargestExposure = strings.ToUpper(e.Underlying)
				}
			}
			if e.DollarDeltaBase == nil || acct.NetLiquidation <= 0 {
				continue
			}
			pct := math.Abs(*e.DollarDeltaBase) / acct.NetLiquidation * 100
			gross := pct
			if out.GrossDeltaPctNLV != nil {
				gross += *out.GrossDeltaPctNLV
			}
			out.GrossDeltaPctNLV = &gross
			if out.LargestDeltaPctNLV == nil || pct > *out.LargestDeltaPctNLV {
				out.LargestDeltaPctNLV = &pct
				out.LargestDeltaExposure = strings.ToUpper(e.Underlying)
			}
		}
	}
	return out
}

func canaryCurrentCushionPct(acct rpc.AccountResult) *float64 {
	if acct.NetLiquidation <= 0 {
		return nil
	}
	switch {
	case acct.Cushion != 0:
		return new(acct.Cushion * 100)
	case acct.ExcessLiquidity != 0:
		return new(acct.ExcessLiquidity / acct.NetLiquidation * 100)
	case canaryHasActiveMarginContext(acct):
		return new(0.0)
	default:
		return nil
	}
}

func canaryLookAheadCushionPct(acct rpc.AccountResult) *float64 {
	if acct.NetLiquidation <= 0 {
		return nil
	}
	switch {
	case acct.LookAheadExcess != 0:
		return new(acct.LookAheadExcess / acct.NetLiquidation * 100)
	case acct.LookAheadMaintMargin > 0 || acct.LookAheadInitMargin > 0 || acct.LookAheadAvailable < 0:
		return new(0.0)
	default:
		return nil
	}
}

func canaryHasActiveMarginContext(acct rpc.AccountResult) bool {
	return acct.ExcessLiquidity < 0 ||
		acct.AvailableFunds < 0 ||
		acct.MaintenanceMargin > 0 ||
		acct.InitialMargin > 0
}

func summarizeCanaryMarket(r rpc.RegimeSnapshotResult, now time.Time) CanaryMarketSummary {
	out := CanaryMarketSummary{
		RegimeVerdict: r.Composite.Verdict,
		SPYPrice:      r.HYGSPYDivergence.SPYPrice,
		SPYChangePct:  r.HYGSPYDivergence.SPYChangePct,
		VIX:           r.VIXTermStructure.VIX,
		VIXChangePct:  r.VIXTermStructure.VIXChangePct,
	}
	contextClusters := canaryMarketContextClusters(r, now)
	rawBands := rawRegimeClusterBands(r)
	bands := confirmedRegimeClusterBands(r, rawBands)
	clusterBands := map[string]string{}
	for i, name := range []string{"vol", "credit", "funding", "fx", "gamma", "breadth"} {
		clusterBands[name] = bands[i]
		if rawBands[i] == "red" && bands[i] != "red" {
			out.UnconfirmedRedClusterNames = append(out.UnconfirmedRedClusterNames, name)
		}
	}
	statuses := map[string][]string{
		"vol":     {r.VIXTermStructure.Status, r.VolOfVol.Status},
		"credit":  {r.HYGSPYDivergence.Status, r.CreditSpreads.Status},
		"funding": {r.FundingStress.Status},
		"fx":      {r.USDJPY.Status},
		"gamma":   {r.GammaZero.Status},
		"breadth": {r.Breadth.Status},
	}
	for name, clusterBand := range clusterBands {
		switch clusterBand {
		case "red":
			out.RedClusterNames = append(out.RedClusterNames, name)
		case "yellow":
			out.YellowClusterNames = append(out.YellowClusterNames, name)
		}
		if clusterBand == "" {
			out.UnrankedClusters++
		} else {
			out.RankedClusters++
		}
		status := weakestStatus(statuses[name])
		if status == rpc.RegimeStatusComputing {
			out.ComputingClusters = append(out.ComputingClusters, name)
		}
		if status == rpc.RegimeStatusStale && !contextClusters[name] {
			out.StaleClusters = append(out.StaleClusters, name)
		}
		if clusterBand == "" {
			if !contextClusters[name] {
				out.AmbiguousClusters = append(out.AmbiguousClusters, name)
			}
		} else if !contextClusters[name] && (status == rpc.RegimeStatusError || status == rpc.RegimeStatusUnavailable || status == rpc.RegimeStatusComputing) {
			out.PartialClusters = append(out.PartialClusters, name)
		}
	}
	if canaryGammaDegraded(r.GammaZero) {
		out.DegradedClusters = append(out.DegradedClusters, "gamma")
	}
	slices.Sort(out.RedClusterNames)
	slices.Sort(out.YellowClusterNames)
	slices.Sort(out.UnconfirmedRedClusterNames)
	slices.Sort(out.AmbiguousClusters)
	slices.Sort(out.PartialClusters)
	slices.Sort(out.ComputingClusters)
	slices.Sort(out.DegradedClusters)
	slices.Sort(out.StaleClusters)
	out.RedClusters = len(out.RedClusterNames)
	out.YellowClusters = len(out.YellowClusterNames)
	return out
}

func canaryMarketContextClusters(r rpc.RegimeSnapshotResult, now time.Time) map[string]bool {
	out := map[string]bool{}
	if canaryGammaContextOnly(r.GammaZero) {
		out["gamma"] = true
	}
	if canaryVolClosedSessionContext(r, now) {
		out["vol"] = true
	}
	return out
}

func canaryGammaContextOnly(g rpc.RegimeGammaZero) bool {
	return g.Envelope.Result != nil &&
		g.Envelope.Result.Quality != nil &&
		g.Envelope.Result.Quality.Rankability == rpc.GammaRankabilityContextOnly
}

func canaryVolClosedSessionContext(r rpc.RegimeSnapshotResult, now time.Time) bool {
	if rpc.ClassifySession(now) == rpc.SessionRTH {
		return false
	}
	for _, q := range r.DataQuality {
		if q.Status == rpc.RegimeStatusStale && slices.Contains(q.StaleClusters, "vol") {
			return true
		}
	}
	switch r.VIXTermStructure.Status {
	case rpc.RegimeStatusStale:
		return true
	case rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
		return r.VolOfVol.Band != "" || r.VolOfVol.Status == rpc.RegimeStatusOK || r.VolOfVol.Status == rpc.RegimeStatusStale
	default:
		return false
	}
}

func canaryMarketIndicators(r rpc.RegimeSnapshotResult, now time.Time) []CanaryMarketIndicator {
	if now.IsZero() {
		now = time.Now()
	}
	contextClusters := canaryMarketContextClusters(r, now)
	rows := []struct {
		cluster string
		row     regimeRow
		asOf    *rpc.RegimeAsOfSummary
		date    string
		status  string
	}{
		{cluster: "vol", row: rowVIXTerm(now, r.VIXTermStructure), asOf: r.VIXTermStructure.AsOf, status: r.VIXTermStructure.Status},
		{cluster: "vol", row: rowVolOfVol(now, r.VolOfVol), asOf: r.VolOfVol.AsOf, date: r.VolOfVol.AsOfDate, status: r.VolOfVol.Status},
		{cluster: "credit", row: rowHYGSPY(now, r.HYGSPYDivergence), asOf: r.HYGSPYDivergence.AsOf, status: r.HYGSPYDivergence.Status},
		{cluster: "credit", row: rowCreditSpreads(now, r.CreditSpreads), asOf: r.CreditSpreads.AsOf, date: r.CreditSpreads.AsOfDate, status: r.CreditSpreads.Status},
		{cluster: "funding", row: rowFundingStress(now, r.FundingStress), asOf: r.FundingStress.AsOf, date: r.FundingStress.AsOfDate, status: r.FundingStress.Status},
		{cluster: "fx", row: rowUSDJPY(now, r.USDJPY), asOf: r.USDJPY.AsOf, status: r.USDJPY.Status},
		{cluster: "gamma", row: rowGamma(now, r.GammaZero), asOf: r.GammaZero.AsOf, status: r.GammaZero.Status},
		{cluster: "breadth", row: rowBreadth(now, r.Breadth), asOf: r.Breadth.AsOf, status: r.Breadth.Status},
	}
	out := make([]CanaryMarketIndicator, 0, len(rows))
	for _, item := range rows {
		reading := item.row.value
		if item.row.stateNote != "" && item.row.band == bandUnranked {
			reading = item.row.stateNote
		}
		contextOnly := contextClusters[item.cluster]
		out = append(out, CanaryMarketIndicator{
			Name:    item.row.name,
			Status:  canaryIndicatorStatus(item.row.band, item.status, contextOnly),
			AsOf:    canaryIndicatorAsOf(item.asOf, item.date, item.row.asOf),
			Reading: reading,
			Comment: canaryIndicatorComment(item.row, reading, contextOnly),
		})
	}
	return out
}

func canaryIndicatorStatus(b regimeBand, status string, contextOnly bool) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case rpc.RegimeStatusComputing, rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
		if b == bandUnranked {
			return "n/a"
		}
	}
	switch b {
	case bandGreen:
		return "green"
	case bandYellow:
		return "amber"
	case bandRed:
		return "red"
	default:
		if contextOnly {
			return "context"
		}
		return "n/a"
	}
}

func canaryIndicatorAsOf(meta *rpc.RegimeAsOfSummary, date, fallback string) string {
	if meta != nil {
		if meta.Date != "" {
			return meta.Date
		}
		if !meta.Time.IsZero() {
			return meta.Time.Local().Format("2006-01-02")
		}
		if meta.Label != "" {
			return meta.Label
		}
	}
	if date != "" {
		return date
	}
	return ifNonEmpty(fallback, "—")
}

func canaryIndicatorComment(row regimeRow, reading string, contextOnly bool) string {
	parts := []string{}
	add := func(part string) {
		part = strings.TrimSpace(part)
		if part == "" || strings.EqualFold(part, strings.TrimSpace(reading)) || slices.Contains(parts, part) {
			return
		}
		parts = append(parts, part)
	}
	add(row.reason)
	if row.status == rpc.RegimeStatusStale && !strings.Contains(strings.ToLower(row.reason), "context") {
		if contextOnly {
			add("closed-session cached context")
		} else {
			add("stale input")
		}
	}
	if row.quality != "" {
		add(strings.TrimSpace(strings.TrimPrefix(row.quality, "·")))
	}
	return strings.Join(parts, "; ")
}

func canaryGammaDegraded(g rpc.RegimeGammaZero) bool {
	if g.Envelope.Result == nil {
		return false
	}
	if g.Envelope.Result.Quality == nil {
		return true
	}
	switch g.Envelope.Result.Quality.Rankability {
	case rpc.GammaRankabilityRankable:
		return false
	case rpc.GammaRankabilityContextOnly:
		return false
	default:
		return true
	}
}

func weakestStatus(statuses []string) string {
	var sawComputing, sawUnavailable, sawError bool
	var sawStale bool
	for _, status := range statuses {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case rpc.RegimeStatusError:
			sawError = true
		case rpc.RegimeStatusUnavailable:
			sawUnavailable = true
		case rpc.RegimeStatusComputing:
			sawComputing = true
		case rpc.RegimeStatusStale:
			sawStale = true
		}
	}
	switch {
	case sawError:
		return rpc.RegimeStatusError
	case sawUnavailable:
		return rpc.RegimeStatusUnavailable
	case sawComputing:
		return rpc.RegimeStatusComputing
	case sawStale:
		return rpc.RegimeStatusStale
	default:
		return rpc.RegimeStatusOK
	}
}

func strongestBand(bands []string) string {
	seenYellow, seenGreen := false, false
	for _, b := range bands {
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "red":
			return "red"
		case "yellow":
			seenYellow = true
		case "green":
			seenGreen = true
		}
	}
	if seenYellow {
		return "yellow"
	}
	if seenGreen {
		return "green"
	}
	return ""
}

func canaryRow(title string, direction risk.SignalDirection, severity risk.SignalSeverity, guidance, evidence string) CanaryRow {
	return CanaryRow{
		Title:     title,
		Direction: direction,
		Severity:  severity,
		Guidance:  guidance,
		Evidence:  evidence,
	}
}

func canaryMarginRow(p CanaryPortfolioSummary) CanaryRow {
	cushion := canaryWorstCushionPct(p)
	if cushion != nil {
		switch {
		case *cushion < canaryPolicy.MarginUrgentPct:
			return canaryRow("Immediate margin safety", risk.DirectionDefensive, risk.SeverityUrgent, fmt.Sprintf("Move to cash-heavy / near-flat now; margin cushion is below %.0f%%.", canaryPolicy.MarginUrgentPct), canaryCushionEvidence(p))
		case *cushion < canaryPolicy.MarginActPct:
			return canaryRow("Immediate margin safety", risk.DirectionDefensive, risk.SeverityAct, fmt.Sprintf("Cut gross and net exposure until cushion is back above %.0f%%.", canaryPolicy.MarginTargetPct), canaryCushionEvidence(p))
		case *cushion < canaryPolicy.MarginWatchPct:
			return canaryRow("Immediate margin safety", risk.DirectionDefensive, risk.SeverityWatch, fmt.Sprintf("Do not add risk; prepare a reduction plan if cushion falls below %.0f%%.", canaryPolicy.MarginTargetPct), canaryCushionEvidence(p))
		}
		return canaryRow("Immediate margin safety", "", risk.SeverityObserve, "No forced margin action.", canaryCushionEvidence(p))
	}
	return canaryRow("Immediate margin safety", risk.DirectionDataQuality, risk.SeverityWatch, "No forced margin action, but confirm account cushion before sizing new risk.", "cushion unavailable")
}

func canaryPnLShockRow(p CanaryPortfolioSummary) CanaryRow {
	if p.DailyPnLPct == nil {
		return canaryRow("Portfolio P&L shock", risk.DirectionDataQuality, risk.SeverityWatch, "Daily P&L is unavailable; this indicator cannot confirm or reject a P&L shock.", "daily P&L unavailable")
	}
	pct := *p.DailyPnLPct
	absPct := math.Abs(pct)
	evidence := fmt.Sprintf("daily P&L %+.1f%% NLV", pct)
	if absPct >= canaryPolicy.DailyPnLActPct {
		if pct < 0 {
			return canaryRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityAct, "Large daily loss; run a defensive risk plan and protect liquidity.", evidence)
		}
		return canaryRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityWatch, "Large daily gain; protect gains and avoid accidental chase-risk.", evidence)
	}
	if absPct >= canaryPolicy.DailyPnLWatchPct {
		if pct < 0 {
			return canaryRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityWatch, "Daily loss is large enough to review risk before adding exposure.", evidence)
		}
		return canaryRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityWatch, "Daily gain is large enough to review sizing and opportunity deliberately.", evidence)
	}
	return canaryRow("Portfolio P&L shock", "", risk.SeverityObserve, "No daily P&L shock signal.", evidence)
}

func canaryMarketRow(m CanaryMarketSummary) CanaryRow {
	evidence := canaryMarketEvidence(m)
	switch {
	case m.RedClusters >= 3 && m.RankedClusters >= 4:
		return canaryRow("Confirmed market stress", risk.DirectionDefensive, risk.SeverityAct, "Reduce equity beta materially; reserve urgent action for margin or exposure rows.", evidence)
	case m.RedClusters >= 2 && m.RankedClusters >= 3:
		return canaryRow("Confirmed market stress", risk.DirectionDefensive, risk.SeverityAct, "Cut marginal longs and short-convexity exposure; keep only intentional hedged risk.", evidence)
	case canaryFastCarryUnwind(m):
		return canaryRow("Fast carry unwind", risk.DirectionDefensive, risk.SeverityAct, "Reduce fragile beta and short-vol exposure; FX stress is confirmed by tape or breadth.", evidence)
	case m.RedClusters == 1 && m.YellowClusters >= 1:
		return canaryRow("Early stress filtered", risk.DirectionDefensive, risk.SeverityWatch, "Wait for a second independent red cluster before major de-risking.", evidence)
	case m.YellowClusters >= 3:
		return canaryRow("Deteriorating tape", risk.DirectionDefensive, risk.SeverityWatch, "Freeze new risk and review hedges; no urgent action without red confirmation.", evidence)
	default:
		return canaryRow("Market stress", "", risk.SeverityObserve, "No market-regime de-risking trigger.", evidence)
	}
}

func canaryTapeShockRow(p CanaryPortfolioSummary, m CanaryMarketSummary) CanaryRow {
	evidence := canaryTapeEvidence(m)
	if m.SPYChangePct == nil && m.VIXChangePct == nil {
		return canaryRow("Index tape shock", risk.DirectionDataQuality, risk.SeverityWatch, "Direct SPY/VIX tape is unavailable; do not treat quiet regime clusters as complete overnight coverage.", evidence)
	}
	spyDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct)
	spyHardDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYHardDropPct)
	spyCrash := pctAtMost(m.SPYChangePct, canaryPolicy.SPYCrashPct)
	vixSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct)
	vixHardSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXHardSpikePct)
	confirmed := (spyDrop && vixSpike) || m.RedClusters >= 1
	switch {
	case spyCrash && confirmed:
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityAct, "Cut broad equity beta now; SPY is in a severe direct tape drawdown with confirmation.", evidence)
	case spyHardDrop && confirmed:
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityAct, "Cut marginal longs and pre-hedge remaining beta; direct SPY stress is confirmed.", evidence)
	case vixHardSpike && (spyDrop || m.RedClusters >= 1):
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityAct, "Reduce short-vol and high-beta exposure; direct VIX stress is confirmed.", evidence)
	case spyHardDrop || vixHardSpike || (spyDrop && vixSpike):
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityWatch, "Freeze new risk and run a second pass; direct overnight tape stress needs confirmation before urgent action.", evidence)
	case spyDrop || vixSpike:
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityWatch, "Freeze new risk; direct SPY/VIX tape is flashing early stress but not enough for defensive action alone.", evidence)
	default:
		return canaryRow("Index tape shock", "", risk.SeverityObserve, "No direct SPY/VIX overnight tape shock.", evidence)
	}
}

func canaryExposureRow(p CanaryPortfolioSummary, m CanaryMarketSummary) CanaryRow {
	gross := derefPct(p.GrossExposurePctNLV)
	delta := derefPct(p.NetDeltaPctNLV)
	grossDelta := derefPct(p.GrossDeltaPctNLV)
	evidence := fmt.Sprintf("gross %.0f%% NLV; net delta %.0f%% NLV; gross delta %.0f%% NLV", gross, delta, grossDelta)
	stressed := m.RedClusters >= 2 || canaryConfirmedTapeStress(m)
	switch {
	case (gross >= canaryPolicy.GrossExposureStressUrgentPct || delta >= canaryPolicy.NetDeltaStressUrgentPct || grossDelta >= canaryPolicy.GrossDeltaStressUrgentPct) && stressed:
		return canaryRow("US equity/options exposure", risk.DirectionDefensive, risk.SeverityUrgent, "Go near-flat on broad equity beta; close or hedge option delta first.", evidence)
	case (gross >= canaryPolicy.GrossExposureStressActPct || delta >= canaryPolicy.NetDeltaStressActPct || grossDelta >= canaryPolicy.GrossDeltaStressActPct) && stressed:
		return canaryRow("US equity/options exposure", risk.DirectionDefensive, risk.SeverityAct, "Cut 30-50% of net equity delta and avoid adding long gamma-dollar exposure.", evidence)
	case gross >= canaryPolicy.GrossExposureWatchPct || delta >= canaryPolicy.NetDeltaWatchPct || grossDelta >= canaryPolicy.GrossDeltaWatchPct:
		return canaryRow("US equity/options exposure", risk.DirectionRebalance, risk.SeverityWatch, "Exposure is high; rebalance toward risk limits without treating this as confirmed market stress.", evidence)
	default:
		return canaryRow("US equity/options exposure", "", risk.SeverityObserve, "No exposure-based de-risking trigger.", evidence)
	}
}

func canaryConcentrationRow(p CanaryPortfolioSummary, m CanaryMarketSummary) CanaryRow {
	if (p.LargestExposurePct == nil || p.LargestExposure == "") && (p.LargestDeltaPctNLV == nil || p.LargestDeltaExposure == "") {
		return canaryRow("Largest concentration", "", risk.SeverityObserve, "No concentration action from available base-currency exposure map.", "no dominant exposure")
	}
	pct := math.Abs(derefPct(p.LargestExposurePct))
	deltaPct := derefPct(p.LargestDeltaPctNLV)
	evidence := canaryConcentrationEvidence(p)
	if (pct >= canaryPolicy.SingleNameExposureWatchPct || deltaPct >= canaryPolicy.SingleNameDeltaWatchPct) && (m.RedClusters >= 2 || canaryConfirmedTapeStress(m)) {
		return canaryRow("Largest concentration", risk.DirectionDefensive, risk.SeverityAct, fmt.Sprintf("Trim this concentration before smaller positions; cap it below %.0f%% NLV in stress.", canaryPolicy.SingleNameTargetPct), evidence)
	}
	if pct >= canaryPolicy.SingleNameExposureWatchPct || deltaPct >= canaryPolicy.SingleNameDeltaWatchPct {
		return canaryRow("Largest concentration", risk.DirectionRebalance, risk.SeverityWatch, "Concentration is above risk limits; rebalance this title without treating it as confirmed market stress.", evidence)
	}
	return canaryRow("Largest concentration", "", risk.SeverityObserve, "No concentration trim required by the canary.", evidence)
}

func canaryOptionsRow(p CanaryPortfolioSummary, pos rpc.PositionsResult, m CanaryMarketSummary) CanaryRow {
	if pos.Portfolio == nil || pos.Portfolio.GreeksTotal == 0 {
		if len(pos.Options) > 0 {
			return canaryRow("Options convexity", risk.DirectionDataQuality, risk.SeverityWatch, "Option positions are present but greeks coverage is unavailable; do not escalate options-specific actions from this snapshot.", "option greeks unavailable")
		}
		return canaryRow("Options convexity", "", risk.SeverityObserve, "No option-greeks action from the current portfolio snapshot.", "no option greeks required")
	}
	coverage := float64(pos.Portfolio.GreeksCoverage) / float64(pos.Portfolio.GreeksTotal) * 100
	evidence := fmt.Sprintf("greeks %.0f%% covered (%s)", coverage, p.OptionGreeks)
	if coverage < canaryPolicy.OptionGreeksMinCoveragePct {
		return canaryRow("Options convexity", risk.DirectionDataQuality, risk.SeverityWatch, fmt.Sprintf("Do not escalate options-specific actions until greeks coverage is at least %.0f%%.", canaryPolicy.OptionGreeksMinCoveragePct), evidence)
	}
	if pos.Portfolio.Gamma != nil && *pos.Portfolio.Gamma < 0 && m.RedClusters >= 2 {
		return canaryRow("Options convexity", risk.DirectionDefensive, risk.SeverityAct, "Reduce negative-gamma structures first; prefer defined-risk or hedged residuals.", evidence)
	}
	return canaryRow("Options convexity", "", risk.SeverityObserve, "No option-convexity de-risking trigger.", evidence)
}

func canaryDataQualityRow(m CanaryMarketSummary, r rpc.RegimeSnapshotResult) CanaryRow {
	if canaryHasMarketDataIssue(m) && (m.RedClusters > 0 || m.YellowClusters > 0) {
		return canaryRow("Ambiguity filter", risk.DirectionDataQuality, risk.SeverityWatch, "Verify incomplete inputs before escalation; do not suppress confirmed independent red signals.", canaryAmbiguityEvidence(m))
	}
	if canaryHasMarketDataIssue(m) {
		return canaryRow("Ambiguity filter", risk.DirectionDataQuality, risk.SeverityWatch, "Refresh or verify incomplete market inputs; keep the canary on watch until coverage and freshness recover.", canaryAmbiguityEvidence(m))
	}
	if m.RankedClusters < 4 {
		return canaryRow("Data quality gate", risk.DirectionDataQuality, risk.SeverityWatch, "Verify market coverage before action; wait for at least four ranked regime clusters.", canaryMarketEvidence(m))
	}
	if r.GammaZero.Status == rpc.RegimeStatusComputing || r.Breadth.Status == rpc.RegimeStatusComputing {
		return canaryRow("Data quality gate", risk.DirectionDataQuality, risk.SeverityWatch, "Do not escalate on gamma/breadth until the daemon finishes the cached compute.", canaryMarketEvidence(m))
	}
	return canaryRow("Data quality gate", "", risk.SeverityObserve, "Market data coverage is sufficient for the canary policy.", canaryMarketEvidence(m))
}

func canaryOverallRow(direction risk.SignalDirection, severity risk.SignalSeverity, summary string, m CanaryMarketSummary, p CanaryPortfolioSummary) CanaryRow {
	return canaryRow("Portfolio canary", direction, severity, summary, fmt.Sprintf("%s; %s", canaryMarketEvidence(m), canaryPortfolioEvidence(p)))
}

const (
	canaryMarketNone      = "none"
	canaryMarketPartial   = "partial"
	canaryMarketConfirmed = "confirmed"
	canaryMarketBlocked   = "blocked"

	canaryPortfolioFitUnknown = "unknown"
	canaryPortfolioFitLow     = "low"
	canaryPortfolioFitMedium  = "medium"
	canaryPortfolioFitHigh    = "high"

	canaryInputOK       = "ok"
	canaryInputWarming  = "warming"
	canaryInputDegraded = "degraded"
	canaryInputFailed   = "failed"

	canaryActionStandDown     = "stand_down"
	canaryActionWatch         = "watch"
	canaryActionDefend        = "defend"
	canaryActionRebalance     = "rebalance"
	canaryActionDeploy        = "deploy"
	canaryActionConfirmInputs = "confirm_inputs"
)

func canaryMarketConfirmation(m CanaryMarketSummary) string {
	if m.RankedClusters < 4 {
		return canaryMarketBlocked
	}
	if canaryConfirmedMarketStress(m) || canaryConfirmedConstructiveTape(m) {
		return canaryMarketConfirmed
	}
	if canaryPartialMarketPressure(m) || canaryPartialConstructiveTape(m) {
		return canaryMarketPartial
	}
	return canaryMarketNone
}

func canaryConfirmedMarketStress(m CanaryMarketSummary) bool {
	return (m.RedClusters >= 2 && len(unhealthyConfirmingClusters(m)) == 0) || canaryConfirmedTapeStress(m)
}

func canaryPartialMarketPressure(m CanaryMarketSummary) bool {
	return m.RedClusters >= 1 ||
		m.YellowClusters >= 3 ||
		len(m.UnconfirmedRedClusterNames) > 0 ||
		pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct) ||
		pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct)
}

func canaryConfirmedConstructiveTape(m CanaryMarketSummary) bool {
	return pctAtLeast(m.SPYChangePct, canaryPolicy.SPYHardRallyPct) ||
		pctAtMost(m.VIXChangePct, canaryPolicy.VIXHardCrushPct)
}

func canaryPartialConstructiveTape(m CanaryMarketSummary) bool {
	return pctAtLeast(m.SPYChangePct, canaryPolicy.SPYRallyPct) ||
		pctAtMost(m.VIXChangePct, canaryPolicy.VIXCrushPct)
}

func canaryPortfolioFit(p CanaryPortfolioSummary, signals []risk.Signal) string {
	if p.NetLiquidation <= 0 {
		return canaryPortfolioFitUnknown
	}
	hasMedium := false
	for _, sig := range signals {
		if len(sig.BlockedBy) > 0 || sig.Direction == risk.DirectionDataQuality {
			continue
		}
		switch sig.ID {
		case risk.SignalGrossExposureHigh,
			risk.SignalNetDeltaHigh,
			risk.SignalGrossDeltaHigh,
			risk.SignalSingleNameExposureHigh,
			risk.SignalSingleNameDeltaHigh,
			risk.SignalShortConvexityHigh:
			return canaryPortfolioFitHigh
		case risk.SignalMarginCushionLow,
			risk.SignalLookAheadCushionLow,
			risk.SignalPortfolioPnLShock,
			risk.SignalOptionGreeksDegraded:
			hasMedium = true
		}
	}
	if hasMedium {
		return canaryPortfolioFitMedium
	}
	return canaryPortfolioFitLow
}

func canaryInputHealth(in CanaryInput, m CanaryMarketSummary, sourceIssues []canarySourceIssue) string {
	switch {
	case in.Account.NetLiquidation <= 0:
		return canaryInputFailed
	case in.Account.DailyPnL == nil:
		return canaryInputWarming
	case len(sourceIssues) > 0 || canaryHasMarketDataIssue(m):
		return canaryInputDegraded
	default:
		return canaryInputOK
	}
}

func canaryDecisionState(marketConfirmation, portfolioFit, inputHealth string, m CanaryMarketSummary, signals []risk.Signal) (risk.SignalDirection, risk.SignalSeverity) {
	if inputHealth == canaryInputFailed || marketConfirmation == canaryMarketBlocked {
		return risk.DirectionDataQuality, risk.SeverityWatch
	}
	if canaryHasConfirmedConstructiveSignal(signals) && inputHealth == canaryInputOK && portfolioFit == canaryPortfolioFitLow {
		return risk.DirectionConstructive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketConfirmed && portfolioFit == canaryPortfolioFitHigh {
		if inputHealth == canaryInputOK {
			if canaryHasUrgentPortfolioShape(signals) {
				return risk.DirectionDefensive, risk.SeverityUrgent
			}
			if canaryPanicMarket(m) {
				return risk.DirectionDefensive, risk.SeverityUrgent
			}
			return risk.DirectionDefensive, risk.SeverityAct
		}
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketConfirmed && portfolioFit == canaryPortfolioFitMedium {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketConfirmed && portfolioFit == canaryPortfolioFitLow {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketPartial && (portfolioFit == canaryPortfolioFitHigh || portfolioFit == canaryPortfolioFitMedium) {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketPartial && portfolioFit == canaryPortfolioFitLow {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketNone && portfolioFit == canaryPortfolioFitHigh {
		return risk.DirectionRebalance, risk.SeverityWatch
	}
	if inputHealth == canaryInputWarming || inputHealth == canaryInputDegraded {
		return risk.DirectionDataQuality, risk.SeverityWatch
	}
	return "", risk.SeverityObserve
}

func canaryHasUrgentPortfolioShape(signals []risk.Signal) bool {
	for _, sig := range signals {
		if len(sig.BlockedBy) > 0 || !severityRankAtLeast(sig.Severity, risk.SeverityUrgent) {
			continue
		}
		switch sig.ID {
		case risk.SignalGrossExposureHigh,
			risk.SignalNetDeltaHigh,
			risk.SignalGrossDeltaHigh,
			risk.SignalSingleNameExposureHigh,
			risk.SignalSingleNameDeltaHigh,
			risk.SignalShortConvexityHigh:
			return true
		}
	}
	return false
}

func canaryHasConfirmedConstructiveSignal(signals []risk.Signal) bool {
	for _, sig := range signals {
		if sig.Direction == risk.DirectionConstructive && severityRankAtLeast(sig.Severity, risk.SeverityWatch) && len(sig.BlockedBy) == 0 {
			return true
		}
	}
	return false
}

func canaryPanicMarket(m CanaryMarketSummary) bool {
	return m.RedClusters >= 3 ||
		pctAtMost(m.SPYChangePct, canaryPolicy.SPYCrashPct) ||
		(pctAtLeast(m.VIXChangePct, canaryPolicy.VIXHardSpikePct) && m.RedClusters >= 1)
}

func canaryAction(direction risk.SignalDirection, severity risk.SignalSeverity, marketConfirmation, portfolioFit, inputHealth string) string {
	if inputHealth == canaryInputFailed || marketConfirmation == canaryMarketBlocked {
		return canaryActionConfirmInputs
	}
	if direction == risk.DirectionDataQuality {
		if portfolioFit == canaryPortfolioFitHigh && marketConfirmation == canaryMarketPartial {
			return canaryActionWatch
		}
		return canaryActionConfirmInputs
	}
	if direction == risk.DirectionDefensive {
		if severityRankAtLeast(severity, risk.SeverityAct) && marketConfirmation == canaryMarketConfirmed && portfolioFit == canaryPortfolioFitHigh && inputHealth == canaryInputOK {
			return canaryActionDefend
		}
		return canaryActionWatch
	}
	if direction == risk.DirectionRebalance {
		return canaryActionRebalance
	}
	if direction == risk.DirectionConstructive {
		if marketConfirmation == canaryMarketConfirmed && inputHealth == canaryInputOK {
			return canaryActionDeploy
		}
		return canaryActionWatch
	}
	if severity == risk.SeverityWatch {
		return canaryActionWatch
	}
	return canaryActionStandDown
}

func canaryPlannerModeFromAction(action string) risk.PlannerMode {
	switch action {
	case canaryActionConfirmInputs:
		return risk.PlannerModeConfirmData
	case canaryActionDefend:
		return risk.PlannerModeDefend
	case canaryActionRebalance:
		return risk.PlannerModeRebalance
	case canaryActionDeploy:
		return risk.PlannerModeDeploy
	case canaryActionWatch:
		return risk.PlannerModeStage
	default:
		return risk.PlannerModeNone
	}
}

func canaryPlannerReadinessFromAction(action string, severity risk.SignalSeverity, inputHealth string) risk.PlannerReadiness {
	switch action {
	case canaryActionConfirmInputs:
		return risk.PlannerReadinessBlocked
	case canaryActionDefend, canaryActionDeploy:
		if inputHealth == canaryInputOK {
			return risk.PlannerReadinessReady
		}
		return risk.PlannerReadinessPrestage
	case canaryActionRebalance:
		return risk.PlannerReadinessReady
	case canaryActionWatch:
		return risk.PlannerReadinessPrestage
	default:
		if severity == risk.SeverityWatch {
			return risk.PlannerReadinessWatch
		}
		return risk.PlannerReadinessNone
	}
}

func canaryDecisionSummary(r CanaryResult) string {
	switch r.Action {
	case canaryActionDefend:
		return "Market stress is confirmed against a vulnerable portfolio; run a defensive risk plan."
	case canaryActionWatch:
		if r.MarketConfirmation == canaryMarketPartial {
			return "Market pressure is developing and the portfolio is exposed; freeze new risk and stage reductions."
		}
		return "Watch this portfolio against market weather; do not run a major action from this snapshot alone."
	case canaryActionRebalance:
		return "Portfolio shape is outside risk limits, but market stress is not confirmed; rebalance through the portfolio-risk workflow."
	case canaryActionDeploy:
		return "Constructive pressure is present and input health is clean; deploy only inside risk budget."
	case canaryActionConfirmInputs:
		return "Confirm input health before treating the canary as a market-context signal."
	default:
		return "No market-context canary action."
	}
}

func canarySignals(p CanaryPortfolioSummary, pos rpc.PositionsResult, m CanaryMarketSummary, r rpc.RegimeSnapshotResult) []risk.Signal {
	signals := []risk.Signal{}
	signals = append(signals, canaryMarginSignals(p)...)
	signals = append(signals, canaryPnLSignals(p)...)
	signals = append(signals, canaryTapeSignals(p, m)...)
	signals = append(signals, canaryRegimeSignals(m)...)
	signals = append(signals, canaryExposureSignals(p, m)...)
	signals = append(signals, canaryConcentrationSignals(p, m)...)
	signals = append(signals, canaryOptionSignals(pos, m)...)
	signals = append(signals, canaryDataQualitySignals(m, r)...)
	for i := range signals {
		if signals[i].Posture == "" {
			signals[i].Posture = canarySignalPosture(signals[i].Direction)
		}
	}
	return signals
}

func canaryMarginSignals(p CanaryPortfolioSummary) []risk.Signal {
	out := []risk.Signal{}
	addCushion := func(id risk.SignalID, metric string, observed *float64) {
		if observed == nil {
			return
		}
		severity, threshold, ok := canaryCushionSeverity(*observed)
		if !ok {
			return
		}
		out = append(out, risk.Signal{
			ID:         id,
			Direction:  risk.DirectionDefensive,
			Severity:   severity,
			Metric:     metric,
			Observed:   observed,
			Threshold:  new(threshold),
			Unit:       "pct_nlv",
			Evidence:   pctEvidence(metric, *observed),
			Confidence: "high",
		})
		if severity == risk.SeverityAct || severity == risk.SeverityUrgent {
			out[len(out)-1].Target = new(canaryPolicy.MarginTargetPct)
		}
	}
	addCushion(risk.SignalMarginCushionLow, "cushion", p.CushionPct)
	addCushion(risk.SignalLookAheadCushionLow, "lookahead_cushion", p.LookAheadCushionPct)
	return out
}

func canaryCushionSeverity(v float64) (risk.SignalSeverity, float64, bool) {
	switch {
	case v < canaryPolicy.MarginUrgentPct:
		return risk.SeverityUrgent, canaryPolicy.MarginUrgentPct, true
	case v < canaryPolicy.MarginActPct:
		return risk.SeverityAct, canaryPolicy.MarginActPct, true
	case v < canaryPolicy.MarginWatchPct:
		return risk.SeverityWatch, canaryPolicy.MarginWatchPct, true
	default:
		return "", 0, false
	}
}

func canaryPnLSignals(p CanaryPortfolioSummary) []risk.Signal {
	if p.DailyPnLPct == nil {
		return []risk.Signal{{
			ID:               risk.SignalRiskDataDegraded,
			Direction:        risk.DirectionDataQuality,
			Severity:         risk.SeverityWatch,
			Subject:          "account.daily_pnl",
			Metric:           "daily_pnl_pct_nlv",
			Evidence:         "daily P&L unavailable",
			Confidence:       "medium-low",
			ConfidenceImpact: "P&L shock indicator unavailable",
			BlockedBy:        []string{"account.daily_pnl"},
		}}
	}
	pct := *p.DailyPnLPct
	absPct := math.Abs(pct)
	if absPct < canaryPolicy.DailyPnLWatchPct {
		return nil
	}
	direction := risk.DirectionDefensive
	severity := risk.SeverityWatch
	threshold := canaryPolicy.DailyPnLWatchPct
	confidenceImpact := ""
	if pct < 0 && absPct >= canaryPolicy.DailyPnLActPct {
		severity = risk.SeverityAct
		threshold = canaryPolicy.DailyPnLActPct
	} else if pct > 0 && absPct >= canaryPolicy.DailyPnLActPct {
		threshold = canaryPolicy.DailyPnLActPct
		confidenceImpact = "protect gains; not deployable without clean risk budget"
	}
	return []risk.Signal{{
		ID:               risk.SignalPortfolioPnLShock,
		Direction:        direction,
		Severity:         severity,
		Metric:           "daily_pnl_pct_nlv",
		Observed:         new(pct),
		Threshold:        new(threshold),
		Unit:             "pct_nlv",
		Evidence:         fmt.Sprintf("daily P&L %+.1f%% NLV", pct),
		Confidence:       "high",
		ConfidenceImpact: confidenceImpact,
	}}
}

func canaryTapeSignals(p CanaryPortfolioSummary, m CanaryMarketSummary) []risk.Signal {
	out := []risk.Signal{}
	spyDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct)
	vixSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct)
	confirmedDrop := (spyDrop && vixSpike) || m.RedClusters >= 1
	confirmedVIXSpike := spyDrop || m.RedClusters >= 1
	if m.SPYChangePct != nil {
		switch {
		case *m.SPYChangePct <= canaryPolicy.SPYCrashPct:
			severity, blockedBy := confirmedSignalSeverity(confirmedDrop)
			out = append(out, tapeSignal(risk.SignalMarketSelloffViolent, risk.DirectionDefensive, severity, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYCrashPct, blockedBy...))
		case *m.SPYChangePct <= canaryPolicy.SPYHardDropPct:
			severity, blockedBy := confirmedSignalSeverity(confirmedDrop)
			out = append(out, tapeSignal(risk.SignalMarketSelloffViolent, risk.DirectionDefensive, severity, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYHardDropPct, blockedBy...))
		case *m.SPYChangePct <= canaryPolicy.SPYDropPct:
			out = append(out, tapeSignal(risk.SignalMarketSelloffViolent, risk.DirectionDefensive, risk.SeverityWatch, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYDropPct))
		case *m.SPYChangePct >= canaryPolicy.SPYHardRallyPct:
			out = append(out, tapeSignal(risk.SignalMarketRallyViolent, risk.DirectionConstructive, risk.SeverityAct, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYHardRallyPct))
		case *m.SPYChangePct >= canaryPolicy.SPYRallyPct:
			out = append(out, tapeSignal(risk.SignalMarketRallyViolent, risk.DirectionConstructive, risk.SeverityWatch, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYRallyPct))
		}
	}
	if m.VIXChangePct != nil {
		switch {
		case *m.VIXChangePct >= canaryPolicy.VIXHardSpikePct:
			severity, blockedBy := confirmedSignalSeverity(confirmedVIXSpike)
			out = append(out, tapeSignal(risk.SignalVolSpikeConfirmed, risk.DirectionDefensive, severity, "vix_change_pct", *m.VIXChangePct, canaryPolicy.VIXHardSpikePct, blockedBy...))
		case *m.VIXChangePct >= canaryPolicy.VIXSpikePct:
			out = append(out, tapeSignal(risk.SignalVolSpikeConfirmed, risk.DirectionDefensive, risk.SeverityWatch, "vix_change_pct", *m.VIXChangePct, canaryPolicy.VIXSpikePct))
		case *m.VIXChangePct <= canaryPolicy.VIXHardCrushPct:
			out = append(out, tapeSignal(risk.SignalVolCrushConfirmed, risk.DirectionConstructive, risk.SeverityAct, "vix_change_pct", *m.VIXChangePct, canaryPolicy.VIXHardCrushPct))
		case *m.VIXChangePct <= canaryPolicy.VIXCrushPct:
			out = append(out, tapeSignal(risk.SignalVolCrushConfirmed, risk.DirectionConstructive, risk.SeverityWatch, "vix_change_pct", *m.VIXChangePct, canaryPolicy.VIXCrushPct))
		}
	}
	return out
}

func confirmedSignalSeverity(confirmed bool) (risk.SignalSeverity, []string) {
	if confirmed {
		return risk.SeverityAct, nil
	}
	return risk.SeverityWatch, []string{"confirmation"}
}

func tapeSignal(id risk.SignalID, direction risk.SignalDirection, severity risk.SignalSeverity, metric string, observed, threshold float64, blockedBy ...string) risk.Signal {
	sig := risk.Signal{
		ID:         id,
		Direction:  direction,
		Severity:   severity,
		Metric:     metric,
		Observed:   new(observed),
		Threshold:  new(threshold),
		Unit:       "pct",
		Evidence:   fmt.Sprintf("%s %+.2f%%", metric, observed),
		Confidence: "medium",
	}
	if len(blockedBy) > 0 {
		sig.BlockedBy = append([]string(nil), blockedBy...)
		sig.ConfidenceImpact = "requires independent confirmation before action"
	}
	return sig
}

func canaryRegimeSignals(m CanaryMarketSummary) []risk.Signal {
	out := []risk.Signal{}
	switch {
	case m.RedClusters >= 2 && m.RankedClusters >= 3:
		observed := float64(m.RedClusters)
		threshold := 2.0
		sig := risk.Signal{ID: risk.SignalRegimeStressConfirmed, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Metric: "red_clusters", Observed: &observed, Threshold: &threshold, Evidence: canaryMarketEvidence(m), Confidence: "medium"}
		if unhealthy := unhealthyConfirmingClusters(m); len(unhealthy) > 0 {
			sig.BlockedBy = unhealthy
			sig.ConfidenceImpact = "confirmed stress includes unhealthy cluster input; verify before severe market-only action"
		}
		out = append(out, sig)
	case canaryFastCarryUnwind(m):
		observed := 1.0
		threshold := 1.0
		out = append(out, risk.Signal{ID: risk.SignalFXCarryUnwind, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Subject: "fx", Metric: "red_fx_cluster_with_tape_confirmation", Observed: &observed, Threshold: &threshold, Evidence: canaryMarketEvidence(m), Confidence: "medium"})
	case m.RedClusters == 1 && m.YellowClusters >= 1:
		observed := float64(m.RedClusters)
		threshold := 1.0
		out = append(out, risk.Signal{ID: risk.SignalRegimeStressEarly, Direction: risk.DirectionDefensive, Severity: risk.SeverityWatch, Metric: "red_clusters", Observed: &observed, Threshold: &threshold, Evidence: canaryMarketEvidence(m), Confidence: "medium"})
	}
	if slices.Contains(m.RedClusterNames, "gamma") {
		observed := 1.0
		out = append(out, risk.Signal{ID: risk.SignalGammaRed, Direction: risk.DirectionDefensive, Severity: risk.SeverityWatch, Subject: "gamma", Metric: "red_cluster", Observed: &observed, Evidence: "gamma cluster red", Confidence: "medium", ConfidenceImpact: "lower when gamma is degraded"})
	}
	return out
}

func unhealthyConfirmingClusters(m CanaryMarketSummary) []string {
	out := []string{}
	for _, cluster := range canaryUniqueClusters(m.DegradedClusters, m.StaleClusters, m.PartialClusters, m.ComputingClusters) {
		if slices.Contains(m.RedClusterNames, cluster) {
			out = append(out, cluster)
		}
	}
	slices.Sort(out)
	return out
}

func canaryExposureSignals(p CanaryPortfolioSummary, m CanaryMarketSummary) []risk.Signal {
	stressed := m.RedClusters >= 2 || canaryConfirmedTapeStress(m)
	out := []risk.Signal{}
	out = appendExposureSignal(out, risk.SignalGrossExposureHigh, "gross_exposure_pct_nlv", p.GrossExposurePctNLV, canaryPolicy.GrossExposureWatchPct, canaryPolicy.GrossExposureStressActPct, canaryPolicy.GrossExposureStressUrgentPct, stressed)
	out = appendExposureSignal(out, risk.SignalNetDeltaHigh, "net_delta_pct_nlv", p.NetDeltaPctNLV, canaryPolicy.NetDeltaWatchPct, canaryPolicy.NetDeltaStressActPct, canaryPolicy.NetDeltaStressUrgentPct, stressed)
	out = appendExposureSignal(out, risk.SignalGrossDeltaHigh, "gross_delta_pct_nlv", p.GrossDeltaPctNLV, canaryPolicy.GrossDeltaWatchPct, canaryPolicy.GrossDeltaStressActPct, canaryPolicy.GrossDeltaStressUrgentPct, stressed)
	return out
}

func appendExposureSignal(out []risk.Signal, id risk.SignalID, metric string, observed *float64, watchThreshold, stressActThreshold, stressUrgentThreshold float64, stressed bool) []risk.Signal {
	if observed == nil {
		return out
	}
	threshold := watchThreshold
	severity := risk.SeverityWatch
	direction := risk.DirectionRebalance
	if stressed {
		direction = risk.DirectionDefensive
		switch {
		case *observed >= stressUrgentThreshold:
			severity = risk.SeverityUrgent
			threshold = stressUrgentThreshold
		case *observed >= stressActThreshold:
			severity = risk.SeverityAct
			threshold = stressActThreshold
		case *observed >= watchThreshold:
			severity = risk.SeverityWatch
		default:
			return out
		}
	} else if *observed < watchThreshold {
		return out
	}
	return append(out, risk.Signal{
		ID:         id,
		Direction:  direction,
		Severity:   severity,
		Metric:     metric,
		Observed:   observed,
		Threshold:  new(threshold),
		Unit:       "pct_nlv",
		Evidence:   fmt.Sprintf("%s %.0f%% NLV", metric, *observed),
		Confidence: "high",
	})
}

func canaryConcentrationSignals(p CanaryPortfolioSummary, m CanaryMarketSummary) []risk.Signal {
	stressed := m.RedClusters >= 2 || canaryConfirmedTapeStress(m)
	severity := risk.SeverityWatch
	direction := risk.DirectionRebalance
	if stressed {
		severity = risk.SeverityAct
		direction = risk.DirectionDefensive
	}
	out := []risk.Signal{}
	if p.LargestExposurePct != nil && math.Abs(*p.LargestExposurePct) >= canaryPolicy.SingleNameExposureWatchPct {
		observed := math.Abs(*p.LargestExposurePct)
		out = append(out, risk.Signal{ID: risk.SignalSingleNameExposureHigh, Direction: direction, Severity: severity, Subject: p.LargestExposure, Metric: "market_value_pct_nlv", Observed: &observed, Threshold: new(canaryPolicy.SingleNameExposureWatchPct), Target: new(canaryPolicy.SingleNameTargetPct), Unit: "pct_nlv", Evidence: fmt.Sprintf("%s market %.0f%% NLV", p.LargestExposure, observed), Confidence: "high"})
	}
	if p.LargestDeltaPctNLV != nil && *p.LargestDeltaPctNLV >= canaryPolicy.SingleNameDeltaWatchPct {
		out = append(out, risk.Signal{ID: risk.SignalSingleNameDeltaHigh, Direction: direction, Severity: severity, Subject: p.LargestDeltaExposure, Metric: "delta_pct_nlv", Observed: p.LargestDeltaPctNLV, Threshold: new(canaryPolicy.SingleNameDeltaWatchPct), Target: new(canaryPolicy.SingleNameTargetPct), Unit: "pct_nlv", Evidence: fmt.Sprintf("%s delta %.0f%% NLV", p.LargestDeltaExposure, *p.LargestDeltaPctNLV), Confidence: "high"})
	}
	return out
}

func canaryOptionSignals(pos rpc.PositionsResult, m CanaryMarketSummary) []risk.Signal {
	if pos.Portfolio == nil || pos.Portfolio.GreeksTotal == 0 {
		if len(pos.Options) > 0 {
			return []risk.Signal{{
				ID:               risk.SignalOptionGreeksDegraded,
				Direction:        risk.DirectionDataQuality,
				Severity:         risk.SeverityWatch,
				Metric:           "option_greeks_coverage_pct",
				Evidence:         "option greeks unavailable",
				Confidence:       "medium-low",
				ConfidenceImpact: "blocks option-specific planning",
				BlockedBy:        []string{"option_greeks"},
			}}
		}
		return nil
	}
	out := []risk.Signal{}
	coverage := float64(pos.Portfolio.GreeksCoverage) / float64(pos.Portfolio.GreeksTotal) * 100
	if coverage < canaryPolicy.OptionGreeksMinCoveragePct {
		out = append(out, risk.Signal{ID: risk.SignalOptionGreeksDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "option_greeks_coverage_pct", Observed: new(coverage), Threshold: new(canaryPolicy.OptionGreeksMinCoveragePct), Unit: "pct", Evidence: fmt.Sprintf("greeks %.0f%% covered", coverage), Confidence: "medium-low", ConfidenceImpact: "blocks option-specific planning"})
	}
	if pos.Portfolio.Gamma != nil && *pos.Portfolio.Gamma < 0 && m.RedClusters >= 2 {
		out = append(out, risk.Signal{ID: risk.SignalShortConvexityHigh, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Metric: "portfolio_gamma", Observed: pos.Portfolio.Gamma, Evidence: "negative portfolio gamma in confirmed market stress", Confidence: "medium"})
	}
	return out
}

func canaryDataQualitySignals(m CanaryMarketSummary, r rpc.RegimeSnapshotResult) []risk.Signal {
	out := []risk.Signal{}
	blockedBy := canaryUniqueClusters(m.AmbiguousClusters, m.PartialClusters, m.DegradedClusters, m.ComputingClusters)
	if len(blockedBy) > 0 {
		observed := float64(len(blockedBy))
		out = append(out, risk.Signal{ID: risk.SignalRiskDataDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "degraded_inputs", Observed: &observed, Evidence: canaryAmbiguityEvidence(m), Confidence: "medium-low", ConfidenceImpact: "requires verification before severe action", BlockedBy: blockedBy})
	}
	if len(m.StaleClusters) > 0 {
		observed := float64(len(m.StaleClusters))
		out = append(out, risk.Signal{ID: risk.SignalMarketDataStale, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "stale_clusters", Observed: &observed, Evidence: "stale " + strings.Join(m.StaleClusters, ","), Confidence: "medium-low", ConfidenceImpact: "requires fresh data", BlockedBy: m.StaleClusters})
	}
	for _, w := range r.WarningDetails {
		if strings.TrimSpace(w.Scope) != "" && strings.Contains(strings.ToLower(w.Severity), "data") {
			out = append(out, risk.Signal{ID: risk.SignalRiskDataDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Subject: w.Scope, Evidence: canaryWarningLine(w), Confidence: "medium-low", ConfidenceImpact: "source warning"})
		}
	}
	return out
}

func canaryUniqueClusters(groups ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, group := range groups {
		for _, cluster := range group {
			cluster = strings.TrimSpace(cluster)
			if cluster == "" || seen[cluster] {
				continue
			}
			seen[cluster] = true
			out = append(out, cluster)
		}
	}
	slices.Sort(out)
	return out
}

func canarySignalPosture(direction risk.SignalDirection) risk.PortfolioPosture {
	switch direction {
	case risk.DirectionDefensive:
		return risk.PortfolioPostureThreat
	case risk.DirectionConstructive:
		return risk.PortfolioPostureOpportunity
	case risk.DirectionRebalance:
		return risk.PortfolioPostureRebalance
	case risk.DirectionMixed:
		return risk.PortfolioPostureThreatOpportunity
	case risk.DirectionDataQuality:
		return risk.PortfolioPostureConfirmData
	default:
		return risk.PortfolioPostureNeutral
	}
}

type canarySourceIssue struct {
	Source string
	Status string
	Reason string
}

func canarySourceIssues(in CanaryInput, now time.Time) []canarySourceIssue {
	issues := []canarySourceIssue{}
	if canarySourceStale(in.Account.AsOf, now) {
		issues = append(issues, canarySourceIssue{Source: "account", Status: rpc.RegimeStatusStale, Reason: "account snapshot stale"})
	}
	if !in.Positions.AsOf.IsZero() && canarySourceStale(in.Positions.AsOf, now) {
		issues = append(issues, canarySourceIssue{Source: "positions", Status: rpc.RegimeStatusStale, Reason: "positions snapshot stale"})
	}
	return issues
}

func canarySourceStale(asOf, now time.Time) bool {
	return !asOf.IsZero() && canarySourceAgeSeconds(now, asOf) > canarySourceMaxAgeSeconds(now)
}

func canaryApplySourceBlocks(signals []risk.Signal, issues []canarySourceIssue) []risk.Signal {
	if len(issues) == 0 {
		return signals
	}
	accountBlocked := canarySourceIssuePresent(issues, "account")
	positionsBlocked := canarySourceIssuePresent(issues, "positions")
	for i := range signals {
		if accountBlocked && canarySignalDependsOnAccount(signals[i].ID) {
			canaryBlockSignal(&signals[i], "account", "requires fresh account snapshot")
		}
		if positionsBlocked && canarySignalDependsOnPositions(signals[i].ID) {
			canaryBlockSignal(&signals[i], "positions", "requires fresh positions snapshot")
		}
	}
	return signals
}

func canarySourceIssuePresent(issues []canarySourceIssue, source string) bool {
	for _, issue := range issues {
		if issue.Source == source {
			return true
		}
	}
	return false
}

func canarySignalDependsOnAccount(id risk.SignalID) bool {
	switch id {
	case risk.SignalMarginCushionLow,
		risk.SignalLookAheadCushionLow,
		risk.SignalPortfolioPnLShock,
		risk.SignalGrossExposureHigh:
		return true
	default:
		return false
	}
}

func canarySignalDependsOnPositions(id risk.SignalID) bool {
	switch id {
	case risk.SignalNetDeltaHigh,
		risk.SignalGrossDeltaHigh,
		risk.SignalSingleNameExposureHigh,
		risk.SignalSingleNameDeltaHigh,
		risk.SignalOptionGreeksDegraded,
		risk.SignalShortConvexityHigh:
		return true
	default:
		return false
	}
}

func canaryBlockSignal(sig *risk.Signal, source, impact string) {
	if sig == nil {
		return
	}
	sig.BlockedBy = appendUniqueString(sig.BlockedBy, source)
	sig.Confidence = "medium-low"
	sig.ConfidenceImpact = impact
}

func appendUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || slices.Contains(values, value) {
		return values
	}
	values = append(values, value)
	slices.Sort(values)
	return values
}

func canarySourceDataQualitySignals(issues []canarySourceIssue) []risk.Signal {
	if len(issues) == 0 {
		return nil
	}
	blockedBy := []string{}
	for _, issue := range issues {
		blockedBy = appendUniqueString(blockedBy, issue.Source)
	}
	observed := float64(len(blockedBy))
	return []risk.Signal{{
		ID:               risk.SignalRiskDataDegraded,
		Direction:        risk.DirectionDataQuality,
		Severity:         risk.SeverityWatch,
		Metric:           "stale_sources",
		Observed:         &observed,
		Evidence:         "stale sources: " + strings.Join(blockedBy, ","),
		Confidence:       "medium-low",
		ConfidenceImpact: "requires fresh account/position source before acting on dependent signals",
		BlockedBy:        blockedBy,
	}}
}

func signalSeverityRank(s risk.SignalSeverity) int {
	switch s {
	case risk.SeverityUrgent:
		return 3
	case risk.SeverityAct:
		return 2
	case risk.SeverityWatch:
		return 1
	default:
		return 0
	}
}

func severityRankAtLeast(got, want risk.SignalSeverity) bool {
	return signalSeverityRank(got) >= signalSeverityRank(want)
}

func canaryPrimaryDrivers(signals []risk.Signal) []risk.SignalID {
	type rankedSignal struct {
		id   risk.SignalID
		rank int
	}
	ranked := []rankedSignal{}
	for _, s := range signals {
		if s.Direction == risk.DirectionDataQuality {
			continue
		}
		ranked = append(ranked, rankedSignal{id: s.ID, rank: signalSeverityRank(s.Severity)})
	}
	if len(ranked) == 0 {
		for _, s := range signals {
			ranked = append(ranked, rankedSignal{id: s.ID, rank: signalSeverityRank(s.Severity)})
		}
	}
	slices.SortStableFunc(ranked, func(a, b rankedSignal) int {
		return cmp.Compare(b.rank, a.rank)
	})
	out := []risk.SignalID{}
	seen := map[risk.SignalID]bool{}
	for _, s := range ranked {
		if seen[s.id] {
			continue
		}
		seen[s.id] = true
		out = append(out, s.id)
		if len(out) == 5 {
			break
		}
	}
	return out
}

func canaryWarnings(m CanaryMarketSummary, r rpc.RegimeSnapshotResult, now time.Time) []string {
	var warnings []string
	detailWarnings, detailedClusters := canaryRegimeWarningDetails(r.WarningDetails, now)
	ambiguousClusters := canaryClustersWithoutDetailedWarning(m.AmbiguousClusters, detailedClusters)
	partialClusters := canaryClustersWithoutDetailedWarning(m.PartialClusters, detailedClusters)
	degradedClusters := canaryClustersWithoutDetailedWarning(m.DegradedClusters, detailedClusters)
	staleClusters := canaryClustersWithoutDetailedWarning(m.StaleClusters, detailedClusters)

	if len(ambiguousClusters) > 0 {
		warnings = append(warnings, "ambiguous clusters: "+canaryClusterList(ambiguousClusters))
	}
	if len(partialClusters) > 0 {
		warnings = append(warnings, "partial clusters: "+canaryClusterList(partialClusters))
	}
	if len(degradedClusters) > 0 {
		warnings = append(warnings, "degraded clusters: "+canaryClusterList(degradedClusters))
	}
	if len(staleClusters) > 0 {
		warnings = append(warnings, "stale clusters: "+canaryClusterList(staleClusters))
	}
	warnings = append(warnings, detailWarnings...)
	return warnings
}

func canaryRegimeWarningDetails(details []rpc.RegimeWarning, now time.Time) ([]string, map[string]bool) {
	lines := []string{}
	clusters := map[string]bool{}
	for _, w := range details {
		if canaryRegimeWarningIsContext(w, now) {
			continue
		}
		line := canaryWarningLine(w)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if cluster := canaryRegimeWarningCluster(w); cluster != "" {
			clusters[cluster] = true
		}
	}
	return lines, clusters
}

func canaryRegimeWarningIsContext(w rpc.RegimeWarning, now time.Time) bool {
	lower := strings.ToLower(strings.Join([]string{w.Code, w.Scope, w.Severity, w.Message, w.Impact, w.Action}, " "))
	if strings.Contains(lower, "gamma") && (strings.Contains(lower, "context_only") || strings.Contains(lower, "context only") || strings.Contains(lower, "displayed as context")) {
		return true
	}
	if rpc.ClassifySession(now) != rpc.SessionRTH &&
		strings.Contains(lower, "vix_term_structure") &&
		(strings.Contains(lower, "stale") || strings.Contains(lower, "no spot tick") || strings.Contains(lower, "calculation hours")) {
		return true
	}
	return false
}

func canaryClustersWithoutDetailedWarning(clusters []string, detailed map[string]bool) []string {
	if len(clusters) == 0 || len(detailed) == 0 {
		return clusters
	}
	out := []string{}
	for _, cluster := range clusters {
		if detailed[cluster] {
			continue
		}
		out = append(out, cluster)
	}
	return out
}

func canaryRegimeWarningCluster(w rpc.RegimeWarning) string {
	text := strings.ToLower(strings.TrimSpace(w.Scope + " " + w.Code))
	switch {
	case strings.Contains(text, "gamma"):
		return "gamma"
	case strings.Contains(text, "funding"):
		return "funding"
	case strings.Contains(text, "vix") || strings.Contains(text, "vvix") || strings.Contains(text, "vol_of_vol"):
		return "vol"
	case strings.Contains(text, "hyg") || strings.Contains(text, "credit") || strings.Contains(text, "oas"):
		return "credit"
	case strings.Contains(text, "usd") || strings.Contains(text, "jpy") || strings.Contains(text, "fx"):
		return "fx"
	case strings.Contains(text, "breadth"):
		return "breadth"
	default:
		return ""
	}
}

func canarySourceHealth(in CanaryInput, now time.Time, accountFP, positionsFP, regimeFP rpc.Fingerprint, inputHealth string, m CanaryMarketSummary) []rpc.SourceHealth {
	return []rpc.SourceHealth{
		canaryTimedSourceHealth("account", in.Account.AsOf, now, accountFP, canaryAccountSourceStatus(in.Account, now), canaryAccountSourceConfidence(in.Account)),
		canaryTimedSourceHealth("positions", in.Positions.AsOf, now, positionsFP, canaryPositionsSourceStatus(in.Positions, now), canaryPositionsSourceConfidence(in.Positions)),
		canaryRegimeSourceHealth(in.Regime.AsOf, now, regimeFP, canaryInputHealthConfidence(inputHealth), m),
	}
}

func canaryInputHealthConfidence(inputHealth string) string {
	switch inputHealth {
	case canaryInputOK:
		return "high"
	case canaryInputFailed:
		return "low"
	default:
		return "medium-low"
	}
}

func canaryTimedSourceHealth(source string, asOf, now time.Time, fp rpc.Fingerprint, status, confidence string) rpc.SourceHealth {
	maxAge := canarySourceMaxAgeSeconds(now)
	age := canarySourceAgeSeconds(now, asOf)
	if !asOf.IsZero() && age > maxAge && status == rpc.RegimeStatusOK {
		status = rpc.RegimeStatusStale
	}
	if status == rpc.RegimeStatusStale && confidence == "high" {
		confidence = "medium"
	}
	return rpc.SourceHealth{
		Source:               source,
		Status:               status,
		AsOf:                 asOf,
		AgeSeconds:           age,
		MaxAgeSeconds:        maxAge,
		Confidence:           confidence,
		Fingerprint:          &fp,
		FingerprintStability: rpc.FingerprintStabilitySemanticBuckets,
	}
}

func canaryRegimeSourceHealth(asOf, now time.Time, fp rpc.Fingerprint, dataConfidence string, m CanaryMarketSummary) rpc.SourceHealth {
	status := rpc.RegimeStatusOK
	notes := []string{}
	if len(m.StaleClusters) > 0 {
		notes = append(notes, "stale clusters: "+strings.Join(m.StaleClusters, ","))
	}
	if len(m.DegradedClusters) > 0 {
		notes = append(notes, "degraded clusters: "+strings.Join(m.DegradedClusters, ","))
	}
	if len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0 {
		notes = append(notes, canaryAmbiguityEvidence(m))
	}
	switch {
	case len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0:
		status = "partial"
	case len(m.DegradedClusters) > 0:
		status = "degraded"
	case len(m.StaleClusters) > 0:
		status = rpc.RegimeStatusStale
	}
	health := canaryTimedSourceHealth("regime", asOf, now, fp, status, dataConfidence)
	health.Notes = notes
	return health
}

func canaryAccountSourceStatus(acct rpc.AccountResult, now time.Time) string {
	if acct.NetLiquidation <= 0 {
		return "partial"
	}
	if acct.AsOf.IsZero() || acct.DailyPnL == nil {
		return "partial"
	}
	if canarySourceAgeSeconds(now, acct.AsOf) > canarySourceMaxAgeSeconds(now) {
		return rpc.RegimeStatusStale
	}
	return rpc.RegimeStatusOK
}

func canaryAccountSourceConfidence(acct rpc.AccountResult) string {
	if acct.NetLiquidation <= 0 || acct.AsOf.IsZero() || acct.DailyPnL == nil {
		return "medium-low"
	}
	return "high"
}

func canaryPositionsSourceStatus(pos rpc.PositionsResult, now time.Time) string {
	if pos.AsOf.IsZero() {
		return "partial"
	}
	if canarySourceAgeSeconds(now, pos.AsOf) > canarySourceMaxAgeSeconds(now) {
		return rpc.RegimeStatusStale
	}
	return rpc.RegimeStatusOK
}

func canaryPositionsSourceConfidence(pos rpc.PositionsResult) string {
	if pos.AsOf.IsZero() {
		return "medium-low"
	}
	return "high"
}

func canarySourceAgeSeconds(now, asOf time.Time) int64 {
	if now.IsZero() || asOf.IsZero() {
		return 0
	}
	age := now.Sub(asOf)
	if age < 0 {
		return 0
	}
	return int64(age.Seconds())
}

func canarySourceMaxAgeSeconds(now time.Time) int64 {
	switch rpc.ClassifySession(now) {
	case rpc.SessionPre, rpc.SessionRTH:
		return int64((10 * time.Minute).Seconds())
	default:
		return int64((90 * time.Minute).Seconds())
	}
}

func canaryWarningLine(w rpc.RegimeWarning) string {
	scope := strings.TrimSpace(w.Scope)
	if scope == "" {
		scope = strings.TrimSpace(w.Code)
	}
	if scope == "" {
		return ""
	}
	msg := strings.TrimSpace(w.Message)
	if msg == "" {
		msg = strings.TrimSpace(w.Impact)
	}
	noisy := strings.Contains(msg, "http://") || strings.Contains(msg, "https://") ||
		strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "HTTP ")
	if noisy {
		switch {
		case strings.TrimSpace(w.Impact) != "":
			msg = strings.TrimSpace(w.Impact)
		case strings.TrimSpace(w.Action) != "":
			msg = strings.TrimSpace(w.Action)
		case strings.TrimSpace(w.Code) != "":
			msg = strings.TrimSpace(w.Code)
		default:
			msg = "source unavailable"
		}
	}
	if msg == "" {
		return ""
	}
	if !strings.Contains(msg, " row is unranked;") {
		msg = strings.Replace(msg, " is unranked;", " row is unranked;", 1)
	}
	return fmt.Sprintf("%s: %s", scope, msg)
}

func canaryMarketEvidence(m CanaryMarketSummary) string {
	red := strings.Join(m.RedClusterNames, ",")
	yellow := strings.Join(m.YellowClusterNames, ",")
	unconfirmedRed := strings.Join(m.UnconfirmedRedClusterNames, ",")
	if red == "" {
		red = "none"
	}
	if yellow == "" {
		yellow = "none"
	}
	out := fmt.Sprintf("%d red clusters (%s), %d yellow (%s), %d/%d ranked",
		m.RedClusters, red, m.YellowClusters, yellow, m.RankedClusters, m.RankedClusters+m.UnrankedClusters)
	if unconfirmedRed != "" {
		out += "; unconfirmed red " + unconfirmedRed
	}
	return out
}

func canaryTapeEvidence(m CanaryMarketSummary) string {
	parts := []string{}
	if m.SPYChangePct != nil {
		parts = append(parts, fmt.Sprintf("SPY %+.2f%%", *m.SPYChangePct))
	} else {
		parts = append(parts, "SPY change unavailable")
	}
	if m.VIXChangePct != nil {
		parts = append(parts, fmt.Sprintf("VIX %+.2f%%", *m.VIXChangePct))
	} else {
		parts = append(parts, "VIX change unavailable")
	}
	return strings.Join(parts, "; ")
}

func canaryConfirmedTapeStress(m CanaryMarketSummary) bool {
	spyDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct)
	spyHardDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYHardDropPct)
	vixSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct)
	vixHardSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXHardSpikePct)
	return (spyHardDrop && (vixSpike || m.RedClusters >= 1)) ||
		(vixHardSpike && (spyDrop || m.RedClusters >= 1)) ||
		(spyDrop && vixSpike && m.RedClusters >= 1) ||
		canaryFastCarryUnwind(m)
}

func canaryFastCarryUnwind(m CanaryMarketSummary) bool {
	fxRed := slices.Contains(m.RedClusterNames, "fx") || slices.Contains(m.UnconfirmedRedClusterNames, "fx")
	if !fxRed {
		return false
	}
	return pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct) ||
		pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct) ||
		slices.Contains(m.YellowClusterNames, "breadth") ||
		slices.Contains(m.RedClusterNames, "breadth")
}

func pctAtMost(v *float64, threshold float64) bool {
	return v != nil && *v <= threshold
}

func pctAtLeast(v *float64, threshold float64) bool {
	return v != nil && *v >= threshold
}

func canaryAmbiguityEvidence(m CanaryMarketSummary) string {
	parts := []string{canaryMarketEvidence(m)}
	if len(m.StaleClusters) > 0 {
		parts = append(parts, "stale "+canaryClusterList(m.StaleClusters))
	}
	if len(m.AmbiguousClusters) > 0 {
		parts = append(parts, "ambiguous "+canaryClusterList(m.AmbiguousClusters))
	}
	if len(m.PartialClusters) > 0 {
		parts = append(parts, "partial "+canaryClusterList(m.PartialClusters))
	}
	if len(m.DegradedClusters) > 0 {
		parts = append(parts, "degraded "+canaryClusterList(m.DegradedClusters))
	}
	if len(m.ComputingClusters) > 0 {
		parts = append(parts, "computing "+canaryClusterList(m.ComputingClusters))
	}
	return strings.Join(parts, "; ")
}

func canaryPortfolioEvidence(p CanaryPortfolioSummary) string {
	return fmt.Sprintf("%s, gross %.0f%% NLV, net delta %.0f%% NLV, gross delta %.0f%% NLV",
		canaryCushionEvidence(p), derefPct(p.GrossExposurePctNLV), derefPct(p.NetDeltaPctNLV), derefPct(p.GrossDeltaPctNLV))
}

func canaryHasMarketDataIssue(m CanaryMarketSummary) bool {
	return len(m.AmbiguousClusters) > 0 ||
		len(m.PartialClusters) > 0 ||
		len(m.DegradedClusters) > 0 ||
		len(m.ComputingClusters) > 0 ||
		len(m.StaleClusters) > 0
}

func canaryWorstCushionPct(p CanaryPortfolioSummary) *float64 {
	switch {
	case p.CushionPct != nil && p.LookAheadCushionPct != nil:
		v := min(*p.CushionPct, *p.LookAheadCushionPct)
		return &v
	case p.CushionPct != nil:
		return p.CushionPct
	default:
		return p.LookAheadCushionPct
	}
}

func canaryCushionEvidence(p CanaryPortfolioSummary) string {
	parts := []string{}
	if p.CushionPct != nil {
		parts = append(parts, pctEvidence("cushion", *p.CushionPct))
	}
	if p.LookAheadCushionPct != nil {
		parts = append(parts, pctEvidence("look-ahead cushion", *p.LookAheadCushionPct))
	}
	if len(parts) == 0 {
		return "cushion unavailable"
	}
	return strings.Join(parts, "; ")
}

func canaryConcentrationEvidence(p CanaryPortfolioSummary) string {
	parts := []string{}
	if p.LargestExposurePct != nil && p.LargestExposure != "" {
		parts = append(parts, fmt.Sprintf("%s market %.0f%% NLV", p.LargestExposure, math.Abs(*p.LargestExposurePct)))
	}
	if p.LargestDeltaPctNLV != nil && p.LargestDeltaExposure != "" {
		parts = append(parts, fmt.Sprintf("%s delta %.0f%% NLV", p.LargestDeltaExposure, *p.LargestDeltaPctNLV))
	}
	return strings.Join(parts, "; ")
}

func pctEvidence(label string, pct float64) string {
	return fmt.Sprintf("%s %.0f%%", label, pct)
}

func derefPct(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func runCanary(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "canary")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON for scheduling")
	details := fs.Bool("details", false, "show full canary evidence rows")
	view := fs.String("view", rpc.ViewFull, "JSON response view: full | alert")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "canary: takes no positional args (got %v)", fs.Args())
	}
	if *view != rpc.ViewFull && *view != rpc.ViewAlert {
		return fail(env, "canary: --view must be %q or %q (got %q)", rpc.ViewFull, rpc.ViewAlert, *view)
	}
	if *view != rpc.ViewFull && !*jsonOut {
		return fail(env, "canary: --view requires --json")
	}
	if !*jsonOut && isTerminal(env.Stdout) {
		stop := startCanarySpinner(env)
		res, err := FetchCanary(ctx, env.Conn)
		stop()
		if err != nil {
			return fail(env, "canary: %v", err)
		}
		return renderCanaryTextDetails(env, env.Stdout, &res, *details)
	}
	res, positions, err := FetchCanarySnapshot(ctx, env.Conn)
	if err != nil {
		return fail(env, "canary: %v", err)
	}
	if *jsonOut {
		if *view == rpc.ViewAlert {
			return printJSON(env, rpc.CompactCanaryAlert(&res, &positions))
		}
		return printJSON(env, res)
	}
	return renderCanaryTextDetails(env, env.Stdout, &res, *details)
}

func startCanarySpinner(env *Env) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	go func() {
		defer close(done)
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-stop:
				fmt.Fprint(env.Stdout, "\r\x1b[K")
				return
			case <-ticker.C:
				fmt.Fprintf(env.Stdout, "\r\x1b[K%s %s",
					env.dim("Populating canary: account, positions, regime, gamma, breadth"), frames[i])
				i = (i + 1) % len(frames)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// FetchCanary reads the three existing snapshots needed by ComputeCanary.
// dial.Conn serializes calls internally, so this stays sequential and avoids
// hidden socket contention in scheduled MCP runs.
func FetchCanary(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (CanaryResult, error) {
	res, _, err := FetchCanarySnapshot(ctx, conn)
	return res, err
}

func FetchCanarySnapshot(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (CanaryResult, rpc.PositionsResult, error) {
	var acct rpc.AccountResult
	if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
		return CanaryResult{}, rpc.PositionsResult{}, fmt.Errorf("account: %w", err)
	}
	var pos rpc.PositionsResult
	if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &pos); err != nil {
		return CanaryResult{}, rpc.PositionsResult{}, fmt.Errorf("positions: %w", err)
	}
	var regime rpc.RegimeSnapshotResult
	if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &regime); err != nil {
		return CanaryResult{}, rpc.PositionsResult{}, fmt.Errorf("regime: %w", err)
	}
	if acct.DailyPnL == nil {
		var refreshed rpc.AccountResult
		if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &refreshed); err == nil && refreshed.DailyPnL != nil {
			acct = refreshed
		}
	}
	rpc.CompactRegimeSnapshot(&regime)
	return ComputeCanary(CanaryInput{Account: acct, Positions: pos, Regime: regime}), pos, nil
}

func renderCanaryText(env *Env, out io.Writer, r *CanaryResult) int {
	return renderCanaryTextDetails(env, out, r, false)
}

func renderCanaryTextDetails(env *Env, out io.Writer, r *CanaryResult, details bool) int {
	width := outputColumns(out)
	if width == 0 {
		width = 120
	}
	return renderCanaryTextWidthDetails(env, out, r, width, details)
}

func renderCanaryTextWidth(env *Env, out io.Writer, r *CanaryResult, width int) int {
	return renderCanaryTextWidthDetails(env, out, r, width, false)
}

func renderCanaryTextWidthDetails(env *Env, out io.Writer, r *CanaryResult, width int, details bool) int {
	if width < 40 {
		width = 80
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Portfolio Canary  ·  %s\n", r.AsOf.Format("2006-01-02 15:04 MST"))
	fmt.Fprintln(out)
	renderCanaryKV(out, "Action", canaryHeadlineText(r), width, func(string) string {
		return canaryHeadlineLabel(env, r)
	})
	renderCanaryKV(out, "Guidance", r.Summary, width, env.bold)
	if len(r.PrimaryDrivers) > 0 {
		renderCanaryKV(out, "Drivers", strings.Join(signalDisplayStrings(r.PrimaryDrivers), ", "), width, nil)
	}
	renderCanaryKV(out, "Next step", canaryPlannerStepText(r.PlannerModeHint, r.PlannerReadiness), width, func(s string) string {
		return canaryPlannerStepLabel(env, r.PlannerModeHint, r.PlannerReadiness)
	})
	fmt.Fprintln(out)

	fmt.Fprintln(out, "  Why this fired")
	renderCanarySectionRow(out, "Market weather", canaryMarketReadText(r), width, nil)
	renderCanarySectionRow(out, "Portfolio shape", canaryPortfolioFitText(r), width, nil)
	renderCanarySectionRow(out, "Combined read", canaryCombinedReadText(r), width, nil)
	renderCanaryMarketIndicators(env, out, r.MarketIndicators, width)

	renderCanaryWarnings(env, out, r.Warnings, width)

	if details {
		fmt.Fprintln(out)
		renderCanaryRowsStacked(env, out, r.Rows, width)
	}
	if r.Fingerprint.Key != "" {
		fmt.Fprintln(out)
		renderCanaryKV(out, "Alert ID", r.Fingerprint.Version+" "+r.Fingerprint.Key, width, env.dim)
	}
	return 0
}

func canaryHeadlineText(r *CanaryResult) string {
	return strings.ToUpper(canaryActionDisplay(r.Action)) + " · " + canaryHeadlineReason(r)
}

func canaryHeadlineLabel(env *Env, r *CanaryResult) string {
	text := canaryHeadlineText(r)
	switch r.Action {
	case canaryActionDefend:
		return env.bold(env.red(text))
	case canaryActionWatch, canaryActionRebalance, canaryActionConfirmInputs:
		return env.bold(env.yellow(text))
	case canaryActionDeploy:
		return env.bold(env.green(text))
	default:
		return env.bold(text)
	}
}

func canaryActionDisplay(action string) string {
	switch action {
	case canaryActionDefend:
		return "defend"
	case canaryActionWatch:
		return "watch"
	case canaryActionRebalance:
		return "rebalance"
	case canaryActionDeploy:
		return "deploy"
	case canaryActionConfirmInputs:
		return "confirm inputs"
	default:
		return "stand down"
	}
}

func canaryHeadlineReason(r *CanaryResult) string {
	switch r.Action {
	case canaryActionDefend:
		return "market stress confirmed against vulnerable portfolio"
	case canaryActionWatch:
		if r.PortfolioFit == canaryPortfolioFitLow || r.PortfolioFit == canaryPortfolioFitUnknown {
			return "market pressure; portfolio fit is not a defense trigger"
		}
		return "market pressure with portfolio exposure"
	case canaryActionRebalance:
		return "portfolio shape outside limits; market stress unconfirmed"
	case canaryActionDeploy:
		return "constructive tape with clean inputs"
	case canaryActionConfirmInputs:
		return "input health blocks the canary"
	default:
		return "no market-context action"
	}
}

func canaryMarketReadText(r *CanaryResult) string {
	return fmt.Sprintf("%s — %s", r.MarketConfirmation, canaryMarketEvidence(r.Market))
}

func canaryPortfolioFitText(r *CanaryResult) string {
	return fmt.Sprintf("%s — %s", r.PortfolioFit, canaryPortfolioEvidence(r.Portfolio))
}

func canaryCombinedReadText(r *CanaryResult) string {
	switch r.Action {
	case canaryActionDefend:
		return "market confirmation and portfolio fit agree; defensive action is justified by this canary."
	case canaryActionWatch:
		return "market pressure is not strong enough for automatic defense; stage a plan and wait for confirmation."
	case canaryActionRebalance:
		return "portfolio shape is high risk, but market weather is not the trigger; use portfolio-risk workflow."
	case canaryActionDeploy:
		return "constructive tape is visible; size only inside the existing risk budget."
	case canaryActionConfirmInputs:
		return "the monitor cannot separate signal from input failure yet."
	default:
		return "market weather and portfolio shape do not call for a canary action."
	}
}

type canaryInputHealthRow struct {
	label string
	text  string
}

func canaryInputHealthRows(r *CanaryResult) []canaryInputHealthRow {
	rows := []canaryInputHealthRow{{
		label: "Overall",
		text:  r.InputHealth,
	}}
	if r.Portfolio.DailyPnLPct == nil {
		rows = append(rows, canaryInputHealthRow{label: "Warming input", text: "account daily P&L has not produced a usable frame yet"})
	}
	if len(r.Market.DegradedClusters) > 0 {
		rows = append(rows, canaryInputHealthRow{label: "Degraded input", text: canaryClusterList(r.Market.DegradedClusters)})
	}
	if len(r.Market.StaleClusters) > 0 {
		rows = append(rows, canaryInputHealthRow{label: "Stale input", text: canaryClusterList(r.Market.StaleClusters)})
	}
	if len(r.Market.PartialClusters) > 0 || len(r.Market.AmbiguousClusters) > 0 || len(r.Market.ComputingClusters) > 0 {
		rows = append(rows, canaryInputHealthRow{label: "Incomplete input", text: canaryAmbiguityEvidence(r.Market)})
	}
	for _, h := range r.SourceHealth {
		if h.Status == "" || h.Status == rpc.RegimeStatusOK {
			continue
		}
		rows = append(rows, canaryInputHealthRow{label: "Source status", text: h.Source + " " + h.Status})
	}
	if len(rows) == 1 && r.InputHealth == canaryInputOK {
		rows[0].text = "ok — account, positions, and regime inputs are usable"
	}
	return rows
}

func renderCanarySectionRow(out io.Writer, label, value string, width int, color func(string) string) {
	const labelW = 16
	available := max(width-4-labelW-1, 24)
	lines := wrapVisibleText(value, available)
	for i, line := range lines {
		if color != nil {
			line = color(line)
		}
		if i == 0 {
			fmt.Fprintf(out, "    %-*s %s\n", labelW, label, line)
		} else {
			fmt.Fprintf(out, "    %-*s %s\n", labelW, "", line)
		}
	}
}

func humanList(value string) string {
	clean := []string{}
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		clean = append(clean, canaryClusterDisplayName(part))
	}
	if len(clean) == 0 {
		return strings.TrimSpace(value)
	}
	if len(clean) == 1 {
		return clean[0]
	}
	if len(clean) == 2 {
		return clean[0] + " and " + clean[1]
	}
	return strings.Join(clean[:len(clean)-1], ", ") + ", and " + clean[len(clean)-1]
}

func canaryClusterList(clusters []string) string {
	return humanList(strings.Join(clusters, ","))
}

func canaryClusterDisplayName(cluster string) string {
	switch strings.ToLower(strings.TrimSpace(cluster)) {
	case "fx":
		return "FX"
	default:
		return strings.TrimSpace(cluster)
	}
}

func renderCanaryKV(out io.Writer, label, value string, width int, color func(string) string) {
	const labelW = 10
	available := max(width-2-labelW-1, 24)
	lines := wrapVisibleText(value, available)
	for i, line := range lines {
		if color != nil {
			line = color(line)
		}
		if i == 0 {
			fmt.Fprintf(out, "  %-*s %s\n", labelW, label, line)
		} else {
			fmt.Fprintf(out, "  %-*s %s\n", labelW, "", line)
		}
	}
}

func renderCanaryRowsStacked(env *Env, out io.Writer, rows []CanaryRow, width int) {
	fmt.Fprintln(out, "  Details")
	for _, row := range rows {
		if row.Title == "Portfolio canary" {
			continue
		}
		state := canaryRiskStateLabel(env, row.Direction, row.Severity, false)
		fmt.Fprintf(out, "  %s · %s\n", row.Title, state)
		renderCanaryIndented(out, "guidance", row.Guidance, width, nil)
		if row.Evidence != "" {
			renderCanaryIndented(out, "evidence", row.Evidence, width, env.dim)
		}
	}
}

func renderCanaryMarketIndicators(env *Env, out io.Writer, indicators []CanaryMarketIndicator, width int) {
	if len(indicators) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Market indicators")
	nameW := 17
	statusW := 6
	asOfW := 10
	for _, row := range indicators {
		nameW = max(nameW, min(visibleLen(row.Name), 22))
		statusW = max(statusW, visibleLen(row.Status))
		asOfW = max(asOfW, min(visibleLen(ifNonEmpty(row.AsOf, "—")), 12))
	}
	detailW := max(width-4-nameW-2-statusW-2-asOfW-2, 24)
	header := fmt.Sprintf("    %s  %s  %s  %s",
		padRightVisible("INDICATOR", nameW),
		padRightVisible("STATE", statusW),
		padRightVisible("AS OF", asOfW),
		"READING / COMMENT")
	fmt.Fprintln(out, env.dim(header))
	for _, row := range indicators {
		detail := strings.TrimSpace(row.Reading)
		if row.Comment != "" {
			if detail != "" {
				detail += " — "
			}
			detail += row.Comment
		}
		if detail == "" {
			detail = "—"
		}
		lines := wrapVisibleText(detail, detailW)
		status := padRightVisible(canaryIndicatorStatusLabel(env, row.Status), statusW)
		for i, line := range lines {
			if i == 0 {
				fmt.Fprintf(out, "    %s  %s  %s  %s\n",
					padRightVisible(row.Name, nameW),
					status,
					padRightVisible(ifNonEmpty(row.AsOf, "—"), asOfW),
					line)
				continue
			}
			fmt.Fprintf(out, "    %s  %s  %s  %s\n",
				strings.Repeat(" ", nameW),
				strings.Repeat(" ", statusW),
				strings.Repeat(" ", asOfW),
				line)
		}
	}
}

func canaryIndicatorStatusLabel(env *Env, status string) string {
	switch status {
	case "green":
		return env.green(status)
	case "amber":
		return env.yellow(status)
	case "red":
		return env.red(status)
	case "context":
		return env.dim(status)
	default:
		return env.dim(status)
	}
}

func renderCanaryIndented(out io.Writer, label, value string, width int, color func(string) string) {
	const labelW = 9
	available := max(width-4-labelW-1, 24)
	for i, line := range wrapVisibleText(value, available) {
		if color != nil {
			line = color(line)
		}
		if i == 0 {
			fmt.Fprintf(out, "    %-*s %s\n", labelW, label, line)
		} else {
			fmt.Fprintf(out, "    %-*s %s\n", labelW, "", line)
		}
	}
}

func renderCanaryWarnings(env *Env, out io.Writer, warnings []string, width int) {
	if len(warnings) == 0 {
		return
	}
	type inputCheck struct {
		label string
		text  string
	}
	checks := []inputCheck{}
	for _, warning := range warnings {
		label, text := canaryWarningLabel(warning)
		text = canaryInputCheckText(label, text)
		if !canaryInputCheckRenderable(label, text) {
			continue
		}
		checks = append(checks, inputCheck{label: label, text: text})
	}
	if len(checks) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Input checks")
	for _, check := range checks {
		label, text := check.label, check.text
		labelText := label + ":"
		available := max(width-4-visibleLen(labelText)-1, 24)
		lines := wrapVisibleText(text, available)
		labelText = canaryWarningLabelColor(env, label, labelText)
		for i, line := range lines {
			if i == 0 {
				fmt.Fprintf(out, "    %s %s\n", labelText, line)
			} else {
				fmt.Fprintf(out, "    %s %s\n", strings.Repeat(" ", visibleLen(label)+1), line)
			}
		}
	}
}

func canaryInputCheckRenderable(label, text string) bool {
	if label != "context" {
		return true
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, "action:") &&
		!strings.Contains(lower, "no immediate fix") &&
		!strings.Contains(lower, "closed-session") &&
		!strings.Contains(lower, "context-only")
}

func signalDisplayStrings(ids []risk.SignalID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, signalDisplayString(id))
	}
	return out
}

func signalDisplayString(id risk.SignalID) string {
	switch id {
	case risk.SignalMarginCushionLow:
		return "margin cushion"
	case risk.SignalLookAheadCushionLow:
		return "look-ahead cushion"
	case risk.SignalPortfolioPnLShock:
		return "P&L shock"
	case risk.SignalFXCarryUnwind:
		return "FX carry unwind"
	case risk.SignalGammaRed:
		return "gamma red"
	case risk.SignalSingleNameExposureHigh:
		return "title exposure"
	case risk.SignalSingleNameDeltaHigh:
		return "title delta"
	case risk.SignalGrossExposureHigh:
		return "gross exposure"
	case risk.SignalNetDeltaHigh:
		return "net delta"
	case risk.SignalGrossDeltaHigh:
		return "gross delta"
	case risk.SignalRiskDataDegraded:
		return "data degraded"
	case risk.SignalMarketDataStale:
		return "market data stale"
	case risk.SignalOptionGreeksDegraded:
		return "option greeks"
	default:
		return string(id)
	}
}

func wrapVisibleText(text string, width int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{""}
	}
	if width <= 0 {
		return []string{text}
	}
	words := strings.Fields(text)
	lines := []string{}
	line := ""
	for _, word := range words {
		for visibleLen(word) > width {
			head, tail := splitVisibleWord(word, width)
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			lines = append(lines, head)
			word = tail
		}
		if line == "" {
			line = word
			continue
		}
		if visibleLen(line)+1+visibleLen(word) <= width {
			line += " " + word
			continue
		}
		lines = append(lines, line)
		line = word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func splitVisibleWord(word string, width int) (string, string) {
	if width <= 0 {
		return word, ""
	}
	used := 0
	for i := range word {
		if used == width {
			return word[:i], word[i:]
		}
		used++
	}
	return word, ""
}

func canaryWarningLabel(warning string) (string, string) {
	lower := strings.ToLower(warning)
	switch {
	case strings.Contains(lower, "error"):
		return "error", warning
	case strings.Contains(lower, "context_only") ||
		strings.Contains(lower, "context only") ||
		strings.Contains(lower, "displayed as context") ||
		strings.Contains(lower, "frozen") ||
		strings.Contains(lower, "closed"):
		return "context", warning
	case strings.Contains(lower, "stale"):
		return "refresh", warning
	case strings.Contains(lower, "computing") || strings.Contains(lower, "ambiguous") || strings.Contains(lower, "unranked"):
		return "verify", warning
	default:
		return "warning", warning
	}
}

func canaryInputCheckText(label, warning string) string {
	lower := strings.ToLower(warning)
	switch {
	case strings.Contains(lower, "gamma_zero") && (strings.Contains(lower, "context_only") || strings.Contains(lower, "context only")):
		return "Gamma is after-hours/context-only. Action: no immediate fix; refresh during active option hours before using gamma as confirmation."
	case strings.Contains(lower, "funding") && strings.Contains(lower, "unranked"):
		return "Funding is not usable confirmation yet. Action: ignore it for escalation and rerun `ibkr regime` after the source updates."
	case strings.Contains(lower, "ambiguous clusters:"):
		clusters := strings.TrimSpace(warning[strings.LastIndex(warning, ":")+1:])
		return fmt.Sprintf("%s cannot confirm the canary yet. Action: check the n/a row in Market indicators and verify with `ibkr regime` before escalating.", humanList(clusters))
	case strings.Contains(lower, "partial clusters:"):
		clusters := strings.TrimSpace(warning[strings.LastIndex(warning, ":")+1:])
		return fmt.Sprintf("%s is partially usable. Action: inspect the affected Market indicators row; rely only on fresh independent clusters for confirmation.", humanList(clusters))
	case strings.Contains(lower, "degraded clusters:"):
		clusters := strings.TrimSpace(warning[strings.LastIndex(warning, ":")+1:])
		return fmt.Sprintf("%s has degraded input quality. Action: inspect the source command before using it as confirmation.", humanList(clusters))
	case strings.Contains(lower, "regime cluster") && strings.Contains(lower, "unranked"):
		return "One or more regime clusters are unranked. Action: treat the canary as context until the n/a Market indicators rows rank."
	case strings.Contains(lower, "stale clusters:"):
		clusters := strings.TrimSpace(warning[strings.LastIndex(warning, ":")+1:])
		return fmt.Sprintf("Refresh %s before escalation. Action: rerun `ibkr canary`; stale rows remain context, not fresh confirmation.", humanList(clusters))
	case strings.Contains(lower, "vix_term_structure") && strings.Contains(lower, "stale"):
		return "Volatility term structure is stale. Action: rerun `ibkr regime` or `ibkr canary` before treating vol as fresh confirmation."
	case strings.Contains(lower, "computing"):
		return warning + ". Action: wait for the daemon compute to finish, then rerun `ibkr canary`."
	case label == "error":
		return warning + ". Action: fix the source or daemon issue before relying on this canary."
	case label == "warning":
		return warning + ". Action: inspect the affected row before escalating."
	default:
		return warning
	}
}

func canaryWarningLabelColor(env *Env, label, text string) string {
	switch label {
	case "context":
		return env.dim(text)
	case "verify", "refresh":
		return env.yellow(text)
	case "error":
		return env.red(text)
	default:
		return env.yellow(text)
	}
}

func canaryRiskStateLabel(env *Env, direction risk.SignalDirection, severity risk.SignalSeverity, current bool) string {
	label := canaryRiskStateText(direction, severity)
	if current {
		label = "[" + label + "]"
	}
	switch severity {
	case risk.SeverityObserve:
		label = env.green(label)
	case risk.SeverityWatch:
		label = env.yellow(label)
	case risk.SeverityAct, risk.SeverityUrgent:
		label = env.red(label)
	}
	if current {
		label = env.bold(label)
	}
	return label
}

func canaryRiskStateText(direction risk.SignalDirection, severity risk.SignalSeverity) string {
	directionLabel := canaryDirectionLabel(direction)
	severityLabel := canarySeverityLabel(severity)
	if directionLabel == "" {
		return severityLabel
	}
	return directionLabel + " / " + severityLabel
}

func canaryDirectionLabel(direction risk.SignalDirection) string {
	switch direction {
	case risk.DirectionDefensive:
		return "Defensive"
	case risk.DirectionConstructive:
		return "Constructive"
	case risk.DirectionRebalance:
		return "Rebalance"
	case risk.DirectionMixed:
		return "Mixed"
	case risk.DirectionDataQuality:
		return "Data quality"
	default:
		return ""
	}
}

func canarySeverityLabel(severity risk.SignalSeverity) string {
	switch severity {
	case risk.SeverityUrgent:
		return "Urgent"
	case risk.SeverityAct:
		return "Act"
	case risk.SeverityWatch:
		return "Watch"
	default:
		return "Observe"
	}
}

func canaryPlannerStepLabel(env *Env, mode risk.PlannerMode, readiness risk.PlannerReadiness) string {
	label := canaryPlannerStepText(mode, readiness)
	if readiness == risk.PlannerReadinessBlocked {
		return env.yellow(label)
	}
	if readiness == risk.PlannerReadinessReady {
		return env.red(label)
	}
	return label
}

func canaryPlannerStepText(mode risk.PlannerMode, readiness risk.PlannerReadiness) string {
	switch mode {
	case risk.PlannerModeDefend:
		if readiness == risk.PlannerReadinessBlocked {
			return "Confirm data before defend"
		}
		return "Run risk-plan defend"
	case risk.PlannerModeStage:
		return "Stage risk-plan"
	case risk.PlannerModeDeploy:
		if readiness == risk.PlannerReadinessBlocked {
			return "Confirm data before deploy"
		}
		return "Run risk-plan deploy"
	case risk.PlannerModeRebalance:
		if readiness == risk.PlannerReadinessBlocked {
			return "Confirm data before rebalance"
		}
		return "Run risk-plan rebalance"
	case risk.PlannerModeConfirmData:
		return "Confirm data"
	default:
		if readiness == risk.PlannerReadinessWatch {
			return "Watch"
		}
		return "No risk-plan"
	}
}
