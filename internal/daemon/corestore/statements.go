package corestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ReplaceStatementProjection atomically replaces the complete current
// inventory/winner projection while retaining every distinct file-content and
// derived-day version as append-only evidence. A same-name restatement is a
// new version when its SHA-256 changes, regardless of file size.
func (s *Store) ReplaceStatementProjection(ctx context.Context, scopeKey string, files []StatementFileRecord, days []StatementEquityDayRecord) error {
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return err
	}
	fileByKey := make(map[string]StatementFileRecord, len(files))
	for i := range files {
		if files[i].ScopeKey != "" && files[i].ScopeKey != scopeKey {
			return errorsf("statement file scope mismatch")
		}
		if err := validateStatementFile(files[i]); err != nil {
			return err
		}
		if _, duplicate := fileByKey[files[i].FileKey]; duplicate {
			return errorsf("duplicate statement file key")
		}
		fileByKey[files[i].FileKey] = files[i]
	}
	winners := make(map[string]struct{}, len(days))
	for i := range days {
		if days[i].ScopeKey != "" && days[i].ScopeKey != scopeKey {
			return errorsf("statement day scope mismatch")
		}
		if err := validateStatementDay(days[i]); err != nil {
			return err
		}
		file, ok := fileByKey[days[i].StatementFileKey]
		if !ok {
			return errorsf("statement day references a file outside the complete inventory")
		}
		if days[i].StatementFileSHA256 != ([sha256.Size]byte{}) && days[i].StatementFileSHA256 != file.SHA256 {
			return errorsf("statement day file digest does not match inventory")
		}
		winnerKey := days[i].AccountKey + "\x00" + days[i].Day
		if _, duplicate := winners[winnerKey]; duplicate {
			return errorsf("duplicate current statement equity-day winner")
		}
		winners[winnerKey] = struct{}{}
	}
	return s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		stamp := formatTime(now)
		for _, file := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_file_versions
(scope_key,file_key,sha256,size_bytes,status,statement_generated_at,ingested_at,recorded_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(scope_key,file_key,sha256) DO NOTHING`, scopeKey, file.FileKey, file.SHA256[:], file.SizeBytes, file.Status, nullableTime(file.StatementGeneratedAt), nullableTime(file.IngestedAt), stamp); err != nil {
				return fmt.Errorf("append statement file version: %w", err)
			}
		}
		for _, day := range days {
			file := fileByKey[day.StatementFileKey]
			rawDigest := sha256.Sum256(day.RawJSON)
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_equity_day_versions
(scope_key,account_key,day,equity_base_text,statement_file_key,statement_file_sha256,generated_at,raw_json,raw_sha256,recorded_at)
VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, scopeKey, day.AccountKey, day.Day, day.EquityBaseText, day.StatementFileKey, file.SHA256[:], formatTime(day.GeneratedAt), day.RawJSON, rawDigest[:], stamp); err != nil {
				return fmt.Errorf("append statement equity-day version: %w", err)
			}
		}
		// These are current projections only. Deleting/replacing them never
		// touches the immutable version tables above.
		if _, err := tx.ExecContext(ctx, `DELETE FROM statement_equity_days WHERE scope_key=?`, scopeKey); err != nil {
			return fmt.Errorf("replace current statement days: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM statement_files WHERE scope_key=?`, scopeKey); err != nil {
			return fmt.Errorf("replace current statement inventory: %w", err)
		}
		for _, file := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_files
