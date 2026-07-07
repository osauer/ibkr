package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/marketcal"
	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
)

// Trading rulebook assembly (docs/design/trading-rulebook.md). The pure
// evaluator lives in internal/risk; this file maps daemon state into
// risk.RuleInputs, owns the cached evaluation preview causes read, and the
// rules-decisions journal. Advisory-only end to end: nothing here touches
// submit eligibility or broker-write authorization.

const (
	// rulesPreviewTTL bounds how stale a cached evaluation may be when the
	// order-preview path annotates drafts with rule causes. Previews never
	// trigger a fresh assembly — a preview must stay a preview-priced call.
	rulesPreviewTTL = 45 * time.Second
	// rulesSPYQuoteTimeout bounds the one best-effort SPY snapshot quote
	// per evaluation during regular hours (rules 9/10 tape context).
	rulesSPYQuoteTimeout = 2500 * time.Millisecond
)

// rulebookEnabled reads features.rulebook.enabled (runtime, default true).
func (s *Server) rulebookEnabled() bool {
	if s.platformSettings == nil {
		return true
	}
	data := s.platformSettings.snapshot()
	if data.Features.Rulebook.Enabled != nil {
		return *data.Features.Rulebook.Enabled
	}
	return true
}

// rulebookEarningsOverrides returns the operator's manual per-symbol
// earnings dates ("YYYY-MM-DD" or "YYYY-MM-DDTamc"/"Tbmo"), uppercased keys.
func (s *Server) rulebookEarningsOverrides() map[string]string {
	if s.platformSettings == nil {
		return nil
	}
	data := s.platformSettings.snapshot()
	if len(data.Features.Rulebook.EarningsOverrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(data.Features.Rulebook.EarningsOverrides))
	for sym, v := range data.Features.Rulebook.EarningsOverrides {
		out[strings.ToUpper(strings.TrimSpace(sym))] = strings.TrimSpace(v)
	}
	return out
}

func (s *Server) handleRulesSnapshot(ctx context.Context, req *rpc.Request) (*rpc.RulesResult, error) {
	var params rpc.RulesSnapshotParams
	if len(req.Params) > 0 {
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, err
		}
	}
	res := s.evaluateRules(ctx, true)
	if params.Symbol != "" {
		filterRuleOffenders(res, strings.ToUpper(strings.TrimSpace(params.Symbol)))
	}
	s.journalRuleTransitions(res)
	s.rulesMu.Lock()
	s.lastRules = res
	s.lastRulesAt = time.Now()
	s.rulesMu.Unlock()
	return res, nil
}

// rulesForPreview returns the last evaluation if fresh enough for advisory
// preview causes; nil means "no rule warnings", never an error.
func (s *Server) rulesForPreview(ctx context.Context) *rpc.RulesResult {
	if !s.rulebookEnabled() {
		return nil
	}
	s.rulesMu.Lock()
	cached, at := s.lastRules, s.lastRulesAt
	s.rulesMu.Unlock()
	if cached != nil && time.Since(at) <= rulesPreviewTTL {
		return cached
	}
	// Tape rules (9/10) never produce preview causes, so skip the SPY quote
	// on this path — a preview must not pay a market-data round-trip.
	res := s.evaluateRules(ctx, false)
	s.rulesMu.Lock()
	s.lastRules = res
	s.lastRulesAt = time.Now()
	s.rulesMu.Unlock()
	return res
}

