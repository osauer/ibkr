//go:build trading

package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

const orderWritesAvailable = true

func (s *Server) handleOrderPlace(ctx context.Context, req *rpc.Request) (*rpc.OrderPlaceResult, error) {
	var p rpc.OrderPlaceParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.placeOrder(ctx, p)
}

func (s *Server) handleOrderModify(ctx context.Context, req *rpc.Request) (*rpc.OrderModifyResult, error) {
	var p rpc.OrderModifyParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.modifyOrder(ctx, p)
}

func (s *Server) handleOrderCancel(ctx context.Context, req *rpc.Request) (*rpc.OrderCancelResult, error) {
	var p rpc.OrderCancelParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.cancelOrder(ctx, p)
}

func (s *Server) currentBrokerWriteAuthorization() brokerWriteAuthorization {
	if s == nil {
		auth := brokerWriteAuthorization{}
		auth.Blockers = appendTradingBlockerOnce(auth.Blockers, rpc.TradingBlocker{
			Code:    "trading_disabled",
			Message: "trading daemon is unavailable",
			Action:  "Start the ibkr daemon before broker writes.",
		})
		return auth
	}
	return s.brokerWriteAuthorization(s.currentTradingStatus())
}

// brokerWriteAuthorizationForRequest is the request-time write gate: the
// origin-agnostic authorization (which also feeds trading-status CanWrite)
// plus the live origin policy for this specific request.
func (s *Server) brokerWriteAuthorizationForRequest(origin, liveConfirmation string) brokerWriteAuthorization {
	auth := s.currentBrokerWriteAuthorization()
	if s == nil {
		return auth
	}
	for _, blocker := range liveOriginBlockers(auth.Status, origin, liveConfirmation) {
		auth.Blockers = appendTradingBlockerOnce(auth.Blockers, blocker)
		auth.Allowed = false
	}
	return auth
}

func (s *Server) placeOrder(ctx context.Context, p rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	auth := s.brokerWriteAuthorizationForRequest(p.Origin, p.LiveConfirmation)
	if !auth.Allowed {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	status := auth.Status
	payload, err := s.verifyPreviewTokenForPlace(p.PreviewToken)
	if err != nil {
		return nil, err
	}
	if msg := s.trailRedemptionGuard(ctx, payload.Draft); msg != "" {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, msg)
	}
	reservedOrderID, err := s.reserveBrokerOrderID(ctx)
	if err != nil {
		return nil, err
	}
	now := s.orderNow()
	confirm := previewTokenConfirmedEvent(payload, reservedOrderID, now, fmt.Sprintf("preview token confirmed for %s broker transmit", auth.Route))
	attempt := orderJournalEventForDraft(payload.Draft, orderJournalEventSendAttempted, status, payload.TokenID, reservedOrderID, now)
	attempt.SendState = orderSendStateSendAttempted
	attempt.Origin = normalizedWriteOrigin(p.Origin)
	attempt.Message = fmt.Sprintf("%s broker placeOrder transmit attempted", auth.Route)
	if err := s.orderJournal.ConfirmPreviewTokenUseAndAppend(confirm, attempt); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTradingDisabled, err)
	}

	contract := previewIBKRContract(payload.Draft.Contract)
	order := previewIBKROrder(payload.Draft)
	order.OrderID = reservedOrderID
	order.ClientID = status.ClientID
	order.Account = status.Account
	if err := s.submitConfiguredOrder(ctx, status, contract, order); err != nil {
		s.appendOrderSendError(payload.Draft, status, payload.TokenID, reservedOrderID, err)
		return nil, fmt.Errorf("%s order transmit: %w", auth.Route, err)
	}

	return &rpc.OrderPlaceResult{
		Accepted:        true,
		Mode:            status.Mode,
		Account:         status.Account,
		Endpoint:        status.Endpoint,
		ClientID:        status.ClientID,
		OrderRef:        payload.Draft.OrderRef,
		PreviewTokenID:  payload.TokenID,
		ReservedOrderID: reservedOrderID,
		Draft:           payload.Draft,
		LifecycleStatus: rpc.OrderLifecyclePendingSubmit,
		SendState:       orderSendStateSendAttempted,
		Message:         fmt.Sprintf("%s broker placeOrder transmit attempted; waiting for broker lifecycle callback", auth.Route),
		AsOf:            s.orderNow(),
	}, nil
}

