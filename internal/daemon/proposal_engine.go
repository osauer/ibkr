package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

const (
	proposalEventFileVersion = 1
	proposalOrderSource      = "trade_proposals"
)

type proposalEngine struct {
	mu       sync.Mutex
	server   *Server
	store    *proposalStore
	cadence  time.Duration
	now      func() time.Time
	snapshot rpc.TradeProposalSnapshot
	ignored  map[string]struct{}
}

type proposalStore struct {
	currentPath string
	eventsPath  string
	mu          sync.Mutex
}

type proposalEvent struct {
	Version            int                                 `json:"version"`
	At                 time.Time                           `json:"at"`
	Type               string                              `json:"type"`
	Key                string                              `json:"key,omitempty"`
	Revision           string                              `json:"revision,omitempty"`
	Bucket             string                              `json:"bucket,omitempty"`
	AccountID          string                              `json:"account_id,omitempty"`
	PolicyID           string                              `json:"policy_id,omitempty"`
	PolicyVersion      int                                 `json:"policy_version,omitempty"`
	PolicyFingerprint  rpc.Fingerprint                     `json:"policy_fingerprint,omitzero"`
	PreviewTokenID     string                              `json:"preview_token_id,omitempty"`
	OrderRef           string                              `json:"order_ref,omitempty"`
	Message            string                              `json:"message,omitempty"`
	Reason             string                              `json:"reason,omitempty"`
	SourceFingerprints rpc.TradeProposalSourceFingerprints `json:"source_fingerprints,omitzero"`
}

func (s *Server) installProposalEngine() {
	current, err := defaultTradingStatePath("trade-proposals-current.json")
	if err != nil {
		s.warnf("trade proposals: resolve current path: %v", err)
		return
	}
	events, err := defaultTradingStatePath("trade-proposals.jsonl")
	if err != nil {
		s.warnf("trade proposals: resolve events path: %v", err)
		return
	}
	e := &proposalEngine{
		server:  s,
		store:   &proposalStore{currentPath: current, eventsPath: events},
		cadence: s.cfg.AutoTrade.WithDefaults().ProposalCadenceDuration(),
		now:     s.now,
		ignored: map[string]struct{}{},
	}
	if snap, err := e.store.LoadCurrent(); err == nil && snap.Kind != "" {
		snap.LoadedFromState = true
		e.snapshot = snap
	}
	s.tradeProposals = e
}

func (e *proposalEngine) Run(ctx context.Context) {
	if e == nil {
		return
	}
	_, _ = e.Refresh(ctx, false)
	t := time.NewTicker(e.cadence)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = e.Refresh(ctx, false)
		}
	}
}

func (e *proposalEngine) Snapshot(show bool) rpc.TradeProposalSnapshot {
	if e == nil {
		return emptyProposalSnapshot(time.Now().UTC())
	}
	e.mu.Lock()
	snap := cloneProposalSnapshot(e.snapshot)
	e.mu.Unlock()
	if snap.Kind == "" {
		snap = emptyProposalSnapshot(e.clock())
	}
	if show {
		e.appendShownEvents(snap)
	}
	return snap
}

func (e *proposalEngine) Refresh(ctx context.Context, show bool) (rpc.TradeProposalSnapshot, error) {
	now := e.clock()
	cfg := e.server.cfg.AutoTrade.WithDefaults()
	autoStatus := e.server.autoTradeStatus()
	if !cfg.ProposalsEnabledResolved() {
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = autoStatus.Policy
		snap.Blockers = []rpc.TradingBlocker{{Code: "proposals_disabled", Message: "manual protection proposals are disabled by config"}}
		e.installSnapshot(snap, show)
		return snap, nil
	}
	policy, policyStatus := e.server.protectionPolicies.Active()
	if policyStatus.Status == rpc.ProtectionPolicyStatusDrift || policyStatus.Status == rpc.ProtectionPolicyStatusError {
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.Blockers = append([]rpc.TradingBlocker(nil), policyStatus.Blockers...)
		e.installSnapshot(snap, show)
		e.appendEvent(proposalEvent{At: now, Type: "policy-" + policyStatus.Status, PolicyID: policyStatus.PolicyID, PolicyVersion: policyStatus.PolicyVersion, PolicyFingerprint: policyStatus.Fingerprint, Message: policyStatus.Message})
		return snap, nil
	}
	acct, err := e.server.handleAccountSummary(ctx)
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "account_unavailable", Message: err.Error()}}
		if snap, ok := e.preserveSnapshotOnRefreshFailure(autoStatus, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.Blockers = blockers
		e.installSnapshot(snap, show)
		return snap, err
	}
	pos, err := e.server.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "positions_unavailable", Message: err.Error()}}
		if snap, ok := e.preserveSnapshotOnRefreshFailure(autoStatus, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.AccountID = acct.AccountID
		snap.Blockers = blockers
		e.installSnapshot(snap, show)
		return snap, err
	}
	accountFP := rpc.BuildAccountFingerprint(acct)
	positionsFP := rpc.BuildPositionsFingerprint(pos, acct.NetLiquidation)
	sources := rpc.TradeProposalSourceFingerprints{Account: &accountFP, Positions: &positionsFP}
	if fp, ok := e.regimeFingerprint(ctx); ok {
		sources.Regime = &fp
	}
	marketEvents := e.marketEventsSnapshot(ctx, pos)
	if marketEvents != nil {
		fp := marketEvents.Fingerprint
		if fp.Key == "" {
			fp = rpc.BuildMarketEventsFingerprint(marketEvents)
		}
		sources.MarketEvents = &fp
	}
	proposals := e.generate(policy, policyStatus, pos, sources, marketEvents, now)
	slices.SortStableFunc(proposals, func(a, b rpc.TradeProposal) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	revision := proposalRevision(policyStatus.Fingerprint, sources, proposals)
	for i := range proposals {
		proposals[i].Rank = i + 1
		proposals[i].Revision = revision
	}
	snap := rpc.TradeProposalSnapshot{
		Kind:               rpc.TradeProposalSnapshotKind,
		SchemaVersion:      rpc.TradeProposalSnapshotSchemaVersion,
		AsOf:               now,
		Revision:           revision,
		AccountID:          acct.AccountID,
		PolicyID:           policy.PolicyID,
		PolicyVersion:      policy.PolicyVersion,
		PolicyFingerprint:  policyStatus.Fingerprint,
		PolicyStatus:       policyStatus,
		AutoTrade:          autoStatus,
		Trading:            autoStatus.Trading,
		SourceFingerprints: sources,
		MarketEvents:       marketEvents,
		Proposals:          proposals,
		Counts:             proposalCounts(proposals),
	}
	e.installSnapshot(snap, show)
	return snap, nil
}

