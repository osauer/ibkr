package risk

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// The trading rulebook: 12 advisory rules evaluated against a mapped
// snapshot of the book (docs/design/trading-rulebook.md). Pure package —
// the daemon maps RPC state into RuleInputs; this file only computes.
//
// The load-bearing invariant is NEVER FALSE PASS: any absent, pending, or
// partial input degrades the affected rows to unknown/not_evaluated. A rule
// may only report pass when its inputs were present and healthy.

// Rule row statuses.
const (
	RuleStatusPass         = "pass"
	RuleStatusInfo         = "info"
	RuleStatusWatch        = "watch"
	RuleStatusAct          = "act"
	RuleStatusUnknown      = "unknown"
	RuleStatusNotEvaluated = "not_evaluated"
)

// Rule ids, in rulebook order.
const (
	RuleSingleNameExposure = "single_name_exposure"
	RuleOptionLinePremium  = "option_line_premium"
	RuleCashSellOnly       = "cash_sell_only"
	RuleExtrinsicBudget    = "extrinsic_budget"
	RuleExpiryRunway       = "expiry_runway"
	RuleCatalystCoverage   = "catalyst_coverage"
	RuleOverwriteEarnings  = "overwrite_earnings"
	RuleEarningsSizeFreeze = "earnings_size_freeze"
	RuleRedOnGreen         = "red_on_green"
	RuleWinnerTrim         = "winner_trim"
	RuleGreenDayAction     = "green_day_action"
	RuleHedgeIntegrity     = "hedge_integrity"
)

// RuleOffender is one name or leg contributing to a breach, worst first.
type RuleOffender struct {
	Symbol     string  `json:"symbol"`
	Leg        string  `json:"leg,omitempty"`
	Observed   float64 `json:"observed"`
	ImpactBase float64 `json:"impact_base,omitempty"`
	Note       string  `json:"note,omitempty"`
}

// RuleRow is one rule's verdict.
type RuleRow struct {
	ID         string         `json:"id"`
	Number     int            `json:"number"`
	Title      string         `json:"title"`
	Status     string         `json:"status"`
	Observed   *float64       `json:"observed,omitempty"`
	Threshold  *float64       `json:"threshold,omitempty"`
	Unit       string         `json:"unit,omitempty"`
	Evidence   string         `json:"evidence"`
	Reason     string         `json:"reason,omitempty"`
	Offenders  []RuleOffender `json:"offenders,omitempty"`
	Exempt     []RuleOffender `json:"exempt,omitempty"`
	ImpactBase float64        `json:"impact_base,omitempty"`
	Notes      []string       `json:"notes,omitempty"`
}

// EarningsInput is the per-name earnings context mapped by the daemon.
type EarningsInput struct {
	Known     bool
	Date      time.Time // ET calendar date (midnight ET)
	TimeOfDay string    // "amc" | "bmo" | "" (unspecified)
	Estimated bool
	Stale     bool
	// SessionsUntil is the number of US equity sessions from today (ET) to
	// the earnings date inclusive, computed by the daemon via marketcal.
	// nil when unknown.
	SessionsUntil *int
	Source        string
}

// LegInput is one option leg of a name.
type LegInput struct {
	Desc       string // "NOW 20260717 C 130"
	Right      string // C | P
	Strike     float64
	Expiry     time.Time // ET calendar date
	DTE        int
	Quantity   float64 // signed contracts
	Multiplier float64
	Mark       float64
	Underlying *float64
	Delta      *float64
	// MarketValueBase is the signed base-currency mark value of the leg.
	MarketValueBase float64
	// ExtrinsicBase is the base-currency extrinsic value for long legs;
	// nil when uncomputable (missing underlying or mark) — never zero it.
	ExtrinsicBase *float64
	// HedgeListed marks the underlying as being on the policy hedge list.
	HedgeListed bool
}

