// Package ibkr implements connectivity with the Interactive Brokers gateway:
// connection pooling/leases, request/response encoding, market data
// subscriptions, basic contract detail queries, and lightweight caches.
//
// Design highlights:
//   - ConnectionPool hands out leases (client IDs) and maintains heartbeats.
//   - Connector multiplexes subscriptions and exposes compact status for APIs.
//   - Option IV: track real IBKR IV (tick 106). When 106 is absent, the package
//     can also subscribe to representative option quotes to derive mid prices,
//     but it should NOT fabricate IV; consumers must treat IV as unavailable.
//   - ContractDetailsLite provides only fields needed for calendar building.
//
// Protocol notes:
//   - Maintain reqMktData v11 field order when encoding requests.
//   - Keep symbol→contract mapping consistent across requests.
//   - Avoid placeholder/extra fields that shift subsequent fields.
package ibkr

import "time"

// Asset represents any tradeable instrument.
type Asset struct {
	Symbol     string    `json:"symbol"`
	AssetType  AssetType `json:"asset_type"`
	Exchange   string    `json:"exchange"`
	Multiplier int       `json:"multiplier"`
	Currency   string    `json:"currency"`
}

// AssetType defines the type of financial instrument.
type AssetType string

const (
	AssetTypeStock  AssetType = "STOCK"
	AssetTypeOption AssetType = "OPTION"
	AssetTypeFuture AssetType = "FUTURE"
	AssetTypeIndex  AssetType = "INDEX"
)

// Greeks represents option sensitivities.
type Greeks struct {
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`
	Rho   float64 `json:"rho"`
}

// AccountSummary represents account information from broker.
type AccountSummary struct {
	AccountID          string  `json:"account_id"`
	NetLiquidation     float64 `json:"net_liquidation"`
	BuyingPower        float64 `json:"buying_power"`
	CashBalance        float64 `json:"cash_balance"`
	RealizedPNL        float64 `json:"realized_pnl"`
	UnrealizedPNL      float64 `json:"unrealized_pnl"`
	AvailableFunds     float64 `json:"available_funds"`
	ExcessLiquidity    float64 `json:"excess_liquidity"`
	MaintenanceMargin  float64 `json:"maintenance_margin"`
	InitialMargin      float64 `json:"initial_margin"`
	GrossPositionValue float64 `json:"gross_position_value"`
	EquityWithLoan     float64 `json:"equity_with_loan"`
}

// Position represents a held position in the portfolio.
type Position struct {
	ID            string    `json:"id"`
	Asset         Asset     `json:"asset"`
	Quantity      float64   `json:"quantity"`
	EntryPrice    float64   `json:"entry_price"`
	CurrentPrice  float64   `json:"current_price"`
	UnrealizedPnL float64   `json:"unrealized_pnl"`
	RealizedPnL   float64   `json:"realized_pnl"`
	OpenedAt      time.Time `json:"opened_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	Greeks *Greeks `json:"greeks,omitempty"`

	VaR            float64 `json:"var"`
	MaxLoss        float64 `json:"max_loss"`
	MarginRequired float64 `json:"margin_required"`
}

// MarketData represents a market data tick.
type MarketData struct {
	Symbol    string    `json:"symbol"`
	Timestamp time.Time `json:"timestamp"`

	Last  float64 `json:"last"`
	Bid   float64 `json:"bid"`
	Ask   float64 `json:"ask"`
	Mid   float64 `json:"mid"`
	Open  float64 `json:"open"`
	High  float64 `json:"high"`
	Low   float64 `json:"low"`
	Close float64 `json:"close"`
	VWAP  float64 `json:"vwap"`

	// Week-range highs/lows from generic tick 165 (Misc Stats). Zero when
	// the gateway hasn't delivered the tick yet — caller must distinguish
	// "not arrived" from "exactly zero" via the timestamp / Observed state.
	Week13Low  float64 `json:"week_13_low,omitempty"`
	Week13High float64 `json:"week_13_high,omitempty"`
	Week26Low  float64 `json:"week_26_low,omitempty"`
	Week26High float64 `json:"week_26_high,omitempty"`
	Week52Low  float64 `json:"week_52_low,omitempty"`
	Week52High float64 `json:"week_52_high,omitempty"`

	Volume  int64 `json:"volume"`
	BidSize int   `json:"bid_size"`
	AskSize int   `json:"ask_size"`
	OpenInt int64 `json:"open_int"`

	IV     float64 `json:"iv"`
	HV     float64 `json:"hv"`
	IVRank float64 `json:"iv_rank"`
	IVPerc float64 `json:"iv_perc"`

	Greeks *Greeks `json:"greeks,omitempty"`

	PutCallRatio  float64 `json:"put_call_ratio"`
	TickDirection string  `json:"tick_direction"`

	Session   string `json:"session,omitempty"`
	DataType  string `json:"data_type,omitempty"`
	IsDelayed bool   `json:"is_delayed,omitempty"`
}

