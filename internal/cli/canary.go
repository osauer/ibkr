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

// CanaryInput is the pure state input shared by the CLI and MCP tool.
// It deliberately consumes existing daemon snapshots instead of adding a
// second risk-data path: account margin, portfolio exposure, and market
// regime stay single-source-of-truth.
type CanaryInput struct {
	Account   rpc.AccountResult
	Positions rpc.PositionsResult
	Regime    rpc.RegimeSnapshotResult
	Now       time.Time
}

// CanaryResult is the compact scheduled-monitor payload. Rows are ordered
// from portfolio-level state to supporting gates so an agent can print the
// table directly without composing prose.
type CanaryResult struct {
	AsOf              time.Time              `json:"as_of"`
	SourceAsOf        CanarySourceAsOf       `json:"source_as_of,omitzero"`
	Policy            string                 `json:"policy,omitempty"`
	Direction         risk.SignalDirection   `json:"direction,omitempty"`
	Severity          risk.SignalSeverity    `json:"severity"`
	PlannerModeHint   risk.PlannerMode       `json:"planner_mode_hint,omitempty"`
	PlannerReadiness  risk.PlannerReadiness  `json:"planner_readiness,omitempty"`
	Summary           string                 `json:"summary"`
	Confidence        string                 `json:"confidence,omitempty"`
	DataConfidence    string                 `json:"data_confidence,omitempty"`
	SignalConfidence  string                 `json:"signal_confidence,omitempty"`
	ConfidenceReasons []string               `json:"confidence_reasons,omitempty"`
	PrimaryDrivers    []risk.SignalID        `json:"primary_drivers,omitempty"`
	Signals           []risk.Signal          `json:"signals,omitempty"`
	Rows              []CanaryRow            `json:"rows"`
	Portfolio         CanaryPortfolioSummary `json:"portfolio"`
	Market            CanaryMarketSummary    `json:"market"`
	Warnings          []string               `json:"warnings,omitempty"`
	NotExecution      string                 `json:"not_execution"`
}

type CanarySourceAsOf struct {
	Account   time.Time `json:"account,omitzero"`
	Positions time.Time `json:"positions,omitzero"`
	Regime    time.Time `json:"regime,omitzero"`
}

type CanaryRow struct {
	Title     string               `json:"title"`
	Direction risk.SignalDirection `json:"direction,omitempty"`
	Severity  risk.SignalSeverity  `json:"severity"`
	Guidance  string               `json:"guidance"`
	Evidence  string               `json:"evidence,omitempty"`
}

type CanaryPortfolioSummary struct {
	BaseCurrency         string   `json:"base_currency,omitempty"`
	NetLiquidation       float64  `json:"net_liquidation,omitempty"`
	CushionPct           *float64 `json:"cushion_pct,omitempty"`
	LookAheadCushionPct  *float64 `json:"look_ahead_cushion_pct,omitempty"`
	GrossExposurePctNLV  *float64 `json:"gross_exposure_pct_nlv,omitempty"`
	NetDeltaPctNLV       *float64 `json:"net_delta_pct_nlv,omitempty"`
	GrossDeltaPctNLV     *float64 `json:"gross_delta_pct_nlv,omitempty"`
	LargestExposure      string   `json:"largest_exposure,omitempty"`
	LargestExposurePct   *float64 `json:"largest_exposure_pct_nlv,omitempty"`
	LargestDeltaExposure string   `json:"largest_delta_exposure,omitempty"`
	LargestDeltaPctNLV   *float64 `json:"largest_delta_pct_nlv,omitempty"`
	DailyPnLPct          *float64 `json:"daily_pnl_pct,omitempty"`
	OptionGreeks         string   `json:"option_greeks,omitempty"`
}

