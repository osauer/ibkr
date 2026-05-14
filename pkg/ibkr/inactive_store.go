package ibkr

import "context"

// InactiveSymbolStore persists inactive symbol metadata across sessions so
// callers can avoid re-requesting obviously delisted contracts on every
// startup. Library consumers can implement this and pass it to
// Connector.UseInactiveSymbolStore. The connector and the bundled daemon
// don't persist by default — the in-memory inactive map is per-process.
type InactiveSymbolStore interface {
	LoadInactiveSymbols(ctx context.Context) (map[string]inactiveSymbolState, error)
	SaveInactiveSymbol(ctx context.Context, symbol string, state inactiveSymbolState) error
	RemoveInactiveSymbol(ctx context.Context, symbol string) error
}
