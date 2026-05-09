package ibkr

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// InactiveSymbolStore persists inactive symbol metadata across sessions so we can
// avoid re-requesting obviously delisted contracts on every startup.
type InactiveSymbolStore interface {
	LoadInactiveSymbols(ctx context.Context) (map[string]inactiveSymbolState, error)
	SaveInactiveSymbol(ctx context.Context, symbol string, state inactiveSymbolState) error
	RemoveInactiveSymbol(ctx context.Context, symbol string) error
}

// DBInactiveSymbolStore saves inactive symbol metadata to Postgres.
type DBInactiveSymbolStore struct {
	db *sql.DB
}

// NewDBInactiveSymbolStore wires a database-backed inactive symbol store.
func NewDBInactiveSymbolStore(db *sql.DB) *DBInactiveSymbolStore {
	if db == nil {
		return nil
	}
	return &DBInactiveSymbolStore{db: db}
}

// LoadInactiveSymbols loads the persisted inactive symbol map.
func (s *DBInactiveSymbolStore) LoadInactiveSymbols(ctx context.Context) (map[string]inactiveSymbolState, error) {
	results := make(map[string]inactiveSymbolState)
	if s == nil || s.db == nil {
		return results, nil
	}

	rows, err := s.db.QueryContext(ctx, `SELECT symbol, reason, marked_at FROM ibkr_inactive_symbols`)
	if err != nil {
		if isUndefinedTableErr(err) {
			return results, nil
		}
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var symbol, reason string
		var markedAt time.Time
		if scanErr := rows.Scan(&symbol, &reason, &markedAt); scanErr != nil {
			return nil, scanErr
		}
		upper := strings.ToUpper(strings.TrimSpace(symbol))
		if upper == "" {
			continue
		}
		results[upper] = inactiveSymbolState{
			reason:   strings.TrimSpace(reason),
			markedAt: markedAt,
		}
	}
	return results, rows.Err()
}

// SaveInactiveSymbol upserts an inactive symbol entry.
func (s *DBInactiveSymbolStore) SaveInactiveSymbol(ctx context.Context, symbol string, state inactiveSymbolState) error {
	if s == nil || s.db == nil || symbol == "" {
		return nil
	}
	upper := strings.ToUpper(symbol)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ibkr_inactive_symbols (symbol, reason, marked_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (symbol) DO UPDATE
		SET reason = EXCLUDED.reason,
		    marked_at = EXCLUDED.marked_at
	`, upper, strings.TrimSpace(state.reason), state.markedAt)
	return err
}

// RemoveInactiveSymbol removes an entry, enabling subscriptions again.
func (s *DBInactiveSymbolStore) RemoveInactiveSymbol(ctx context.Context, symbol string) error {
	if s == nil || s.db == nil || symbol == "" {
		return nil
	}
	upper := strings.ToUpper(symbol)
	_, err := s.db.ExecContext(ctx, `DELETE FROM ibkr_inactive_symbols WHERE symbol = $1`, upper)
	return err
}

func isUndefinedTableErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "does not exist") && strings.Contains(msg, "relation")
}
