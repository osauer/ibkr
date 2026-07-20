package daemon

import (
	"context"
	"sync"
	"time"
)

// Indexed order reads (docs/design/history-index.md, phase 2 workstream D).
//
// The uniform safety rule: an indexed order read is served only when the
// history index is provably complete for the order journal at that
// instant — on-disk size equals the committed ingest watermark and no
// parse-marker rows exist; otherwise the byte-identical legacy journal
// scan runs. SQL only prunes; the existing Go predicates and folds decide.
// The journal scan remains the semantics-defining reference
// implementation, the fallback is automatic and disclosed, and nothing
// here touches broker-write behavior, submit eligibility, freeze, or
// journaling semantics.

// orderIndexTokenTimeout bounds the token-redemption index query so an
// in-flight backfill can only cause a fallback, never a stall.
const orderIndexTokenTimeout = 500 * time.Millisecond

// orderIndexWarnEvery rate-limits the per-surface fallback disclosures.
const orderIndexWarnEvery = time.Minute

type orderIndexWarnLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// warnOrderIndexFallback discloses one indexed-read fallback, rate-limited
// to one line per surface per minute so a cold-start backlog cannot flood
// the log.
func (s *Server) warnOrderIndexFallback(surface, cause string) {
	if s == nil {
		return
	}
	l := &s.orderIndexWarns
	l.mu.Lock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	now := time.Now()
	if now.Sub(l.last[surface]) < orderIndexWarnEvery {
		l.mu.Unlock()
		return
	}
	l.last[surface] = now
	l.mu.Unlock()
	s.logger.Warnf("order index: %s served from the journal scan (%s); the scan is the reference path and results are identical", surface, cause)
}

// installOrderIndexReads wires the order-journal store's index hooks:
// the append kick and the token-redemption index lookup. Called from
// installOrderJournalStore; both closures are nil-safe before the index
// opens (every serve falls back to the journal scan).
func (s *Server) installOrderIndexReads() {
	if s == nil || s.orderJournal == nil {
		return
	}
	s.orderJournal.onAppend = s.kickHistoryIndex
	s.orderJournal.tokenIndex = s.orderTokenIndexLookup
}

// orderTokenIndexLookup answers the token-redemption prior-event lookup
// from the index when — and only when — the index is provably fresh and
// clean. Runs under orderJournalStore.mu, so the journal cannot grow
// while the freshness proof and query execute.
func (s *Server) orderTokenIndexLookup(tokenID string) ([][]byte, bool) {
	store := s.historyIndex.Load()
	if store == nil {
		return nil, false
	}
	if !store.OrdersFresh() {
		s.warnOrderIndexFallback("token-redemption", "index not provably fresh")
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), orderIndexTokenTimeout)
	defer cancel()
	raws, err := store.OrderEventsForToken(ctx, tokenID)
	if err != nil {
		s.warnOrderIndexFallback("token-redemption", "index query failed: "+err.Error())
		return nil, false
	}
	return raws, true
}

// indexedOrderEvents serves the full decoded event list from order_events
// when the index is provably fresh and clean; ok=false means the caller
// must run the legacy journal scan. since/until, when non-nil, prune in
// SQL with the range widened by one millisecond at both ends — the exact
// Go range predicate is re-applied by the caller.
func (s *Server) indexedOrderEvents(surface string, sinceMS, untilMS *int64) ([]orderJournalEvent, bool) {
	if s == nil {
		return nil, false
	}
	store := s.historyIndex.Load()
	if store == nil {
		return nil, false
	}
	if !store.OrdersFresh() {
		s.warnOrderIndexFallback(surface, "index not provably fresh")
		return nil, false
	}
	raws, err := store.OrderEventLines(sinceMS, untilMS)
	if err != nil {
		s.warnOrderIndexFallback(surface, "index query failed: "+err.Error())
		return nil, false
	}
	events, ok := decodeOrderJournalRawLines(raws)
	if !ok {
		s.warnOrderIndexFallback(surface, "indexed line failed the daemon decode")
		return nil, false
	}
	return events, true
}

// loadOrderJournalEventsForRead returns the complete event list for the
// order-view fold: indexed when provably fresh, otherwise the unchanged
// LoadEvents journal scan.
func (s *Server) loadOrderJournalEventsForRead(surface string) ([]orderJournalEvent, error) {
	if events, ok := s.indexedOrderEvents(surface, nil, nil); ok {
		return events, nil
	}
	return s.orderJournal.LoadEvents(0)
}

// reservedBrokerOrderIDFloor is the order-ID floor read: MAX(reserved_order_id)
// from the index when provably fresh, else the unchanged full journal scan.
func (s *Server) reservedBrokerOrderIDFloor() (int, error) {
	if s == nil {
		return maxReservedBrokerOrderID(nil)
	}
	store := s.historyIndex.Load()
	switch {
	case store == nil:
	case !store.OrdersFresh():
		s.warnOrderIndexFallback("order-id-floor", "index not provably fresh")
	default:
		id, err := store.MaxReservedOrderID()
		if err == nil {
			return id, nil
		}
		s.warnOrderIndexFallback("order-id-floor", "index query failed: "+err.Error())
	}
	return maxReservedBrokerOrderID(s.orderJournal)
}