func (s *Server) evaluateRules(ctx context.Context, includeTape bool) *rpc.RulesResult {
	now := time.Now()
	pol := risk.DefaultRulebookPolicy()
	fp := rpc.Fingerprint{Version: "rulebook-fp-v1", Key: pol.FingerprintKey()}
	res := &rpc.RulesResult{
		AsOf:              now,
		Enabled:           s.rulebookEnabled(),
		Status:            "ok",
		PolicyID:          pol.ID,
		PolicyVersion:     pol.Version,
		PolicyFingerprint: &fp,
	}
	if !res.Enabled {
		res.Status = "disabled"
		return res
	}

	in := risk.RuleInputs{AsOf: now}
	var health []rpc.SourceHealth

	acct, acctErr := s.handleAccountSummary(ctx)
	if acctErr != nil || acct == nil {
		in.Account = risk.SourceState{Healthy: false, Reason: "account_unavailable"}
		health = append(health, rpc.SourceHealth{Source: "account", Status: "unavailable", Notes: []string{errText(acctErr)}})
	} else {
		in.Account = risk.SourceState{Healthy: true}
		in.NLVBase = new(acct.NetLiquidation)
		in.CashBase = new(acct.TotalCash)
		in.DailyPnLBase = acct.DailyPnL
		in.BaseCurrency = acct.BaseCurrency
		res.BaseCurrency = acct.BaseCurrency
		health = append(health, rpc.SourceHealth{Source: "account", Status: "ok", AsOf: now})
	}

	pos, posErr := s.handlePositionsList(ctx, &rpc.Request{})
	switch {
	case posErr != nil || pos == nil:
		in.Positions = risk.SourceState{Healthy: false, Reason: "positions_unavailable"}
		health = append(health, rpc.SourceHealth{Source: "positions", Status: "unavailable", Notes: []string{errText(posErr)}})
	case acct != nil && proposalPositionsUnprimed(pos, acct):
		in.Positions = risk.SourceState{Healthy: false, Reason: "positions_pending"}
		health = append(health, rpc.SourceHealth{Source: "positions", Status: "pending",
			Notes: []string{"portfolio stream not yet primed; account summary reports open positions"}})
	default:
		in.Positions = risk.SourceState{Healthy: true}
		health = append(health, rpc.SourceHealth{Source: "positions", Status: "ok", AsOf: pos.AsOf})
	}

	cal := marketcal.New()
	if session, err := cal.SessionAt(marketcal.MarketUSEquity, now); err == nil {
		in.SessionOpen = session.IsOpen && session.State != marketcal.StateClosed && session.State != marketcal.StateHoliday
	}

	if in.Positions.Healthy && pos != nil {
		in.Names = mapRuleNames(pos, pol)
		earnings, infos := s.assembleEarnings(ctx, in.Names, pol, cal, now)
		in.Earnings = earnings
		res.Earnings = infos
		earningsStatus := "ok"
		for _, info := range infos {
			if info.Source == "unknown" || info.Stale {
				earningsStatus = "degraded"
				break
			}
		}
		health = append(health, rpc.SourceHealth{Source: "earnings", Status: earningsStatus, AsOf: now})
	}

	if includeTape && in.SessionOpen && in.Positions.Healthy {
		in.SPYDayChangePct = s.spyDayChangePct(ctx)
		tapeStatus := "ok"
		if in.SPYDayChangePct == nil {
			tapeStatus = "unavailable"
		}
		health = append(health, rpc.SourceHealth{Source: "tape", Status: tapeStatus, AsOf: now})
	}

	ev := risk.EvaluateRulebook(in, pol)
	res.Rules = ev.Rows
	res.Ranked = ev.Ranked
	res.InputHealth = health
	counts := map[string]int{}
	for _, r := range ev.Rows {
		counts[r.Status]++
	}
	res.BreachCounts = counts
	if !in.Positions.Healthy || !in.Account.Healthy {
		res.Status = "degraded"
	}
	return res
}

