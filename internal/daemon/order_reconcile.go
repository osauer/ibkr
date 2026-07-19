package daemon

import (
	"context"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

const (
	// orderReconcileGrace keeps the sweep away from rows with recent journal
	// activity so it never races an in-flight place, modify, or cancel.
	orderReconcileGrace = 10 * time.Minute
	// orderReconcileSnapshotWait bounds the reqAllOpenOrders round trip; on
	// expiry the snapshot is incomplete and the sweep proves nothing.
	orderReconcileSnapshotWait = 15 * time.Second
	// orderReconcileConnectDelay lets the gateway's own post-connect order
	// re-binds land (and refresh journal rows) before the first sweep.
	orderReconcileConnectDelay = 45 * time.Second
	// orderReconcileInterval paces the standing sweep while open rows exist.
	orderReconcileInterval = 30 * time.Minute
)

// reconcileOrderJournalWithBroker closes journal rows the broker no longer
// reports. IBKR does not replay missed terminal callbacks (a cancel that
// landed while the daemon was offline is gone for good), so absence from a
// COMPLETE open-order snapshot is the only broker-truth signal a stale row
// will ever get. The sweep is read-only at the broker: its single wire
// interaction is the snapshot request; rows are closed by appending terminal
// reconciled-absent events to the append-only journal, never by rewriting.
//
// Fail-safe rules: no complete snapshot → no action; only rows in the
// current concrete account/mode scope; only rows the broker once accepted
// (PermID set); never rows with journal activity inside the grace window.
// Rows still present in the snapshot are refreshed for free — their
// openOrder callbacks flow through the normal lifecycle journal handler,
// which also restores send_state on rows wedged by a missed cancel ack.
func (s *Server) reconcileOrderJournalWithBroker(ctx context.Context) {
	if s == nil || s.orderJournal == nil {
		return
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		// An unfiltered scope matches every journal row; sweeping against a
		// single connection's snapshot would close other accounts' rows.
		return
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		s.warnf("order reconcile: load order views: %v", err)
		return
	}
	now := s.orderNow()
	var candidates []rpc.OrderView
	for _, v := range views {
		if !v.Open || v.PermID == 0 || !orderViewMatchesBrokerScope(v, scope) {
			continue
		}
		if v.UpdatedAt.IsZero() || now.Sub(v.UpdatedAt) < orderReconcileGrace {
			continue
		}
		candidates = append(candidates, v)
	}
	if len(candidates) == 0 {
		return
	}

	snap, err := s.snapshotOpenOrders(ctx)
	if err != nil && !snap.Complete {
		s.warnf("order reconcile: open-order snapshot unavailable: %v", err)
		return
	}
	if !snap.Complete {
		s.warnf("order reconcile: open-order snapshot incomplete; skipping sweep")
		return
	}

	var events []orderJournalEvent
	for _, v := range candidates {
		if openOrderSnapshotContains(snap, v) {
			continue
		}
		ev := orderJournalEventFromView(v, orderJournalEventReconciledAbsent, s.orderNow())
		// orderJournalEventFromView copies neither PermID nor a clean Status:
		// carry the PermID as the row's stable identity key, and carry no
		// broker Status so the sticky earlier Status stays visibly last-known
		// while the reconciled-absent lifecycle branch closes the row.
		ev.PermID = v.PermID
		ev.Status = ""
		ev.SendState = orderSendStateTerminal
		ev.Message = "broker open-order snapshot did not include this order; closed by reconciliation. Final broker state unknown (cancelled or filled outside daemon view) — confirm against broker statements."
		events = append(events, ev)
	}
	if len(events) == 0 {
		return
	}
	if err := s.orderJournal.AppendAll(events); err != nil {
		s.warnf("order reconcile: append reconciled-absent events: %v", err)
		return
	}
	s.infof("order reconcile: closed %d journal row(s) absent from a complete broker open-order snapshot (%d snapshot orders, %d candidates)", len(events), len(snap.Orders), len(candidates))
}

// snapshotOpenOrders resolves the snapshot source: the orderSnapshotFn test
// seam when set, else the live connector.
func (s *Server) snapshotOpenOrders(ctx context.Context) (ibkrlib.OpenOrderSnapshot, error) {
	if s.orderSnapshotFn != nil {
		return s.orderSnapshotFn(ctx)
	}
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	if c == nil || !c.IsConnected() {
		return ibkrlib.OpenOrderSnapshot{}, ibkrlib.ErrIBKRUnavailable
	}
	snapCtx, cancel := context.WithTimeout(ctx, orderReconcileSnapshotWait)
	defer cancel()
	return c.SnapshotOpenOrders(snapCtx)
}

// openOrderSnapshotContains reports whether the snapshot still carries the
// journal row's broker order: primary key PermID, fallback broker order id
// (with client agreement when both sides know their client).
func openOrderSnapshotContains(snap ibkrlib.OpenOrderSnapshot, view rpc.OrderView) bool {
	for _, o := range snap.Orders {
		if view.PermID != 0 && o.PermID != 0 && o.PermID == view.PermID {
			return true
		}
		if view.ReservedOrderID > 0 && o.OrderID == view.ReservedOrderID {
			if o.ClientID == 0 || view.ClientID == 0 || o.ClientID == view.ClientID {
				return true
			}
		}
	}
	return false
}

// runOrderReconcileLoop is the standing sweep, launched once per daemon
// lifetime from postConnectSetup. The per-connect settle-delay one-shots are
// scheduled separately on every successful (re)connect.
func (s *Server) runOrderReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(orderReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileOrderJournalWithBroker(ctx)
		}
	}
}
