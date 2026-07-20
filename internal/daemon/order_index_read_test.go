package daemon

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// orderParityEventSet covers the fold's known-tricky shapes: alias merge
// (ref → reserved id → perm id), broker-error working-order preserve,
// purge_id rows, reconcile events, terminal fills, and out-of-scope rows.
func orderParityEventSet(base time.Time) []orderJournalEvent {
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }
	return []orderJournalEvent{
		{At: at(0), Type: orderJournalEventPreviewed, OrderRef: "par-1", PreviewTokenID: "par-tok-1", Account: "UTEST", Mode: "paper", Symbol: "AAA", SecType: "STK", Action: "SELL", Quantity: 5, SendState: orderSendStateReserved},
		{At: at(1), Type: orderJournalEventTokenConfirmed, OrderRef: "par-1", PreviewTokenID: "par-tok-1", Account: "UTEST", Mode: "paper"},
		{At: at(2), Type: orderJournalEventSendAttempted, OrderRef: "par-1", PreviewTokenID: "par-tok-1", ReservedOrderID: 700, Account: "UTEST", Mode: "paper", SendState: orderSendStateSendAttempted},
		{At: at(3), Type: orderJournalEventBrokerAcknowledged, ReservedOrderID: 700, PermID: 8801, Account: "UTEST", Mode: "paper", Status: "Submitted", SendState: orderSendStateBrokerAcknowledged},
		{At: at(4), Type: orderJournalEventModifyRequested, OrderRef: "par-1", ReservedOrderID: 700, Account: "UTEST", Mode: "paper"},
		{At: at(5), Type: orderJournalEventBrokerError, ReservedOrderID: 700, Account: "UTEST", Mode: "paper", SendState: orderSendStateTerminal, Message: "broker error 10147: modify rejected"},
		{At: at(6), Type: orderJournalEventPreviewed, OrderRef: "par-2", PreviewTokenID: "par-tok-2", Account: "UTEST", Mode: "paper", Symbol: "BBB", SecType: "STK", Action: "BUY", Quantity: 2, PurgeID: "purge-x"},
		{At: at(7), Type: orderJournalEventStatusUpdated, OrderRef: "par-2", Account: "UTEST", Mode: "paper", Status: "Filled", Filled: 2, SendState: orderSendStateTerminal, PurgeID: "purge-x"},
		{At: at(8), Type: orderJournalEventReconciledUnknown, OrderRef: "par-3", Account: "UTEST", Mode: "paper"},
		{At: at(9), Type: orderJournalEventReconciledAbsent, OrderRef: "par-3", Account: "UTEST", Mode: "paper"},
		{At: at(10), Type: orderJournalEventPreviewed, OrderRef: "par-other-scope", Account: "OTHER", Mode: "live", Symbol: "CCC"},
		{At: at(11), Type: orderJournalEventBrokerAcknowledged, PermID: 9902, Account: "UTEST", Mode: "paper", Status: "Submitted", SendState: orderSendStateBrokerAcknowledged},
	}
}

func waitOrdersFresh(t *testing.T, s *Server) {
	t.Helper()
	waitForHistory(t, func() (bool, error) {
		return s.historyIndex.Load().OrdersFresh(), nil
	})
}

// sortedViews normalizes fold output for deep comparison: the fold's own
// ordering is stable only per invocation for equal UpdatedAt values.
func sortedViews(views []rpc.OrderView) []rpc.OrderView {
	out := append([]rpc.OrderView(nil), views...)
	slices.SortFunc(out, func(a, b rpc.OrderView) int {
		return strings.Compare(orderViewKey(a), orderViewKey(b))
	})
	return out
}