type CanaryMarketSummary struct {
	RegimeVerdict      string   `json:"regime_verdict,omitempty"`
	RedClusters        int      `json:"red_clusters"`
	YellowClusters     int      `json:"yellow_clusters"`
	RankedClusters     int      `json:"ranked_clusters"`
	UnrankedClusters   int      `json:"unranked_clusters"`
	RedClusterNames    []string `json:"red_cluster_names,omitempty"`
	YellowClusterNames []string `json:"yellow_cluster_names,omitempty"`
	AmbiguousClusters  []string `json:"ambiguous_clusters,omitempty"`
	PartialClusters    []string `json:"partial_clusters,omitempty"`
	ComputingClusters  []string `json:"computing_clusters,omitempty"`
	DegradedClusters   []string `json:"degraded_clusters,omitempty"`
	StaleClusters      []string `json:"stale_clusters,omitempty"`
	SPYChangePct       *float64 `json:"spy_change_pct,omitempty"`
	VIXChangePct       *float64 `json:"vix_change_pct,omitempty"`
}

func ComputeCanary(in CanaryInput) CanaryResult {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	res := CanaryResult{
		AsOf:         now,
		SourceAsOf:   CanarySourceAsOf{Account: in.Account.AsOf, Positions: in.Positions.AsOf, Regime: in.Regime.AsOf},
		Policy:       canaryPolicy.Name,
		Portfolio:    summarizeCanaryPortfolio(in.Account, in.Positions),
		Market:       summarizeCanaryMarket(in.Regime),
		NotExecution: "Read-only recommendation; no orders are placed by ibkr.",
	}

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
	res.Direction = canaryOverallDirection(rows, res.Signals)
	res.Severity = canaryOverallSeverity(rows, res.Signals, res.Market, res.Portfolio)
	res.PrimaryDrivers = canaryPrimaryDrivers(res.Signals)
	res.DataConfidence, res.SignalConfidence, res.ConfidenceReasons = canaryConfidenceProfile(res.Severity, res.Portfolio, res.Market, res.Signals)
	res.PlannerModeHint = canaryPlannerMode(res.Direction, res.Severity, res.DataConfidence, res.Portfolio, res.Market, res.Signals)
	res.PlannerReadiness = canaryPlannerReadiness(res.PlannerModeHint, res.Severity, res.DataConfidence, res.Portfolio)
	res.Confidence = canaryConfidence(res.DataConfidence, res.SignalConfidence)
	res.Summary = canaryOverallSummary(res.Direction, res.Severity, res.PlannerModeHint, res.PlannerReadiness)
	overall := canaryOverallRow(res.Direction, res.Severity, res.Summary, res.Market, res.Portfolio)
	res.Rows = append([]CanaryRow{overall}, rows...)
	res.Warnings = canaryWarnings(res.Market, in.Regime)
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

func summarizeCanaryMarket(r rpc.RegimeSnapshotResult) CanaryMarketSummary {
	out := CanaryMarketSummary{
		RegimeVerdict:    r.Composite.Verdict,
		RedClusters:      r.Composite.ClusterRedCount,
		YellowClusters:   r.Composite.ClusterYellowCount,
		RankedClusters:   r.Composite.ClusterRankedCount,
		UnrankedClusters: r.Composite.ClusterUnrankedCount,
		SPYChangePct:     r.HYGSPYDivergence.SPYChangePct,
		VIXChangePct:     r.VIXTermStructure.VIXChangePct,
	}
	clusters := map[string][]string{
		"vol":     {r.VIXTermStructure.Band, r.VolOfVol.Band},
		"credit":  {r.HYGSPYDivergence.Band, r.CreditSpreads.Band},
		"funding": {r.FundingStress.Band},
		"fx":      {r.USDJPY.Band},
		"gamma":   {r.GammaZero.Band},
		"breadth": {r.Breadth.Band},
	}
	statuses := map[string][]string{
		"vol":     {r.VIXTermStructure.Status, r.VolOfVol.Status},
		"credit":  {r.HYGSPYDivergence.Status, r.CreditSpreads.Status},
		"funding": {r.FundingStress.Status},
		"fx":      {r.USDJPY.Status},
		"gamma":   {r.GammaZero.Status},
		"breadth": {r.Breadth.Status},
	}
	for name, bands := range clusters {
		clusterBand := strongestBand(bands)
		switch clusterBand {
		case "red":
			out.RedClusterNames = append(out.RedClusterNames, name)
		case "yellow":
			out.YellowClusterNames = append(out.YellowClusterNames, name)
		}
		status := weakestStatus(statuses[name])
		if status == rpc.RegimeStatusComputing {
			out.ComputingClusters = append(out.ComputingClusters, name)
		}
		if status == rpc.RegimeStatusStale {
			out.StaleClusters = append(out.StaleClusters, name)
		}
		if clusterBand == "" {
			out.AmbiguousClusters = append(out.AmbiguousClusters, name)
		} else if status == rpc.RegimeStatusError || status == rpc.RegimeStatusUnavailable || status == rpc.RegimeStatusComputing {
			out.PartialClusters = append(out.PartialClusters, name)
		}
	}
	if canaryGammaDegraded(r.GammaZero) {
		out.DegradedClusters = append(out.DegradedClusters, "gamma")
	}
	slices.Sort(out.RedClusterNames)
	slices.Sort(out.YellowClusterNames)
	slices.Sort(out.AmbiguousClusters)
	slices.Sort(out.PartialClusters)
	slices.Sort(out.ComputingClusters)
	slices.Sort(out.DegradedClusters)
	slices.Sort(out.StaleClusters)
	if out.RedClusters == 0 && len(out.RedClusterNames) > 0 {
		out.RedClusters = len(out.RedClusterNames)
	}
	if out.YellowClusters == 0 && len(out.YellowClusterNames) > 0 {
		out.YellowClusters = len(out.YellowClusterNames)
	}
	return out
}

func canaryGammaDegraded(g rpc.RegimeGammaZero) bool {
	if g.Envelope.Result == nil || g.Envelope.Result.Summary == nil {
		return false
	}
	return strings.EqualFold(g.Envelope.Result.Summary.Confidence, "degraded")
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
		return canaryRow("Portfolio P&L shock", "", risk.SeverityObserve, "No daily P&L shock signal.", "daily P&L unavailable")
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
	confirmed := (spyDrop && vixSpike) || m.RedClusters >= 1 || canaryMarginPressure(p)
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
		return canaryRow("US equity/options exposure", risk.DirectionDefensive, risk.SeverityWatch, "Exposure is high; pre-stage reductions but wait for confirmed market stress.", evidence)
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
		return canaryRow("Largest concentration", risk.DirectionDefensive, risk.SeverityWatch, "Pre-stage a trim for this concentration if a second stress cluster confirms.", evidence)
	}
	return canaryRow("Largest concentration", "", risk.SeverityObserve, "No concentration trim required by the canary.", evidence)
}