// NameInput is the per-underlying aggregation the daemon maps from
// PositionGroup/UnderlyingExposure. ExposureBase must be the same value the
// canary's concentration check reads.
type NameInput struct {
	Symbol string
	// ExposureBase = stock + Σ delta×contracts×multiplier×spot, base ccy.
	ExposureBase float64
	// GreeksGapNotionalBase is |notional| of option legs missing delta —
	// exposure understatement risk.
	GreeksGapNotionalBase float64
	MarketValueBase       float64
	HasStockLeg           bool
	// StockDayChangePct is the stock leg's quote-enriched day change; nil
	// for option-only names or when enrichment failed.
	StockDayChangePct *float64
	Legs              []LegInput
}

// SourceState is the mapped health of one input source.
type SourceState struct {
	Healthy bool
	Reason  string // positions_pending, account_unavailable, …
}

// RuleInputs is the full mapped snapshot for one evaluation.
type RuleInputs struct {
	AsOf         time.Time
	BaseCurrency string

	Positions SourceState
	Account   SourceState

	NLVBase      *float64
	CashBase     *float64
	DailyPnLBase *float64

	Names []NameInput

	// SessionOpen: US equity regular session per marketcal.
	SessionOpen bool
	// SPYDayChangePct is the market tape for rule 9; nil when unavailable.
	SPYDayChangePct *float64

	Earnings map[string]EarningsInput
}

// Evaluation is the pure result: rows in rulebook order plus the
// hardest-first ranking (indexes into Rows).
type Evaluation struct {
	Rows   []RuleRow
	Ranked []int
}

type ruleContext struct {
	in     RuleInputs
	pol    RulebookPolicy
	nlv    float64
	hasNLV bool
	// overHedged suppresses rule 5's hedge exemption (a "hedge" bigger than
	// the band protects nothing extra; it may be a directional bet).
	overHedged bool
}

// EvaluateRulebook computes all 12 rules. It never returns fewer than 12
// rows; degraded inputs degrade statuses, not row presence.
func EvaluateRulebook(in RuleInputs, pol RulebookPolicy) Evaluation {
	pol.Normalize()
	ctx := &ruleContext{in: in, pol: pol}
	if in.NLVBase != nil && *in.NLVBase > 0 {
		ctx.nlv = *in.NLVBase
		ctx.hasNLV = true
	}

	// Rule 12 runs first: its over-hedged verdict feeds rule 5's exemption
	// suppression. Rule 11 runs last: it reads the other rows' statuses.
	r12 := ctx.hedgeIntegrity()
	rows := []RuleRow{
		ctx.singleNameExposure(),
		ctx.optionLinePremium(),
		ctx.cashSellOnly(),
		ctx.extrinsicBudget(),
		ctx.expiryRunway(),
		ctx.catalystCoverage(),
		ctx.overwriteEarnings(),
		ctx.earningsSizeFreeze(),
		ctx.redOnGreen(),
		ctx.winnerTrim(),
		{}, // rule 11 placeholder
		r12,
	}
	rows[10] = ctx.greenDayAction(rows)
	return Evaluation{Rows: rows, Ranked: rankRows(rows)}
}

// portfolioGate degrades a portfolio-dependent rule when positions or
// account inputs are unhealthy. Returns a non-nil row to emit instead.
func (c *ruleContext) portfolioGate(id string, num int, title string) *RuleRow {
	if !c.in.Positions.Healthy {
		return &RuleRow{ID: id, Number: num, Title: title, Status: RuleStatusUnknown,
			Reason:   nonEmpty(c.in.Positions.Reason, "positions_unavailable"),
			Evidence: "Positions source not healthy — not asserting a pass."}
	}
	if !c.in.Account.Healthy || !c.hasNLV {
		return &RuleRow{ID: id, Number: num, Title: title, Status: RuleStatusUnknown,
			Reason:   nonEmpty(c.in.Account.Reason, "account_unavailable"),
			Evidence: "Account NLV not available — not asserting a pass."}
	}
	return nil
}

