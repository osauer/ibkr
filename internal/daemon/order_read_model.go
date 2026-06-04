package daemon

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/rpc"
)

func (s *Server) handleOrdersOpen(_ context.Context, req *rpc.Request) (*rpc.OrdersOpenResult, error) {
	var p rpc.OrdersOpenParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		return nil, err
	}
	account := strings.TrimSpace(p.Account)
	out := make([]rpc.OrderView, 0, len(views))
	for _, v := range views {
		if account != "" && !strings.EqualFold(v.Account, account) {
			continue
		}
		if v.Open {
			out = append(out, v)
		}
	}
	sortOrderViews(out)
	return &rpc.OrdersOpenResult{Orders: out, AsOf: s.orderNow()}, nil
}

func (s *Server) handleOrderStatus(_ context.Context, req *rpc.Request) (*rpc.OrderStatusResult, error) {
	var p rpc.OrderStatusParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return nil, errBadRequest("order status id is required")
	}
	views, eventsByKey, err := s.loadOrderViews()
	if err != nil {
		return nil, err
	}
	for _, view := range views {
		if !orderViewMatchesID(view, id) {
			continue
		}
		events := append([]rpc.OrderEvent{}, eventsByKey[orderViewKey(view)]...)
		return &rpc.OrderStatusResult{
			Found:  true,
			Order:  view,
			Events: events,
			AsOf:   s.orderNow(),
		}, nil
	}
	return &rpc.OrderStatusResult{Found: false, AsOf: s.orderNow()}, nil
}

func (s *Server) loadOrderViews() ([]rpc.OrderView, map[string][]rpc.OrderEvent, error) {
	if s == nil || s.orderJournal == nil {
		return nil, nil, fmt.Errorf("%w: order journal is unavailable", ErrTradingDisabled)
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return nil, nil, err
	}
	return buildOrderViews(events), buildOrderEventsByKey(events), nil
}

func buildOrderViews(events []orderJournalEvent) []rpc.OrderView {
	viewsByKey := map[string]rpc.OrderView{}
	for _, ev := range events {
		key := orderJournalKey(ev)
		if key == "" {
			continue
		}
		view := viewsByKey[key]
		mergeOrderJournalEventIntoView(&view, ev)
		viewsByKey[key] = view
	}
	views := make([]rpc.OrderView, 0, len(viewsByKey))
	for _, view := range viewsByKey {
		view.LifecycleStatus = mapOrderViewLifecycleStatus(view)
		view.Open = orderViewIsOpen(view)
		views = append(views, view)
	}
	sortOrderViews(views)
	return views
}

func buildOrderEventsByKey(events []orderJournalEvent) map[string][]rpc.OrderEvent {
	out := map[string][]rpc.OrderEvent{}
	for _, ev := range events {
		key := orderJournalKey(ev)
		if key == "" {
			continue
		}
		out[key] = append(out[key], orderEventFromJournal(ev))
	}
	for key := range out {
		slices.SortStableFunc(out[key], func(a, b rpc.OrderEvent) int {
			return a.At.Compare(b.At)
		})
	}
	return out
}

func mergeOrderJournalEventIntoView(view *rpc.OrderView, ev orderJournalEvent) {
	if view.OrderRef == "" {
		view.OrderRef = ev.OrderRef
	}
	if view.PreviewTokenID == "" {
		view.PreviewTokenID = ev.PreviewTokenID
	}
	if ev.ReservedOrderID != 0 {
		view.ReservedOrderID = ev.ReservedOrderID
	}
	if ev.ClientID != 0 {
		view.ClientID = ev.ClientID
	}
	if ev.PermID != 0 {
		view.PermID = ev.PermID
	}
	if ev.Account != "" {
		view.Account = ev.Account
	}
	if ev.Endpoint != "" {
		view.Endpoint = ev.Endpoint
	}
	if ev.Mode != "" {
		view.Mode = ev.Mode
	}
	if ev.Symbol != "" {
		view.Symbol = ev.Symbol
	}
	if ev.SecType != "" {
		view.SecType = ev.SecType
	}
	if ev.Action != "" {
		view.Action = ev.Action
	}
	if ev.OrderType != "" {
		view.OrderType = ev.OrderType
	}
	if ev.TIF != "" {
		view.TIF = ev.TIF
	}
	if ev.Quantity != 0 {
		view.Quantity = ev.Quantity
	}
	if ev.LimitPrice != 0 {
		view.LimitPrice = ev.LimitPrice
	}
	if ev.Status != "" {
		view.Status = ev.Status
	}
	if ev.Filled != 0 {
		view.Filled = ev.Filled
	}
	if ev.Remaining != 0 {
		view.Remaining = ev.Remaining
	}
	if ev.AvgFillPrice != 0 {
		view.AvgFillPrice = ev.AvgFillPrice
	}
	if ev.LastFillPrice != 0 {
		view.LastFillPrice = ev.LastFillPrice
	}
	if ev.SendState != "" {
		view.SendState = ev.SendState
	}
	if ev.Message != "" {
		view.LastMessage = ev.Message
	}
	if !ev.At.IsZero() && (view.UpdatedAt.IsZero() || ev.At.After(view.UpdatedAt)) {
		view.UpdatedAt = ev.At
		view.LastEvent = ev.Type
	}
}

