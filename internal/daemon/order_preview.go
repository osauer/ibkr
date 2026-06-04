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
)

type orderPreviewTokenPayload struct {
	Version      int                     `json:"version"`
	TokenID      string                  `json:"token_id"`
	Scope        string                  `json:"scope"`
	IssuedAt     time.Time               `json:"issued_at"`
	ExpiresAt    time.Time               `json:"expires_at"`
	Mode         string                  `json:"mode"`
	Account      string                  `json:"account"`
	Endpoint     string                  `json:"endpoint"`
	ClientID     int                     `json:"client_id"`
	Draft        rpc.OrderDraft          `json:"draft"`
	Quote        rpc.OrderQuoteSnapshot  `json:"quote"`
	Position     rpc.OrderPositionImpact `json:"position"`
	Notional     float64                 `json:"notional"`
	WhatIf       rpc.OrderWhatIfResult   `json:"what_if"`
	WhatIfStatus string                  `json:"what_if_status,omitempty"`
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
	payload.Scope = rpc.OrderTokenScopePlace
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
	if payload.Scope != rpc.OrderTokenScopePlace {
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
	if !status.Enabled {
		return nil, fmt.Errorf("%w: enable [trading] before order preview", ErrTradingDisabled)
	}
	if status.Blocked {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(status.Blockers))
	}
	if s.orderTokens == nil {
		return nil, fmt.Errorf("%w: order preview token signer is unavailable", ErrTradingDisabled)
	}

	cfg := config.Trading{}.WithDefaults()
	if s.cfg != nil {
		cfg = s.cfg.Trading.WithDefaults()
	}
	action, err := normalizeOrderAction(p.Action)
	if err != nil {
		return nil, err
	}
	contract, err := normalizePreviewContract(p.Contract)
	if err != nil {
		return nil, err
	}
	if contract.SecType != "STK" {
		return nil, errBadRequest("order preview currently supports stocks and ETFs only; single-leg options remain disabled until their parser and broker WhatIf gates are complete")
	}
	if p.Quantity <= 0 {
		return nil, errBadRequest("quantity must be positive")
	}
	orderType := strings.ToUpper(strings.TrimSpace(p.OrderType))
	if orderType == "" {
		orderType = rpc.OrderTypeLMT
	}
	if orderType != rpc.OrderTypeLMT {
		return nil, errBadRequest("order preview supports LMT orders only")
	}
	tif := strings.ToUpper(strings.TrimSpace(p.TIF))
	if tif == "" {
		tif = rpc.OrderTIFDay
	}
	if tif != rpc.OrderTIFDay {
		return nil, errBadRequest("order preview supports DAY time-in-force only")
	}

	timeout := time.Duration(p.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	quote, err := s.fetchPreviewQuote(ctx, contract, timeout)
	if err != nil {
		return nil, err
	}
	strategy := normalizePreviewStrategy(p.Strategy, p.LimitPrice)
	limit, err := previewLimitPrice(action, strategy, p.LimitPrice, quote)
	if err != nil {
		return nil, err
	}
	notional := float64(p.Quantity) * limit
	if notional > cfg.MaxNotional {
		return nil, errBadRequest(fmt.Sprintf("order notional %.2f exceeds [trading].max_notional %.2f", notional, cfg.MaxNotional))
	}
	position, err := s.fetchPreviewPositionImpact(ctx, contract.Symbol, action, p.Quantity)
	if err != nil {
		return nil, err
	}
	if stockShortOrFlip(position.Effect) && !cfg.AllowStockShort {
		return nil, errBadRequest("stock short/flip previews require [trading].allow_stock_short = true")
	}

	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	draft := rpc.OrderDraft{
		Action:     action,
		Contract:   contract,
		Quantity:   p.Quantity,
		OrderType:  orderType,
		LimitPrice: limit,
		TIF:        tif,
		OutsideRTH: p.OutsideRTH,
		Strategy:   strategy,
		OrderRef:   previewOrderRef(now),
	}
	whatIf, err := s.fetchPreviewWhatIf(ctx, draft)
	if err != nil {
		return nil, err
	}
	token, tokenID, expiresAt, err := s.orderTokens.mint(orderPreviewTokenPayload{
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
		Action:         draft.Action,
		OrderType:      draft.OrderType,
		TIF:            draft.TIF,
		Quantity:       float64(draft.Quantity),
		LimitPrice:     draft.LimitPrice,
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

	return &rpc.OrderPreviewResult{
		PreviewToken:          token,
		PreviewTokenID:        tokenID,
		PreviewTokenScope:     rpc.OrderTokenScopePlace,
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
		MaxNotional:           cfg.MaxNotional,
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

func (s *Server) fetchPreviewPositionImpact(ctx context.Context, symbol, action string, qty int) (rpc.OrderPositionImpact, error) {
	if s.orderPreviewPositionImpact != nil {
		return s.orderPreviewPositionImpact(ctx, symbol, action, qty)
	}
	return s.previewPositionImpact(ctx, symbol, action, qty)
}

func (s *Server) fetchPreviewWhatIf(ctx context.Context, draft rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
	if s.orderPreviewWhatIf != nil {
		return s.orderPreviewWhatIf(ctx, draft)
	}
	return previewWhatIfUnavailable(), nil
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
	case "STK":
		_, echo, _, err := normaliseStockQuoteContract(rpc.ContractParams{
			Symbol:       in.Symbol,
			SecType:      secType,
			Market:       in.Market,
			Exchange:     in.Exchange,
			PrimaryExch:  in.PrimaryExch,
			Currency:     in.Currency,
			LocalSymbol:  in.LocalSymbol,
			TradingClass: in.TradingClass,
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
		echo.SecType = "STK"
		return echo, nil
	case "OPT":
		return normaliseOptionQuoteContract(in)
	default:
		return rpc.ContractParams{}, errBadRequest("order preview supports STK contracts only in this slice")
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
		Symbol:       q.Symbol,
		Bid:          q.Bid,
		Ask:          q.Ask,
		Last:         q.Last,
		Mark:         q.Mark,
		DataType:     q.DataType,
		QuoteQuality: q.QuoteQuality,
		SpreadPct:    q.SpreadPct,
		PriceAt:      q.PriceAt,
		AsOf:         q.AsOf,
		Warnings:     append([]rpc.DataWarning{}, q.WarningDetails...),
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

func priceTick(price float64) float64 {
	if price < 1 {
		return 0.0001
	}
	return 0.01
}

func roundPrice(price float64) float64 {
	return math.Round(price*10000) / 10000
}

func (s *Server) previewPositionImpact(ctx context.Context, symbol, action string, qty int) (rpc.OrderPositionImpact, error) {
	c := s.gatewayConnector()
	if c == nil {
		return rpc.OrderPositionImpact{}, ibkrlib.ErrIBKRUnavailable
	}
	positions, err := c.GetCachedPositions()
	if err != nil {
		return rpc.OrderPositionImpact{}, err
	}
	_ = ctx
	before := stockPositionQuantity(positions, symbol)
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

func stockPositionQuantity(positions []*ibkrlib.RawPosition, symbol string) float64 {
	want := strings.ToUpper(strings.TrimSpace(symbol))
	var qty float64
	for _, pos := range positions {
		if pos == nil {
			continue
		}
		if strings.EqualFold(pos.Contract.SecType, "STK") && strings.EqualFold(pos.Contract.Symbol, want) {
			qty += pos.Position
		}
	}
	return qty
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

func previewWhatIfUnavailable() rpc.OrderWhatIfResult {
	return rpc.OrderWhatIfResult{
		Status:            rpc.OrderWhatIfStatusUnavailable,
		RequiredForSubmit: true,
		Available:         false,
		Message:           "Broker WhatIf is isolated from the default read-only order wire; no place/modify/cancel path was opened.",
		Action:            "Wire WhatIf behind the trading build tag before enabling order.place.",
	}
}

func previewWhatIfJournalMessage(whatIf rpc.OrderWhatIfResult) string {
	switch whatIf.Status {
	case rpc.OrderWhatIfStatusAccepted:
		return "preview token minted with broker WhatIf acceptance; no broker order was placed"
	case rpc.OrderWhatIfStatusRejected:
		return "preview token minted with broker WhatIf rejection; later place gate must reject this token"
	default:
		return "preview token minted; broker WhatIf path not enabled in the default build"
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
			Message:  "Broker WhatIf is not wired in the default build; this preview token cannot bypass the later broker-WhatIf submit gate.",
			Action:   "Use this token only as preview evidence until the trading build wires WhatIf and place handling.",
		}}
	}
}

func previewOrderRef(now time.Time) string {
	return "ibkr-" + now.UTC().Format("20060102-150405")
}
