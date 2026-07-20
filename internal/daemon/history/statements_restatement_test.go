package history

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func equityDay(t *testing.T, s *Store, day string) (float64, string) {
	t.Helper()
	var equity float64
	var source string
	if err := s.db.QueryRow(`SELECT equity_base, source_stmt FROM statement_equity_days WHERE day = ?`, day).
		Scan(&equity, &source); err != nil {
		t.Fatalf("read equity day %s: %v", day, err)
	}
	return equity, source
}

func requireNoEquityDay(t *testing.T, s *Store, day string) {
	t.Helper()
	var equity float64
	err := s.db.QueryRow(`SELECT equity_base FROM statement_equity_days WHERE day = ?`, day).Scan(&equity)
	if err != sql.ErrNoRows {
		t.Fatalf("equity day %s err = %v, want sql.ErrNoRows (value %v)", day, err, equity)
	}
}

func TestStatementChangedFileRetractsRemovedDay(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	const name = "flex-20260713-063000.xml"
	writeStatement(t, opts.StatementsDir, name, stmtFixtureA)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	changed := strings.Replace(stmtFixtureA,
		`    <EquitySummaryByReportDateInBase reportDate="20260709" total="259100.10" />`+"\n", "", 1)
	writeStatement(t, opts.StatementsDir, name, changed)
	s.ingestAll(context.Background())

	if got := countRows(t, s, "statement_equity_days"); got != 1 {
		t.Fatalf("statement_equity_days rows = %d, want 1 after correction removed a day", got)
	}
	requireNoEquityDay(t, s, "2026-07-09")
}

func TestStatementSameGenerationSameSizeCorrectionWins(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	const name = "flex-20260713-063000.xml"
	writeStatement(t, opts.StatementsDir, name, stmtFixtureA)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	corrected := strings.Replace(stmtFixtureA, "261234.56", "261111.11", 1)
	if len(corrected) != len(stmtFixtureA) {
		t.Fatal("fixture correction must preserve file size")
	}
	writeStatement(t, opts.StatementsDir, name, corrected)
	s.ingestAll(context.Background())

	equity, source := equityDay(t, s, "2026-07-08")
	if equity != 261111.11 || source != name {
		t.Fatalf("same-generation correction = %v from %q, want 261111.11 from %q", equity, source, name)
	}
}

func TestStatementRebuildNewestGenerationWinsAndOlderCannotOverwrite(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	older := strings.Replace(stmtFixtureA, "259100.10", "111111.11", 1)
	newer := strings.Replace(stmtFixtureB, "259999.99", "222222.22", 1)
	writeStatement(t, opts.StatementsDir, "flex-20260713-063000.xml", older)
	writeStatement(t, opts.StatementsDir, "flex-20260714-063000.xml", newer)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	equity, source := equityDay(t, s, "2026-07-09")
	if equity != 222222.22 || source != "flex-20260714-063000.xml" {
		t.Fatalf("newest statement = %v from %q, want newer generation", equity, source)
	}

	// Force a rebuild by adding an older statement. Recomputing the whole set
	// must not let the newly observed but older generation replace the winner.
	oldest := strings.ReplaceAll(stmtFixtureB, "20260714;063000", "20260712;063000")
	oldest = strings.Replace(oldest, "259999.99", "333333.33", 1)
	writeStatement(t, opts.StatementsDir, "flex-20260712-063000.xml", oldest)
	s.ingestAll(context.Background())

	equity, source = equityDay(t, s, "2026-07-09")
	if equity != 222222.22 || source != "flex-20260714-063000.xml" {
		t.Fatalf("older statement replaced winner: %v from %q", equity, source)
	}
}

func TestStatementRemovedFileRebuildsCurrentSet(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	const olderName = "flex-20260713-063000.xml"
	const newerName = "flex-20260714-063000.xml"
	writeStatement(t, opts.StatementsDir, olderName, stmtFixtureA)
	writeStatement(t, opts.StatementsDir, newerName, stmtFixtureB)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	if err := os.Remove(filepath.Join(opts.StatementsDir, newerName)); err != nil {
		t.Fatal(err)
	}
	s.ingestAll(context.Background())

	equity, source := equityDay(t, s, "2026-07-09")
	if equity != 259100.10 || source != olderName {
		t.Fatalf("fallback after removed file = %v from %q, want older retained statement", equity, source)
	}
	requireNoEquityDay(t, s, "2026-07-10")
	if got := countRows(t, s, "statement_files"); got != 1 {
		t.Fatalf("statement_files rows = %d, want 1 after removal", got)
	}
}

func TestStatementParseFailurePreservesCompleteSnapshot(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	const name = "flex-20260713-063000.xml"
	writeStatement(t, opts.StatementsDir, name, stmtFixtureA)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	writeStatement(t, opts.StatementsDir, name, "<FlexQueryResponse><broken")
	s.ingestAll(context.Background())

	equity, source := equityDay(t, s, "2026-07-09")
	if equity != 259100.10 || source != name {
		t.Fatalf("parse failure replaced complete snapshot: %v from %q", equity, source)
	}
}
