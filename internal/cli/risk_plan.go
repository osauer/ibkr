package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
)

type RiskPlanInput struct {
	Account       rpc.AccountResult
	Positions     rpc.PositionsResult
	Canary        rpc.CanaryResult
	TriggerCanary *rpc.CanaryResult
	RequestedMode string
	Now           time.Time
}

func runRiskPlan(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "risk-plan")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	mode := fs.String("mode", rpc.RiskPlanModeAuto, "planner mode: auto | defend | rebalance | stage | confirm-data | deploy")
	fromCanary := fs.String("from-canary", "", "optional canary JSON artifact that triggered this plan")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "risk-plan: takes no positional args (got %v)", fs.Args())
	}
	var trigger *rpc.CanaryResult
	if strings.TrimSpace(*fromCanary) != "" {
		loaded, err := loadCanaryArtifact(strings.TrimSpace(*fromCanary))
		if err != nil {
			return fail(env, "risk-plan: %v", err)
		}
		trigger = &loaded
	}
	res, err := FetchRiskPlan(ctx, env.Conn, strings.TrimSpace(*mode), trigger)
	if err != nil {
		return fail(env, "risk-plan: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderRiskPlanText(env, &res)
	return 0
}

func loadCanaryArtifact(path string) (rpc.CanaryResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return rpc.CanaryResult{}, fmt.Errorf("read canary artifact %s: %w", path, err)
	}
	var out rpc.CanaryResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return rpc.CanaryResult{}, fmt.Errorf("decode canary artifact %s: %w", path, err)
	}
	if out.Fingerprint.Key == "" {
		return rpc.CanaryResult{}, fmt.Errorf("canary artifact %s has no fingerprint", path)
	}
	return out, nil
}

func FetchRiskPlan(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, mode string, trigger *rpc.CanaryResult) (rpc.RiskPlanResult, error) {
	var acct rpc.AccountResult
	if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
		return rpc.RiskPlanResult{}, fmt.Errorf("account: %w", err)
	}
	var pos rpc.PositionsResult
	if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &pos); err != nil {
		return rpc.RiskPlanResult{}, fmt.Errorf("positions: %w", err)
	}
	var regime rpc.RegimeSnapshotResult
	if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &regime); err != nil {
		return rpc.RiskPlanResult{}, fmt.Errorf("regime: %w", err)
	}
	if acct.DailyPnL == nil {
		var refreshed rpc.AccountResult
		if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &refreshed); err == nil && refreshed.DailyPnL != nil {
			acct = refreshed
		}
	}
	rpc.CompactRegimeSnapshot(&regime)
	canary := ComputeCanary(CanaryInput{Account: acct, Positions: pos, Regime: regime})
	return ComputeRiskPlan(RiskPlanInput{
		Account:       acct,
		Positions:     pos,
		Canary:        canary,
		TriggerCanary: trigger,
		RequestedMode: mode,
	}), nil
}