func (c *ruleContext) singleNameExposure() RuleRow {
	row := RuleRow{ID: RuleSingleNameExposure, Number: 1, Title: "Per-name exposure cap", Unit: "% NLV"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watch, act := c.pol.SingleNameWatchPct, c.pol.SingleNameActPct
	row.Threshold = new(act)
	var offenders, gaps []RuleOffender
	worst := 0.0
	for _, n := range c.in.Names {
		if c.greeksGapMaterial(n) {
			gaps = append(gaps, RuleOffender{Symbol: n.Symbol, Observed: pct(n.GreeksGapNotionalBase, c.nlv),
				Note: "delta unavailable on material legs; exposure understated"})
			continue
		}
		p := pct(math.Abs(n.ExposureBase), c.nlv)
		worst = math.Max(worst, p)
		if p >= watch {
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: round1(p),
				ImpactBase: math.Abs(n.ExposureBase)})
		}
	}
	sortOffenders(offenders)
	row.Observed = new(round1(worst))
	row.Offenders = offenders
	switch {
	case len(gaps) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "greeks_gap"
		row.Offenders = append(offenders, gaps...)
		row.Evidence = fmt.Sprintf("%d name(s) missing delta on material legs — exposure not trustworthy.", len(gaps))
	case worst > act:
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("%s at %.1f%% of NLV exceeds the %.0f%% cap.", offenders[0].Symbol, offenders[0].Observed, act)
	case worst >= watch:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("%s at %.1f%% of NLV approaches the %.0f%% cap.", offenders[0].Symbol, offenders[0].Observed, act)
	default:
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("Largest name %.1f%% of NLV, under the %.0f%% cap.", round1(worst), act)
	}
	for _, o := range row.Offenders {
		row.ImpactBase += o.ImpactBase
	}
	return row
}

func (c *ruleContext) optionLinePremium() RuleRow {
	row := RuleRow{ID: RuleOptionLinePremium, Number: 2, Title: "Single option line premium", Unit: "% NLV"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watch, act := c.pol.OptionLineWatchPct, c.pol.OptionLineActPct
	row.Threshold = new(watch)
	var offenders []RuleOffender
	worst := 0.0
	for _, n := range c.in.Names {
		for _, l := range n.Legs {
			if l.Quantity <= 0 {
				continue
			}
			p := pct(math.Abs(l.MarketValueBase), c.nlv)
			worst = math.Max(worst, p)
			if p >= watch {
				offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
					Observed: round1(p), ImpactBase: math.Abs(l.MarketValueBase)})
			}
		}
	}
	sortOffenders(offenders)
	row.Observed = new(round1(worst))
	row.Offenders = offenders
	switch {
	case worst > act:
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("%s holds %.1f%% of NLV in one option line (cap %.0f%%).", offenders[0].Leg, offenders[0].Observed, watch)
	case worst >= watch:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("%s holds %.1f%% of NLV in one option line (cap %.0f%%).", offenders[0].Leg, offenders[0].Observed, watch)
	default:
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("Largest option line %.1f%% of NLV, under the %.0f%% cap.", round1(worst), watch)
	}
	for _, o := range row.Offenders {
		row.ImpactBase += o.ImpactBase
	}
	return row
}

func (c *ruleContext) cashSellOnly() RuleRow {
	row := RuleRow{ID: RuleCashSellOnly, Number: 3, Title: "Negative cash sell-only mode", Unit: "% NLV"}
	if !c.in.Account.Healthy || !c.hasNLV || c.in.CashBase == nil {
		row.Status = RuleStatusUnknown
		row.Reason = nonEmpty(c.in.Account.Reason, "cash_unavailable")
		row.Evidence = "Cash balance not available — not asserting a pass."
		return row
	}
	limit := c.pol.CashSellOnlyPct
	ratio := pct(*c.in.CashBase, c.nlv)
	row.Observed = new(round1(ratio))
	row.Threshold = new(limit)
	if ratio < limit {
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("Cash at %.1f%% of NLV is below %.0f%% — sell-only until the debit shrinks (margin interest is negative carry too).", round1(ratio), limit)
	} else {
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("Cash at %.1f%% of NLV, above the %.0f%% floor.", round1(ratio), limit)
	}
	return row
}

