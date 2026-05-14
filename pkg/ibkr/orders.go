package ibkr

import (
	"fmt"
	"time"
)

// IBKROrder represents an order in the IBKR system
type IBKROrder struct {
	OrderID  int
	ClientID int
	PermID   int
	Account  string

	// Contract details
	Symbol       string
	SecType      string
	ConID        int
	Exchange     string
	Currency     string
	Expiry       string
	Strike       float64
	Right        string
	Multiplier   string
	PrimaryExch  string
	LocalSymbol  string
	TradingClass string
	SecIDType    string
	SecID        string

	// Order details
	Action    string // BUY or SELL
	TotalQty  int
	OrderType string // MKT, LMT, STP, etc.
	LmtPrice  float64
	AuxPrice  float64 // Stop price for stop orders

	// Time in force
	TIF           string // DAY, GTC, IOC, FOK
	OcaGroup      string
	OcaType       int
	OrderRef      string // Our reference
	Transmit      bool
	OpenClose     string
	Origin        int
	ParentID      int
	BlockOrder    bool
	SweepToFill   bool
	DisplaySize   int
	TriggerMethod int
	OutsideRth    bool // Allow trading outside regular hours
	Hidden        bool

	// State
	Status        string // PendingSubmit, Submitted, Filled, Cancelled, etc.
	Filled        int
	Remaining     int
	AvgFillPrice  float64
	LastFillPrice float64

	// Timestamps
	CreatedTime   time.Time
	SubmittedTime time.Time
	FilledTime    *time.Time
	CancelledTime *time.Time

	// Error tracking
	LastError string
	WhyHeld   string

	// Misc optional parameters
	GoodAfterTime                  string
	GoodTillDate                   string
	Rule80A                        string
	SettlingFirm                   string
	AllOrNone                      bool
	MinQty                         int
	PercentOffset                  float64
	ETradeOnly                     bool
	FirmQuoteOnly                  bool
	NbboPriceCap                   float64
	AuctionStrategy                int
	StartingPrice                  float64
	StockRefPrice                  float64
	Delta                          float64
	StockRangeLower                float64
	StockRangeUpper                float64
	OverridePercentageConstraints  bool
	Volatility                     float64
	VolatilityType                 int
	DeltaNeutralOrderType          string
	DeltaNeutralAuxPrice           float64
	DeltaNeutralConID              int
	DeltaNeutralSettlingFirm       string
	DeltaNeutralClearingAccount    string
	DeltaNeutralClearingIntent     string
	DeltaNeutralOpenClose          string
	DeltaNeutralShortSale          bool
	DeltaNeutralShortSaleSlot      int
	DeltaNeutralDesignatedLocation string
	ContinuousUpdate               int
	ReferencePriceType             int
	TrailStopPrice                 float64
	TrailingPercent                float64
	BasisPoints                    float64
	BasisPointsType                int
	ScaleInitLevelSize             int
	ScaleSubsLevelSize             int
	ScalePriceIncrement            float64
	ScalePriceAdjustValue          float64
	ScalePriceAdjustInterval       int
	ScaleProfitOffset              float64
	ScaleAutoReset                 bool
	ScaleInitPosition              int
	ScaleInitFillQty               int
	ScaleRandomPercent             bool
	HedgeType                      string
	HedgeParam                     string
	OptOutSmartRouting             bool
	ClearingAccount                string
	ClearingIntent                 string
	NotHeld                        bool
	ModelCode                      string
	ShortSaleSlot                  int
	DesignatedLocation             string
	ExemptCode                     int
	DiscretionaryAmt               float64
	FaGroup                        string
	FaMethod                       string
	FaPercentage                   string
	FaProfile                      string
}

// ValidateOrder performs basic order validation
func ValidateOrder(order *IBKROrder) error {
	if order == nil {
		return fmt.Errorf("order is nil")
	}

	if order.Symbol == "" {
		return fmt.Errorf("symbol is required")
	}

	if order.TotalQty <= 0 {
		return fmt.Errorf("quantity must be positive")
	}

	if order.Action != "BUY" && order.Action != "SELL" {
		return fmt.Errorf("action must be BUY or SELL")
	}

	if order.OrderType == "" {
		return fmt.Errorf("order type is required")
	}

	// Validate limit price for limit orders
	if order.OrderType == "LMT" && order.LmtPrice <= 0 {
		return fmt.Errorf("limit price required for limit orders")
	}

	// Validate stop price for stop orders
	if (order.OrderType == "STP" || order.OrderType == "STP LMT") && order.AuxPrice <= 0 {
		return fmt.Errorf("stop price required for stop orders")
	}

	// Default TIF if not specified
	if order.TIF == "" {
		order.TIF = "DAY"
	}

	return nil
}
