package corestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestSchemaObjectManifestRejectsTampering(t *testing.T) {
	mutations := []struct {
		name string
		run  func(*testing.T, *sql.DB)
	}{
		{
			name: "missing append-only trigger",
			run: func(t *testing.T, db *sql.DB) {
				t.Helper()
				if _, err := db.Exec(`DROP TRIGGER event_log_no_update`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "changed index definition",
			run: func(t *testing.T, db *sql.DB) {
				t.Helper()
				if _, err := db.Exec(`DROP INDEX observations_scope_time`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`CREATE INDEX observations_scope_time ON observations(scope_key, source, kind, observed_at_ms, observation_id)`); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	checks := []struct {
		name string
		run  func(context.Context, string, AuthorityHead) error
	}{
		{
			name: "open",
			run: func(ctx context.Context, path string, head AuthorityHead) error {
				store, err := Open(ctx, Options{Path: path, MinimumHead: &head})
				if store != nil {
					_ = store.Close()
				}
				return err
			},
		},
		{
			name: "backup verification",
			run: func(ctx context.Context, path string, head AuthorityHead) error {
				_, err := VerifyBackup(ctx, path, head)
				return err
			},
		},
	}

	for _, mutation := range mutations {
		for _, check := range checks {
			t.Run(mutation.name+"/"+check.name, func(t *testing.T) {
				path, head := createClosedIntegrityFixture(t, "")
				db := rawDB(t, path)
				mutation.run(t, db)
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				err := check.run(t.Context(), path, head)
				if err == nil || !strings.Contains(err.Error(), "schema object manifest mismatch") {
					t.Fatalf("tampered schema error=%v", err)
				}
			})
		}
	}
}

func TestApplicationContentHashesRejectValidPayloadTampering(t *testing.T) {
	cases := []struct {
		name       string
		table      string
		column     string
		newJSON    string
		appendOnly bool
	}{
		{name: "state document", table: "state_documents", column: "document_json", newJSON: `{"tampered":"state"}`},
		{name: "event", table: "event_log", column: "payload_json", newJSON: `{"tampered":"event"}`, appendOnly: true},
		{name: "observation", table: "observations", column: "payload", newJSON: `{"tampered":"observation"}`, appendOnly: true},
		{name: "statement version", table: "statement_equity_day_versions", column: "raw_json", newJSON: `{"tampered":"statement"}`, appendOnly: true},
	}
	checks := []struct {
		name string
		run  func(context.Context, string, AuthorityHead) error
	}{
		{
			name: "open",
			run: func(ctx context.Context, path string, head AuthorityHead) error {
				store, err := Open(ctx, Options{Path: path, MinimumHead: &head})
				if store != nil {
					_ = store.Close()
				}
				return err
			},
		},
		{
			name: "backup verification",
			run: func(ctx context.Context, path string, head AuthorityHead) error {
				_, err := VerifyBackup(ctx, path, head)
				return err
			},
		},
	}

	for _, tc := range cases {
		for _, check := range checks {
			t.Run(tc.name+"/"+check.name, func(t *testing.T) {
				path, head := createClosedIntegrityFixture(t, tc.name)
				db := rawDB(t, path)
				tamperStoredPayload(t, db, tc.table, tc.column, tc.newJSON, tc.appendOnly)
				if err := validateSchemaObjects(t.Context(), db, len(migrations)); err != nil {
					t.Fatalf("payload tamper changed schema: %v", err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				err := check.run(t.Context(), path, head)
				if err == nil || !strings.Contains(err.Error(), "application content hash mismatch") {
					t.Fatalf("stale application digest error=%v", err)
				}
			})
		}
		if tc.name == "state document" {
			t.Run(tc.name+"/read", func(t *testing.T) {
				path, _ := createClosedIntegrityFixture(t, tc.name)
				store, err := Open(t.Context(), Options{Path: path})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				tamperStoredPayload(t, store.db, tc.table, tc.column, tc.newJSON, false)
				if _, _, err := store.GetStateDocument(t.Context(), "safety", "guardrails"); err == nil || !strings.Contains(err.Error(), "stored state document digest") {
					t.Fatalf("state read digest error=%v", err)
				}
			})
		}

		t.Run(tc.name+"/direct integrity check", func(t *testing.T) {
			path, _ := createClosedIntegrityFixture(t, tc.name)
			store, err := Open(t.Context(), Options{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			tamperStoredPayload(t, store.db, tc.table, tc.column, tc.newJSON, tc.appendOnly)
			if _, err := store.CheckIntegrity(t.Context()); err == nil || !strings.Contains(err.Error(), "application content hash mismatch") {
				t.Fatalf("direct integrity error=%v", err)
			}
		})
	}
}

func createClosedIntegrityFixture(t *testing.T, contentKind string) (string, AuthorityHead) {
	t.Helper()
	path := privateTempDir(t) + "/daemon.db"
	store, err := Open(t.Context(), Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	switch contentKind {
	case "":
	case "state document":
		_, err = store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
			ScopeKey: "safety", Kind: "guardrails", JSON: []byte(`{"freeze":true,"capital":"100.00"}`),
		})
	case "event":
		_, err = store.AppendEvents(t.Context(), []EventInput{{
			ScopeKey: "market", EventKey: "integrity-event", Type: "test.event", Action: "observe", Origin: "daemon",
			OccurredAt: time.Unix(1_700_000_000, 0).UTC(), PayloadJSON: []byte(`{"original":"event"}`),
		}})
	case "observation":
		_, err = store.AppendObservation(t.Context(), ObservationInput{
			ScopeKey: "market", Source: "test", Kind: "integrity", ObservedAt: time.Unix(1_700_000_000, 0).UTC(),
			ContentType: "application/json", Payload: []byte(`{"original":"observation"}`),
		})
	case "statement version":
		fileDigest := sha256.Sum256([]byte("statement-evidence"))
		generatedAt := time.Unix(1_700_000_000, 0).UTC()
		err = store.ReplaceStatementProjection(t.Context(), "statements", []StatementFileRecord{{
			FileKey: "statement.xml", SizeBytes: 18, SHA256: fileDigest, Status: "ingested", StatementGeneratedAt: &generatedAt,
		}}, []StatementEquityDayRecord{{
			AccountKey: "account", Day: "2026-07-20", EquityBaseText: "100.00", StatementFileKey: "statement.xml",
			GeneratedAt: generatedAt, RawJSON: []byte(`{"original":"statement"}`),
		}})
	default:
		t.Fatalf("unsupported integrity fixture %q", contentKind)
	}
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	head, err := store.AuthorityHead(t.Context())
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path, head
}

func tamperStoredPayload(t *testing.T, db *sql.DB, table, column, replacement string, appendOnly bool) {
	t.Helper()
	trigger := table + "_no_update"
	if appendOnly {
		if _, err := db.Exec(`DROP TRIGGER ` + trigger); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE `+table+` SET `+column+`=?`, []byte(replacement)); err != nil {
		t.Fatal(err)
	}
	if appendOnly {
		if _, err := db.Exec(`CREATE TRIGGER ` + trigger + ` BEFORE UPDATE ON ` + table + ` BEGIN SELECT RAISE(ABORT, '` + table + ` is append-only'); END`); err != nil {
			t.Fatal(err)
		}
	}
}