func (c *ruleContext) extrinsicBudget() RuleRow {
	row := RuleRow{ID: RuleExtrinsicBudget, Number: 4, Title: "Portfolio extrinsic budget", Unit: "% NLV"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watch, act := c.pol.ExtrinsicWatchPct, c.pol.ExtrinsicActPct
	row.Threshold = new(watch)
	total := 0.0
	var offenders, unknowns []RuleOffender
	for _, n := range c.in.Names {
		for _, l := range n.Legs {
			if l.Quantity <= 0 {
				continue
			}
			if l.ExtrinsicBase == nil {
				if pct(math.Abs(l.MarketValueBase), c.nlv) >= c.pol.GreeksGapFloorPctNLV {
					unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
						Note: "extrinsic uncomputable (missing underlying or mark)"})
				}
				continue
			}
			total += *l.ExtrinsicBase
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
				Observed: round1(pct(*l.ExtrinsicBase, c.nlv)), ImpactBase: *l.ExtrinsicBase})
		}
	}
	p := pct(total, c.nlv)
	row.Observed = new(round1(p))
	row.ImpactBase = total
	if len(unknowns) > 0 {
		row.Status = RuleStatusUnknown
		row.Reason = "extrinsic_uncomputable"
		row.Offenders = unknowns
		row.Evidence = fmt.Sprintf("%d material leg(s) with uncomputable extrinsic — budget not trustworthy.", len(unknowns))
		return row
	}
	sortOffenders(offenders)
	if len(offenders) > 3 {
		offenders = offenders[:3]
	}
	row.Offenders = offenders
	switch {
	case p > act:
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("Paying decay on %.1f%% of NLV in extrinsic (budget %.0f%%).", round1(p), watch)
	case p >= watch:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("Extrinsic at %.1f%% of NLV against a %.0f%% budget.", round1(p), watch)
	default:
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("Extrinsic at %.1f%% of NLV, inside the %.0f%% budget.", round1(p), watch)
	}
	return row
}

func (c *ruleContext) expiryRunway() RuleRow {
	row := RuleRow{ID: RuleExpiryRunway, Number: 5, Title: "Expiry runway", Unit: "DTE"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watchDTE, actDTE := c.pol.RunwayWatchDTE, c.pol.RunwayActDTE
	row.Threshold = new(float64(watchDTE))
	var offenders, exempt []RuleOffender
	worstStatus := RuleStatusPass
	minDTE := math.Inf(1)
	for _, n := range c.in.Names {
		for _, l := range n.Legs {
			if l.Quantity <= 0 || l.DTE >= watchDTE {
				continue
			}
			minDTE = math.Min(minDTE, float64(l.DTE))
			if l.Delta != nil && math.Abs(*l.Delta) >= c.pol.RunwayITMDeltaFloor {
				exempt = append(exempt, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: float64(l.DTE),
					Note: fmt.Sprintf("ITM exemption (|delta| %.2f)", math.Abs(*l.Delta))})
				continue
			}
			if l.HedgeListed && isPut(l.Right) {
				if c.overHedged {
					offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: float64(l.DTE),
						ImpactBase: math.Abs(l.MarketValueBase), Note: "hedge exemption suppressed: hedge band breached high"})
					worstStatus = worseRunway(worstStatus, l.DTE, actDTE)
					continue
				}
				exempt = append(exempt, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: float64(l.DTE), Note: "hedge leg"})
				continue
			}
			note := ""
			if l.Delta == nil {
				note = "delta unavailable — ITM exemption not assessable, flagged conservatively"
			}
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: float64(l.DTE),
				ImpactBase: math.Abs(l.MarketValueBase), Note: note})
			worstStatus = worseRunway(worstStatus, l.DTE, actDTE)
		}
	}
	sort.Slice(offenders, func(i, j int) bool { return offenders[i].Observed < offenders[j].Observed })
	row.Offenders = offenders
	row.Exempt = exempt
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	if len(offenders) == 0 {
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("No long option inside %d DTE without an exemption.", watchDTE)
		return row
	}
	row.Observed = new(minDTE)
	row.Status = worstStatus
	row.Evidence = fmt.Sprintf("%d long option line(s) inside %d DTE — roll or close before the final-week decay cliff.", len(offenders), watchDTE)
	return row
}

// earningsExempt: policy hedge symbols are index products with no earnings
// print; screening them through rules 6-8 would manufacture permanent
// unknowns instead of protecting anything.
func (c *ruleContext) earningsExempt(sym string) bool {
	return c.pol.IsHedgeSymbol(sym)
}

