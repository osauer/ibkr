package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

const (
	canaryDecisionHold      = "HOLD"
	canaryDecisionWatch     = "WATCH"
	canaryDecisionDelever   = "DE-LEVER"
	canaryDecisionLiquidate = "LIQUIDATE"
)

// CanaryInput is the pure decision input shared by the CLI and MCP tool.
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
// from portfolio-level decision to supporting gates so an agent can print the
// table directly without composing prose.
type CanaryResult struct {
	AsOf         time.Time              `json:"as_of"`
	SourceAsOf   CanarySourceAsOf       `json:"source_as_of,omitzero"`
	Decision     string                 `json:"decision"`
	Action       string                 `json:"action"`
	Confidence   string                 `json:"confidence"`
	Rows         []CanaryRow            `json:"rows"`
	Portfolio    CanaryPortfolioSummary `json:"portfolio"`
	Market       CanaryMarketSummary    `json:"market"`
	Warnings     []string               `json:"warnings,omitempty"`
	NotExecution string                 `json:"not_execution"`
}

type CanarySourceAsOf struct {
	Account   time.Time `json:"account,omitzero"`
	Positions time.Time `json:"positions,omitzero"`
	Regime    time.Time `json:"regime,omitzero"`
}

type CanaryRow struct {
	Title    string `json:"title"`
	Decision string `json:"decision"`
	Action   string `json:"action"`
	Evidence string `json:"evidence,omitempty"`
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

type canarySeverity int

const (
	canarySeverityHold canarySeverity = iota
	canarySeverityWatch
	canarySeverityDelever
	canarySeverityLiquidate
)

func (s canarySeverity) decision() string {
	switch s {
	case canarySeverityLiquidate:
		return canaryDecisionLiquidate
	case canarySeverityDelever:
		return canaryDecisionDelever
	case canarySeverityWatch:
		return canaryDecisionWatch
	default:
		return canaryDecisionHold
	}
}

func ComputeCanary(in CanaryInput) CanaryResult {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	res := CanaryResult{
		AsOf:         now,
		SourceAsOf:   CanarySourceAsOf{Account: in.Account.AsOf, Positions: in.Positions.AsOf, Regime: in.Regime.AsOf},
		Portfolio:    summarizeCanaryPortfolio(in.Account, in.Positions),
		Market:       summarizeCanaryMarket(in.Regime),
		NotExecution: "Read-only recommendation; no orders are placed by ibkr.",
	}

	rows := []CanaryRow{
		canaryMarginRow(res.Portfolio),
		canaryTapeShockRow(res.Portfolio, res.Market),
		canaryMarketRow(res.Market),
		canaryExposureRow(res.Portfolio, res.Market),
		canaryConcentrationRow(res.Portfolio, res.Market),
		canaryOptionsRow(res.Portfolio, in.Positions, res.Market),
		canaryDataQualityRow(res.Market, in.Regime),
	}
	overall := canaryOverallRow(rows, res.Market, res.Portfolio)
	res.Rows = append([]CanaryRow{overall}, rows...)
	res.Decision = overall.Decision
	res.Action = overall.Action
	res.Confidence = canaryConfidence(res.Decision, res.Market)
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

func canaryMarginRow(p CanaryPortfolioSummary) CanaryRow {
	cushion := canaryWorstCushionPct(p)
	if cushion != nil {
		switch {
		case *cushion < 10:
			return CanaryRow{"Immediate margin safety", canaryDecisionLiquidate, "Move to cash-heavy / near-flat now; margin cushion is below 10%.", canaryCushionEvidence(p)}
		case *cushion < 20:
			return CanaryRow{"Immediate margin safety", canaryDecisionDelever, "Cut gross and net exposure until cushion is back above 25%.", canaryCushionEvidence(p)}
		case *cushion < 35:
			return CanaryRow{"Immediate margin safety", canaryDecisionWatch, "Do not add risk; prepare a reduction plan if cushion falls below 25%.", canaryCushionEvidence(p)}
		}
		return CanaryRow{"Immediate margin safety", canaryDecisionHold, "No forced margin action.", canaryCushionEvidence(p)}
	}
	return CanaryRow{"Immediate margin safety", canaryDecisionWatch, "No forced margin action, but confirm account cushion before sizing new risk.", "cushion unavailable"}
}

func canaryMarketRow(m CanaryMarketSummary) CanaryRow {
	evidence := canaryMarketEvidence(m)
	switch {
	case m.RedClusters >= 3 && m.RankedClusters >= 4:
		return CanaryRow{"Confirmed market stress", canaryDecisionDelever, "Reduce equity beta materially; only consider liquidation if margin or exposure rows also fire.", evidence}
	case m.RedClusters >= 2 && m.RankedClusters >= 3:
		return CanaryRow{"Confirmed market stress", canaryDecisionDelever, "Cut marginal longs and short-convexity exposure; keep only intentional hedged risk.", evidence}
	case m.RedClusters == 1 && m.YellowClusters >= 1:
		return CanaryRow{"Early stress filtered", canaryDecisionWatch, "Wait for a second independent red cluster before major de-risking.", evidence}
	case m.YellowClusters >= 3:
		return CanaryRow{"Deteriorating tape", canaryDecisionWatch, "Freeze new risk and review hedges; no liquidation without red confirmation.", evidence}
	default:
		return CanaryRow{"Market stress", canaryDecisionHold, "No market-regime de-risking trigger.", evidence}
	}
}

func canaryTapeShockRow(p CanaryPortfolioSummary, m CanaryMarketSummary) CanaryRow {
	evidence := canaryTapeEvidence(m)
	if m.SPYChangePct == nil && m.VIXChangePct == nil {
		return CanaryRow{"Index tape shock", canaryDecisionWatch, "Direct SPY/VIX tape is unavailable; do not treat quiet regime clusters as complete overnight coverage.", evidence}
	}
	spyDrop := pctAtMost(m.SPYChangePct, -1.5)
	spyHardDrop := pctAtMost(m.SPYChangePct, -2.5)
	spyCrash := pctAtMost(m.SPYChangePct, -4)
	vixSpike := pctAtLeast(m.VIXChangePct, 10)
	vixHardSpike := pctAtLeast(m.VIXChangePct, 20)
	confirmed := (spyDrop && vixSpike) || m.RedClusters >= 1 || canaryMarginPressure(p)
	switch {
	case spyCrash && confirmed:
		return CanaryRow{"Index tape shock", canaryDecisionDelever, "Cut broad equity beta now; SPY is in a severe direct tape drawdown with confirmation.", evidence}
	case spyHardDrop && confirmed:
		return CanaryRow{"Index tape shock", canaryDecisionDelever, "Cut marginal longs and pre-hedge remaining beta; direct SPY stress is confirmed.", evidence}
	case vixHardSpike && (spyDrop || m.RedClusters >= 1):
		return CanaryRow{"Index tape shock", canaryDecisionDelever, "Reduce short-vol and high-beta exposure; direct VIX stress is confirmed.", evidence}
	case spyHardDrop || vixHardSpike || (spyDrop && vixSpike):
		return CanaryRow{"Index tape shock", canaryDecisionWatch, "Freeze new risk and run a second pass; direct overnight tape stress needs confirmation before major liquidation.", evidence}
	case spyDrop || vixSpike:
		return CanaryRow{"Index tape shock", canaryDecisionWatch, "Freeze new risk; direct SPY/VIX tape is flashing early stress but not enough for de-leveraging alone.", evidence}
	default:
		return CanaryRow{"Index tape shock", canaryDecisionHold, "No direct SPY/VIX overnight tape shock.", evidence}
	}
}

func canaryExposureRow(p CanaryPortfolioSummary, m CanaryMarketSummary) CanaryRow {
	gross := derefPct(p.GrossExposurePctNLV)
	delta := derefPct(p.NetDeltaPctNLV)
	grossDelta := derefPct(p.GrossDeltaPctNLV)
	evidence := fmt.Sprintf("gross %.0f%% NLV; net delta %.0f%% NLV; gross delta %.0f%% NLV", gross, delta, grossDelta)
	stressed := m.RedClusters >= 2 || canaryConfirmedTapeStress(m)
	switch {
	case (gross >= 150 || delta >= 125 || grossDelta >= 150) && stressed:
		return CanaryRow{"US equity/options exposure", canaryDecisionLiquidate, "Go near-flat on broad equity beta; close or hedge option delta first.", evidence}
	case (gross >= 100 || delta >= 80 || grossDelta >= 100) && stressed:
		return CanaryRow{"US equity/options exposure", canaryDecisionDelever, "Cut 30-50% of net equity delta and avoid adding long gamma-dollar exposure.", evidence}
	case gross >= 150 || delta >= 125 || grossDelta >= 150:
		return CanaryRow{"US equity/options exposure", canaryDecisionWatch, "Exposure is high; pre-stage reductions but wait for confirmed market stress.", evidence}
	default:
		return CanaryRow{"US equity/options exposure", canaryDecisionHold, "No exposure-based de-risking trigger.", evidence}
	}
}

func canaryConcentrationRow(p CanaryPortfolioSummary, m CanaryMarketSummary) CanaryRow {
	if (p.LargestExposurePct == nil || p.LargestExposure == "") && (p.LargestDeltaPctNLV == nil || p.LargestDeltaExposure == "") {
		return CanaryRow{"Largest concentration", canaryDecisionHold, "No concentration action from available base-currency exposure map.", "no dominant exposure"}
	}
	pct := math.Abs(derefPct(p.LargestExposurePct))
	deltaPct := derefPct(p.LargestDeltaPctNLV)
	evidence := canaryConcentrationEvidence(p)
	if (pct >= 35 || deltaPct >= 35) && (m.RedClusters >= 2 || canaryConfirmedTapeStress(m)) {
		return CanaryRow{"Largest concentration", canaryDecisionDelever, "Trim this concentration before smaller positions; cap it below 25% NLV in stress.", evidence}
	}
	if pct >= 35 || deltaPct >= 35 {
		return CanaryRow{"Largest concentration", canaryDecisionWatch, "Pre-stage a trim for this concentration if a second stress cluster confirms.", evidence}
	}
	return CanaryRow{"Largest concentration", canaryDecisionHold, "No concentration trim required by the canary.", evidence}
}

func canaryOptionsRow(p CanaryPortfolioSummary, pos rpc.PositionsResult, m CanaryMarketSummary) CanaryRow {
	if pos.Portfolio == nil || pos.Portfolio.GreeksTotal == 0 {
		return CanaryRow{"Options convexity", canaryDecisionHold, "No option-greeks action from the current portfolio snapshot.", "no option greeks required"}
	}
	coverage := float64(pos.Portfolio.GreeksCoverage) / float64(pos.Portfolio.GreeksTotal) * 100
	evidence := fmt.Sprintf("greeks %.0f%% covered (%s)", coverage, p.OptionGreeks)
	if coverage < 80 {
		return CanaryRow{"Options convexity", canaryDecisionWatch, "Do not escalate options-specific actions until greeks coverage is at least 80%.", evidence}
	}
	if pos.Portfolio.Gamma != nil && *pos.Portfolio.Gamma < 0 && m.RedClusters >= 2 {
		return CanaryRow{"Options convexity", canaryDecisionDelever, "Reduce negative-gamma structures first; prefer defined-risk or hedged residuals.", evidence}
	}
	return CanaryRow{"Options convexity", canaryDecisionHold, "No option-convexity de-risking trigger.", evidence}
}

func canaryDataQualityRow(m CanaryMarketSummary, r rpc.RegimeSnapshotResult) CanaryRow {
	if canaryHasMarketDataIssue(m) && (m.RedClusters > 0 || m.YellowClusters > 0) {
		return CanaryRow{"Ambiguity filter", canaryDecisionWatch, "Treat this as unresolved or degraded stress: verify weak clusters, but do not suppress confirmed independent red signals.", canaryAmbiguityEvidence(m)}
	}
	if canaryHasMarketDataIssue(m) {
		return CanaryRow{"Ambiguity filter", canaryDecisionWatch, "Market inputs are incomplete or stale; keep the canary on Watch until coverage and freshness recover.", canaryAmbiguityEvidence(m)}
	}
	if m.RankedClusters < 4 {
		return CanaryRow{"Data quality gate", canaryDecisionWatch, "Do not liquidate on market data alone; wait for at least four ranked regime clusters.", canaryMarketEvidence(m)}
	}
	if r.GammaZero.Status == rpc.RegimeStatusComputing || r.Breadth.Status == rpc.RegimeStatusComputing {
		return CanaryRow{"Data quality gate", canaryDecisionWatch, "Do not escalate on gamma/breadth until the daemon finishes the cached compute.", canaryMarketEvidence(m)}
	}
	return CanaryRow{"Data quality gate", canaryDecisionHold, "Market data coverage is sufficient for the canary policy.", canaryMarketEvidence(m)}
}

func canaryOverallRow(rows []CanaryRow, m CanaryMarketSummary, p CanaryPortfolioSummary) CanaryRow {
	sev := canarySeverityHold
	for _, row := range rows {
		sev = max(sev, canarySeverityFromDecision(row.Decision))
	}
	if sev == canarySeverityLiquidate && !canaryImmediateDanger(p) && m.RedClusters < 2 {
		sev = canarySeverityDelever
	}
	if sev == canarySeverityLiquidate && len(m.AmbiguousClusters) > 2 && !canaryImmediateDanger(p) {
		sev = canarySeverityDelever
	}
	if sev == canarySeverityDelever && m.RedClusters == 0 && !canaryMarginPressure(p) && !canaryConfirmedTapeStress(m) {
		sev = canarySeverityWatch
	}
	decision := sev.decision()
	return CanaryRow{
		Title:    "Portfolio canary",
		Decision: decision,
		Action:   canaryOverallAction(decision),
		Evidence: fmt.Sprintf("%s; %s", canaryMarketEvidence(m), canaryPortfolioEvidence(p)),
	}
}

func canarySeverityFromDecision(decision string) canarySeverity {
	switch decision {
	case canaryDecisionLiquidate:
		return canarySeverityLiquidate
	case canaryDecisionDelever:
		return canarySeverityDelever
	case canaryDecisionWatch:
		return canarySeverityWatch
	default:
		return canarySeverityHold
	}
}

func canaryImmediateDanger(p CanaryPortfolioSummary) bool {
	cushion := canaryWorstCushionPct(p)
	return cushion != nil && *cushion < 10
}

func canaryMarginPressure(p CanaryPortfolioSummary) bool {
	cushion := canaryWorstCushionPct(p)
	return cushion != nil && *cushion < 20
}

func canaryOverallAction(decision string) string {
	switch decision {
	case canaryDecisionLiquidate:
		return "Liquidate or hedge to near-flat equity beta immediately; preserve only explicit hedges and cash."
	case canaryDecisionDelever:
		return "Cut 30-50% of net equity beta, close weakest longs, and reduce negative-gamma or concentrated option risk."
	case canaryDecisionWatch:
		return "Freeze new risk, pre-stage reductions, and wait for independent confirmation before major liquidation."
	default:
		return "Hold current risk posture; no canary de-risking action."
	}
}

func canaryConfidence(decision string, m CanaryMarketSummary) string {
	if decision == canaryDecisionLiquidate || decision == canaryDecisionDelever {
		if m.RedClusters >= 2 && m.RankedClusters >= 4 {
			return "high"
		}
		return "medium"
	}
	if m.UnrankedClusters > 0 || canaryHasMarketDataIssue(m) {
		return "medium-low"
	}
	return "medium"
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
	spyDrop := pctAtMost(m.SPYChangePct, -1.5)
	spyHardDrop := pctAtMost(m.SPYChangePct, -2.5)
	vixSpike := pctAtLeast(m.VIXChangePct, 10)
	vixHardSpike := pctAtLeast(m.VIXChangePct, 20)
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
	fmt.Fprintf(out, "  %-10s %s\n", "Stage", canaryDecisionBadge(env, r.Decision, true))
	fmt.Fprintf(out, "  %-10s %s\n", "Confidence", canaryConfidenceLabel(env, r.Confidence))
	fmt.Fprintf(out, "  %-10s %s\n", "Action", env.bold(r.Action))
	fmt.Fprintf(out, "  %-10s %s\n", "Escalate", canaryStageLadder(env, r.Decision))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-28s %-10s %s\n", "Title", "Stage", "Action")
	fmt.Fprintf(out, "  %-28s %-10s %s\n", strings.Repeat("-", 28), strings.Repeat("-", 10), strings.Repeat("-", 54))
	for _, row := range r.Rows {
		stage := padRightVisible(canaryDecisionBadge(env, row.Decision, false), 10)
		fmt.Fprintf(out, "  %-28s %s %s\n", row.Title, stage, row.Action)
		if row.Evidence != "" {
			fmt.Fprintf(out, "  %-28s %-10s %s\n", "", "", env.dim(row.Evidence))
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

func canaryStageLadder(env *Env, decision string) string {
	stages := []string{
		canaryDecisionHold,
		canaryDecisionWatch,
		canaryDecisionDelever,
		canaryDecisionLiquidate,
	}
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		parts = append(parts, canaryDecisionBadge(env, stage, stage == decision))
	}
	return strings.Join(parts, " > ")
}

func canaryDecisionBadge(env *Env, decision string, current bool) string {
	label := canaryDisplayDecision(decision)
	if current {
		label = "[" + label + "]"
	}
	switch decision {
	case canaryDecisionHold:
		label = env.green(label)
	case canaryDecisionWatch:
		label = env.yellow(label)
	case canaryDecisionDelever, canaryDecisionLiquidate:
		label = env.red(label)
	}
	if current {
		label = env.bold(label)
	}
	return label
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

func canaryDisplayDecision(decision string) string {
	switch decision {
	case canaryDecisionHold:
		return "Go"
	case canaryDecisionWatch:
		return "Watch"
	case canaryDecisionDelever:
		return "De-lever"
	case canaryDecisionLiquidate:
		return "Liquidate"
	default:
		return decision
	}
}
