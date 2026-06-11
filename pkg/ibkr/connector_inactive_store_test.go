package ibkr

import (
	"context"
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
