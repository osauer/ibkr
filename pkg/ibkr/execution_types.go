package ibkr

import "time"

// ExecutionContract captures the contract metadata attached to an execution report.
type ExecutionContract struct {
	ConID        int
	Symbol       string
	SecType      string
	Expiry       string
	Strike       float64
	Right        string
	Multiplier   string
	Exchange     string
	Currency     string
	LocalSymbol  string
	TradingClass string
}

// ExecutionReport represents a single execution/fill message from IBKR.
type ExecutionReport struct {
	ReqID                int
	OrderID              int
	ExecID               string
	Account              string
	Exchange             string
	Side                 string
	Shares               float64
	Price                float64
	PermID               int
	ClientID             int
	Liquidation          int
	CumQty               float64
	AvgPrice             float64
	OrderRef             string
	EvRule               string
	EvMultiplier         float64
	ModelCode            string
	LastLiquidity        int
	PendingPriceRevision bool
	Submitter            string
	Timestamp            time.Time
	TimeRaw              string
	Contract             ExecutionContract
}

// CommissionReport contains commission and realized PnL data for a specific execution.
type CommissionReport struct {
	ExecID              string
	CommissionAndFees   float64
	Currency            string
	RealizedPNL         float64
	Yield               float64
	YieldRedemptionDate int
}

// ExecutionFilter controls the scope of reqExecutions requests.
type ExecutionFilter struct {
	ReqID         int
	ClientID      int
	Account       string
	Time          string
	Symbol        string
	SecType       string
	Exchange      string
	Side          string
	LastNDays     int
	SpecificDates []int
}
