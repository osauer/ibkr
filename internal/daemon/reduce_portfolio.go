package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// maxBasketLegs bounds how many orders one portfolio sweep can fan out. A book
// with more eligible positions than this returns a basket-level blocker and
// places nothing — a sanity backstop against a runaway one-tap action.
const maxBasketLegs = 25

// reduceBasketDedupeTTL is how long a submit RequestRef is remembered so a
// double-tap or client retry replays the prior result instead of placing again.
const reduceBasketDedupeTTL = 90 * time.Second

type reduceBasketDedupeEntry struct {
	at     time.Time
	result *rpc.TradeProposalReducePortfolioResult
}

// reduceCandidate is one eligible position in a sweep. hedgeExcluded marks a
// protective short that the current protect_hedges setting carves out.
type reduceCandidate struct {
	row           rpc.PositionView
	hedgeExcluded bool
}

// reduceBasketCandidates enumerates the positions a sweep touches, in portfolio
// order, and returns how many are actionable (will get a broker order). Rows
// outside the reduce scope (short options) are dropped entirely; protective
// shorts become hedge-excluded candidates when protectHedges is set.
func reduceBasketCandidates(pos *rpc.PositionsResult, protectHedges bool) (cands []reduceCandidate, actionable int) {
	if pos == nil {
		return nil, 0
	}
	rows := make([]rpc.PositionView, 0, len(pos.Stocks)+len(pos.Options))
	rows = append(rows, pos.Stocks...)
	rows = append(rows, pos.Options...)
	for _, row := range rows {
		if !reduceEligible(row) {
			continue
		}
		// Skip defunct rows the enricher flagged stale (e.g. a delisted stock
		// with zero mark): they are position truth but not tradable, so a sweep
		// should not enumerate them as perpetually-blocked legs. Stale is the
		// deliberate signal — do not infer defunct from a zero/absent mark, which
		// a live row can legitimately carry off-hours.
		if row.Stale {
			continue
		}
		he := isProtectiveShort(row) && protectHedges
		cands = append(cands, reduceCandidate{row: row, hedgeExcluded: he})
		if !he {
			actionable++
		}
	}
	return cands, actionable
}

func perLegReduceParams(p rpc.TradeProposalReducePortfolioParams) rpc.TradeProposalReduceParams {
	return rpc.TradeProposalReduceParams{Percent: p.Percent, IncludeHedges: !p.ProtectHedges, TimeoutMs: p.TimeoutMs}
}

func reduceLegBase(row rpc.PositionView) rpc.TradeProposalReduceLeg {
	return rpc.TradeProposalReduceLeg{
		ConID:            row.ConID,
		Symbol:           strings.ToUpper(strings.TrimSpace(row.Symbol)),
		SecType:          positionWireSecType(row.SecType),
		PositionQuantity: row.Quantity,
		HedgeLike:        isProtectiveShort(row),
	}
}

func hedgeExcludedLeg(row rpc.PositionView) rpc.TradeProposalReduceLeg {
	leg := reduceLegBase(row)
	leg.Blockers = []rpc.TradingBlocker{{Code: "hedge_excluded", Message: fmt.Sprintf("%s is a protective short (hedge); excluded from the sweep", leg.Symbol), Action: "Uncheck Protect hedges to include it."}}
	return leg
}

func legHedgeExcluded(leg rpc.TradeProposalReduceLeg) bool {
	return len(leg.Blockers) > 0 && leg.Blockers[0].Code == "hedge_excluded"
}

// legContext bounds one leg's quote/WhatIf/place so a single stuck leg cannot
// drain the basket budget; it never exceeds the parent deadline.
func legContext(parent context.Context, timeoutMs int) (context.Context, context.CancelFunc) {
	budget := max(time.Duration(timeoutMs)*time.Millisecond, 15*time.Second)
	budget = min(budget, 30*time.Second)
	if dl, ok := parent.Deadline(); ok {
		budget = min(budget, time.Until(dl)-500*time.Millisecond)
	}
	if budget <= 0 {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, cancel
	}
	return context.WithTimeout(parent, budget)
}