func ComputeRiskPlan(in RiskPlanInput) rpc.RiskPlanResult {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	policy := risk.DefaultPolicy()
	requested := normalizeRiskPlanMode(in.RequestedMode)
	responseMode := resolveRiskPlanMode(requested, in.Canary)
	policyFP := rpc.Fingerprint{Version: risk.PolicyFingerprintVersion, Key: policy.FingerprintKey()}
	triggerFP := in.Canary.Fingerprint
	if in.TriggerCanary != nil && in.TriggerCanary.Fingerprint.Key != "" {
		triggerFP = in.TriggerCanary.Fingerprint
	}
	riskBefore := riskPlanSnapshot(in.Account, in.Positions, in.Canary)
	out := rpc.RiskPlanResult{
		Kind:                       rpc.RiskPlanKind,
		SchemaVersion:              rpc.RiskPlanSchemaVersion,
		AsOf:                       now,
		AccountID:                  firstNonEmpty(in.Account.AccountID, in.Positions.AccountID),
		BaseCurrency:               firstNonEmpty(in.Account.BaseCurrency, riskBefore.BaseCurrency),
		RequestedMode:              requested,
		ResponseMode:               responseMode,
		PolicyProfile:              policy.PolicyProfile(),
		PolicyVersion:              policy.PolicyVersion(),
		PolicyFingerprint:          policyFP,
		TriggerCanaryFingerprint:   &triggerFP,
		RefreshedCanaryFingerprint: in.Canary.Fingerprint,
		SourceAsOf:                 in.Canary.SourceAsOf,
		SourceFingerprints:         in.Canary.SourceFingerprints,
		Canary: rpc.RiskPlanCanarySummary{
			Action:             in.Canary.Action,
			Direction:          in.Canary.Direction,
			Severity:           in.Canary.Severity,
			PlannerModeHint:    in.Canary.PlannerModeHint,
			PlannerReadiness:   in.Canary.PlannerReadiness,
			MarketConfirmation: in.Canary.MarketConfirmation,
			PortfolioFit:       in.Canary.PortfolioFit,
			InputHealth:        in.Canary.InputHealth,
			Summary:            in.Canary.Summary,
			PrimaryDrivers:     append([]risk.SignalID(nil), in.Canary.PrimaryDrivers...),
			Signals:            append([]risk.Signal(nil), in.Canary.Signals...),
		},
		RiskBefore:         riskBefore,
		BestPracticeChecks: riskPlanPracticeChecks(),
		ExecutionAuthority: rpc.RiskPlanExecutionAuthorityNone,
		NextRequiredStep:   rpc.RiskPlanNextExpertReview,
		NotExecution:       "Read-only risk plan; it proposes reduce-only candidate intents and never places orders or mints submit-capable tokens.",
	}
	out.Warnings = riskPlanWarnings(in.Positions, riskBefore, policy)
	candidates := []rpc.RiskPlanCandidate{}
	if bb := concentrationCandidate("BB", in, policy, riskBefore, responseMode); bb != nil {
		candidates = append(candidates, *bb)
	}
	if nowFront := nowFrontDTECandidate(in, policy, riskBefore, responseMode); nowFront != nil {
		candidates = append(candidates, *nowFront)
	}
	if nowMid := nowMidDTECandidate(in, policy, riskBefore, responseMode); nowMid != nil {
		candidates = append(candidates, *nowMid)
	}
	if stock := stockTrimCandidate("CRWV", in, policy, riskBefore, responseMode); stock != nil {
		candidates = append(candidates, *stock)
	}
	for i := range candidates {
		candidates[i].Rank = i + 1
		candidates[i].ID = riskPlanCandidateID(out, candidates[i])
		candidates[i].PreviewCommand = fmt.Sprintf("ibkr order preview --from-plan PLAN.json --candidate %s", candidates[i].ID)
	}
	out.Candidates = candidates
	out.PlanID = riskPlanID(out)
	return out
}

func normalizeRiskPlanMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", rpc.RiskPlanModeAuto:
		return rpc.RiskPlanModeAuto
	case rpc.RiskPlanModeDefend, rpc.RiskPlanModeRebalance, rpc.RiskPlanModeStage, rpc.RiskPlanModeConfirmData, rpc.RiskPlanModeDeploy:
		return mode
	default:
		return rpc.RiskPlanModeAuto
	}
}

func resolveRiskPlanMode(requested string, canary rpc.CanaryResult) string {
	if requested != rpc.RiskPlanModeAuto {
		return requested
	}
	if hasActOrUrgentSignal(canary.Signals, risk.SignalMarginCushionLow) ||
		hasActOrUrgentSignal(canary.Signals, risk.SignalLookAheadCushionLow) ||
		hasActOrUrgentSignal(canary.Signals, risk.SignalPortfolioPnLShock) {
		return rpc.RiskPlanModeDefend
	}
	switch canary.PlannerModeHint {
	case risk.PlannerModeDefend:
		return rpc.RiskPlanModeDefend
	case risk.PlannerModeRebalance:
		return rpc.RiskPlanModeRebalance
	case risk.PlannerModeDeploy:
		return rpc.RiskPlanModeDeploy
	case risk.PlannerModeConfirmData:
		return rpc.RiskPlanModeConfirmData
	default:
		return rpc.RiskPlanModeStage
	}
}

func hasActOrUrgentSignal(signals []risk.Signal, id risk.SignalID) bool {
	for _, sig := range signals {
		if sig.ID != id {
			continue
		}
		return sig.Severity == risk.SeverityAct || sig.Severity == risk.SeverityUrgent
	}
	return false
}

