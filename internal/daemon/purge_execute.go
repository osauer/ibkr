package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

const (
	purgeExecuteStatusBlocked   = "blocked"
	purgeExecuteStatusFlat      = "flat"
	purgeExecuteStatusSubmitted = "submitted"
	purgeExecuteStatusPartial   = "partial"
	purgeExecuteStatusError     = "error"

	purgeExecuteSource     = "purge"
	purgeOriginalSideLong  = "LONG"
	purgeOriginalSideShort = "SHORT"
)

func (s *Server) handlePurgeExecute(ctx context.Context, req *rpc.Request) (*rpc.PurgeExecuteResult, error) {
	var p rpc.PurgeExecuteParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.executePurge(ctx, p)
}

func (s *Server) executePurge(ctx context.Context, p rpc.PurgeExecuteParams) (*rpc.PurgeExecuteResult, error) {
	bypassPreview := true
	if p.BypassPreview != nil {
		bypassPreview = *p.BypassPreview
	}
	targetSymbols := purgeExecuteSymbols(p.Symbols)
	if p.All {
		targetSymbols = nil
	}
	wait := time.Duration(p.WaitMs) * time.Millisecond
	if wait <= 0 {
		wait = 2 * time.Second
	}
	if wait > 10*time.Second {
		wait = 10 * time.Second
	}

	status := s.currentTradingStatus()
	res := &rpc.PurgeExecuteResult{
		Kind:                 "ibkr.purge_execute",
		PurgeID:              strings.TrimSpace(p.PurgeID),
		Status:               purgeExecuteStatusBlocked,
		Mode:                 status.Mode,
		Account:              status.Account,
		Endpoint:             status.Endpoint,
		ClientID:             status.ClientID,
		BypassPreview:        bypassPreview,
		MonitorCommand:       "ibkr purge monitor",
		RestoreReviewCommand: "ibkr purge restore SYMBOL",
		AsOf:                 s.orderNow(),
	}
	if res.PurgeID == "" {
		res.PurgeID = "purge_" + s.orderNow().UTC().Format("20060102_150405")
	}
	if !bypassPreview {
		res.Blockers = append(res.Blockers, rpc.TradingBlocker{
			Code:    "purge_preview_mode_unavailable",
			Message: "purge currently supports the fast path only",
			Action:  "Run `ibkr purge` with the default preview bypass.",
		})
		res.Message = res.Blockers[0].Message
		return res, nil
	}
	if blockers := s.purgeExecuteBlockers(status); len(blockers) > 0 {
		res.Blockers = blockers
		res.Message = firstTradingBlockerMessage(blockers)
		return res, nil
	}

	positions, err := s.refreshPurgePositions()
	if err != nil {
		res.Status = purgeExecuteStatusError
		res.ErrorLegs = max(1, len(p.Legs))
		res.Message = "refresh current positions: " + err.Error()
		return res, nil
	}
	positions = filterPurgePositionsForAccount(positions, status.Account)

	legs := p.Legs
	if len(legs) == 0 {
		legs = purgeLegsFromCurrentPositions(positions, targetSymbols)
	}
	res.SelectedLegs = len(legs)
	if len(legs) == 0 {
		res.Status = purgeExecuteStatusFlat
		res.Message = purgeNoCurrentPositionMessage(targetSymbols)
		res.AsOf = s.orderNow()
		return res, nil
	}
	openByLeg, err := s.openPurgeOrdersByLeg(status.Account)
	if err != nil {
		res.Status = purgeExecuteStatusError
		res.ErrorLegs = len(legs)
		res.Message = "load open purge orders: " + err.Error()
		return res, nil
	}
	activeByLegSide, err := s.activePurgeLedgerQuantityByLegSide(status.Account)
	if err != nil {
		res.Status = purgeExecuteStatusError
		res.ErrorLegs = len(legs)
		res.Message = "load active purge ledger rows: " + err.Error()
		return res, nil
	}

	for _, leg := range legs {
		if err := ctx.Err(); err != nil {
			res.Status = purgeExecuteStatusError
			res.Message = err.Error()
			return res, nil
		}
		s.executePurgeLeg(ctx, status, positions, openByLeg, activeByLegSide, res, leg)
	}

	if res.SubmittedLegs > 0 && wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			res.Warnings = append(res.Warnings, "lifecycle wait interrupted: "+ctx.Err().Error())
		}
		s.refreshPurgeOrderViews(res)
	}
	res.Status = purgeExecuteFinalStatus(*res)
	if res.Message == "" {
		res.Message = purgeExecuteMessage(*res)
	}
	res.AsOf = s.orderNow()
	return res, nil
}

