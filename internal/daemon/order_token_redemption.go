package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func (s *Server) confirmPreviewTokenForPlace(token string) (orderPreviewTokenPayload, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: preview token is required", ErrTradingDisabled)
	}
	if s == nil || s.orderTokens == nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: order preview token signer is unavailable", ErrTradingDisabled)
	}
	payload, err := s.orderTokens.verify(token)
	if err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: %v", ErrTradingDisabled, err)
	}

	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.tradingStatus(ep)
	if !status.Enabled {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: enable [trading] before confirming order preview token", ErrTradingDisabled)
	}
	if status.Blocked {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(status.Blockers))
	}
	if payload.Mode != status.Mode || payload.Account != status.Account || payload.Endpoint != status.Endpoint || payload.ClientID != status.ClientID {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: preview token was minted for %s/%s client %d account %s, current gate is %s/%s client %d account %s",
			ErrTradingDisabled,
			payload.Mode, payload.Endpoint, payload.ClientID, payload.Account,
			status.Mode, status.Endpoint, status.ClientID, status.Account)
	}
	if payload.WhatIf.Status != rpc.OrderWhatIfStatusAccepted {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: preview token is not backed by an accepted broker WhatIf result", ErrTradingDisabled)
	}
	if payload.WhatIf.RequiredForSubmit {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: accepted broker WhatIf result still requires a submit gate", ErrTradingDisabled)
	}
	if s.orderJournal == nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: order journal is unavailable", ErrTradingDisabled)
	}

	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	if err := s.orderJournal.ConfirmPreviewTokenUse(orderJournalEvent{
		At:             now,
		Type:           orderJournalEventTokenConfirmed,
		OrderRef:       payload.Draft.OrderRef,
		PreviewTokenID: payload.TokenID,
		ClientID:       payload.ClientID,
		Account:        payload.Account,
		Endpoint:       payload.Endpoint,
		Mode:           payload.Mode,
		Symbol:         payload.Draft.Contract.Symbol,
		SecType:        payload.Draft.Contract.SecType,
		Action:         payload.Draft.Action,
		OrderType:      payload.Draft.OrderType,
		TIF:            payload.Draft.TIF,
		Quantity:       float64(payload.Draft.Quantity),
		LimitPrice:     payload.Draft.LimitPrice,
		Message:        "preview token confirmed for a future place flow; no broker send was attempted",
	}); err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: %w", ErrTradingDisabled, err)
	}
	return payload, nil
}