func riskPlanSnapshot(acct rpc.AccountResult, pos rpc.PositionsResult, canary rpc.CanaryResult) rpc.RiskPlanRiskSnapshot {
	out := rpc.RiskPlanRiskSnapshot{
		NetLiquidationBase:    canary.Portfolio.NetLiquidation,
		BaseCurrency:          canary.Portfolio.BaseCurrency,
		MarginCushionPct:      canary.Portfolio.CushionPct,
		LookAheadCushionPct:   canary.Portfolio.LookAheadCushionPct,
		GrossExposurePctNLV:   canary.Portfolio.GrossExposurePctNLV,
		NetDeltaPctNLV:        canary.Portfolio.NetDeltaPctNLV,
		GrossDeltaPctNLV:      canary.Portfolio.GrossDeltaPctNLV,
		DailyPnLPctNLV:        canary.Portfolio.DailyPnLPct,
		LargestExposure:       canary.Portfolio.LargestExposure,
		LargestExposurePctNLV: canary.Portfolio.LargestExposurePct,
		LargestDeltaExposure:  canary.Portfolio.LargestDeltaExposure,
		LargestDeltaPctNLV:    canary.Portfolio.LargestDeltaPctNLV,
		SPYHedgeOffsetPct:     rpc.CompactPositionsRisk(&pos, 5).SPYHedgeOffsetPct,
		OptionGreeks:          canary.Portfolio.OptionGreeks,
	}
	if out.NetLiquidationBase == 0 && pos.Portfolio != nil && pos.Portfolio.NetLiquidationBase != nil {
		out.NetLiquidationBase = *pos.Portfolio.NetLiquidationBase
	}
	if out.NetLiquidationBase == 0 {
		out.NetLiquidationBase = acct.NetLiquidation
	}
	if out.BaseCurrency == "" && pos.Portfolio != nil {
		out.BaseCurrency = pos.Portfolio.BaseCurrency
	}
	if out.BaseCurrency == "" {
		out.BaseCurrency = acct.BaseCurrency
	}
	return out
}

func riskPlanWarnings(pos rpc.PositionsResult, snap rpc.RiskPlanRiskSnapshot, policy risk.Policy) []string {
	warnings := []string{}
	riskView := rpc.CompactPositionsRisk(&pos, 5)
	for _, leg := range riskView.FlaggedOptionLegs {
		warnings = append(warnings, fmt.Sprintf("%s %s %s %.4g%s flagged: %s",
			leg.Symbol, leg.Expiry, leg.Right, leg.Strike, quantityLabel(leg.Quantity), strings.Join(leg.Reasons, ",")))
	}
	if snap.SPYHedgeOffsetPct != nil &&
		*snap.SPYHedgeOffsetPct >= policy.Reduce.HedgeOffsetMinPct &&
		*snap.SPYHedgeOffsetPct <= policy.Reduce.HedgeOffsetMaxPct &&
		snap.NetDeltaPctNLV != nil && *snap.NetDeltaPctNLV > 0 {
		warnings = append(warnings, fmt.Sprintf("SPY/index long-put hedge offset %.1f%% is inside the %.0f-%.0f%% preservation band; no hedge-sale candidate emitted.",
			*snap.SPYHedgeOffsetPct, policy.Reduce.HedgeOffsetMinPct, policy.Reduce.HedgeOffsetMaxPct))
	}
	return warnings
}

func concentrationCandidate(symbol string, in RiskPlanInput, policy risk.Policy, before rpc.RiskPlanRiskSnapshot, mode string) *rpc.RiskPlanCandidate {
	exposure := exposureForSymbol(in.Positions, symbol)
	if exposure == nil || exposure.MarketValuePctNLV == nil || *exposure.MarketValuePctNLV <= policy.SingleNameTargetPct || before.NetLiquidationBase <= 0 {
		return nil
	}
	targetMarketValue := before.NetLiquidationBase * policy.SingleNameTargetPct / 100
	needed := max(exposure.MarketValueBase-targetMarketValue, 0)
	if needed <= 0 {
		return nil
	}
	legs := optionReductionLegs(symbol, in.Positions, policy, needed, in.Canary.AsOf)
	if len(legs) == 0 {
		return nil
	}
	c := rpc.RiskPlanCandidate{
		Status:          candidateStatus(legs),
		Intent:          "reduce_single_name_concentration",
		Subject:         strings.ToUpper(symbol),
		Reason:          fmt.Sprintf("%s market exposure is %.1f%% NLV; reduce toward the %.0f%% policy target with the smallest option sales first.", strings.ToUpper(symbol), *exposure.MarketValuePctNLV, policy.SingleNameTargetPct),
		PolicySignalIDs: []risk.SignalID{risk.SignalSingleNameExposureHigh, risk.SignalGrossDeltaHigh, risk.SignalNetDeltaHigh},
		Legs:            legs,
		References:      []string{"FINRA options: closing sale exits a long option position", "IBKR WhatIf required before any submit-capable token"},
	}
	applyCandidateEstimates(&c, in, before, mode)
	return &c
}

