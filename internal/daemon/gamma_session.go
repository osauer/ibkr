package daemon

import (
	"time"

	"github.com/osauer/ibkr/v2/internal/marketcal"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// gammaClassifySession classifies the option-data surface used by dealer
// gamma, not the underlying ETF quote session. The compute needs option OI,
// IV/model ticks, and classed SPX/SPXW contracts; outside the official regular
// U.S. listed-options session a non-force refresh is not expected to improve a
// good last-known snapshot reliably.
func gammaClassifySession(now time.Time) rpc.SessionClass {
	if now.IsZero() {
		now = time.Now()
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	session, err := cal.SessionAt(marketcal.MarketUSOptions, now)
	if err == nil {
		if session.IsOpen {
			return rpc.SessionRTH
		}
		if session.State != marketcal.StateUnknown {
			return rpc.SessionClosed
		}
	}
	if gammaWeekdayOptionsRegular(now) {
		return rpc.SessionRTH
	}
	return rpc.SessionClosed
}

func gammaWeekdayOptionsRegular(now time.Time) bool {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return true
	}
	t := now.In(ny)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	open := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, ny)
	closeT := time.Date(t.Year(), t.Month(), t.Day(), 16, 15, 0, 0, ny)
	return !t.Before(open) && t.Before(closeT)
}