func (e *proposalEngine) generate(policy protectionPolicy, status rpc.ProtectionPolicyStatus, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, marketEvents *rpc.MarketEventsResult, now time.Time) []rpc.TradeProposal {
	var out []rpc.TradeProposal
	if policy.Buckets.ThetaHygiene.Enabled {
		for _, row := range pos.Options {
			if p, ok := thetaProposal(policy, status, row, sources, now); ok {
				applyMarketEventFlagsToProposal(&p, marketEvents)
				if !e.isIgnored(p.Key) {
					out = append(out, p)
				}
			}
		}
	}
	if policy.Buckets.RiskReduction.Enabled {
		for _, group := range pos.ByUnderlying {
			if p, ok := riskReductionProposal(policy, status, group, sources, now); ok {
				applyMarketEventFlagsToProposal(&p, marketEvents)
				if !e.isIgnored(p.Key) {
					out = append(out, p)
				}
			}
		}
	}
	if policy.Buckets.TrailingStop.Enabled {
		stockEnabled := true
		if e != nil && e.server != nil {
			stockEnabled = e.server.stockProtectionEnabled()
		}
		if policy.Buckets.TrailingStop.StockETF.Enabled {
			for _, row := range pos.Stocks {
				if p, ok := trailingStopStockProposal(policy, status, row, sources, now, stockEnabled); ok {
					applyMarketEventFlagsToProposal(&p, marketEvents)
					if !e.isIgnored(p.Key) {
						out = append(out, p)
					}
				}
			}
		}
		if policy.Buckets.TrailingStop.Options.Enabled {
			multiLegBySymbol := multiLegOptionSymbols(pos.Options)
			for _, row := range pos.Options {
				if p, ok := trailingStopOptionProposal(policy, status, row, sources, now, multiLegBySymbol[strings.ToUpper(strings.TrimSpace(row.Symbol))]); ok {
					applyMarketEventFlagsToProposal(&p, marketEvents)
					if !e.isIgnored(p.Key) {
						out = append(out, p)
					}
				}
			}
		}
	}
	return out
}

func (e *proposalEngine) marketEventsSnapshot(ctx context.Context, pos *rpc.PositionsResult) *rpc.MarketEventsResult {
	if e == nil || e.server == nil {
		return nil
	}
	symbols := marketEventSymbolsFromPositions(pos)
	if len(symbols) == 0 {
		return nil
	}
	eventsCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	res := e.server.marketEventsForSymbols(eventsCtx, symbols)
	return &res
}

func (e *proposalEngine) regimeFingerprint(ctx context.Context) (rpc.Fingerprint, bool) {
	if e == nil || e.server == nil {
		return rpc.Fingerprint{}, false
	}
	regimeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	regime, err := e.server.handleRegimeSnapshot(regimeCtx, &rpc.Request{})
	if err != nil || regime == nil {
		return rpc.Fingerprint{}, false
	}
	fp := regime.Fingerprint
	if fp.Key == "" {
		fp = rpc.BuildRegimeFingerprint(regime)
	}
	return fp, fp.Key != ""
}

func thetaProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, row rpc.PositionView, sources rpc.TradeProposalSourceFingerprints, now time.Time) (rpc.TradeProposal, bool) {
	if !strings.EqualFold(row.SecType, "OPTION") && !strings.EqualFold(row.SecType, "OPT") || row.Quantity == 0 || row.Theta == nil {
		return rpc.TradeProposal{}, false
	}
	dte, ok := optionDTE(row.Expiry, now)
	if !ok || dte > policy.Buckets.ThetaHygiene.MaxDTE {
		return rpc.TradeProposal{}, false
	}
	thetaPerDay := math.Abs(*row.Theta * row.Quantity * float64(max(row.Multiplier, 1)))
	if thetaPerDay < policy.Buckets.ThetaHygiene.MinAbsThetaPerDay {
		return rpc.TradeProposal{}, false
	}
	qty := int(math.Ceil(math.Abs(row.Quantity)))
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketThetaHygiene, row, action, qty, rpc.OrderPositionEffectClose, fmt.Sprintf("option expires in %d DTE with %.2f/day theta exposure", dte, thetaPerDay))
	p.ThetaPerDay = thetaPerDay
	p.Score = thetaPerDay + float64(max(policy.Buckets.ThetaHygiene.MaxDTE-dte, 0))
	p.Details = []string{fmt.Sprintf("dte=%d", dte)}
	if row.SpreadPct != nil && *row.SpreadPct > policy.Buckets.ThetaHygiene.MaxSpreadPctOfMid {
		p.State = rpc.TradeProposalStateBlocked
		p.Blockers = []rpc.TradingBlocker{{Code: "wide_spread", Message: fmt.Sprintf("option spread %.1f%% exceeds policy max %.1f%% of mid", *row.SpreadPct, policy.Buckets.ThetaHygiene.MaxSpreadPctOfMid)}}
	}
	return p, true
}

func riskReductionProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, group rpc.PositionGroup, sources rpc.TradeProposalSourceFingerprints, now time.Time) (rpc.TradeProposal, bool) {
	if group.GroupMarketValuePctNLV == nil || math.Abs(*group.GroupMarketValuePctNLV) <= policy.Buckets.RiskReduction.SingleNameTargetPctNLV {
		return rpc.TradeProposal{}, false
	}
	var row rpc.PositionView
	if group.Stock != nil && group.Stock.Quantity != 0 {
		row = *group.Stock
	} else {
		for _, opt := range group.Options {
			if opt.Quantity != 0 {
				row = opt
				break
			}
		}
	}
	if row.Symbol == "" || row.Quantity == 0 {
		return rpc.TradeProposal{}, false
	}
	if !proposalSupportedSecType(row.SecType) {
		return rpc.TradeProposal{}, false
	}
	pct := math.Abs(*group.GroupMarketValuePctNLV)
	excessPct := pct - policy.Buckets.RiskReduction.SingleNameTargetPctNLV
	excessNotional := math.Abs(groupMarketValueOrderValue(group)) * (excessPct / pct)
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	maxQty := int(math.Ceil(math.Abs(row.Quantity)))
	qty := maxQty
	mark := math.Abs(row.Mark)
	if mark <= 0 {
		mark = math.Abs(row.ValuationMark)
	}
	if mark > 0 {
		mult := float64(max(row.Multiplier, 1))
		qty = int(math.Ceil(excessNotional / (mark * mult)))
		maxByNotional := int(math.Max(1, math.Floor(policy.Buckets.RiskReduction.MaxOrderNotional/(mark*mult))))
		qty = min(qty, maxByNotional)
	}
	qty = max(1, min(qty, maxQty))
	effect := rpc.OrderPositionEffectReduce
	if qty == maxQty {
		effect = rpc.OrderPositionEffectClose
	}
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketRiskReduction, row, action, qty, effect, fmt.Sprintf("%s is %.1f%% of NLV, above %.1f%% target", group.Underlying, pct, policy.Buckets.RiskReduction.SingleNameTargetPctNLV))
	p.MarketValuePctNLV = cloneFloat64Ptr(group.GroupMarketValuePctNLV)
	p.RiskExcessNotional = excessNotional
	p.RiskExcessCurrency = p.Contract.Currency
	p.Score = pct
	return p, true
}

func trailingStopStockProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, row rpc.PositionView, sources rpc.TradeProposalSourceFingerprints, now time.Time, stockProtectionEnabled bool) (rpc.TradeProposal, bool) {
	secType := strings.ToUpper(strings.TrimSpace(row.SecType))
	if secType != rpc.SecTypeStock && secType != "STK" && secType != "ETF" || row.Quantity == 0 {
		return rpc.TradeProposal{}, false
	}
	cfg := policy.Buckets.TrailingStop.StockETF
	qty := int(math.Ceil(math.Abs(row.Quantity)))
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	reference := trailingStopReferencePrice(row, action)
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketTrailingStop, row, action, qty, rpc.OrderPositionEffectClose, fmt.Sprintf("broker-side trailing stop at %.1f%% below/above the instrument price", cfg.DefaultPct))
	applyTrailToProposal(&p, cfg.OrderType, cfg.DefaultPct, reference, action, cfg.LimitOffsetAbs)
	p.Score = math.Abs(row.MarketValue)
	p.Details = append(p.Details, fmt.Sprintf("trail=%.1f%%", cfg.DefaultPct))
	if !stockProtectionEnabled {
		proposalBlock(&p, "stock_protection_disabled", "stock/ETF protection is disabled in platform settings")
	}
	if reference <= 0 {
		proposalBlock(&p, "missing_reference_price", "stock/ETF trailing stop requires bid/ask or a positive portfolio mark")
	}
	if row.SpreadPct != nil && *row.SpreadPct > cfg.MaxSpreadPctOfMid {
		proposalBlock(&p, "wide_spread", fmt.Sprintf("stock/ETF spread %.1f%% exceeds policy max %.1f%% of mid", *row.SpreadPct, cfg.MaxSpreadPctOfMid))
	}
	return p, true
}

func trailingStopOptionProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, row rpc.PositionView, sources rpc.TradeProposalSourceFingerprints, now time.Time, multiLeg bool) (rpc.TradeProposal, bool) {
	if !strings.EqualFold(row.SecType, "OPTION") && !strings.EqualFold(row.SecType, "OPT") || row.Quantity == 0 {
		return rpc.TradeProposal{}, false
	}
	cfg := policy.Buckets.TrailingStop.Options
	qty := int(math.Ceil(math.Abs(row.Quantity)))
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	reference, spreadAbs, ok := optionTrailReference(row, action)
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketTrailingStop, row, action, qty, rpc.OrderPositionEffectClose, fmt.Sprintf("broker-side option premium trailing stop at %.1f%%", cfg.DefaultPct))
	applyTrailToProposal(&p, cfg.OrderType, cfg.DefaultPct, reference, action, cfg.LimitOffsetAbs)
	p.Score = math.Abs(row.MarketValue)
	p.Details = append(p.Details, fmt.Sprintf("premium trail=%.1f%%", cfg.DefaultPct))
	if row.Quantity < 0 && !cfg.AllowShortProfitTrail {
		proposalBlock(&p, "short_option_trail_disabled", "short-option trailing stops require explicit buy-to-close profit-trail policy")
	}
	if multiLeg {
		proposalBlock(&p, "multi_leg_option_trail_unsupported", "broker-side option trails are supported for single-leg option positions only in V1")
	}
	if !ok {
		proposalBlock(&p, "missing_option_bid_ask", "option trailing stop requires live two-sided option bid/ask")
	}
	if row.Stale {
		proposalBlock(&p, "stale_quote", "option trailing stop requires a fresh live option quote")
	}
	if row.SessionContext == nil || !row.SessionContext.IsOpen {
		proposalBlock(&p, "option_rth_closed", "option trailing stop proposals require the regular option session to be open")
	}
	if row.SpreadPct != nil && *row.SpreadPct > cfg.MaxSpreadPctOfMid {
		proposalBlock(&p, "wide_spread", fmt.Sprintf("option spread %.1f%% exceeds policy max %.1f%% of mid", *row.SpreadPct, cfg.MaxSpreadPctOfMid))
	}
	trailAbs := reference * cfg.DefaultPct / 100
	if reference > 0 && trailAbs < cfg.MinTrailAbs {
		proposalBlock(&p, "trail_too_small", fmt.Sprintf("option trail %.4f is below policy minimum %.4f", trailAbs, cfg.MinTrailAbs))
	}
	if reference > 0 && spreadAbs > 0 && trailAbs < cfg.SpreadMultiple*spreadAbs {
		proposalBlock(&p, "trail_inside_spread", fmt.Sprintf("option trail %.4f is below %.1fx spread %.4f", trailAbs, cfg.SpreadMultiple, spreadAbs))
	}
	return p, true
}

func multiLegOptionSymbols(rows []rpc.PositionView) map[string]bool {
	counts := make(map[string]int)
	for _, row := range rows {
		if row.Quantity == 0 {
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
		if symbol == "" {
			continue
		}
		counts[symbol]++
	}
	out := make(map[string]bool)
	for symbol, count := range counts {
		if count > 1 {
			out[symbol] = true
		}
	}
	return out
}

func applyTrailToProposal(p *rpc.TradeProposal, orderType string, pct, reference float64, action string, limitOffset float64) {
	if p == nil {
		return
	}
	p.OrderType = strings.ToUpper(strings.TrimSpace(orderType))
	if p.OrderType == "" {
		p.OrderType = rpc.OrderTypeTRAIL
	}
	trail := &rpc.OrderTrailSpec{
		Basis:      rpc.OrderTrailBasisInstrumentPrice,
		OffsetType: rpc.OrderTrailOffsetPercent,
	}
	if reference > 0 {
		amount := ceilPriceToTick(reference*pct/100, trailMinimumTick(p.Contract, reference))
		trail.OffsetType = rpc.OrderTrailOffsetAmount
		trail.TrailingAmount = cloneFloat64Ptr(&amount)
		trail.InitialStopPrice = trailingStopInitialPriceForContract(action, reference, amount, p.Contract)
	} else {
		trail.TrailingPercent = cloneFloat64Ptr(&pct)
	}
	if strings.EqualFold(p.OrderType, rpc.OrderTypeTRAILLIMIT) && limitOffset > 0 {
		trail.LimitOffset = cloneFloat64Ptr(&limitOffset)
	}
	p.Trail = trail
}

func trailingStopReferencePrice(row rpc.PositionView, action string) float64 {
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		if row.Ask != nil && *row.Ask > 0 {
			return *row.Ask
		}
	} else if row.Bid != nil && *row.Bid > 0 {
		return *row.Bid
	}
	if row.QuotePrice != nil && *row.QuotePrice > 0 {
		return *row.QuotePrice
	}
	if row.Mark > 0 {
		return row.Mark
	}
	if row.ValuationMark > 0 {
		return row.ValuationMark
	}
	return 0
}

