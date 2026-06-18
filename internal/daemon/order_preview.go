package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
)

const (
	orderPreviewTokenVersion = 1
	orderPreviewTokenPrefix  = "ibkrp1"
	orderPreviewTokenTTL     = 10 * time.Minute
	orderPreviewKeyBytes     = 32
	orderPreviewDefaultWait  = 5 * time.Second
	orderPreviewMaxWait      = 3 * time.Minute
)

type orderPreviewTokenPayload struct {
	Version      int                       `json:"version"`
	TokenID      string                    `json:"token_id"`
	Scope        string                    `json:"scope"`
	IssuedAt     time.Time                 `json:"issued_at"`
	ExpiresAt    time.Time                 `json:"expires_at"`
	Mode         string                    `json:"mode"`
	Account      string                    `json:"account"`
	Endpoint     string                    `json:"endpoint"`
	ClientID     int                       `json:"client_id"`
	Draft        rpc.OrderDraft            `json:"draft"`
	Quote        rpc.OrderQuoteSnapshot    `json:"quote"`
	Position     rpc.OrderPositionImpact   `json:"position"`
	Notional     float64                   `json:"notional"`
	WhatIf       rpc.OrderWhatIfResult     `json:"what_if"`
	WhatIfStatus string                    `json:"what_if_status,omitempty"`
	Replace      orderPreviewReplaceTarget `json:"replace"`
}

type orderPreviewReplaceTarget struct {
	OrderRef        string              `json:"order_ref,omitempty"`
	ReservedOrderID int                 `json:"reserved_order_id,omitempty"`
	PermID          int                 `json:"perm_id,omitempty"`
	ClientID        int                 `json:"client_id,omitempty"`
	Account         string              `json:"account,omitempty"`
	Endpoint        string              `json:"endpoint,omitempty"`
	Mode            string              `json:"mode,omitempty"`
	Status          string              `json:"status,omitempty"`
	LifecycleStatus string              `json:"lifecycle_status,omitempty"`
	OrderType       string              `json:"order_type,omitempty"`
	TIF             string              `json:"tif,omitempty"`
	TriggerMethod   int                 `json:"trigger_method,omitempty"`
	Quantity        float64             `json:"quantity,omitempty"`
	Filled          float64             `json:"filled,omitempty"`
	Remaining       float64             `json:"remaining,omitempty"`
	LimitPrice      float64             `json:"limit_price,omitempty"`
	Trail           *rpc.OrderTrailSpec `json:"trail,omitempty"`
	OutsideRTH      bool                `json:"outside_rth,omitempty"`
}

type orderTokenSigner struct {
	key []byte
	now func() time.Time
}

func defaultOrderTokenKeyPath() (string, error) {
	return defaultTradingStatePath("order-preview-key")
}

func newOrderTokenSigner(path string, now func() time.Time) (*orderTokenSigner, error) {
	key, err := loadOrCreateOrderTokenKey(path)
	if err != nil {
		return nil, err
	}
	return &orderTokenSigner{key: key, now: now}, nil
}

func loadOrCreateOrderTokenKey(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("order preview token key path is empty")
	}
	data, err := os.ReadFile(path)
	if err == nil {
		key, decErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(data)))
		if decErr != nil {
			return nil, fmt.Errorf("decode order preview token key: %w", decErr)
		}
		if len(key) != orderPreviewKeyBytes {
			return nil, fmt.Errorf("order preview token key has %d bytes, want %d", len(key), orderPreviewKeyBytes)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read order preview token key: %w", err)
	}
	key := make([]byte, orderPreviewKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate order preview token key: %w", err)
	}
	encoded := []byte(base64.RawURLEncoding.EncodeToString(key) + "\n")
	if err := writePrivateStateAtomic(path, encoded); err != nil {
		return nil, fmt.Errorf("write order preview token key: %w", err)
	}
	return key, nil
}