// earningsFor resolves usable earnings context for a name; ok=false means
// unknown/stale and the caller must degrade, not pass.
func (c *ruleContext) earningsFor(sym string) (EarningsInput, bool) {
	e, found := c.in.Earnings[sym]
	if !found || !e.Known || e.Stale {
		return e, false
	}
	return e, true
}

func (c *ruleContext) catalystCoverage() RuleRow {
	row := RuleRow{ID: RuleCatalystCoverage, Number: 6, Title: "Option outlives its catalyst"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	var offenders, unknowns []RuleOffender
	for _, n := range c.in.Names {
		if c.earningsExempt(n.Symbol) {
			continue
		}
		var otmLegs []LegInput
		for _, l := range n.Legs {
			if l.Quantity <= 0 {
				continue
			}
			if l.Underlying == nil {
				continue // OTM-ness unassessable; rule 4 already tracks uncomputables
			}
			if OptionIntrinsicPerShare(l.Right, *l.Underlying, l.Strike) > 0 {
				continue
			}
			otmLegs = append(otmLegs, l)
		}
		if len(otmLegs) == 0 {
			continue
		}
		e, ok := c.earningsFor(n.Symbol)
		if !ok {
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol,
				Note: fmt.Sprintf("%d OTM long leg(s), earnings date %s", len(otmLegs), earningsGapWord(e))})
			continue
		}
		for _, l := range otmLegs {
			if expiresBeforeCatalyst(l.Expiry, e) {
				offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
					Observed: float64(l.DTE), ImpactBase: math.Abs(l.MarketValueBase),
					Note: fmt.Sprintf("expires %s, earnings %s%s", l.Expiry.Format("Jan 2"), e.Date.Format("Jan 2"), estNote(e))})
			}
		}
	}
	sortOffenders(offenders)
	row.Offenders = offenders
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	switch {
	case len(offenders) > 0:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("%d OTM long option(s) die before their name's earnings — decay with no catalyst inside the option's life.", len(offenders))
		row.Offenders = append(offenders, unknowns...)
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "earnings_unknown"
		row.Offenders = unknowns
		row.Evidence = fmt.Sprintf("%d name(s) with OTM long options have no usable earnings date.", len(unknowns))
	default:
		row.Status = RuleStatusPass
		row.Evidence = "Every OTM long option outlives its name's next earnings (or no OTM longs held)."
	}
	return row
}

func (c *ruleContext) overwriteEarnings() RuleRow {
	row := RuleRow{ID: RuleOverwriteEarnings, Number: 7, Title: "Overwrite spans earnings"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	var offenders, unknowns []RuleOffender
	for _, n := range c.in.Names {
		if c.earningsExempt(n.Symbol) {
			continue
		}
		var shortCalls []LegInput
		for _, l := range n.Legs {
			if l.Quantity < 0 && isCall(l.Right) {
				shortCalls = append(shortCalls, l)
			}
		}
		if len(shortCalls) == 0 {
			continue
		}
		e, ok := c.earningsFor(n.Symbol)
		if !ok {
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol,
				Note: fmt.Sprintf("%d short call leg(s), earnings date %s", len(shortCalls), earningsGapWord(e))})
			continue
		}
		for _, l := range shortCalls {
			spans, ambiguous := spansEarningsGap(c.in.AsOf, l.Expiry, e)
			if !spans {
				continue
			}
			note := fmt.Sprintf("short through earnings %s%s", e.Date.Format("Jan 2"), estNote(e))
			if ambiguous {
				note += "; time-of-day unknown, flagged conservatively"
			}
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
				ImpactBase: math.Abs(l.MarketValueBase), Note: note})
		}
	}
	sortOffenders(offenders)
	row.Offenders = offenders
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	switch {
	case len(offenders) > 0:
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("%d short call line(s) span an earnings print — capped upside through the exact event that pays.", len(offenders))
		row.Offenders = append(offenders, unknowns...)
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "earnings_unknown"
		row.Offenders = unknowns
		row.Evidence = fmt.Sprintf("%d name(s) with short calls have no usable earnings date.", len(unknowns))
	default:
		row.Status = RuleStatusPass
		row.Evidence = "No short call spans a known earnings print."
	}
	return row
}