// mapRuleNames converts the positions snapshot into pure rule inputs. The
// exposure figure is the same DollarDeltaBase the canary reads — one
// aggregation, several bars (design: "Which verdict wins when").
func mapRuleNames(pos *rpc.PositionsResult, pol risk.RulebookPolicy) []risk.NameInput {
	loc, _ := time.LoadLocation("America/New_York")
	today := time.Now().In(loc)
	names := make([]risk.NameInput, 0, len(pos.ByUnderlying))
	for _, g := range pos.ByUnderlying {
		n := risk.NameInput{Symbol: g.Underlying}
		if g.GroupDollarDeltaBase != nil {
			n.ExposureBase = *g.GroupDollarDeltaBase
		}
		if g.GroupMarketValueBase != nil {
			n.MarketValueBase = *g.GroupMarketValueBase
		}
		if g.Stock != nil && g.Stock.Quantity != 0 {
			n.HasStockLeg = true
			n.StockDayChangePct = g.Stock.DayChangePct
		}
		for _, o := range g.Options {
			if o.Quantity == 0 {
				continue
			}
			leg := risk.LegInput{
				Desc:        legDesc(o),
				Right:       o.Right,
				Strike:      o.Strike,
				Quantity:    o.Quantity,
				Multiplier:  float64(max(o.Multiplier, 1)),
				Mark:        o.Mark,
				Underlying:  o.Underlying,
				Delta:       o.Delta,
				HedgeListed: pol.IsHedgeSymbol(g.Underlying),
			}
			if o.MarketValueBase != nil {
				leg.MarketValueBase = *o.MarketValueBase
			} else {
				leg.MarketValueBase = o.MarketValue
			}
			if exp, err := time.ParseInLocation("20060102", o.Expiry, loc); err == nil {
				leg.Expiry = exp
				leg.DTE = int(exp.Sub(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)).Hours() / 24)
			}
			if extPerShare, ok := risk.OptionExtrinsicPerShare(o.Right, o.Underlying, o.Strike, o.Mark); ok && o.Quantity > 0 {
				ext := extPerShare * o.Quantity * leg.Multiplier
				if o.MarketValueBase != nil && o.MarketValue != 0 {
					ext *= *o.MarketValueBase / o.MarketValue
				}
				leg.ExtrinsicBase = &ext
			}
			if o.Delta == nil {
				gap := leg.MarketValueBase
				if gap < 0 {
					gap = -gap
				}
				n.GreeksGapNotionalBase += gap
			}
			n.Legs = append(n.Legs, leg)
		}
		if n.HasStockLeg || len(n.Legs) > 0 {
			names = append(names, n)
		}
	}
	return names
}

func legDesc(o rpc.PositionView) string {
	return fmt.Sprintf("%s %s %s %s", o.Symbol, o.Expiry, o.Right, trimFloat(o.Strike))
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// assembleEarnings merges manual overrides (authoritative) with the cache,
// kicks the async refresher for gaps, and computes ET session distances.
func (s *Server) assembleEarnings(ctx context.Context, names []risk.NameInput, pol risk.RulebookPolicy, cal *marketcal.Calendar, now time.Time) (map[string]risk.EarningsInput, []rpc.EarningsInfo) {
	loc, _ := time.LoadLocation("America/New_York")
	overrides := s.rulebookEarningsOverrides()
	earnings := make(map[string]risk.EarningsInput, len(names))
	infos := make([]rpc.EarningsInfo, 0, len(names))
	var toFetch []string
	for _, n := range names {
		sym := strings.ToUpper(n.Symbol)
		if pol.IsHedgeSymbol(sym) {
			continue // index products have no earnings print
		}
		info := rpc.EarningsInfo{Symbol: sym, Source: "unknown"}
		if raw, ok := overrides[sym]; ok {
			if e, parseOK := parseEarningsOverride(raw, loc); parseOK {
				e.Source = "override"
				e.SessionsUntil = sessionsUntil(cal, now.In(loc), e.Date)
				earnings[sym] = e
				info.Source = "override"
				info.Date = e.Date.Format("2006-01-02")
				info.TimeOfDay = e.TimeOfDay
				infos = append(infos, info)
				continue
			}
		}
		if entry, stale, ok := s.earnings.get(sym); ok {
			if d, err := time.ParseInLocation("2006-01-02", entry.Date, loc); err == nil {
				e := risk.EarningsInput{Known: true, Date: d, TimeOfDay: entry.TimeOfDay,
					Estimated: entry.Estimated, Stale: stale, Source: "fetched"}
				e.SessionsUntil = sessionsUntil(cal, now.In(loc), d)
				earnings[sym] = e
				info.Source = "fetched"
				info.Date = entry.Date
				info.TimeOfDay = entry.TimeOfDay
				info.Estimated = entry.Estimated
				info.ObservedAt = entry.ObservedAt
				info.Stale = stale
			}
		}
		if info.Source != "override" && (info.Source == "unknown" || info.Stale) {
			toFetch = append(toFetch, sym)
		}
		infos = append(infos, info)
	}
	// Async, bounded, off the snapshot path — this call returns immediately.
	s.earnings.kickRefresh(context.WithoutCancel(ctx), toFetch)
	return earnings, infos
}

// parseEarningsOverride accepts "YYYY-MM-DD" or "YYYY-MM-DDTamc"/"Tbmo".
func parseEarningsOverride(raw string, loc *time.Location) (risk.EarningsInput, bool) {
	tod := ""
	if date, half, found := strings.Cut(raw, "T"); found {
		half = strings.ToLower(strings.TrimSpace(half))
		if half == "amc" || half == "bmo" {
			tod = half
		}
		raw = date
	}
	d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(raw), loc)
	if err != nil {
		return risk.EarningsInput{}, false
	}
	return risk.EarningsInput{Known: true, Date: d, TimeOfDay: tod}, true
}

