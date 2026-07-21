//go:build trading

package ibkr

import "errors"

// ErrTradingDisabled is the same sentinel exposed by the default build so
// callers can compile against either build mode. Unrestricted order-writing
// methods do not return it in a "trading" build; that only enables the raw wire
// methods and does not grant application-level submit authority.
var ErrTradingDisabled = errors.New("trading disabled (pkg/ibkr is read-only by default; rebuild with -tags trading to enable order wire methods)")

var tradingEnabled = true