func optionTrailReference(row rpc.PositionView, action string) (reference float64, spreadAbs float64, ok bool) {
	if row.OptionBid == nil || row.OptionAsk == nil || *row.OptionBid <= 0 || *row.OptionAsk <= *row.OptionBid {
		return 0, 0, false
	}
	spreadAbs = *row.OptionAsk - *row.OptionBid
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		return *row.OptionAsk, spreadAbs, true
	}
	return *row.OptionBid, spreadAbs, true
}

func proposalBlock(p *rpc.TradeProposal, code, message string) {
	if p == nil {
		return
	}
	p.State = rpc.TradeProposalStateBlocked
	p.Blockers = appendTradingBlockerOnce(p.Blockers, rpc.TradingBlocker{Code: code, Message: message})
}

func baseProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, bucket string, row rpc.PositionView, action string, qty int, effect string, reason string) rpc.TradeProposal {
	secType := "STK"
	if strings.EqualFold(row.SecType, "OPTION") || strings.EqualFold(row.SecType, "OPT") {
		secType = "OPT"
	} else if strings.EqualFold(row.SecType, "ETF") {
		secType = "ETF"
	}
	contract := proposalContractFromPosition(row, secType)
	p := rpc.TradeProposal{Key: proposalKey(bucket, contract, action), State: rpc.TradeProposalStateGenerated, Bucket: bucket, Symbol: contract.Symbol, SecType: secType, Action: action, Quantity: qty, MaxQuantity: int(math.Ceil(math.Abs(row.Quantity))), PositionQuantity: row.Quantity, PositionEffect: effect, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: contract, Reason: reason, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, SourceFingerprints: sources, CreatedAt: now}
	if row.Mark > 0 {
		v := row.Mark
		p.LimitPrice = &v
		p.Notional = math.Abs(row.Mark) * float64(qty) * float64(max(row.Multiplier, 1))
	}
	return p
}

func proposalContractFromPosition(row rpc.PositionView, secType string) rpc.ContractParams {
	contract := rpc.ContractParams{
		ConID:        row.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(row.Symbol)),
		SecType:      secType,
		Exchange:     nonEmptyString(row.Exchange, "SMART"),
		Currency:     nonEmptyString(row.Currency, "USD"),
		LocalSymbol:  row.LocalSymbol,
		TradingClass: row.TradingClass,
		Expiry:       row.Expiry,
		Strike:       row.Strike,
		Right:        row.Right,
		Multiplier:   row.Multiplier,
	}
	if (secType == "STK" || secType == "ETF") && strings.EqualFold(strings.TrimSpace(row.Exchange), "IBIS") {
		contract.Market = "de"
		contract.Exchange = "SMART"
		contract.PrimaryExch = "IBIS"
		contract.Currency = nonEmptyString(row.Currency, "EUR")
	}
	return contract
}

func applyMarketEventFlagsToProposal(prop *rpc.TradeProposal, events *rpc.MarketEventsResult) {
	if prop == nil || events == nil {
		return
	}
	flags := proposalMarketEventFlags(*prop, events)
	if len(flags) == 0 {
		return
	}
	prop.MarketFlags = flags
	for _, flag := range flags {
		switch {
		case flag.ID == rpc.MarketEventHaltRegulatoryOrNews && flag.Status == rpc.MarketEventStatusActive:
			marketEventBlockProposal(prop, flag, "active halt")
		case flag.ID == rpc.MarketEventLULDRecent && flag.Status == rpc.MarketEventStatusActive:
			marketEventBlockProposal(prop, flag, "active LULD pause")
		}
	}
}

func proposalMarketEventFlags(prop rpc.TradeProposal, events *rpc.MarketEventsResult) []rpc.MarketEventFlag {
	if events == nil || events.BySymbol == nil {
		return nil
	}
	symbol := strings.ToUpper(strings.TrimSpace(prop.Symbol))
	if symbol == "" {
		return nil
	}
	out := []rpc.MarketEventFlag{}
	for _, flag := range events.BySymbol[symbol] {
		if !proposalMarketEventFlagApplies(prop, flag) {
			continue
		}
		out = append(out, flag)
	}
	slices.SortFunc(out, func(a, b rpc.MarketEventFlag) int {
		if c := cmpMarketEventSeverity(a.Severity, b.Severity); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func proposalMarketEventFlagApplies(prop rpc.TradeProposal, flag rpc.MarketEventFlag) bool {
	switch flag.ID {
	case rpc.MarketEventHaltRegulatoryOrNews, rpc.MarketEventLULDRecent:
		return flag.Status == rpc.MarketEventStatusActive || flag.Status == rpc.MarketEventStatusRecent
	case rpc.MarketEventRegSHOThreshold:
		return proposalCloseReduceEffect(prop.PositionEffect)
	case rpc.MarketEventBorrowInventoryTight, rpc.MarketEventBorrowFeeExtreme:
		return prop.PositionQuantity < 0 &&
			strings.EqualFold(prop.Action, rpc.OrderActionBuy) &&
			proposalCloseReduceEffect(prop.PositionEffect)
	default:
		return flag.Status == rpc.MarketEventStatusActive || flag.Status == rpc.MarketEventStatusRecent
	}
}

func marketEventBlockProposal(prop *rpc.TradeProposal, flag rpc.MarketEventFlag, reason string) {
	prop.State = rpc.TradeProposalStateBlocked
	code := "market_event_" + flag.ID
	message := fmt.Sprintf("%s is %s for %s", flag.Label, reason, flag.Symbol)
	if flag.Source != "" {
		message += " (" + flag.Source + ")"
	}
	prop.Blockers = appendTradingBlockerOnce(prop.Blockers, rpc.TradingBlocker{
		Code:    code,
		Message: message + "; refresh proposals after the market event clears.",
		Action:  "Wait for fresh tradability context before previewing or submitting this protection proposal.",
	})
}

func (e *proposalEngine) Preview(ctx context.Context, p rpc.TradeProposalPreviewParams) (rpc.TradeProposalPreviewResult, error) {
	prop, blockers, err := e.previewProposal(ctx, p)
	now := e.clock()
	if len(blockers) > 0 || err != nil {
		e.appendBlocked(prop, p.Key, p.Revision, blockers, err)
		return rpc.TradeProposalPreviewResult{Proposal: prop, Blockers: blockers, AsOf: now}, err
	}
	preview, err := e.server.previewOrder(ctx, proposalOrderPreviewParams(prop, selectedProposalQty(prop, p.Quantity), p.TimeoutMs))
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error()}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, err)
		return rpc.TradeProposalPreviewResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	e.appendEvent(proposalEventForProposal("previewed", prop, now, preview.PreviewTokenID, preview.Draft.OrderRef, "proposal previewed"))
	if blockers := proposalPreviewSafetyBlockers(prop, preview); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalPreviewResult{Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, Preview: sanitizeProposalPreview(preview), Blockers: blockers, AsOf: now}, nil
	}
	if !preview.SubmitEligible {
		blockers := previewNotSubmitEligibleBlockers()
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalPreviewResult{Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, SubmitEligible: false, Preview: sanitizeProposalPreview(preview), Blockers: blockers, AsOf: now}, nil
	}
	return rpc.TradeProposalPreviewResult{Accepted: true, Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, SubmitEligible: preview.SubmitEligible, Preview: sanitizeProposalPreview(preview), AsOf: now}, nil
}

