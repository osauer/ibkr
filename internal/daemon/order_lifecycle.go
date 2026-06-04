package daemon

import (
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
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
	s.enrichOrderLifecycleEventFromJournal(&journalEvent)

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
}

func (s *Server) enrichOrderLifecycleEventFromJournal(ev *orderJournalEvent) {
	if s == nil || ev == nil || s.orderJournal == nil {
		return
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		return
	}
	for _, view := range views {
		if !orderJournalEventMatchesView(*ev, view) {
			continue
		}
		copyOrderJournalIdentityFromView(ev, view)
		return
	}
}

func orderJournalEventMatchesView(ev orderJournalEvent, view rpc.OrderView) bool {
	if ev.ReservedOrderID != 0 && view.ReservedOrderID == ev.ReservedOrderID {
		return true
	}
	if ev.OrderRef != "" && view.OrderRef == ev.OrderRef {
		return true
	}
	return ev.PermID != 0 && view.PermID == ev.PermID
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
	if view.OutsideRTH {
		ev.OutsideRTH = true
	}
	if ev.Quantity == 0 {
		ev.Quantity = view.Quantity
	}
	if ev.LimitPrice == 0 {
		ev.LimitPrice = view.LimitPrice
	}
	if ev.OpenClose == "" {
		ev.OpenClose = view.OpenClose
	}
}
