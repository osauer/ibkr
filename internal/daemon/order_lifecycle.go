package daemon

import (
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

func (s *Server) registerOrderLifecycleJournal(c *ibkrlib.Connector) {
	if s == nil || c == nil || s.orderJournal == nil {
		return
	}
	s.orderLifecycleHandlersMu.Lock()
	if s.orderLifecycleHandlers == nil {
		s.orderLifecycleHandlers = make(map[*ibkrlib.Connector]struct{})
	}
	if _, exists := s.orderLifecycleHandlers[c]; exists {
		s.orderLifecycleHandlersMu.Unlock()
		return
	}
	s.orderLifecycleHandlers[c] = struct{}{}
	s.orderLifecycleHandlersMu.Unlock()

	c.RegisterOrderLifecycleHandler(func(ev ibkrlib.OrderLifecycleEvent) {
		s.appendOrderLifecycleEvent(ev)
	})
}

func (s *Server) appendOrderLifecycleEvent(ev ibkrlib.OrderLifecycleEvent) {
	if s == nil || s.orderJournal == nil {
		return
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	journalEvent, ok := orderJournalEventFromLifecycle(ev, now)
	if !ok {
		return
	}
	matched, viewsLoaded := s.enrichOrderLifecycleEventFromJournal(&journalEvent)
	if viewsLoaded && !matched &&
		(ev.Type == ibkrlib.OrderLifecycleEventOpenOrder || ev.Type == ibkrlib.OrderLifecycleEventStatus) {
		// reqAllOpenOrders snapshots surface ALL of the account's open
		// orders, including manual TWS orders this journal has never
		// tracked. The journal's contract is daemon-placed orders only
		// ("may miss manual orders"), so unmatched openOrder/orderStatus
		// callbacks are observed, not adopted — journaling them would mint
		// phantom rows. Keyed on the broker event type: execDetails and
		// error events keep their existing pass-through behavior.
		s.debugf("order lifecycle: dropping unmatched %s callback for broker order %d (perm %d)", ev.Type, ev.OrderID, ev.PermID)
		return
	}
	normalizeOrderLifecycleJournalEvent(&journalEvent)

	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.tradingStatus(ep)
	if journalEvent.Account == "" {
		journalEvent.Account = status.Account
	}
	if journalEvent.Endpoint == "" {
		journalEvent.Endpoint = status.Endpoint
	}
	if journalEvent.Mode == "" {
		journalEvent.Mode = status.Mode
	}
	if journalEvent.ClientID == 0 {
		journalEvent.ClientID = status.ClientID
	}

	if err := s.orderJournal.Append(journalEvent); err != nil {
		s.warnf("append order lifecycle event: %v", err)
		return
	}
	if err := s.purgeLedger.ApplyOrderFill(journalEvent); err != nil {
		s.warnf("apply purge ledger fill: %v", err)
	}
	if journalEvent.Source == proposalOrderSource && s.proposalOutcomes != nil && journalEvent.Filled > 0 {
		var submitted proposalEvent
		if s.tradeProposals != nil && s.tradeProposals.store != nil {
			if ev, ok, err := s.tradeProposals.store.FindSubmittedEvent(journalEvent.OrderRef, journalEvent.PreviewTokenID); err != nil {
				s.warnf("lookup submitted proposal event: %v", err)
			} else if ok {
				submitted = ev
			}
		}
		if submitted.PolicyID == "" {
			s.warnf("skip proposal outcome fill: no submitted proposal event for order_ref=%s token_id=%s", journalEvent.OrderRef, journalEvent.PreviewTokenID)
			return
		}
		if err := s.proposalOutcomes.AppendMark(proposalOutcomeFilledFromJournal(journalEvent, submitted, now)); err != nil {
			s.warnf("append proposal outcome fill: %v", err)
		}
	}
}

func normalizeOrderLifecycleJournalEvent(ev *orderJournalEvent) {
	if ev == nil {
		return
	}
	if ev.Type != orderJournalEventStatusUpdated || !strings.EqualFold(ev.Status, "Execution") {
		return
	}
	if ev.Quantity > 0 && ev.Filled >= ev.Quantity-1e-9 {
		ev.Status = "Filled"
		ev.Remaining = 0
		ev.SendState = orderSendStateTerminal
		return
	}
	if ev.Remaining > 0 {
		ev.SendState = orderSendStateBrokerAcknowledged
	}
}

// enrichOrderLifecycleEventFromJournal copies identity from the matching
// journal view. matched reports whether an existing row claimed the event;
// viewsLoaded distinguishes "no match" from "journal unreadable" so callers
// can fail open on load errors instead of dropping broker evidence.
func (s *Server) enrichOrderLifecycleEventFromJournal(ev *orderJournalEvent) (matched, viewsLoaded bool) {
	if s == nil || ev == nil || s.orderJournal == nil {
		return false, false
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		return false, false
	}
	if view, ok := orderJournalViewForLifecycleEvent(*ev, views); ok {
		copyOrderJournalIdentityFromView(ev, view)
		return true, true
	}
	return false, true
}

func orderJournalViewForLifecycleEvent(ev orderJournalEvent, views []rpc.OrderView) (rpc.OrderView, bool) {
	if ev.OrderRef != "" {
		for _, view := range views {
			if view.OrderRef == ev.OrderRef {
				return view, true
			}
		}
	}
	if ev.PermID != 0 {
		for _, view := range views {
			if view.PermID == ev.PermID {
				return view, true
			}
		}
	}
	if ev.ReservedOrderID == 0 {
		return rpc.OrderView{}, false
	}
	matches := make([]rpc.OrderView, 0, 1)
	openMatches := make([]rpc.OrderView, 0, 1)
	for _, view := range views {
		if view.ReservedOrderID != ev.ReservedOrderID {
			continue
		}
		matches = append(matches, view)
		if view.Open {
			openMatches = append(openMatches, view)
		}
	}
	if len(openMatches) == 1 {
		return openMatches[0], true
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return rpc.OrderView{}, false
}

func copyOrderJournalIdentityFromView(ev *orderJournalEvent, view rpc.OrderView) {
	if ev.OrderRef == "" {
		ev.OrderRef = view.OrderRef
	}
	if ev.PreviewTokenID == "" {
		ev.PreviewTokenID = view.PreviewTokenID
	}
	if ev.Source == "" {
		ev.Source = view.Source
	}
	if ev.PurgeID == "" {
		ev.PurgeID = view.PurgeID
	}
	if ev.LegID == "" {
		ev.LegID = view.LegID
	}
	if view.BypassPreview {
		ev.BypassPreview = true
	}
	if ev.Account == "" {
		ev.Account = view.Account
	}
	if ev.Endpoint == "" {
		ev.Endpoint = view.Endpoint
	}
	if ev.Mode == "" {
		ev.Mode = view.Mode
	}
	if ev.ClientID == 0 {
		ev.ClientID = view.ClientID
	}
	if ev.ConID == 0 {
		ev.ConID = view.ConID
	}
	if ev.Symbol == "" {
		ev.Symbol = view.Symbol
	}
	if ev.SecType == "" {
		ev.SecType = view.SecType
	}
	if ev.Exchange == "" {
		ev.Exchange = view.Exchange
	}
	if ev.PrimaryExch == "" {
		ev.PrimaryExch = view.PrimaryExch
	}
	if ev.Currency == "" {
		ev.Currency = view.Currency
	}
	if ev.LocalSymbol == "" {
		ev.LocalSymbol = view.LocalSymbol
	}
	if ev.TradingClass == "" {
		ev.TradingClass = view.TradingClass
	}
	if ev.Expiry == "" {
		ev.Expiry = view.Expiry
	}
	if ev.Strike == 0 {
		ev.Strike = view.Strike
	}
	if ev.Right == "" {
		ev.Right = view.Right
	}
	if ev.Multiplier == 0 {
		ev.Multiplier = view.Multiplier
	}
	if ev.Action == "" {
		ev.Action = view.Action
	}
	if ev.OrderType == "" {
		ev.OrderType = view.OrderType
	}
	if ev.TIF == "" {
		ev.TIF = view.TIF
	}
	if ev.TriggerMethod == 0 {
		ev.TriggerMethod = view.TriggerMethod
	}
	if view.OutsideRTH {
		ev.OutsideRTH = true
	}
	if ev.Quantity == 0 {
		ev.Quantity = view.Quantity
	}
	if ev.LimitPrice == 0 {
		ev.LimitPrice = view.LimitPrice
	}
	if ev.Trail == nil {
		ev.Trail = cloneTrailSpec(view.Trail)
	}
	if ev.OpenClose == "" {
		ev.OpenClose = view.OpenClose
	}
}
