//go:build !trading

package ibkr

import "errors"

// ErrTradingDisabled is returned by unrestricted order-writing methods in the
// default build before an order frame is sent. Building with the "trading" tag
// enables those raw methods. The narrower paper-gated methods remain available
// in either build and require their separate [PaperOrderGate] evidence; neither
// build mode nor that evidence grants application-level submit authority.
var ErrTradingDisabled = errors.New("trading disabled (pkg/ibkr is read-only by default; rebuild with -tags trading to enable order wire methods)")

var tradingEnabled = false
