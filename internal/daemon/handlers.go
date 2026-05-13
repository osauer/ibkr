package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/cache"
	"github.com/osauer/ibkr/internal/rpc"
)

// handleAccountSummary issues a one-shot reqAccountSummary and converts the
// result into the wire shape exposed to the CLI.
func (s *Server) handleAccountSummary(ctx context.Context) (*rpc.AccountResult, error) {
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	raw, err := c.RequestAccountSummary(ctx, 8*time.Second)
	if err != nil {
		return nil, err
	}
	res := &rpc.AccountResult{
		AccountID:    raw.AccountID,
		BaseCurrency: raw.Currency,
		AsOf:         raw.AsOf,
		DataType:     "live",
	}
	if raw.NetLiquidation != nil {
		res.NetLiquidation = *raw.NetLiquidation
	}
	if raw.BuyingPower != nil {
		res.BuyingPower = *raw.BuyingPower
	}
	if raw.AvailableFunds != nil {
		res.AvailableFunds = *raw.AvailableFunds
	}
	if raw.ExcessLiquidity != nil {
		res.ExcessLiquidity = *raw.ExcessLiquidity
	}
	if raw.TotalCashValue != nil {
		res.TotalCash = *raw.TotalCashValue
	}
	if raw.MaintenanceMargin != nil {
		res.MaintenanceMargin = *raw.MaintenanceMargin
	}
	if raw.InitMarginReq != nil {
		res.InitialMargin = *raw.InitMarginReq
	}
	res.CurrencyExposure = buildCurrencyExposure(raw.CurrencyLedger, res.BaseCurrency)
	return res, nil
}