func (s *Server) currentTradingStatus() rpc.TradingStatus {
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	return s.tradingStatus(ep)
}

func (s *Server) purgeExecuteBlockers(status rpc.TradingStatus) []rpc.TradingBlocker {
	blockers := s.brokerWriteAuthorization(status).Blockers
	add := func(code, message, action string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: code, Message: message, Action: action})
	}
	if s == nil || s.purgeLedger == nil {
		add("purge_ledger_unavailable", "purge execution requires a writable daemon purge ledger", "Fix the daemon state directory before purging positions.")
	}
	if !s.purgeRestoreEnabled() {
		add("purge_restore_disabled", "purge/restore actions are disabled in platform settings", "Run `ibkr settings set features.purge_restore.enabled=true` before using purge/restore.")
	}
	return blockers
}

func (s *Server) refreshPurgePositions() ([]*ibkrlib.RawPosition, error) {
	if s.purgeRefreshPositions != nil {
		return s.purgeRefreshPositions()
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	return c.RefreshPositions(10 * time.Second)
}

func filterPurgePositionsForAccount(positions []*ibkrlib.RawPosition, account string) []*ibkrlib.RawPosition {
	account = strings.TrimSpace(account)
	if account == "" {
		return positions
	}
	filtered := make([]*ibkrlib.RawPosition, 0, len(positions))
	for _, pos := range positions {
		if pos == nil {
			continue
		}
		if pos.Account == "" || strings.EqualFold(pos.Account, account) {
			filtered = append(filtered, pos)
		}
	}
	return filtered
}

func purgeExecuteSymbols(symbols []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" || seen[symbol] {
			continue
		}
		seen[symbol] = true
		out = append(out, symbol)
	}
	return out
}

func purgeLegsFromCurrentPositions(positions []*ibkrlib.RawPosition, symbols []string) []rpc.PurgeExecuteLeg {
	symbolSet := map[string]bool{}
	for _, symbol := range symbols {
		symbolSet[strings.ToUpper(strings.TrimSpace(symbol))] = true
	}
	legs := make([]rpc.PurgeExecuteLeg, 0, len(positions))
	for _, pos := range positions {
		if pos == nil || pos.Position == 0 {
			continue
		}
		contract := contractParamsFromRawPosition(*pos, rpc.ContractParams{})
		if contract.Symbol == "" {
			continue
		}
		if len(symbolSet) > 0 && !symbolSet[strings.ToUpper(contract.Symbol)] {
			continue
		}
		action := rpc.OrderActionSell
		originalSide := purgeOriginalSideLong
		if pos.Position < 0 {
			action = rpc.OrderActionBuy
			originalSide = purgeOriginalSideShort
		}
		legs = append(legs, rpc.PurgeExecuteLeg{
			LegID:        purgeLegIDForContract(contract),
			Symbol:       contract.Symbol,
			SecType:      contract.SecType,
			Contract:     contract,
			OriginalSide: originalSide,
			PurgeAction:  action,
			Quantity:     math.Abs(pos.Position),
			Multiplier:   contract.Multiplier,
		})
	}
	return legs
}

func purgeNoCurrentPositionMessage(symbols []string) string {
	if len(symbols) == 0 {
		return "no current positions to purge"
	}
	return "no current positions matched purge target"
}

