package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/cache"
	"github.com/osauer/ibkr/internal/rpc"
)

// handleAccountSummary issues a one-shot reqAccountSummary and converts the
// result into the wire shape exposed to the CLI.
func (s *Server) handleAccountSummary(ctx context.Context) (*rpc.AccountResult, error) {
	if !s.gatewayReady() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	raw, err := s.connector.RequestAccountSummary(ctx, 8*time.Second)
	if err != nil {
		return nil, err
	}
	res := &rpc.AccountResult{
		AccountID:    raw.AccountID,
		Profile:      s.cfg.ProfileName,
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
	return res, nil
}

// handlePositionsList fetches all positions, splits stocks vs options, and
// applies the optional symbol/type filter.
func (s *Server) handlePositionsList(ctx context.Context, req *rpc.Request) (*rpc.PositionsResult, error) {
	var p rpc.PositionsListParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("decode params: %w", err)
		}
	}
	if !s.gatewayReady() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	positions, err := s.connector.GetCachedPositions()
	if err != nil {
		return nil, err
	}

	res := &rpc.PositionsResult{
		AsOf:     time.Now(),
		DataType: "live",
		Stocks:   []rpc.PositionView{},
		Options:  []rpc.PositionView{},
	}
	wantSym := strings.ToUpper(strings.TrimSpace(p.Symbol))
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
	res.ByUnderlying = groupByUnderlying(res.Stocks, res.Options)
	return res, nil
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
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}
	if p.Contract.Symbol == "" {
		return nil, errBadRequest("contract.symbol required")
	}
	if !s.gatewayReady() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	sym := strings.ToUpper(strings.TrimSpace(p.Contract.Symbol))
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

	if err := s.connector.SubscribeMarketData(sym, []string{"100", "101", "104"}); err != nil {
		// Already subscribed → fall through and read whatever's in the cache.
	}
	defer func() { _ = s.connector.UnsubscribeMarketData(sym) }()

	deadline := time.Now().Add(timeout)
	poll := time.NewTicker(75 * time.Millisecond)
	defer poll.Stop()
	for {
		md := s.connector.GetMarketData()
		if data, ok := md[sym]; ok {
			if data.Bid > 0 {
				v := data.Bid
				q.Bid = &v
			}
			if data.Ask > 0 {
				v := data.Ask
				q.Ask = &v
			}
			if data.Last > 0 {
				v := data.Last
				q.Last = &v
			}
			if data.BidSize > 0 {
				v := data.BidSize
				q.BidSize = &v
			}
			if data.AskSize > 0 {
				v := data.AskSize
				q.AskSize = &v
			}
			if data.Volume > 0 {
				v := data.Volume
				q.Volume = &v
			}
			if q.Bid != nil || q.Ask != nil || q.Last != nil {
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-poll.C:
		}
	}
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

// handleQuoteSubscribe streams ticks until the client disconnects, the
// daemon shuts down, or the underlying subscription errors. Client
// disconnect is detected by an EOF watcher reading from r: any read result
// (a stray byte or EOF) cancels streamCtx, which unwinds the loop and
// runs UnsubscribeMarketData via defer.
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
	if !s.gatewayReady() {
		writeError(enc, req.ID, rpc.CodeGatewayUnavailable, ibkrlib.ErrIBKRUnavailable.Error())
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.streams[req.ID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, req.ID)
		s.mu.Unlock()
		// Always cancel the underlying IBKR subscription.
		_ = s.connector.UnsubscribeMarketData(p.Contract.Symbol)
	}()

	// EOF watcher: streaming clients are silent after the initial subscribe
	// request, so any read result on r means either a stray byte (rare) or
	// connection close (the common case). Either way cancel the stream so
	// the deferred UnsubscribeMarketData runs. Pre-fix, no read happened
	// while the handler blocked on tick.C, so client disconnect went
	// undetected and the gateway-side subscription leaked across CLI
	// invocations (manifested as `already subscribed to AAPL` on retry).
	go func() {
		_, _ = r.ReadByte()
		cancel()
	}()

	if err := s.connector.SubscribeMarketData(p.Contract.Symbol, []string{"100", "101", "104"}); err != nil {
		writeError(enc, req.ID, rpc.CodeInternal, err.Error())
		return
	}
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	var lastBid, lastAsk, lastLast float64
	var lastBidSize, lastAskSize int
	var emitted bool
	for {
		select {
		case <-streamCtx.Done():
			_ = enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, End: true})
			return
		case <-tick.C:
			data := s.connector.GetMarketData()
			md, ok := data[strings.ToUpper(p.Contract.Symbol)]
			if !ok {
				continue
			}
			if emitted && md.Bid == lastBid && md.Ask == lastAsk && md.Last == lastLast &&
				md.BidSize == lastBidSize && md.AskSize == lastAskSize {
				continue
			}
			frame := rpc.Frame{T: time.Now()}
			if md.Bid != 0 {
				v := md.Bid
				frame.Bid = &v
			}
			if md.Ask != 0 {
				v := md.Ask
				frame.Ask = &v
			}
			if md.Last != 0 {
				v := md.Last
				frame.Last = &v
			}
			if md.BidSize != 0 {
				v := md.BidSize
				frame.BidSize = &v
			}
			if md.AskSize != 0 {
				v := md.AskSize
				frame.AskSize = &v
			}
			buf, err := json.Marshal(frame)
			if err != nil {
				writeError(enc, req.ID, rpc.CodeInternal, err.Error())
				return
			}
			if err := enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, Frame: buf}); err != nil {
				return
			}
			lastBid, lastAsk, lastLast = md.Bid, md.Ask, md.Last
			lastBidSize, lastAskSize = md.BidSize, md.AskSize
			emitted = true
		}
	}
}