func canaryOptionsRow(p CanaryPortfolioSummary, pos rpc.PositionsResult, m CanaryMarketSummary) CanaryRow {
	if pos.Portfolio == nil || pos.Portfolio.GreeksTotal == 0 {
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
		return canaryRow("Ambiguity filter", risk.DirectionDataQuality, risk.SeverityWatch, "Treat this as unresolved or degraded stress: verify weak clusters, but do not suppress confirmed independent red signals.", canaryAmbiguityEvidence(m))
	}
	if canaryHasMarketDataIssue(m) {
		return canaryRow("Ambiguity filter", risk.DirectionDataQuality, risk.SeverityWatch, "Market inputs are incomplete or stale; keep the canary on watch until coverage and freshness recover.", canaryAmbiguityEvidence(m))
	}
	if m.RankedClusters < 4 {
		return canaryRow("Data quality gate", risk.DirectionDataQuality, risk.SeverityWatch, "Do not take urgent action on market data alone; wait for at least four ranked regime clusters.", canaryMarketEvidence(m))
	}
	if r.GammaZero.Status == rpc.RegimeStatusComputing || r.Breadth.Status == rpc.RegimeStatusComputing {
		return canaryRow("Data quality gate", risk.DirectionDataQuality, risk.SeverityWatch, "Do not escalate on gamma/breadth until the daemon finishes the cached compute.", canaryMarketEvidence(m))
	}
	return canaryRow("Data quality gate", "", risk.SeverityObserve, "Market data coverage is sufficient for the canary policy.", canaryMarketEvidence(m))
}

func canaryOverallRow(direction risk.SignalDirection, severity risk.SignalSeverity, summary string, m CanaryMarketSummary, p CanaryPortfolioSummary) CanaryRow {
	return canaryRow("Portfolio canary", direction, severity, summary, fmt.Sprintf("%s; %s", canaryMarketEvidence(m), canaryPortfolioEvidence(p)))
}