func (s *Server) activePurgeLedgerQuantityByLegSide(account string) (map[string]float64, error) {
	out := map[string]float64{}
	if s == nil || s.purgeLedger == nil {
		return out, nil
	}
	rows, _, err := s.purgeLedger.Snapshot(strings.TrimSpace(account), "")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.RemainingQuantity <= 0 || row.LegID == "" {
			continue
		}
		legID := purgeLegIDForContract(row.Contract)
		if legID == "" {
			legID = row.LegID
		}
		out[purgeLedgerCoverageKey(legID, row.OriginalSide)] += row.RemainingQuantity
	}
	return out, nil
}

func (s *Server) executePurgeLeg(ctx context.Context, status rpc.TradingStatus, positions []*ibkrlib.RawPosition, openByLeg map[string][]rpc.OrderView, activeByLegSide map[string]float64, res *rpc.PurgeExecuteResult, leg rpc.PurgeExecuteLeg) {
	contract, currentQty, found := currentPurgePosition(positions, leg)
	if !found || currentQty == 0 {
		addPurgeSkipped(res, leg, "already flat")
		return
	}
	currentSide := purgeOriginalSideForQuantity(currentQty)
	if leg.OriginalSide == "" {
		leg.OriginalSide = currentSide
	}
	inputLegID := strings.TrimSpace(leg.LegID)
	leg.LegID = purgeLegIDForContract(contract)
	legIDs := purgeLegIDCandidates(inputLegID, leg.LegID, purgeLegacyLegIDForContract(contract))
	leg.Symbol = contract.Symbol
	leg.SecType = contract.SecType
	leg.Contract = contract
	if purgeSideFlipped(leg.OriginalSide, currentQty) {
		addPurgeSkipped(res, leg, "current position side no longer matches selected purge leg")
		return
	}
	qty := math.Abs(currentQty)
	if purgeOpenOrderExists(openByLeg, legIDs) {
		addPurgeSkipped(res, leg, "open purge/restore order exists for this ledger row")
		return
	}
	if covered := activePurgeLedgerCoveredQuantity(activeByLegSide, legIDs, currentSide); covered > 0 {
		if qty <= covered+1e-9 {
			addPurgeSkipped(res, leg, "current quantity already covered by active purge ledger")
			return
		}
		qty -= covered
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: reduced purge quantity by %.4g already covered in active purge ledger", purgeExecuteLegLabel(leg), covered))
	}
	if math.Trunc(qty) != qty {
		addPurgeSkipped(res, leg, "fractional current quantity cannot use the integer order path")
		return
	}
	action := rpc.OrderActionSell
	if currentQty < 0 {
		action = rpc.OrderActionBuy
	}
	if leg.PurgeAction != "" && !strings.EqualFold(leg.PurgeAction, action) {
		addPurgeSkipped(res, leg, "current position action no longer matches selected purge leg")
		return
	}
	if !purgeContractSupported(contract) {
		addPurgeSkipped(res, leg, "unsupported security type for purge execute")
		return
	}

	quote, err := s.fetchPreviewQuote(ctx, contract, 5*time.Second)
	if err != nil {
		addPurgeError(res, leg, "quote: "+err.Error())
		return
	}
	limit, err := purgeAggressiveLimit(action, contract, quote)
	if err != nil {
		addPurgeError(res, leg, "pricing: "+err.Error())
		return
	}
	orderRef := purgeOrderRef(s.orderNow())
	draft := rpc.OrderDraft{
		Action:     action,
		Contract:   contract,
		Quantity:   int(qty),
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: limit,
		TIF:        rpc.OrderTIFDay,
		Strategy:   "purge-aggressive-limit",
		OrderRef:   orderRef,
		OpenClose:  "C",
	}
	orderID, err := s.reserveBrokerOrderID(ctx)
	if err != nil {
		addPurgeError(res, leg, "reserve order id: "+err.Error())
		return
	}
	attempt := purgeJournalEventForDraft(draft, status, res.PurgeID, leg.LegID, orderID, s.orderNow())
	if err := s.orderJournal.Append(attempt); err != nil {
		addPurgeError(res, leg, "append send journal: "+err.Error())
		return
	}

	brokerContract := previewIBKRContract(contract)
	order := previewIBKROrder(draft)
	order.OrderID = orderID
	order.ClientID = status.ClientID
	order.Account = status.Account
	if err := s.submitConfiguredOrder(ctx, status, brokerContract, order); err != nil {
		ev := purgeJournalEventForDraft(draft, status, res.PurgeID, leg.LegID, orderID, s.orderNow())
		ev.Type = orderJournalEventSendError
		ev.SendState = orderSendStateUncertainSend
		ev.Message = "purge broker send returned error; reconcile before reusing this intent: " + err.Error()
		if appendErr := s.orderJournal.Append(ev); appendErr != nil {
			res.Warnings = append(res.Warnings, "append purge send error: "+appendErr.Error())
		}
		res.ErrorLegs++
		res.Orders = append(res.Orders, purgeOrderResult(leg, draft, orderID, quote, orderSendStateUncertainSend, ev.Message))
		return
	}

	res.SubmittedLegs++
	res.Orders = append(res.Orders, purgeOrderResult(leg, draft, orderID, quote, orderSendStateSendAttempted, "purge broker placeOrder transmit attempted; waiting for broker lifecycle callback"))
}