func nowFrontDTECandidate(in RiskPlanInput, policy risk.Policy, before rpc.RiskPlanRiskSnapshot, mode string) *rpc.RiskPlanCandidate {
	nowLegs := optionLegsFor("NOW", in.Positions)
	front := []rpc.PositionView{}
	for _, leg := range nowLegs {
		dte := dte(leg.Expiry, in.Canary.AsOf)
		if dte != nil && *dte <= policy.Reduce.FrontDTE && leg.Quantity > 0 {
			front = append(front, leg)
		}
	}
	if len(front) == 0 {
		return nil
	}
	legs := make([]rpc.RiskPlanCandidateLeg, 0, len(front))
	for _, leg := range front {
		legs = append(legs, riskPlanLegFromOption(leg, int(math.Abs(leg.Quantity)), policy, in.Canary.AsOf))
	}
	c := rpc.RiskPlanCandidate{
		Status:          candidateStatus(legs),
		Intent:          "reduce_front_dte_loss_and_gamma",
		Subject:         "NOW",
		Reason:          fmt.Sprintf("NOW front-DTE calls are inside the <=%d day policy bucket after a large daily loss; close the front bucket before touching longer-dated optionality.", policy.Reduce.FrontDTE),
		PolicySignalIDs: []risk.SignalID{risk.SignalPortfolioPnLShock, risk.SignalGrossDeltaHigh},
		Legs:            legs,
		References:      []string{"FINRA options: expiry changes option risk and closing sale realizes the exit", "OCC ODD governs option characteristics and risks"},
	}
	applyCandidateEstimates(&c, in, before, mode)
	return &c
}

func nowMidDTECandidate(in RiskPlanInput, policy risk.Policy, before rpc.RiskPlanRiskSnapshot, mode string) *rpc.RiskPlanCandidate {
	exposure := exposureForSymbol(in.Positions, "NOW")
	if exposure == nil || exposure.DailyPnLBase == nil || before.NetLiquidationBase <= 0 {
		return nil
	}
	lossPct := *exposure.DailyPnLBase / before.NetLiquidationBase * 100
	if lossPct > -policy.DailyPnLWatchPct {
		return nil
	}
	legs := []rpc.RiskPlanCandidateLeg{}
	for _, opt := range optionLegsFor("NOW", in.Positions) {
		days := dte(opt.Expiry, in.Canary.AsOf)
		if days == nil || *days <= policy.Reduce.FrontDTE || *days > policy.Reduce.MidDTE || opt.Quantity <= 0 {
			continue
		}
		qty := min(int(math.Abs(opt.Quantity)), 10)
		if qty > 0 {
			legs = append(legs, riskPlanLegFromOption(opt, qty, policy, in.Canary.AsOf))
			break
		}
	}
	if len(legs) == 0 {
		return nil
	}
	c := rpc.RiskPlanCandidate{
		Status:          candidateStatus(legs),
		Intent:          "reduce_mid_dte_high_loss_delta",
		Subject:         "NOW",
		Reason:          fmt.Sprintf("NOW daily P&L is %.1f%% NLV; reduce a small mid-DTE slice after the front bucket to lower remaining delta without liquidating all optionality.", lossPct),
		PolicySignalIDs: []risk.SignalID{risk.SignalPortfolioPnLShock, risk.SignalGrossDeltaHigh, risk.SignalNetDeltaHigh},
		Legs:            legs,
		References:      []string{"FINRA options: options are leveraged and contract-specific"},
	}
	applyCandidateEstimates(&c, in, before, mode)
	return &c
}

func stockTrimCandidate(symbol string, in RiskPlanInput, policy risk.Policy, before rpc.RiskPlanRiskSnapshot, mode string) *rpc.RiskPlanCandidate {
	exposure := exposureForSymbol(in.Positions, symbol)
	if exposure == nil || exposure.MarketValuePctNLV == nil || *exposure.MarketValuePctNLV <= policy.SingleNameTargetPct || before.NetLiquidationBase <= 0 {
		return nil
	}
	stock := stockForSymbol(in.Positions, symbol)
	if stock == nil || stock.Quantity <= 0 || stock.MarketValueBase == nil || *stock.MarketValueBase <= 0 {
		return nil
	}
	target := before.NetLiquidationBase * policy.SingleNameTargetPct / 100
	reduceBase := max(exposure.MarketValueBase-target, 0)
	perShareBase := *stock.MarketValueBase / stock.Quantity
	qty := int(math.Ceil(reduceBase / perShareBase))
	qty = min(qty, int(math.Abs(stock.Quantity)))
	if qty <= 0 {
		return nil
	}
	limit := stock.Mark
	mv := perShareBase * float64(qty)
	delta := mv
	leg := rpc.RiskPlanCandidateLeg{
		Action:              "SELL",
		Contract:            positionContract(*stock, "STK"),
		Quantity:            qty,
		HeldQuantity:        stock.Quantity,
		PositionEffect:      positionEffect(stock.Quantity, qty),
		OrderType:           policy.Reduce.OrderType,
		TIF:                 policy.Reduce.TIF,
		LimitStrategy:       rpc.OrderStrategyPatientLimit,
		EstimatedLimitPrice: &limit,
		MarketValueBase:     mv,
		DollarDeltaBase:     &delta,
	}
	c := rpc.RiskPlanCandidate{
		Status:          rpc.RiskPlanCandidatePreviewable,
		Intent:          "reduce_stock_margin_and_concentration",
		Subject:         strings.ToUpper(symbol),
		Reason:          fmt.Sprintf("%s stock is %.1f%% NLV, above the %.0f%% target; trim only the excess if margin pressure remains after option reductions.", strings.ToUpper(symbol), *exposure.MarketValuePctNLV, policy.SingleNameTargetPct),
		PolicySignalIDs: []risk.SignalID{risk.SignalMarginCushionLow, risk.SignalLookAheadCushionLow},
		Legs:            []rpc.RiskPlanCandidateLeg{leg},
		References:      []string{"IBKR WhatIf required before any submit-capable token"},
	}
	applyCandidateEstimates(&c, in, before, mode)
	return &c
}