func (s *orderTokenSigner) mint(payload orderPreviewTokenPayload) (token string, tokenID string, expiresAt time.Time, err error) {
	if s == nil || len(s.key) == 0 {
		return "", "", time.Time{}, fmt.Errorf("order preview token signer is not configured")
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	tokenID = payload.TokenID
	if tokenID == "" {
		tokenID, err = randomTokenID()
		if err != nil {
			return "", "", time.Time{}, err
		}
	}
	payload.Version = orderPreviewTokenVersion
	payload.TokenID = tokenID
	if payload.Scope == "" {
		payload.Scope = rpc.OrderTokenScopePlace
	}
	switch payload.Scope {
	case rpc.OrderTokenScopePlace, rpc.OrderTokenScopeModify:
	default:
		return "", "", time.Time{}, fmt.Errorf("unsupported order preview token scope %q", payload.Scope)
	}
	payload.IssuedAt = now
	if payload.ExpiresAt.IsZero() {
		payload.ExpiresAt = now.Add(orderPreviewTokenTTL)
	}
	expiresAt = payload.ExpiresAt
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("marshal order preview token: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return orderPreviewTokenPrefix + "." + body + "." + sig, tokenID, expiresAt, nil
}

func (s *orderTokenSigner) verify(token string) (orderPreviewTokenPayload, error) {
	if s == nil || len(s.key) == 0 {
		return orderPreviewTokenPayload{}, fmt.Errorf("order preview token signer is not configured")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != orderPreviewTokenPrefix {
		return orderPreviewTokenPayload{}, fmt.Errorf("invalid order preview token shape")
	}
	body := parts[1]
	wantMAC := hmac.New(sha256.New, s.key)
	wantMAC.Write([]byte(body))
	wantSig := wantMAC.Sum(nil)
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("decode order preview token signature: %w", err)
	}
	if !hmac.Equal(gotSig, wantSig) {
		return orderPreviewTokenPayload{}, fmt.Errorf("invalid order preview token signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("decode order preview token payload: %w", err)
	}
	var payload orderPreviewTokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("decode order preview token payload: %w", err)
	}
	if payload.Version != orderPreviewTokenVersion {
		return orderPreviewTokenPayload{}, fmt.Errorf("unsupported order preview token version %d", payload.Version)
	}
	switch payload.Scope {
	case rpc.OrderTokenScopePlace, rpc.OrderTokenScopeModify:
	default:
		return orderPreviewTokenPayload{}, fmt.Errorf("unsupported order preview token scope %q", payload.Scope)
	}
	if payload.WhatIf.Status == "" && payload.WhatIfStatus != "" {
		payload.WhatIf.Status = payload.WhatIfStatus
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	if !payload.ExpiresAt.IsZero() && !now.Before(payload.ExpiresAt) {
		return orderPreviewTokenPayload{}, fmt.Errorf("order preview token expired at %s", payload.ExpiresAt.Format(time.RFC3339))
	}
	return payload, nil
}

func randomTokenID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate order preview token id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func (s *Server) handleOrderPreview(ctx context.Context, req *rpc.Request) (*rpc.OrderPreviewResult, error) {
	var p rpc.OrderPreviewParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.previewOrder(ctx, p)
}

func (s *Server) previewOrder(ctx context.Context, p rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	if s == nil {
		return nil, ErrTradingDisabled
	}
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.tradingStatus(ep)
	if status.Mode == config.TradingModeDisabled {
		return nil, fmt.Errorf("%w: set [trading].mode to paper or live before order preview", ErrTradingDisabled)
	}
	if status.Blocked {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(status.Blockers))
	}
	if s.orderTokens == nil {
		return nil, fmt.Errorf("%w: order preview token signer is unavailable", ErrTradingDisabled)
	}
	scope := rpc.OrderTokenScopePlace
	var replaceView rpc.OrderView
	replaceID := strings.TrimSpace(p.ReplaceID)
	if replaceID != "" {
		if !status.CanWrite {
			return nil, fmt.Errorf("%w: broker modify is not available", ErrTradingDisabled)
		}
		view, err := s.openOrderViewForWrite(replaceID, status)
		if err != nil {
			return nil, err
		}
		if !view.ModifyEligible {
			return nil, errBadRequest("replacement preview target is not modify-eligible")
		}
		scope = rpc.OrderTokenScopeModify
		replaceView = view
	}

	cfg := s.effectiveTradingConfig()
	action, err := normalizeOrderAction(p.Action)
	if err != nil {
		return nil, err
	}
	contract, err := normalizePreviewContract(p.Contract)
	if err != nil {
		return nil, err
	}
	if contract.MinTick <= 0 {
		contract.MinTick = s.resolveContractMinTick(ctx, contract, previewMinTickTimeout)
	}
	if p.Quantity <= 0 {
		return nil, errBadRequest("quantity must be positive")
	}
	// Position impact resolves before the size caps because the caps are
	// intent-aware: they bind risk-increasing orders only. A failed
	// position read fails the preview right here, so the exemption below
	// never runs on unknown intent (fail closed).
	position, err := s.fetchPreviewPositionImpact(ctx, contract, action, p.Quantity)
	if err != nil {
		return nil, err
	}
	riskReducing := isRiskReducing(position.Effect)
	if !riskReducing && contract.SecType == "OPT" && p.Quantity > cfg.MaxOptionContracts {
		return nil, errBadRequest(fmt.Sprintf("option quantity %d exceeds [trading].max_option_contracts %d", p.Quantity, cfg.MaxOptionContracts))
	}
	orderType := strings.ToUpper(strings.TrimSpace(p.OrderType))
	if orderType == "" {
		orderType = rpc.OrderTypeLMT
	}
	if !previewSupportedOrderType(orderType) {
		return nil, errBadRequest("order preview supports LMT, TRAIL, and TRAIL LIMIT orders only")
	}
	tif := strings.ToUpper(strings.TrimSpace(p.TIF))
	if tif == "" && scope == rpc.OrderTokenScopeModify {
		// Modify previews freeze TIF to the open order; defaulting to DAY here
		// would reject every GTC protective-trail replacement at the draft gate.
		tif = strings.ToUpper(strings.TrimSpace(replaceView.TIF))
	}
	if tif == "" {
		tif = rpc.OrderTIFDay
	}
	if tif != rpc.OrderTIFDay && !(tif == rpc.OrderTIFGTC && isTrailOrderType(orderType)) {
		return nil, errBadRequest("order preview supports DAY time-in-force, or GTC for TRAIL and TRAIL LIMIT orders")
	}
	triggerMethod, err := previewOrderTriggerMethod(orderType, contract, p.TriggerMethod)
	if err != nil {
		return nil, err
	}
	if scope == rpc.OrderTokenScopeModify && p.TriggerMethod == rpc.OrderTriggerMethodDefault {
		triggerMethod = replaceView.TriggerMethod
	}

	timeout := orderPreviewTimeout(p.TimeoutMs)
	quote, err := s.fetchPreviewQuote(ctx, contract, timeout)
	if err != nil {
		return nil, err
	}
	strategy, limit, trail, notionalPrice, err := previewOrderPricing(action, orderType, p.Strategy, p.LimitPrice, p.Trail, contract, quote)
	if err != nil {
		return nil, err
	}
	notional := float64(p.Quantity) * notionalPrice * float64(contractMultiplier(contract))
	if !riskReducing && notional > cfg.MaxNotional {
		return nil, errBadRequest(fmt.Sprintf("order notional %.2f exceeds [trading].max_notional %.2f", notional, cfg.MaxNotional))
	}
	switch {
	case contract.SecType == "STK" && stockShortOrFlip(position.Effect) && !cfg.AllowStockShort:
		return nil, errBadRequest("stock short/flip previews require [trading].allow_stock_short = true")
	case contract.SecType == "OPT" && optionSellToOpen(action, position.Effect) && !cfg.AllowOptionSellToOpen:
		return nil, errBadRequest("option sell-to-open previews require [trading].allow_option_sell_to_open = true")
	}

	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	draft := rpc.OrderDraft{
		Action:        action,
		Contract:      contract,
		Quantity:      p.Quantity,
		OrderType:     orderType,
		LimitPrice:    limit,
		Trail:         trail,
		TriggerMethod: triggerMethod,
		TIF:           tif,
		OutsideRTH:    p.OutsideRTH,
		Strategy:      strategy,
		OrderRef:      previewOrderRef(now),
		OpenClose:     orderOpenCloseForEffect(position.Effect),
		Source:        strings.TrimSpace(p.Source),
	}
	if scope == rpc.OrderTokenScopeModify {
		if err := validateModifyDraft(replaceView, draft); err != nil {
			return nil, err
		}
		draft.Contract = modifyContractForView(replaceView, draft.Contract)
	}
	var whatIf rpc.OrderWhatIfResult
	if scope == rpc.OrderTokenScopeModify {
		whatIf, err = s.fetchModifyPreviewWhatIf(ctx, status, replaceView, draft, timeout)
	} else {
		whatIf, err = s.fetchPreviewWhatIf(ctx, status, draft, timeout)
	}
	if err != nil {
		return nil, err
	}
	token, tokenID, expiresAt, err := s.orderTokens.mint(orderPreviewTokenPayload{
		Scope:        scope,
		Mode:         status.Mode,
		Account:      status.Account,
		Endpoint:     status.Endpoint,
		ClientID:     status.ClientID,
		Draft:        draft,
		Quote:        quote,
		Position:     position,
		Notional:     notional,
		WhatIf:       whatIf,
		WhatIfStatus: whatIf.Status,
		Replace:      replaceTargetFromView(replaceView),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTradingDisabled, err)
	}
	if err := s.orderJournal.Append(orderJournalEvent{
		At:             now,
		Type:           orderJournalEventPreviewed,
		OrderRef:       draft.OrderRef,
		PreviewTokenID: tokenID,
		ClientID:       status.ClientID,
		Account:        status.Account,
		Endpoint:       status.Endpoint,
		Mode:           status.Mode,
		Symbol:         draft.Contract.Symbol,
		SecType:        draft.Contract.SecType,
		ConID:          draft.Contract.ConID,
		Exchange:       draft.Contract.Exchange,
		PrimaryExch:    draft.Contract.PrimaryExch,
		Currency:       draft.Contract.Currency,
		LocalSymbol:    draft.Contract.LocalSymbol,
		TradingClass:   draft.Contract.TradingClass,
		Expiry:         draft.Contract.Expiry,
		Strike:         draft.Contract.Strike,
		Right:          draft.Contract.Right,
		Multiplier:     draft.Contract.Multiplier,
		Action:         draft.Action,
		OrderType:      draft.OrderType,
		TIF:            draft.TIF,
		TriggerMethod:  draft.TriggerMethod,
		OutsideRTH:     draft.OutsideRTH,
		Quantity:       float64(draft.Quantity),
		LimitPrice:     draft.LimitPrice,
		Trail:          cloneTrailSpec(draft.Trail),
		OpenClose:      draft.OpenClose,
		Source:         draft.Source,
		Message:        previewWhatIfJournalMessage(whatIf),
	}); err != nil {
		return nil, fmt.Errorf("%w: append preview journal: %v", ErrTradingDisabled, err)
	}

	warnings := append([]rpc.DataWarning{}, quote.Warnings...)
	warnings = append(warnings, previewWhatIfWarnings(whatIf)...)
	if p.OutsideRTH {
		warnings = append(warnings, rpc.DataWarning{
			Code:     "outside_rth_requested",
			Severity: "info",
			Message:  "outside_rth=true was explicitly requested.",
		})
	}
	tokenMinted := token != "" && tokenID != ""
	submitEligible := tokenMinted && whatIf.Status == rpc.OrderWhatIfStatusAccepted && !whatIf.RequiredForSubmit

	maxNotional := cfg.MaxNotional
	if riskReducing {
		// A reduce-only preview was not checked against the cap; echoing
		// the configured value would claim a gate that did not bind.
		// omitempty drops the zero from the wire result.
		maxNotional = 0
	}
	return &rpc.OrderPreviewResult{
		PreviewToken:          token,
		PreviewTokenID:        tokenID,
		PreviewTokenScope:     scope,
		PreviewTokenExpiresAt: expiresAt,
		TokenMinted:           tokenMinted,
		SubmitEligible:        submitEligible,
		Executable:            submitEligible,
		Mode:                  status.Mode,
		Account:               status.Account,
		Endpoint:              status.Endpoint,
		ClientID:              status.ClientID,
		Draft:                 draft,
		Quote:                 quote,
		Position:              position,
		Notional:              notional,
		MaxNotional:           maxNotional,
		WhatIf:                whatIf,
		Warnings:              warnings,
		AsOf:                  now,
	}, nil
}

func (s *Server) fetchPreviewQuote(ctx context.Context, contract rpc.ContractParams, timeout time.Duration) (rpc.OrderQuoteSnapshot, error) {
	if s.orderPreviewQuote != nil {
		return s.orderPreviewQuote(ctx, contract, timeout)
	}
	return s.previewQuote(ctx, contract, timeout)
}

func (s *Server) fetchPreviewPositionImpact(ctx context.Context, contract rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
	if s.orderPreviewPositionImpact != nil {
		return s.orderPreviewPositionImpact(ctx, contract, action, qty)
	}
	return s.previewPositionImpact(ctx, contract, action, qty)
}

func orderPreviewTimeout(timeoutMs int) time.Duration {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		return orderPreviewDefaultWait
	}
	return min(timeout, orderPreviewMaxWait)
}

func (s *Server) fetchPreviewWhatIf(ctx context.Context, status rpc.TradingStatus, draft rpc.OrderDraft, timeout time.Duration) (rpc.OrderWhatIfResult, error) {
	whatIfCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if s.orderPreviewWhatIf != nil {
		return s.orderPreviewWhatIf(whatIfCtx, draft)
	}
	c := s.gatewayConnector()
	if c == nil {
		return previewWhatIfUnavailableWithMessage("Broker WhatIf unavailable: gateway connector is not ready."), nil
	}
	result, err := c.PreviewOrderWhatIf(whatIfCtx, previewIBKRContract(draft.Contract), previewIBKROrderForStatus(draft, status))
	if err != nil {
		return rpc.OrderWhatIfResult{}, err
	}
	return rpcWhatIfResultFromBroker(result), nil
}

func (s *Server) fetchModifyPreviewWhatIf(ctx context.Context, status rpc.TradingStatus, view rpc.OrderView, draft rpc.OrderDraft, timeout time.Duration) (rpc.OrderWhatIfResult, error) {
	whatIfCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if s.orderPreviewWhatIf != nil {
		return s.orderPreviewWhatIf(whatIfCtx, draft)
	}
	c := s.gatewayConnector()
	if c == nil {
		return previewWhatIfUnavailableWithMessage("Broker WhatIf unavailable: gateway connector is not ready."), nil
	}
	result, err := c.PreviewOrderWhatIfWithOrderID(whatIfCtx, previewIBKRContract(draft.Contract), previewIBKROrderForStatus(draft, status), view.ReservedOrderID)
	if err != nil {
		return rpc.OrderWhatIfResult{}, err
	}
	return rpcWhatIfResultFromBroker(result), nil
}

func previewIBKRContract(contract rpc.ContractParams) *ibkrlib.Contract {
	secType := strings.ToUpper(strings.TrimSpace(contract.SecType))
	if secType == "" {
		secType = "STK"
	}
	exchange := strings.TrimSpace(contract.Exchange)
	if exchange == "" {
		exchange = "SMART"
	}
	currency := strings.ToUpper(strings.TrimSpace(contract.Currency))
	if currency == "" {
		currency = "USD"
	}
	multiplier := contract.Multiplier
	if secType != "OPT" {
		multiplier = 0
	}
	return &ibkrlib.Contract{
		ConID:        contract.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(contract.Symbol)),
		SecType:      secType,
		Exchange:     exchange,
		PrimaryExch:  strings.TrimSpace(contract.PrimaryExch),
		Currency:     currency,
		LocalSymbol:  strings.TrimSpace(contract.LocalSymbol),
		TradingClass: strings.TrimSpace(contract.TradingClass),
		Expiry:       strings.TrimSpace(contract.Expiry),
		Strike:       contract.Strike,
		Right:        strings.ToUpper(strings.TrimSpace(contract.Right)),
		Multiplier:   multiplier,
	}
}