func (c *ruleContext) earningsSizeFreeze() RuleRow {
	row := RuleRow{ID: RuleEarningsSizeFreeze, Number: 8, Title: "At size before earnings", Unit: "sessions"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	freeze := c.pol.EarningsFreezeSessions
	row.Threshold = new(float64(freeze))
	var offenders, unknowns []RuleOffender
	for _, n := range c.in.Names {
		if c.earningsExempt(n.Symbol) {
			continue
		}
		if c.greeksGapMaterial(n) {
			continue // rule 1 already reports the gap
		}
		p := pct(math.Abs(n.ExposureBase), c.nlv)
		if p < c.pol.SingleNameWatchPct {
			continue
		}
		e, ok := c.earningsFor(n.Symbol)
		if !ok {
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol, Observed: round1(p),
				Note: "oversized with earnings date " + earningsGapWord(e)})
			continue
		}
		if e.SessionsUntil == nil {
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol, Observed: round1(p), Note: "session distance uncomputable"})
			continue
		}
		if *e.SessionsUntil >= 0 && *e.SessionsUntil <= freeze {
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: float64(*e.SessionsUntil),
				ImpactBase: math.Abs(n.ExposureBase),
				Note:       fmt.Sprintf("%.1f%% of NLV, earnings %s%s in %d session(s)", round1(p), e.Date.Format("Jan 2"), estNote(e), *e.SessionsUntil)})
		}
	}
	sortOffenders(offenders)
	row.Offenders = offenders
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	switch {
	case len(offenders) > 0:
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("%d oversized name(s) inside %d sessions of earnings — hold only what you'd buy fresh today.", len(offenders), freeze)
		row.Offenders = append(offenders, unknowns...)
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "earnings_unknown"
		row.Offenders = unknowns
		row.Evidence = fmt.Sprintf("%d oversized name(s) have no usable earnings date to check against.", len(unknowns))
	default:
		row.Status = RuleStatusPass
		row.Evidence = "No oversized name inside the pre-earnings freeze window."
	}
	return row
}

func (c *ruleContext) redOnGreen() RuleRow {
	row := RuleRow{ID: RuleRedOnGreen, Number: 9, Title: "Relative weakness on a green tape", Unit: "% day"}
	if !c.in.SessionOpen {
		row.Status = RuleStatusNotEvaluated
		row.Reason = "off_session"
		row.Evidence = "Tape rules evaluate during the US regular session only."
		return row
	}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	if c.in.SPYDayChangePct == nil {
		row.Status = RuleStatusUnknown
		row.Reason = "no_spy_tape"
		row.Evidence = "SPY day change unavailable — market tape not assessable."
		return row
	}
	spy := *c.in.SPYDayChangePct
	row.Threshold = new(c.pol.RedOnGreenNameDropPct)
	if spy < c.pol.RedOnGreenSPYUpPct {
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("Tape not green (SPY %+.1f%%) — relative-weakness screen idle.", spy)
		return row
	}
	var offenders []RuleOffender
	skipped := 0
	for _, n := range c.in.Names {
		if n.StockDayChangePct == nil {
			if !n.HasStockLeg {
				skipped++
			}
			continue
		}
		if *n.StockDayChangePct <= c.pol.RedOnGreenNameDropPct {
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: round1(*n.StockDayChangePct),
				ImpactBase: math.Abs(n.ExposureBase),
				Note:       fmt.Sprintf("%+.1f%% against SPY %+.1f%%", round1(*n.StockDayChangePct), round1(spy))})
		}
	}
	sortOffenders(offenders)
	row.Offenders = offenders
	if skipped > 0 {
		row.Notes = append(row.Notes, fmt.Sprintf("%d option-only name(s) have no stock-leg tape and were not screened.", skipped))
	}
	if len(offenders) > 0 {
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("%d name(s) red on a green tape — the market is naming your exits.", len(offenders))
		for _, o := range offenders {
			row.ImpactBase += o.ImpactBase
		}
	} else {
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("No held name red beyond %.1f%% on a green tape.", c.pol.RedOnGreenNameDropPct)
	}
	return row
}