func purgeOriginalSideForQuantity(qty float64) string {
	if qty < 0 {
		return purgeOriginalSideShort
	}
	return purgeOriginalSideLong
}

func purgeLedgerCoverageKey(legID, originalSide string) string {
	return strings.TrimSpace(legID) + "|" + strings.ToUpper(strings.TrimSpace(originalSide))
}

func purgeLegIDCandidates(values ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func purgeOpenOrderExists(openByLeg map[string][]rpc.OrderView, legIDs []string) bool {
	for _, legID := range legIDs {
		if len(openByLeg[legID]) > 0 {
			return true
		}
	}
	return false
}

func activePurgeLedgerCoveredQuantity(activeByLegSide map[string]float64, legIDs []string, originalSide string) float64 {
	var covered float64
	for _, legID := range legIDs {
		covered += activeByLegSide[purgeLedgerCoverageKey(legID, originalSide)]
	}
	return covered
}

func purgeExecuteLegLabel(leg rpc.PurgeExecuteLeg) string {
	if strings.EqualFold(leg.Contract.SecType, "OPT") {
		expiry := leg.Contract.Expiry
		if len(expiry) == 8 {
			expiry = expiry[2:]
		}
		return fmt.Sprintf("%s %s %s %.2f", leg.Contract.Symbol, expiry, leg.Contract.Right, leg.Contract.Strike)
	}
	if leg.Contract.Symbol != "" {
		return leg.Contract.Symbol
	}
	return leg.Symbol
}

func currentPurgePosition(positions []*ibkrlib.RawPosition, leg rpc.PurgeExecuteLeg) (rpc.ContractParams, float64, bool) {
	for _, pos := range positions {
		if pos == nil || !purgePositionMatchesLeg(*pos, leg) {
			continue
		}
		return contractParamsFromRawPosition(*pos, leg.Contract), pos.Position, true
	}
	return rpc.ContractParams{}, 0, false
}

func purgePositionMatchesLeg(pos ibkrlib.RawPosition, leg rpc.PurgeExecuteLeg) bool {
	if leg.Contract.ConID > 0 && pos.Contract.ConID > 0 {
		return leg.Contract.ConID == pos.Contract.ConID
	}
	secType := strings.ToUpper(strings.TrimSpace(leg.Contract.SecType))
	if secType == "" {
		secType = strings.ToUpper(strings.TrimSpace(leg.SecType))
	}
	posSecType := strings.ToUpper(strings.TrimSpace(pos.Contract.SecType))
	if secType == "STK" || secType == "ETF" || strings.EqualFold(leg.SecType, rpc.SecTypeStock) {
		if posSecType != "STK" && posSecType != "ETF" {
			return false
		}
		return purgeStockPositionMatches(pos.Contract, leg.Contract)
	}
	if secType != "OPT" || posSecType != "OPT" {
		return secType != "" &&
			strings.EqualFold(posSecType, secType) &&
			strings.EqualFold(pos.Contract.Symbol, leg.Contract.Symbol) &&
			optionalEqual(pos.Contract.Currency, leg.Contract.Currency) &&
			optionalEqual(pos.Contract.LocalSymbol, leg.Contract.LocalSymbol) &&
			optionalEqual(pos.Contract.TradingClass, leg.Contract.TradingClass)
	}
	return strings.EqualFold(pos.Contract.Symbol, leg.Contract.Symbol) &&
		strings.TrimSpace(pos.Contract.Expiry) == strings.TrimSpace(leg.Contract.Expiry) &&
		samePreviewFloat(pos.Contract.Strike, leg.Contract.Strike) &&
		strings.EqualFold(pos.Contract.Right, leg.Contract.Right) &&
		optionalEqual(pos.Contract.Currency, leg.Contract.Currency) &&
		optionalEqual(pos.Contract.TradingClass, leg.Contract.TradingClass) &&
		optionalEqual(pos.Contract.LocalSymbol, leg.Contract.LocalSymbol) &&
		optionalMultiplierEqual(pos.Contract.Multiplier, leg.Contract.Multiplier)
}

func purgeStockPositionMatches(pos ibkrlib.Contract, want rpc.ContractParams) bool {
	if !strings.EqualFold(pos.Symbol, want.Symbol) {
		return false
	}
	if want.Currency != "" && pos.Currency != "" && !strings.EqualFold(pos.Currency, want.Currency) {
		return false
	}
	if want.LocalSymbol != "" && pos.LocalSymbol != "" && !strings.EqualFold(pos.LocalSymbol, want.LocalSymbol) {
		return false
	}
	if want.TradingClass != "" && pos.TradingClass != "" && !strings.EqualFold(pos.TradingClass, want.TradingClass) {
		return false
	}
	if want.PrimaryExch != "" && !stockVenueMatches(pos, want.PrimaryExch) {
		return false
	}
	if want.PrimaryExch == "" && want.Exchange != "" && !strings.EqualFold(want.Exchange, "SMART") && !stockVenueMatches(pos, want.Exchange) {
		return false
	}
	return true
}

func optionalEqual(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	return a == "" || b == "" || strings.EqualFold(a, b)
}

func optionalMultiplierEqual(a, b int) bool {
	return a <= 0 || b <= 0 || a == b
}

func contractParamsFromRawPosition(pos ibkrlib.RawPosition, fallback rpc.ContractParams) rpc.ContractParams {
	rawMultiplier := pos.Contract.Multiplier
	fallbackMultiplier := fallback.Multiplier
	c := rpc.ContractParams{
		ConID:        pos.Contract.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(pos.Contract.Symbol)),
		SecType:      strings.ToUpper(strings.TrimSpace(pos.Contract.SecType)),
		Exchange:     strings.TrimSpace(pos.Contract.Exchange),
		PrimaryExch:  strings.TrimSpace(pos.Contract.PrimaryExch),
		Currency:     strings.ToUpper(strings.TrimSpace(pos.Contract.Currency)),
		LocalSymbol:  strings.TrimSpace(pos.Contract.LocalSymbol),
		TradingClass: strings.TrimSpace(pos.Contract.TradingClass),
		Expiry:       strings.TrimSpace(pos.Contract.Expiry),
		Strike:       pos.Contract.Strike,
		Right:        strings.ToUpper(strings.TrimSpace(pos.Contract.Right)),
	}
	if c.SecType == "" {
		c.SecType = fallback.SecType
	}
	if c.SecType == "" || c.SecType == rpc.SecTypeStock {
		c.SecType = "STK"
	}
	if c.SecType == rpc.SecTypeOption {
		c.SecType = "OPT"
	}
	if c.Exchange == "" {
		c.Exchange = fallback.Exchange
	}
	if c.Exchange == "" {
		c.Exchange = "SMART"
	}
	if c.Currency == "" {
		c.Currency = fallback.Currency
	}
	if c.Currency == "" {
		c.Currency = "USD"
	}
	normalisePositionStockRoute(&c)
	switch strings.ToUpper(strings.TrimSpace(c.SecType)) {
	case "STK", "ETF":
		c.Multiplier = 1
	case "OPT":
		if rawMultiplier > 0 {
			c.Multiplier = rawMultiplier
		} else if fallbackMultiplier > 0 {
			c.Multiplier = fallbackMultiplier
		} else {
			c.Multiplier = 100
		}
	default:
		if rawMultiplier > 0 {
			c.Multiplier = rawMultiplier
		} else if fallbackMultiplier > 0 {
			c.Multiplier = fallbackMultiplier
		} else {
			c.Multiplier = contractMultiplier(c)
		}
	}
	return c
}

