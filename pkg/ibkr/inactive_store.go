package ibkr

import "context"

// inactiveSymbolStore persists inactive symbol metadata across sessions
// so the connector can avoid re-requesting obviously delisted contracts
// on every startup. Unexported because the load/save methods reference
// the unexported inactiveSymbolState, so this contract cannot be
// satisfied by external callers. The daemon does not wire up a store —
// the in-memory inactive map is per-process — leaving this as a test
// hook.
type inactiveSymbolStore interface {
	LoadInactiveSymbols(ctx context.Context) (map[string]inactiveSymbolState, error)
	SaveInactiveSymbol(ctx context.Context, symbol string, state inactiveSymbolState) error
	RemoveInactiveSymbol(ctx context.Context, symbol string) error
}
