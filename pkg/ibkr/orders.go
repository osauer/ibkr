package ibkr

import (
	"fmt"
	"sync"
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

// OrderFill represents an execution/fill
type OrderFill struct {
	OrderID      int
	ExecID       string
	Time         time.Time
	Account      string
	Exchange     string
	Side         string
	Shares       int
	Price        float64
	PermID       int
	ClientID     int
	Liquidation  int
	CumQty       int
	AvgPrice     float64
	OrderRef     string
	EvRule       string
	EvMultiplier float64
	ModelCode    string
}

// OrderManager handles order lifecycle
type OrderManager struct {
	orders      map[int]*IBKROrder
	ordersByRef map[string]*IBKROrder
	fills       map[int][]*OrderFill
	mu          sync.RWMutex

	// Order ID management
	nextOrderID int
	orderIDMu   sync.Mutex
}

// NewOrderManager creates a new order manager
func NewOrderManager() *OrderManager {
	return &OrderManager{
		orders:      make(map[int]*IBKROrder),
		ordersByRef: make(map[string]*IBKROrder),
		fills:       make(map[int][]*OrderFill),
		nextOrderID: 1,
	}
}

// GetNextOrderID returns the next available order ID
func (om *OrderManager) GetNextOrderID() int {
	om.orderIDMu.Lock()
	defer om.orderIDMu.Unlock()

	id := om.nextOrderID
	om.nextOrderID++
	return id
}

// SetNextOrderID updates the next order ID (from IBKR)
func (om *OrderManager) SetNextOrderID(id int) {
	om.orderIDMu.Lock()
	defer om.orderIDMu.Unlock()

	if id > om.nextOrderID {
		om.nextOrderID = id
	}
}

// AddOrder adds a new order to tracking
func (om *OrderManager) AddOrder(order *IBKROrder) {
	om.mu.Lock()
	defer om.mu.Unlock()

	om.orders[order.OrderID] = order
	if order.OrderRef != "" {
		om.ordersByRef[order.OrderRef] = order
	}
}

// GetOrder retrieves an order by ID
func (om *OrderManager) GetOrder(orderID int) (*IBKROrder, bool) {
	om.mu.RLock()
	defer om.mu.RUnlock()

	order, exists := om.orders[orderID]
	return order, exists
}

// GetOrderByRef retrieves an order by reference
func (om *OrderManager) GetOrderByRef(ref string) (*IBKROrder, bool) {
	om.mu.RLock()
	defer om.mu.RUnlock()

	order, exists := om.ordersByRef[ref]
	return order, exists
}

// UpdateOrderStatus updates the status of an order
func (om *OrderManager) UpdateOrderStatus(orderID int, status string, filled int, remaining int, avgFillPrice float64) {
	om.mu.Lock()
	defer om.mu.Unlock()

	if order, exists := om.orders[orderID]; exists {
		order.Status = status
		order.Filled = filled
		order.Remaining = remaining
		order.AvgFillPrice = avgFillPrice

		// Update timestamps based on status
		now := time.Now()
		switch status {
		case "Submitted":
			if order.SubmittedTime.IsZero() {
				order.SubmittedTime = now
			}
		case "Filled":
			if order.FilledTime == nil {
				order.FilledTime = &now
			}
		case "Cancelled":
			if order.CancelledTime == nil {
				order.CancelledTime = &now
			}
		}
	}
}

// AddFill records an order fill/execution
func (om *OrderManager) AddFill(fill *OrderFill) {
	om.mu.Lock()
	defer om.mu.Unlock()

	om.fills[fill.OrderID] = append(om.fills[fill.OrderID], fill)

	// Update order's last fill price
	if order, exists := om.orders[fill.OrderID]; exists {
		order.LastFillPrice = fill.Price
	}
}

// GetFills retrieves all fills for an order
func (om *OrderManager) GetFills(orderID int) []*OrderFill {
	om.mu.RLock()
	defer om.mu.RUnlock()

	return om.fills[orderID]
}

// GetOpenOrders returns all open orders
func (om *OrderManager) GetOpenOrders() []*IBKROrder {
	om.mu.RLock()
	defer om.mu.RUnlock()

	var openOrders []*IBKROrder
	for _, order := range om.orders {
		if isOrderOpen(order.Status) {
			openOrders = append(openOrders, order)
		}
	}
	return openOrders
}

// isOrderOpen checks if an order status indicates it's still open
func isOrderOpen(status string) bool {
	switch status {
	case "PendingSubmit", "PendingCancel", "PreSubmitted", "Submitted":
		return true
	case "Filled", "Cancelled", "Inactive":
		return false
	default:
		return false
	}
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
