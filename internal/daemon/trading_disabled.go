package daemon

import (
	"context"
	"errors"

	"github.com/osauer/ibkr/internal/rpc"
)

// ErrTradingDisabled is returned by the order-related handlers. The daemon
// is read-only in v1 by design — every order verb the dispatcher routes
// produces this error rather than 'unknown_method', so a misconfigured
// client gets a clear refusal instead of a method-typo guess. README's
// safety section spells out the layers this sits in (settings allowlist,
// PreToolUse hook, MCP test) — this is the daemon-side anchor.
var ErrTradingDisabled = errors.New("trading disabled (this binary is read-only by design)")

func handleOrderPlace(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}

func handleOrderCancel(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}
