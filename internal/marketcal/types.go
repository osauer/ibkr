// Package marketcal provides official exchange-session calendars for the
// markets ibkr can explain without consulting IBKR contract metadata.
package marketcal

import "time"

// Market is the supported exchange/session calendar identifier.
type Market string

const (
	// MarketUSEquity is the regular U.S. cash-equity / ETF session.
	MarketUSEquity Market = "us_equity"
	// MarketUSOptions is the regular U.S. listed-options session. v1
	// intentionally models only the regular Cboe-style session, not SPX/VIX
	// global/curb trading or per-class late-close exceptions.
	MarketUSOptions Market = "us_options"
	// MarketDEXetra is Deutsche Boerse Xetra's cash-equity session.
	MarketDEXetra Market = "de_xetra"
)

// State classifies a market at an instant or a calendar date.
type State string

const (
	StateRegular    State = "regular"
	StateClosed     State = "closed"
	StateHoliday    State = "holiday"
	StateEarlyClose State = "early_close"
	StateUnknown    State = "unknown"
)

// Session is one market's trading-session context for a date or instant.
type Session struct {
	Market        Market
	Label         string
	Date          string
	Timezone      string
	State         State
	IsOpen        bool
	Reason        string
	Open          time.Time
	Close         time.Time
	NextOpen      *time.Time
	NextClose     *time.Time
	Source        string
	SourceURL     string
	CoverageStart string
	CoverageEnd   string
	Notes         string
}

// Query requests calendar context. Date and At are mutually complementary:
// At wins when non-zero; otherwise Date is interpreted in the market's local
// timezone at noon so the result describes that date without boundary noise.
type Query struct {
	Market Market
	Date   string
	At     time.Time
	Days   int
}

// Result is the calendar response: the current/day session plus a forward
// list of sessions when Days > 0.
type Result struct {
	Market        Market
	Label         string
	Timezone      string
	AsOf          time.Time
	CoverageStart string
	CoverageEnd   string
	Source        string
	SourceURL     string
	Session       Session
	Sessions      []Session
}
