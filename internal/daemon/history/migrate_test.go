package history

import (
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"testing"
)

// v1BaseDDL is the phase-1 ingest_sources CREATE, verbatim, so the
// migration test exercises a real v1 file rather than whatever the
// current code would produce.
const v1BaseDDL = `CREATE TABLE ingest_sources (
  source     TEXT PRIMARY KEY,
  path       TEXT NOT NULL,
  offset     INTEGER NOT NULL DEFAULT 0,
  genesis    TEXT,
  updated_at TEXT
)`

// buildV1Fixture creates a phase-1 database at path: v1 DDL (the regime
// and rules DDL are unchanged between v1 and v2, so the shared builders
// are the verbatim source), user_version 1, and seeded phase-1 rows.
func buildV1Fixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{v1BaseDDL}
	stmts = append(stmts, regimeDDL()...)
	stmts = append(stmts, rulesDDL()...)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("apply v1 DDL: %v", err)
		}
	}
	seeds := []string{
		`INSERT INTO ingest_sources (source, path, offset, genesis, updated_at) VALUES ('regime', '/j/regime.jsonl', 120, 'aa', '2026-07-01T00:00:00Z')`,
		`INSERT INTO regime_decisions (src_offset, at, at_unix_ms, stage, raw_json) VALUES (0, '2026-07-01T00:00:00Z', 1, 'calm', '{"v":1}')`,
		`INSERT INTO regime_indicators (decision_id, indicator, band) VALUES (1, 'vix_term', 'green')`,
		`INSERT INTO rule_transitions (src_offset, at, at_unix_ms, rule_id, status, raw_json) VALUES (0, '2026-07-01T00:00:00Z', 1, 'r1', 'pass', '{"version":1}')`,
		`PRAGMA user_version = 1`,
	}
	for _, stmt := range seeds {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed v1: %v", err)
		}
	}
}

// schemaShape captures the comparable schema identity: the sqlite_master
// name set plus PRAGMA table_info per table.
func schemaShape(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	shape := map[string][]string{}
	rows, err := s.db.Query(`SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatal(err)
		}
		shape["master"] = append(shape["master"], typ+":"+name)
		if typ == "table" {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, table := range tables {
		info, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err != nil {
			t.Fatal(err)
		}
		for info.Next() {
			var cid int
			var name, ctype string
			var notNull, pk int
			var dflt sql.NullString
			if err := info.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			shape[table] = append(shape[table], fmt.Sprintf("%d|%s|%s|%d|%s|%d", cid, name, ctype, notNull, dflt.String, pk))
		}
		if err := info.Err(); err != nil {
			t.Fatal(err)
		}
		info.Close()
	}
	return shape
}

// TestMigrateV1ToV2SchemaEquality is the binding D2 test: a migrated v1
// file and a fresh v2 file have identical sqlite_master name sets and
// identical per-table column layouts, phase-1 rows survive, and base is 0.
func TestMigrateV1ToV2SchemaEquality(t *testing.T) {
	t.Parallel()
	migratedOpts := testOptions(t)
	buildV1Fixture(t, migratedOpts.DBPath)
	migrated := openTestStore(t, migratedOpts)
	if got := userVersion(t, migrated); got != 2 {
		t.Fatalf("migrated user_version = %d, want 2", got)
	}

	fresh := openTestStore(t, testOptions(t))
	freshShape := schemaShape(t, fresh)
	migratedShape := schemaShape(t, migrated)
	for _, key := range slices.Sorted(maps.Keys(freshShape)) {
		if !slices.Equal(freshShape[key], migratedShape[key]) {
			t.Errorf("schema %s differs:\n fresh    %v\n migrated %v", key, freshShape[key], migratedShape[key])
		}
	}
	for _, key := range slices.Sorted(maps.Keys(migratedShape)) {
		if _, ok := freshShape[key]; !ok {
			t.Errorf("migrated has extra schema object %s", key)
		}
	}

	// Phase-1 rows intact, base defaulted to 0 (logical ≡ physical).
	var offset, base int64
	var genesis string
	if err := migrated.db.QueryRow(`SELECT offset, base, genesis FROM ingest_sources WHERE source = 'regime'`).Scan(&offset, &base, &genesis); err != nil {
		t.Fatalf("read migrated bookkeeping: %v", err)
	}
	if offset != 120 || base != 0 || genesis != "aa" {
		t.Fatalf("migrated bookkeeping = offset %d base %d genesis %q, want 120/0/aa", offset, base, genesis)
	}
	var stage string
	if err := migrated.db.QueryRow(`SELECT stage FROM regime_decisions WHERE src_offset = 0`).Scan(&stage); err != nil || stage != "calm" {
		t.Fatalf("phase-1 decision row did not survive: %q %v", stage, err)
	}
	var transitions int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM rule_transitions`).Scan(&transitions); err != nil || transitions != 1 {
		t.Fatalf("phase-1 transition rows = %d (%v), want 1", transitions, err)
	}
	// The new evidence tables exist and are empty.
	for _, table := range []string{"capital_events", "risk_policy_events", "proposal_outcomes", "canary_transitions", "order_events", "rotation_log", "archive_files", "statement_files", "statement_equity_days"} {
		var n int
		if err := migrated.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Errorf("migrated table %s missing: %v", table, err)
		} else if n != 0 {
			t.Errorf("migrated table %s has %d rows, want 0", table, n)
		}
	}
}

// TestMigrateV1RowsIngestContinues proves a migrated file keeps its
// idempotency: the stored offset still governs, so re-ingest of a journal
// the v1 index already covered adds nothing.
func TestMigrateV1RowsIngestContinues(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	line := `{"v":1,"ts":"2026-07-01T00:00:00Z","stage":"calm"}` + "\n"
	writeJournal(t, opts.RegimeJournalPath, line)
	buildV1Fixture(t, opts.DBPath)
	// Point the fixture's bookkeeping at the real journal, fully ingested.
	db, err := sql.Open("sqlite", "file:"+opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Clean(opts.RegimeJournalPath)
	hash := lineHash([]byte(`{"v":1,"ts":"2026-07-01T00:00:00Z","stage":"calm"}`))
	if _, err := db.Exec(`UPDATE ingest_sources SET path = ?, offset = ?, genesis = ? WHERE source = 'regime'`, f, len(line), hash); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s := openTestStore(t, opts)
	s.ingestAll(t.Context())
	if got := countRows(t, s, "regime_decisions"); got != 1 {
		t.Fatalf("regime rows after migrated re-ingest = %d, want 1 (offset governs)", got)
	}
}