func previewIBKROrder(draft rpc.OrderDraft) *ibkrlib.RawOrder {
	order := &ibkrlib.RawOrder{
		Action:        strings.ToUpper(strings.TrimSpace(draft.Action)),
		TotalQty:      draft.Quantity,
		OrderType:     strings.ToUpper(strings.TrimSpace(draft.OrderType)),
		LmtPrice:      draft.LimitPrice,
		TIF:           strings.ToUpper(strings.TrimSpace(draft.TIF)),
		TriggerMethod: draft.TriggerMethod,
		OrderRef:      draft.OrderRef,
		OutsideRth:    draft.OutsideRTH,
		OpenClose:     strings.ToUpper(strings.TrimSpace(draft.OpenClose)),
	}
	if draft.Trail != nil {
		order.TrailStopPrice = draft.Trail.InitialStopPrice
		if draft.Trail.TrailingPercent != nil {
			order.TrailingPercent = *draft.Trail.TrailingPercent
		}
		if draft.Trail.TrailingAmount != nil {
			order.AuxPrice = *draft.Trail.TrailingAmount
		}
		if draft.Trail.LimitOffset != nil {
			order.LmtPriceOffset = *draft.Trail.LimitOffset
		}
	}
	return order
}

func previewIBKROrderForStatus(draft rpc.OrderDraft, status rpc.TradingStatus) *ibkrlib.RawOrder {
	order := previewIBKROrder(draft)
	order.Account = status.Account
	order.ClientID = status.ClientID
	return order
}

