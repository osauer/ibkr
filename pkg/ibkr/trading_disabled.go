//go:build !trading

package ibkr

import "errors"

// ErrTradingDisabled is returned by order-writing methods in the default
// package build. The shipped ibkr binary is read-only; downstream forks that
// intentionally want the raw order wire methods must opt in with -tags trading.
var ErrTradingDisabled = errors.New("trading disabled (pkg/ibkr is read-only by default; rebuild with -tags trading to enable order wire methods)")

var tradingEnabled = false
