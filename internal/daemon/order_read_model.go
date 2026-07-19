package daemon

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"

	"github.com/osauer/ibkr/v2/internal/marketcal"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

const (
	orderHistoryDefaultLookback = 7 * 24 * time.Hour
	orderHistoryDefaultLimit    = 50
	orderHistoryMaxLimit        = 500
	orderHistoryDefaultEvents   = 20
	orderHistoryMaxEvents       = 200
)

func (s *Server) handleOrdersOpen(ctx context.Context, req *rpc.Request) (*rpc.OrdersOpenResult, error) {
	var p rpc.OrdersOpenParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	views, eventsByKey, err := s.loadOrderViewsReconciled(ctx)
	if err != nil {
		return nil, err
	}
	scope := s.currentBrokerStateScope()
	out := make([]rpc.OrderView, 0, len(views))
	for _, v := range views {
		if !orderViewMatchesBrokerScope(v, scope) {
			continue
		}
		if v.Open {
			out = append(out, v)
		}
	}
	sortOrderViews(out)
	return &rpc.OrdersOpenResult{
		Orders:             out,
		AsOf:               s.orderNow(),
		Account:            scope.Account,
		Mode:               scope.Mode,
		LastLocalEventAt:   latestScopedOrderEventAt(views, eventsByKey, scope),
		NotBrokerStatement: orderHistoryNotBrokerStatement(),
		Limitations:        orderHistoryLimitations(),
	}, nil
}

func (s *Server) handleOrdersHistory(_ context.Context, req *rpc.Request) (*rpc.OrdersHistoryResult, error) {
	var p rpc.OrdersHistoryParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	now := s.orderNow()
	since, until, err := orderHistoryRange(p, now)
	if err != nil {
		return nil, err
	}
	limit, err := orderHistoryLimit(p.Limit)
	if err != nil {
		return nil, err
	}
	eventLimit, err := orderHistoryEventLimit(p.EventLimit)
	if err != nil {
		return nil, err
	}
	scope := s.currentBrokerStateScope()
	journalEvents, err := s.loadScopedOrderHistoryEvents(since, until, scope)
	if err != nil {
		return nil, err
	}
	views := buildOrderViews(journalEvents)
	eventsByKey := buildOrderEventsByKey(journalEvents)
	inferDayOrderExpiry(views, eventsByKey, now)
	rows := make([]rpc.OrdersHistoryRow, 0, len(views))
	for _, view := range views {
		if !orderViewMatchesBrokerScope(view, scope) {
			continue
		}
		events := eventsByKey[orderViewKey(view)]
		if len(events) == 0 {
			continue
		}
		totalEvents := len(events)
		events, eventsTruncated := limitOrderHistoryEvents(events, eventLimit)
		rows = append(rows, rpc.OrdersHistoryRow{
			Order:            view,
			Events:           events,
			EventsCount:      len(events),
			TotalEventsCount: totalEvents,
			EventsTruncated:  eventsTruncated,
		})
	}
	sortOrderHistoryRows(rows)
	totalCount := len(rows)
	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}
	eventsCount := 0
	totalEventsCount := 0
	eventsTruncated := false
	for _, row := range rows {
		eventsCount += row.EventsCount
		totalEventsCount += row.TotalEventsCount
		eventsTruncated = eventsTruncated || row.EventsTruncated
	}
	return &rpc.OrdersHistoryResult{
		Orders:             rows,
		AsOf:               now,
		Since:              since,
		Until:              until,
		Account:            scope.Account,
		Mode:               scope.Mode,
		Count:              len(rows),
		TotalCount:         totalCount,
		EventsCount:        eventsCount,
		TotalEventsCount:   totalEventsCount,
		Limit:              limit,
		EventLimit:         eventLimit,
		Truncated:          truncated,
		EventsTruncated:    eventsTruncated,
		NotBrokerStatement: orderHistoryNotBrokerStatement(),
		Limitations:        orderHistoryLimitations(),
	}, nil
}

func (s *Server) handleOrderStatus(ctx context.Context, req *rpc.Request) (*rpc.OrderStatusResult, error) {
	var p rpc.OrderStatusParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return nil, errBadRequest("order status id is required")
	}
	views, eventsByKey, err := s.loadOrderViewsReconciled(ctx)
	if err != nil {
		return nil, err
	}
	scope := s.currentBrokerStateScope()
	for _, view := range views {
		if !orderViewMatchesID(view, id) {
			continue
		}
		if !orderViewMatchesBrokerScope(view, scope) {
			continue
		}
		events := append([]rpc.OrderEvent{}, eventsByKey[orderViewKey(view)]...)
		lastLocalEventAt := latestOrderEventAt(events)
		return &rpc.OrderStatusResult{
			Found:              true,
			Order:              view,
			Events:             events,
			AsOf:               s.orderNow(),
			Account:            scope.Account,
			Mode:               scope.Mode,
			LastLocalEventAt:   lastLocalEventAt,
			NotBrokerStatement: orderHistoryNotBrokerStatement(),
			Limitations:        orderHistoryLimitations(),
		}, nil
	}
	return &rpc.OrderStatusResult{
		Found:              false,
		AsOf:               s.orderNow(),
		Account:            scope.Account,
		Mode:               scope.Mode,
		LastLocalEventAt:   latestScopedOrderEventAt(views, eventsByKey, scope),
		NotBrokerStatement: orderHistoryNotBrokerStatement(),
		Limitations:        orderHistoryLimitations(),
	}, nil
}