// reduceLegPrepare sizes and previews one actionable leg through the gated order
// path. It returns the leg (with disclosure + per-leg blockers) and, when the
// leg is submit-eligible, the raw preview so the caller can redeem its token. A
// nil preview means the leg is terminal (blocked/not eligible) — never placed.
func (s *Server) reduceLegPrepare(ctx context.Context, row rpc.PositionView, p rpc.TradeProposalReduceParams) (rpc.TradeProposalReduceLeg, *rpc.OrderPreviewResult) {
	leg := reduceLegBase(row)
	prep, blockers := prepareReduceForRow(row, p)
	if len(blockers) > 0 {
		leg.Blockers = blockers
		return leg, nil
	}
	leg.Action = prep.action
	leg.ReduceQuantity = prep.qty
	legCtx, cancel := legContext(ctx, p.TimeoutMs)
	defer cancel()
	preview, err := s.previewOrder(legCtx, prep.params)
	if err != nil {
		leg.Blockers = []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error(), Action: "Refresh positions and preview again."}}
		return leg, nil
	}
	leg.Notional = preview.Notional
	leg.NotionalCurrency = preview.Draft.Contract.Currency
	if row.FXRate != nil && *row.FXRate > 0 {
		base := preview.Notional * *row.FXRate
		leg.NotionalBase = &base
	}
	leg.PreviewTokenID = preview.PreviewTokenID
	leg.SubmitEligible = preview.SubmitEligible
	leg.Preview = sanitizeProposalPreviewForProposal(preview, rpc.TradeProposal{})
	if b := reduceCloseReduceBlockers(preview); len(b) > 0 {
		leg.Blockers = b
		return leg, nil
	}
	if !preview.SubmitEligible {
		leg.Blockers = previewNotSubmitEligibleBlockers()
		return leg, nil
	}
	return leg, preview
}

func (s *Server) reduceLegSubmit(ctx context.Context, row rpc.PositionView, p rpc.TradeProposalReduceParams, origin string) rpc.TradeProposalReduceLeg {
	leg, preview := s.reduceLegPrepare(ctx, row, p)
	if preview == nil {
		return leg // terminal: blocked or not submit-eligible; nothing placed
	}
	legCtx, cancel := legContext(ctx, p.TimeoutMs)
	defer cancel()
	place, err := s.proposalPlaceOrder(legCtx, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, TimeoutMs: p.TimeoutMs, Origin: origin})
	if err != nil {
		leg.Blockers = []rpc.TradingBlocker{{Code: "submit_failed", Message: err.Error(), Action: "Reconcile this leg before retrying the sweep."}}
		return leg
	}
	leg.Placed = place.Accepted
	leg.Place = place
	leg.OrderRef = place.OrderRef
	leg.Message = place.Message
	return leg
}

// aggregateBasket fills the counts, base-currency total, and Accepted verdict.
// submit=true counts placed legs as eligible; preview counts submit-eligible
// legs. Hedge-excluded legs are disclosed but never count as blocked.
func aggregateBasket(res *rpc.TradeProposalReducePortfolioResult, submit bool) {
	res.LegCount = len(res.Legs)
	eligible, blocked, hedged := 0, 0, 0
	var total float64
	fxMissing := false
	for _, leg := range res.Legs {
		if legHedgeExcluded(leg) {
			hedged++
			continue
		}
		ok := leg.SubmitEligible
		if submit {
			ok = leg.Placed
		}
		if ok && len(leg.Blockers) == 0 {
			eligible++
			if leg.NotionalBase != nil {
				total += *leg.NotionalBase
			} else {
				fxMissing = true
			}
		} else {
			blocked++
		}
	}
	res.EligibleCount = eligible
	res.BlockedCount = blocked
	res.HedgeExcludedCount = hedged
	if eligible > 0 {
		if res.BaseCurrency != "" && !fxMissing {
			res.TotalNotional = total
		} else {
			res.FXIncomplete = true
			if res.BaseCurrency != "" {
				res.TotalNotional = total
			}
		}
	}
	res.Accepted = eligible > 0 && blocked == 0
	verb := "eligible"
	if submit {
		verb = "placed"
	}
	res.Message = fmt.Sprintf("%d %s · %d blocked · %d hedge-excluded of %d legs", eligible, verb, blocked, hedged, res.LegCount)
}