// TestOrderDualReadParityGolden is the permanent dual-read gate over the
// deterministic tricky-shape journal: the indexed path and the journal
// scan must produce deep-equal events, folds, and history assemblies.
func TestOrderDualReadParityGolden(t *testing.T) {
	s := newHistoryIndexServer(t)
	base := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	if err := s.orderJournal.AppendAll(orderParityEventSet(base)); err != nil {
		t.Fatal(err)
	}
	waitOrdersFresh(t, s)

	scanEvents, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	idxEvents, ok := s.indexedOrderEvents("parity", nil, nil)
	if !ok {
		t.Fatal("index did not serve despite freshness")
	}
	if !reflect.DeepEqual(scanEvents, idxEvents) {
		t.Fatalf("decoded event lists diverge:\n scan %+v\n idx  %+v", scanEvents, idxEvents)
	}
	if !reflect.DeepEqual(sortedViews(buildOrderViews(scanEvents)), sortedViews(buildOrderViews(idxEvents))) {
		t.Fatal("buildOrderViews diverges between scan and index")
	}
	if !reflect.DeepEqual(buildOrderEventsByKey(scanEvents), buildOrderEventsByKey(idxEvents)) {
		t.Fatal("buildOrderEventsByKey diverges between scan and index")
	}

	// orders.history assembly through the REAL loader, index vs forced scan.
	scope := brokerStateScope{Account: "UTEST", Mode: "paper"}
	since := base.Add(-time.Hour)
	until := base.Add(time.Hour)
	idxScoped, err := s.loadScopedOrderHistoryEvents(since, until, scope)
	if err != nil {
		t.Fatal(err)
	}
	store := s.historyIndex.Load()
	s.historyIndex.Store(nil)
	scanScoped, err := s.loadScopedOrderHistoryEvents(since, until, scope)
	s.historyIndex.Store(store)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(idxScoped, scanScoped) {
		t.Fatalf("orders.history scoped events diverge:\n idx  %+v\n scan %+v", idxScoped, scanScoped)
	}
	if len(idxScoped) == 0 {
		t.Fatal("scoped window unexpectedly empty")
	}
	// Boundary semantics: a window that excludes everything matches on
	// both paths (the widened SQL range must not leak rows past the Go
	// predicates).
	idxNone, err := s.loadScopedOrderHistoryEvents(base.Add(-2*time.Hour), base.Add(-time.Hour), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(idxNone) != 0 {
		t.Fatalf("empty window served %d rows from the index", len(idxNone))
	}
	// Exact-boundary window: since inclusive, until exclusive.
	exact, err := s.loadScopedOrderHistoryEvents(base, base.Add(time.Second), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 1 || exact[0].Type != orderJournalEventPreviewed {
		t.Fatalf("boundary window = %+v, want exactly the first event", exact)
	}
}

// TestOrderDualReadParityRandomized is the property-style gate: random
// event permutations appended through the real writer must fold
// identically from both paths.
func TestOrderDualReadParityRandomized(t *testing.T) {
	s := newHistoryIndexServer(t)
	rng := rand.New(rand.NewSource(20260720))
	types := []string{
		orderJournalEventPreviewed, orderJournalEventTokenConfirmed, orderJournalEventSendAttempted,
		orderJournalEventSendError, orderJournalEventBrokerError, orderJournalEventBrokerAcknowledged,
		orderJournalEventStatusUpdated, orderJournalEventModifyRequested, orderJournalEventCancelRequested,
		orderJournalEventReconciledUnknown, orderJournalEventReconciledAbsent,
	}
	statuses := []string{"", "Submitted", "PreSubmitted", "Filled", "Cancelled", "Inactive", "ApiPending"}
	sendStates := []string{"", orderSendStateReserved, orderSendStateSendAttempted, orderSendStateBrokerAcknowledged, orderSendStateUncertainSend, orderSendStateTerminal}
	accounts := []string{"UTEST", "OTHER", ""}
	modes := []string{"paper", "live", ""}
	base := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	seq := 0

	for round := range 8 {
		batch := make([]orderJournalEvent, 0, 16)
		for range 16 {
			seq++
			ev := orderJournalEvent{
				// Distinct timestamps keep the fold's ordering total.
				At:        base.Add(time.Duration(seq) * time.Millisecond),
				Type:      types[rng.Intn(len(types))],
				Account:   accounts[rng.Intn(len(accounts))],
				Mode:      modes[rng.Intn(len(modes))],
				Status:    statuses[rng.Intn(len(statuses))],
				SendState: sendStates[rng.Intn(len(sendStates))],
			}
			if rng.Intn(2) == 0 {
				ev.OrderRef = fmt.Sprintf("rnd-%d", rng.Intn(6))
			}
			if rng.Intn(2) == 0 {
				ev.ReservedOrderID = 100 + rng.Intn(6)
			}
			if rng.Intn(3) == 0 {
				ev.PermID = 9000 + rng.Intn(4)
			}
			if rng.Intn(3) == 0 {
				ev.PreviewTokenID = fmt.Sprintf("rnd-tok-%d", rng.Intn(4))
			}
			if rng.Intn(4) == 0 {
				ev.PurgeID = "rnd-purge"
			}
			if rng.Intn(4) == 0 {
				ev.Filled = float64(rng.Intn(10))
				ev.Remaining = float64(rng.Intn(10))
			}
			batch = append(batch, ev)
		}
		if err := s.orderJournal.AppendAll(batch); err != nil {
			t.Fatal(err)
		}
		waitOrdersFresh(t, s)

		scanEvents, err := s.orderJournal.LoadEvents(0)
		if err != nil {
			t.Fatal(err)
		}
		idxEvents, ok := s.indexedOrderEvents("parity-rnd", nil, nil)
		if !ok {
			t.Fatalf("round %d: index did not serve", round)
		}
		if !reflect.DeepEqual(scanEvents, idxEvents) {
			t.Fatalf("round %d: decoded event lists diverge", round)
		}
		if !reflect.DeepEqual(sortedViews(buildOrderViews(scanEvents)), sortedViews(buildOrderViews(idxEvents))) {
			t.Fatalf("round %d: buildOrderViews diverges", round)
		}
		if !reflect.DeepEqual(buildOrderEventsByKey(scanEvents), buildOrderEventsByKey(idxEvents)) {
			t.Fatalf("round %d: buildOrderEventsByKey diverges", round)
		}
	}
}

// TestTokenRedemptionEquivalence pins byte-identical accept/reject
// behavior between the indexed check and the journal scan, plus the
// automatic fallbacks.
func TestTokenRedemptionEquivalence(t *testing.T) {
	s := newHistoryIndexServer(t)
	base := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	seed := []orderJournalEvent{
		{At: base, Type: orderJournalEventPreviewed, OrderRef: "tok-ord-1", PreviewTokenID: "tok-used", Account: "UTEST", Mode: "paper"},
		{At: base.Add(time.Second), Type: orderJournalEventSendAttempted, OrderRef: "tok-ord-1", PreviewTokenID: "tok-used", Account: "UTEST", Mode: "paper"},
		{At: base.Add(2 * time.Second), Type: orderJournalEventPreviewed, OrderRef: "tok-ord-2", PreviewTokenID: "tok-free", Account: "UTEST", Mode: "paper"},
		{At: base.Add(3 * time.Second), Type: orderJournalEventPreviewed, OrderRef: "tok-ord-3", PreviewTokenID: "tok-multi", Account: "UTEST", Mode: "paper"},
		{At: base.Add(4 * time.Second), Type: orderJournalEventPreviewed, OrderRef: "tok-ord-3", PreviewTokenID: "tok-multi", Account: "UTEST", Mode: "paper"},
	}
	if err := s.orderJournal.AppendAll(seed); err != nil {
		t.Fatal(err)
	}
	waitOrdersFresh(t, s)

	confirm := func(token string) error {
		return s.orderJournal.ConfirmPreviewTokenUseAndAppend(orderJournalEvent{
			At: time.Now().UTC(), Type: orderJournalEventTokenConfirmed,
			OrderRef: "tok-ord-x", PreviewTokenID: token, Account: "UTEST", Mode: "paper",
		})
	}

	// Consumed token: indexed decision (fresh index) vs journal scan must
	// be byte-identical rejections.
	errIdx := confirm("tok-used")
	if !errors.Is(errIdx, errOrderPreviewTokenAlreadyUsed) {
		t.Fatalf("indexed rejection = %v", errIdx)
	}
	store := s.historyIndex.Load()
	s.historyIndex.Store(nil)
	errScan := confirm("tok-used")
	s.historyIndex.Store(store)
	if !errors.Is(errScan, errOrderPreviewTokenAlreadyUsed) {
		t.Fatalf("scan rejection = %v", errScan)
	}
	if errIdx.Error() != errScan.Error() {
		t.Fatalf("rejection strings differ:\n idx  %q\n scan %q", errIdx.Error(), errScan.Error())
	}

	// Multi-event unconsumed token (two previews) accepts.
	if err := confirm("tok-multi"); err != nil {
		t.Fatalf("multi-preview token rejected: %v", err)
	}
	// Second redemption of the same token rejects — the confirming append
	// itself is the consumer, and the index is now stale, so this also
	// exercises the automatic stale fallback inside the check.
	if err := confirm("tok-multi"); !errors.Is(err, errOrderPreviewTokenAlreadyUsed) {
		t.Fatalf("second redemption = %v, want already-used", err)
	}

	// Unconsumed token accepts through the indexed path once fresh again.
	waitOrdersFresh(t, s)
	if err := confirm("tok-free"); err != nil {
		t.Fatalf("free token rejected: %v", err)
	}
}

// TestTokenRedemptionConcurrentSingleWinner runs racing redemptions of
// one token under -race: the existing mu serialization must yield exactly
// one winner regardless of which path answered the check.
func TestTokenRedemptionConcurrentSingleWinner(t *testing.T) {
	s := newHistoryIndexServer(t)
	if err := s.orderJournal.Append(orderJournalEvent{
		At: time.Now().UTC(), Type: orderJournalEventPreviewed,
		OrderRef: "race-ord", PreviewTokenID: "tok-race", Account: "UTEST", Mode: "paper",
	}); err != nil {
		t.Fatal(err)
	}
	waitOrdersFresh(t, s)

	const attempts = 8
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			errs[i] = s.orderJournal.ConfirmPreviewTokenUseAndAppend(orderJournalEvent{
				At: time.Now().UTC(), Type: orderJournalEventTokenConfirmed,
				OrderRef: "race-ord", PreviewTokenID: "tok-race", Account: "UTEST", Mode: "paper",
			})
		})
	}
	wg.Wait()
	winners := 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, errOrderPreviewTokenAlreadyUsed):
		default:
			t.Fatalf("unexpected redemption error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
}

