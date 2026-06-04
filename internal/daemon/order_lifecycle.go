package daemon

import (
	"time"

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
	}
}