func canaryOverallSeverity(rows []CanaryRow, signals []risk.Signal, m CanaryMarketSummary, p CanaryPortfolioSummary) risk.SignalSeverity {
	severity := risk.SeverityObserve
	for _, row := range rows {
		if signalSeverityRank(row.Severity) > signalSeverityRank(severity) {
			severity = row.Severity
		}
	}
	for _, s := range signals {
		if signalSeverityRank(s.Severity) > signalSeverityRank(severity) {
			severity = s.Severity
		}
	}
	if severity == risk.SeverityUrgent && !canaryImmediateDanger(p) && m.RedClusters < 2 {
		severity = risk.SeverityAct
	}
	if severity == risk.SeverityUrgent && len(m.AmbiguousClusters) > 2 && !canaryImmediateDanger(p) {
		severity = risk.SeverityAct
	}
	if severity == risk.SeverityAct && m.RedClusters == 0 && !canaryMarginPressure(p) && !canaryConfirmedTapeStress(m) {
		severity = risk.SeverityWatch
	}
	return severity
}

func canaryImmediateDanger(p CanaryPortfolioSummary) bool {
	cushion := canaryWorstCushionPct(p)
	return cushion != nil && *cushion < canaryPolicy.MarginUrgentPct
}

func canaryMarginPressure(p CanaryPortfolioSummary) bool {
	cushion := canaryWorstCushionPct(p)
	return cushion != nil && *cushion < canaryPolicy.MarginActPct
}

func canaryOverallSummary(direction risk.SignalDirection, severity risk.SignalSeverity, mode risk.PlannerMode, readiness risk.PlannerReadiness) string {
	if readiness == risk.PlannerReadinessBlocked {
		return "Refresh or confirm degraded inputs before planning major portfolio changes."
	}
	if direction == risk.DirectionDataQuality {
		return "Confirm data quality before acting on the canary state."
	}
	switch mode {
	case risk.PlannerModeDefend:
		if severity == risk.SeverityUrgent {
			return "Run a defensive risk plan now; prioritize liquidity, margin, and fragile exposure."
		}
		return "Run a defensive risk plan; reduce the largest confirmed risk first."
	case risk.PlannerModeStage:
		return "Freeze new risk and stage a risk plan; wait for confirmation before major action."
	case risk.PlannerModeDeploy:
		return "Constructive pressure is present; deploy only if risk budget and data quality are clean."
	case risk.PlannerModeRebalance:
		return "Rebalance toward accepted risk limits using the shared policy."
	case risk.PlannerModeConfirmData:
		return "Refresh or confirm data before planning portfolio action."
	default:
		return "Hold current risk posture; no canary-triggered risk plan."
	}
}

func canaryConfidence(dataConfidence, signalConfidence string) string {
	if dataConfidence == "medium-low" {
		return "medium-low"
	}
	if signalConfidence == "high" {
		return "high"
	}
	if dataConfidence == "" || signalConfidence == "" {
		return "medium-low"
	}
	return "medium"
}

func canaryConfidenceProfile(severity risk.SignalSeverity, p CanaryPortfolioSummary, m CanaryMarketSummary, signals []risk.Signal) (string, string, []string) {
	dataConfidence := "high"
	var reasons []string
	if m.UnrankedClusters > 0 {
		dataConfidence = "medium-low"
		reasons = append(reasons, fmt.Sprintf("%d regime cluster(s) unranked", m.UnrankedClusters))
	}
	if len(m.DegradedClusters) > 0 {
		dataConfidence = "medium-low"
		reasons = append(reasons, "degraded clusters: "+strings.Join(m.DegradedClusters, ","))
	}
	if len(m.StaleClusters) > 0 {
		dataConfidence = "medium-low"
		reasons = append(reasons, "stale clusters: "+strings.Join(m.StaleClusters, ","))
	}
	if len(m.AmbiguousClusters) > 0 {
		dataConfidence = "medium-low"
		reasons = append(reasons, "ambiguous clusters: "+strings.Join(m.AmbiguousClusters, ","))
	}
	if len(m.PartialClusters) > 0 {
		dataConfidence = "medium-low"
		reasons = append(reasons, "partial clusters: "+strings.Join(m.PartialClusters, ","))
	}
	if len(m.ComputingClusters) > 0 {
		dataConfidence = "medium-low"
		reasons = append(reasons, "computing clusters: "+strings.Join(m.ComputingClusters, ","))
	}

	signalConfidence := "medium"
	hasPortfolioSignal := false
	for _, s := range signals {
		if s.Direction == risk.DirectionDataQuality {
			continue
		}
		if s.Confidence == "high" {
			hasPortfolioSignal = true
			break
		}
	}
	if hasPortfolioSignal {
		signalConfidence = "high"
	}
	if signalConfidence == "medium" && len(signals) == 0 {
		signalConfidence = "medium"
	}
	if severity == risk.SeverityWatch && !canaryMarginPressure(p) && !canaryConfirmedTapeStress(m) && m.RedClusters < 2 {
		reasons = append(reasons, "portfolio breach lacks independent market-stress confirmation")
	}
	return dataConfidence, signalConfidence, reasons
}