func rpcWhatIfResultFromBroker(result ibkrlib.OrderWhatIfResult) rpc.OrderWhatIfResult {
	switch result.Status {
	case ibkrlib.OrderWhatIfStatusAccepted:
		return rpc.OrderWhatIfResult{
			Status:            rpc.OrderWhatIfStatusAccepted,
			RequiredForSubmit: false,
			Available:         true,
			Message:           strings.TrimSpace(result.Message),
			Margin:            rpcMarginFromBroker(result.Margin),
		}
	case ibkrlib.OrderWhatIfStatusRejected:
		msg := strings.TrimSpace(result.Message)
		if msg == "" {
			msg = "Broker WhatIf rejected this order draft."
		}
		return rpc.OrderWhatIfResult{
			Status:             rpc.OrderWhatIfStatusRejected,
			RequiredForSubmit:  true,
			Available:          true,
			Message:            msg,
			AdvancedRejectJSON: strings.TrimSpace(result.AdvancedRejectJSON),
			Action:             "Adjust the draft and run order preview again before any submit attempt.",
			Margin:             rpcMarginFromBroker(result.Margin),
		}
	default:
		msg := strings.TrimSpace(result.Message)
		if msg == "" {
			msg = "Broker WhatIf did not return an accepted preview."
		}
		return previewWhatIfUnavailableWithMessage(msg)
	}
}

func rpcMarginFromBroker(in ibkrlib.OrderWhatIfMargin) *rpc.OrderMarginImpact {
	out := &rpc.OrderMarginImpact{
		Currency:                in.Currency,
		InitialMarginBefore:     cloneFloat64Ptr(in.InitialMarginBefore),
		InitialMarginAfter:      cloneFloat64Ptr(in.InitialMarginAfter),
		MaintenanceMarginBefore: cloneFloat64Ptr(in.MaintenanceMarginBefore),
		MaintenanceMarginAfter:  cloneFloat64Ptr(in.MaintenanceMarginAfter),
		EquityWithLoanBefore:    cloneFloat64Ptr(in.EquityWithLoanBefore),
		EquityWithLoanAfter:     cloneFloat64Ptr(in.EquityWithLoanAfter),
		Commission:              cloneFloat64Ptr(in.Commission),
		MinCommission:           cloneFloat64Ptr(in.MinCommission),
		MaxCommission:           cloneFloat64Ptr(in.MaxCommission),
		CommissionCurrency:      in.CommissionCurrency,
		WarningText:             in.WarningText,
	}
	if out.Currency == "" &&
		out.InitialMarginBefore == nil &&
		out.InitialMarginAfter == nil &&
		out.MaintenanceMarginBefore == nil &&
		out.MaintenanceMarginAfter == nil &&
		out.EquityWithLoanBefore == nil &&
		out.EquityWithLoanAfter == nil &&
		out.Commission == nil &&
		out.MinCommission == nil &&
		out.MaxCommission == nil &&
		out.CommissionCurrency == "" &&
		out.WarningText == "" {
		return nil
	}
	return out
}

func cloneFloat64Ptr(in *float64) *float64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func firstTradingBlockerMessage(blockers []rpc.TradingBlocker) string {
	if len(blockers) == 0 {
		return "trading gate is blocked"
	}
	return blockers[0].Message
}

func normalizeOrderAction(action string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(action)) {
	case "BUY":
		return rpc.OrderActionBuy, nil
	case "SELL":
		return rpc.OrderActionSell, nil
	default:
		return "", errBadRequest("action must be buy or sell")
	}
}

func normalizePreviewContract(in rpc.ContractParams) (rpc.ContractParams, error) {
	secType := strings.ToUpper(strings.TrimSpace(in.SecType))
	if secType == "" {
		secType = "STK"
	}
	switch secType {
	case "STK", "ETF":
		_, echo, _, err := normaliseStockQuoteContract(rpc.ContractParams{
			ConID:        in.ConID,
			Symbol:       in.Symbol,
			SecType:      secType,
			Market:       in.Market,
			Exchange:     in.Exchange,
			PrimaryExch:  in.PrimaryExch,
			Currency:     in.Currency,
			LocalSymbol:  in.LocalSymbol,
			TradingClass: in.TradingClass,
			Multiplier:   in.Multiplier,
		})
		if err != nil {
			return rpc.ContractParams{}, err
		}
		if echo.Exchange == "" {
			echo.Exchange = "SMART"
		}
		if echo.Currency == "" {
			echo.Currency = "USD"
		}
		echo.SecType = secType
		echo.ConID = in.ConID
		echo.Multiplier = max(in.Multiplier, 1)
		return echo, nil
	case "OPT":
		return normaliseOptionQuoteContract(in)
	default:
		return rpc.ContractParams{}, errBadRequest("order preview supports STK/ETF/OPT contracts only")
	}
}