func orderHistoryRange(p rpc.OrdersHistoryParams, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	until := now
	if raw := strings.TrimSpace(p.Until); raw != "" {
		parsed, dateOnly, err := parseOrderHistoryTime(raw)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		until = parsed
		if dateOnly {
			until = until.Add(24 * time.Hour)
		}
	}
	since := until.Add(-orderHistoryDefaultLookback)
	if raw := strings.TrimSpace(p.Since); raw != "" {
		parsed, _, err := parseOrderHistoryTime(raw)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		since = parsed
	}
	if !since.Before(until) {
		return time.Time{}, time.Time{}, errBadRequest("orders history: since must be before until")
	}
	return since.UTC(), until.UTC(), nil
}

func parseOrderHistoryTime(raw string) (time.Time, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false, errBadRequest("orders history: empty time boundary")
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), false, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", raw, time.UTC); err == nil {
		return t.UTC(), true, nil
	}
	return time.Time{}, false, errBadRequest("orders history: time boundaries must be YYYY-MM-DD or RFC3339")
}

func orderHistoryLimit(limit int) (int, error) {
	if limit == 0 {
		return orderHistoryDefaultLimit, nil
	}
	if limit < 0 || limit > orderHistoryMaxLimit {
		return 0, errBadRequest(fmt.Sprintf("orders history: limit must be between 1 and %d", orderHistoryMaxLimit))
	}
	return limit, nil
}

func orderHistoryEventLimit(limit int) (int, error) {
	if limit == 0 {
		return orderHistoryDefaultEvents, nil
	}
	if limit < 0 || limit > orderHistoryMaxEvents {
		return 0, errBadRequest(fmt.Sprintf("orders history: event_limit must be between 1 and %d", orderHistoryMaxEvents))
	}
	return limit, nil
}

