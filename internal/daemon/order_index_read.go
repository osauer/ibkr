package daemon

import (
	"context"
)

// installOrderIndexReads is intentionally empty. daemon.db is authoritative;
// history.db is neither a freshness oracle nor an order fallback.
func (s *Server) installOrderIndexReads() {}

// indexedOrderEvents now means an authoritative SQLite read. The bool reports
// whether the read completed; callers must not interpret false as permission
// to scan the sealed legacy journal.
func (s *Server) indexedOrderEvents(_ string, sinceMS, untilMS *int64) ([]orderJournalEvent, bool) {
	if s == nil || s.orderJournal == nil {
		return nil, false
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return nil, false
	}
	if sinceMS == nil && untilMS == nil {
		return events, true
	}
	out := make([]orderJournalEvent, 0, len(events))
	for _, ev := range events {
		at := ev.At.UnixMilli()
		if sinceMS != nil && at < *sinceMS {
			continue
		}
		if untilMS != nil && at > *untilMS {
			continue
		}
		out = append(out, ev)
	}
	return out, true
}

func (s *Server) loadOrderJournalEventsForRead(_ string) ([]orderJournalEvent, error) {
	if s == nil || s.orderJournal == nil {
		return nil, ErrTradingDisabled
	}
	return s.orderJournal.LoadEvents(0)
}

func (s *Server) reservedBrokerOrderIDFloor() (int, error) {
	if s == nil || s.orderJournal == nil {
		return 0, nil
	}
	store, err := s.orderJournal.coreStore()
	if err != nil {
		return 0, err
	}
	floor, err := store.GlobalOrderIDFloor(context.Background())
	return int(floor), err
}