func normalizePreviewStrategy(strategy string, limit *float64) string {
	strategy = strings.ToLower(strings.TrimSpace(strategy))
	if strategy == "" {
		if limit != nil {
			return rpc.OrderStrategyExplicitLimit
		}
		return rpc.OrderStrategyPatientLimit
	}
	return strategy
}

func previewSupportedOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeLMT, rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return true
	default:
		return false
	}
}

func previewOrderTriggerMethod(orderType string, contract rpc.ContractParams, requested int) (int, error) {
	if requested != rpc.OrderTriggerMethodDefault {
		if !isTrailOrderType(orderType) {
			return 0, errBadRequest("trigger_method is supported for TRAIL and TRAIL LIMIT orders only")
		}
		if !validOrderTriggerMethod(requested) {
			return 0, errBadRequest("trigger_method must be one of 1, 2, 3, 4, 7, or 8")
		}
		return requested, nil
	}
	if isTrailOrderType(orderType) && trailTriggerDefaultsToLast(contract) {
		return rpc.OrderTriggerMethodLast, nil
	}
	return rpc.OrderTriggerMethodDefault, nil
}

func validOrderTriggerMethod(method int) bool {
	switch method {
	case rpc.OrderTriggerMethodDoubleBidAsk,
		rpc.OrderTriggerMethodLast,
		rpc.OrderTriggerMethodDoubleLast,
		rpc.OrderTriggerMethodBidAsk,
		rpc.OrderTriggerMethodLastOrBidAsk,
		rpc.OrderTriggerMethodMidpoint:
		return true
	default:
		return false
	}
}

func trailTriggerDefaultsToLast(contract rpc.ContractParams) bool {
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "STOCK", "ETF":
		return true
	default:
		return false
	}
}

func previewOrderPricing(action, orderType, rawStrategy string, explicit *float64, rawTrail *rpc.OrderTrailSpec, contract rpc.ContractParams, quote rpc.OrderQuoteSnapshot) (strategy string, limit float64, trail *rpc.OrderTrailSpec, notionalPrice float64, err error) {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeLMT:
		if rawTrail != nil {
			return "", 0, nil, 0, errBadRequest("LMT order preview must not include trail fields")
		}
		strategy = normalizePreviewStrategy(rawStrategy, explicit)
		limit, err = previewLimitPrice(action, strategy, explicit, quote)
		return strategy, limit, nil, limit, err
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		strategy = strings.ToLower(strings.TrimSpace(rawStrategy))
		if strategy == "" {
			strategy = rpc.OrderStrategyBrokerTrail
		}
		if strategy != rpc.OrderStrategyBrokerTrail {
			return "", 0, nil, 0, errBadRequest("TRAIL order preview requires broker-trail strategy")
		}
		if explicit != nil {
			return "", 0, nil, 0, errBadRequest("TRAIL order preview must not include limit_price")
		}
		trail, err = previewTrailSpec(action, orderType, rawTrail, contract, quote)
		if err != nil {
			return "", 0, nil, 0, err
		}
		notionalPrice = trailNotionalReferencePrice(action, trail, quote)
		return strategy, 0, trail, notionalPrice, nil
	default:
		return "", 0, nil, 0, errBadRequest("unsupported order type")
	}
}

func previewTrailSpec(action, orderType string, raw *rpc.OrderTrailSpec, contract rpc.ContractParams, quote rpc.OrderQuoteSnapshot) (*rpc.OrderTrailSpec, error) {
	if raw == nil {
		return nil, errBadRequest("TRAIL order preview requires trail fields")
	}
	out := cloneTrailSpec(raw)
	out.Basis = strings.ToLower(strings.TrimSpace(out.Basis))
	if out.Basis == "" {
		out.Basis = rpc.OrderTrailBasisInstrumentPrice
	}
	if out.Basis != rpc.OrderTrailBasisInstrumentPrice {
		return nil, errBadRequest("trail basis must be instrument_price")
	}
	out.OffsetType = strings.ToLower(strings.TrimSpace(out.OffsetType))
	hasPercent := out.TrailingPercent != nil
	hasAmount := out.TrailingAmount != nil
	switch {
	case hasPercent == hasAmount:
		return nil, errBadRequest("trail requires exactly one of trailing_percent or trailing_amount")
	case hasPercent:
		if !positiveFinite(*out.TrailingPercent) {
			return nil, errBadRequest("trailing_percent must be positive")
		}
		if out.OffsetType == "" {
			out.OffsetType = rpc.OrderTrailOffsetPercent
		}
		if out.OffsetType != rpc.OrderTrailOffsetPercent {
			return nil, errBadRequest("trail offset_type must match trailing_percent")
		}
	case hasAmount:
		if !positiveFinite(*out.TrailingAmount) {
			return nil, errBadRequest("trailing_amount must be positive")
		}
		if out.OffsetType == "" {
			out.OffsetType = rpc.OrderTrailOffsetAmount
		}
		if out.OffsetType != rpc.OrderTrailOffsetAmount {
			return nil, errBadRequest("trail offset_type must match trailing_amount")
		}
		amount := ceilPriceToTick(*out.TrailingAmount, trailMinimumTick(contract, *out.TrailingAmount))
		out.TrailingAmount = &amount
	}
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeTRAIL:
		if out.LimitOffset != nil {
			return nil, errBadRequest("TRAIL order preview must not include limit_offset")
		}
	case rpc.OrderTypeTRAILLIMIT:
		if out.LimitOffset == nil || !positiveFinite(*out.LimitOffset) {
			return nil, errBadRequest("TRAIL LIMIT order preview requires positive limit_offset")
		}
		offset := ceilPriceToTick(*out.LimitOffset, trailMinimumTick(contract, *out.LimitOffset))
		out.LimitOffset = &offset
	}
	if out.InitialStopPrice <= 0 {
		reference, err := trailQuoteReferencePrice(action, quote)
		if err != nil {
			// The wire validators reject trailStopPrice <= 0, so a draft
			// without a seedable stop can never transmit — surface the real
			// reason (no live reference) instead of a confusing broker
			// "trail stop price required" failure later.
			return nil, fmt.Errorf("%w; provide initial_stop_price explicitly or retry with live bid/ask", err)
		}
		offset := trailOffsetAmount(out, reference)
		out.InitialStopPrice = trailingStopInitialPriceForContract(action, reference, offset, contract)
	} else if !positiveFinite(out.InitialStopPrice) {
		return nil, errBadRequest("initial_stop_price must be positive")
	} else {
		out.InitialStopPrice = roundStopPriceForContract(action, out.InitialStopPrice, contract)
	}
	return out, nil
}

