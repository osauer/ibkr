package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/marketcal"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
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
	// Observe the complete result before any caller-local offender filter.
	// A filtered view cannot prove that rules outside its symbol are clear.
	s.observeRulebookAlertShadow(ctx, res, s.currentBrokerStateScope())
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
	return s.evaluateRulesMode(ctx, includeTape, true)
}

// evaluateRulesMode lets read-only daemon composition reuse the exact rule
// mapper/evaluator without turning its account fetch into a capital-state
// observation. Transition journaling remains exclusively in
// handleRulesSnapshot.
func (s *Server) evaluateRulesMode(ctx context.Context, includeTape, allowMaintenance bool) *rpc.RulesResult {
	now := time.Now()
	pol := risk.DefaultRulebookPolicy()
	fp := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: pol.FingerprintKey()}
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

	acct, acctErr := s.buildAccountSummary(ctx, allowMaintenance)
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

	// Rule 14 inputs: base-normalized non-base NLV, corroborated against the
	// positions snapshot so an empty currency report on a book with non-base
	// legs degrades to unknown instead of passing (never-false-pass). An
	// unhealthy or unprimed snapshot cannot corroborate anything — pass nil
	// so the empty-report case stays unknown during boot races.
	if acct != nil {
		corr := pos
		if !in.Positions.Healthy {
			corr = nil
		}
		in.NonBaseNLVBase, in.NonBaseCurrencies = nonBaseExposure(acct, corr)
	}

	// Rules 3/4/12 regime-conditional thresholds: serve the latched stage,
	// mark it carried past the policy max age, and self-heal a cold or stale
	// latch with one async refresh (single-flight; never from previews).
	if st := s.rulesRegimeStageSnapshot(); st.Bucket != "" {
		in.RegimeStage = st.Bucket
		in.RegimeStageAsOf = st.AsOf
		maxAge := time.Duration(pol.RegimeStageMaxAgeMinutes) * time.Minute
		in.RegimeStageCarried = time.Since(st.AsOf) > maxAge
		stageStatus := "ok"
		if in.RegimeStageCarried {
			stageStatus = "stale"
		}
		health = append(health, rpc.SourceHealth{Source: "regime_stage", Status: stageStatus, AsOf: st.AsOf,
			Notes: []string{fmt.Sprintf("stage %s (%s bucket)", st.Stage, st.Bucket)}})
	} else {
		health = append(health, rpc.SourceHealth{Source: "regime_stage", Status: "unavailable",
			Notes: []string{"no regime stage observed yet; calm thresholds apply until a regime snapshot lands"}})
	}
	if includeTape && (in.RegimeStage == "" || in.RegimeStageCarried) {
		s.kickRulesRegimeStageRefresh(ctx)
	}

	earningsDegraded := false
	if in.Positions.Healthy && pos != nil {
		in.Names = mapRuleNames(pos, pol, in.BaseCurrency)
		earnings, infos := s.assembleEarnings(ctx, in.Names, pol, cal, now, allowMaintenance)
		in.Earnings = earnings
		res.Earnings = infos
		earningsHealth, degraded := rulesEarningsSourceHealth(infos, now)
		earningsDegraded = degraded
		health = append(health, earningsHealth)
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
	if !in.Positions.Healthy || !in.Account.Healthy || earningsDegraded {
		res.Status = "degraded"
	}
	// The envelope is the completion boundary for the assembled inputs. Some
	// components (notably the positions read model) stamp their own receipt
	// after evaluation starts; retaining the start time here makes valid
	// same-call evidence appear to come from the future to strict consumers.
	res.AsOf = time.Now().UTC()
	return res
}

func rulesEarningsSourceHealth(infos []rpc.EarningsInfo, now time.Time) (rpc.SourceHealth, bool) {
	status := rpc.SourceStatusOK
	degraded := false
	var lastFailure *rpc.SourceFailure
	var notes []string
	for _, info := range infos {
		if info.Source == "unknown" || info.Stale || info.Status != rpc.EarningsStatusDate {
			status = rpc.SourceStatusDegraded
			degraded = true
			notes = append(notes, info.Symbol+": "+nonEmptyString(info.Reason, nonEmptyString(info.Status, "not_observed")))
		} else if info.Reason == earningsReasonSingleSource && status == rpc.SourceStatusOK {
			status = rpc.SourceStatusPartial
		}
		for _, provider := range info.Providers {
			failure := provider.LastFailure
			if failure != nil && (lastFailure == nil || failure.FailedAt.After(lastFailure.FailedAt)) {
				copyFailure := *failure
				lastFailure = &copyFailure
			}
		}
	}
	return rpc.SourceHealth{Source: "earnings", Status: status, AsOf: now, LastFailure: lastFailure, Notes: notes}, degraded
}