func optionReductionLegs(symbol string, pos rpc.PositionsResult, policy risk.Policy, neededBase float64, now time.Time) []rpc.RiskPlanCandidateLeg {
	options := optionLegsFor(symbol, pos)
	slices.SortStableFunc(options, func(a, b rpc.PositionView) int {
		if c := cmpExpiryBucket(a.Expiry, b.Expiry, now); c != 0 {
			return c
		}
		if strings.EqualFold(a.Right, "C") && strings.EqualFold(b.Right, "C") {
			return cmpFloatDesc(a.Strike, b.Strike)
		}
		return cmpFloatDesc(absPositionMarketValue(a), absPositionMarketValue(b))
	})
	remaining := neededBase
	legs := []rpc.RiskPlanCandidateLeg{}
	for _, opt := range options {
		if remaining <= 0 || opt.Quantity <= 0 {
			continue
		}
		perContract := absPositionMarketValueBase(opt) / math.Abs(opt.Quantity)
		if perContract <= 0 {
			continue
		}
		qty := min(int(math.Ceil(remaining/perContract)), int(math.Abs(opt.Quantity)))
		if qty <= 0 {
			continue
		}
		legs = append(legs, riskPlanLegFromOption(opt, qty, policy, now))
		remaining -= perContract * float64(qty)
	}
	return legs
}

func optionLegsFor(symbol string, pos rpc.PositionsResult) []rpc.PositionView {
	out := []rpc.PositionView{}
	for _, opt := range pos.Options {
		if strings.EqualFold(opt.Symbol, symbol) && opt.MarketValueBase != nil && math.Abs(*opt.MarketValueBase) > 0 {
			out = append(out, opt)
		}
	}
	return out
}

func riskPlanLegFromOption(opt rpc.PositionView, qty int, policy risk.Policy, now time.Time) rpc.RiskPlanCandidateLeg {
	action := "SELL_TO_CLOSE"
	orderAction := rpc.OrderActionSell
	if opt.Quantity < 0 {
		action = "BUY_TO_CLOSE"
		orderAction = rpc.OrderActionBuy
	}
	bid, ask, mid, spread, spreadPct := optionQuoteFields(opt)
	limit := bid
	if orderAction == rpc.OrderActionBuy {
		limit = ask
	}
	mv := absPositionMarketValueBase(opt) / math.Abs(opt.Quantity) * float64(qty)
	dd := optionDollarDeltaBase(opt, qty)
	realized := positionRealizedPnLEstimate(opt, qty)
	leg := rpc.RiskPlanCandidateLeg{
		Action:                  action,
		Contract:                positionContract(opt, "OPT"),
		Quantity:                qty,
		HeldQuantity:            opt.Quantity,
		PositionEffect:          positionEffect(opt.Quantity, qty),
		OrderType:               policy.Reduce.OrderType,
		TIF:                     policy.Reduce.TIF,
		LimitStrategy:           rpc.OrderStrategyPatientLimit,
		EstimatedLimitPrice:     limit,
		Bid:                     bid,
		Ask:                     ask,
		Mid:                     mid,
		Spread:                  spread,
		SpreadPctOfMid:          spreadPct,
		DTE:                     dte(opt.Expiry, now),
		Moneyness:               optionMoneyness(opt),
		Delta:                   opt.Delta,
		Gamma:                   opt.Gamma,
		Theta:                   opt.Theta,
		Vega:                    opt.Vega,
		MarketValueBase:         mv,
		DollarDeltaBase:         dd,
		RealizedPnLEstimateBase: realized,
	}
	leg.Warnings = optionLegWarnings(opt, policy, spread, spreadPct)
	return leg
}

