package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrderJournalDefaultPathUsesXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/ibkr-state")

	got, err := defaultOrderJournalPath()
	if err != nil {
		t.Fatalf("defaultOrderJournalPath: %v", err)
	}
	want := filepath.Join("/tmp/ibkr-state", "ibkr", "order-journal.jsonl")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestOrderJournalAppendWritesPrivateJSONL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "order-journal.jsonl")
	store := newOrderJournalStore(path)
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)

	if err := store.Append(orderJournalEvent{
		At:              now,
		Type:            orderJournalEventSendAttempted,
		OrderRef:        "ibkr-20260528-test",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Symbol:          "AAPL",
		SecType:         "STK",
		Action:          "BUY",
		OrderType:       "LMT",
		TIF:             "DAY",
		Quantity:        1,
		LimitPrice:      100,
		SendState:       orderSendStateSendAttempted,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("journal lines = %d, want 1: %q", len(lines), data)
	}

	events, err := store.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 1 || events[0].OrderRef != "ibkr-20260528-test" {
		t.Fatalf("events = %+v", events)
	}
}

func TestOrderJournalSummaryCountsNonTerminalLatestState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "order-journal.jsonl")
	store := newOrderJournalStore(path)
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)

	events := []orderJournalEvent{
		{At: now, Type: orderJournalEventSendAttempted, OrderRef: "open", SendState: orderSendStateSendAttempted},
		{At: now.Add(time.Minute), Type: orderJournalEventStatusUpdated, OrderRef: "closed", SendState: orderSendStateSendAttempted},
		{At: now.Add(2 * time.Minute), Type: orderJournalEventStatusUpdated, OrderRef: "closed", SendState: orderSendStateTerminal},
	}
	for _, ev := range events {
		if err := store.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	summary, err := store.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.OpenOrders != 1 {
		t.Fatalf("OpenOrders = %d, want 1", summary.OpenOrders)
	}
	if !strings.Contains(summary.LastEvent, "closed") {
		t.Fatalf("LastEvent = %q, want closed order ref", summary.LastEvent)
	}
}