func (s *Server) reducePortfolioPreview(ctx context.Context, p rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	now := s.reduceClock()
	res := &rpc.TradeProposalReducePortfolioResult{Percent: p.Percent, ProtectHedges: p.ProtectHedges, AsOf: now}
	if p.Percent <= 0 || p.Percent > 100 {
		res.Blockers = []rpc.TradingBlocker{{Code: "bad_request", Message: fmt.Sprintf("percent %d must be between 1 and 100", p.Percent), Action: "Choose 25, 50, 75, or 100."}}
		return res, nil
	}
	pos, err := s.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		return nil, err
	}
	cands, actionable := reduceBasketCandidates(pos, p.ProtectHedges)
	if actionable > maxBasketLegs {
		res.Blockers = []rpc.TradingBlocker{{Code: "too_many_legs", Message: fmt.Sprintf("portfolio sweep would place %d orders, above the %d-leg cap", actionable, maxBasketLegs), Action: "Trim individual holdings instead."}}
		return res, nil
	}
	if pos.Portfolio != nil {
		res.BaseCurrency = pos.Portfolio.BaseCurrency
	}
	legP := perLegReduceParams(p)
	for _, c := range cands {
		if c.hedgeExcluded {
			res.Legs = append(res.Legs, hedgeExcludedLeg(c.row))
			continue
		}
		leg, _ := s.reduceLegPrepare(ctx, c.row, legP)
		res.Legs = append(res.Legs, leg)
	}
	aggregateBasket(res, false)
	return res, nil
}

func (s *Server) reducePortfolioSubmit(ctx context.Context, p rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	now := s.reduceClock()
	res := &rpc.TradeProposalReducePortfolioResult{Percent: p.Percent, ProtectHedges: p.ProtectHedges, AsOf: now}
	if p.Percent <= 0 || p.Percent > 100 {
		res.Blockers = []rpc.TradingBlocker{{Code: "bad_request", Message: fmt.Sprintf("percent %d must be between 1 and 100", p.Percent), Action: "Choose 25, 50, 75, or 100."}}
		return res, nil
	}
	if cached, ok := s.reduceBasketReplay(p.RequestRef); ok {
		return cached, nil
	}
	// Write gate once: a frozen/non-writable origin touches zero legs. Not
	// cached, so a retry after unfreezing re-attempts rather than replaying.
	if blockers := s.proposalSubmitWriteBlockers(p.Origin); len(blockers) > 0 {
		res.Blockers = blockers
		return res, nil
	}
	pos, err := s.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		return nil, err
	}
	cands, actionable := reduceBasketCandidates(pos, p.ProtectHedges)
	if actionable > maxBasketLegs {
		res.Blockers = []rpc.TradingBlocker{{Code: "too_many_legs", Message: fmt.Sprintf("portfolio sweep would place %d orders, above the %d-leg cap", actionable, maxBasketLegs), Action: "Trim individual holdings instead."}}
		return res, nil
	}
	if pos.Portfolio != nil {
		res.BaseCurrency = pos.Portfolio.BaseCurrency
	}
	legP := perLegReduceParams(p)
	for _, c := range cands {
		if c.hedgeExcluded {
			res.Legs = append(res.Legs, hedgeExcludedLeg(c.row))
			continue
		}
		res.Legs = append(res.Legs, s.reduceLegSubmit(ctx, c.row, legP, p.Origin))
	}
	aggregateBasket(res, true)
	s.reduceBasketStore(p.RequestRef, res)
	return res, nil
}

// reduceBasketReplay returns a prior submit result for a repeated RequestRef
// (marked Replayed), sweeping expired entries on the way.
func (s *Server) reduceBasketReplay(ref string) (*rpc.TradeProposalReducePortfolioResult, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, false
	}
	s.reduceBasketMu.Lock()
	defer s.reduceBasketMu.Unlock()
	now := s.reduceClock()
	for k, e := range s.reduceBasketDedupe {
		if now.Sub(e.at) > reduceBasketDedupeTTL {
			delete(s.reduceBasketDedupe, k)
		}
	}
	e, ok := s.reduceBasketDedupe[ref]
	if !ok {
		return nil, false
	}
	clone := *e.result
	clone.Replayed = true
	return &clone, true
}

func (s *Server) reduceBasketStore(ref string, res *rpc.TradeProposalReducePortfolioResult) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return
	}
	s.reduceBasketMu.Lock()
	defer s.reduceBasketMu.Unlock()
	if s.reduceBasketDedupe == nil {
		s.reduceBasketDedupe = make(map[string]reduceBasketDedupeEntry)
	}
	s.reduceBasketDedupe[ref] = reduceBasketDedupeEntry{at: s.reduceClock(), result: res}
}

func (s *Server) handleTradeProposalsReducePortfolioPreview(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalReducePortfolioResult, error) {
	var p rpc.TradeProposalReducePortfolioParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.reducePortfolioPreview(ctx, p)
}

func (s *Server) handleTradeProposalsReducePortfolioSubmit(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalReducePortfolioResult, error) {
	var p rpc.TradeProposalReducePortfolioParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	// Serialize the whole basket against every other broker writer (R1): one
	// lock around all legs, matching handleTradeProposalsSubmit's discipline.
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.reducePortfolioSubmit(ctx, p)
}