func trailQuoteReferencePrice(action string, quote rpc.OrderQuoteSnapshot) (float64, error) {
	if err := requireFreshPreviewQuote(quote, "broker-trail"); err != nil {
		return 0, err
	}
	if !rpc.IsLiveDataType(quote.DataType) {
		return 0, errBadRequest("broker-trail requires live bid/ask data")
	}
	switch action {
	case rpc.OrderActionSell:
		if quote.Bid == nil || *quote.Bid <= 0 {
			return 0, errBadRequest("broker-trail SELL requires a positive bid")
		}
		return *quote.Bid, nil
	case rpc.OrderActionBuy:
		if quote.Ask == nil || *quote.Ask <= 0 {
			return 0, errBadRequest("broker-trail BUY requires a positive ask")
		}
		return *quote.Ask, nil
	default:
		return 0, errBadRequest("action must be buy or sell")
	}
}

func trailOffsetAmount(trail *rpc.OrderTrailSpec, reference float64) float64 {
	if trail == nil {
		return 0
	}
	if trail.TrailingPercent != nil {
		return reference * *trail.TrailingPercent / 100
	}
	if trail.TrailingAmount != nil {
		return *trail.TrailingAmount
	}
	return 0
}

func trailNotionalReferencePrice(action string, trail *rpc.OrderTrailSpec, quote rpc.OrderQuoteSnapshot) float64 {
	if ref, err := trailQuoteReferencePrice(action, quote); err == nil && ref > 0 {
		return ref
	}
	if trail != nil && trail.InitialStopPrice > 0 {
		return trail.InitialStopPrice
	}
	return 0
}

func positiveFinite(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func (s *Server) previewQuote(ctx context.Context, contract rpc.ContractParams, timeout time.Duration) (rpc.OrderQuoteSnapshot, error) {
	params := rpc.QuoteSnapshotParams{
		Contract:         contract,
		TimeoutMs:        int(timeout.Milliseconds()),
		IncludeLiquidity: true,
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return rpc.OrderQuoteSnapshot{}, fmt.Errorf("marshal quote params: %w", err)
	}
	q, err := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
	if err != nil {
		return rpc.OrderQuoteSnapshot{}, err
	}
	if q == nil {
		return rpc.OrderQuoteSnapshot{}, errBadRequest("quote snapshot unavailable")
	}
	out := rpc.OrderQuoteSnapshot{
		Symbol:         q.Symbol,
		Bid:            q.Bid,
		Ask:            q.Ask,
		Last:           q.Last,
		Mark:           q.Mark,
		DataType:       q.DataType,
		QuoteQuality:   q.QuoteQuality,
		SpreadPct:      q.SpreadPct,
		PriceAt:        q.PriceAt,
		PriceAsOf:      q.PriceAsOf,
		Stale:          q.Stale,
		StaleReason:    q.StaleReason,
		AsOf:           q.AsOf,
		SessionContext: q.SessionContext,
		Warnings:       append([]rpc.DataWarning{}, q.WarningDetails...),
	}
	if q.Bid != nil && q.Ask != nil && *q.Bid > 0 && *q.Ask > *q.Bid {
		mid := (*q.Bid + *q.Ask) / 2
		out.Midpoint = &mid
	}
	return out, nil
}

func previewLimitPrice(action, strategy string, explicit *float64, quote rpc.OrderQuoteSnapshot) (float64, error) {
	switch strategy {
	case rpc.OrderStrategyExplicitLimit:
		if explicit == nil {
			return 0, errBadRequest("explicit-limit strategy requires --limit")
		}
		if *explicit <= 0 || math.IsNaN(*explicit) || math.IsInf(*explicit, 0) {
			return 0, errBadRequest("limit price must be positive")
		}
		return roundPrice(*explicit), nil
	case rpc.OrderStrategyPatientLimit:
		if explicit != nil {
			return 0, errBadRequest("--limit uses explicit-limit strategy; omit --strategy or set --strategy explicit-limit")
		}
		if err := requireFreshPreviewQuote(quote, "patient-limit"); err != nil {
			return 0, err
		}
		if !rpc.IsLiveDataType(quote.DataType) {
			return 0, errBadRequest("patient-limit requires live bid/ask data; use --limit for explicit-limit preview on stale or delayed data")
		}
		if quote.Bid == nil || quote.Ask == nil || *quote.Bid <= 0 || *quote.Ask <= *quote.Bid {
			return 0, errBadRequest("patient-limit requires a positive two-sided bid/ask")
		}
		mid := (*quote.Bid + *quote.Ask) / 2
		tick := priceTick(mid)
		switch action {
		case rpc.OrderActionBuy:
			return roundPrice(max(math.Floor(mid/tick)*tick, *quote.Bid)), nil
		case rpc.OrderActionSell:
			return roundPrice(min(math.Ceil(mid/tick)*tick, *quote.Ask)), nil
		default:
			return 0, errBadRequest("action must be buy or sell")
		}
	default:
		return 0, errBadRequest("strategy must be patient-limit or explicit-limit")
	}
}

func requireFreshPreviewQuote(quote rpc.OrderQuoteSnapshot, useCase string) error {
	if quote.Stale {
		reason := strings.TrimSpace(quote.StaleReason)
		if reason == "" {
			reason = "quote data is stale"
		}
		return errBadRequest(fmt.Sprintf("%s requires fresh quote data: %s", useCase, reason))
	}
	if quote.SessionContext != nil && !quote.SessionContext.IsOpen {
		label := strings.TrimSpace(quote.SessionContext.State)
		if label == "" {
			label = "market is closed"
		}
		return errBadRequest(fmt.Sprintf("%s requires an open market session: %s", useCase, label))
	}
	return nil
}

func priceTick(price float64) float64 {
	if price < 1 {
		return 0.0001
	}
	return 0.01
}

func roundPrice(price float64) float64 {
	return math.Round(price*10000) / 10000
}

func trailMinimumTick(contract rpc.ContractParams, price float64) float64 {
	// Prefer the broker-reported venue increment (MiFID-banded grids on
	// Xetra make the static 0.01 wrong for e.g. EUR names above €100 —
	// broker error 110 at WhatIf/place). Zero means unresolved: fall back
	// to the static US-style grid and let WhatIf fail closed.
	if contract.MinTick > 0 {
		return contract.MinTick
	}
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF":
		return 0.01
	default:
		return priceTick(price)
	}
}