func (e *proposalEngine) previewProposal(ctx context.Context, p rpc.TradeProposalPreviewParams) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
	if p.FastPath {
		if prop, blockers, ok := e.fastPathPreviewProposal(p.Key, p.Revision); ok {
			return prop, blockers, nil
		}
	}
	return e.revalidatedProposal(ctx, p.Key, p.Revision)
}

func (e *proposalEngine) fastPathPreviewProposal(key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, bool) {
	key, revision = strings.TrimSpace(key), strings.TrimSpace(revision)
	if key == "" || revision == "" {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "bad_request", Message: "proposal key and revision are required"}}, true
	}
	e.mu.Lock()
	snap := cloneProposalSnapshot(e.snapshot)
	e.mu.Unlock()
	if snap.Kind == "" || snap.Revision == "" {
		return rpc.TradeProposal{}, nil, false
	}
	if len(snap.Blockers) > 0 && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, true
	}
	if snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusDrift || snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusError {
		return rpc.TradeProposal{}, snap.PolicyStatus.Blockers, true
	}
	if len(snap.AutoTrade.Blockers) > 0 {
		return rpc.TradeProposal{}, snap.AutoTrade.Blockers, true
	}
	if snap.Revision != revision {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "proposal revision is stale; refresh proposals before preview or submit"}}, true
	}
	for _, prop := range snap.Proposals {
		if prop.Key != key {
			continue
		}
		if prop.Bucket != rpc.TradeProposalBucketTrailingStop {
			return rpc.TradeProposal{}, nil, false
		}
		if len(snap.Blockers) > 0 {
			return prop, mergeTradingBlockers(snap.Blockers, prop.Blockers), true
		}
		return prop, prop.Blockers, true
	}
	return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "proposal key is not present in the current snapshot"}}, true
}

func (e *proposalEngine) Submit(ctx context.Context, p rpc.TradeProposalSubmitParams) (rpc.TradeProposalSubmitResult, error) {
	prop, blockers, err := e.revalidatedProposal(ctx, p.Key, p.Revision)
	now := e.clock()
	if len(blockers) > 0 || err != nil {
		e.appendBlocked(prop, p.Key, p.Revision, blockers, err)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, err
	}
	cfg := e.server.cfg.AutoTrade.WithDefaults()
	if !cfg.FastPathEnabledResolved() || !p.FastPath {
		blockers := []rpc.TradingBlocker{{Code: "fast_path_disabled", Message: "proposal submit requires fast_path=true and [auto_trade].fast_path_enabled=true"}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	preview, err := e.server.previewOrder(ctx, proposalOrderPreviewParams(prop, selectedProposalQty(prop, p.Quantity), p.TimeoutMs))
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error()}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, err)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	e.appendEvent(proposalEventForProposal("previewed", prop, now, preview.PreviewTokenID, preview.Draft.OrderRef, "proposal fast-path previewed"))
	if blockers := proposalPreviewSafetyBlockers(prop, preview); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreview(preview), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	if !preview.SubmitEligible {
		blockers := previewNotSubmitEligibleBlockers()
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreview(preview), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	place, err := e.server.proposalPlaceOrder(ctx, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, TimeoutMs: p.TimeoutMs})
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "submit_failed", Message: err.Error()}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, err)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreview(preview), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	e.appendEvent(proposalEventForProposal("submitted", prop, now, preview.PreviewTokenID, place.OrderRef, "proposal submitted through preview-backed fast path"))
	if e.server.proposalOutcomes != nil {
		if err := e.server.proposalOutcomes.AppendMark(proposalOutcomeSubmitted(prop, preview, place, now)); err != nil {
			e.server.warnf("trade proposal outcomes: append submitted mark: %v", err)
		}
	}
	return rpc.TradeProposalSubmitResult{Accepted: place.Accepted, Proposal: prop, Preview: sanitizeProposalPreview(preview), Place: place, PreviewTokenID: preview.PreviewTokenID, OrderRef: place.OrderRef, Message: place.Message, AsOf: e.clock()}, nil
}

func previewNotSubmitEligibleBlockers() []rpc.TradingBlocker {
	return []rpc.TradingBlocker{{
		Code:    "preview_not_submit_eligible",
		Message: "broker WhatIf did not make this proposal submit-eligible",
		Action:  "Resolve broker WhatIf availability and preview again before submitting a broker-managed stop.",
	}}
}

func (e *proposalEngine) Ignore(p rpc.TradeProposalIgnoreParams) rpc.TradeProposalIgnoreResult {
	now := e.clock()
	key := strings.TrimSpace(p.Key)
	e.mu.Lock()
	e.ignored[key] = struct{}{}
	e.mu.Unlock()
	e.appendEvent(proposalEvent{At: now, Type: "ignored", Key: key, Revision: strings.TrimSpace(p.Revision), Reason: strings.TrimSpace(p.Reason), Message: "proposal ignored"})
	return rpc.TradeProposalIgnoreResult{Accepted: true, Key: key, Revision: strings.TrimSpace(p.Revision), Message: "proposal ignored", AsOf: now}
}

func (e *proposalEngine) revalidatedProposal(ctx context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
	key, revision = strings.TrimSpace(key), strings.TrimSpace(revision)
	if key == "" || revision == "" {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "bad_request", Message: "proposal key and revision are required"}}, nil
	}
	snap, err := e.Refresh(ctx, false)
	if err != nil && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, err
	}
	if len(snap.Blockers) > 0 && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, nil
	}
	if snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusDrift || snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusError {
		return rpc.TradeProposal{}, snap.PolicyStatus.Blockers, nil
	}
	if len(snap.AutoTrade.Blockers) > 0 {
		return rpc.TradeProposal{}, snap.AutoTrade.Blockers, nil
	}
	if snap.Revision != revision {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "proposal revision is stale; refresh proposals before preview or submit"}}, nil
	}
	for _, prop := range snap.Proposals {
		if prop.Key == key {
			if len(snap.Blockers) > 0 {
				return prop, mergeTradingBlockers(snap.Blockers, prop.Blockers), nil
			}
			return prop, prop.Blockers, nil
		}
	}
	return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "proposal key is not present in the current snapshot"}}, nil
}

