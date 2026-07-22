package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/config"
	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

type orderPositionAuthority struct {
	Impact                 rpc.OrderPositionImpact
	Generation             uint64
	Health                 ibkrlib.PortfolioStreamHealth
	EvidenceAt             time.Time
	BaseCurrency           string
	BaseCurrencyProvenance ibkrlib.AccountBaseCurrencyProvenance
	TestOnly               bool
}

// orderPreviewBrokerAuthority binds every production preview read to one
// daemon-published Connector and one physical broker socket generation. The
// same authority resolves the contract, reads the portfolio, runs WhatIf, and
// is revalidated immediately before the signed token is minted.
type orderPreviewBrokerAuthority struct {
	connector      *ibkrlib.Connector
	connectorEpoch uint64
	session        ibkrlib.ConnectorSessionBinding
}

func (s *Server) hasOrderPreviewBrokerTestSeam() bool {
	return s.orderPreviewQuote != nil || s.orderPreviewPositionImpact != nil || s.orderRiskAuthorityForTest != nil ||
		s.orderContractResolverForTest != nil || s.orderPreviewWhatIf != nil
}

func (s *Server) captureOrderPreviewBrokerAuthority() (*orderPreviewBrokerAuthority, error) {
	s.mu.Lock()
	connector, connectorEpoch := s.connector, s.connectorEpoch
	s.mu.Unlock()
	if connector == nil || !connector.IsReady() {
		if s.hasOrderPreviewBrokerTestSeam() {
			return nil, nil
		}
		s.triggerReconnect()
		return nil, s.gatewayUnavailableError()
	}
	session, ok := connector.CaptureSession()
	if !ok {
		return nil, fmt.Errorf("%w: broker session changed before contract resolution", ErrTradingDisabled)
	}
	authority := &orderPreviewBrokerAuthority{connector: connector, connectorEpoch: connectorEpoch, session: session}
	if !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return nil, fmt.Errorf("%w: broker session changed before contract resolution", ErrTradingDisabled)
	}
	return authority, nil
}

func (s *Server) orderPreviewBrokerAuthorityCurrent(authority *orderPreviewBrokerAuthority) bool {
	if s == nil || authority == nil || authority.connector == nil {
		return false
	}
	s.mu.Lock()
	current := s.connector == authority.connector && s.connectorEpoch == authority.connectorEpoch
	s.mu.Unlock()
	return current && authority.connector.SessionCurrent(authority.session)
}

func (s *Server) resolvePreviewOrderContract(ctx context.Context, authority *orderPreviewBrokerAuthority, contract rpc.ContractParams, timeout time.Duration) (rpc.ContractParams, error) {
	if s.orderContractResolverForTest != nil {
		resolved, err := s.orderContractResolverForTest(ctx, contract, timeout)
		if err != nil {
			return rpc.ContractParams{}, err
		}
		if resolved.ConID <= 0 {
			return rpc.ContractParams{}, fmt.Errorf("%w: contract resolver returned no positive ConID", ErrTradingDisabled)
		}
		return resolved, nil
	}
	// Existing unit tests may replace the complete quote/position/WhatIf
	// authority without constructing a socket. Production never takes this
	// path because no test seam is installed.
	if authority == nil {
		return contract, nil
	}
	resolved, err := authority.connector.ResolveOrderContractForSession(ctx, authority.session, *previewIBKRContract(contract), timeout)
	if err != nil {
		return rpc.ContractParams{}, fmt.Errorf("%w: exact contract resolution failed: %v", ErrTradingDisabled, err)
	}
	result := contract
	result.ConID = resolved.Contract.ConID
	result.Symbol = resolved.Contract.Symbol
	result.SecType = resolved.Contract.SecType
	result.Expiry = resolved.Contract.Expiry
	result.Strike = resolved.Contract.Strike
	result.Right = resolved.Contract.Right
	result.Multiplier = resolved.Contract.Multiplier
	result.Exchange = resolved.Contract.Exchange
	result.PrimaryExch = resolved.Contract.PrimaryExch
	result.Currency = resolved.Contract.Currency
	result.LocalSymbol = resolved.Contract.LocalSymbol
	result.TradingClass = resolved.Contract.TradingClass
	result.MinTick = resolved.MinTick
	if result.ConID <= 0 || !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return rpc.ContractParams{}, fmt.Errorf("%w: broker session changed during contract resolution", ErrTradingDisabled)
	}
	return result, nil
}

