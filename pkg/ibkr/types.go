// Package ibkr implements connectivity with the Interactive Brokers gateway:
// connection lifecycle, request/response encoding, market data
// subscriptions, basic contract detail queries, and lightweight caches.
//
// Design highlights:
//   - Connector owns a single Connection keyed by client ID; multi-client
//     pool scaffolding was retired in favour of one-client-one-connection.
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

// Greeks represents option sensitivities.
type Greeks struct {
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`
	Rho   float64 `json:"rho"`
}

// MarketData represents a market data tick.
type MarketData struct {
	Symbol    string    `json:"symbol"`
	Timestamp time.Time `json:"timestamp"`

	Last float64 `json:"last"`
	Bid  float64 `json:"bid"`
	Ask  float64 `json:"ask"`
	Mid  float64 `json:"mid"`
	// MarkPrice is tick 37 from IBKR — the gateway's calculated fair
	// price. Populated for every symbol, but only load-bearing for
	// indices (VIX, VIX3M, SPX), which don't emit bid/ask/last.
	MarkPrice float64 `json:"mark_price,omitempty"`
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	VWAP      float64 `json:"vwap"`

	// Week-range highs/lows from generic tick 165 (Misc Stats). Zero when
	// the gateway hasn't delivered the tick yet — caller must distinguish
	// "not arrived" from "exactly zero" via the timestamp / Observed state.
	Week13Low  float64 `json:"week_13_low,omitempty"`
	Week13High float64 `json:"week_13_high,omitempty"`
	Week26Low  float64 `json:"week_26_low,omitempty"`
	Week26High float64 `json:"week_26_high,omitempty"`
	Week52Low  float64 `json:"week_52_low,omitempty"`
	Week52High float64 `json:"week_52_high,omitempty"`

	Volume          int64     `json:"volume"`
	AvgVolume       int64     `json:"avg_volume,omitempty"`
	LastTradeTime   time.Time `json:"last_trade_time,omitzero"`
	BidSize         int       `json:"bid_size"`
	AskSize         int       `json:"ask_size"`
	OpenInt         int64     `json:"open_int"`
	OpenIntObserved bool      `json:"open_int_observed,omitempty"`

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

// Order represents a trading order.
type Order struct {
	ID          string      `json:"id"`
	BrokerID    string      `json:"broker_id"`
	Symbol      string      `json:"symbol"`
	Side        OrderSide   `json:"side"`
	Quantity    float64     `json:"quantity"`
	OrderType   OrderType   `json:"order_type"`
	LimitPrice  float64     `json:"limit_price"`
	StopPrice   float64     `json:"stop_price"`
	TimeInForce TimeInForce `json:"time_in_force"`
	Status      OrderStatus `json:"status"`
	FilledQty   float64     `json:"filled_qty"`
	FilledPrice float64     `json:"filled_price"`
	Commission  float64     `json:"commission"`
	Reason      string      `json:"reason"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	FilledAt    *time.Time  `json:"filled_at,omitempty"`
	CancelledAt *time.Time  `json:"cancelled_at,omitempty"`

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