// buildCurrencyExposure flattens RawAccountSummary.CurrencyLedger into the
// wire-shape CurrencyExposure rows, sorted by currency for stable output.
// Drops the row whose currency matches the account base (it duplicates
// the top-level totals and exposure is by definition "non-base") and
// also drops rows whose ExchangeRate is exactly 1.0 as a defense-in-
// depth fallback when the caller didn't supply a base.
func buildCurrencyExposure(ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) []rpc.CurrencyExposure {
	if len(ledger) == 0 {
		return nil
	}
	baseCcy = normCcy(baseCcy)
	out := make([]rpc.CurrencyExposure, 0, len(ledger))
	for ccy, row := range ledger {
		upper := normCcy(ccy)
		if upper == baseCcy {
			continue
		}
		// ExchangeRate==1 fallback for accounts where the base
		// currency couldn't be resolved upstream.
		if baseCcy == "" && row.ExchangeRate == 1.0 {
			continue
		}
		nlBase := row.NetLiquidationByCurrency * row.ExchangeRate
		out = append(out, rpc.CurrencyExposure{
			Currency:             upper,
			NetLiquidationCcy:    row.NetLiquidationByCurrency,
			CashCcy:              row.CashBalance,
			StockMarketValueCcy:  row.StockMarketValue,
			OptionMarketValueCcy: row.OptionMarketValue,
			UnrealizedPnLCcy:     row.UnrealizedPnL,
			RealizedPnLCcy:       row.RealizedPnL,
			ExchangeRate:         row.ExchangeRate,
			NetLiquidationBase:   nlBase,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// handlePositionsList fetches all positions, splits stocks vs options, and
// applies the optional symbol/type filter.
func (s *Server) handlePositionsList(ctx context.Context, req *rpc.Request) (*rpc.PositionsResult, error) {
	var p rpc.PositionsListParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	positions, err := c.GetCachedPositions()
	if err != nil {
		return nil, err
	}

	res := &rpc.PositionsResult{
		AsOf:     time.Now(),
		DataType: "live",
		Stocks:   []rpc.PositionView{},
		Options:  []rpc.PositionView{},
	}
	wantSym := normSym(p.Symbol)
	wantType := strings.ToLower(strings.TrimSpace(p.Type))

	for _, pos := range positions {
		if pos == nil {
			continue
		}
		isOpt := pos.Asset.AssetType == ibkrlib.AssetTypeOption
		if wantType == "stk" && isOpt {
			continue
		}
		if wantType == "opt" && !isOpt {
			continue
		}
		baseSym := pos.Asset.Symbol
		// For options the synthetic symbol carries expiry/strike — strip back to underlying for filter.
		under := baseSym
		if i := strings.IndexByte(baseSym, '_'); i > 0 {
			under = baseSym[:i]
		}
		if wantSym != "" && wantSym != strings.ToUpper(under) && wantSym != strings.ToUpper(baseSym) {
			continue
		}
		view := rpc.PositionView{
			Symbol:        baseSym,
			SecType:       string(pos.Asset.AssetType),
			Exchange:      pos.Asset.Exchange,
			Currency:      pos.Asset.Currency,
			Quantity:      pos.Quantity,
			Multiplier:    maxInt(pos.Asset.Multiplier, 1),
			AvgCost:       pos.EntryPrice,
			Mark:          pos.CurrentPrice,
			MarketValue:   pos.CurrentPrice * pos.Quantity * float64(maxInt(pos.Asset.Multiplier, 1)),
			UnrealizedPnL: pos.UnrealizedPnL,
			RealizedPnL:   pos.RealizedPnL,
		}
		if isOpt {
			view.Symbol = under
			parts := strings.Split(baseSym, "_")
			if len(parts) == 3 {
				view.Expiry = parts[1]
				if len(parts[2]) > 0 {
					view.Right = string(parts[2][0])
					var strike float64
					_, _ = fmt.Sscanf(parts[2][1:], "%f", &strike)
					view.Strike = strike
				}
			}
			res.Options = append(res.Options, view)
		} else {
			res.Stocks = append(res.Stocks, view)
		}
	}
	sort.SliceStable(res.Stocks, func(i, j int) bool { return res.Stocks[i].Symbol < res.Stocks[j].Symbol })
	sort.SliceStable(res.Options, func(i, j int) bool {
		if res.Options[i].Symbol == res.Options[j].Symbol {
			if res.Options[i].Expiry == res.Options[j].Expiry {
				return res.Options[i].Strike < res.Options[j].Strike
			}
			return res.Options[i].Expiry < res.Options[j].Expiry
		}
		return res.Options[i].Symbol < res.Options[j].Symbol
	})

	// Pre-warm prev-close cache for the held stock underlyings, then
	// fill DayChange/DayChangePct on each row. Bounded fan-out keeps the
	// gateway's market-data slot churn under control even for accounts
	// with many positions; the cache makes subsequent calls instant.
	s.prewarmPrevCloses(ctx, c, res.Stocks)
	s.fillDailyChange(res.Stocks)
	// Options group with their underlying so stock prev close feeds the
	// option's underlying field too — useful as a contextual anchor even
	// though we don't compute per-option DayChange yet.
	s.fillOptionUnderlyingPrevClose(res.Options)
	// Greeks: brief subscribe to each option leg, harvest model-
	// computation tick within budget, fill per-leg Delta/Gamma/Theta/
	// Vega. Same bounded fan-out and TTL-cached pattern as prev close.
	s.prewarmOptionGreeks(ctx, c, res.Options)
	s.fillOptionGreeks(c, res.Options)

	// FX decoration: read the per-currency snapshot maintained by the
	// daemon's reqAccountUpdates subscription (no extra gateway round
	// trip) and fill MarketValueCcy / FXRate on each non-base position.
	// Empty map → no FX data yet (pre-handshake or single-currency
	// account); leaves all pointers nil.
	ledger := c.CurrencyLedgerSnapshot()
	baseCcy := normCcy(s.cachedBaseCurrency())
	fillFXRates(res.Stocks, ledger, baseCcy)
	fillFXRates(res.Options, ledger, baseCcy)

	res.ByUnderlying = groupByUnderlying(res.Stocks, res.Options)
	res.Portfolio = buildPortfolioAggregates(res.Stocks, res.Options)
	addFXSensitivity(res.Portfolio, ledger, baseCcy)
	return res, nil
}

// cachedBaseCurrency returns the account's base currency, derived from
// the gateway's continuously-fresh accountSummary map. Empty string when
// unknown; callers fall back to treating every currency as non-base,
// which surfaces an exposure row but no sensitivity (the safer "I don't
// know yet" answer).
func (s *Server) cachedBaseCurrency() string {
	c := s.gatewayConnector()
	if c == nil {
		return ""
	}
	return baseCurrencyFromRaw(c.AccountSummaryRaw())
}

// baseCurrencyFromRaw resolves the account's base currency by scanning
// the raw accountSummary map. The bare "Currency" tag IBKR emits carries
// the literal string "BASE" (the pseudo-currency name, not the actual
// base currency), so it is useless on its own — we only return it when
// the value is something other than "BASE". The reliable signal is the
// `$LEDGER:ALL` subscription's `ExchangeRate_<ccy>` rows: the currency
// whose rate is ~1.0 is the base by definition. A small epsilon tolerates
// the gateway's occasional float drift (e.g. 1.0000000001).
func baseCurrencyFromRaw(raw map[string]string) string {
	if v, ok := raw["Currency"]; ok {
		ccy := normCcy(v)
		if ccy != "" && ccy != "BASE" {
			return ccy
		}
	}
	const erPrefix = "ExchangeRate_"
	const eps = 1e-6
	for k, v := range raw {
		ccy, ok := strings.CutPrefix(k, erPrefix)
		if !ok {
			continue
		}
		ccy = normCcy(ccy)
		if ccy == "" || ccy == "BASE" {
			continue
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || math.Abs(rate-1.0) > eps {
			continue
		}
		return ccy
	}
	return ""
}

// fillFXRates copies the per-currency ExchangeRate into each non-base
// position's FXRate field and computes MarketValueCcy when we know the
// rate (MarketValueCcy = MarketValue / ExchangeRate, since IBKR's
// MarketValue is in base currency for option/stock rows reported via
// reqAccountUpdates). Same-currency rows (Currency == baseCcy) keep
// both pointers nil — exposure surfacing applies only to non-base.
func fillFXRates(rows []rpc.PositionView, ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) {
	for i := range rows {
		p := &rows[i]
		ccy := normCcy(p.Currency)
		if ccy == "" || ccy == baseCcy {
			continue
		}
		entry, ok := ledger[ccy]
		if !ok || entry.ExchangeRate <= 0 {
			continue
		}
		fx := entry.ExchangeRate
		p.FXRate = &fx
		// Position.MarketValue, populated by msgPortfolioValue, is the
		// contract-currency market value (qty * marketPrice * multiplier).
		// So MarketValueCcy IS p.MarketValue — we just label it
		// explicitly so JSON consumers don't have to infer.
		mvc := p.MarketValue
		p.MarketValueCcy = &mvc
	}
}

// addFXSensitivity computes the portfolio-wide 1%-FX-move sensitivity
// in base currency: Σ (non-base NetLiquidation × ExchangeRate × 0.01).
// Skips when the ledger is empty (single-currency book or pre-handshake)
// — never fabricates a zero when the answer is "unknown".
func addFXSensitivity(p *rpc.PositionsPortfolio, ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) {
	if p == nil || len(ledger) == 0 {
		return
	}
	var sens float64
	any := false
	for ccy, row := range ledger {
		if strings.EqualFold(ccy, baseCcy) {
			continue
		}
		if row.NetLiquidationByCurrency == 0 || row.ExchangeRate <= 0 {
			continue
		}
		sens += row.NetLiquidationByCurrency * row.ExchangeRate * 0.01
		any = true
	}
	if !any {
		return
	}
	v := sens
	p.FXSensitivityPerPct = &v
	p.FXBaseCurrency = baseCcy
}

// prewarmPrevCloses dispatches up to positionsPrewarmWorkers concurrent
// brief subscribes to fetch the previous regular-session close for any
// held stock underlying not already cached. Negative-caches a zero on
// timeout / dead stream so a second positions call within the TTL
// doesn't re-poll a known-empty source.
func (s *Server) prewarmPrevCloses(ctx context.Context, c *ibkrlib.Connector, stocks []rpc.PositionView) {
	if s.prevCloses == nil || c == nil || len(stocks) == 0 {
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	var jobs []string
	for _, p := range stocks {
		sym := normSym(p.Symbol)
		if sym == "" || seen[sym] {
			continue
		}
		seen[sym] = true
		if _, ok := s.prevCloses.get(sym, now); ok {
			continue
		}
		jobs = append(jobs, sym)
	}
	runBounded(jobs, positionsPrewarmWorkers, func(sym string) {
		if ctx.Err() != nil {
			return
		}
		pc := briefSnapshotClose(ctx, c, sym, 1*time.Second)
		s.prevCloses.put(sym, prevCloseEntry{value: pc}, time.Now())
	})
}

// fillDailyChange populates PrevClose / DayChange / DayChangePct on each
// stock row from the cache. Rows whose underlying has no positive cached
// prev close (cache miss, dead stream) are left untouched — pointers stay
// nil and the renderer shows an em-dash.
func (s *Server) fillDailyChange(stocks []rpc.PositionView) {
	if s.prevCloses == nil {
		return
	}
	now := time.Now()
	for i := range stocks {
		p := &stocks[i]
		sym := normSym(p.Symbol)
		e, ok := s.prevCloses.get(sym, now)
		if !ok || e.value <= 0 {
			continue
		}
		v := e.value
		p.PrevClose = &v
		p.DayChange, p.DayChangePct = computePositionDayChange(p.Mark, e.value)
	}
}

// fillOptionUnderlyingPrevClose copies the cached underlying prev close
// onto each option leg's PrevClose field — useful as a contextual anchor
// when the renderer groups by underlying. The option's own DayChange
// stays nil because we don't track contract-level prev close.
func (s *Server) fillOptionUnderlyingPrevClose(options []rpc.PositionView) {
	if s.prevCloses == nil {
		return
	}
	now := time.Now()
	for i := range options {
		p := &options[i]
		under := normSym(p.Symbol)
		e, ok := s.prevCloses.get(under, now)
		if !ok || e.value <= 0 {
			continue
		}
		v := e.value
		p.PrevClose = &v
	}
}

// positionsPrewarmWorkers bounds the per-positions-call market-data
// fan-out. 4 mirrors handleChainFetch — the gateway throttles
// aggressive subscribe churn beyond that.
const positionsPrewarmWorkers = 4

// optionGreeksBudget is the per-leg deadline for capturing the IBKR
// model-computation tick. Long enough for the gateway's typical 200-
// 800 ms latency between subscribe and the first tick-21 row, short
// enough that a 15-leg book cold-fetch stays under ~6 s wall time at
// 4-way parallelism. Negative-cache means subsequent calls within the
// TTL pay zero for legs that already returned empty.
const optionGreeksBudget = 2500 * time.Millisecond

// prewarmOptionGreeks dispatches up to positionsPrewarmWorkers concurrent
// brief subscribes for each option leg, harvests the model-computation
// Greeks (msg 21 tickType 13), then unsubscribes. Caches into s.greeks
// keyed by the OPRA-style option key. Skips legs whose Greeks are
// already cached (positive or negative entry) within the TTL.
func (s *Server) prewarmOptionGreeks(ctx context.Context, c *ibkrlib.Connector, options []rpc.PositionView) {
	if s.greeks == nil || c == nil || len(options) == 0 {
		return
	}
	now := time.Now()
	type job struct {
		key    string
		under  string
		expiry string
		strike float64
		right  string
	}
	var jobs []job
	seen := map[string]bool{}
	for _, p := range options {
		key := optionGreeksKey(p)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := s.greeks.get(key, now); ok {
			continue
		}
		jobs = append(jobs, job{
			key:    key,
			under:  strings.ToUpper(p.Symbol),
			expiry: p.Expiry,
			strike: p.Strike,
			right:  p.Right,
		})
	}
	runBounded(jobs, positionsPrewarmWorkers, func(j job) {
		if ctx.Err() != nil {
			return
		}
		entry := captureOptionGreeks(ctx, c, j.under, j.expiry, j.strike, j.right, optionGreeksBudget)
		s.greeks.put(j.key, entry, time.Now())
	})
}

// captureOptionGreeks runs one option subscribe → poll → unsubscribe
// cycle and returns a cache entry. The entry is negative (ok=false)
// when no valid model-computation tick arrived before the deadline.
// Honors ctx cancellation so a daemon-shutdown mid-prewarm tears the
// subscription down promptly.
func captureOptionGreeks(ctx context.Context, c *ibkrlib.Connector, under, expiryYMD string, strike float64, right string, budget time.Duration) greeksEntry {
	out := greeksEntry{}
	if under == "" || expiryYMD == "" || strike <= 0 || right == "" {
		return out
	}
	key, _, err := c.SubscribeOption(ctx, under, expiryYMD, strike, right)
	if err != nil {
		return out
	}
	defer func() { _ = c.UnsubscribeMarketData(key) }()

	_ = pollUntil(ctx, time.Now().Add(budget), func() bool {
		g, ok := c.GetOptionGreeks(key)
		if !ok {
			return false
		}
		out.value = g
		out.ok = true
		if u, uok := c.GetOptionUnderlyingPrice(key); uok && u > 0 {
			out.underlying = u
		}
		return true
	})
	return out
}

// fillOptionGreeks copies cached Greeks onto each option leg's
// Delta/Gamma/Theta/Vega fields, plus the option-contract-level bid/ask,
// IV, and prev_close pulled from the connector's tick maps (populated by
// the prewarm subscription). Legs whose data is absent keep nil pointers —
// never zero-substituted.
func (s *Server) fillOptionGreeks(c *ibkrlib.Connector, options []rpc.PositionView) {
	if s.greeks == nil {
		return
	}
	now := time.Now()
	for i := range options {
		p := &options[i]
		key := optionGreeksKey(*p)
		if key == "" {
			continue
		}
		e, ok := s.greeks.get(key, now)
		if ok && e.ok {
			g := e.value
			if g.Delta != 0 {
				d := g.Delta
				p.Delta = &d
			}
			if g.Gamma != 0 {
				d := g.Gamma
				p.Gamma = &d
			}
			if g.Theta != 0 {
				d := g.Theta
				p.Theta = &d
			}
			if g.Vega != 0 {
				d := g.Vega
				p.Vega = &d
			}
		}
		if c == nil {
			continue
		}
		if bid, ask, ok := c.GetOptionQuoteBidAsk(key); ok {
			if bid > 0 {
				b := bid
				p.OptionBid = &b
			}
			if ask > 0 {
				a := ask
				p.OptionAsk = &a
			}
		}
		if iv, ok := c.GetOptionIV(key); ok && iv > 0 {
			v := iv
			p.IV = &v
		}
		if pc, ok := c.GetOptionPrevClose(key); ok {
			v := pc
			p.OptionPrevClose = &v
		}
	}
}

// optionGreeksKey builds the same OPRA-style key that
// Connector.SubscribeOption returns. Mirrors:
//
//	fmt.Sprintf("%s_%s%s%.0f", upper(under), expiryYMD[2:], right, strike)
//
// Returns "" when any required field is missing (e.g. a malformed
// position string we couldn't parse).
//
// Accepts both rpc.SecTypeOption ("OPTION" — the AssetType enum value
// stamped by pkg/ibkr's convertIBKRPositions, the canonical wire value)
// and "OPT" (the IBKR API request-side short form, here as a defensive
// fallback for any code path that still threads the short form through).
// The original v0.10.0 release had only the "OPT" check and reported
// greeks_coverage 0/N for every option-bearing account, because
// positions came through as "OPTION"; this dual-tolerance is the
// belt-and-braces fix.
func optionGreeksKey(p rpc.PositionView) string {
	if p.SecType != rpc.SecTypeOption && p.SecType != "OPT" {
		return ""
	}
	under := normSym(p.Symbol)
	if under == "" || len(p.Expiry) < 8 || p.Strike <= 0 || p.Right == "" {
		return ""
	}
	return fmt.Sprintf("%s_%s%s%.0f", under, p.Expiry[2:], strings.ToUpper(p.Right), p.Strike)
}

// buildPortfolioAggregates rolls per-leg Greeks and currency exposure
// into one PositionsPortfolio block.
//
// Sign convention: Quantity carries the position sign (long calls +qty,
// short puts -qty). EffectiveDelta sums per-leg delta × qty × multiplier
// plus stock qty. DollarDelta uses the model-computation underlying
// price IBKR sent alongside the Greeks (kept in lockstep so the dollar
// figure is consistent with the delta it was computed against).
//
// Currency mixing is honest: we report DollarDeltaCurrency as the
// single contract currency only when every contributing option leg
// agrees. A truly mixed-currency option book gets "MIX" so the
// caller knows not to compare to a single FX rate.
//
// Always returns a non-nil result (the renderer relies on this), but
// individual pointer fields stay nil when their inputs were absent.
func buildPortfolioAggregates(stocks, options []rpc.PositionView) *rpc.PositionsPortfolio {
	p := &rpc.PositionsPortfolio{}

	// Greeks aggregation: only option positions contribute Greeks
	// directly; stocks fold in as raw share equivalents below.
	var effDelta, dollarDelta, daily, gamma, vega float64
	var haveDelta, haveDollarDelta, haveTheta, haveGamma, haveVega bool
	greeksCovered := 0
	// Per-aggregate currency tracking. dollarCcy/thetaCcy follow the same
	// "single ISO when every contributor agrees, MIX otherwise" rule.
	// Tracked separately because the contributing leg sets can differ:
	// a leg can report theta without delta (some IBKR ticks land partial),
	// so a uniform dollarCcy doesn't necessarily mean a uniform thetaCcy.
	dollarCcy, thetaCcy := "", ""
	dollarMixed, thetaMixed := false, false
	for _, o := range options {
		p.GreeksTotal++
		mult := optionMultiplier(o)
		legCcy := normCcy(o.Currency)
		if o.Delta != nil {
			effDelta += *o.Delta * o.Quantity * float64(mult)
			haveDelta = true
			// Dollar delta needs a spot; use the option's mark-side
			// underlying if available, else fall back to PrevClose
			// which carries the underlying's prev close at this point.
			spot := 0.0
			if o.PrevClose != nil && *o.PrevClose > 0 {
				spot = *o.PrevClose
			}
			if spot > 0 {
				dollarDelta += *o.Delta * o.Quantity * float64(mult) * spot
				haveDollarDelta = true
				if dollarCcy == "" {
					dollarCcy = legCcy
				} else if legCcy != dollarCcy {
					dollarMixed = true
				}
			}
		}
		if o.Theta != nil {
			daily += *o.Theta * o.Quantity * float64(mult)
			haveTheta = true
			if thetaCcy == "" {
				thetaCcy = legCcy
			} else if legCcy != thetaCcy {
				thetaMixed = true
			}
		}
		if o.Gamma != nil {
			gamma += *o.Gamma * o.Quantity * float64(mult)
			haveGamma = true
		}
		if o.Vega != nil {
			vega += *o.Vega * o.Quantity * float64(mult)
			haveVega = true
		}
		if o.Delta != nil || o.Theta != nil || o.Gamma != nil || o.Vega != nil {
			greeksCovered++
		}
	}
	// Stock legs add raw share-equivalent exposure to effective + dollar
	// delta (delta=1 for stock by definition). Stocks with mark=0 are
	// excluded — these are delisted-but-IBKR-still-reports zombies (e.g.
	// HGENQ) that the gateway streams via msgPortfolioValue with no live
	// quote. Including them inflates effective_delta by their full share
	// count on the first call after daemon start (before market-data
	// probe flags them inactive), then drops on the second call when the
	// inactive flag kicks in. Filtering on mark==0 keeps the aggregate
	// stable across calls — the position row still renders with mark=0,
	// which is the honest answer.
	for _, st := range stocks {
		if st.Mark <= 0 {
			continue
		}
		effDelta += st.Quantity
		haveDelta = true
		if st.Mark > 0 {
			dollarDelta += st.Quantity * st.Mark
			haveDollarDelta = true
			ccy := normCcy(st.Currency)
			if dollarCcy == "" {
				dollarCcy = ccy
			} else if ccy != dollarCcy {
				dollarMixed = true
			}
		}
	}

	if haveDelta {
		v := effDelta
		p.EffectiveDelta = &v
	}
	if haveDollarDelta {
		v := dollarDelta
		p.DollarDelta = &v
		if dollarMixed {
			p.DollarDeltaCurrency = "MIX"
		} else {
			p.DollarDeltaCurrency = dollarCcy
		}
	}
	if haveTheta {
		v := daily
		p.DailyTheta = &v
		if thetaMixed {
			p.DailyThetaCurrency = "MIX"
		} else {
			p.DailyThetaCurrency = thetaCcy
		}
	}
	if haveGamma {
		v := gamma
		p.Gamma = &v
	}
	if haveVega {
		v := vega
		p.Vega = &v
	}
	p.GreeksCoverage = greeksCovered

	return p
}

// optionMultiplier returns the contract multiplier used to scale a per-
// option Greek into a share-equivalent quantity. Standard equity options
// are 100; minis (e.g. NDXP) and some indexes differ. The position view
// doesn't carry the multiplier today, so we use 100 as the safe default
// — accurate for the overwhelming majority of retail accounts and
// labeled in the docs as such.
func optionMultiplier(_ rpc.PositionView) int { return 100 }

// briefSnapshotClose subscribes to sym for up to timeout, polls the
// connector's market-data cache for a positive tick 9 (previous
// regular-session close), then unsubscribes. Returns 0 on miss /
// timeout / error so callers can negative-cache. Distinct from
// briefSnapshotPrice / briefSnapshotFull because daily-change consumers
// don't need a price — just the anchor.
func briefSnapshotClose(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration) float64 {
	if c == nil {
		return 0
	}
	sym := normSym(symbol)
	if sym == "" {
		return 0
	}
	// SubscribeMarketData is idempotent — a pre-existing subscription is
	// not an error here, just fall through and read.
	_ = c.SubscribeMarketData(sym, []string{"100", "101", "104"})
	defer func() { _ = c.UnsubscribeMarketData(sym) }()

	var close float64
	_ = pollMarketData(ctx, c, sym, time.Now().Add(timeout), func(d *ibkrlib.MarketData) bool {
		if d.Close > 0 {
			close = d.Close
			return true
		}
		return false
	})
	return close
}

// groupByUnderlying produces one PositionGroup per underlying symbol present
// in either the stocks or options slice. Stock + option totals contribute to
// GroupMarketValue / GroupUnrealizedPnL; the stock leg is optional.
func groupByUnderlying(stocks, options []rpc.PositionView) []rpc.PositionGroup {
	groups := map[string]*rpc.PositionGroup{}
	getOrInit := func(under string) *rpc.PositionGroup {
		g, ok := groups[under]
		if !ok {
			g = &rpc.PositionGroup{Underlying: under}
			groups[under] = g
		}
		return g
	}
	for i := range stocks {
		s := stocks[i]
		g := getOrInit(strings.ToUpper(s.Symbol))
		stk := s
		g.Stock = &stk
		g.GroupMarketValue += s.MarketValue
		g.GroupUnrealizedPnL += s.UnrealizedPnL
	}
	for i := range options {
		o := options[i]
		g := getOrInit(strings.ToUpper(o.Symbol))
		g.Options = append(g.Options, o)
		g.GroupMarketValue += o.MarketValue
		g.GroupUnrealizedPnL += o.UnrealizedPnL
	}
	out := make([]rpc.PositionGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Underlying < out[j].Underlying })
	return out
}

// handleQuoteSnapshot resolves a contract, briefly subscribes to streaming
// market data, harvests whatever ticks arrive within the timeout window, and
// returns a snapshot. We avoid IBKR's true snapshot mode (snapshot=true)
// because it does not reliably emit tickSnapshotEnd for frozen/closed-market
// requests, leaving snapshot calls hanging until the deadline.
func (s *Server) handleQuoteSnapshot(ctx context.Context, req *rpc.Request) (*rpc.Quote, error) {
	var p rpc.QuoteSnapshotParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Contract.Symbol == "" {
		return nil, errBadRequest("contract.symbol required")
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	sym := normSym(p.Contract.Symbol)
	q := &rpc.Quote{
		Symbol:   sym,
		Contract: p.Contract,
		IVStatus: "unavailable",
		DataType: "live",
		AsOf:     time.Now(),
	}
	if q.Contract.SecType == "" {
		q.Contract.SecType = "STK"
	}
	q.Contract.Symbol = sym

	// Route through the daemon's subscription manager so a snapshot
	// running concurrently with `quote --watch` (or another snapshot, or
	// an MCP subscriber) shares the same IBKR market-data line via the
	// refcount. Without this, the snapshot's deferred unsubscribe used
	// to cancel the watcher's sub mid-stream.
	releaseSub, err := s.subs.Hold(sym)
	if err != nil && !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
		return nil, err
	}
	defer releaseSub()

	if err := pollMarketData(ctx, c, sym, time.Now().Add(timeout), func(d *ibkrlib.MarketData) bool {
		q.Bid = ptrIfPos(d.Bid)
		q.Ask = ptrIfPos(d.Ask)
		q.Last = ptrIfPos(d.Last)
		q.PrevClose = ptrIfPos(d.Close)
		q.BidSize = ptrIfPos(d.BidSize)
		q.AskSize = ptrIfPos(d.AskSize)
		q.Volume = ptrIfPos(d.Volume)
		return q.Bid != nil || q.Ask != nil || q.Last != nil
	}); err != nil && err != context.DeadlineExceeded {
		return nil, err
	}
	// Compute deltas daemon-side so every consumer (CLI text, JSON,
	// MCP) sees the same numbers without re-deriving them.
	q.Change, q.ChangePct = computeQuoteChange(q.Last, q.PrevClose)
	q.AsOf = time.Now()

	if s.contractCache != nil {
		s.contractCache.Put(cache.Contract{
			Symbol:   sym,
			SecType:  q.Contract.SecType,
			Exchange: q.Contract.Exchange,
			Currency: q.Contract.Currency,
		})
	}
	return q, nil
}

// handleCancel terminates a streaming subscription previously started via
// MethodQuoteSubscribe. The wire contract is intentionally strict: an
// unknown id returns CodeBadRequest because callers only ever cancel ids
// the daemon handed them, and "silent success" would mask client-side
// programming errors. Cancel is idempotent against itself only via the
// underlying context cancel — re-cancelling the same id after it has
// already been released returns the same bad_request, which is fine.
//
// Returning an empty result keeps the JSON-RPC shape uniform with other
// unary methods (Ok: true, Result: {}); callers that don't care about
// the body can ignore it.
func (s *Server) handleCancel(req *rpc.Request) (struct{}, error) {
	var p rpc.CancelParams
	if err := decodeParams(req.Params, &p); err != nil {
		return struct{}{}, err
	}
	if p.ID == "" {
		return struct{}{}, &badRequestError{msg: "id required"}
	}
	s.mu.Lock()
	cancel, ok := s.streams[p.ID]
	s.mu.Unlock()
	if !ok {
		return struct{}{}, &badRequestError{msg: "no active stream with id " + p.ID}
	}
	cancel()
	return struct{}{}, nil
}

// handleQuoteSubscribe attaches a fan-out tap to the daemon's per-symbol
// market-data subscription and streams coalesced frames to the caller
// until the client disconnects, the daemon shuts down, or a terminal
// error frame arrives from the manager.
//
// Client disconnect is detected by an EOF watcher reading from r: any
// read result (a stray byte or EOF) cancels streamCtx. Multiple concurrent
// subscribers to the same symbol share one IBKR market-data line via the
// subManager refcount; the line is released when the last subscriber
// releases its tap.
func (s *Server) handleQuoteSubscribe(ctx context.Context, req *rpc.Request, enc *json.Encoder, r *bufio.Reader) {
	var p rpc.QuoteSubscribeParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeError(enc, req.ID, rpc.CodeBadRequest, err.Error())
		return
	}
	if p.Contract.Symbol == "" {
		writeError(enc, req.ID, rpc.CodeBadRequest, "contract.symbol required")
		return
	}

	frames, release, err := s.subs.Subscribe(p.Contract.Symbol)
	if err != nil {
		if errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
			writeError(enc, req.ID, rpc.CodeGatewayUnavailable, err.Error())
			return
		}
		writeError(enc, req.ID, rpc.CodeInternal, err.Error())
		return
	}
	defer release()

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.streams[req.ID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, req.ID)
		s.mu.Unlock()
	}()

	// EOF watcher: streaming clients are silent after the initial subscribe
	// request, so any read result on r means either a stray byte (rare) or
	// connection close (the common case). Either way cancel the stream so
	// release() runs and the refcount drops.
	go func() {
		_, _ = r.ReadByte()
		cancel()
	}()

	for {
		select {
		case <-streamCtx.Done():
			_ = enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, End: true})
			return
		case frame, ok := <-frames:
			if !ok {
				// Manager torn the tap down (daemon_shutdown, gateway_lost, etc).
				// The terminal error frame, if any, was the last frame delivered
				// before close. Signal stream end to the client envelope.
				_ = enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, End: true})
				return
			}
			buf, err := json.Marshal(frame)
			if err != nil {
				writeError(enc, req.ID, rpc.CodeInternal, err.Error())
				return
			}
			if err := enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, Frame: buf}); err != nil {
				return
			}
		}
	}
}