// TestReservedOrderIDFloorIndexAndFallback pins index==scan for the
// order-ID floor and the automatic stale fallback.
func TestReservedOrderIDFloorIndexAndFallback(t *testing.T) {
	s := newHistoryIndexServer(t)
	if err := s.orderJournal.AppendAll([]orderJournalEvent{
		{At: time.Now().UTC(), Type: orderJournalEventSendAttempted, OrderRef: "floor-1", ReservedOrderID: 311, Account: "UTEST", Mode: "paper"},
		{At: time.Now().UTC(), Type: orderJournalEventSendAttempted, OrderRef: "floor-2", ReservedOrderID: 4200, Account: "UTEST", Mode: "paper"},
	}); err != nil {
		t.Fatal(err)
	}
	waitOrdersFresh(t, s)
	fromIndex, err := s.reservedBrokerOrderIDFloor()
	if err != nil {
		t.Fatal(err)
	}
	fromScan, err := maxReservedBrokerOrderID(s.orderJournal)
	if err != nil {
		t.Fatal(err)
	}
	if fromIndex != 4200 || fromIndex != fromScan {
		t.Fatalf("floor index=%d scan=%d, want equal 4200", fromIndex, fromScan)
	}

	// Stale index (direct file append, no kick): the floor must fall back
	// to the scan and see the newest reservation.
	appendOrderJournalLineDirect(t, s.orderJournal.Path,
		`{"version":1,"at":"2026-07-13T12:00:00Z","type":"send-attempted","order_ref":"floor-3","reserved_order_id":9999,"account":"UTEST","mode":"paper"}`)
	floor, err := s.reservedBrokerOrderIDFloor()
	if err != nil {
		t.Fatal(err)
	}
	if floor != 9999 {
		t.Fatalf("stale-fallback floor = %d, want the scan's 9999", floor)
	}
}

