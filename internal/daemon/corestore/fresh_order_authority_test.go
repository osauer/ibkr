package corestore

import (
	"errors"
	"testing"
)

func TestInitializeFreshOrderAuthorityIsAtomicAndAllowsUnrelatedState(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
		ScopeKey: "daemon", Kind: "settings_v1", JSON: []byte(`{"version":1}`),
	}); err != nil {
		t.Fatalf("seed unrelated state: %v", err)
	}

	want := []byte(`{"kind":"ibkr.purge_ledger","schema_version":"purge-ledger-v2","updated_at":"1970-01-01T00:00:00Z","rows":[]}`)
	doc, err := store.InitializeFreshOrderAuthority(t.Context(), StateDocumentCAS{
		ScopeKey: "daemon", Kind: "purge_ledger_v2", JSON: want,
	})
	if err != nil {
		t.Fatalf("InitializeFreshOrderAuthority: %v", err)
	}
	if doc.Revision != 1 || string(doc.JSON) != string(want) {
		t.Fatalf("fresh state = revision %d %s", doc.Revision, doc.JSON)
	}
	for table, wantRows := range map[string]int{
		"legacy_imports":          1,
		"order_id_floors":         1,
		"order_events":            0,
		"consumed_preview_tokens": 0,
		"broker_scopes":           0,
	} {
		if got := countRows(t, store, table); got != wantRows {
			t.Fatalf("%s rows = %d, want %d", table, got, wantRows)
		}
	}
	floor, err := store.GlobalOrderIDFloor(t.Context())
	if err != nil || floor != 0 {
		t.Fatalf("global floor = %d, error = %v", floor, err)
	}
	if _, err := store.InitializeFreshOrderAuthority(t.Context(), StateDocumentCAS{
		ScopeKey: "daemon", Kind: "purge_ledger_v2", JSON: want,
	}); !errors.Is(err, ErrFreshAuthorityConflict) {
		t.Fatalf("second initialization error = %v, want ErrFreshAuthorityConflict", err)
	}
}

func TestInitializeFreshOrderAuthorityRejectsPartialStateWithoutMutation(t *testing.T) {
	t.Run("purge document", func(t *testing.T) {
		store, _ := openTestStore(t)
		if _, err := store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
			ScopeKey: "daemon", Kind: "purge_ledger_v2", JSON: []byte(`{"partial":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := store.InitializeFreshOrderAuthority(t.Context(), StateDocumentCAS{
			ScopeKey: "daemon", Kind: "purge_ledger_v2", JSON: []byte(`{"empty":true}`),
		})
		if !errors.Is(err, ErrFreshAuthorityConflict) {
			t.Fatalf("error = %v, want ErrFreshAuthorityConflict", err)
		}
		if countRows(t, store, "legacy_imports") != 0 || countRows(t, store, "order_id_floors") != 0 {
			t.Fatal("failed initialization partially wrote order authority")
		}
	})

	t.Run("order import marker", func(t *testing.T) {
		store, _ := openTestStore(t)
		if _, err := store.ImportLegacyOrderAuthority(t.Context(), LegacyOrderImport{SourceFingerprint: "legacy-empty"}); err != nil {
			t.Fatal(err)
		}
		_, err := store.InitializeFreshOrderAuthority(t.Context(), StateDocumentCAS{
			ScopeKey: "daemon", Kind: "purge_ledger_v2", JSON: []byte(`{"empty":true}`),
		})
		if !errors.Is(err, ErrFreshAuthorityConflict) {
			t.Fatalf("error = %v, want ErrFreshAuthorityConflict", err)
		}
		if _, ok, readErr := store.GetStateDocument(t.Context(), "daemon", "purge_ledger_v2"); readErr != nil || ok {
			t.Fatalf("failed initialization wrote purge state: found=%v error=%v", ok, readErr)
		}
	})
}