func proposalOrderPreviewParams(prop rpc.TradeProposal, qty, timeoutMs int) rpc.OrderPreviewParams {
	orderType := strings.ToUpper(strings.TrimSpace(prop.OrderType))
	if orderType == "" {
		orderType = rpc.OrderTypeLMT
	}
	strategy := rpc.OrderStrategyPatientLimit
	if orderType == rpc.OrderTypeTRAIL || orderType == rpc.OrderTypeTRAILLIMIT {
		strategy = rpc.OrderStrategyBrokerTrail
	}
	trail := cloneTrailSpec(prop.Trail)
	return rpc.OrderPreviewParams{Action: prop.Action, Contract: prop.Contract, Quantity: qty, OrderType: orderType, Trail: trail, Strategy: strategy, TIF: rpc.OrderTIFDay, OutsideRTH: prop.OutsideRTH, TimeoutMs: timeoutMs, Source: proposalOrderSource}
}

func selectedProposalQty(prop rpc.TradeProposal, requested int) int {
	if requested <= 0 {
		return prop.Quantity
	}
	return max(1, min(requested, prop.MaxQuantity))
}

func proposalPreviewSafetyBlockers(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	var blockers []rpc.TradingBlocker
	add := func(code, message, action string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: code, Message: message, Action: action})
	}
	if preview == nil {
		add("proposal_preview_missing", "proposal preview result is unavailable", "Refresh and preview the proposal again before submit.")
		return blockers
	}
	if !proposalCloseReduceEffect(prop.PositionEffect) {
		add("proposal_effect_not_close_reduce", fmt.Sprintf("proposal effect %q is not close/reduce", prop.PositionEffect), "Refresh proposals so the daemon can rebuild a close/reduce-only recommendation.")
	}
	if !proposalCloseReduceEffect(preview.Position.Effect) {
		add("preview_effect_not_close_reduce", fmt.Sprintf("preview effect %q is not close/reduce", preview.Position.Effect), "Refresh positions and preview again; proposal submit cannot open, increase, or flip exposure.")
	}
	if !proposalSupportedSecType(prop.SecType) || !proposalSupportedSecType(preview.Draft.Contract.SecType) {
		add("unsupported_security_type", "protection proposals support single-leg STK/ETF/OPT orders only", "Use a manual workflow for unsupported instruments.")
	}
	if !proposalSupportedOrderType(preview.Draft.OrderType) {
		add("unsupported_order_type", fmt.Sprintf("proposal order type %q is not supported", preview.Draft.OrderType), "Refresh proposals and preview a supported close/reduce order.")
	}
	if preview.Draft.TIF != rpc.OrderTIFDay {
		add("unsupported_tif", fmt.Sprintf("proposal time-in-force %q is not DAY", preview.Draft.TIF), "Refresh proposals and preview a DAY order.")
	}
	if strings.EqualFold(preview.Draft.Contract.SecType, "OPT") && preview.Draft.OutsideRTH {
		add("option_outside_rth", "option protection proposals must not request outside_rth", "Refresh proposals and preview during the supported option session.")
	}
	if preview.Draft.Quantity <= 0 || preview.Draft.Quantity > prop.MaxQuantity {
		add("quantity_outside_position", fmt.Sprintf("proposal preview quantity %d exceeds close/reduce cap %d", preview.Draft.Quantity, prop.MaxQuantity), "Refresh positions and preview a quantity within the current position.")
	}
	if !strings.EqualFold(preview.Draft.Action, prop.Action) {
		add("action_drift", fmt.Sprintf("preview action %q does not match proposal action %q", preview.Draft.Action, prop.Action), "Refresh proposals and preview again.")
	}
	propOrderType := strings.ToUpper(strings.TrimSpace(prop.OrderType))
	if propOrderType == "" {
		propOrderType = rpc.OrderTypeLMT
	}
	if strings.ToUpper(strings.TrimSpace(preview.Draft.OrderType)) != propOrderType {
		add("order_type_drift", fmt.Sprintf("preview order type %q does not match proposal order type %q", preview.Draft.OrderType, prop.OrderType), "Refresh proposals and preview again.")
	}
	if isTrailOrderType(preview.Draft.OrderType) {
		switch {
		case prop.Trail == nil:
			add("proposal_trail_missing", "proposal is missing broker-side trail fields", "Refresh proposals and preview again.")
		case preview.Draft.Trail == nil:
			add("trail_missing", "proposal preview is missing broker-side trail fields", "Refresh proposals and preview again.")
		default:
			for _, blocker := range proposalTrailDriftBlockers(prop.Trail, preview.Draft.Trail) {
				add(blocker.Code, blocker.Message, blocker.Action)
			}
		}
	}
	if strings.TrimSpace(preview.Draft.Source) != proposalOrderSource {
		add("source_drift", "proposal preview source does not match the protection proposal engine", "Refresh proposals and preview again.")
	}
	return blockers
}

func proposalTrailDriftBlockers(proposal, preview *rpc.OrderTrailSpec) []rpc.TradingBlocker {
	var blockers []rpc.TradingBlocker
	add := func(code, message string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{
			Code:    code,
			Message: message,
			Action:  "Refresh proposals and preview again before submitting a broker-managed stop.",
		})
	}
	if !strings.EqualFold(strings.TrimSpace(proposal.OffsetType), strings.TrimSpace(preview.OffsetType)) {
		add("trail_offset_type_drift", fmt.Sprintf("preview trail offset type %q does not match proposal offset type %q", preview.OffsetType, proposal.OffsetType))
	}
	if !floatPtrEqual(proposal.TrailingPercent, preview.TrailingPercent) {
		add("trail_percent_drift", "preview trailing_percent does not match proposal trailing_percent")
	}
	if !floatPtrEqual(proposal.TrailingAmount, preview.TrailingAmount) {
		add("trail_amount_drift", "preview trailing_amount does not match proposal trailing_amount")
	}
	if !floatPtrEqual(proposal.LimitOffset, preview.LimitOffset) {
		add("trail_limit_offset_drift", "preview limit_offset does not match proposal limit_offset")
	}
	if !floatEqual(proposal.InitialStopPrice, preview.InitialStopPrice) {
		add("trail_initial_stop_drift", "preview initial_stop_price does not match proposal initial_stop_price")
	}
	return blockers
}

func floatPtrEqual(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return math.Abs(*a-*b) < 1e-9
	}
}

func floatEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func proposalSupportedOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeLMT, rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return true
	default:
		return false
	}
}

func isTrailOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return true
	default:
		return false
	}
}

func cloneTrailSpec(in *rpc.OrderTrailSpec) *rpc.OrderTrailSpec {
	if in == nil {
		return nil
	}
	out := *in
	out.TrailingPercent = cloneFloat64Ptr(in.TrailingPercent)
	out.TrailingAmount = cloneFloat64Ptr(in.TrailingAmount)
	out.LimitOffset = cloneFloat64Ptr(in.LimitOffset)
	return &out
}

func mergeTradingBlockers(first, second []rpc.TradingBlocker) []rpc.TradingBlocker {
	out := append([]rpc.TradingBlocker(nil), first...)
	for _, blocker := range second {
		out = appendTradingBlockerOnce(out, blocker)
	}
	return out
}

func proposalCloseReduceEffect(effect string) bool {
	switch effect {
	case rpc.OrderPositionEffectClose, rpc.OrderPositionEffectReduce:
		return true
	default:
		return false
	}
}

func sanitizeProposalPreview(in *rpc.OrderPreviewResult) *rpc.TradeProposalOrderPreview {
	if in == nil {
		return nil
	}
	return &rpc.TradeProposalOrderPreview{PreviewTokenID: in.PreviewTokenID, PreviewTokenScope: in.PreviewTokenScope, PreviewTokenExpiresAt: in.PreviewTokenExpiresAt, TokenMinted: in.TokenMinted, SubmitEligible: in.SubmitEligible, Mode: in.Mode, Account: in.Account, Endpoint: in.Endpoint, ClientID: in.ClientID, Draft: in.Draft, Quote: in.Quote, Position: in.Position, Notional: in.Notional, MaxNotional: in.MaxNotional, WhatIf: in.WhatIf, Warnings: append([]rpc.DataWarning(nil), in.Warnings...), AsOf: in.AsOf}
}

func (e *proposalEngine) installSnapshot(snap rpc.TradeProposalSnapshot, show bool) {
	e.replaceSnapshot(snap)
	for _, prop := range snap.Proposals {
		e.appendEvent(proposalEventForProposal("generated", prop, snap.AsOf, "", "", "proposal generated"))
		if e.server != nil && e.server.proposalOutcomes != nil {
			if err := e.server.proposalOutcomes.AppendMark(proposalOutcomeMarked(prop, snap.AsOf)); err != nil {
				e.server.warnf("trade proposal outcomes: append daily mark: %v", err)
			}
		}
	}
	if show {
		e.appendShownEvents(snap)
	}
}

func (e *proposalEngine) installPreservedSnapshot(snap rpc.TradeProposalSnapshot, show bool) {
	e.replaceSnapshot(snap)
	if show {
		e.appendShownEvents(snap)
	}
}

func (e *proposalEngine) replaceSnapshot(snap rpc.TradeProposalSnapshot) {
	e.mu.Lock()
	e.snapshot = cloneProposalSnapshot(snap)
	e.mu.Unlock()
	if e.store == nil {
		return
	}
	if err := e.store.SaveCurrent(snap); err != nil && e.server != nil {
		e.server.warnf("trade proposals: save current snapshot: %v", err)
	}
}

func (e *proposalEngine) preserveSnapshotOnRefreshFailure(autoStatus rpc.AutoTradeStatus, policyStatus rpc.ProtectionPolicyStatus, blockers []rpc.TradingBlocker, show bool) (rpc.TradeProposalSnapshot, bool) {
	e.mu.Lock()
	snap := cloneProposalSnapshot(e.snapshot)
	e.mu.Unlock()
	if !proposalSnapshotUsable(snap) || !sameProposalPolicy(snap, policyStatus) {
		return rpc.TradeProposalSnapshot{}, false
	}
	snap.AutoTrade = autoStatus
	snap.PolicyStatus = policyStatus
	snap.Trading = autoStatus.Trading
	merged := append([]rpc.TradingBlocker(nil), blockers...)
	for _, blocker := range snap.Blockers {
		merged = appendTradingBlockerOnce(merged, blocker)
	}
	snap.Blockers = merged
	e.installPreservedSnapshot(snap, show)
	return snap, true
}

func proposalSnapshotUsable(snap rpc.TradeProposalSnapshot) bool {
	return snap.Kind == rpc.TradeProposalSnapshotKind && snap.Revision != "" && snap.Revision != "empty" && len(snap.Proposals) > 0
}

func sameProposalPolicy(snap rpc.TradeProposalSnapshot, status rpc.ProtectionPolicyStatus) bool {
	if snap.PolicyID != "" && status.PolicyID != "" && snap.PolicyID != status.PolicyID {
		return false
	}
	if snap.PolicyVersion != 0 && status.PolicyVersion != 0 && snap.PolicyVersion != status.PolicyVersion {
		return false
	}
	if snap.PolicyFingerprint.Key != "" && status.Fingerprint.Key != "" && snap.PolicyFingerprint.Key != status.Fingerprint.Key {
		return false
	}
	return true
}

func (e *proposalEngine) appendShownEvents(snap rpc.TradeProposalSnapshot) {
	for _, prop := range snap.Proposals {
		e.appendEvent(proposalEventForProposal("shown", prop, e.clock(), "", "", "proposal shown"))
	}
}

func (e *proposalEngine) appendBlocked(prop rpc.TradeProposal, key, revision string, blockers []rpc.TradingBlocker, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if len(blockers) > 0 {
		msg = blockers[0].Message
	}
	ev := proposalEventForProposal("blocked", prop, e.clock(), "", "", msg)
	if ev.Key == "" {
		ev.Key = strings.TrimSpace(key)
	}
	if ev.Revision == "" {
		ev.Revision = strings.TrimSpace(revision)
	}
	e.appendEvent(ev)
}

func proposalEventForProposal(eventType string, prop rpc.TradeProposal, at time.Time, tokenID, orderRef, msg string) proposalEvent {
	return proposalEvent{At: at, Type: eventType, Key: prop.Key, Revision: prop.Revision, Bucket: prop.Bucket, PolicyID: prop.PolicyID, PolicyVersion: prop.PolicyVersion, PolicyFingerprint: prop.PolicyFingerprint, PreviewTokenID: tokenID, OrderRef: orderRef, Message: msg, SourceFingerprints: prop.SourceFingerprints}
}

func (e *proposalEngine) appendEvent(ev proposalEvent) {
	if err := e.store.AppendEvent(ev); err != nil {
		e.server.warnf("trade proposals: append event: %v", err)
	}
}

func (e *proposalEngine) isIgnored(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.ignored[key]
	return ok
}

func (e *proposalEngine) clock() time.Time {
	if e.now != nil {
		return e.now().UTC()
	}
	return time.Now().UTC()
}

