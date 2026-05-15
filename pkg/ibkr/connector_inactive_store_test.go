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