// computeQuoteChange returns (change, change_pct) pointers given last and
// prevClose. Both stay nil unless last and prevClose are present and
// prevClose is strictly positive — no fabrication, no divide-by-zero.
// Centralised here so quote (snapshot) and any future watch / position
// delta caller share one formula.
func computeQuoteChange(last, prevClose *float64) (*float64, *float64) {
	if last == nil || prevClose == nil || *prevClose <= 0 {
		return nil, nil
	}
	chg := *last - *prevClose
	pct := chg / *prevClose * 100
	return &chg, &pct
}

// marketDataTypeName maps the gateway's numeric data-type notice
// (1=RealTime, 2=Frozen, 3=Delayed, 4=DelayedFrozen) to a stable
// lower-case string used on the wire and in the CLI badge. Empty for
// unknown so callers can omit the field via omitempty.
func marketDataTypeName(t int) string {
	switch t {
	case 1:
		return rpc.MarketDataLive
	case 2:
		return rpc.MarketDataFrozen
	case 3:
		return rpc.MarketDataDelayed
	case 4:
		return rpc.MarketDataDelayedFrozen
	default:
		return ""
	}
}

// defaultExpiryIVCap is how many expiries get IV by default — the front
// weeklies, the next few monthlies, plus the next quarterly. Anything
// further out is rarely on the decision path and would burn slot budget
// on every chain refresh. Callers can override via params.AllExpiries.
const defaultExpiryIVCap = 12