func orderEventFromJournal(ev orderJournalEvent) rpc.OrderEvent {
	return rpc.OrderEvent{
		At:              ev.At,
		Type:            ev.Type,
		OrderRef:        ev.OrderRef,
		PreviewTokenID:  ev.PreviewTokenID,
		ReservedOrderID: ev.ReservedOrderID,
		ClientID:        ev.ClientID,
		PermID:          ev.PermID,
		Account:         ev.Account,
		Endpoint:        ev.Endpoint,
		Mode:            ev.Mode,
		Symbol:          ev.Symbol,
		SecType:         ev.SecType,
		Action:          ev.Action,
		OrderType:       ev.OrderType,
		TIF:             ev.TIF,
		Quantity:        ev.Quantity,
		LimitPrice:      ev.LimitPrice,
		Status:          ev.Status,
		LifecycleStatus: mapOrderJournalLifecycleStatus(ev),
		Filled:          ev.Filled,
		Remaining:       ev.Remaining,
		AvgFillPrice:    ev.AvgFillPrice,
		LastFillPrice:   ev.LastFillPrice,
		ExecID:          ev.ExecID,
		ExecTime:        ev.ExecTime,
		SendState:       ev.SendState,
		Message:         ev.Message,
	}
}

func orderJournalEventFromLifecycle(ev ibkrlib.OrderLifecycleEvent, at time.Time) (orderJournalEvent, bool) {
	out := orderJournalEvent{
		At:              at,
		OrderRef:        ev.OrderRef,
		ReservedOrderID: ev.OrderID,
		ClientID:        ev.ClientID,
		PermID:          ev.PermID,
		Account:         ev.Account,
		Symbol:          ev.Symbol,
		SecType:         ev.SecType,
		Action:          ev.Action,
		OrderType:       ev.OrderType,
		TIF:             ev.TIF,
		Quantity:        ev.TotalQuantity,
		LimitPrice:      ev.LimitPrice,
		Status:          ev.Status,
		Filled:          ev.Filled,
		Remaining:       ev.Remaining,
		AvgFillPrice:    ev.AvgFillPrice,
		LastFillPrice:   ev.LastFillPrice,
		ExecID:          ev.ExecID,
		ExecTime:        ev.ExecTime,
	}
	switch ev.Type {
	case ibkrlib.OrderLifecycleEventOpenOrder:
		out.Type = orderJournalEventBrokerAcknowledged
		out.SendState = orderSendStateBrokerAcknowledged
		if out.Status == "" {
			out.Status = "Submitted"
		}
	case ibkrlib.OrderLifecycleEventStatus:
		out.Type = orderJournalEventStatusUpdated
		if orderLifecycleStatusIsTerminal(mapBrokerOrderLifecycleStatus(ev.Status, ev.Filled, ev.Remaining)) {
			out.SendState = orderSendStateTerminal
		} else if ev.Status != "" {
			out.SendState = orderSendStateBrokerAcknowledged
		}
	case ibkrlib.OrderLifecycleEventExecDetails:
		out.Type = orderJournalEventStatusUpdated
		out.Status = "Execution"
		out.Filled = ev.CumQty
		out.LastFillPrice = ev.Price
		out.AvgFillPrice = ev.AvgFillPrice
	default:
		return orderJournalEvent{}, false
	}
	return out, out.ReservedOrderID > 0 || out.OrderRef != "" || out.PermID > 0
}

func mapOrderJournalLifecycleStatus(ev orderJournalEvent) string {
	if ev.Status != "" {
		return mapBrokerOrderLifecycleStatus(ev.Status, ev.Filled, ev.Remaining)
	}
	switch ev.Type {
	case orderJournalEventPreviewed, orderJournalEventTokenConfirmed:
		return rpc.OrderLifecyclePreviewed
	case orderJournalEventSendAttempted:
		return rpc.OrderLifecyclePendingSubmit
	case orderJournalEventBrokerAcknowledged:
		return rpc.OrderLifecycleSubmitted
	case orderJournalEventCancelRequested:
		return rpc.OrderLifecyclePendingCancel
	case orderJournalEventReconciledUnknown:
		return rpc.OrderLifecycleUnknownReconcileRequired
	default:
		if ev.SendState == orderSendStateUncertainSend {
			return rpc.OrderLifecycleUnknownReconcileRequired
		}
		return rpc.OrderLifecyclePreviewed
	}
}

