//go:build !trading

package daemon

import (
	"context"
	"errors"

	"github.com/osauer/ibkr/internal/rpc"
)

// ErrTradingDisabled is returned by every order-related handler in the
// default v1 build. v2 introduces the counterpart trading_enabled.go behind
// the trading build tag with confirmation, dry-run and audit-log paths.
var ErrTradingDisabled = errors.New("trading disabled in this build (v1 is read-only by design)")

// handleOrderPlace is intentionally a stub in the default build. The function
// exists so the dispatcher can route MethodOrderPlace and produce a uniform
// trading_disabled error rather than 'unknown_method'.
func handleOrderPlace(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}

// handleOrderCancel is the cancellation counterpart, equally disabled.
func handleOrderCancel(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}