func (c *ruleContext) winnerTrim() RuleRow {
	row := RuleRow{ID: RuleWinnerTrim, Number: 10, Title: "Trim winners into strength", Unit: "% day"}
	if !c.in.SessionOpen {
		row.Status = RuleStatusNotEvaluated
		row.Reason = "off_session"
		row.Evidence = "Tape rules evaluate during the US regular session only."
		return row
	}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	row.Threshold = new(c.pol.WinnerTrimDayUpPct)
	var offenders []RuleOffender
	for _, n := range c.in.Names {
		if n.StockDayChangePct == nil {
			continue
		}
		expo := pct(math.Abs(n.ExposureBase), c.nlv)
		if *n.StockDayChangePct >= c.pol.WinnerTrimDayUpPct && expo >= c.pol.WinnerTrimMinExpoPct {
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: round1(*n.StockDayChangePct),
				ImpactBase: math.Abs(n.ExposureBase),
				Note:       fmt.Sprintf("+%.1f%% today at %.1f%% of NLV — someone is paying up", round1(*n.StockDayChangePct), round1(expo))})
		}
	}
	sortOffenders(offenders)
	row.Offenders = offenders
	if len(offenders) > 0 {
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("%d oversized name(s) up hard today — sell strength while the bid is there.", len(offenders))
		for _, o := range offenders {
			row.ImpactBase += o.ImpactBase
		}
	} else {
		row.Status = RuleStatusPass
		row.Evidence = "No oversized name up past the trim trigger today."
	}
	return row
}

func (c *ruleContext) greenDayAction(rows []RuleRow) RuleRow {
	row := RuleRow{ID: RuleGreenDayAction, Number: 11, Title: "Green day is an execution day"}
	if !c.in.Account.Healthy || c.in.DailyPnLBase == nil {
		row.Status = RuleStatusNotEvaluated
		row.Reason = "pnl_unavailable"
		row.Evidence = "Daily P&L unavailable — nudge idle."
		return row
	}
	actOpen := 0
	for _, r := range rows {
		if r.Status == RuleStatusAct {
			actOpen++
		}
	}
	if *c.in.DailyPnLBase > 0 && actOpen > 0 {
		row.Status = RuleStatusInfo
		row.Evidence = fmt.Sprintf("Portfolio green today with %d act-severity rule(s) open — run the de-risk list now, not on a hypothetical better day.", actOpen)
	} else {
		row.Status = RuleStatusPass
		row.Evidence = "No green-day nudge: either the tape is red or nothing is at act severity."
	}
	return row
}

func (c *ruleContext) hedgeIntegrity() RuleRow {
	row := RuleRow{ID: RuleHedgeIntegrity, Number: 12, Title: "Hedge sized to the book", Unit: "% gross long"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	grossLong, hedgeShort := 0.0, 0.0
	var gaps []RuleOffender
	var hedgeLegs []RuleOffender
	for _, n := range c.in.Names {
		if c.greeksGapMaterial(n) {
			gaps = append(gaps, RuleOffender{Symbol: n.Symbol, Note: "delta unavailable on material legs"})
			continue
		}
		if n.ExposureBase > 0 {
			grossLong += n.ExposureBase
		}
		for _, l := range n.Legs {
			if l.HedgeListed && isPut(l.Right) && l.Quantity > 0 && l.Delta != nil && l.Underlying != nil {
				short := math.Abs(*l.Delta * l.Quantity * l.Multiplier * *l.Underlying)
				hedgeShort += short
				hedgeLegs = append(hedgeLegs, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: round1(short), Note: "classified hedge"})
			}
		}
	}
	if len(gaps) > 0 {
		row.Status = RuleStatusUnknown
		row.Reason = "greeks_gap"
		row.Offenders = gaps
		row.Evidence = "Delta gaps make the hedge ratio untrustworthy."
		return row
	}
	if grossLong <= 0 {
		row.Status = RuleStatusNotEvaluated
		row.Reason = "no_long_book"
		row.Evidence = "No net-long exposure to hedge."
		return row
	}
	ratio := pct(hedgeShort, grossLong)
	row.Observed = new(round1(ratio))
	row.Threshold = new(c.pol.HedgeBandMinPct)
	row.Exempt = hedgeLegs
	minB, maxB := c.pol.HedgeBandMinPct, c.pol.HedgeBandMaxPct
	switch {
	case ratio < minB:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("Hedge short-delta covers %.1f%% of gross long exposure — under the %.0f–%.0f%% band; the book is barer than it feels.", round1(ratio), minB, maxB)
	case ratio > maxB:
		row.Status = RuleStatusWatch
		c.overHedged = true
		row.Evidence = fmt.Sprintf("Hedge short-delta at %.1f%% of gross long exposure — over the %.0f–%.0f%% band; oversized hedges are directional bets in disguise.", round1(ratio), minB, maxB)
	default:
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("Hedge at %.1f%% of gross long exposure, inside the %.0f–%.0f%% band.", round1(ratio), minB, maxB)
	}
	return row
}

