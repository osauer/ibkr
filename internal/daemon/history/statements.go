package history

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/osauer/ibkr/v2/internal/flexstmt"
)

// ingestStatements is the file-set ingest for retained Flex statements:
// every *.xml in StatementsDir missing from statement_files (or whose size
// changed) is parsed and its equity rows upserted into
// statement_equity_days with newest-whenGenerated-wins semantics — exactly
// the restatement rule of the recon engine's mergeRetainedStatements. A
// file that fails to parse is warned and skipped without a statement_files
// row, so it retries on the next pass. Statements are never modified or
// pruned by this package. Callers hold ingestMu.
func (s *Store) ingestStatements(ctx context.Context) error {
	entries, err := os.ReadDir(s.opts.StatementsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	type onDisk struct {
		name string
		size int64
	}
	var files []onDisk
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, onDisk{name: e.Name(), size: info.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	recorded := map[string]int64{}
	rows, err := s.db.Query(`SELECT name, size FROM statement_files`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			rows.Close()
			return err
		}
		recorded[name] = size
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if size, ok := recorded[f.name]; ok && size == f.size {
			continue
		}
		if err := s.ingestStatementFile(f.name, f.size); err != nil {
			s.warnf("history: statement %s not ingested (will retry): %v", f.name, err)
		}
	}
	return nil
}

// ingestStatementFile parses one retained statement and upserts its equity
// days in a single transaction with the statement_files record, so a crash
// leaves either the whole file ingested or none of it.
func (s *Store) ingestStatementFile(name string, size int64) error {
	data, err := os.ReadFile(filepath.Join(s.opts.StatementsDir, name))
	if err != nil {
		return err
	}
	statements, err := flexstmt.Parse(data)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	equityDays := 0
	for _, st := range statements {
		when := st.WhenGenerated.UTC().Format("2006-01-02T15:04:05Z07:00")
		for _, row := range st.Equity {
			if _, err := tx.Exec(`INSERT INTO statement_equity_days (account_id, day, equity_base, source_stmt, when_generated)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(account_id, day) DO UPDATE SET
  equity_base = excluded.equity_base,
  source_stmt = excluded.source_stmt,
  when_generated = excluded.when_generated
WHERE excluded.when_generated > statement_equity_days.when_generated`,
				st.AccountID, row.ReportDate.UTC().Format("2006-01-02"), row.TotalBase, name, when); err != nil {
				return err
			}
			equityDays++
		}
	}
	if _, err := tx.Exec(`INSERT INTO statement_files (name, size, ingested_at, equity_days)
VALUES (?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET size = excluded.size, ingested_at = excluded.ingested_at, equity_days = excluded.equity_days`,
		name, size, nowUTC(), equityDays); err != nil {
		return err
	}
	return tx.Commit()
}