func normalisePositionStockRoute(c *rpc.ContractParams) {
	if c == nil {
		return
	}
	switch strings.ToUpper(strings.TrimSpace(c.SecType)) {
	case "STK", "ETF":
	default:
		return
	}
	exchange := strings.ToUpper(strings.TrimSpace(c.Exchange))
	primary := strings.ToUpper(strings.TrimSpace(c.PrimaryExch))
	if primary == "" && exchange != "" && exchange != "SMART" {
		c.PrimaryExch = exchange
	}
	if exchange == "" || exchange != "SMART" {
		c.Exchange = "SMART"
	}
}

func purgeSideFlipped(originalSide string, currentQty float64) bool {
	switch strings.ToUpper(strings.TrimSpace(originalSide)) {
	case "LONG":
		return currentQty < 0
	case "SHORT":
		return currentQty > 0
	default:
		return false
	}
}

func purgeContractSupported(contract rpc.ContractParams) bool {
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF", "OPT":
		return true
	default:
		return false
	}
}

func purgeAggressiveLimit(action string, contract rpc.ContractParams, quote rpc.OrderQuoteSnapshot) (float64, error) {
	if !rpc.IsLiveDataType(quote.DataType) {
		return 0, fmt.Errorf("requires live bid/ask data")
	}
	if quote.Bid == nil || quote.Ask == nil || *quote.Bid <= 0 || *quote.Ask <= *quote.Bid {
		return 0, fmt.Errorf("requires a positive two-sided bid/ask")
	}
	bid := *quote.Bid
	ask := *quote.Ask
	mid := (bid + ask) / 2
	tick := purgeQuoteTick(contract, bid, ask, mid)
	cushion := max(2*tick, 0.25*(ask-bid))
	steps := math.Ceil(cushion / tick)
	if steps < 1 {
		steps = 1
	}
	switch action {
	case rpc.OrderActionBuy:
		return roundPrice(ask + steps*tick), nil
	case rpc.OrderActionSell:
		price := bid - steps*tick
		if price < tick {
			return 0, fmt.Errorf("aggressive sell limit would be below minimum tick")
		}
		return roundPrice(price), nil
	default:
		return 0, fmt.Errorf("action must be BUY or SELL")
	}
}