func positionContract(p rpc.PositionView, secType string) rpc.ContractParams {
	return rpc.ContractParams{
		Symbol:       strings.ToUpper(strings.TrimSpace(p.Symbol)),
		SecType:      secType,
		Exchange:     "SMART",
		Currency:     strings.ToUpper(strings.TrimSpace(p.Currency)),
		LocalSymbol:  strings.TrimSpace(p.LocalSymbol),
		TradingClass: strings.TrimSpace(p.TradingClass),
		Expiry:       p.Expiry,
		Strike:       p.Strike,
		Right:        strings.ToUpper(strings.TrimSpace(p.Right)),
	}
}

func optionQuoteFields(opt rpc.PositionView) (*float64, *float64, *float64, *float64, *float64) {
	bid := opt.OptionBid
	ask := opt.OptionAsk
	if bid == nil || ask == nil || *bid <= 0 || *ask <= 0 {
		return bid, ask, nil, nil, nil
	}
	mid := (*bid + *ask) / 2
	spread := *ask - *bid
	pct := 0.0
	if mid > 0 {
		pct = spread / mid * 100
	}
	return bid, ask, &mid, &spread, &pct
}

func optionLegWarnings(opt rpc.PositionView, policy risk.Policy, spread, spreadPct *float64) []string {
	warnings := []string{}
	if opt.MarkOutsideBidAsk {
		warnings = append(warnings, "mark_outside_bid_ask")
	}
	if spread == nil || spreadPct == nil {
		warnings = append(warnings, "missing_bid_ask_blocks_submit_eligible_preview")
		return warnings
	}
	maxSpread := policy.Reduce.MaxOptionSpreadAbs
	if *spreadPct != 0 {
		mid := (*spread) / (*spreadPct / 100)
		maxSpread = max(maxSpread, mid*policy.Reduce.MaxOptionSpreadPctOfMid/100)
	}
	if *spread > maxSpread {
		warnings = append(warnings, fmt.Sprintf("spread %.4f exceeds max %.4f", *spread, maxSpread))
	}
	return warnings
}

func candidateStatus(legs []rpc.RiskPlanCandidateLeg) string {
	for _, leg := range legs {
		for _, warning := range leg.Warnings {
			if strings.Contains(warning, "missing_bid_ask") || strings.Contains(warning, "exceeds max") {
				return rpc.RiskPlanCandidateBlocked
			}
		}
	}
	return rpc.RiskPlanCandidatePreviewable
}

func applyCandidateEstimates(c *rpc.RiskPlanCandidate, in RiskPlanInput, before rpc.RiskPlanRiskSnapshot, mode string) {
	marketReduction := 0.0
	deltaReduction := 0.0
	realized := 0.0
	hasRealized := false
	slippage := 0.0
	hasSlippage := false
	for _, leg := range c.Legs {
		marketReduction += leg.MarketValueBase
		if leg.DollarDeltaBase != nil {
			deltaReduction += *leg.DollarDeltaBase
		}
		if leg.RealizedPnLEstimateBase != nil {
			realized += *leg.RealizedPnLEstimateBase
			hasRealized = true
		}
		if leg.Spread != nil {
			slippage += *leg.Spread / 2 * float64(leg.Quantity) * contractMultiplier(leg.Contract)
			hasSlippage = true
		}
	}
	after := before
	if before.NetLiquidationBase > 0 {
		ge := subtractPct(before.GrossExposurePctNLV, marketReduction/before.NetLiquidationBase*100)
		after.GrossExposurePctNLV = ge
		if in.Positions.Portfolio != nil && in.Positions.Portfolio.DollarDeltaBase != nil {
			net := math.Abs(*in.Positions.Portfolio.DollarDeltaBase-deltaReduction) / before.NetLiquidationBase * 100
			after.NetDeltaPctNLV = &net
		} else {
			after.NetDeltaPctNLV = subtractPct(before.NetDeltaPctNLV, math.Abs(deltaReduction)/before.NetLiquidationBase*100)
		}
		after.GrossDeltaPctNLV = subtractPct(before.GrossDeltaPctNLV, math.Abs(deltaReduction)/before.NetLiquidationBase*100)
		if strings.EqualFold(c.Subject, before.LargestExposure) && before.LargestExposurePctNLV != nil {
			v := max(*before.LargestExposurePctNLV-marketReduction/before.NetLiquidationBase*100, 0)
			after.LargestExposurePctNLV = &v
		}
	}
	c.EstimatedRiskAfter = after
	reduction := rpc.RiskPlanReduction{
		MarketValueBase:     marketReduction,
		GrossExposurePctNLV: pctPtr(marketReduction, before.NetLiquidationBase),
		NetDeltaPctNLV:      pctPtr(math.Abs(deltaReduction), before.NetLiquidationBase),
		GrossDeltaPctNLV:    pctPtr(math.Abs(deltaReduction), before.NetLiquidationBase),
	}
	if hasRealized {
		reduction.RealizedPnLBase = &realized
	}
	c.EstimatedReduction = reduction
	maxSpreadRule := fmt.Sprintf("option spread <= max(%.2f, %.0f%% of mid); LMT DAY only; broker WhatIf required", risk.DefaultPolicy().Reduce.MaxOptionSpreadAbs, risk.DefaultPolicy().Reduce.MaxOptionSpreadPctOfMid)
	c.EstimatedTradingCost = rpc.RiskPlanTradingCost{MaxSpreadRule: maxSpreadRule, WhatIfRequired: true}
	if hasSlippage {
		c.EstimatedTradingCost.EstimatedSlippageBase = &slippage
	}
	if mode == rpc.RiskPlanModeConfirmData {
		c.Status = rpc.RiskPlanCandidateInformational
		c.BlockedBy = append(c.BlockedBy, "confirm_data_mode")
	}
}