func (s *Server) modifyOrder(ctx context.Context, p rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	auth := s.brokerWriteAuthorizationForRequest(p.Origin, p.LiveConfirmation)
	if !auth.Allowed {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	status := auth.Status
	view, err := s.openOrderViewForWrite(p.ID, status)
	if err != nil {
		return nil, err
	}
	if view.ReservedOrderID <= 0 {
		return nil, errBadRequest("order modify requires a broker order ID")
	}
	if !view.ModifyEligible {
		return nil, errBadRequest("order is not modify-eligible")
	}
	payload, err := s.verifyPreviewTokenForModify(p.PreviewToken, view)
	if err != nil {
		return nil, err
	}
	if err := validateModifyDraft(view, payload.Draft); err != nil {
		return nil, err
	}

	now := s.orderNow()
	modifiedDraft := payload.Draft
	modifiedDraft.OrderRef = view.OrderRef
	modifiedDraft.Contract = modifyContractForView(view, modifiedDraft.Contract)
	modifiedDraft.OutsideRTH = view.OutsideRTH
	confirm := previewTokenConfirmedEvent(payload, view.ReservedOrderID, now, fmt.Sprintf("preview token confirmed for %s broker modify", auth.Route))
	confirm.OrderRef = view.OrderRef
	modify := orderJournalEventForDraft(modifiedDraft, orderJournalEventModifyRequested, status, payload.TokenID, view.ReservedOrderID, now)
	modify.SendState = orderSendStateSendAttempted
	modify.Message = fmt.Sprintf("%s broker modify attempted", auth.Route)
	if err := s.orderJournal.ConfirmPreviewTokenUseAndAppend(confirm, modify); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTradingDisabled, err)
	}

	contract := previewIBKRContract(modifiedDraft.Contract)
	order := previewIBKROrder(modifiedDraft)
	order.OrderID = view.ReservedOrderID
	order.ClientID = status.ClientID
	order.Account = status.Account
	if err := s.submitConfiguredOrder(ctx, status, contract, order); err != nil {
		s.appendOrderSendError(modifiedDraft, status, payload.TokenID, view.ReservedOrderID, err)
		return nil, fmt.Errorf("%s order modify: %w", auth.Route, err)
	}

	return &rpc.OrderModifyResult{
		Accepted:        true,
		Mode:            status.Mode,
		Account:         status.Account,
		Endpoint:        status.Endpoint,
		ClientID:        status.ClientID,
		OrderRef:        view.OrderRef,
		PreviewTokenID:  payload.TokenID,
		ReservedOrderID: view.ReservedOrderID,
		Draft:           modifiedDraft,
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       orderSendStateSendAttempted,
		Message:         fmt.Sprintf("%s broker modify attempted; waiting for broker lifecycle callback", auth.Route),
		AsOf:            s.orderNow(),
	}, nil
}