// handleChainExpiries returns the sorted, deduped option expiries for the
// underlying. WithIV opt-in fetches per-expiry ATM implied volatility (one
// subscribe cycle per row, run sequentially because the gateway throttles
// aggressive subscription churn). On any per-strike error the row keeps
// IV=nil with IVStatus="timeout"|"unavailable" — never fail the whole call.
func (s *Server) handleChainExpiries(ctx context.Context, req *rpc.Request) (*rpc.ChainExpiriesResult, error) {
	var p rpc.ChainExpiriesParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}
	sym := strings.ToUpper(strings.TrimSpace(p.Symbol))
	if sym == "" {
		return nil, errBadRequest("symbol required")
	}
	if !s.gatewayReady() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}

	expiries, strikesByExpiry, err := fetchExpiriesAndStrikes(s.connector, sym, 12*time.Second)
	if err != nil {
		return nil, err
	}

	res := &rpc.ChainExpiriesResult{
		Symbol:   sym,
		AsOf:     time.Now(),
		Expiries: make([]rpc.ChainExpiry, 0, len(expiries)),
	}

	if !p.WithIV {
		for _, e := range expiries {
			res.Expiries = append(res.Expiries, rpc.ChainExpiry{Date: e})
		}
		return res, nil
	}

	// --with-iv: pick ATM strike per expiry, briefly subscribe, capture IV.
	spot, _ := s.briefSnapshotPrice(ctx, sym, 5*time.Second)
	for _, e := range expiries {
		row := rpc.ChainExpiry{Date: e}
		strikes := strikesByExpiry[e]
		if spot <= 0 || len(strikes) == 0 {
			row.IVStatus = "unavailable"
			res.Expiries = append(res.Expiries, row)
			continue
		}
		atm := closestStrike(strikes, spot)
		expiryYMD := strings.ReplaceAll(e, "-", "")
		iv, status := s.collectExpiryATMIV(ctx, sym, expiryYMD, atm, 2*time.Second)
		if iv != nil {
			row.IV = iv
		}
		row.IVStatus = status
		res.Expiries = append(res.Expiries, row)
	}
	return res, nil
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
func (s *Server) collectExpiryATMIV(ctx context.Context, symbol, expiryYMD string, strike float64, perStrikeTimeout time.Duration) (*float64, string) {
	expiryT, err := time.Parse("20060102", expiryYMD)
	if err != nil {
		return nil, "unavailable"
	}
	reqID, err := s.connector.SubscribeOptionIV(symbol, expiryT, strike, "C")
	if err != nil {
		return nil, "unavailable"
	}
	_ = reqID
	// Pick the streaming-quote key SubscribeOption produces so we can also
	// unsubscribe cleanly. SubscribeOptionIV uses an internal req path that
	// doesn't expose a market-data key; cancellation via UnsubscribeMarketData
	// is best-effort. Keying by symbol is enough for the IV side-channel.
	defer func() { _ = s.connector.UnsubscribeMarketData(symbol) }()

	deadline := time.Now().Add(perStrikeTimeout)
	poll := time.NewTicker(75 * time.Millisecond)
	defer poll.Stop()
	for {
		if iv, ok := s.connector.GetOptionIV(symbol); ok && iv > 0 {
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
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
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
	if !s.gatewayReady() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	expiryYMD, err := normalizeExpiry(p.Expiry)
	if err != nil {
		return nil, err
	}
	dte := daysUntil(expiryYMD)

	spot, dataType := s.briefSnapshotPrice(ctx, p.Symbol, 5*time.Second)
	if spot <= 0 {
		if !s.gatewayReady() {
			return nil, ibkrlib.ErrIBKRUnavailable
		}
		return nil, fmt.Errorf("no spot price available for %s (market closed or symbol inactive)", p.Symbol)
	}
	_ = dataType
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

	for i := -p.Width; i <= p.Width; i++ {
		strike := atm + float64(i)*step
		row := rpc.ChainStrike{Strike: strike, IsATM: i == 0}
		if wantCalls {
			s.fillOptionLeg(ctx, &row, p.Symbol, expiryYMD, strike, "C")
		}
		if wantPuts {
			s.fillOptionLeg(ctx, &row, p.Symbol, expiryYMD, strike, "P")
		}
		res.Strikes = append(res.Strikes, row)
	}
	return res, nil
}

func (s *Server) fillOptionLeg(ctx context.Context, row *rpc.ChainStrike, symbol, expiryYMD string, strike float64, right string) {
	key, _, err := s.connector.SubscribeOption(symbol, expiryYMD, strike, right)
	if err != nil {
		return
	}
	defer func() { _ = s.connector.UnsubscribeMarketData(key) }()

	deadline := time.Now().Add(2500 * time.Millisecond)
	poll := time.NewTicker(75 * time.Millisecond)
	defer poll.Stop()
	var bid, ask, last float64
	pricesArrived := false
	for {
		md := s.connector.GetMarketData()
		if data, ok := md[key]; ok {
			if data.Bid > 0 || data.Ask > 0 || data.Last > 0 {
				bid, ask, last = data.Bid, data.Ask, data.Last
				pricesArrived = true
				break
			}
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
	// Tick 13 (model option computation) often arrives a beat after the first
	// bid/ask print. Keep polling for IV until the same deadline expires; if it
	// never lands, leave the row IV nil rather than fabricating one.
	var iv float64
	if pricesArrived {
		for time.Now().Before(deadline) {
			if v, ok := s.connector.GetOptionIV(key); ok && v > 0 {
				iv = v
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-poll.C:
			}
		}
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
}

// briefSnapshotPrice subscribes to a symbol, polls the cache for the first
// usable price, and unsubscribes. Returns the price (last → mid → bid → ask)
// and the data-type string. Zero on timeout.
func (s *Server) briefSnapshotPrice(ctx context.Context, symbol string, timeout time.Duration) (float64, string) {
	bid, ask, last := s.briefSnapshotFull(ctx, symbol, timeout)
	switch {
	case last > 0:
		return last, "live"
	case bid > 0 && ask > 0:
		return (bid + ask) / 2, "live"
	case bid > 0:
		return bid, "live"
	case ask > 0:
		return ask, "live"
	default:
		return 0, "live"
	}
}

// briefSnapshotFull does the same as briefSnapshotPrice but returns the raw
// bid/ask/last triple so option chains can populate independently.
func (s *Server) briefSnapshotFull(ctx context.Context, symbol string, timeout time.Duration) (float64, float64, float64) {
	if s.connector == nil {
		return 0, 0, 0
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if err := s.connector.SubscribeMarketData(sym, []string{"100", "101", "104"}); err != nil {
		// Already subscribed → fall through and just read.
	}
	defer func() { _ = s.connector.UnsubscribeMarketData(sym) }()

	deadline := time.Now().Add(timeout)
	poll := time.NewTicker(75 * time.Millisecond)
	defer poll.Stop()
	for {
		md := s.connector.GetMarketData()
		if data, ok := md[sym]; ok {
			if data.Bid > 0 || data.Ask > 0 || data.Last > 0 {
				return data.Bid, data.Ask, data.Last
			}
		}
		if time.Now().After(deadline) {
			return 0, 0, 0
		}
		select {
		case <-ctx.Done():
			return 0, 0, 0
		case <-poll.C:
		}
	}
}

// handleScanRun runs a configured scanner preset. v1 returns an empty result
// with explanatory comment if the preset is unknown.
func (s *Server) handleScanRun(ctx context.Context, req *rpc.Request) (*rpc.ScanResult, error) {
	var p rpc.ScanRunParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}
	preset, ok := s.cfg.Scans[p.Preset]
	if !ok {
		return nil, errBadRequest(fmt.Sprintf("unknown preset %q (run 'ibkr scan list' for available)", p.Preset))
	}
	if !s.gatewayReady() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	limit := p.Limit
	if limit <= 0 {
		limit = preset.Limit
	}
	res := &rpc.ScanResult{
		Preset: p.Preset,
		Type:   preset.Type,
		AsOf:   time.Now(),
	}
	scanTimeout := preset.Timeout.Std()
	if scanTimeout <= 0 {
		scanTimeout = 20 * time.Second
	}
	rows, err := s.connector.RunScannerSubscription(ctx, ibkrlib.ScannerSubscription{
		Type:     preset.Type,
		Exchange: preset.Exchange,
		Limit:    limit,
	}, scanTimeout)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		res.Rows = append(res.Rows, rpc.ScanRow{
			Rank:    r.Rank,
			Symbol:  r.Symbol,
			Comment: r.Comment,
		})
	}
	return res, nil
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
func (s *Server) handleStatusHealth() *rpc.HealthResult {
	res := &rpc.HealthResult{
		DaemonVersion: s.version,
		DaemonStarted: s.startedAt,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Profile:       s.cfg.ProfileName,
		Account:       s.cfg.Profile.Account,
		GatewayHost:   s.cfg.Profile.Host,
		GatewayPort:   s.cfg.Profile.Port,
		GatewayTLS:    s.cfg.Profile.TLS,
		ClientID:      s.cfg.Profile.ClientID,
	}
	if s.connector != nil {
		res.Connected = s.connector.IsConnected()
		res.ServerVersion = s.connector.ServerVersion()
		res.NegotiatedTLS = s.connector.UsingTLS()
	}
	if res.Connected {
		res.DataType = "live"
	}
	s.mu.Lock()
	res.LastError = s.lastConnectError
	s.mu.Unlock()
	return res
}

// gatewayReady reports whether the connector exists and the underlying TCP
// session is live. All read handlers short-circuit on this so the CLI can
// report a uniform gateway_unavailable rather than empty/blank data.
func (s *Server) gatewayReady() bool {
	return s.connector != nil && s.connector.IsConnected()
}

// handleHistoryDaily returns N days of daily OHLCV bars for a symbol.
// Calendar lookback (matching IBKR HMDS): the gateway returns whatever
// trading days fall inside the window, so an N=90 request typically yields
// ~63 bars. Days defaults to 90.
func (s *Server) handleHistoryDaily(ctx context.Context, req *rpc.Request) (*rpc.HistoryDailyResult, error) {
	var p rpc.HistoryDailyParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}
	sym := strings.ToUpper(strings.TrimSpace(p.Symbol))
	if sym == "" {
		return nil, errBadRequest("symbol required")
	}
	days := p.Days
	if days <= 0 {
		days = 90
	}
	if !s.gatewayReady() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	bars, err := s.connector.FetchHistoricalDailyBars(sym, days, 30*time.Second)
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