func riskPlanPracticeChecks() []rpc.RiskPlanPracticeCheck {
	return []rpc.RiskPlanPracticeCheck{
		{ID: "option_contract_specific_risk", Status: "applied", Reference: "https://www.finra.org/investors/investing/investment-products/options", Note: "Candidates carry DTE, strike/right, greeks, multiplier, bid/ask, and close-only action."},
		{ID: "occ_odd_governance", Status: "applied", Reference: "https://www.theocc.com/company-information/documents-and-archives/options-disclosure-document", Note: "Risk-plan treats options as governed products and does not open new option exposure in v1."},
		{ID: "ibkr_whatif_margin_preview", Status: "required", Reference: "https://interactivebrokers.github.io/tws-api/margin.html", Note: "No submit-capable token is valid without broker WhatIf margin acceptance."},
	}
}

func riskPlanCandidateID(plan rpc.RiskPlanResult, c rpc.RiskPlanCandidate) string {
	projection := struct {
		Schema            string                     `json:"schema"`
		PolicyProfile     string                     `json:"policy_profile"`
		PolicyVersion     string                     `json:"policy_version"`
		PolicyFingerprint rpc.Fingerprint            `json:"policy_fingerprint"`
		Trigger           *rpc.Fingerprint           `json:"trigger"`
		Intent            string                     `json:"intent"`
		Subject           string                     `json:"subject"`
		Legs              []rpc.RiskPlanCandidateLeg `json:"legs"`
	}{
		Schema:            rpc.RiskPlanSchemaVersion,
		PolicyProfile:     plan.PolicyProfile,
		PolicyVersion:     plan.PolicyVersion,
		PolicyFingerprint: plan.PolicyFingerprint,
		Trigger:           plan.TriggerCanaryFingerprint,
		Intent:            c.Intent,
		Subject:           c.Subject,
		Legs:              c.Legs,
	}
	return "rp_" + shortHash(projection, 12)
}

func riskPlanID(plan rpc.RiskPlanResult) string {
	projection := struct {
		Schema            string                       `json:"schema"`
		PolicyFingerprint rpc.Fingerprint              `json:"policy_fingerprint"`
		Trigger           *rpc.Fingerprint             `json:"trigger"`
		Source            rpc.CanarySourceFingerprints `json:"source"`
		Candidates        []string                     `json:"candidates"`
	}{
		Schema:            rpc.RiskPlanSchemaVersion,
		PolicyFingerprint: plan.PolicyFingerprint,
		Trigger:           plan.TriggerCanaryFingerprint,
		Source:            plan.SourceFingerprints,
	}
	for _, candidate := range plan.Candidates {
		projection.Candidates = append(projection.Candidates, candidate.ID)
	}
	return "plan_" + shortHash(projection, 16)
}

func shortHash(v any, n int) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:n]
}

func renderRiskPlanText(env *Env, plan *rpc.RiskPlanResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Risk Plan  %s\n", env.statusBadge(statusConcern{Text: strings.ToUpper(plan.ResponseMode), Level: statusConcernWarn}))
	statusRow(env, out, "Plan", plan.PlanID)
	statusRow(env, out, "Policy", plan.PolicyProfile+" "+plan.PolicyVersion)
	if plan.TriggerCanaryFingerprint != nil {
		statusRow(env, out, "Canary", plan.TriggerCanaryFingerprint.Version+" "+plan.TriggerCanaryFingerprint.Key)
	}
	statusRow(env, out, "Authority", plan.ExecutionAuthority+"; next "+plan.NextRequiredStep)
	if plan.RiskBefore.MarginCushionPct != nil {
		statusRow(env, out, "Cushion", fmt.Sprintf("%.1f%%", *plan.RiskBefore.MarginCushionPct))
	}
	if plan.RiskBefore.DailyPnLPctNLV != nil {
		statusRow(env, out, "Daily P&L", fmt.Sprintf("%.1f%% NLV", *plan.RiskBefore.DailyPnLPctNLV))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Candidates:")
	if len(plan.Candidates) == 0 {
		fmt.Fprintln(out, "  No reduce-only candidates are currently previewable.")
	} else {
		for _, c := range plan.Candidates {
			fmt.Fprintf(out, "  %d. %s  %s  %s\n", c.Rank, c.ID, c.Status, c.Reason)
			for _, leg := range c.Legs {
				fmt.Fprintf(out, "     - %s %d %s %s %s %.4g", leg.Action, leg.Quantity, leg.Contract.Symbol, leg.Contract.Expiry, leg.Contract.Right, leg.Contract.Strike)
				if leg.EstimatedLimitPrice != nil {
					fmt.Fprintf(out, " @ %.4f", *leg.EstimatedLimitPrice)
				}
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "       preview: %s\n", c.PreviewCommand)
		}
	}
	if len(plan.Warnings) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Warnings:")
		for _, warning := range plan.Warnings {
			fmt.Fprintf(out, "  - %s\n", warning)
		}
	}
	fmt.Fprintln(out)
}