// chainExpiryWorkers bounds the per-expiry ATM IV fetcher fan-out.
// The IBKR market-data farm throttles aggressive subscribe churn; 4
// concurrent legs is the documented safe ceiling we already use for the
// chain strikes loop (see handleChainFetch). Higher values trigger
// "market data subscription limit reached" against the entitled slot
// count well before the fan-out wins anything.
const chainExpiryWorkers = 4

// handleChainExpiries returns the sorted, deduped option expiries for the
// underlying. WithIV (default-on via CLI) fetches per-expiry ATM implied
// volatility through a bounded worker pool, with daemon-side caching so
// the second invocation within the TTL is instant. AllExpiries lifts the
// default 12-expiry cap. On any per-strike error the row keeps IV=nil
// with IVStatus="timeout"|"unavailable" — never fail the whole call.
func (s *Server) handleChainExpiries(ctx context.Context, req *rpc.Request) (*rpc.ChainExpiriesResult, error) {
	var p rpc.ChainExpiriesParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	sym := normSym(p.Symbol)
	if sym == "" {
		return nil, errBadRequest("symbol required")
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}

	expiries, strikesByExpiry, err := fetchExpiriesAndStrikes(c, sym, 12*time.Second)
	if err != nil {
		return nil, wrapChainExpiriesErr(sym, err)
	}

	res := &rpc.ChainExpiriesResult{
		Symbol:   sym,
		AsOf:     time.Now(),
		Expiries: make([]rpc.ChainExpiry, 0, len(expiries)),
	}

	if !p.WithIV {
		today := todayLocal()
		for _, e := range expiries {
			res.Expiries = append(res.Expiries, rpc.ChainExpiry{Date: e, DTE: dteFromDate(today, e)})
		}
		return res, nil
	}

	// Cap the expiry list before IV fetch so the slow path stays bounded.
	// expiries is already sorted ascending by fetchExpiriesAndStrikes, so
	// slicing from the front picks the nearest expiries which is what
	// users actually evaluate.
	work := expiries
	if !p.AllExpiries && len(work) > defaultExpiryIVCap {
		work = work[:defaultExpiryIVCap]
	}

	// Spot is required to pick the ATM strike. A single brief subscribe
	// shared across all expiries — pre-fix this ran once before the loop
	// already; only the loop changed shape (parallel + cached).
	spot, _ := briefSnapshotPrice(ctx, c, sym, 5*time.Second)
	if spot > 0 {
		res.Spot = spot
	}

	now := time.Now()
	today := todayLocal()
	rows := make([]rpc.ChainExpiry, len(work))
	type job struct {
		idx       int
		expiry    string
		expiryYMD string
		atm       float64
	}
	var jobs []job
	for i, e := range work {
		row := rpc.ChainExpiry{Date: e, DTE: dteFromDate(today, e)}
		// Cache lookup first — a hit avoids the round-trip entirely.
		if cached, ok := s.expiryIVs.get(sym, e, now); ok {
			if cached.iv > 0 {
				v := cached.iv
				row.IV = &v
			}
			row.IVStatus = cached.status
			rows[i] = row
			continue
		}
		strikes := strikesByExpiry[e]
		if spot <= 0 || len(strikes) == 0 {
			row.IVStatus = "unavailable"
			rows[i] = row
			// Negative-cache so we don't re-poll every refresh.
			s.expiryIVs.put(sym, e, expiryIVEntry{status: "unavailable"}, now)
			continue
		}
		atm := closestStrike(strikes, spot)
		expiryYMD := strings.ReplaceAll(e, "-", "")
		rows[i] = row // populate placeholder; worker will overwrite IV/IVStatus
		jobs = append(jobs, job{idx: i, expiry: e, expiryYMD: expiryYMD, atm: atm})
	}

	// Workers write index-disjoint rows[j.idx], so no per-write mutex is
	// needed — wg.Wait inside runBounded provides happens-before to the
	// caller. The expiryIVs cache is responsible for its own locking.
	runBounded(jobs, chainExpiryWorkers, func(j job) {
		if ctx.Err() != nil {
			return
		}
		iv, status := collectExpiryATMIV(ctx, c, sym, j.expiryYMD, j.atm, 2*time.Second)
		entry := expiryIVEntry{status: status}
		if iv != nil {
			entry.iv = *iv
		}
		s.expiryIVs.put(sym, j.expiry, entry, time.Now())
		if iv != nil {
			rows[j.idx].IV = iv
		}
		rows[j.idx].IVStatus = status
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decorate each row with the 1-σ implied move now that IV is settled.
	// Pure derivation from spot + IV + DTE — no extra round trips. Skips
	// rows missing any of the three so the field stays nil rather than
	// silently absorbing a zero.
	for i := range rows {
		if mv, mvPct, ok := computeImpliedMove(spot, rows[i].IV, rows[i].DTE); ok {
			rows[i].ImpliedMove = &mv
			rows[i].ImpliedMovePct = &mvPct
		}
	}

	// Append the working set, then the rest (without IV) when caller
	// asked for the full list. AllExpiries=false drops the tail.
	res.Expiries = append(res.Expiries, rows...)
	if p.AllExpiries && len(expiries) > len(work) {
		for _, e := range expiries[len(work):] {
			res.Expiries = append(res.Expiries, rpc.ChainExpiry{Date: e, DTE: dteFromDate(today, e)})
		}
	}
	return res, nil
}

// todayLocal returns today's date at midnight local time. Surfaced as a
// helper so dteFromDate and the no-IV / AllExpiries-tail paths agree on
// the reference instant — they all read the same wall clock at handler
// entry.
func todayLocal() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// dteFromDate returns the calendar day count from today's local date to
// the YYYY-MM-DD expiry. Same-day returns 0; one calendar day out returns
// 1. Returns 0 on parse failure or expired dates — callers treat 0 as
// "intraday" and downstream math (sqrt(DTE/365)) safely yields 0 too.
func dteFromDate(today time.Time, expiry string) int {
	t, err := time.ParseInLocation("2006-01-02", expiry, today.Location())
	if err != nil {
		return 0
	}
	days := int(t.Sub(today).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// computeImpliedMove returns the 1-σ expected dollar move by expiration,
// computed from spot × IV × √(DTE/365). Industry-standard "expected move
// by expiry" formula — same shape the CBOE option calculator uses.
//
// Returns (move, movePct, true) when spot > 0, IV is non-nil and > 0,
// and DTE >= 0. The percent value is `move / spot` (a fraction, so 0.042
// means 4.2%). A DTE of 0 yields a zero move, which is correct: at expiry
// the option's time value collapses to intrinsic.
func computeImpliedMove(spot float64, iv *float64, dte int) (float64, float64, bool) {
	if spot <= 0 || iv == nil || *iv <= 0 || dte < 0 {
		return 0, 0, false
	}
	mv := spot * (*iv) * math.Sqrt(float64(dte)/365.0)
	return mv, mv / spot, true
}

// fetchExpiriesAndStrikes is a small seam for tests — the connector's
// FetchOptionExpiries and FetchOptionExpiryStrikes share an internal fetcher,
// but the daemon needs both halves and the connector public surface returns
// them via separate calls. We do one round trip via the strikes path (which
// is a superset) and derive the sorted-expiry list from the map keys.
var fetchExpiriesAndStrikes = func(connector chainExpiriesConnector, symbol string, timeout time.Duration) ([]string, map[string][]float64, error) {
	strikes, err := connector.FetchOptionExpiryStrikes(symbol, timeout)
	if err != nil {
		return nil, nil, err
	}
	expiries := make([]string, 0, len(strikes))
	for k := range strikes {
		expiries = append(expiries, k)
	}
	sort.Strings(expiries)
	return expiries, strikes, nil
}

// chainExpiriesConnector is the narrow connector surface handleChainExpiries
// uses. Defined here (not in pkg/ibkr) so tests can stub the daemon side
// without lifting the dependency back into the library.
type chainExpiriesConnector interface {
	FetchOptionExpiryStrikes(symbol string, timeout time.Duration) (map[string][]float64, error)
}

// wrapChainExpiriesErr turns the low-level pkg/ibkr errors that surface from
// the chain-expiries fetch into something a user can act on. The big one:
// ErrContractDetailsTimeout, which happens when the IBKR security-definition
// data farm is degraded (often pre-market or just after gateway start). The
// underlying quote subscription typically works in this state — the chain
// path is a separate gateway request that depends on contract resolution.
// Surfacing a generic "internal: timeout" leaves the user guessing whether
// it's a bug, a bad symbol, or a transient gateway condition.
func wrapChainExpiriesErr(symbol string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ibkrlib.ErrContractDetailsTimeout) {
		return &chainContractTimeoutError{symbol: symbol, cause: err}
	}
	return err
}

// chainContractTimeoutError carries the gateway-side "no security definitions"
// case. classifyError maps it to rpc.CodeTimeout so the CLI can recognise it
// distinctly from CodeInternal. The Error() text is the user-facing message —
// keep it short, name the symbol, and point at a concrete next step.
type chainContractTimeoutError struct {
	symbol string
	cause  error
}

func (e *chainContractTimeoutError) Error() string {
	return fmt.Sprintf("option chain unavailable for %s: gateway did not deliver security definitions in time. This is usually transient — try again in a moment, or run `ibkr status` to verify the gateway connection.", e.symbol)
}

func (e *chainContractTimeoutError) Unwrap() error { return e.cause }

// closestStrike picks the strike closest to spot. For ties (which only happens
// when strikes straddle spot equidistantly) the lower strike wins for
// determinism — IBKR's IV surface is symmetric enough that this rarely matters.
func closestStrike(strikes []float64, spot float64) float64 {
	best := strikes[0]
	bestDist := math.Abs(best - spot)
	for _, k := range strikes[1:] {
		d := math.Abs(k - spot)
		if d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}

// collectExpiryATMIV subscribes to the ATM option for one expiry, polls the
// connector's IV cache for up to perStrikeTimeout, then unsubscribes. Returns
// (iv, "ok"), (nil, "timeout"), or (nil, "unavailable") on subscribe failure.
// Takes the connector as an argument so the caller's snapshot is reused —
// avoids re-reading s.connector from inside a per-strike loop where a
// concurrent stopConnector would cause a nil deref.
func collectExpiryATMIV(ctx context.Context, c *ibkrlib.Connector, symbol, expiryYMD string, strike float64, perStrikeTimeout time.Duration) (*float64, string) {
	expiryT, err := time.Parse("20060102", expiryYMD)
	if err != nil {
		return nil, "unavailable"
	}
	reqID, err := c.SubscribeOptionIV(ctx, symbol, expiryT, strike, "C")
	if err != nil {
		return nil, "unavailable"
	}
	_ = reqID
	// Pick the streaming-quote key SubscribeOption produces so we can also
	// unsubscribe cleanly. SubscribeOptionIV uses an internal req path that
	// doesn't expose a market-data key; cancellation via UnsubscribeMarketData
	// is best-effort. Keying by symbol is enough for the IV side-channel.
	defer func() { _ = c.UnsubscribeMarketData(symbol) }()

	deadline := time.Now().Add(perStrikeTimeout)
	poll := time.NewTicker(75 * time.Millisecond)
	defer poll.Stop()
	for {
		if iv, ok := c.GetOptionIV(symbol); ok && iv > 0 {
			v := iv
			return &v, "ok"
		}
		if time.Now().After(deadline) {
			return nil, "timeout"
		}
		select {
		case <-ctx.Done():
			return nil, "timeout"
		case <-poll.C:
		}
	}
}

// handleChainFetch returns ATM ± width strikes for the specified expiry.
// Greeks are populated only when IBKR delivers them.
func (s *Server) handleChainFetch(ctx context.Context, req *rpc.Request) (*rpc.ChainResult, error) {
	var p rpc.ChainFetchParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Symbol == "" {
		return nil, errBadRequest("symbol required")
	}
	if p.Width <= 0 {
		p.Width = 5
	}
	if p.Side == "" {
		p.Side = "both"
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	expiryYMD, err := normalizeExpiry(p.Expiry)
	if err != nil {
		return nil, err
	}
	dte := daysUntil(expiryYMD)

	spot, dataType := briefSnapshotPrice(ctx, c, p.Symbol, 5*time.Second)
	if spot <= 0 {
		if s.gatewayConnector() == nil {
			return nil, ibkrlib.ErrIBKRUnavailable
		}
		return nil, fmt.Errorf("no spot price available for %s (market closed or symbol inactive)", p.Symbol)
	}
	step := strikeStep(spot)
	atm := math.Round(spot/step) * step

	res := &rpc.ChainResult{
		Symbol:   strings.ToUpper(p.Symbol),
		Spot:     spot,
		Expiry:   expiryYMD[:4] + "-" + expiryYMD[4:6] + "-" + expiryYMD[6:8],
		DTE:      dte,
		DataType: dataType,
		AsOf:     time.Now(),
	}

	wantCalls := p.Side == "calls" || p.Side == "both"
	wantPuts := p.Side == "puts" || p.Side == "both"

	// Pre-build the strike grid in display order, then fan the per-leg
	// fills out to a bounded worker pool. Pre-fix the loop was sequential
	// — for width=5 both sides that's 22 legs × ~2.5s each ≈ 55s,
	// reliably blowing past the CLI's 60s budget. 4 concurrent legs is
	// the documented safe gateway throttle (v0.2 backlog notes); the
	// gateway-side rate limiter (AcquireMarketDataSlot) serialises
	// further if we'd exceed the entitled slot count.
	n := 2*p.Width + 1
	res.Strikes = make([]rpc.ChainStrike, n)
	for i := -p.Width; i <= p.Width; i++ {
		idx := i + p.Width
		res.Strikes[idx] = rpc.ChainStrike{Strike: atm + float64(i)*step, IsATM: i == 0}
	}

	type job struct {
		idx   int
		right string
	}
	var jobs []job
	for idx := 0; idx < n; idx++ {
		if wantCalls {
			jobs = append(jobs, job{idx: idx, right: "C"})
		}
		if wantPuts {
			jobs = append(jobs, job{idx: idx, right: "P"})
		}
	}

	// Two workers can target the same strike (one C-leg, one P-leg)
	// writing disjoint fields. Go's memory model still requires a
	// happens-before for the publish, so one mutex around mergeStrikeSide
	// is plenty — contention is bounded at one merge per leg.
	var mergeMu sync.Mutex
	runBounded(jobs, 4, func(j job) {
		if ctx.Err() != nil {
			return
		}
		var local rpc.ChainStrike
		local.Strike = res.Strikes[j.idx].Strike
		fillOptionLeg(ctx, c, &local, p.Symbol, expiryYMD, local.Strike, j.right)
		mergeMu.Lock()
		mergeStrikeSide(&res.Strikes[j.idx], &local, j.right)
		mergeMu.Unlock()
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// mergeStrikeSide copies the side-specific fields (call or put)
// populated by a worker into the shared row. Disjoint by construction
// — the C worker only writes Call*, the P worker only writes Put* —
// but go through one helper so the field list stays in one place.
func mergeStrikeSide(dst, src *rpc.ChainStrike, right string) {
	if right == "C" {
		dst.CallBid = src.CallBid
		dst.CallAsk = src.CallAsk
		dst.CallLast = src.CallLast
		dst.CallIV = src.CallIV
		dst.CallDelta = src.CallDelta
		return
	}
	dst.PutBid = src.PutBid
	dst.PutAsk = src.PutAsk
	dst.PutLast = src.PutLast
	dst.PutIV = src.PutIV
	dst.PutDelta = src.PutDelta
}

func fillOptionLeg(ctx context.Context, c *ibkrlib.Connector, row *rpc.ChainStrike, symbol, expiryYMD string, strike float64, right string) {
	key, _, err := c.SubscribeOption(ctx, symbol, expiryYMD, strike, right)
	if err != nil {
		return
	}
	defer func() { _ = c.UnsubscribeMarketData(key) }()

	deadline := time.Now().Add(2500 * time.Millisecond)
	var bid, ask, last float64
	if err := pollMarketData(ctx, c, key, deadline, func(d *ibkrlib.MarketData) bool {
		if d.Bid > 0 || d.Ask > 0 || d.Last > 0 {
			bid, ask, last = d.Bid, d.Ask, d.Last
			return true
		}
		return false
	}); err != nil && err != context.DeadlineExceeded {
		return
	}
	// Tick 13 (model option computation) typically arrives a beat after
	// the first bid/ask print. IV gets its own 1 s budget, capped to
	// whatever's left of the leg's overall deadline so a slow quote can't
	// outlive the per-request budget.
	//
	// Pre-market / after-hours, the option book may have no bid/ask/last
	// but IBKR can still deliver IV via model computation. The poll runs
	// unconditionally so those fills land — costs at most one extra 1 s
	// per dead leg, bounded by the per-request budget.
	var iv float64
	ivDeadline := time.Now().Add(1 * time.Second)
	if ivDeadline.After(deadline) {
		ivDeadline = deadline
	}
	if err := pollUntil(ctx, ivDeadline, func() bool {
		v, ok := c.GetOptionIV(key)
		if ok && v > 0 {
			iv = v
			return true
		}
		return false
	}); err != nil && err != context.DeadlineExceeded {
		return
	}
	// Greeks: the same SubscribeOption path drives msg-21 model-
	// computation ticks, so by the time we have IV the per-leg Delta
	// is typically already cached. No extra wait — we just read what
	// landed. Gamma/Theta/Vega aren't surfaced on the chain wire
	// shape today; if a future chain consumer wants them we extend
	// ChainStrike rather than fold them into the same fields.
	var delta *float64
	if g, ok := c.GetOptionGreeks(key); ok && g.Delta != 0 {
		d := g.Delta
		delta = &d
	}
	if right == "C" {
		if bid > 0 {
			v := bid
			row.CallBid = &v
		}
		if ask > 0 {
			v := ask
			row.CallAsk = &v
		}
		if last > 0 {
			v := last
			row.CallLast = &v
		}
		if iv > 0 {
			v := iv
			row.CallIV = &v
		}
		row.CallDelta = delta
		return
	}
	if bid > 0 {
		v := bid
		row.PutBid = &v
	}
	if ask > 0 {
		v := ask
		row.PutAsk = &v
	}
	if last > 0 {
		v := last
		row.PutLast = &v
	}
	if iv > 0 {
		v := iv
		row.PutIV = &v
	}
	row.PutDelta = delta
}

// briefSnapshotPrice subscribes to a symbol, polls the cache for the first
// usable price, and unsubscribes. Returns the price (last → mid → bid → ask)
// and the gateway's data-type notice. Zero price + empty data type on
// timeout. Pre-fix the data-type string was hardcoded "live"; the chain
// + watch UX now needs the truthful value (frozen / delayed / etc.) to
// render the after-hours badge.
func briefSnapshotPrice(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration) (float64, string) {
	bid, ask, last, dt := briefSnapshotFull(ctx, c, symbol, timeout)
	if dt == "" {
		dt = "live"
	}
	switch {
	case last > 0:
		return last, dt
	case bid > 0 && ask > 0:
		return (bid + ask) / 2, dt
	case bid > 0:
		return bid, dt
	case ask > 0:
		return ask, dt
	default:
		return 0, ""
	}
}

// briefSnapshotFull does the same as briefSnapshotPrice but returns the raw
// bid/ask/last triple plus the gateway's data-type name (live, frozen,
// delayed, delayed-frozen, or "" on timeout). The data type is captured
// while the subscription is still live — once UnsubscribeMarketData
// fires (defer), the connector's symbol→reqID mapping is gone and the
// type would always read "unknown".
func briefSnapshotFull(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration) (bid, ask, last float64, dataType string) {
	if c == nil {
		return 0, 0, 0, ""
	}
	sym := normSym(symbol)
	_ = c.SubscribeMarketData(sym, []string{"100", "101", "104"})
	defer func() { _ = c.UnsubscribeMarketData(sym) }()

	if err := pollMarketData(ctx, c, sym, time.Now().Add(timeout), func(d *ibkrlib.MarketData) bool {
		if d.Bid > 0 || d.Ask > 0 || d.Last > 0 {
			bid, ask, last = d.Bid, d.Ask, d.Last
			// Capture data-type while the subscription is still live;
			// once UnsubscribeMarketData fires (defer above), the
			// connector's symbol→reqID mapping is gone and the type
			// would always read "unknown".
			dataType = marketDataTypeName(c.GetMarketDataTypeForSymbol(sym))
			return true
		}
		return false
	}); err != nil {
		return 0, 0, 0, ""
	}
	return bid, ask, last, dataType
}

// adHocScanLimitCap is the maximum number of rows an ad-hoc scan
// (Preset == "") may request. Presets carry their own limit and bypass
// this cap; the cap is here to keep a careless agent from asking the
// gateway for thousands of rows on an ad-hoc call. The TWS Market
// Scanner UI itself ranks to 50 by default.
const adHocScanLimitCap = 50

// defaultScanSubscriptionTimeout is how long the daemon waits for the
// gateway's first scannerData frame before giving up. 20 s is enough
// during regular trading hours but cold-starts off-hours (especially for
// scanCodes that depend on a current-session open or live option flow
// — HIGH_OPEN_GAP, TOP_PERC_GAIN, HIGH_OPT_IMP_VOLAT_OVER_HIST,
// HOT_BY_OPT_VOLUME) routinely need 25-45 s for the scanner subsystem
// to warm up. 35 s is the empirical sweet spot — long enough to absorb
// the warmup, short enough that a genuinely dead gateway still fails
// fast rather than hanging the user.
const defaultScanSubscriptionTimeout = 35 * time.Second

// handleScanRun runs a scanner. Two modes:
//
//  1. Preset (p.Preset != ""): looks up [scans.<name>] in config and runs
//     it. Limit override honored; preset.Timeout applies. Returns
//     bad_request if the preset is unknown.
//  2. Ad-hoc (p.Preset == ""): runs scanCode = p.Type / locationCode =
//     p.Exchange directly. Both fields required; missing either → bad_request.
//     Limit clamped to adHocScanLimitCap. Fixed 20s timeout.
func (s *Server) handleScanRun(ctx context.Context, req *rpc.Request) (*rpc.ScanResult, error) {
	var p rpc.ScanRunParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	var (
		scanType    string
		scanExch    string
		scanLimit   int
		scanTimeout time.Duration
		presetName  string
	)
	switch {
	case p.Preset != "":
		preset, ok := s.cfg.Scans[p.Preset]
		if !ok {
			return nil, errBadRequest(fmt.Sprintf("unknown preset %q (run 'ibkr scan list' for available)", p.Preset))
		}
		scanType = preset.Type
		scanExch = preset.Exchange
		scanLimit = p.Limit
		if scanLimit <= 0 {
			scanLimit = preset.Limit
		}
		scanTimeout = preset.Timeout.Std()
		if scanTimeout <= 0 {
			scanTimeout = defaultScanSubscriptionTimeout
		}
		presetName = p.Preset
	default:
		// Ad-hoc.
		if strings.TrimSpace(p.Type) == "" {
			return nil, errBadRequest("ad-hoc scan requires either preset or type (scanCode); see 'ibkr scan params' for available scanCodes")
		}
		if strings.TrimSpace(p.Exchange) == "" {
			return nil, errBadRequest("ad-hoc scan requires exchange (locationCode); see 'ibkr scan params' for available locationCodes")
		}
		scanType = p.Type
		scanExch = p.Exchange
		scanLimit = p.Limit
		if scanLimit <= 0 || scanLimit > adHocScanLimitCap {
			scanLimit = adHocScanLimitCap
		}
		scanTimeout = defaultScanSubscriptionTimeout
	}

	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	res := &rpc.ScanResult{
		Preset: presetName,
		Type:   scanType,
		AsOf:   time.Now(),
	}
	rows, err := c.RunScannerSubscription(ctx, ibkrlib.ScannerSubscription{
		Type:     scanType,
		Exchange: scanExch,
		Limit:    scanLimit,
	}, scanTimeout)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		res.Rows = append(res.Rows, rpc.ScanRow{
			Rank:     r.Rank,
			Symbol:   r.Symbol,
			Currency: normCcy(r.Currency),
			Comment:  r.Comment,
		})
	}
	s.enrichScanRows(ctx, c, res.Rows)
	return res, nil
}

// scanEnrichWindow is the per-row deadline for collecting market-data ticks
// after subscribing. Most US stocks deliver bid/ask/last + prev-close + IV
// + 52w within 2-4 s during RTH; the slowest generic-tick set (tick 165
// Misc Stats) tail-arrives up to ~6 s. Off-hours, ticks often don't arrive
// at all; rows then surface with whatever made it (typically prev-close
// only) and the other fields nil — the honest read.
const scanEnrichWindow = 6 * time.Second

// scanEnrichConcurrency bounds the in-flight enrichment subscriptions.
// IBKR Pro accounts typically have a 100-slot market-data cap; the daemon
// holds a few for `quote --watch`, positions Greeks, MCP subscribers, etc.
// 20 leaves comfortable headroom and reduces a 50-row scan to 2-3 waves
// at scanEnrichWindow each (~12-18 s wall clock, well under the
// MethodScanRun unary deadline).
const scanEnrichConcurrency = 20

// enrichScanRows fans out one Hold-based subscribe per row symbol,
// collects last/prev-close/change/volume/IV/52w from the daemon's tick
// cache, and writes the result back into rows in place. Bounded by
// scanEnrichConcurrency goroutines. Per-row failures are silent: the row
// keeps its existing rank+symbol+comment, the numeric fields stay nil,
// and the renderer shows "—" — never a fabricated value.
//
// Ctx cancellation propagates: a CLI Ctrl-C during enrichment aborts
// in-flight subscriptions and lets the result return with whatever data
// arrived first, again with no fabrication.
func (s *Server) enrichScanRows(ctx context.Context, c *ibkrlib.Connector, rows []rpc.ScanRow) {
	if len(rows) == 0 || c == nil {
		return
	}
	sem := make(chan struct{}, scanEnrichConcurrency)
	var wg sync.WaitGroup
	for i := range rows {
		i := i
		if rows[i].Symbol == "" {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.enrichOneScanRow(ctx, c, &rows[i])
		}()
	}
	wg.Wait()
}

// enrichOneScanRow holds a market-data subscription on the row's symbol,
// polls the connector's tick cache until the row has at least a last
// price (the minimum signal worth rendering) or the per-row window
// elapses, then writes whatever arrived back into the row.
//
// The shape of "good enough" is intentionally loose: we keep polling
// even after `last` arrives because IV and 52w typically lag bid/ask/last
// by 1-2 s, and the row is more useful with them than without.
func (s *Server) enrichOneScanRow(ctx context.Context, c *ibkrlib.Connector, row *rpc.ScanRow) {
	releaseSub, err := s.subs.Hold(row.Symbol)
	if err != nil {
		// Hold can only fail with ErrIBKRUnavailable (gateway dropped
		// mid-scan) or an internal subscribe error. Either way, the
		// row stays bare — no fabrication.
		return
	}
	defer releaseSub()

	deadline := time.Now().Add(scanEnrichWindow)
	poll := time.NewTicker(75 * time.Millisecond)
	defer poll.Stop()
	var snap *ibkrlib.MarketData
	for {
		md := c.GetMarketData()
		if data, ok := md[row.Symbol]; ok {
			snap = data
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
	if snap == nil {
		return
	}
	if snap.Last > 0 {
		v := snap.Last
		row.Last = &v
	}
	if snap.Close > 0 {
		v := snap.Close
		row.PrevClose = &v
	}
	if row.Last != nil && row.PrevClose != nil {
		ch, pct := computeQuoteChange(row.Last, row.PrevClose)
		row.Change = ch
		row.ChangePct = pct
	}
	if snap.Volume > 0 {
		v := snap.Volume
		row.Volume = &v
	}
	if snap.IV > 0 {
		v := snap.IV
		row.IV = &v
	}
	if snap.Week52High > 0 {
		v := snap.Week52High
		row.Week52High = &v
	}
	if snap.Week52Low > 0 {
		v := snap.Week52Low
		row.Week52Low = &v
	}
}

// handleScanParams fetches the gateway's scanner catalog (scanCodes,
// locationCodes, instruments) so agents can discover what's available
// without guessing at the magic strings. Result includes the raw XML
// only when explicitly requested — the payload is ~200 KB on a US Pro
// gateway and overwhelms typical agent context budgets if always sent.
func (s *Server) handleScanParams(ctx context.Context, req *rpc.Request) (*rpc.ScanParamsResult, error) {
	var p rpc.ScanParamsParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	params, err := c.RunScannerParameters(ctx, 10*time.Second)
	if err != nil {
		return nil, err
	}
	out := &rpc.ScanParamsResult{AsOf: time.Now()}
	for _, in := range params.Instruments {
		out.Instruments = append(out.Instruments, rpc.ScanParamInstrument{Name: in.Name, Type: in.Type})
	}
	for _, loc := range params.Locations {
		out.Locations = append(out.Locations, rpc.ScanParamLocation{Code: loc.Code, DisplayName: loc.DisplayName})
	}
	scans := params.ScanTypes
	if p.Instrument != "" {
		scans = params.FilterByInstrument(p.Instrument)
	}
	for _, st := range scans {
		out.ScanTypes = append(out.ScanTypes, rpc.ScanParamScanType{
			Code:        st.Code,
			DisplayName: st.DisplayName,
			Instruments: st.Instruments,
		})
	}
	if p.IncludeRawXML {
		out.RawXML = params.RawXML
	}
	return out, nil
}

// handleScanList enumerates the configured presets.
func (s *Server) handleScanList() *rpc.ScanListResult {
	out := &rpc.ScanListResult{}
	for name, preset := range s.cfg.Scans {
		out.Presets = append(out.Presets, rpc.ScanPresetSummary{
			Name:     name,
			Type:     preset.Type,
			Exchange: preset.Exchange,
			Limit:    preset.Limit,
		})
	}
	sort.SliceStable(out.Presets, func(i, j int) bool { return out.Presets[i].Name < out.Presets[j].Name })
	return out
}

// handleStatusHealth describes daemon + gateway state for status command.
// Takes connector + endpoint snapshots under mu so all IBKR-side fields
// describe the same point in time even if reconnectFlow races with this
// call (reconnect rewrites s.endpoint and s.connector).
//
// PortOrigin / TLSOrigin / Alternates come from the discovery layer and
// let `ibkr status` show whether the endpoint was pinned in config or
// found by probe — the user-visible contract for the AUTO-by-default
// design.
//
// When the daemon is currently degraded, kick off a background
// rediscover+reconnect (triggerReconnect throttles itself via the
// in-flight gate). Clearing lastConnectError as part of that turns this
// status response into "handshake in flight," which prompts the CLI's
// 25s status wait loop to keep polling — so a user who just moved IBKR
// from Gateway (4001) to TWS (7496) gets recovery in a single `ibkr
// status` invocation instead of having to restart the daemon.
func (s *Server) handleStatusHealth() *rpc.HealthResult {
	s.triggerReconnect()
	s.mu.Lock()
	ep := s.endpoint
	c := s.connector
	lastErr := s.lastConnectError
	s.mu.Unlock()

	res := &rpc.HealthResult{
		DaemonVersion: s.version,
		DaemonStarted: s.startedAt,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Account:       ep.Account,
		GatewayHost:   ep.Host,
		GatewayPort:   ep.Port,
		GatewayTLS:    ep.TLS,
		PortOrigin:    string(ep.PortOrigin),
		TLSOrigin:     string(ep.TLSOrigin),
		Alternates:    ep.Alternates,
		ClientID:      ep.ClientID,
		LastError:     lastErr,
	}
	if c != nil {
		// Report IsReady, not IsConnected: the gateway being TCP-reachable
		// is not enough — handlers must be armed (post-handshake) for any
		// data verb to succeed. Reporting IsConnected here while every
		// other verb gates on IsReady made `status` lie when the connector
		// got stuck in the {ready=false, conn=true} state (overnight TWS
		// hiccups, market-data farm reconnects). triggerReconnect (above)
		// already fired by the time we're here, so the next call sees the
		// recovered state.
		res.Connected = c.IsReady()
		res.ServerVersion = c.ServerVersion()
		res.NegotiatedTLS = c.UsingTLS()
	}
	if res.Connected {
		res.DataType = "live"
	}
	return res
}

// handleHistoryDaily returns N days of daily OHLCV bars for a symbol.
// Calendar lookback (matching IBKR HMDS): the gateway returns whatever
// trading days fall inside the window, so an N=90 request typically yields
// ~63 bars. Days defaults to 90.
func (s *Server) handleHistoryDaily(ctx context.Context, req *rpc.Request) (*rpc.HistoryDailyResult, error) {
	var p rpc.HistoryDailyParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	sym := normSym(p.Symbol)
	if sym == "" {
		return nil, errBadRequest("symbol required")
	}
	days := p.Days
	if days <= 0 {
		days = 90
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	bars, err := c.FetchHistoricalDailyBars(sym, days, 30*time.Second)
	if err != nil {
		return nil, err
	}
	res := &rpc.HistoryDailyResult{
		Symbol:   sym,
		Days:     days,
		DataType: "live",
		AsOf:     time.Now(),
		Bars:     make([]rpc.HistoryBar, 0, len(bars)),
	}
	for _, b := range bars {
		res.Bars = append(res.Bars, rpc.HistoryBar{
			Date:   barDate(b),
			Open:   b.Open,
			High:   b.High,
			Low:    b.Low,
			Close:  b.Close,
			Volume: b.Volume,
		})
	}
	return res, nil
}

// barDate returns the bar's date as YYYY-MM-DD. IBKR's daily bar dates arrive
// as YYYYMMDD strings; the parsed Time field is best-effort.
func barDate(b ibkrlib.HistoricalBar) string {
	if !b.Time.IsZero() {
		return b.Time.Format("2006-01-02")
	}
	if len(b.Date) == 8 {
		return b.Date[:4] + "-" + b.Date[4:6] + "-" + b.Date[6:8]
	}
	return b.Date
}

// errBadRequest tags a typed error so dispatch can map it to CodeBadRequest
// instead of falling through to the catch-all internal classification.
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func errBadRequest(msg string) error { return &badRequestError{msg: msg} }

// decodeParams unmarshals req.Params into dst and tags failures as bad-request
// errors so classifyError surfaces them as CodeBadRequest instead of internal.
func decodeParams[T any](raw json.RawMessage, dst *T) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errBadRequest("decode params: " + err.Error())
	}
	return nil
}

// strikeStep picks a sensible strike interval based on spot. Mirrors common
// IBKR option spacings; refined chains use whatever IBKR returns.
func strikeStep(spot float64) float64 {
	switch {
	case spot < 25:
		return 1
	case spot < 100:
		return 2.5
	case spot < 250:
		return 5
	default:
		return 10
	}
}

func normalizeExpiry(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch len(s) {
	case 8: // YYYYMMDD
		return s, nil
	case 10: // YYYY-MM-DD
		return s[:4] + s[5:7] + s[8:], nil
	default:
		return "", fmt.Errorf("expiry must be YYYY-MM-DD or YYYYMMDD")
	}
}

func daysUntil(expiryYMD string) int {
	t, err := time.Parse("20060102", expiryYMD)
	if err != nil {
		return 0
	}
	return int(time.Until(t).Hours() / 24)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