(scope_key,file_key,size_bytes,sha256,status,statement_generated_at,ingested_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`, scopeKey, file.FileKey, file.SizeBytes, file.SHA256[:], file.Status, nullableTime(file.StatementGeneratedAt), nullableTime(file.IngestedAt), stamp); err != nil {
				return fmt.Errorf("write current statement inventory: %w", err)
			}
		}
		for _, day := range days {
			file := fileByKey[day.StatementFileKey]
			if _, err := tx.ExecContext(ctx, `INSERT INTO statement_equity_days
(scope_key,account_key,day,equity_base_text,statement_file_key,statement_file_sha256,generated_at,raw_json,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`, scopeKey, day.AccountKey, day.Day, day.EquityBaseText, day.StatementFileKey, file.SHA256[:], formatTime(day.GeneratedAt), day.RawJSON, stamp); err != nil {
				return fmt.Errorf("write current statement equity-day winner: %w", err)
			}
		}
		_, err := advanceHeadTx(ctx, tx, 0, now)
		return err
	})
}

func (s *Store) LoadStatementFiles(ctx context.Context, scopeKey string) ([]StatementFileRecord, error) {
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT file_key,size_bytes,sha256,status,statement_generated_at,ingested_at,updated_at FROM statement_files WHERE scope_key=? ORDER BY file_key`, scopeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementFileRecord
	for rows.Next() {
		var item StatementFileRecord
		var digest []byte
		var generated, ingested sql.NullString
		var updated string
		item.ScopeKey = scopeKey
		if err := rows.Scan(&item.FileKey, &item.SizeBytes, &digest, &item.Status, &generated, &ingested, &updated); err != nil {
			return nil, err
		}
		if len(digest) != len(item.SHA256) {
			return nil, errorsf("stored statement digest is invalid")
		}
		copy(item.SHA256[:], digest)
		if generated.Valid {
			v, err := parseTime(generated.String)
			if err != nil {
				return nil, err
			}
			item.StatementGeneratedAt = &v
		}
		if ingested.Valid {
			v, err := parseTime(ingested.String)
			if err != nil {
				return nil, err
			}
			item.IngestedAt = &v
		}
		item.UpdatedAt, err = parseTime(updated)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) LoadStatementEquityDays(ctx context.Context, scopeKey, fromDay, toDay string, limit int) ([]StatementEquityDayRecord, error) {
	if err := validateKey("scope key", scopeKey, 512); err != nil {
		return nil, err
	}
	if limit < 0 || limit > 10000 {
		return nil, errorsf("statement day limit is invalid")
	}
	if limit == 0 {
		limit = 1000
	}
	clauses := []string{"scope_key=?"}
	args := []any{scopeKey}
	if fromDay != "" {
		clauses = append(clauses, "day>=?")
		args = append(args, fromDay)
	}
	if toDay != "" {
		clauses = append(clauses, "day<=?")
		args = append(args, toDay)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT equity_day_id,account_key,day,equity_base_text,statement_file_key,statement_file_sha256,generated_at,raw_json FROM statement_equity_days WHERE `+strings.Join(clauses, " AND ")+` ORDER BY day,equity_day_id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementEquityDayRecord
	for rows.Next() {
		var item StatementEquityDayRecord
		var fileDigest []byte
		var generated string
		item.ScopeKey = scopeKey
		if err := rows.Scan(&item.ID, &item.AccountKey, &item.Day, &item.EquityBaseText, &item.StatementFileKey, &fileDigest, &generated, &item.RawJSON); err != nil {
			return nil, err
		}
		if len(fileDigest) != sha256.Size {
			return nil, errorsf("stored statement-day file digest is invalid")
		}
		copy(item.StatementFileSHA256[:], fileDigest)
		item.GeneratedAt, err = parseTime(generated)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func validateStatementFile(v StatementFileRecord) error {
	if err := validateKey("statement file key", v.FileKey, 512); err != nil {
		return err
	}
	if v.SizeBytes < 0 {
		return errorsf("statement file size must not be negative")
	}
	if v.SHA256 == ([32]byte{}) {
		return errorsf("statement file digest is required")
	}
	return validateKey("statement file status", v.Status, 64)
}
func validateStatementDay(v StatementEquityDayRecord) error {
	for _, item := range []struct {
		label, value string
		limit        int
	}{{"statement account key", v.AccountKey, 256}, {"statement day", v.Day, 32}, {"equity value", v.EquityBaseText, 128}, {"statement file key", v.StatementFileKey, 512}} {
		if err := validateKey(item.label, item.value, item.limit); err != nil {
			return err
		}
	}
	if v.GeneratedAt.IsZero() {
		return errorsf("statement generation time is required")
	}
	if !json.Valid(v.RawJSON) {
		return errorsf("statement equity payload must be valid JSON")
	}
	return nil
}
func nullableTime(v *time.Time) any {
	if v == nil || v.IsZero() {
		return nil
	}
	return formatTime(*v)
}