func exposureForSymbol(pos rpc.PositionsResult, symbol string) *rpc.UnderlyingExposure {
	if pos.Portfolio == nil {
		return nil
	}
	for i := range pos.Portfolio.ExposureBase {
		if strings.EqualFold(pos.Portfolio.ExposureBase[i].Underlying, symbol) {
			return &pos.Portfolio.ExposureBase[i]
		}
	}
	return nil
}

func stockForSymbol(pos rpc.PositionsResult, symbol string) *rpc.PositionView {
	for i := range pos.Stocks {
		if strings.EqualFold(pos.Stocks[i].Symbol, symbol) {
			return &pos.Stocks[i]
		}
	}
	return nil
}

func dte(expiry string, now time.Time) *int {
	expiry = strings.TrimSpace(expiry)
	if expiry == "" {
		return nil
	}
	t, err := time.Parse("20060102", expiry)
	if err != nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	y, m, d := now.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	days := int(t.Sub(start).Hours() / 24)
	return &days
}

func optionMoneyness(opt rpc.PositionView) *float64 {
	if opt.Underlying == nil || opt.Strike <= 0 {
		return nil
	}
	v := *opt.Underlying / opt.Strike
	return &v
}

func positionEffect(held float64, qty int) string {
	if float64(qty) >= math.Abs(held) {
		return rpc.OrderPositionEffectClose
	}
	return rpc.OrderPositionEffectReduce
}

func absPositionMarketValue(p rpc.PositionView) float64 {
	return math.Abs(p.MarketValue)
}

func absPositionMarketValueBase(p rpc.PositionView) float64 {
	if p.MarketValueBase != nil {
		return math.Abs(*p.MarketValueBase)
	}
	fx := 1.0
	if p.FXRate != nil {
		fx = *p.FXRate
	}
	return math.Abs(p.MarketValue) * fx
}

func optionDollarDeltaBase(opt rpc.PositionView, qty int) *float64 {
	if opt.Delta == nil {
		return nil
	}
	underlying := opt.Underlying
	if underlying == nil || *underlying <= 0 {
		return nil
	}
	fx := 1.0
	if opt.FXRate != nil {
		fx = *opt.FXRate
	}
	v := *opt.Delta * float64(qty) * float64(max(opt.Multiplier, 1)) * *underlying * fx
	return &v
}

func positionRealizedPnLEstimate(p rpc.PositionView, qty int) *float64 {
	base := p.UnrealizedPnLBase
	if base == nil || p.Quantity == 0 {
		return nil
	}
	v := *base * float64(qty) / math.Abs(p.Quantity)
	return &v
}

func subtractPct(v *float64, reduction float64) *float64 {
	if v == nil {
		return nil
	}
	out := max(*v-reduction, 0)
	return &out
}

func pctPtr(numerator, denominator float64) *float64 {
	if denominator <= 0 || numerator == 0 {
		return nil
	}
	v := numerator / denominator * 100
	return &v
}

func contractMultiplier(c rpc.ContractParams) float64 {
	if c.SecType == "OPT" {
		return 100
	}
	return 1
}

func cmpExpiryBucket(a, b string, now time.Time) int {
	ad := dte(a, now)
	bd := dte(b, now)
	switch {
	case ad == nil && bd == nil:
		return 0
	case ad == nil:
		return 1
	case bd == nil:
		return -1
	default:
		return cmpFloat(float64(*ad), float64(*bd))
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpFloatDesc(a, b float64) int {
	return -cmpFloat(a, b)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func quantityLabel(q float64) string {
	if q == 0 {
		return ""
	}
	return fmt.Sprintf(" qty %.4g", q)
}
