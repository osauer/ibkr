package ibkr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type savedInactiveSymbol struct {
	symbol string
	state  inactiveSymbolState
}

type stubInactiveStore struct {
	load  map[string]inactiveSymbolState
	saved []savedInactiveSymbol
}

func (s *stubInactiveStore) LoadInactiveSymbols(ctx context.Context) (map[string]inactiveSymbolState, error) {
	if s.load == nil {
		return map[string]inactiveSymbolState{}, nil
	}
	return s.load, nil
}

func (s *stubInactiveStore) SaveInactiveSymbol(ctx context.Context, symbol string, state inactiveSymbolState) error {
	s.saved = append(s.saved, savedInactiveSymbol{symbol: symbol, state: state})
	return nil
}

func (s *stubInactiveStore) RemoveInactiveSymbol(ctx context.Context, symbol string) error {
	return nil
}

func TestConnectoruseInactiveSymbolStoreSeedsState(t *testing.T) {
	cfg := &ConnectorConfig{BaseConfig: DefaultConfig(), PreferredClientID: 1}
	conn := NewConnector(cfg)
	store := &stubInactiveStore{
		load: map[string]inactiveSymbolState{
			"HGENQ": {
				reason:   "No security definition has been found",
				markedAt: time.Now().Add(-24 * time.Hour),
			},
		},
	}
	if err := conn.useInactiveSymbolStore(context.Background(), store); err != nil {
		t.Fatalf("useInactiveSymbolStore: %v", err)
	}
	if !conn.IsSymbolInactive("hgenq") {
		t.Fatalf("expected symbol seeded as inactive")
	}
}

func TestConnectorPersistsDelistedSymbolReasons(t *testing.T) {
	cfg := &ConnectorConfig{BaseConfig: DefaultConfig(), PreferredClientID: 1}
	conn := NewConnector(cfg)
	store := &stubInactiveStore{
		load: map[string]inactiveSymbolState{},
	}
	if err := conn.useInactiveSymbolStore(context.Background(), store); err != nil {
		t.Fatalf("useInactiveSymbolStore: %v", err)
	}

	conn.markSymbolInactive("HGENQ", "No security definition has been found for the request")
	if len(store.saved) != 1 {
		t.Fatalf("expected one persisted entry, got %d", len(store.saved))
	}
	if store.saved[0].symbol != "HGENQ" {
		t.Fatalf("unexpected symbol persisted: %s", store.saved[0].symbol)
	}

	conn.markSymbolInactive("SPY", "Temporary data outage")
	if len(store.saved) != 1 {
		t.Fatalf("expected temporary errors to skip persistence, got %d saved entries", len(store.saved))
	}
}

// TestConnectorNeverPersistsCashRoutes pins the FX guard: CASH routes are
// currency-ledger repair infrastructure, not listings — the inverted
// direction of a pair legitimately draws "no security definition" (observed
// 2026-06-11: USD|CASH|IDEALPRO|IDEALPRO|EUR|| suppressed on the FX repair
// path), and a persisted entry could silently break positions FX-rate repair
// across sessions. In-memory suppression is the ceiling.
func TestConnectorNeverPersistsCashRoutes(t *testing.T) {
	cfg := &ConnectorConfig{BaseConfig: DefaultConfig(), PreferredClientID: 1}
	conn := NewConnector(cfg)
	store := &stubInactiveStore{load: map[string]inactiveSymbolState{}}
	if err := conn.useInactiveSymbolStore(context.Background(), store); err != nil {
		t.Fatalf("useInactiveSymbolStore: %v", err)
	}

	conn.markSymbolInactive("USD.EUR", "No security definition has been found for the request")
	conn.markSymbolInactive("USD|CASH|IDEALPRO|IDEALPRO|EUR||", "No security definition has been found for the request")

	if len(store.saved) != 0 {
		t.Fatalf("CASH/FX routes must never persist, got %d saved entries: %+v", len(store.saved), store.saved)
	}
	if !conn.IsSymbolInactive("USD.EUR") {
		t.Fatal("in-memory suppression of the FX route should still apply")
	}
}

func TestFileInactiveSymbolStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "inactive-symbols.json")
	store := newFileInactiveSymbolStore(path)
	markedAt := time.Date(2026, time.June, 19, 7, 0, 0, 0, time.UTC)

	if err := store.SaveInactiveSymbol(ctx, "hgenq", inactiveSymbolState{
		reason:   "No security definition has been found for the request",
		markedAt: markedAt,
	}); err != nil {
		t.Fatalf("SaveInactiveSymbol: %v", err)
	}

	records, err := store.LoadInactiveSymbols(ctx)
	if err != nil {
		t.Fatalf("LoadInactiveSymbols: %v", err)
	}
	got, ok := records["HGENQ"]
	if !ok {
		t.Fatalf("expected HGENQ record, got %+v", records)
	}
	if got.reason != "No security definition has been found for the request" {
		t.Fatalf("reason = %q", got.reason)
	}
	if !got.markedAt.Equal(markedAt) {
		t.Fatalf("markedAt = %s, want %s", got.markedAt, markedAt)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat store: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode = %o, want 600", info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat store dir: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("store dir mode = %o, want 700", info.Mode().Perm())
	}
}

func TestFileInactiveSymbolStoreSkipsTransientAndCashRoutes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "inactive-symbols.json")
	store := newFileInactiveSymbolStore(path)

	if err := store.SaveInactiveSymbol(ctx, "SPY", inactiveSymbolState{
		reason:   "Temporary data outage",
		markedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveInactiveSymbol transient: %v", err)
	}
	if err := store.SaveInactiveSymbol(ctx, "USD|CASH|IDEALPRO|IDEALPRO|EUR||", inactiveSymbolState{
		reason:   "No security definition has been found for the request",
		markedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveInactiveSymbol CASH route: %v", err)
	}
	records, err := store.LoadInactiveSymbols(ctx)
	if err != nil {
		t.Fatalf("LoadInactiveSymbols: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no persisted records, got %+v", records)
	}
}

func TestNewConnectorLoadsInactiveSymbolsFromConfiguredPath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "inactive-symbols.json")
	store := newFileInactiveSymbolStore(path)
	if err := store.SaveInactiveSymbol(ctx, "HGENQ", inactiveSymbolState{
		reason:   "No security definition has been found for the request",
		markedAt: time.Date(2026, time.June, 19, 7, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveInactiveSymbol: %v", err)
	}

	conn := NewConnector(&ConnectorConfig{
		BaseConfig:              DefaultConfig(),
		PreferredClientID:       1,
		InactiveSymbolStorePath: path,
	})
	if !conn.IsSymbolInactive("hgenq") {
		t.Fatal("expected connector to preload HGENQ from inactive symbol store")
	}
}
