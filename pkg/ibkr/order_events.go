package ibkr

import (
	"strconv"
	"strings"
)

const (
	OrderLifecycleEventOpenOrder   = "openOrder"
	OrderLifecycleEventStatus      = "orderStatus"
	OrderLifecycleEventExecDetails = "execDetails"
)

// OrderLifecycleEvent is the typed subset of IBKR order callbacks needed by
// daemon reconciliation. It is intentionally independent from Connector.Order
// mutation: socket writes do not imply broker acknowledgement.
type OrderLifecycleEvent struct {
	Type          string
	OrderID       int
	PermID        int
	ClientID      int
	RequestID     int
	Status        string
	Symbol        string
	SecType       string
	Expiry        string
	Strike        float64
	Right         string
	Multiplier    int
	Exchange      string
	Currency      string
	LocalSymbol   string
	TradingClass  string
	Action        string
	TotalQuantity float64
	OrderType     string
	LimitPrice    float64
	AuxPrice      float64
	TIF           string
	Filled        float64
	Remaining     float64
	AvgFillPrice  float64
	LastFillPrice float64
	WhyHeld       string
	ExecID        string
	ExecTime      string
	Account       string
	ExecutionSide string
	Shares        float64
	Price         float64
	CumQty        float64
	OrderRef      string
	Raw           []string
}

// ParseOrderLifecycleEvent parses the three broker callbacks that move product
// order state: openOrder, orderStatus, and execDetails. Unknown or malformed
// messages return ok=false so the general connection dispatcher can ignore
// them without manufacturing state.
func ParseOrderLifecycleEvent(fields []string) (ev OrderLifecycleEvent, ok bool) {
	if len(fields) == 0 {
		return OrderLifecycleEvent{}, false
	}
	msgID, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil {
		return OrderLifecycleEvent{}, false
	}
	switch msgID {
	case msgOpenOrder:
		return parseOpenOrderEvent(fields)
	case msgOrderStatus:
		return parseOrderStatusEvent(fields)
	case msgExecDetails:
		return parseExecDetailsEvent(fields)
	default:
		return OrderLifecycleEvent{}, false
	}
}

func parseOpenOrderEvent(fields []string) (OrderLifecycleEvent, bool) {
	start := 1
	if len(fields) > 2 && orderEventIntOK(fields[1]) {
		start = 2
	}
	if len(fields) <= start+17 {
		return OrderLifecycleEvent{}, false
	}
	ev := OrderLifecycleEvent{
		Type:          OrderLifecycleEventOpenOrder,
		OrderID:       orderEventInt(fields[start]),
		PermID:        orderEventInt(orderEventField(fields, start+19)),
		Symbol:        strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+2))),
		SecType:       strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+3))),
		Expiry:        strings.TrimSpace(orderEventField(fields, start+4)),
		Strike:        orderEventFloat(orderEventField(fields, start+5)),
		Right:         strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+6))),
		Multiplier:    orderEventInt(orderEventField(fields, start+7)),
		Exchange:      strings.TrimSpace(orderEventField(fields, start+8)),
		Currency:      strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+9))),
		LocalSymbol:   strings.TrimSpace(orderEventField(fields, start+10)),
		TradingClass:  strings.TrimSpace(orderEventField(fields, start+11)),
		Action:        strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+12))),
		TotalQuantity: orderEventFloat(orderEventField(fields, start+13)),
		OrderType:     strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+14))),
		LimitPrice:    orderEventFloat(orderEventField(fields, start+15)),
		AuxPrice:      orderEventFloat(orderEventField(fields, start+16)),
		TIF:           strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+17))),
		Status:        strings.TrimSpace(orderEventField(fields, start+18)),
		Raw:           append([]string{}, fields...),
	}
	return ev, ev.OrderID > 0
}

func parseOrderStatusEvent(fields []string) (OrderLifecycleEvent, bool) {
	start := 1
	if len(fields) > 3 && orderEventIntOK(fields[1]) && orderEventIntOK(fields[2]) {
		start = 2
	}
	if len(fields) <= start+3 {
		return OrderLifecycleEvent{}, false
	}
	ev := OrderLifecycleEvent{
		Type:          OrderLifecycleEventStatus,
		OrderID:       orderEventInt(fields[start]),
		Status:        strings.TrimSpace(orderEventField(fields, start+1)),
		Filled:        orderEventFloat(orderEventField(fields, start+2)),
		Remaining:     orderEventFloat(orderEventField(fields, start+3)),
		AvgFillPrice:  orderEventFloat(orderEventField(fields, start+4)),
		PermID:        orderEventInt(orderEventField(fields, start+5)),
		LastFillPrice: orderEventFloat(orderEventField(fields, start+6)),
		ClientID:      orderEventInt(orderEventField(fields, start+7)),
		WhyHeld:       strings.TrimSpace(orderEventField(fields, start+9)),
		Raw:           append([]string{}, fields...),
	}
	return ev, ev.OrderID > 0 && ev.Status != ""
}

func parseExecDetailsEvent(fields []string) (OrderLifecycleEvent, bool) {
	start := 1
	if len(fields) > 2 && orderEventIntOK(fields[1]) {
		start = 2
	}
	if len(fields) <= start+19 {
		return OrderLifecycleEvent{}, false
	}
	ev := OrderLifecycleEvent{
		Type:          OrderLifecycleEventExecDetails,
		RequestID:     orderEventInt(orderEventField(fields, start)),
		OrderID:       orderEventInt(orderEventField(fields, start+1)),
		Symbol:        strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+3))),
		SecType:       strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+4))),
		Expiry:        strings.TrimSpace(orderEventField(fields, start+5)),
		Strike:        orderEventFloat(orderEventField(fields, start+6)),
		Right:         strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+7))),
		Multiplier:    orderEventInt(orderEventField(fields, start+8)),
		Exchange:      strings.TrimSpace(orderEventField(fields, start+9)),
		Currency:      strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+10))),
		LocalSymbol:   strings.TrimSpace(orderEventField(fields, start+11)),
		ExecID:        strings.TrimSpace(orderEventField(fields, start+12)),
		ExecTime:      strings.TrimSpace(orderEventField(fields, start+13)),
		Account:       strings.TrimSpace(orderEventField(fields, start+14)),
		ExecutionSide: strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+16))),
		Shares:        orderEventFloat(orderEventField(fields, start+17)),
		Price:         orderEventFloat(orderEventField(fields, start+18)),
		PermID:        orderEventInt(orderEventField(fields, start+19)),
		ClientID:      orderEventInt(orderEventField(fields, start+20)),
		CumQty:        orderEventFloat(orderEventField(fields, start+22)),
		AvgFillPrice:  orderEventFloat(orderEventField(fields, start+23)),
		OrderRef:      strings.TrimSpace(orderEventField(fields, start+24)),
		Raw:           append([]string{}, fields...),
	}
	return ev, ev.OrderID > 0 && ev.ExecID != ""
}

func orderEventField(fields []string, idx int) string {
	if idx < 0 || idx >= len(fields) {
		return ""
	}
	return fields[idx]
}

func orderEventInt(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func orderEventIntOK(s string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil
}

func orderEventFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}