func ceilPriceToTick(price, tick float64) float64 {
	if tick <= 0 {
		return roundPrice(price)
	}
	return roundPrice(math.Ceil((price-1e-9)/tick) * tick)
}

func floorPriceToTick(price, tick float64) float64 {
	if tick <= 0 {
		return roundPrice(price)
	}
	return roundPrice(math.Floor((price+1e-9)/tick) * tick)
}

func roundStopPriceForContract(action string, price float64, contract rpc.ContractParams) float64 {
	tick := trailMinimumTick(contract, price)
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		return ceilPriceToTick(price, tick)
	}
	return floorPriceToTick(price, tick)
}

func trailingStopInitialPriceForContract(action string, reference, trailAbs float64, contract rpc.ContractParams) float64 {
	if reference <= 0 || trailAbs <= 0 {
		return 0
	}
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		return roundStopPriceForContract(action, reference+trailAbs, contract)
	}
	return roundStopPriceForContract(action, max(reference-trailAbs, 0.0001), contract)
}

func contractMultiplier(contract rpc.ContractParams) int {
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF":
		return 1
	case "OPT":
		if contract.Multiplier > 0 {
			return contract.Multiplier
		}
		return 100
	default:
		if contract.Multiplier > 0 {
			return contract.Multiplier
		}
		return 1
	}
}

func (s *Server) previewPositionImpact(ctx context.Context, contract rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
	c := s.gatewayConnector()
	if c == nil {
		return rpc.OrderPositionImpact{}, s.gatewayUnavailableError()
	}
	positions, err := c.GetCachedPositions()
	if err != nil {
		return rpc.OrderPositionImpact{}, err
	}
	_ = ctx
	before := positionQuantityForContract(positions, contract)
	delta := float64(qty)
	if action == rpc.OrderActionSell {
		delta = -delta
	}
	after := before + delta
	return rpc.OrderPositionImpact{
		Before: before,
		After:  after,
		Effect: classifyPositionEffect(before, after),
	}, nil
}

func positionQuantityForContract(positions []*ibkrlib.RawPosition, contract rpc.ContractParams) float64 {
	if strings.EqualFold(contract.SecType, "OPT") {
		return optionPositionQuantity(positions, contract)
	}
	return stockPositionQuantity(positions, contract)
}

func stockPositionQuantity(positions []*ibkrlib.RawPosition, contract rpc.ContractParams) float64 {
	wantSymbol := strings.ToUpper(strings.TrimSpace(contract.Symbol))
	if wantSymbol == "" {
		return 0
	}
	wantCurrency := strings.ToUpper(strings.TrimSpace(contract.Currency))
	wantLocalSymbol := strings.ToUpper(strings.TrimSpace(contract.LocalSymbol))
	wantTradingClass := strings.ToUpper(strings.TrimSpace(contract.TradingClass))
	wantPrimaryExch := strings.ToUpper(strings.TrimSpace(contract.PrimaryExch))
	wantExchange := strings.ToUpper(strings.TrimSpace(contract.Exchange))
	var qty float64
	for _, pos := range positions {
		if pos == nil {
			continue
		}
		if !strings.EqualFold(pos.Contract.SecType, "STK") || !strings.EqualFold(pos.Contract.Symbol, wantSymbol) {
			continue
		}
		if wantCurrency != "" && pos.Contract.Currency != "" && !strings.EqualFold(pos.Contract.Currency, wantCurrency) {
			continue
		}
		if wantLocalSymbol != "" && pos.Contract.LocalSymbol != "" && !strings.EqualFold(pos.Contract.LocalSymbol, wantLocalSymbol) {
			continue
		}
		if wantTradingClass != "" && pos.Contract.TradingClass != "" && !strings.EqualFold(pos.Contract.TradingClass, wantTradingClass) {
			continue
		}
		if wantPrimaryExch != "" && !stockVenueMatches(pos.Contract, wantPrimaryExch) {
			continue
		}
		if wantPrimaryExch == "" && wantExchange != "" && wantExchange != "SMART" && !stockVenueMatches(pos.Contract, wantExchange) {
			continue
		}
		qty += pos.Position
	}
	return qty
}

func optionPositionQuantity(positions []*ibkrlib.RawPosition, contract rpc.ContractParams) float64 {
	wantSymbol := strings.ToUpper(strings.TrimSpace(contract.Symbol))
	wantExpiry := strings.TrimSpace(contract.Expiry)
	wantRight := strings.ToUpper(strings.TrimSpace(contract.Right))
	wantCurrency := strings.ToUpper(strings.TrimSpace(contract.Currency))
	wantLocalSymbol := strings.ToUpper(strings.TrimSpace(contract.LocalSymbol))
	wantTradingClass := strings.ToUpper(strings.TrimSpace(contract.TradingClass))
	for _, pos := range positions {
		if pos == nil || !strings.EqualFold(pos.Contract.SecType, "OPT") {
			continue
		}
		if contract.ConID > 0 && pos.Contract.ConID > 0 {
			if contract.ConID == pos.Contract.ConID {
				return pos.Position
			}
			continue
		}
		if !strings.EqualFold(pos.Contract.Symbol, wantSymbol) ||
			strings.TrimSpace(pos.Contract.Expiry) != wantExpiry ||
			!strings.EqualFold(pos.Contract.Right, wantRight) ||
			!samePreviewFloat(pos.Contract.Strike, contract.Strike) {
			continue
		}
		if wantCurrency != "" && pos.Contract.Currency != "" && !strings.EqualFold(pos.Contract.Currency, wantCurrency) {
			continue
		}
		if wantLocalSymbol != "" && pos.Contract.LocalSymbol != "" && !strings.EqualFold(pos.Contract.LocalSymbol, wantLocalSymbol) {
			continue
		}
		if wantTradingClass != "" && pos.Contract.TradingClass != "" && !strings.EqualFold(pos.Contract.TradingClass, wantTradingClass) {
			continue
		}
		if !optionalMultiplierEqual(pos.Contract.Multiplier, contract.Multiplier) {
			continue
		}
		return pos.Position
	}
	return 0
}

