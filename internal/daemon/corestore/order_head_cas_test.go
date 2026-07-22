package corestore

import (
	"errors"
	"testing"
)

func TestAppendOrderEventsAtHeadRejectsChangedJournal(t *testing.T) {
	store, _ := openTestStore(t)
	scope := testScope("reconcile-cas")
	if _, err := store.AppendOrderEvents(t.Context(), []OrderEventRecord{orderEvent(scope, "first", "", 101)}); err != nil {
		t.Fatal(err)
	}
	captured, err := store.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOrderEvents(t.Context(), []OrderEventRecord{orderEvent(scope, "intervening", "", 102)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOrderEventsAtHead(t.Context(), captured.LastEventSeq, []OrderEventRecord{orderEvent(scope, "stale-reconcile", "", 103)}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale reconciliation CAS error=%v, want revision conflict", err)
	}
	records, err := store.LoadOrderEvents(t.Context(), OrderQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("stale reconciliation appended rows: %d", len(records))
	}
	current, err := store.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOrderEventsAtHead(t.Context(), current.LastEventSeq, []OrderEventRecord{orderEvent(scope, "current-reconcile", "", 104)}); err != nil {
		t.Fatalf("current reconciliation CAS failed: %v", err)
	}
}