func (c *ruleContext) greeksGapMaterial(n NameInput) bool {
	return c.hasNLV && pct(n.GreeksGapNotionalBase, c.nlv) >= c.pol.GreeksGapFloorPctNLV
}

// expiresBeforeCatalyst: rule 6 — an OTM long that dies before its name's
// earnings gap. AMC on expiry day: the option expires at the close, the gap
// happens after → dies before the catalyst → true. BMO on expiry day: the
// gap happened pre-open → option lived through it → false. Unknown time of
// day on expiry day is conservative (true, disclosed by caller).
func expiresBeforeCatalyst(expiry time.Time, e EarningsInput) bool {
	ed, xd := dateOnly(e.Date), dateOnly(expiry)
	if xd.Before(ed) {
		return true
	}
	if xd.Equal(ed) {
		switch e.TimeOfDay {
		case "bmo":
			return false
		default: // amc or unknown → gap is after the close
			return true
		}
	}
	return false
}

// spansEarningsGap: rule 7 — a short call alive through the earnings gap.
// ambiguous=true when the verdict hinged on an unknown time-of-day.
func spansEarningsGap(now, expiry time.Time, e EarningsInput) (spans, ambiguous bool) {
	ed, xd, today := dateOnly(e.Date), dateOnly(expiry), dateOnly(now)
	if ed.Before(today) || xd.Before(ed) {
		return false, false
	}
	if xd.Equal(ed) {
		switch e.TimeOfDay {
		case "amc":
			return false, false // dies at the close, gap is after
		case "bmo":
			return true, false
		default:
			return true, true // conservative
		}
	}
	return true, false
}

func rankRows(rows []RuleRow) []int {
	idx := make([]int, 0, len(rows))
	for i := range rows {
		idx = append(idx, i)
	}
	weight := map[string]int{RuleStatusAct: 5, RuleStatusWatch: 4, RuleStatusUnknown: 3, RuleStatusInfo: 2, RuleStatusNotEvaluated: 1, RuleStatusPass: 0}
	sort.SliceStable(idx, func(a, b int) bool {
		ra, rb := rows[idx[a]], rows[idx[b]]
		if weight[ra.Status] != weight[rb.Status] {
			return weight[ra.Status] > weight[rb.Status]
		}
		if ra.ImpactBase != rb.ImpactBase {
			return ra.ImpactBase > rb.ImpactBase
		}
		return ra.Number < rb.Number
	})
	return idx
}

func worseRunway(current string, dte, actDTE int) string {
	if dte < actDTE {
		return RuleStatusAct
	}
	if current != RuleStatusAct {
		return RuleStatusWatch
	}
	return current
}

func sortOffenders(o []RuleOffender) {
	sort.SliceStable(o, func(i, j int) bool {
		if o[i].ImpactBase != o[j].ImpactBase {
			return o[i].ImpactBase > o[j].ImpactBase
		}
		return math.Abs(o[i].Observed) > math.Abs(o[j].Observed)
	})
}

func earningsGapWord(e EarningsInput) string {
	if e.Known && e.Stale {
		return "stale"
	}
	return "unknown"
}

func estNote(e EarningsInput) string {
	if e.Estimated {
		return " (estimated)"
	}
	return ""
}

func isPut(right string) bool  { return right == "P" || right == "PUT" || right == "p" }
func isCall(right string) bool { return right == "C" || right == "CALL" || right == "c" }

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func pct(v, base float64) float64 {
	if base == 0 {
		return 0
	}
	return v / base * 100
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