func canaryPlannerMode(direction risk.SignalDirection, severity risk.SignalSeverity, dataConfidence string, p CanaryPortfolioSummary, m CanaryMarketSummary, signals []risk.Signal) risk.PlannerMode {
	if direction == risk.DirectionDataQuality {
		return risk.PlannerModeConfirmData
	}
	if direction == risk.DirectionDefensive || direction == risk.DirectionMixed {
		if severityRankAtLeast(severity, risk.SeverityAct) {
			return risk.PlannerModeDefend
		}
		if severity == risk.SeverityWatch && len(signals) > 0 {
			return risk.PlannerModeStage
		}
	}
	if direction == risk.DirectionConstructive {
		if severityRankAtLeast(severity, risk.SeverityAct) && dataConfidence == "high" && !canaryMarginPressure(p) && !canaryHasMarketDataIssue(m) {
			return risk.PlannerModeDeploy
		}
		if severity == risk.SeverityWatch {
			return risk.PlannerModeStage
		}
	}
	return risk.PlannerModeNone
}

func canaryPlannerReadiness(mode risk.PlannerMode, severity risk.SignalSeverity, dataConfidence string, p CanaryPortfolioSummary) risk.PlannerReadiness {
	switch mode {
	case risk.PlannerModeConfirmData:
		return risk.PlannerReadinessBlocked
	case risk.PlannerModeDefend:
		if dataConfidence == "medium-low" && !canaryImmediateDanger(p) {
			return risk.PlannerReadinessBlocked
		}
		return risk.PlannerReadinessReady
	case risk.PlannerModeStage:
		return risk.PlannerReadinessPrestage
	case risk.PlannerModeDeploy, risk.PlannerModeRebalance:
		return risk.PlannerReadinessReady
	default:
		if severity == risk.SeverityWatch {
			return risk.PlannerReadinessWatch
		}
		return risk.PlannerReadinessNone
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
		return nil
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
	confirmedDrop := (spyDrop && vixSpike) || m.RedClusters >= 1 || canaryMarginPressure(p)
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
		out = append(out, risk.Signal{ID: risk.SignalRegimeStressConfirmed, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Metric: "red_clusters", Observed: &observed, Threshold: &threshold, Evidence: canaryMarketEvidence(m), Confidence: "medium"})
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
	if stressed {
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
		Direction:  risk.DirectionDefensive,
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
	if stressed {
		severity = risk.SeverityAct
	}
	out := []risk.Signal{}
	if p.LargestExposurePct != nil && math.Abs(*p.LargestExposurePct) >= canaryPolicy.SingleNameExposureWatchPct {
		observed := math.Abs(*p.LargestExposurePct)
		out = append(out, risk.Signal{ID: risk.SignalSingleNameExposureHigh, Direction: risk.DirectionDefensive, Severity: severity, Subject: p.LargestExposure, Metric: "market_value_pct_nlv", Observed: &observed, Threshold: new(canaryPolicy.SingleNameExposureWatchPct), Target: new(canaryPolicy.SingleNameTargetPct), Unit: "pct_nlv", Evidence: fmt.Sprintf("%s market %.0f%% NLV", p.LargestExposure, observed), Confidence: "high"})
	}
	if p.LargestDeltaPctNLV != nil && *p.LargestDeltaPctNLV >= canaryPolicy.SingleNameDeltaWatchPct {
		out = append(out, risk.Signal{ID: risk.SignalSingleNameDeltaHigh, Direction: risk.DirectionDefensive, Severity: severity, Subject: p.LargestDeltaExposure, Metric: "delta_pct_nlv", Observed: p.LargestDeltaPctNLV, Threshold: new(canaryPolicy.SingleNameDeltaWatchPct), Target: new(canaryPolicy.SingleNameTargetPct), Unit: "pct_nlv", Evidence: fmt.Sprintf("%s delta %.0f%% NLV", p.LargestDeltaExposure, *p.LargestDeltaPctNLV), Confidence: "high"})
	}
	return out
}

func canaryOptionSignals(pos rpc.PositionsResult, m CanaryMarketSummary) []risk.Signal {
	if pos.Portfolio == nil || pos.Portfolio.GreeksTotal == 0 {
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
	blockedBy := []string{}
	for _, part := range [][]string{m.AmbiguousClusters, m.PartialClusters, m.DegradedClusters, m.ComputingClusters} {
		blockedBy = append(blockedBy, part...)
	}
	if len(blockedBy) > 0 || m.UnrankedClusters > 0 {
		observed := float64(len(blockedBy) + m.UnrankedClusters)
		out = append(out, risk.Signal{ID: risk.SignalRiskDataDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "degraded_inputs", Observed: &observed, Evidence: canaryAmbiguityEvidence(m), Confidence: "medium-low", ConfidenceImpact: "requires confirmation before severe action", BlockedBy: blockedBy})
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

func canaryOverallDirection(rows []CanaryRow, signals []risk.Signal) risk.SignalDirection {
	sawDefensive, sawConstructive, sawData := false, false, false
	for _, row := range rows {
		switch row.Direction {
		case risk.DirectionDefensive:
			sawDefensive = true
		case risk.DirectionConstructive:
			sawConstructive = true
		case risk.DirectionDataQuality:
			sawData = true
		}
	}
	for _, s := range signals {
		switch s.Direction {
		case risk.DirectionDefensive:
			sawDefensive = true
		case risk.DirectionConstructive:
			sawConstructive = true
		case risk.DirectionDataQuality:
			sawData = true
		}
	}
	switch {
	case sawDefensive && sawConstructive:
		return risk.DirectionMixed
	case sawDefensive:
		return risk.DirectionDefensive
	case sawConstructive:
		return risk.DirectionConstructive
	case sawData:
		return risk.DirectionDataQuality
	default:
		return ""
	}
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

func canaryWarnings(m CanaryMarketSummary, r rpc.RegimeSnapshotResult) []string {
	var warnings []string
	if m.UnrankedClusters > 0 {
		warnings = append(warnings, fmt.Sprintf("%d regime cluster(s) unranked; severe market actions require independent confirmation", m.UnrankedClusters))
	}
	if len(m.AmbiguousClusters) > 0 {
		warnings = append(warnings, "ambiguous clusters: "+strings.Join(m.AmbiguousClusters, ","))
	}
	if len(m.PartialClusters) > 0 {
		warnings = append(warnings, "partial clusters: "+strings.Join(m.PartialClusters, ","))
	}
	if len(m.DegradedClusters) > 0 {
		warnings = append(warnings, "degraded clusters: "+strings.Join(m.DegradedClusters, ","))
	}
	if len(m.StaleClusters) > 0 {
		warnings = append(warnings, "stale clusters: "+strings.Join(m.StaleClusters, ","))
	}
	for _, w := range r.WarningDetails {
		line := canaryWarningLine(w)
		if line == "" {
			continue
		}
		warnings = append(warnings, line)
	}
	return warnings
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
	if red == "" {
		red = "none"
	}
	if yellow == "" {
		yellow = "none"
	}
	out := fmt.Sprintf("%d red clusters (%s), %d yellow (%s), %d/%d ranked",
		m.RedClusters, red, m.YellowClusters, yellow, m.RankedClusters, m.RankedClusters+m.UnrankedClusters)
	if m.SPYChangePct != nil || m.VIXChangePct != nil {
		out += "; " + canaryTapeEvidence(m)
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
		(spyDrop && vixSpike && m.RedClusters >= 1)
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
		parts = append(parts, "stale "+strings.Join(m.StaleClusters, ","))
	}
	if len(m.AmbiguousClusters) > 0 {
		parts = append(parts, "ambiguous "+strings.Join(m.AmbiguousClusters, ","))
	}
	if len(m.PartialClusters) > 0 {
		parts = append(parts, "partial "+strings.Join(m.PartialClusters, ","))
	}
	if len(m.DegradedClusters) > 0 {
		parts = append(parts, "degraded "+strings.Join(m.DegradedClusters, ","))
	}
	if len(m.ComputingClusters) > 0 {
		parts = append(parts, "computing "+strings.Join(m.ComputingClusters, ","))
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
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "canary: takes no positional args (got %v)", fs.Args())
	}
	if !*jsonOut && isTerminal(env.Stdout) {
		stop := startCanarySpinner(env)
		res, err := FetchCanary(ctx, env.Conn)
		stop()
		if err != nil {
			return fail(env, "canary: %v", err)
		}
		return renderCanaryText(env, env.Stdout, &res)
	}
	res, err := FetchCanary(ctx, env.Conn)
	if err != nil {
		return fail(env, "canary: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderCanaryText(env, env.Stdout, &res)
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
	var acct rpc.AccountResult
	if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
		return CanaryResult{}, fmt.Errorf("account: %w", err)
	}
	var pos rpc.PositionsResult
	if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &pos); err != nil {
		return CanaryResult{}, fmt.Errorf("positions: %w", err)
	}
	var regime rpc.RegimeSnapshotResult
	if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &regime); err != nil {
		return CanaryResult{}, fmt.Errorf("regime: %w", err)
	}
	rpc.CompactRegimeSnapshot(&regime)
	return ComputeCanary(CanaryInput{Account: acct, Positions: pos, Regime: regime}), nil
}

func renderCanaryText(env *Env, out io.Writer, r *CanaryResult) int {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Portfolio Canary  ·  %s\n", r.AsOf.Format("2006-01-02 15:04 MST"))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-10s %s\n", "Risk state", canaryRiskStateLabel(env, r.Direction, r.Severity, true))
	fmt.Fprintf(out, "  %-10s %s\n", "Next step", canaryPlannerStepLabel(env, r.PlannerModeHint, r.PlannerReadiness))
	fmt.Fprintf(out, "  %-10s %s\n", "Confidence", canaryConfidenceLabel(env, r.Confidence))
	if r.DataConfidence != "" || r.SignalConfidence != "" {
		fmt.Fprintf(out, "  %-10s data %s · signals %s\n", "Quality",
			canaryConfidenceLabel(env, r.DataConfidence), canaryConfidenceLabel(env, r.SignalConfidence))
	}
	fmt.Fprintf(out, "  %-10s %s\n", "Guidance", env.bold(r.Summary))
	if len(r.PrimaryDrivers) > 0 {
		fmt.Fprintf(out, "  %-10s %s\n", "Drivers", strings.Join(signalIDStrings(r.PrimaryDrivers), ", "))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-28s %-22s %s\n", "Title", "Risk state", "Guidance")
	fmt.Fprintf(out, "  %-28s %-22s %s\n", strings.Repeat("-", 28), strings.Repeat("-", 22), strings.Repeat("-", 54))
	for _, row := range r.Rows {
		state := padRightVisible(canaryRiskStateLabel(env, row.Direction, row.Severity, false), 22)
		fmt.Fprintf(out, "  %-28s %s %s\n", row.Title, state, row.Guidance)
		if row.Evidence != "" {
			fmt.Fprintf(out, "  %-28s %-22s %s\n", "", "", env.dim(row.Evidence))
		}
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintln(out)
		for _, w := range r.Warnings {
			fmt.Fprintf(out, "  %s %s\n", env.dim("warning:"), w)
		}
	}
	return 0
}

func signalIDStrings(ids []risk.SignalID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
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
		return "Run risk-plan deploy"
	case risk.PlannerModeRebalance:
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

func canaryConfidenceLabel(env *Env, confidence string) string {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "high":
		return env.green("High")
	case "medium-low", "low":
		return env.yellow("Medium-low")
	case "medium":
		return "Medium"
	default:
		if confidence == "" {
			return "Unknown"
		}
		return confidence
	}
}