func emptyProposalSnapshot(now time.Time) rpc.TradeProposalSnapshot {
	return rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: now, Revision: "empty", Proposals: []rpc.TradeProposal{}}
}

func proposalCounts(proposals []rpc.TradeProposal) rpc.TradeProposalCounts {
	var out rpc.TradeProposalCounts
	out.Total = len(proposals)
	for _, p := range proposals {
		if len(p.Blockers) == 0 {
			out.Actionable++
		}
		out.MarketFlags += len(p.MarketFlags)
		switch p.Bucket {
		case rpc.TradeProposalBucketThetaHygiene:
			out.ThetaHygiene++
			out.ThetaPerDay += p.ThetaPerDay
		case rpc.TradeProposalBucketRiskReduction:
			out.RiskReduction++
			out.RiskReductionExcessNotional += p.RiskExcessNotional
			out.RiskReductionExcessCurrency = mergedCurrency(out.RiskReductionExcessCurrency, p.RiskExcessCurrency)
		case rpc.TradeProposalBucketTrailingStop:
			out.TrailingStop++
		}
	}
	return out
}

func proposalRevision(policy rpc.Fingerprint, sources rpc.TradeProposalSourceFingerprints, proposals []rpc.TradeProposal) string {
	stableSources := sources
	// Regime and market-event evidence are informative for ranking and blockers,
	// but their source-health fields can advance between list and preview. Keep
	// revision anchored to policy/account/positions so the one-confirm path does
	// not false-stale while refreshed proposals still carry live blockers.
	stableSources.Regime = nil
	stableSources.MarketEvents = nil
	projection := struct {
		Policy   rpc.Fingerprint                     `json:"policy"`
		Sources  rpc.TradeProposalSourceFingerprints `json:"sources"`
		Proposal []string                            `json:"proposal"`
	}{Policy: policy, Sources: stableSources}
	for _, p := range proposals {
		projection.Proposal = append(projection.Proposal, p.Key+":"+strconv.Itoa(p.Quantity)+":"+p.PositionEffect)
	}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func proposalKey(bucket string, contract rpc.ContractParams, action string) string {
	raw := strings.Join([]string{bucket, strings.ToUpper(contract.Symbol), strings.ToUpper(contract.SecType), strings.ToUpper(contract.LocalSymbol), contract.Expiry, strings.ToUpper(contract.Right), fmt.Sprintf("%.4f", contract.Strike), strings.ToUpper(action)}, "|")
	sum := sha256.Sum256([]byte(raw))
	return bucket + ":" + hex.EncodeToString(sum[:8])
}

func optionDTE(expiry string, now time.Time) (int, bool) {
	expiry = strings.TrimSpace(expiry)
	var t time.Time
	var err error
	switch len(expiry) {
	case len("20060102"):
		t, err = time.ParseInLocation("20060102", expiry, now.Location())
	case len("2006-01-02"):
		t, err = time.ParseInLocation("2006-01-02", expiry, now.Location())
	default:
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	exp := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location())
	return int(exp.Sub(today).Hours() / 24), true
}

func groupMarketValueOrderValue(g rpc.PositionGroup) float64 {
	if g.GroupMarketValue != 0 {
		return g.GroupMarketValue
	}
	if g.GroupMarketValueBase != nil {
		return *g.GroupMarketValueBase
	}
	return 0
}

func mergedCurrency(existing, next string) string {
	next = strings.ToUpper(strings.TrimSpace(next))
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	if existing == next {
		return existing
	}
	return "MIX"
}

func proposalSupportedSecType(secType string) bool {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "ETF", "OPT", "OPTION":
		return true
	default:
		return false
	}
}

func cloneProposalSnapshot(in rpc.TradeProposalSnapshot) rpc.TradeProposalSnapshot {
	out := in
	out.Proposals = append([]rpc.TradeProposal(nil), in.Proposals...)
	for i := range out.Proposals {
		out.Proposals[i].Details = append([]string(nil), in.Proposals[i].Details...)
		out.Proposals[i].MarketFlags = append([]rpc.MarketEventFlag(nil), in.Proposals[i].MarketFlags...)
		out.Proposals[i].Blockers = append([]rpc.TradingBlocker(nil), in.Proposals[i].Blockers...)
	}
	out.Blockers = append([]rpc.TradingBlocker(nil), in.Blockers...)
	if in.MarketEvents != nil {
		events := *in.MarketEvents
		events.Flags = append([]rpc.MarketEventFlag(nil), in.MarketEvents.Flags...)
		events.SourceHealth = append([]rpc.SourceHealth(nil), in.MarketEvents.SourceHealth...)
		events.WarningDetails = append([]rpc.DataWarning(nil), in.MarketEvents.WarningDetails...)
		if in.MarketEvents.BySymbol != nil {
			events.BySymbol = make(map[string][]rpc.MarketEventFlag, len(in.MarketEvents.BySymbol))
			for sym, flags := range in.MarketEvents.BySymbol {
				events.BySymbol[sym] = append([]rpc.MarketEventFlag(nil), flags...)
			}
		}
		out.MarketEvents = &events
	}
	return out
}

func (s *proposalStore) SaveCurrent(snap rpc.TradeProposalSnapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writePrivateStateAtomic(s.currentPath, data)
}

func (s *proposalStore) LoadCurrent() (rpc.TradeProposalSnapshot, error) {
	data, err := os.ReadFile(s.currentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return rpc.TradeProposalSnapshot{}, nil
		}
		return rpc.TradeProposalSnapshot{}, err
	}
	var snap rpc.TradeProposalSnapshot
	err = json.Unmarshal(data, &snap)
	return snap, err
}

func (s *proposalStore) AppendEvent(ev proposalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if ev.Version == 0 {
		ev.Version = proposalEventFileVersion
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := ensurePrivateStateDir(s.eventsPath); err != nil {
		return err
	}
	f, err := os.OpenFile(s.eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

func (s *proposalStore) FindSubmittedEvent(orderRef, tokenID string) (proposalEvent, bool, error) {
	if s == nil || s.eventsPath == "" {
		return proposalEvent{}, false, nil
	}
	orderRef = strings.TrimSpace(orderRef)
	tokenID = strings.TrimSpace(tokenID)
	if orderRef == "" && tokenID == "" {
		return proposalEvent{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return proposalEvent{}, false, nil
		}
		return proposalEvent{}, false, err
	}
	defer func() { _ = f.Close() }()
	var found proposalEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev proposalEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return proposalEvent{}, false, err
		}
		if ev.Type != "submitted" {
			continue
		}
		if orderRef != "" && ev.OrderRef == orderRef || tokenID != "" && ev.PreviewTokenID == tokenID {
			found = ev
		}
	}
	if err := sc.Err(); err != nil {
		return proposalEvent{}, false, err
	}
	if found.Type == "" {
		return proposalEvent{}, false, nil
	}
	return found, true, nil
}