func TestReservedOrderIDFloorIgnoresNegativeIDs(t *testing.T) {
	s := newHistoryIndexServer(t)
	if err := s.orderJournal.Append(orderJournalEvent{
		At: time.Now().UTC(), Type: orderJournalEventSendAttempted,
		OrderRef: "negative-floor", ReservedOrderID: -7, Account: "UTEST", Mode: "paper",
	}); err != nil {
		t.Fatal(err)
	}
	waitOrdersFresh(t, s)

	fromIndex, err := s.reservedBrokerOrderIDFloor()
	if err != nil {
		t.Fatal(err)
	}
	store := s.historyIndex.Load()
	s.historyIndex.Store(nil)
	fromScan, scanErr := s.reservedBrokerOrderIDFloor()
	s.historyIndex.Store(store)
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	if fromIndex != 0 || fromScan != 0 {
		t.Fatalf("negative-only floor index=%d scan=%d, want 0/0", fromIndex, fromScan)
	}
}

func appendOrderJournalLineDirect(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOrderIndexFallbackDisclosure pins D10's disclosure: a stale index
// serves the identical journal result and warns once per surface per
// minute.
func TestOrderIndexFallbackDisclosure(t *testing.T) {
	s, sink := newHistoryIndexServerLogged(t, "warn")
	if err := s.orderJournal.Append(orderJournalEvent{
		At: time.Now().UTC(), Type: orderJournalEventPreviewed, OrderRef: "disc-1", Account: "UTEST", Mode: "paper",
	}); err != nil {
		t.Fatal(err)
	}
	waitOrdersFresh(t, s)
	// Make the index stale without a kick.
	appendOrderJournalLineDirect(t, s.orderJournal.Path,
		`{"version":1,"at":"2026-07-13T12:00:00Z","type":"previewed","order_ref":"disc-2","account":"UTEST","mode":"paper"}`)

	views, _, err := s.loadOrderViews()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range views {
		if v.OrderRef == "disc-2" {
			found = true
		}
	}
	if !found {
		t.Fatal("fallback did not serve the journal's newest row")
	}
	if _, _, err := s.loadOrderViews(); err != nil {
		t.Fatal(err)
	}
	warns := strings.Count(sink.String(), "orders.open served from the journal scan")
	if warns != 1 {
		t.Fatalf("fallback warns = %d, want exactly 1 (rate-limited)", warns)
	}
}

// TestOrderIndexParseMarkerFallsBackToScanFailure pins the parse_ok=0
// rule: the index refuses to serve and the legacy scan's loud failure is
// what the caller sees — identical to pre-index behavior.
func TestOrderIndexParseMarkerFallsBackToScanFailure(t *testing.T) {
	s := newHistoryIndexServer(t)
	appendOrderJournalLineDirect(t, s.orderJournal.Path, "this is not an order journal line")
	s.kickHistoryIndex()
	waitForHistory(t, func() (bool, error) {
		events, ok := s.indexedOrderEvents("marker", nil, nil)
		_ = events
		return !ok && !s.historyIndex.Load().OrdersFresh(), nil
	})
	if _, _, err := s.loadOrderViews(); err == nil || !strings.Contains(err.Error(), "parse order journal line") {
		t.Fatalf("marker journal load = %v, want the legacy scan failure", err)
	}
	if err := s.orderJournal.ConfirmPreviewTokenUseAndAppend(orderJournalEvent{
		Type: orderJournalEventTokenConfirmed, PreviewTokenID: "tok-marker",
	}); err == nil || !strings.Contains(err.Error(), "parse order journal line") {
		t.Fatalf("marker redemption = %v, want the legacy scan failure", err)
	}
}

func TestOrderIndexCanonicalDecodeFailuresDisableFastPath(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{
			name: "field omitted by index projection has wrong type",
			line: `{"version":1,"at":"2026-07-13T12:00:00Z","type":"previewed","quantity":"corrupt"}`,
		},
		{
			name: "bad timestamp",
			line: `{"version":1,"at":"not-a-timestamp","type":"previewed"}`,
		},
		{
			name: "larger than canonical scanner limit",
			line: `{"version":1,"at":"2026-07-13T12:00:00Z","type":"previewed","message":"` + strings.Repeat("x", maxFrameBytes) + `"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newHistoryIndexServer(t)
			appendOrderJournalLineDirect(t, s.orderJournal.Path, tt.line)
			s.kickHistoryIndex()
			waitForHistory(t, func() (bool, error) {
				h, err := s.historyIndex.Load().Health("orders")
				return err == nil && h.JournalBytes == h.IngestedBytes && !s.historyIndex.Load().OrdersFresh(), err
			})

			if _, _, err := s.loadOrderViews(); err == nil {
				t.Fatal("order read succeeded; want canonical journal-scan failure")
			}
			if _, err := s.reservedBrokerOrderIDFloor(); err == nil {
				t.Fatal("reserved-id floor succeeded; want canonical journal-scan failure")
			}
			if err := s.orderJournal.ConfirmPreviewTokenUseAndAppend(orderJournalEvent{
				Type: orderJournalEventTokenConfirmed, PreviewTokenID: "redacted-test-token",
			}); err == nil {
				t.Fatal("token redemption succeeded; want canonical journal-scan failure")
			}
		})
	}
}

func TestOrderIndexBlankLineIsConservativeFallback(t *testing.T) {
	s := newHistoryIndexServer(t)
	appendOrderJournalLineDirect(t, s.orderJournal.Path,
		`{"version":1,"at":"2026-07-13T12:00:00Z","type":"send-attempted","reserved_order_id":321}`)
	appendOrderJournalLineDirect(t, s.orderJournal.Path, "")
	s.kickHistoryIndex()
	waitForHistory(t, func() (bool, error) {
		h, err := s.historyIndex.Load().Health("orders")
		return err == nil && h.JournalBytes == h.IngestedBytes && !s.historyIndex.Load().OrdersFresh(), err
	})

	// The index refuses the file, but the unchanged reference scanner
	// ignores the blank line and still supplies the correct result.
	floor, err := s.reservedBrokerOrderIDFloor()
	if err != nil {
		t.Fatalf("blank-line fallback failed: %v", err)
	}
	if floor != 321 {
		t.Fatalf("blank-line fallback floor = %d, want 321", floor)
	}
}

func TestOrderIndexEqualSizeReplacementIsImmediatelyStale(t *testing.T) {
	s := newHistoryIndexServer(t)
	first := `{"version":1,"at":"2026-07-13T10:00:00Z","type":"previewed","order_ref":"same"}` + "\n"
	oldSecond := `{"version":1,"at":"2026-07-13T11:00:00Z","type":"send-attempted","reserved_order_id":111}` + "\n"
	newSecond := `{"version":1,"at":"2026-07-13T11:00:00Z","type":"send-attempted","reserved_order_id":999}` + "\n"
	if err := os.WriteFile(s.orderJournal.Path, []byte(first+oldSecond), 0o600); err != nil {
		t.Fatal(err)
	}
	s.kickHistoryIndex()
	waitOrdersFresh(t, s)
	original, err := os.Stat(s.orderJournal.Path)
	if err != nil {
		t.Fatal(err)
	}

	tmp := s.orderJournal.Path + ".replacement"
	if err := os.WriteFile(tmp, []byte(first+newSecond), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, s.orderJournal.Path); err != nil {
		t.Fatal(err)
	}
	if s.historyIndex.Load().OrdersFresh() {
		t.Fatal("same-size, same-mtime replacement reported fresh")
	}

	s.kickHistoryIndex()
	waitOrdersFresh(t, s)
	floor, err := s.reservedBrokerOrderIDFloor()
	if err != nil {
		t.Fatal(err)
	}
	if floor != 999 {
		t.Fatalf("rebuilt floor = %d, want 999", floor)
	}

	// Same inode, size, mtime, and first line: ctime is the remaining
	// generation boundary. Restoring mtime must not make an in-place rewrite
	// look fresh.
	validated, err := os.Stat(s.orderJournal.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.orderJournal.Path, []byte(first+oldSecond), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(s.orderJournal.Path, validated.ModTime(), validated.ModTime()); err != nil {
		t.Fatal(err)
	}
	if s.historyIndex.Load().OrdersFresh() {
		t.Fatal("same-inode, same-size, restored-mtime rewrite reported fresh")
	}
	s.kickHistoryIndex()
	waitOrdersFresh(t, s)
	floor, err = s.reservedBrokerOrderIDFloor()
	if err != nil {
		t.Fatal(err)
	}
	if floor != 111 {
		t.Fatalf("in-place rebuilt floor = %d, want 111", floor)
	}
}