func mapOrderViewLifecycleStatus(view rpc.OrderView) string {
	if view.Status != "" {
		return mapBrokerOrderLifecycleStatus(view.Status, view.Filled, view.Remaining)
	}
	switch view.LastEvent {
	case orderJournalEventPreviewed, orderJournalEventTokenConfirmed:
		return rpc.OrderLifecyclePreviewed
	case orderJournalEventSendAttempted:
		return rpc.OrderLifecyclePendingSubmit
	case orderJournalEventBrokerAcknowledged:
		return rpc.OrderLifecycleSubmitted
	case orderJournalEventCancelRequested:
		return rpc.OrderLifecyclePendingCancel
	case orderJournalEventReconciledUnknown:
		return rpc.OrderLifecycleUnknownReconcileRequired
	default:
		if view.SendState == orderSendStateUncertainSend {
			return rpc.OrderLifecycleUnknownReconcileRequired
		}
		return rpc.OrderLifecyclePreviewed
	}
}

func mapBrokerOrderLifecycleStatus(status string, filled, remaining float64) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pendingcancel":
		return rpc.OrderLifecyclePendingCancel
	case "pendingsubmit":
		return rpc.OrderLifecyclePendingSubmit
	case "presubmitted":
		return rpc.OrderLifecyclePreSubmitted
	case "submitted", "apipending":
		if filled > 0 && remaining > 0 {
			return rpc.OrderLifecyclePartiallyFilled
		}
		return rpc.OrderLifecycleSubmitted
	case "filled":
		return rpc.OrderLifecycleFilled
	case "cancelled", "apicancelled":
		return rpc.OrderLifecycleCancelled
	case "inactive":
		return rpc.OrderLifecycleInactive
	case "rejected":
		return rpc.OrderLifecycleRejected
	case "execution":
		return rpc.OrderLifecyclePartiallyFilled
	default:
		if filled > 0 && remaining > 0 {
			return rpc.OrderLifecyclePartiallyFilled
		}
		if filled > 0 && remaining == 0 {
			return rpc.OrderLifecycleFilled
		}
		return rpc.OrderLifecycleUnknownReconcileRequired
	}
}

func orderLifecycleStatusIsTerminal(status string) bool {
	switch status {
	case rpc.OrderLifecycleFilled, rpc.OrderLifecycleCancelled, rpc.OrderLifecycleRejected, rpc.OrderLifecycleInactive:
		return true
	default:
		return false
	}
}

func orderViewIsOpen(view rpc.OrderView) bool {
	if orderLifecycleStatusIsTerminal(view.LifecycleStatus) {
		return false
	}
	switch view.LastEvent {
	case orderJournalEventSendAttempted, orderJournalEventBrokerAcknowledged, orderJournalEventStatusUpdated, orderJournalEventModifyRequested, orderJournalEventCancelRequested, orderJournalEventReconciledUnknown:
		return true
	default:
		return view.SendState == orderSendStateReserved ||
			view.SendState == orderSendStateSendAttempted ||
			view.SendState == orderSendStateBrokerAcknowledged ||
			view.SendState == orderSendStateUncertainSend
	}
}

func orderJournalKey(ev orderJournalEvent) string {
	if ev.OrderRef != "" {
		return "ref:" + ev.OrderRef
	}
	if ev.ReservedOrderID != 0 {
		return "order:" + strconv.Itoa(ev.ReservedOrderID)
	}
	if ev.PermID != 0 {
		return "perm:" + strconv.Itoa(ev.PermID)
	}
	return ""
}

func orderViewKey(view rpc.OrderView) string {
	if view.OrderRef != "" {
		return "ref:" + view.OrderRef
	}
	if view.ReservedOrderID != 0 {
		return "order:" + strconv.Itoa(view.ReservedOrderID)
	}
	if view.PermID != 0 {
		return "perm:" + strconv.Itoa(view.PermID)
	}
	return ""
}

func orderViewMatchesID(view rpc.OrderView, id string) bool {
	if view.OrderRef == id {
		return true
	}
	if view.ReservedOrderID != 0 && strconv.Itoa(view.ReservedOrderID) == id {
		return true
	}
	return view.PermID != 0 && strconv.Itoa(view.PermID) == id
}

func sortOrderViews(views []rpc.OrderView) {
	slices.SortStableFunc(views, func(a, b rpc.OrderView) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
}

func (s *Server) orderNow() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}