func samePreviewFloat(a, b float64) bool {
	const epsilon = 1e-9
	if a > b {
		return a-b < epsilon
	}
	return b-a < epsilon
}

func stockVenueMatches(contract ibkrlib.Contract, want string) bool {
	want = strings.ToUpper(strings.TrimSpace(want))
	if want == "" {
		return true
	}
	primaryExch := strings.ToUpper(strings.TrimSpace(contract.PrimaryExch))
	exchange := strings.ToUpper(strings.TrimSpace(contract.Exchange))
	if exchange == "SMART" {
		exchange = ""
	}
	if primaryExch == "" && exchange == "" {
		return true
	}
	return primaryExch == want || exchange == want
}

func classifyPositionEffect(before, after float64) string {
	switch {
	case before == 0 && after > 0:
		return rpc.OrderPositionEffectOpen
	case before > 0 && after > before:
		return rpc.OrderPositionEffectIncrease
	case before > 0 && after > 0 && after < before:
		return rpc.OrderPositionEffectReduce
	case before > 0 && after == 0:
		return rpc.OrderPositionEffectClose
	case before == 0 && after < 0:
		return rpc.OrderPositionEffectOpenShort
	case before >= 0 && after < 0:
		return rpc.OrderPositionEffectFlip
	case before < 0 && after > 0:
		return rpc.OrderPositionEffectFlip
	case before < 0 && after == 0:
		return rpc.OrderPositionEffectClose
	case before < 0 && after < 0 && math.Abs(after) < math.Abs(before):
		return rpc.OrderPositionEffectReduce
	case before < 0 && after < before:
		return rpc.OrderPositionEffectIncrease
	default:
		return rpc.OrderPositionEffectIncrease
	}
}

func stockShortOrFlip(effect string) bool {
	return effect == rpc.OrderPositionEffectFlip || effect == rpc.OrderPositionEffectOpenShort
}

// isRiskReducing reports whether the previewed order only closes or reduces
// existing exposure. The [trading].max_notional and max_option_contracts caps
// bind risk-increasing orders only: a close/reduce order is bounded by the
// position itself, and capping it would block the daemon's own protection
// proposals (e.g. a full-position protective trail above the notional cap).
// Flip is deliberately not in the allowlist — its through-zero portion opens
// new exposure — so flips and every unrecognized effect stay capped.
func isRiskReducing(effect string) bool {
	return effect == rpc.OrderPositionEffectClose || effect == rpc.OrderPositionEffectReduce
}

func optionSellToOpen(action, effect string) bool {
	return action == rpc.OrderActionSell && (effect == rpc.OrderPositionEffectOpenShort || effect == rpc.OrderPositionEffectFlip)
}

func orderOpenCloseForEffect(effect string) string {
	switch effect {
	case rpc.OrderPositionEffectClose, rpc.OrderPositionEffectReduce:
		return "C"
	default:
		return "O"
	}
}

func previewWhatIfUnavailable() rpc.OrderWhatIfResult {
	return rpc.OrderWhatIfResult{
		Status:            rpc.OrderWhatIfStatusUnavailable,
		RequiredForSubmit: true,
		Available:         false,
		Message:           "Broker WhatIf did not return an accepted preview; no broker order was placed.",
		Action:            "Run order preview again with a ready broker WhatIf path before any submit attempt.",
	}
}

func previewWhatIfUnavailableWithMessage(message string) rpc.OrderWhatIfResult {
	out := previewWhatIfUnavailable()
	if msg := strings.TrimSpace(message); msg != "" {
		out.Message = msg
		out.Action = "Resolve broker WhatIf availability and run order preview again before any submit attempt."
	}
	return out
}

func previewWhatIfJournalMessage(whatIf rpc.OrderWhatIfResult) string {
	switch whatIf.Status {
	case rpc.OrderWhatIfStatusAccepted:
		return "preview token minted with broker WhatIf acceptance; no broker order was placed"
	case rpc.OrderWhatIfStatusRejected:
		return "preview token minted with broker WhatIf rejection; later place gate must reject this token"
	default:
		return "preview token minted; broker WhatIf unavailable or not accepted"
	}
}

func previewWhatIfWarnings(whatIf rpc.OrderWhatIfResult) []rpc.DataWarning {
	switch whatIf.Status {
	case rpc.OrderWhatIfStatusAccepted:
		return nil
	case rpc.OrderWhatIfStatusRejected:
		msg := strings.TrimSpace(whatIf.Message)
		if msg == "" {
			msg = "Broker WhatIf rejected this order draft."
		}
		action := strings.TrimSpace(whatIf.Action)
		if action == "" {
			action = "Adjust the draft and preview again before any submit attempt."
		}
		return []rpc.DataWarning{{
			Code:     "broker_what_if_rejected",
			Severity: "error",
			Message:  msg,
			Action:   action,
		}}
	default:
		return []rpc.DataWarning{{
			Code:     "broker_what_if_unavailable",
			Severity: "warning",
			Message:  "Broker WhatIf did not accept this draft; this preview token cannot bypass the later broker-WhatIf submit gate.",
			Action:   "Use this token only as preview evidence until broker WhatIf returns accepted for the exact draft.",
		}}
	}
}

func previewOrderRef(now time.Time) string {
	tokenID, err := randomTokenID()
	if err != nil {
		return "ibkr-" + now.UTC().Format("20060102-150405")
	}
	return "ibkr-" + now.UTC().Format("20060102-150405") + "-" + tokenID[:8]
}

func replaceTargetFromView(view rpc.OrderView) orderPreviewReplaceTarget {
	if view.OrderRef == "" && view.ReservedOrderID == 0 && view.PermID == 0 {
		return orderPreviewReplaceTarget{}
	}
	return orderPreviewReplaceTarget{
		OrderRef:        view.OrderRef,
		ReservedOrderID: view.ReservedOrderID,
		PermID:          view.PermID,
		ClientID:        view.ClientID,
		Account:         view.Account,
		Endpoint:        view.Endpoint,
		Mode:            view.Mode,
		Status:          view.Status,
		LifecycleStatus: view.LifecycleStatus,
		OrderType:       view.OrderType,
		TIF:             view.TIF,
		TriggerMethod:   view.TriggerMethod,
		Quantity:        view.Quantity,
		Filled:          view.Filled,
		Remaining:       view.Remaining,
		LimitPrice:      view.LimitPrice,
		Trail:           cloneTrailSpec(view.Trail),
		OutsideRTH:      view.OutsideRTH,
	}
}