// sessionsUntil counts US equity sessions from today to target inclusive,
// in ET; nil when the target is behind us or unreasonably far.
func sessionsUntil(cal *marketcal.Calendar, today time.Time, target time.Time) *int {
	start := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	end := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, today.Location())
	if end.Before(start) {
		return nil
	}
	count := 0
	for d := start; !d.After(end) && count <= 40; d = d.AddDate(0, 0, 1) {
		session, err := cal.SessionAt(marketcal.MarketUSEquity, d.Add(16*time.Hour+30*time.Minute))
		if err != nil {
			return nil
		}
		if session.State == marketcal.StateRegular || session.State == marketcal.StateEarlyClose {
			count++
		}
	}
	return &count
}

// spyDayChangePct fetches one best-effort SPY snapshot quote for the tape
// rules. Snapshot quotes are transient (no standing subscription); failure
// degrades rule 9 to unknown rather than failing the evaluation.
func (s *Server) spyDayChangePct(ctx context.Context) *float64 {
	ctx, cancel := context.WithTimeout(ctx, rulesSPYQuoteTimeout)
	defer cancel()
	params, err := json.Marshal(rpc.QuoteSnapshotParams{
		Contract:  rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"},
		TimeoutMs: int(rulesSPYQuoteTimeout / time.Millisecond),
	})
	if err != nil {
		return nil
	}
	q, err := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: params})
	if err != nil || q == nil {
		return nil
	}
	if q.QuoteChangePct != nil {
		return q.QuoteChangePct
	}
	return q.ChangePct
}

// filterRuleOffenders narrows offender/exempt lists to one symbol without
// changing verdicts — scoping is presentation, not policy.
func filterRuleOffenders(res *rpc.RulesResult, sym string) {
	for i := range res.Rules {
		res.Rules[i].Offenders = filterOffenders(res.Rules[i].Offenders, sym)
		res.Rules[i].Exempt = filterOffenders(res.Rules[i].Exempt, sym)
	}
}

func filterOffenders(list []risk.RuleOffender, sym string) []risk.RuleOffender {
	if len(list) == 0 {
		return list
	}
	out := list[:0:0]
	for _, o := range list {
		if strings.EqualFold(o.Symbol, sym) {
			out = append(out, o)
		}
	}
	return out
}