func purgeQuoteTick(contract rpc.ContractParams, bid, ask, price float64) float64 {
	fallback := purgePriceTick(contract, price)
	if strings.EqualFold(contract.SecType, "OPT") {
		return fallback
	}
	spread := roundPrice(ask - bid)
	if spread > 0 && spread <= 0.1 {
		return spread
	}
	return fallback
}

func purgePriceTick(contract rpc.ContractParams, price float64) float64 {
	if strings.EqualFold(contract.SecType, "OPT") {
		return 0.01
	}
	return priceTick(price)
}

func purgeLegIDForContract(c rpc.ContractParams) string {
	return purgeLegIDForContractWithMultiplier(c, true)
}

func purgeLegacyLegIDForContract(c rpc.ContractParams) string {
	return purgeLegIDForContractWithMultiplier(c, false)
}

func purgeLegIDForContractWithMultiplier(c rpc.ContractParams, includeMultiplier bool) string {
	if c.ConID > 0 {
		parts := []string{
			strings.ToUpper(strings.TrimSpace(c.Symbol)),
			strings.ToUpper(strings.TrimSpace(c.SecType)),
			strconv.Itoa(c.ConID),
			strings.ToUpper(strings.TrimSpace(c.Currency)),
		}
		if includeMultiplier {
			parts = append(parts, strconv.Itoa(c.Multiplier))
		}
		sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
		return "leg_" + hex.EncodeToString(sum[:])[:12]
	}
	parts := []string{
		strings.ToUpper(strings.TrimSpace(c.Symbol)),
		strings.ToUpper(strings.TrimSpace(c.SecType)),
		strconv.Itoa(c.ConID),
		strings.ToUpper(strings.TrimSpace(c.Exchange)),
		strings.ToUpper(strings.TrimSpace(c.PrimaryExch)),
		strings.ToUpper(strings.TrimSpace(c.Currency)),
		strings.ToUpper(strings.TrimSpace(c.LocalSymbol)),
		strings.ToUpper(strings.TrimSpace(c.TradingClass)),
		strings.TrimSpace(c.Expiry),
		fmt.Sprintf("%.4f", c.Strike),
		strings.ToUpper(strings.TrimSpace(c.Right)),
	}
	if includeMultiplier {
		parts = append(parts, strconv.Itoa(c.Multiplier))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "leg_" + hex.EncodeToString(sum[:])[:12]
}

func purgeOrderRef(now time.Time) string {
	tokenID, err := randomTokenID()
	if err != nil {
		return "purge-" + now.UTC().Format("20060102-150405")
	}
	return "purge-" + now.UTC().Format("20060102-150405") + "-" + tokenID[:8]
}

func purgeJournalEventForDraft(draft rpc.OrderDraft, status rpc.TradingStatus, purgeID, legID string, orderID int, at time.Time) orderJournalEvent {
	return orderJournalEvent{
		At:              at,
		Type:            orderJournalEventSendAttempted,
		OrderRef:        draft.OrderRef,
		ReservedOrderID: orderID,
		ClientID:        status.ClientID,
		Account:         status.Account,
		Endpoint:        status.Endpoint,
		Mode:            status.Mode,
		Source:          purgeExecuteSource,
		PurgeID:         purgeID,
		LegID:           legID,
		BypassPreview:   true,
		Symbol:          draft.Contract.Symbol,
		SecType:         draft.Contract.SecType,
		ConID:           draft.Contract.ConID,
		Exchange:        draft.Contract.Exchange,
		PrimaryExch:     draft.Contract.PrimaryExch,
		Currency:        draft.Contract.Currency,
		LocalSymbol:     draft.Contract.LocalSymbol,
		TradingClass:    draft.Contract.TradingClass,
		Expiry:          draft.Contract.Expiry,
		Strike:          draft.Contract.Strike,
		Right:           draft.Contract.Right,
		Multiplier:      draft.Contract.Multiplier,
		Action:          draft.Action,
		OrderType:       draft.OrderType,
		TIF:             draft.TIF,
		OutsideRTH:      draft.OutsideRTH,
		Quantity:        float64(draft.Quantity),
		LimitPrice:      draft.LimitPrice,
		OpenClose:       draft.OpenClose,
		SendState:       orderSendStateSendAttempted,
		Message:         "purge broker placeOrder transmit attempted",
	}
}

func purgeOrderResult(leg rpc.PurgeExecuteLeg, draft rpc.OrderDraft, orderID int, quote rpc.OrderQuoteSnapshot, sendState, message string) rpc.PurgeExecuteOrder {
	return rpc.PurgeExecuteOrder{
		LegID:           leg.LegID,
		Symbol:          draft.Contract.Symbol,
		SecType:         draft.Contract.SecType,
		Contract:        draft.Contract,
		Action:          draft.Action,
		Quantity:        draft.Quantity,
		LimitPrice:      draft.LimitPrice,
		OrderRef:        draft.OrderRef,
		ReservedOrderID: orderID,
		LifecycleStatus: rpc.OrderLifecyclePendingSubmit,
		SendState:       sendState,
		Message:         message,
		Quote:           quote,
	}
}

func addPurgeSkipped(res *rpc.PurgeExecuteResult, leg rpc.PurgeExecuteLeg, reason string) {
	res.SkippedLegs++
	res.Skipped = append(res.Skipped, rpc.PurgeExecuteSkippedLeg{
		LegID:    leg.LegID,
		Symbol:   leg.Symbol,
		SecType:  leg.SecType,
		Contract: leg.Contract,
		Reason:   reason,
	})
}

func addPurgeError(res *rpc.PurgeExecuteResult, leg rpc.PurgeExecuteLeg, reason string) {
	res.ErrorLegs++
	addPurgeSkipped(res, leg, reason)
}

func (s *Server) refreshPurgeOrderViews(res *rpc.PurgeExecuteResult) {
	views, _, err := s.loadOrderViews()
	if err != nil {
		res.Warnings = append(res.Warnings, "refresh order lifecycle: "+err.Error())
		return
	}
	for i := range res.Orders {
		for _, view := range views {
			if !orderViewMatchesID(view, res.Orders[i].OrderRef) && (res.Orders[i].ReservedOrderID == 0 || view.ReservedOrderID != res.Orders[i].ReservedOrderID) {
				continue
			}
			res.Orders[i].Status = view.Status
			res.Orders[i].LifecycleStatus = view.LifecycleStatus
			res.Orders[i].SendState = view.SendState
			if view.LastMessage != "" {
				res.Orders[i].Message = view.LastMessage
			}
			break
		}
	}
}

func purgeExecuteFinalStatus(res rpc.PurgeExecuteResult) string {
	if res.ErrorLegs > 0 {
		if res.SubmittedLegs > 0 {
			return purgeExecuteStatusPartial
		}
		return purgeExecuteStatusError
	}
	for _, skipped := range res.Skipped {
		if !purgeExecuteSkippedReasonIsIdempotent(skipped.Reason) {
			if res.SubmittedLegs > 0 {
				return purgeExecuteStatusPartial
			}
			return purgeExecuteStatusBlocked
		}
	}
	if res.SubmittedLegs > 0 {
		return purgeExecuteStatusSubmitted
	}
	return purgeExecuteStatusFlat
}

func purgeExecuteSkippedReasonIsIdempotent(reason string) bool {
	switch reason {
	case "already flat",
		"open purge/restore order exists for this ledger row",
		"current quantity already covered by active purge ledger":
		return true
	default:
		return false
	}
}

func purgeExecuteMessage(res rpc.PurgeExecuteResult) string {
	switch res.Status {
	case purgeExecuteStatusSubmitted:
		return fmt.Sprintf("submitted %d purge order(s)", res.SubmittedLegs)
	case purgeExecuteStatusFlat:
		if len(res.Skipped) > 0 {
			return "selected purge legs are already flat or already covered by active purge state"
		}
		return "selected purge legs are already flat"
	case purgeExecuteStatusPartial:
		return fmt.Sprintf("submitted %d purge order(s); %d leg(s) need attention", res.SubmittedLegs, res.SkippedLegs)
	case purgeExecuteStatusError:
		return "purge execution failed before any successful submit"
	default:
		return "purge execution is blocked"
	}
}