func (s *Server) cancelOrder(ctx context.Context, p rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	auth := s.currentBrokerWriteAuthorization()
	if !auth.Allowed {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	status := auth.Status
	view, err := s.openOrderViewForWrite(p.ID, status)
	if err != nil {
		return nil, err
	}
	if view.ReservedOrderID <= 0 {
		return nil, errBadRequest("order cancel requires a broker order ID")
	}
	if !view.CancelEligible {
		return nil, errBadRequest("order is not cancel-eligible")
	}

	now := s.orderNow()
	cancelEvent := orderJournalEventFromView(view, orderJournalEventCancelRequested, now)
	cancelEvent.SendState = orderSendStateSendAttempted
	cancelEvent.Origin = normalizedWriteOrigin(p.Origin)
	cancelEvent.Message = fmt.Sprintf("%s broker cancel attempted", auth.Route)
	if err := s.orderJournal.Append(cancelEvent); err != nil {
		return nil, fmt.Errorf("%w: append cancel journal: %v", ErrTradingDisabled, err)
	}
	if err := s.cancelConfiguredOrder(ctx, status, view.ReservedOrderID); err != nil {
		s.appendOrderSendError(orderDraftFromView(view), status, view.PreviewTokenID, view.ReservedOrderID, err)
		return nil, fmt.Errorf("%s order cancel: %w", auth.Route, err)
	}
	view.LastEvent = orderJournalEventCancelRequested
	view.SendState = orderSendStateSendAttempted
	view.LifecycleStatus = rpc.OrderLifecyclePendingCancel
	view.Open = true
	view.ModifyEligible = false
	view.CancelEligible = false
	view.LastMessage = cancelEvent.Message
	view.UpdatedAt = now
	return &rpc.OrderCancelResult{
		Accepted:        true,
		Order:           view,
		Status:          view.Status,
		LifecycleStatus: view.LifecycleStatus,
		SendState:       view.SendState,
		Message:         fmt.Sprintf("%s broker cancel attempted; waiting for broker cancellation status", auth.Route),
		AsOf:            s.orderNow(),
	}, nil
}

func (s *Server) reserveBrokerOrderID(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.orderReserveBrokerID != nil {
		return s.orderReserveBrokerID(ctx)
	}
	c := s.gatewayConnector()
	if c == nil {
		return 0, s.gatewayUnavailableError()
	}
	id, err := c.ReserveOrderID()
	if err != nil {
		return 0, err
	}
	floor, err := maxReservedBrokerOrderID(s.orderJournal)
	if err != nil {
		return 0, err
	}
	for id <= floor {
		id, err = c.ReserveOrderID()
		if err != nil {
			return 0, err
		}
	}
	return id, nil
}

func (s *Server) submitPaperOrder(ctx context.Context, gate ibkrlib.PaperOrderGate, contract *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.orderPlaceBroker != nil {
		return s.orderPlaceBroker(ctx, contract, order)
	}
	c := s.gatewayConnector()
	if c == nil {
		return s.gatewayUnavailableError()
	}
	return c.SubmitPaperOrder(gate, contract, order)
}

func (s *Server) submitConfiguredOrder(ctx context.Context, status rpc.TradingStatus, contract *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	auth := s.brokerWriteAuthorization(status)
	if !auth.Allowed {
		return fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	if s.orderPlaceBroker != nil {
		return s.orderPlaceBroker(ctx, contract, order)
	}
	c := s.gatewayConnector()
	if c == nil {
		return s.gatewayUnavailableError()
	}
	switch auth.Route {
	case config.TradingModePaper:
		return c.SubmitPaperOrder(paperGateFromStatus(status), contract, order)
	case config.TradingModeLive:
		return c.SubmitOrder(contract, order)
	default:
		return fmt.Errorf("%w: trading mode %q is invalid", ErrTradingDisabled, auth.Route)
	}
}

func (s *Server) cancelPaperOrder(ctx context.Context, gate ibkrlib.PaperOrderGate, orderID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.orderCancelBroker != nil {
		return s.orderCancelBroker(ctx, orderID)
	}
	c := s.gatewayConnector()
	if c == nil {
		return s.gatewayUnavailableError()
	}
	return c.CancelPaperOrder(gate, orderID)
}

func (s *Server) cancelConfiguredOrder(ctx context.Context, status rpc.TradingStatus, orderID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	auth := s.brokerWriteAuthorization(status)
	if !auth.Allowed {
		return fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	if s.orderCancelBroker != nil {
		return s.orderCancelBroker(ctx, orderID)
	}
	c := s.gatewayConnector()
	if c == nil {
		return s.gatewayUnavailableError()
	}
	switch auth.Route {
	case config.TradingModePaper:
		return c.CancelPaperOrder(paperGateFromStatus(status), orderID)
	case config.TradingModeLive:
		return c.CancelOrder(orderID)
	default:
		return fmt.Errorf("%w: trading mode %q is invalid", ErrTradingDisabled, auth.Route)
	}
}

func (s *Server) verifyPreviewTokenForModify(token string, view rpc.OrderView) (orderPreviewTokenPayload, error) {
	payload, err := s.verifyPreviewTokenForCurrentGate(token)
	if err != nil {
		return orderPreviewTokenPayload{}, err
	}
	if payload.Scope != rpc.OrderTokenScopeModify {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: preview token scope %q cannot be used for modify", ErrTradingDisabled, payload.Scope)
	}
	if err := requireSubmitEligiblePreviewToken(payload); err != nil {
		return orderPreviewTokenPayload{}, err
	}
	if err := validateModifyPreviewTarget(payload.Replace, view); err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: %v", ErrTradingDisabled, err)
	}
	return payload, nil
}

func validateModifyPreviewTarget(target orderPreviewReplaceTarget, view rpc.OrderView) error {
	if target.ReservedOrderID <= 0 {
		return fmt.Errorf("modify preview token is not bound to a broker order ID")
	}
	if target.ReservedOrderID != view.ReservedOrderID {
		return fmt.Errorf("modify preview token targets broker order ID %d, current order is %d", target.ReservedOrderID, view.ReservedOrderID)
	}
	if target.OrderRef != "" && view.OrderRef != "" && target.OrderRef != view.OrderRef {
		return fmt.Errorf("modify preview token targets order ref %s, current order is %s", target.OrderRef, view.OrderRef)
	}
	if target.PermID != 0 && view.PermID != 0 && target.PermID != view.PermID {
		return fmt.Errorf("modify preview token targets permanent ID %d, current order is %d", target.PermID, view.PermID)
	}
	if target.ClientID != 0 && view.ClientID != 0 && target.ClientID != view.ClientID {
		return fmt.Errorf("modify preview token targets client ID %d, current order is %d", target.ClientID, view.ClientID)
	}
	if !strings.EqualFold(target.Account, view.Account) || target.Endpoint != view.Endpoint || !strings.EqualFold(target.Mode, view.Mode) {
		return fmt.Errorf("modify preview token target gate no longer matches current order")
	}
	if target.Status != view.Status || target.LifecycleStatus != view.LifecycleStatus {
		return fmt.Errorf("modify preview token target status changed from %s/%s to %s/%s", target.Status, target.LifecycleStatus, view.Status, view.LifecycleStatus)
	}
	if !sameOrderFloat(target.Quantity, view.Quantity) || !sameOrderFloat(target.Filled, view.Filled) || !sameOrderFloat(target.Remaining, view.Remaining) || !sameOrderFloat(target.LimitPrice, view.LimitPrice) {
		return fmt.Errorf("modify preview token target quantities/prices changed; preview the replacement again")
	}
	if target.OutsideRTH != view.OutsideRTH {
		return fmt.Errorf("modify preview token target outside_rth changed; preview the replacement again")
	}
	return nil
}

func sameOrderFloat(a, b float64) bool {
	const epsilon = 1e-9
	if a > b {
		return a-b < epsilon
	}
	return b-a < epsilon
}

func paperGateFromStatus(status rpc.TradingStatus) ibkrlib.PaperOrderGate {
	return ibkrlib.PaperOrderGate{
		Mode:     status.Mode,
		Account:  status.Account,
		Endpoint: status.Endpoint,
		Host:     status.GatewayHost,
		Port:     status.GatewayPort,
		ClientID: status.ClientID,
	}
}

func orderJournalEventForDraft(draft rpc.OrderDraft, eventType string, status rpc.TradingStatus, tokenID string, orderID int, at time.Time) orderJournalEvent {
	return orderJournalEvent{
		At:              at,
		Type:            eventType,
		OrderRef:        draft.OrderRef,
		PreviewTokenID:  tokenID,
		ReservedOrderID: orderID,
		ClientID:        status.ClientID,
		Account:         status.Account,
		Endpoint:        status.Endpoint,
		Mode:            status.Mode,
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
		Trail:           cloneTrailSpec(draft.Trail),
		OpenClose:       draft.OpenClose,
		Source:          draft.Source,
	}
}

func orderJournalEventFromView(view rpc.OrderView, eventType string, at time.Time) orderJournalEvent {
	return orderJournalEvent{
		At:              at,
		Type:            eventType,
		OrderRef:        view.OrderRef,
		PreviewTokenID:  view.PreviewTokenID,
		ReservedOrderID: view.ReservedOrderID,
		ClientID:        view.ClientID,
		Account:         view.Account,
		Endpoint:        view.Endpoint,
		Mode:            view.Mode,
		Source:          view.Source,
		PurgeID:         view.PurgeID,
		LegID:           view.LegID,
		BypassPreview:   view.BypassPreview,
		Symbol:          view.Symbol,
		SecType:         view.SecType,
		ConID:           view.ConID,
		Exchange:        view.Exchange,
		PrimaryExch:     view.PrimaryExch,
		Currency:        view.Currency,
		LocalSymbol:     view.LocalSymbol,
		TradingClass:    view.TradingClass,
		Expiry:          view.Expiry,
		Strike:          view.Strike,
		Right:           view.Right,
		Multiplier:      view.Multiplier,
		Action:          view.Action,
		OrderType:       view.OrderType,
		TIF:             view.TIF,
		OutsideRTH:      view.OutsideRTH,
		Quantity:        view.Quantity,
		LimitPrice:      view.LimitPrice,
		Trail:           cloneTrailSpec(view.Trail),
		OpenClose:       view.OpenClose,
		Status:          view.Status,
		Filled:          view.Filled,
		Remaining:       view.Remaining,
		AvgFillPrice:    view.AvgFillPrice,
		LastFillPrice:   view.LastFillPrice,
		WhyHeld:         view.WhyHeld,
		MktCapPrice:     view.MktCapPrice,
	}
}

func (s *Server) appendOrderSendError(draft rpc.OrderDraft, status rpc.TradingStatus, tokenID string, orderID int, sendErr error) {
	if s == nil || s.orderJournal == nil {
		return
	}
	ev := orderJournalEventForDraft(draft, orderJournalEventSendError, status, tokenID, orderID, s.orderNow())
	ev.SendState = orderSendStateUncertainSend
	ev.Message = "broker send returned error; reconcile before reusing this intent: " + sendErr.Error()
	if err := s.orderJournal.Append(ev); err != nil {
		s.warnf("append order send error: %v", err)
	}
}

func orderDraftFromView(view rpc.OrderView) rpc.OrderDraft {
	return rpc.OrderDraft{
		Action: view.Action,
		Contract: rpc.ContractParams{
			ConID:        view.ConID,
			Symbol:       view.Symbol,
			SecType:      view.SecType,
			Exchange:     view.Exchange,
			PrimaryExch:  view.PrimaryExch,
			Currency:     view.Currency,
			LocalSymbol:  view.LocalSymbol,
			TradingClass: view.TradingClass,
			Expiry:       view.Expiry,
			Strike:       view.Strike,
			Right:        view.Right,
			Multiplier:   view.Multiplier,
		},
		Quantity:   int(view.Quantity),
		OrderType:  view.OrderType,
		LimitPrice: view.LimitPrice,
		Trail:      cloneTrailSpec(view.Trail),
		TIF:        view.TIF,
		OutsideRTH: view.OutsideRTH,
		OrderRef:   view.OrderRef,
		OpenClose:  view.OpenClose,
	}
}
