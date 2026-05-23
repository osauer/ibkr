//go:build trading

package ibkr

import "errors"

// ErrTradingDisabled is retained for errors.Is compatibility across default
// and trading builds. It is not returned when the trading build tag is enabled.
var ErrTradingDisabled = errors.New("trading disabled (pkg/ibkr is read-only by default; rebuild with -tags trading to enable order wire methods)")

var tradingEnabled = true