func (s *Server) capturePreviewOrderPositionAuthority(ctx context.Context, status rpc.TradingStatus, contract rpc.ContractParams, action string, qty int) (orderPositionAuthority, error) {
	if s.orderRiskAuthorityForTest != nil {
		return s.orderRiskAuthorityForTest(ctx, status, contract, action, qty)
	}
	if s.orderPreviewPositionImpact != nil {
		impact, err := s.orderPreviewPositionImpact(ctx, contract, action, qty)
		if err != nil {
			return orderPositionAuthority{}, err
		}
		now := s.orderNow()
		return orderPositionAuthority{
			Impact: impact, Generation: 1, BaseCurrency: strings.ToUpper(strings.TrimSpace(contract.Currency)), BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag, TestOnly: true,
			Health: ibkrlib.PortfolioStreamHealth{Account: status.Account, InitialCompletedAt: now, LastUpdateAt: now, ProjectionGeneration: 1}, EvidenceAt: now,
		}, nil
	}
	c := s.gatewayConnector()
	if c == nil {
		return orderPositionAuthority{}, s.gatewayUnavailableError()
	}
	session, ok := c.CaptureSession()
	if !ok {
		return orderPositionAuthority{}, fmt.Errorf("%w: broker session changed before portfolio validation", ErrTradingDisabled)
	}
	return s.captureBoundOrderPositionAuthority(ctx, c, session, status, contract, action, qty)
}