// journalRuleTransitions appends status changes to rules-decisions.jsonl so
// week-one threshold calibration has data (regime-decisions precedent).
func (s *Server) journalRuleTransitions(res *rpc.RulesResult) {
	s.rulesMu.Lock()
	prev := s.lastRules
	s.rulesMu.Unlock()
	if res == nil || len(res.Rules) == 0 {
		return
	}
	prevStatus := map[string]string{}
	if prev != nil {
		for _, r := range prev.Rules {
			prevStatus[r.ID] = r.Status
		}
	}
	path, err := defaultTradingStatePath("rules-decisions.jsonl")
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range res.Rules {
		if was, seen := prevStatus[r.ID]; seen && was == r.Status {
			continue
		}
		_ = enc.Encode(map[string]any{
			"version": 1, "at": res.AsOf, "rule": r.ID, "status": r.Status,
			"was": prevStatus[r.ID], "evidence": r.Evidence,
			"policy_id": res.PolicyID, "policy_version": res.PolicyVersion,
		})
	}
}

func errText(err error) string {
	if err == nil {
		return "unavailable"
	}
	return err.Error()
}

// rulebookPreviewWarnings maps currently breached rules to advisory
// DataWarnings on an order preview — only when the draft would WORSEN the
// breached metric. Reduce/close intents never warn; submit eligibility is
// never touched (advisory-only, docs/design/trading-rulebook.md).
func rulebookPreviewWarnings(res *rpc.RulesResult, draft rpc.OrderDraft, position rpc.OrderPositionImpact) []rpc.DataWarning {
	if res == nil || !res.Enabled || len(res.Rules) == 0 {
		return nil
	}
	switch position.Effect {
	case "close", "reduce":
		return nil
	}
	sym := strings.ToUpper(draft.Contract.Symbol)
	isBuy := strings.EqualFold(draft.Action, "BUY")
	isOption := strings.EqualFold(draft.Contract.SecType, "OPT")
	rows := map[string]risk.RuleRow{}
	for _, r := range res.Rules {
		rows[r.ID] = r
	}
	breached := func(id string) (risk.RuleRow, bool) {
		r, found := rows[id]
		return r, found && (r.Status == risk.RuleStatusWatch || r.Status == risk.RuleStatusAct)
	}
	offends := func(r risk.RuleRow) bool {
		for _, o := range r.Offenders {
			if strings.EqualFold(o.Symbol, sym) {
				return true
			}
		}
		return false
	}
	warn := func(r risk.RuleRow, msg string) rpc.DataWarning {
		return rpc.DataWarning{
			Code:     "rule_" + r.ID,
			Scope:    "rulebook",
			Severity: r.Status,
			Message:  msg,
			Impact:   fmt.Sprintf("Advisory rulebook cause (rule %d, as of %s); submit eligibility is unaffected.", r.Number, res.AsOf.Format(time.RFC3339)),
			Action:   "Run `ibkr rules` for the full checklist.",
		}
	}
	var out []rpc.DataWarning
	if r, ok := breached(risk.RuleSingleNameExposure); ok && offends(r) {
		out = append(out, warn(r, fmt.Sprintf("%s already breaches the per-name exposure cap; this order increases it.", sym)))
	}
	if r, ok := breached(risk.RuleOptionLinePremium); ok && isBuy && isOption && offends(r) {
		out = append(out, warn(r, fmt.Sprintf("%s already holds an option line over the premium cap; this adds premium.", sym)))
	}
	if r, ok := breached(risk.RuleCashSellOnly); ok && isBuy {
		out = append(out, warn(r, "Cash ratio is below the sell-only floor; a buy deepens the margin debit."))
	}
	if r, ok := breached(risk.RuleExtrinsicBudget); ok && isBuy && isOption {
		out = append(out, warn(r, "Portfolio extrinsic already exceeds its budget; buying options adds nightly decay."))
	}
	if r, ok := breached(risk.RuleEarningsSizeFreeze); ok && offends(r) {
		out = append(out, warn(r, fmt.Sprintf("%s is oversized inside its pre-earnings freeze window; adding is the opposite of at-size.", sym)))
	}
	return out
}