// MarketPhase represents the current trading session phase.
type MarketPhase string

const (
	MarketPhaseClosed     MarketPhase = "CLOSED"
	MarketPhasePreMarket  MarketPhase = "PRE_MARKET"
	MarketPhaseOpening    MarketPhase = "OPENING"
	MarketPhaseOpen       MarketPhase = "OPEN"
	MarketPhaseClosing    MarketPhase = "CLOSING"
	MarketPhaseAfterHours MarketPhase = "AFTER_HOURS"
)

// FreshThresholdForPhase returns the maximum acceptable age for market data
// given a specific market phase.
func FreshThresholdForPhase(phase MarketPhase) time.Duration {
	switch phase {
	case MarketPhaseOpen, MarketPhaseOpening:
		return 5 * time.Second
	case MarketPhaseClosing:
		return 15 * time.Second
	case MarketPhasePreMarket:
		return 30 * time.Minute
	case MarketPhaseAfterHours:
		return 4 * time.Hour
	case MarketPhaseClosed:
		return 72 * time.Hour
	default:
		return 5 * time.Second
	}
}

// Order represents a trading order.
type Order struct {
	ID            string      `json:"id"`
	BrokerID      string      `json:"broker_id"`
	Symbol        string      `json:"symbol"`
	Asset         Asset       `json:"asset"`
	Side          OrderSide   `json:"side"`
	Quantity      float64     `json:"quantity"`
	OrderType     OrderType   `json:"order_type"`
	LimitPrice    float64     `json:"limit_price"`
	StopPrice     float64     `json:"stop_price"`
	TimeInForce   TimeInForce `json:"time_in_force"`
	Status        OrderStatus `json:"status"`
	FilledQty     float64     `json:"filled_qty"`
	FilledPrice   float64     `json:"filled_price"`
	Commission    float64     `json:"commission"`
	StrategyID    string      `json:"strategy_id"`
	ParentOrderID string      `json:"parent_order_id"`
	InstanceID    string      `json:"instance_id,omitempty"`
	Reason        string      `json:"reason"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
	FilledAt      *time.Time  `json:"filled_at,omitempty"`
	CancelledAt   *time.Time  `json:"cancelled_at,omitempty"`

	AllowOutsideRth bool `json:"outside_rth,omitempty"`
}

// OrderSide represents order direction.
type OrderSide string

const (
	OrderSideBuy  OrderSide = "BUY"
	OrderSideSell OrderSide = "SELL"
)

// OrderType represents order execution type.
type OrderType string

const (
	OrderTypeMarket    OrderType = "MARKET"
	OrderTypeLimit     OrderType = "LIMIT"
	OrderTypeStop      OrderType = "STOP"
	OrderTypeStopLimit OrderType = "STOP_LIMIT"
	OrderTypeMOC       OrderType = "MOC"
	OrderTypeLOC       OrderType = "LOC"
	OrderTypePegMid    OrderType = "PEG_MID"
)

// TimeInForce specifies order duration.
type TimeInForce string

const (
	TimeInForceDay TimeInForce = "DAY"
	TimeInForceGTC TimeInForce = "GTC"
	TimeInForceIOC TimeInForce = "IOC"
	TimeInForceFOK TimeInForce = "FOK"
	TimeInForceGTD TimeInForce = "GTD"
	TimeInForceOPG TimeInForce = "OPG"
)

// OrderStatus represents order state.
type OrderStatus string

const (
	OrderStatusNew       OrderStatus = "NEW"
	OrderStatusPending   OrderStatus = "PENDING"
	OrderStatusSubmitted OrderStatus = "SUBMITTED"
	OrderStatusAccepted  OrderStatus = "ACCEPTED"
	OrderStatusPartial   OrderStatus = "PARTIAL"
	OrderStatusFilled    OrderStatus = "FILLED"
	OrderStatusCancelled OrderStatus = "CANCELLED"
	OrderStatusRejected  OrderStatus = "REJECTED"
	OrderStatusExpired   OrderStatus = "EXPIRED"
)
