package daemon

import "errors"

// ErrTradingDisabled is returned when the local order-entry gate is closed or
// an order-write handler is intentionally unavailable. The dispatcher returns
// this as CodeTradingDisabled rather than unknown_method so clients get a
// clear safety refusal instead of a method-typo guess.
var ErrTradingDisabled = errors.New("trading disabled")