func (s *Server) loadScopedOrderHistoryEvents(since, until time.Time, scope brokerStateScope) ([]orderJournalEvent, error) {
	if s == nil || s.orderJournal == nil {
		return nil, fmt.Errorf("%w: order journal is unavailable", ErrTradingDisabled)
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return nil, err
	}
	out := make([]orderJournalEvent, 0, len(events))
	for _, ev := range events {
		if !orderJournalEventMatchesBrokerScope(ev, scope) || !orderJournalEventInRange(ev, since, until) {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

func orderJournalEventMatchesBrokerScope(ev orderJournalEvent, scope brokerStateScope) bool {
	return brokerScopedAccountMatches(ev.Account, scope) &&
		brokerScopedModeMatches(ev.Mode, scope.Mode)
}

func orderJournalEventInRange(ev orderJournalEvent, since, until time.Time) bool {
	at := ev.At.UTC()
	return !at.IsZero() && !at.Before(since) && at.Before(until)
}

func limitOrderHistoryEvents(events []rpc.OrderEvent, limit int) ([]rpc.OrderEvent, bool) {
	if limit <= 0 || len(events) <= limit {
		return events, false
	}
	if limit == 1 {
		return append([]rpc.OrderEvent(nil), events[0]), true
	}
	head := limit / 2
	tail := limit - head
	out := make([]rpc.OrderEvent, 0, limit)
	out = append(out, events[:head]...)
	out = append(out, events[len(events)-tail:]...)
	return out, true
}

func sortOrderHistoryRows(rows []rpc.OrdersHistoryRow) {
	slices.SortStableFunc(rows, func(a, b rpc.OrdersHistoryRow) int {
		cmp := b.Order.UpdatedAt.Compare(a.Order.UpdatedAt)
		if cmp != 0 {
			return cmp
		}
		return latestOrderHistoryEventAt(b).Compare(latestOrderHistoryEventAt(a))
	})
}

func latestOrderHistoryEventAt(row rpc.OrdersHistoryRow) time.Time {
	if len(row.Events) == 0 {
		return time.Time{}
	}
	return row.Events[len(row.Events)-1].At
}

func latestScopedOrderEventAt(views []rpc.OrderView, eventsByKey map[string][]rpc.OrderEvent, scope brokerStateScope) time.Time {
	var latest time.Time
	for _, view := range views {
		if !orderViewMatchesBrokerScope(view, scope) {
			continue
		}
		if at := latestOrderEventAt(eventsByKey[orderViewKey(view)]); at.After(latest) {
			latest = at
		}
	}
	return latest
}

func latestOrderEventAt(events []rpc.OrderEvent) time.Time {
	if len(events) == 0 {
		return time.Time{}
	}
	return events[len(events)-1].At
}

func orderHistoryNotBrokerStatement() string {
	return "local order journal only; not an IBKR Activity Statement, trade confirmation, execution report, or historical broker audit"
}

func orderHistoryLimitations() []string {
	return []string{
		"Reduced from the daemon's local append-only order journal for the current broker account/mode only.",
		"May miss manual orders, other-client orders, broker activity while the daemon was offline, and rows outside the selected account/mode scope.",
		"Broker callbacks remain authoritative when journaled; absence of a local event is not proof that no broker activity occurred.",
	}
}

func (s *Server) loadOrderViews() ([]rpc.OrderView, map[string][]rpc.OrderEvent, error) {
	if s == nil || s.orderJournal == nil {
		return nil, nil, fmt.Errorf("%w: order journal is unavailable", ErrTradingDisabled)
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return nil, nil, err
	}
	views := buildOrderViews(events)
	eventsByKey := buildOrderEventsByKey(events)
	inferDayOrderExpiry(views, eventsByKey, s.orderNow())
	return views, eventsByKey, nil
}

func (s *Server) loadOrderViewsReconciled(ctx context.Context) ([]rpc.OrderView, map[string][]rpc.OrderEvent, error) {
	views, eventsByKey, err := s.loadOrderViews()
	if err != nil {
		return nil, nil, err
	}
	pos, posErr := s.handlePositionsList(ctx, &rpc.Request{})
	if posErr == nil {
		reconcileFlatPositionProtectiveOrders(views, pos, s.orderNow())
	}
	return views, eventsByKey, nil
}

func reconcileFlatPositionProtectiveOrders(views []rpc.OrderView, pos *rpc.PositionsResult, now time.Time) {
	if pos == nil {
		return
	}
	for i := range views {
		view := &views[i]
		if !view.Open || !orderViewIsCloseProtective(*view) {
			continue
		}
		current := positionQuantityForOrderView(pos, *view)
		remaining := orderViewRemainingQuantity(*view)
		if orderViewActionCanCloseQuantity(*view, current, remaining) {
			continue
		}
		view.ModifyEligible = false
		view.LifecycleStatus = rpc.OrderLifecycleUnknownReconcileRequired
		view.ReconciliationState = "position_mismatch"
		view.BrokerTruthAsOf = now
		view.LastMessage = fmt.Sprintf("current position %.4g no longer supports close-only protective order remaining %.4g; broker reconciliation required", current, remaining)
		classifyProtectiveMismatch(view, current, remaining)
	}
}

// classifyProtectiveMismatch grades a position_mismatch by its consequence.
// coverage is the position magnitude available in the order's closing
// direction; whatever the order's remaining quantity exceeds it by would open
// a position in the opposite direction on trigger. Both kinds are critical —
// the damaging event is the same — they differ only in the offered fix:
// no coverage → cancel; partial coverage → reduce to exactly the coverage.
func classifyProtectiveMismatch(view *rpc.OrderView, current, remaining float64) {
	coverage := protectiveCloseCoverage(*view, current)
	view.ReconciliationSeverity = rpc.OrderReconciliationSeverityCritical
	if coverage > 0 && coverage < remaining {
		view.ReconciliationKind = rpc.OrderReconciliationKindShortEntryExcess
		view.ShortRiskQuantity = remaining - coverage
		view.ReduceToQuantity = coverage
		return
	}
	view.ReconciliationKind = rpc.OrderReconciliationKindShortEntryFull
	view.ShortRiskQuantity = remaining
}

// protectiveCloseCoverage is the position quantity available in the order's
// closing direction: long shares for a SELL close, short magnitude for a BUY
// cover. Zero or negative means the position cannot absorb any part of the
// order.
func protectiveCloseCoverage(view rpc.OrderView, current float64) float64 {
	switch strings.ToUpper(strings.TrimSpace(view.Action)) {
	case rpc.OrderActionSell:
		return current
	case rpc.OrderActionBuy:
		return -current
	default:
		return 0
	}
}

func orderViewIsCloseProtective(view rpc.OrderView) bool {
	if !strings.EqualFold(view.OpenClose, "C") && !strings.EqualFold(view.Source, proposalOrderSource) {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(view.OrderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT, rpc.OrderTypeLMT:
		return true
	default:
		return false
	}
}

func orderViewRemainingQuantity(view rpc.OrderView) float64 {
	if view.Remaining > 0 {
		return view.Remaining
	}
	if view.Quantity > 0 && view.Filled < view.Quantity {
		return view.Quantity - view.Filled
	}
	return view.Quantity
}

func orderViewActionCanCloseQuantity(view rpc.OrderView, current, remaining float64) bool {
	if remaining <= 0 {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(view.Action)) {
	case rpc.OrderActionSell:
		return current > 0 && current+1e-9 >= remaining
	case rpc.OrderActionBuy:
		return current < 0 && math.Abs(current)+1e-9 >= remaining
	default:
		return false
	}
}

func positionQuantityForOrderView(pos *rpc.PositionsResult, view rpc.OrderView) float64 {
	if pos == nil {
		return 0
	}
	if strings.EqualFold(view.SecType, "OPT") || strings.EqualFold(view.SecType, "OPTION") {
		for _, row := range pos.Options {
			if positionViewMatchesOrderView(row, view) {
				return row.Quantity
			}
		}
		return 0
	}
	var qty float64
	for _, row := range pos.Stocks {
		if positionViewMatchesOrderView(row, view) {
			qty += row.Quantity
		}
	}
	return qty
}

func positionViewMatchesOrderView(row rpc.PositionView, view rpc.OrderView) bool {
	if view.ConID != 0 && row.ConID != 0 {
		return view.ConID == row.ConID
	}
	if !strings.EqualFold(row.Symbol, view.Symbol) {
		return false
	}
	if strings.EqualFold(view.SecType, "OPT") || strings.EqualFold(view.SecType, "OPTION") {
		if !strings.EqualFold(row.SecType, "OPT") && !strings.EqualFold(row.SecType, "OPTION") {
			return false
		}
		return strings.EqualFold(row.Expiry, view.Expiry) &&
			strings.EqualFold(row.Right, view.Right) &&
			math.Abs(row.Strike-view.Strike) < 1e-9
	}
	return equivalentStockSecType(row.SecType, view.SecType)
}

// inferDayOrderExpiry marks non-terminal stock/ETF DAY orders whose effective
// session closed well in the past as expired_inferred. It complements the
// broker open-order snapshot reconcile (order_reconcile.go): calendar
// inference closes DAY rows immediately after their session, without waiting
// for the next snapshot sweep, and works even while disconnected. The state
// is local calendar inference, never broker-confirmed: rows are closed for
// display and duplicate-suppression but stay cancel-ineligible.
func inferDayOrderExpiry(views []rpc.OrderView, eventsByKey map[string][]rpc.OrderEvent, now time.Time) {
	cal := marketcal.New()
	for i := range views {
		view := &views[i]
		if !view.Open || !strings.EqualFold(view.TIF, rpc.OrderTIFDay) {
			continue
		}
		if !strings.EqualFold(view.SecType, "STK") && !strings.EqualFold(view.SecType, "ETF") {
			continue
		}
		placed := orderViewPlacedAt(*view, eventsByKey)
		if placed.IsZero() {
			continue
		}
		deadline, ok := dayOrderSessionDeadline(cal, *view, placed)
		// One hour of margin past the close absorbs late broker callbacks.
		if !ok || now.Before(deadline.Add(time.Hour)) {
			continue
		}
		view.LifecycleStatus = rpc.OrderLifecycleExpiredInferred
		view.Open = false
		view.ModifyEligible = false
		view.CancelEligible = false
		view.LastMessage = "DAY order is past its session close; expiry inferred locally (not broker-confirmed)"
	}
}

func orderViewPlacedAt(view rpc.OrderView, eventsByKey map[string][]rpc.OrderEvent) time.Time {
	if view.OrderRef != "" {
		if events := eventsByKey["ref:"+view.OrderRef]; len(events) > 0 {
			return events[0].At
		}
	}
	return view.UpdatedAt
}

// dayOrderSessionDeadline returns the close of the first session whose close
// follows placement. An order placed off-hours works the *next* session, so
// placement-day inference would mark it dead prematurely and let the
// duplicate-suppression layer place a doubled order at the open.
func dayOrderSessionDeadline(cal *marketcal.Calendar, view rpc.OrderView, placed time.Time) (time.Time, bool) {
	market := quoteMarketForStockContract(rpc.ContractParams{
		Exchange:    view.Exchange,
		PrimaryExch: view.PrimaryExch,
	})
	ses, err := cal.SessionAt(market, placed)
	if err != nil {
		return time.Time{}, false
	}
	if !ses.Close.IsZero() && placed.Before(ses.Close) {
		return ses.Close, true
	}
	if ses.NextClose != nil {
		return *ses.NextClose, true
	}
	return time.Time{}, false
}

func buildOrderViews(events []orderJournalEvent) []rpc.OrderView {
	aliases := orderJournalKeyAliases(events)
	viewsByKey := map[string]rpc.OrderView{}
	for _, ev := range events {
		key := orderJournalCanonicalKey(ev, aliases)
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
		view.ModifyEligible = orderViewModifyEligible(view)
		view.CancelEligible = orderViewCancelEligible(view)
		views = append(views, view)
	}
	sortOrderViews(views)
	return views
}

func buildOrderEventsByKey(events []orderJournalEvent) map[string][]rpc.OrderEvent {
	aliases := orderJournalKeyAliases(events)
	out := map[string][]rpc.OrderEvent{}
	for _, ev := range events {
		key := orderJournalCanonicalKey(ev, aliases)
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

// orderJournalEventFromView projects a folded view back into a journal event
// carrying the row's full identity. Lives untagged (not in the trading-build
// file) because the reconcile sweep needs it in read-only builds too.
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
		TriggerMethod:   view.TriggerMethod,
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

func mergeOrderJournalEventIntoView(view *rpc.OrderView, ev orderJournalEvent) {
	preserveWorkingOrderOnBrokerError := brokerErrorShouldPreserveWorkingOrder(*view, ev)
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
	if ev.Source != "" {
		view.Source = ev.Source
	}
	if ev.PurgeID != "" {
		view.PurgeID = ev.PurgeID
	}
	if ev.LegID != "" {
		view.LegID = ev.LegID
	}
	if ev.BypassPreview {
		view.BypassPreview = true
	}
	if ev.Symbol != "" {
		view.Symbol = ev.Symbol
	}
	if ev.SecType != "" {
		view.SecType = ev.SecType
	}
	if ev.ConID != 0 {
		view.ConID = ev.ConID
	}
	if ev.Exchange != "" {
		view.Exchange = ev.Exchange
	}
	if ev.PrimaryExch != "" {
		view.PrimaryExch = ev.PrimaryExch
	}
	if ev.Currency != "" {
		view.Currency = ev.Currency
	}
	if ev.LocalSymbol != "" {
		view.LocalSymbol = ev.LocalSymbol
	}
	if ev.TradingClass != "" {
		view.TradingClass = ev.TradingClass
	}
	if ev.Expiry != "" {
		view.Expiry = ev.Expiry
	}
	if ev.Strike != 0 {
		view.Strike = ev.Strike
	}
	if ev.Right != "" {
		view.Right = ev.Right
	}
	if ev.Multiplier != 0 {
		view.Multiplier = ev.Multiplier
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
	if ev.TriggerMethod != 0 {
		view.TriggerMethod = ev.TriggerMethod
	}
	if ev.OutsideRTH {
		view.OutsideRTH = true
	}
	if ev.Quantity != 0 {
		view.Quantity = ev.Quantity
	}
	if ev.LimitPrice != 0 {
		view.LimitPrice = ev.LimitPrice
	}
	if ev.Trail != nil {
		view.Trail = cloneTrailSpec(ev.Trail)
	}
	if ev.OpenClose != "" {
		view.OpenClose = ev.OpenClose
	}
	if ev.Status != "" && !preserveWorkingOrderOnBrokerError {
		view.Status = ev.Status
	}
	if ev.Filled != 0 {
		view.Filled = ev.Filled
	}
	if ev.Remaining != 0 || orderJournalEventCarriesZeroRemaining(ev) {
		view.Remaining = ev.Remaining
	}
	if ev.AvgFillPrice != 0 {
		view.AvgFillPrice = ev.AvgFillPrice
	}
	if ev.LastFillPrice != 0 {
		view.LastFillPrice = ev.LastFillPrice
	}
	if ev.WhyHeld != "" {
		view.WhyHeld = ev.WhyHeld
	}
	if ev.MktCapPrice != 0 {
		view.MktCapPrice = ev.MktCapPrice
	}
	if ev.SendState != "" {
		if preserveWorkingOrderOnBrokerError && ev.SendState == orderSendStateTerminal {
			view.SendState = orderSendStateUncertainSend
		} else {
			view.SendState = ev.SendState
		}
	}
	if ev.Message != "" {
		view.LastMessage = ev.Message
	}
	if !ev.At.IsZero() && (view.UpdatedAt.IsZero() || ev.At.After(view.UpdatedAt)) {
		view.UpdatedAt = ev.At
		view.LastEvent = ev.Type
	}
}

func orderJournalEventCarriesZeroRemaining(ev orderJournalEvent) bool {
	if ev.Remaining != 0 {
		return true
	}
	return strings.EqualFold(ev.Status, "Filled") ||
		(strings.EqualFold(ev.Status, "Cancelled") && ev.Filled == ev.Quantity && ev.Quantity > 0)
}

func brokerErrorShouldPreserveWorkingOrder(view rpc.OrderView, ev orderJournalEvent) bool {
	if ev.Type != orderJournalEventBrokerError {
		return false
	}
	switch view.LastEvent {
	case orderJournalEventModifyRequested, orderJournalEventCancelRequested:
		return true
	default:
		return false
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
		Source:          ev.Source,
		PurgeID:         ev.PurgeID,
		LegID:           ev.LegID,
		BypassPreview:   ev.BypassPreview,
		Symbol:          ev.Symbol,
		SecType:         ev.SecType,
		ConID:           ev.ConID,
		Exchange:        ev.Exchange,
		PrimaryExch:     ev.PrimaryExch,
		Currency:        ev.Currency,
		LocalSymbol:     ev.LocalSymbol,
		TradingClass:    ev.TradingClass,
		Expiry:          ev.Expiry,
		Strike:          ev.Strike,
		Right:           ev.Right,
		Multiplier:      ev.Multiplier,
		Action:          ev.Action,
		OrderType:       ev.OrderType,
		TIF:             ev.TIF,
		TriggerMethod:   ev.TriggerMethod,
		OutsideRTH:      ev.OutsideRTH,
		Quantity:        ev.Quantity,
		LimitPrice:      ev.LimitPrice,
		Trail:           cloneTrailSpec(ev.Trail),
		OpenClose:       ev.OpenClose,
		Status:          ev.Status,
		LifecycleStatus: mapOrderJournalLifecycleStatus(ev),
		Filled:          ev.Filled,
		Remaining:       ev.Remaining,
		AvgFillPrice:    ev.AvgFillPrice,
		LastFillPrice:   ev.LastFillPrice,
		WhyHeld:         ev.WhyHeld,
		MktCapPrice:     ev.MktCapPrice,
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
		Exchange:        ev.Exchange,
		Currency:        ev.Currency,
		LocalSymbol:     ev.LocalSymbol,
		TradingClass:    ev.TradingClass,
		Expiry:          ev.Expiry,
		Strike:          ev.Strike,
		Right:           ev.Right,
		Multiplier:      ev.Multiplier,
		Action:          ev.Action,
		OrderType:       ev.OrderType,
		TIF:             ev.TIF,
		TriggerMethod:   ev.TriggerMethod,
		OutsideRTH:      ev.OutsideRth,
		Quantity:        ev.TotalQuantity,
		LimitPrice:      ev.LimitPrice,
		Trail:           trailSpecFromLifecycle(ev),
		Status:          ev.Status,
		Filled:          ev.Filled,
		Remaining:       ev.Remaining,
		AvgFillPrice:    ev.AvgFillPrice,
		LastFillPrice:   ev.LastFillPrice,
		WhyHeld:         ev.WhyHeld,
		MktCapPrice:     ev.MktCapPrice,
		ExecID:          ev.ExecID,
		ExecTime:        ev.ExecTime,
		Message:         ev.Message,
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
		} else if brokerOrderStatusIsUncertainPending(ev.Status) {
			out.SendState = orderSendStateUncertainSend
		} else if ev.Status != "" {
			out.SendState = orderSendStateBrokerAcknowledged
		}
	case ibkrlib.OrderLifecycleEventExecDetails:
		out.Type = orderJournalEventStatusUpdated
		out.Status = "Execution"
		out.Filled = ev.CumQty
		out.LastFillPrice = ev.Price
		out.AvgFillPrice = ev.AvgFillPrice
		if out.Quantity > 0 && out.Filled >= out.Quantity-1e-9 {
			out.Status = "Filled"
			out.Remaining = 0
			out.SendState = orderSendStateTerminal
		}
	case ibkrlib.OrderLifecycleEventError:
		out.Type = orderJournalEventBrokerError
		if out.Status == "" {
			out.SendState = orderSendStateUncertainSend
		} else if orderLifecycleStatusIsTerminal(mapBrokerOrderLifecycleStatus(out.Status, out.Filled, out.Remaining)) {
			out.SendState = orderSendStateTerminal
		}
	default:
		return orderJournalEvent{}, false
	}
	return out, out.ReservedOrderID > 0 || out.OrderRef != "" || out.PermID > 0
}

func trailSpecFromLifecycle(ev ibkrlib.OrderLifecycleEvent) *rpc.OrderTrailSpec {
	switch strings.ToUpper(strings.TrimSpace(ev.OrderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
	default:
		return nil
	}
	trail := &rpc.OrderTrailSpec{
		Basis:            rpc.OrderTrailBasisInstrumentPrice,
		InitialStopPrice: ev.TrailStopPrice,
	}
	if ev.TrailingPercent > 0 {
		trail.OffsetType = rpc.OrderTrailOffsetPercent
		trail.TrailingPercent = cloneFloat64Ptr(&ev.TrailingPercent)
	} else if ev.AuxPrice > 0 {
		trail.OffsetType = rpc.OrderTrailOffsetAmount
		trail.TrailingAmount = cloneFloat64Ptr(&ev.AuxPrice)
	}
	if ev.LmtPriceOffset > 0 {
		trail.LimitOffset = cloneFloat64Ptr(&ev.LmtPriceOffset)
	}
	if trail.InitialStopPrice <= 0 && trail.TrailingPercent == nil && trail.TrailingAmount == nil && trail.LimitOffset == nil {
		return nil
	}
	return trail
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
	case orderJournalEventModifyRequested:
		return rpc.OrderLifecycleSubmitted
	case orderJournalEventCancelRequested:
		return rpc.OrderLifecyclePendingCancel
	case orderJournalEventBrokerError:
		if brokerErrorIsTerminalReject(ev.Message) {
			return rpc.OrderLifecycleRejected
		}
		return rpc.OrderLifecycleUnknownReconcileRequired
	case orderJournalEventReconciledUnknown:
		return rpc.OrderLifecycleUnknownReconcileRequired
	case orderJournalEventReconciledAbsent:
		return rpc.OrderLifecycleClosedReconciled
	default:
		if ev.SendState == orderSendStateUncertainSend {
			return rpc.OrderLifecycleUnknownReconcileRequired
		}
		return rpc.OrderLifecyclePreviewed
	}
}

func mapOrderViewLifecycleStatus(view rpc.OrderView) string {
	if view.LastEvent == orderJournalEventReconciledAbsent {
		// A complete broker open-order snapshot did not include this order:
		// like the error-135 heal below, broker truth overrides any sticky
		// earlier Status (a stale "PreSubmitted" must not resurrect the row).
		return rpc.OrderLifecycleClosedReconciled
	}
	if view.LastEvent == orderJournalEventBrokerError && brokerErrorProvesOrderGone(view.LastMessage) {
		// The broker answered a write aimed at this order ID with "can't
		// find order": whatever happened while the daemon was not
		// listening — fill, broker-side cancel, expiry — the order is not
		// working now, and that overrides any sticky earlier Status. This
		// is the only self-heal a stale GTC row has (GTC is deliberately
		// excluded from calendar expiry inference); without it the row
		// stays open forever and permanently blocks re-protecting the
		// symbol.
		return rpc.OrderLifecycleInactive
	}
	if view.LastEvent == orderJournalEventBrokerError && view.SendState == orderSendStateUncertainSend {
		if view.Status != "" {
			return rpc.OrderLifecycleUnknownReconcileRequired
		}
		if brokerErrorIsTerminalReject(view.LastMessage) {
			return rpc.OrderLifecycleRejected
		}
		return rpc.OrderLifecycleUnknownReconcileRequired
	}
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
	case orderJournalEventModifyRequested:
		return rpc.OrderLifecycleSubmitted
	case orderJournalEventCancelRequested:
		return rpc.OrderLifecyclePendingCancel
	case orderJournalEventBrokerError:
		return rpc.OrderLifecycleUnknownReconcileRequired
	case orderJournalEventReconciledUnknown:
		return rpc.OrderLifecycleUnknownReconcileRequired
	default:
		if view.SendState == orderSendStateUncertainSend {
			return rpc.OrderLifecycleUnknownReconcileRequired
		}
		return rpc.OrderLifecyclePreviewed
	}
}

// brokerErrorProvesOrderGone matches IBKR error 135 ("Can't find order
// with id …"), the broker's statement that the targeted order does not
// exist on its books. The journaled message keeps the raw broker text, so
// the fill-vs-cancelled ambiguity stays visible in the audit trail.
func brokerErrorProvesOrderGone(message string) bool {
	msg := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(msg, "broker error 135:") ||
		strings.Contains(msg, "can't find order")
}

func brokerErrorIsTerminalReject(message string) bool {
	msg := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(msg, "broker error 110:") ||
		strings.Contains(msg, "price does not conform to the minimum price variation") ||
		strings.Contains(msg, "duplicate order id") ||
		strings.Contains(msg, "reject")
}

func mapBrokerOrderLifecycleStatus(status string, filled, remaining float64) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pendingcancel":
		return rpc.OrderLifecyclePendingCancel
	case "pendingsubmit":
		return rpc.OrderLifecyclePendingSubmit
	case "presubmitted":
		return rpc.OrderLifecyclePreSubmitted
	case "apipending":
		return rpc.OrderLifecyclePendingSubmit
	case "submitted":
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

func brokerOrderStatusIsUncertainPending(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "ApiPending")
}

func orderLifecycleStatusIsTerminal(status string) bool {
	switch status {
	case rpc.OrderLifecycleFilled, rpc.OrderLifecycleCancelled, rpc.OrderLifecycleRejected, rpc.OrderLifecycleInactive, rpc.OrderLifecycleExpiredInferred, rpc.OrderLifecycleClosedReconciled:
		return true
	default:
		return false
	}
}

func orderViewIsOpen(view rpc.OrderView) bool {
	if view.SendState == orderSendStateTerminal {
		return false
	}
	if orderLifecycleStatusIsTerminal(view.LifecycleStatus) {
		return false
	}
	switch view.LastEvent {
	case orderJournalEventSendAttempted, orderJournalEventBrokerAcknowledged, orderJournalEventStatusUpdated, orderJournalEventModifyRequested, orderJournalEventCancelRequested, orderJournalEventBrokerError, orderJournalEventReconciledUnknown:
		return true
	default:
		return view.SendState == orderSendStateReserved ||
			view.SendState == orderSendStateSendAttempted ||
			view.SendState == orderSendStateBrokerAcknowledged ||
			view.SendState == orderSendStateUncertainSend
	}
}

func orderViewModifyEligible(view rpc.OrderView) bool {
	if !view.Open ||
		view.ReservedOrderID <= 0 ||
		!orderViewBrokerConfirmedForWrite(view) ||
		view.LifecycleStatus == rpc.OrderLifecyclePendingCancel ||
		!strings.EqualFold(view.SecType, "STK") {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(view.OrderType)) {
	case rpc.OrderTypeLMT:
		return strings.EqualFold(view.TIF, rpc.OrderTIFDay)
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		// Protective trails are amended in place (same broker order ID) so a
		// re-price never opens an unprotected cancel/replace window. Live
		// protection policy issues GTC trails, so GTC stays modify-eligible.
		return strings.EqualFold(view.TIF, rpc.OrderTIFDay) || strings.EqualFold(view.TIF, rpc.OrderTIFGTC)
	default:
		return false
	}
}

func orderViewCancelEligible(view rpc.OrderView) bool {
	return view.Open &&
		view.ReservedOrderID > 0 &&
		orderViewBrokerConfirmedForWrite(view) &&
		view.LifecycleStatus != rpc.OrderLifecyclePendingCancel
}

func orderViewBrokerConfirmedForWrite(view rpc.OrderView) bool {
	if view.SendState != orderSendStateBrokerAcknowledged {
		return false
	}
	switch view.LifecycleStatus {
	case rpc.OrderLifecycleSubmitted, rpc.OrderLifecyclePreSubmitted, rpc.OrderLifecyclePartiallyFilled:
		return true
	default:
		return false
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

func orderJournalCanonicalKey(ev orderJournalEvent, aliases map[string]string) string {
	return resolveOrderJournalAlias(orderJournalKey(ev), aliases)
}

func orderJournalKeyAliases(events []orderJournalEvent) map[string]string {
	aliases := map[string]string{}
	ambiguousOrderIDs := ambiguousReservedOrderIDs(events)
	prelinkedOrderIDs := reservedOrderIDsWithPrelinkedBrokerOnlyEvents(events)
	for _, ev := range events {
		keys := orderJournalIdentityKeysForAliases(ev, ambiguousOrderIDs, prelinkedOrderIDs)
		if len(keys) == 0 {
			continue
		}

		canonical := ""
		for _, key := range keys {
			if resolved := resolveOrderJournalAlias(key, aliases); resolved != "" {
				canonical = resolved
				break
			}
		}
		if ev.OrderRef != "" {
			canonical = "ref:" + ev.OrderRef
		}
		if canonical == "" {
			canonical = keys[0]
		}
		for _, key := range keys {
			aliases[key] = canonical
		}
		for key, resolved := range aliases {
			if resolved == canonical {
				continue
			}
			if slices.Contains(keys, resolved) {
				aliases[key] = canonical
			}
		}
	}
	for key := range aliases {
		aliases[key] = resolveOrderJournalAlias(key, aliases)
	}
	return aliases
}

func ambiguousReservedOrderIDs(events []orderJournalEvent) map[int]bool {
	refsByOrderID := map[int]map[string]bool{}
	for _, ev := range events {
		if ev.ReservedOrderID == 0 || ev.OrderRef == "" {
			continue
		}
		refs := refsByOrderID[ev.ReservedOrderID]
		if refs == nil {
			refs = map[string]bool{}
			refsByOrderID[ev.ReservedOrderID] = refs
		}
		refs[ev.OrderRef] = true
	}
	out := map[int]bool{}
	for orderID, refs := range refsByOrderID {
		if len(refs) > 1 {
			out[orderID] = true
		}
	}
	return out
}

func reservedOrderIDsWithPrelinkedBrokerOnlyEvents(events []orderJournalEvent) map[int]bool {
	firstRefIndex := map[int]int{}
	for i, ev := range events {
		if ev.ReservedOrderID == 0 || ev.OrderRef == "" {
			continue
		}
		if _, exists := firstRefIndex[ev.ReservedOrderID]; !exists {
			firstRefIndex[ev.ReservedOrderID] = i
		}
	}
	out := map[int]bool{}
	for i, ev := range events {
		if ev.ReservedOrderID == 0 || ev.OrderRef != "" {
			continue
		}
		refIndex, exists := firstRefIndex[ev.ReservedOrderID]
		if exists && i < refIndex {
			out[ev.ReservedOrderID] = true
		}
	}
	return out
}

func orderJournalIdentityKeysForAliases(ev orderJournalEvent, ambiguousOrderIDs, prelinkedOrderIDs map[int]bool) []string {
	keys := make([]string, 0, 3)
	if ev.OrderRef != "" {
		keys = append(keys, "ref:"+ev.OrderRef)
	}
	if ev.ReservedOrderID != 0 && !(ev.OrderRef != "" && (ambiguousOrderIDs[ev.ReservedOrderID] || prelinkedOrderIDs[ev.ReservedOrderID])) {
		keys = append(keys, "order:"+strconv.Itoa(ev.ReservedOrderID))
	}
	if ev.PermID != 0 {
		keys = append(keys, "perm:"+strconv.Itoa(ev.PermID))
	}
	return keys
}

func resolveOrderJournalAlias(key string, aliases map[string]string) string {
	if key == "" {
		return ""
	}
	seen := map[string]bool{}
	for {
		next := aliases[key]
		if next == "" || next == key || seen[key] {
			return key
		}
		seen[key] = true
		key = next
	}
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