func (s *Server) captureBoundOrderPositionAuthority(ctx context.Context, connector *ibkrlib.Connector, session ibkrlib.ConnectorSessionBinding, status rpc.TradingStatus, contract rpc.ContractParams, action string, qty int) (orderPositionAuthority, error) {
	if s.orderRiskAuthorityForTest != nil {
		return s.orderRiskAuthorityForTest(ctx, status, contract, action, qty)
	}
	if s.orderPreviewPositionImpact != nil {
		impact, err := s.orderPreviewPositionImpact(ctx, contract, action, qty)
		if err != nil {
			return orderPositionAuthority{}, err
		}
		now := s.orderNow()
		return orderPositionAuthority{
			Impact: impact, Generation: 1, BaseCurrency: strings.ToUpper(strings.TrimSpace(contract.Currency)), BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag, TestOnly: true,
			Health: ibkrlib.PortfolioStreamHealth{Account: status.Account, InitialCompletedAt: now, LastUpdateAt: now, ProjectionGeneration: 1}, EvidenceAt: now,
		}, nil
	}
	if connector == nil {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	projection, ok := connector.CapturePortfolioProjectionForSession(session)
	if !ok {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	scope := brokerStateScope{Account: status.Account, Mode: status.Mode}
	if health := classifyPortfolioStreamHealth(scope, projection.Health, s.orderNow()); health != orderIntegrityHealthCurrent {
		return orderPositionAuthority{}, fmt.Errorf("%w: current account-scoped portfolio evidence is %s; refresh and preview again", ErrTradingDisabled, health)
	}
	if !cachedPositionsMatchBrokerScope(projection.Positions, scope) {
		return orderPositionAuthority{}, fmt.Errorf("%w: portfolio evidence belongs to another account; refresh and preview again", ErrTradingDisabled)
	}
	before, err := exactRiskPositionQuantity(projection.Positions, contract)
	if err != nil {
		return orderPositionAuthority{}, fmt.Errorf("%w: exact contract position evidence unavailable: %v", ErrTradingDisabled, err)
	}
	delta := float64(qty)
	if strings.EqualFold(action, rpc.OrderActionSell) {
		delta = -delta
	}
	authority := orderPositionAuthority{
		Impact:     rpc.OrderPositionImpact{Before: before, After: before + delta, Effect: classifyPositionEffect(before, before+delta)},
		Generation: projection.Generation, Health: projection.Health,
		EvidenceAt: portfolioStreamEvidenceAsOf(projection.Health),
	}
	// A position-only close/reduce classification does not grant a risk
	// exemption. reqAllOpenOrders cannot prove future manual-TWS activity for
	// this non-client-0 daemon, so another working order may already consume the
	// apparent exit capacity. Every order therefore binds current base-currency
	// evidence and the ordinary size controls; sell-side apparent exits also
	// pass the short/sell-to-open gates below under worst-case exposure.
	// Account base currency is immutable for one concrete broker account and
	// socket session. Capture it from a completed one-shot request inside that
	// session, then prove the session is still current before returning it.
	// Reconnect invalidates the whole binding; an unstamped streaming cache or
	// ExchangeRate=1 inference is never authority for an order cap.
	account, provenance, err := connector.RequestAccountSummaryWithProvenance(ctx, 3*time.Second)
	if err != nil {
		return orderPositionAuthority{}, fmt.Errorf("%w: current account base-currency evidence unavailable: %v", ErrTradingDisabled, err)
	}
	if provenance != ibkrlib.AccountSummaryProvenanceRequest || account == nil || !strings.EqualFold(strings.TrimSpace(account.AccountID), strings.TrimSpace(status.Account)) {
		return orderPositionAuthority{}, fmt.Errorf("%w: current exact-account base-currency evidence unavailable", ErrTradingDisabled)
	}
	base, ok := rulebookBaseCurrency(account.BaseCurrency)
	if !ok || (account.BaseCurrencyProvenance != ibkrlib.AccountBaseCurrencyExplicitTag && account.BaseCurrencyProvenance != ibkrlib.AccountBaseCurrencyValueSuffix) {
		return orderPositionAuthority{}, fmt.Errorf("%w: explicit account base-currency evidence is unavailable (provenance %q)", ErrTradingDisabled, account.BaseCurrencyProvenance)
	}
	if !connector.SessionCurrent(session) {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	authority.BaseCurrency = base
	authority.BaseCurrencyProvenance = account.BaseCurrencyProvenance
	return authority, nil
}

// captureWireOrderPositionAuthority reads only already-bound, in-memory
// evidence. Base currency is the session-invariant value captured again at
// preview-token redemption; the first-byte guard deliberately reuses that
// binding instead of issuing a broker request or reparsing unstamped cache.
// The real transport calls this while WithBoundBrokerSession owns
// publicationBarrier.R and the protected Connection send owns
// evidenceBarrier.W. It must never issue a broker request or recursively take
// either barrier.
func (s *Server) captureWireOrderPositionAuthority(binding brokerWriteTransactionBinding, status rpc.TradingStatus, draft rpc.OrderDraft) (orderPositionAuthority, error) {
	if binding.testOnly && s.orderRiskAuthorityForTest == nil && s.orderPreviewPositionImpact == nil {
		return orderPositionAuthority{
			Impact: binding.riskPosition, Generation: binding.riskPortfolioGeneration,
			Health:       ibkrlib.PortfolioStreamHealth{Account: binding.riskPortfolioAccount, ProjectionGeneration: binding.riskPortfolioGeneration},
			BaseCurrency: binding.riskBaseCurrency, BaseCurrencyProvenance: binding.riskBaseCurrencyProvenance, TestOnly: true,
		}, nil
	}
	if s.orderRiskAuthorityForTest != nil || s.orderPreviewPositionImpact != nil {
		return s.captureBoundOrderPositionAuthority(context.Background(), binding.connector, binding.session, status, draft.Contract, draft.Action, draft.Quantity)
	}
	if binding.connector == nil {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	projection, ok := binding.connector.CapturePortfolioProjectionForBoundSession(binding.session)
	if !ok {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	scope := brokerStateScope{Account: status.Account, Mode: status.Mode}
	if health := classifyPortfolioStreamHealth(scope, projection.Health, s.orderNow()); health != orderIntegrityHealthCurrent {
		return orderPositionAuthority{}, fmt.Errorf("%w: current account-scoped portfolio evidence is %s; refresh and preview again", ErrTradingDisabled, health)
	}
	if !cachedPositionsMatchBrokerScope(projection.Positions, scope) {
		return orderPositionAuthority{}, fmt.Errorf("%w: portfolio evidence belongs to another account; refresh and preview again", ErrTradingDisabled)
	}
	before, err := exactRiskPositionQuantity(projection.Positions, draft.Contract)
	if err != nil {
		return orderPositionAuthority{}, fmt.Errorf("%w: exact contract position evidence unavailable: %v", ErrTradingDisabled, err)
	}
	delta := float64(draft.Quantity)
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) {
		delta = -delta
	}
	return orderPositionAuthority{
		Impact: rpc.OrderPositionImpact{
			Before: before, After: before + delta,
			Effect: classifyPositionEffect(before, before+delta),
		},
		Generation: projection.Generation,
		Health:     projection.Health, EvidenceAt: portfolioStreamEvidenceAsOf(projection.Health),
		BaseCurrency: binding.riskBaseCurrency, BaseCurrencyProvenance: binding.riskBaseCurrencyProvenance,
	}, nil
}

// exactRiskPositionQuantity deliberately has no symbol/fallback matching.
// A broker-positive ConID is required to classify current effect truthfully,
// but position evidence alone never grants a close/reduce exemption. Same-
// symbol instruments, ticker reuse, malformed
// zero IDs, duplicate rows, or conflicting secType/currency evidence fail
// closed instead of being aggregated into another contract's position.
func exactRiskPositionQuantity(positions []*ibkrlib.RawPosition, contract rpc.ContractParams) (float64, error) {
	if contract.ConID <= 0 {
		return 0, fmt.Errorf("contract ConID must be positive")
	}
	wantSecType := strings.ToUpper(strings.TrimSpace(contract.SecType))
	wantCurrency := strings.ToUpper(strings.TrimSpace(contract.Currency))
	if wantSecType == "" || wantCurrency == "" {
		return 0, fmt.Errorf("contract secType and currency are required")
	}
	wantSymbol := strings.ToUpper(strings.TrimSpace(contract.Symbol))
	matched := false
	var quantity float64
	for _, position := range positions {
		if position == nil || position.Position == 0 {
			continue
		}
		posSecType := strings.ToUpper(strings.TrimSpace(position.Contract.SecType))
		posSymbol := strings.ToUpper(strings.TrimSpace(position.Contract.Symbol))
		if position.Contract.ConID <= 0 {
			if posSymbol != "" && posSymbol == wantSymbol && riskSecTypeConsistent(wantSecType, posSecType) {
				return 0, fmt.Errorf("same-symbol portfolio row has no positive ConID")
			}
			continue
		}
		if position.Contract.ConID != contract.ConID {
			continue
		}
		if !riskSecTypeConsistent(wantSecType, posSecType) {
			return 0, fmt.Errorf("ConID %d has conflicting secType %q", contract.ConID, position.Contract.SecType)
		}
		posCurrency := strings.ToUpper(strings.TrimSpace(position.Contract.Currency))
		if posCurrency == "" || posCurrency != wantCurrency {
			return 0, fmt.Errorf("ConID %d has conflicting currency %q", contract.ConID, position.Contract.Currency)
		}
		if wantSymbol != "" && posSymbol != "" && posSymbol != wantSymbol {
			return 0, fmt.Errorf("ConID %d has conflicting symbol %q", contract.ConID, position.Contract.Symbol)
		}
		if matched {
			return 0, fmt.Errorf("ConID %d appears in duplicate portfolio rows", contract.ConID)
		}
		matched = true
		quantity = position.Position
	}
	return quantity, nil
}

func riskSecTypeConsistent(want, got string) bool {
	want = strings.ToUpper(strings.TrimSpace(want))
	got = strings.ToUpper(strings.TrimSpace(got))
	if want == got {
		return true
	}
	// TWS reports exchange-traded funds as STK contracts even when the public
	// order surface preserves ETF as the trader-facing classification.
	return (want == "ETF" && got == "STK") || (want == "STK" && got == "ETF")
}

func validateOrderRiskAuthority(cfg config.Trading, draft rpc.OrderDraft, position rpc.OrderPositionImpact, notional float64, baseCurrency string) error {
	cfg = cfg.WithDefaults()
	contractCurrency := strings.ToUpper(strings.TrimSpace(draft.Contract.Currency))
	baseCurrency = strings.ToUpper(strings.TrimSpace(baseCurrency))
	if contractCurrency == "" || baseCurrency == "" || contractCurrency != baseCurrency {
		return fmt.Errorf("risk-increasing order currency %q cannot be compared with account-base max_notional currency %q without current typed FX evidence", contractCurrency, baseCurrency)
	}
	if strings.EqualFold(draft.Contract.SecType, "OPT") && draft.Quantity > cfg.MaxOptionContracts {
		return fmt.Errorf("option quantity %d exceeds [trading].max_option_contracts %d", draft.Quantity, cfg.MaxOptionContracts)
	}
	if notional > cfg.MaxNotional {
		return fmt.Errorf("order notional %.2f %s exceeds [trading].max_notional %.2f %s", notional, baseCurrency, cfg.MaxNotional, baseCurrency)
	}
	riskEffect := position.Effect
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) && isRiskReducing(riskEffect) {
		// Incomplete manual-order visibility means the apparent long exit may
		// arrive after another sell consumed that capacity. Apply the same
		// explicit short-opening permission as a zero-position sell.
		riskEffect = rpc.OrderPositionEffectOpenShort
	}
	switch {
	case isStockLikeRiskSecType(draft.Contract.SecType) && stockShortOrFlip(riskEffect) && !cfg.AllowStockShort:
		return fmt.Errorf("stock short/flip requires [trading].allow_stock_short = true")
	case strings.EqualFold(draft.Contract.SecType, "OPT") && optionSellToOpen(draft.Action, riskEffect) && !cfg.AllowOptionSellToOpen:
		return fmt.Errorf("option sell-to-open requires [trading].allow_option_sell_to_open = true")
	}
	return nil
}

func isStockLikeRiskSecType(secType string) bool {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "ETF":
		return true
	default:
		return false
	}
}

func sameOrderPositionImpact(a, b rpc.OrderPositionImpact) bool {
	return a.Before == b.Before && a.After == b.After && a.Effect == b.Effect
}

// bindPreviewOrderRiskAuthority revalidates the signed preview against one
// exact broker session and the current controls before any broker ID is
// reserved or token is consumed. Drift always asks for a new preview/WhatIf;
// it never silently rewrites OpenClose or adapts the signed quantity.
func (s *Server) bindPreviewOrderRiskAuthority(ctx context.Context, binding *brokerWriteTransactionBinding, status rpc.TradingStatus, payload orderPreviewTokenPayload, draft rpc.OrderDraft) error {
	if binding == nil {
		return brokerWriteTransactionDriftError()
	}
	cfg, controlGeneration := s.effectiveTradingControlSnapshot()
	if controlGeneration != binding.tradingControlGeneration || controlGeneration != payload.TradingControlGeneration {
		return fmt.Errorf("%w: trading controls changed after preview; preview again", ErrTradingDisabled)
	}
	var current orderPositionAuthority
	var err error
	if binding.testOnly && s.orderRiskAuthorityForTest == nil && s.orderPreviewPositionImpact == nil {
		current = orderPositionAuthority{
			Impact: payload.Position, Generation: payload.PortfolioGeneration,
			Health:       ibkrlib.PortfolioStreamHealth{Account: payload.PortfolioAccount, ProjectionGeneration: payload.PortfolioGeneration},
			EvidenceAt:   payload.PortfolioEvidenceAt,
			BaseCurrency: payload.BaseCurrency, TestOnly: true,
			BaseCurrencyProvenance: payload.BaseCurrencyProvenance,
		}
		if current.Generation == 0 {
			current.Generation = 1
			current.Health.ProjectionGeneration = 1
		}
		if current.Health.Account == "" {
			current.Health.Account = status.Account
		}
		if current.BaseCurrency == "" {
			current.BaseCurrency = strings.ToUpper(strings.TrimSpace(draft.Contract.Currency))
			current.BaseCurrencyProvenance = ibkrlib.AccountBaseCurrencyExplicitTag
		}
	} else {
		current, err = s.captureBoundOrderPositionAuthority(ctx, binding.connector, binding.session, status, draft.Contract, draft.Action, draft.Quantity)
		if err != nil {
			return err
		}
	}
	expectedGeneration := payload.PortfolioGeneration
	expectedAccount := payload.PortfolioAccount
	expectedBase := payload.BaseCurrency
	expectedBaseProvenance := payload.BaseCurrencyProvenance
	expectedImpact := payload.Position
	// Focused test fixtures that mint payloads directly predate the v3
	// authority fields. Production v3 tokens are minted only by previewOrder
	// and always carry all three; this compatibility branch is reachable only
	// through an explicit in-process position seam.
	if current.TestOnly && expectedGeneration == 0 && expectedAccount == "" && expectedBase == "" {
		expectedGeneration = current.Generation
		expectedAccount = current.Health.Account
		expectedBase = current.BaseCurrency
		expectedBaseProvenance = current.BaseCurrencyProvenance
		expectedImpact = current.Impact
	}
	if current.Generation != expectedGeneration ||
		!strings.EqualFold(strings.TrimSpace(current.Health.Account), strings.TrimSpace(expectedAccount)) ||
		!sameOrderPositionImpact(current.Impact, expectedImpact) ||
		!strings.EqualFold(strings.TrimSpace(current.BaseCurrency), strings.TrimSpace(expectedBase)) ||
		current.BaseCurrencyProvenance != expectedBaseProvenance {
		return fmt.Errorf("%w: portfolio risk authority changed after preview; preview again", ErrTradingDisabled)
	}
	if err := validateOrderRiskAuthority(cfg, draft, current.Impact, payload.Notional, current.BaseCurrency); err != nil {
		return fmt.Errorf("%w: current trading controls reject the order: %v", ErrTradingDisabled, err)
	}
	binding.riskBound = true
	binding.riskDraft = draft
	binding.riskPosition = current.Impact
	binding.riskPortfolioGeneration = current.Generation
	binding.riskPortfolioAccount = current.Health.Account
	binding.riskBaseCurrency = current.BaseCurrency
	binding.riskBaseCurrencyProvenance = current.BaseCurrencyProvenance
	binding.riskNotional = payload.Notional
	return nil
}