// mapRuleNames converts the positions snapshot into pure rule inputs. The
// exposure figure is the same DollarDeltaBase the canary reads — one
// aggregation, several bars (design: "Which verdict wins when").
func mapRuleNames(pos *rpc.PositionsResult, pol risk.RulebookPolicy, baseCcy string) []risk.NameInput {
	loc, _ := time.LoadLocation("America/New_York")
	today := time.Now().In(loc)
	names := make([]risk.NameInput, 0, len(pos.ByUnderlying))
	for _, g := range pos.ByUnderlying {
		n := risk.NameInput{Symbol: g.Underlying}
		// ExposureBaseComplete guards rule 1's lower bound: the group sum
		// excludes legs the aggregator couldn't price (delta without spot,
		// markless stock, missing FX) — a bound seeded from a partial sum
		// could overstate, and "proven ≥" must never overstate.
		n.ExposureBaseComplete = g.GroupDollarDeltaBase != nil
		if g.GroupDollarDeltaBase != nil {
			n.ExposureBase = *g.GroupDollarDeltaBase
		}
		if g.GroupMarketValueBase != nil {
			n.MarketValueBase = *g.GroupMarketValueBase
		}
		var stockSpot *float64
		if g.Stock != nil && g.Stock.Quantity != 0 {
			n.HasStockLeg = true
			n.StockDayChangePct = g.Stock.DayChangePct
			// The account mark that values the book is good enough to assess
			// an option leg's OTM-ness/extrinsic when the leg's own greeks
			// tick carried no spot (routine off-session). Stale marks are
			// not: nil stays nil rather than valuing against a dead price.
			// Indicative is deliberately NOT gated: it describes the quote
			// enrichment layer, not the account mark, and pre-market — where
			// every stock row is indicative — is exactly when this join
			// earns its keep.
			if g.Stock.Mark > 0 && !g.Stock.Stale {
				stockSpot = new(g.Stock.Mark)
			}
			if g.Stock.Mark <= 0 {
				n.ExposureBaseComplete = false
			}
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
			if o.Delta != nil && o.Underlying == nil {
				// The aggregator can't pair this delta with a spot, so the
				// group sum excluded the leg — no lower bound may build on it.
				n.ExposureBaseComplete = false
			}
			if leg.Underlying == nil && stockSpot != nil {
				leg.Underlying = stockSpot
				leg.UnderlyingSource = risk.UnderlyingSourceStockLegMark
			}
			if o.MarketValueBase != nil {
				leg.MarketValueBase = *o.MarketValueBase
			} else {
				leg.MarketValueBase = o.MarketValue
			}
			// FX path: same-currency is implicitly 1 and non-base uses the
			// gateway rate (positionBaseRate) — the MV ratio is only a
			// fallback and is undefined on a zero-marked leg, which is
			// exactly the −100% line rule 13 exists to catch.
			if rate, ok := positionBaseRate(o, baseCcy); ok {
				leg.FXToBase = &rate
			} else if o.MarketValueBase != nil && o.MarketValue != 0 {
				leg.FXToBase = new(*o.MarketValueBase / o.MarketValue)
			}
			// IBKR AvgCost is multiplier-inclusive on options: cost basis is
			// AvgCost × |contracts| × fx, with NO extra multiplier.
			if leg.FXToBase != nil && o.AvgCost > 0 {
				leg.CostBasisBase = new(o.AvgCost * math.Abs(o.Quantity) * *leg.FXToBase)
			}
			if exp, err := time.ParseInLocation("20060102", o.Expiry, loc); err == nil {
				leg.Expiry = exp
				leg.DTE = int(exp.Sub(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)).Hours() / 24)
			}
			if extPerShare, ok := risk.OptionExtrinsicPerShare(o.Right, leg.Underlying, o.Strike, o.Mark); ok && o.Quantity > 0 {
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

// nonBaseExposure sums base-normalized NLV held outside the account's base
// currency (rule 14). nil = unavailable: an empty currency report is only
// trusted as "base-only" when the positions snapshot corroborates it —
// $LEDGER flakes must degrade to unknown, never pass a 90%-USD book.
func nonBaseExposure(acct *rpc.AccountResult, pos *rpc.PositionsResult) (*float64, []string) {
	base := strings.ToUpper(strings.TrimSpace(acct.BaseCurrency))
	if base == "" {
		return nil, nil
	}
	if len(acct.CurrencyExposure) == 0 {
		if positionsCarryNonBase(pos, base) {
			return nil, nil // report absent but the book visibly holds non-base legs
		}
		return new(0.0), nil // documented same-currency shape, corroborated
	}
	total := 0.0
	var ccys []string
	for _, row := range acct.CurrencyExposure {
		ccy := strings.ToUpper(strings.TrimSpace(row.Currency))
		if ccy == "" || ccy == base || ccy == "BASE" {
			continue
		}
		if row.ExchangeRate == 0 || (row.NetLiquidationBase == 0 && row.NetLiquidationCcy != 0) {
			return nil, nil // FX conversion unavailable for this slice of NLV
		}
		total += row.NetLiquidationBase
		ccys = append(ccys, ccy)
	}
	return &total, ccys
}

func positionsCarryNonBase(pos *rpc.PositionsResult, base string) bool {
	if pos == nil {
		return true // can't corroborate — stay unknown
	}
	for _, g := range pos.ByUnderlying {
		if g.Stock != nil && g.Stock.Currency != "" && !strings.EqualFold(g.Stock.Currency, base) {
			return true
		}
		for _, o := range g.Options {
			if o.Currency != "" && !strings.EqualFold(o.Currency, base) {
				return true
			}
		}
	}
	if pos.Portfolio != nil && pos.Portfolio.FXSensitivityPerPct != nil {
		return true
	}
	return false
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// assembleEarnings merges manual overrides (authoritative) with the cache,
// kicks the async refresher for gaps, and computes ET session distances.
func (s *Server) assembleEarnings(ctx context.Context, names []risk.NameInput, pol risk.RulebookPolicy, cal *marketcal.Calendar, now time.Time, allowRefresh bool) (map[string]risk.EarningsInput, []rpc.EarningsInfo) {
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
		info := rpc.EarningsInfo{Symbol: sym, Source: "unknown", Reason: "not_observed"}
		if raw, ok := overrides[sym]; ok {
			if e, parseOK := parseEarningsOverride(raw, loc); parseOK {
				e.Source = "override"
				e.Reason = "override"
				e.SessionsUntil = sessionsUntil(cal, now.In(loc), e.Date)
				earnings[sym] = e
				info.Source = "override"
				info.Status = rpc.EarningsStatusDate
				info.Reason = "override"
				info.Date = e.Date.Format("2006-01-02")
				info.TimeOfDay = e.TimeOfDay
				infos = append(infos, info)
				continue
			}
		}
		if s.earnings != nil {
			view, observed := s.earnings.resolution(sym)
			if observed {
				info.Status = view.Status
				info.Reason = view.Reason
				info.Providers = view.Providers
				if view.Status == rpc.EarningsStatusDate {
					entry := view.Entry
					if d, err := time.ParseInLocation("2006-01-02", entry.Date, loc); err == nil {
						e := risk.EarningsInput{Known: true, Date: d, TimeOfDay: entry.TimeOfDay,
							Estimated: entry.Estimated, Stale: view.Stale, Source: "fetched", Reason: view.Reason}
						e.SessionsUntil = sessionsUntil(cal, now.In(loc), d)
						earnings[sym] = e
						info.Source = "fetched"
						info.Date = entry.Date
						info.TimeOfDay = entry.TimeOfDay
						info.Estimated = entry.Estimated
						info.ObservedAt = entry.ObservedAt
						info.Stale = view.Stale
					}
				} else {
					earnings[sym] = risk.EarningsInput{Known: false, Source: "fetched", Reason: nonEmptyString(view.Reason, view.Status)}
				}
			}
		}
		if _, ok := earnings[sym]; !ok {
			earnings[sym] = risk.EarningsInput{Known: false, Source: "unknown", Reason: info.Reason}
		}
		// Aggregate freshness and provider retry readiness are different
		// clocks. Always hand non-override names to the cache; kickRefresh
		// cheaply filters them by each provider's durable NextAttempt.
		toFetch = append(toFetch, sym)
		infos = append(infos, info)
	}
	// Async, bounded, off the snapshot path — this call returns immediately.
	if allowRefresh && s.earnings != nil {
		s.earnings.kickRefresh(context.WithoutCancel(ctx), toFetch)
	}
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

// journalRuleTransitions appends status changes as typed SQLite events so
// threshold calibration has a direct authoritative history. The JSONL branch
// remains only for legacy unit/import oracles.
func (s *Server) journalRuleTransitions(res *rpc.RulesResult) {
	s.rulesMu.Lock()
	prev := s.lastRules
	s.rulesMu.Unlock()
	if res == nil || len(res.Rules) == 0 {
		return
	}
	policyFingerprint := ""
	if res.PolicyFingerprint != nil {
		policyFingerprint = res.PolicyFingerprint.Key
	}
	prevStatus := map[string]string{}
	if prev != nil {
		for _, r := range prev.Rules {
			prevStatus[r.ID] = r.Status
		}
	}
	// All transition lines are buffered and land in ONE write syscall: to
	// the history-index ingester a torn multi-syscall append would be
	// indistinguishable from a crash mid-write. Line shape is unchanged
	// (json.Encoder emits each map with a trailing newline).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	var coreEvents []corestore.EventInput
	coreEventOrdinal := 0
	if s.coreStore != nil {
		head, err := s.coreStore.AuthorityHead(context.Background())
		if err != nil {
			s.warnf("rules transitions: SQLite authority head unavailable: %v", err)
			return
		}
		coreEventOrdinal = int(head.LastEventSeq) + 1
	}
	for _, r := range res.Rules {
		if was, seen := prevStatus[r.ID]; seen && was == r.Status {
			continue
		}
		entry := map[string]any{
			"version": 1, "at": res.AsOf, "rule": r.ID, "status": r.Status,
			"was": prevStatus[r.ID], "evidence": r.Evidence,
			"policy_id": res.PolicyID, "policy_version": res.PolicyVersion,
			"policy_fingerprint": policyFingerprint,
		}
		_ = enc.Encode(entry)
		if s.coreStore != nil {
			raw, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			version := int64(res.PolicyVersion)
			key := coreEventKey(coreEventRuleTransition, res.AsOf, raw, coreEventOrdinal+len(coreEvents))
			coreEvents = append(coreEvents, corestore.EventInput{
				ScopeKey: daemonStateScope, EventKey: key, Type: coreEventRuleTransition,
				Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
				OccurredAt: res.AsOf, PayloadJSON: raw,
				Projection: corestore.EventProjection{RuleTransition: &corestore.RuleTransitionProjection{
					RuleID: r.ID, Status: r.Status, PreviousStatus: prevStatus[r.ID],
					PolicyID: res.PolicyID, PolicyVersion: &version, PolicyFingerprint: policyFingerprint,
				}},
			})
		}
	}
	if s.coreStore != nil {
		if len(coreEvents) > 0 {
			if _, err := s.coreStore.AppendEvents(context.Background(), coreEvents); err != nil {
				s.warnf("rules transitions: SQLite append failed: %v", err)
			}
		}
		return
	}
	// rulesJournalMu is the writer-quiescence lock journal rotation
	// excludes: held across path resolve, open, write, and close so a
	// live-file swap can never interleave with an append. Line shape,
	// dedupe, and the 0o644 mode are untouched.
	s.rulesJournalMu.Lock()
	path, err := defaultTradingStatePath("rules-decisions.jsonl")
	if err != nil {
		s.rulesJournalMu.Unlock()
		return
	}
	if err := ensurePrivateStateDir(path); err != nil {
		s.rulesJournalMu.Unlock()
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.rulesJournalMu.Unlock()
		return
	}
	if buf.Len() > 0 {
		_, _ = f.Write(buf.Bytes())
	}
	_ = f.Close()
	s.rulesJournalMu.Unlock()
	// Wake the history-index ingester; data-free by design (the journal
	// file is the only ingest input).
	s.kickHistoryIndex()
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
	// Averaging down is the one behavior that resets rule 13's loss fence —
	// warn when the drafted option leg matches a line already past it. Rule
	// 14 (fx_exposure) deliberately has no preview cause: at structurally
	// high non-base exposure it would fire on every ordinary order, and a
	// warning with a 100% base rate trains the operator to ignore rule_*
	// causes entirely.
	if r, ok := breached(risk.RuleExitDiscipline); ok && isBuy && isOption {
		draftDesc := fmt.Sprintf("%s %s %s %s", sym,
			strings.ReplaceAll(draft.Contract.Expiry, "-", ""), strings.ToUpper(draft.Contract.Right), trimFloat(draft.Contract.Strike))
		for _, o := range r.Offenders {
			if strings.EqualFold(o.Leg, draftDesc) {
				out = append(out, warn(r, fmt.Sprintf("%s is already past the exit-discipline loss fence; averaging down resets the basis and hides the loss.", draftDesc)))
				break
			}
		}
	}
	return out
}
