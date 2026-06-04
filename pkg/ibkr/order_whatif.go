package ibkr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	OrderWhatIfStatusUnavailable = "unavailable"
	OrderWhatIfStatusAccepted    = "accepted"
	OrderWhatIfStatusRejected    = "rejected"

	placeOrderFieldWhatIf = 100
)

var ErrOrderWhatIfUnavailable = errors.New("order whatif unavailable")

// OrderWhatIfMargin is the broker's pre-trade margin and commission estimate
// returned on a WhatIf openOrder callback.
type OrderWhatIfMargin struct {
	Currency                string
	InitialMarginBefore     *float64
	InitialMarginAfter      *float64
	MaintenanceMarginBefore *float64
	MaintenanceMarginAfter  *float64
	EquityWithLoanBefore    *float64
	EquityWithLoanAfter     *float64
	Commission              *float64
	MinCommission           *float64
	MaxCommission           *float64
	CommissionCurrency      string
	WarningText             string
}

// OrderWhatIfResult is the narrow broker preview result used by the daemon's
// order-preview gate. It is not an order submission receipt.
type OrderWhatIfResult struct {
	OrderID            int
	Status             string
	BrokerStatus       string
	Message            string
	AdvancedRejectJSON string
	Margin             OrderWhatIfMargin
}

// PreviewOrderWhatIf sends a broker WhatIf order preview and waits for the
// matching broker openOrder/error callback. It is intentionally available in
// the default build because WhatIf=true makes the broker evaluate without
// creating a working order; PlaceOrder/CancelOrder remain guarded.
func (c *Connection) PreviewOrderWhatIf(ctx context.Context, order *IBKROrder) (OrderWhatIfResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if order == nil {
		return OrderWhatIfResult{}, fmt.Errorf("order is nil")
	}
	if !c.IsConnected() {
		return orderWhatIfUnavailableResult("not connected to IBKR"), nil
	}
	if err := preparePlaceOrder(order, c); err != nil {
		return OrderWhatIfResult{}, err
	}
	order.WhatIf = true
	order.Transmit = true
	c.markWhatIfOrderID(order.OrderID)

	resultCh := make(chan OrderWhatIfResult, 1)
	errorHandlerID := c.RegisterHandler(msgErrMsg, func(fields []string) {
		reqID, code, msg, ok := parseOrderWhatIfError(fields)
		if !ok || reqID != order.OrderID || orderWhatIfInformationalError(code) {
			return
		}
		sendOrderWhatIfResult(resultCh, OrderWhatIfResult{
			OrderID:      order.OrderID,
			Status:       OrderWhatIfStatusRejected,
			BrokerStatus: "rejected",
			Message:      orderWhatIfErrorMessage(code, msg, ""),
		})
	})
	systemNoticeHandlerID := c.RegisterHandler(msgSystemNotification, func(fields []string) {
		reqID, code, msg, advancedRejectJSON, ok := parseOrderWhatIfSystemNotice(fields)
		if !ok || reqID != order.OrderID || orderWhatIfInformationalError(code) {
			return
		}
		sendOrderWhatIfResult(resultCh, OrderWhatIfResult{
			OrderID:            order.OrderID,
			Status:             OrderWhatIfStatusRejected,
			BrokerStatus:       "rejected",
			Message:            orderWhatIfErrorMessage(code, msg, advancedRejectJSON),
			AdvancedRejectJSON: advancedRejectJSON,
		})
	})
	openOrderHandlerID := c.RegisterHandler(msgOpenOrder, func(fields []string) {
		result, ok := parseOrderWhatIfOpenOrder(fields, order.OrderID, c.serverVersion)
		if ok {
			sendOrderWhatIfResult(resultCh, result)
		}
	})
	defer c.UnregisterHandler(msgErrMsg, errorHandlerID)
	defer c.UnregisterHandler(msgSystemNotification, systemNoticeHandlerID)
	defer c.UnregisterHandler(msgOpenOrder, openOrderHandlerID)

	if err := c.sendPlaceOrderFrame(order); err != nil {
		return orderWhatIfUnavailableResult(fmt.Sprintf("send broker WhatIf: %v", err)), nil
	}

	select {
	case <-ctx.Done():
		return orderWhatIfUnavailableResult("timeout waiting for broker WhatIf response"), nil
	case result := <-resultCh:
		return result, nil
	}
}

// PreviewOrderWhatIf sends a broker WhatIf preview for a connector-level
// contract/order pair. It deliberately does not update Connector.openOrders
// because no working order should exist.
func (c *Connector) PreviewOrderWhatIf(ctx context.Context, contract *Contract, order *RawOrder) (OrderWhatIfResult, error) {
	return c.previewOrderWhatIf(ctx, contract, order, 0)
}

// PreviewOrderWhatIfWithOrderID sends a broker WhatIf preview using an
// existing broker order ID. It is used for paper modify previews where IBKR
// must evaluate the exact replacement draft for a tracked order.
func (c *Connector) PreviewOrderWhatIfWithOrderID(ctx context.Context, contract *Contract, order *RawOrder, orderID int) (OrderWhatIfResult, error) {
	if orderID <= 0 {
		return OrderWhatIfResult{}, fmt.Errorf("order ID must be positive")
	}
	return c.previewOrderWhatIf(ctx, contract, order, orderID)
}

func (c *Connector) previewOrderWhatIf(ctx context.Context, contract *Contract, order *RawOrder, orderID int) (OrderWhatIfResult, error) {
	if contract == nil {
		return OrderWhatIfResult{}, fmt.Errorf("contract is nil")
	}
	if order == nil {
		return OrderWhatIfResult{}, fmt.Errorf("order is nil")
	}
	if !c.isConnected() {
		return orderWhatIfUnavailableResult("not connected to IBKR"), nil
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return orderWhatIfUnavailableResult("no active connection"), nil
	}

	ibkrOrder := &IBKROrder{
		ConID:        contract.ConID,
		Symbol:       contract.Symbol,
		SecType:      contract.SecType,
		Expiry:       contract.Expiry,
		Strike:       contract.Strike,
		Right:        contract.Right,
		Multiplier:   multiplierToString(contract.Multiplier),
		Exchange:     contract.Exchange,
		PrimaryExch:  contract.PrimaryExch,
		Currency:     contract.Currency,
		LocalSymbol:  contract.LocalSymbol,
		TradingClass: contract.TradingClass,
		SecIDType:    contract.SecIDType,
		SecID:        contract.SecID,
		Action:       order.Action,
		TotalQty:     order.TotalQty,
		OrderType:    order.OrderType,
		LmtPrice:     order.LmtPrice,
		AuxPrice:     order.AuxPrice,
		TIF:          order.TIF,
		OrderRef:     order.OrderRef,
		OutsideRth:   order.OutsideRth,
		Account:      order.Account,
		Transmit:     false,
		WhatIf:       true,
		OpenClose:    strings.ToUpper(strings.TrimSpace(order.OpenClose)),
		Origin:       0,
	}
	if ibkrOrder.OpenClose == "" {
		ibkrOrder.OpenClose = "O"
	}
	if orderID > 0 {
		ibkrOrder.OrderID = orderID
	}
	return conn.PreviewOrderWhatIf(ctx, ibkrOrder)
}

func preparePlaceOrder(order *IBKROrder, c *Connection) error {
	if err := ValidateOrder(order); err != nil {
		return err
	}

	stringFields := []struct {
		name  string
		value string
	}{
		{"symbol", order.Symbol},
		{"secType", order.SecType},
		{"exchange", order.Exchange},
		{"currency", order.Currency},
		{"primary exchange", order.PrimaryExch},
		{"local symbol", order.LocalSymbol},
		{"trading class", order.TradingClass},
		{"account", order.Account},
		{"orderRef", order.OrderRef},
		{"tif", order.TIF},
		{"action", order.Action},
	}
	for _, field := range stringFields {
		if err := ensureASCII(field.name, field.value); err != nil {
			return err
		}
	}

	if order.OrderID == 0 {
		order.OrderID = c.GetNextOrderID()
	}
	if order.ClientID == 0 {
		order.ClientID = c.config.ClientID
	}
	if order.Account == "" {
		order.Account = c.account
	}
	if order.Account == "" {
		order.Account = c.config.Account
	}
	if order.OpenClose == "" {
		order.OpenClose = "O"
	}
	return nil
}

func (c *Connection) sendPlaceOrderFrame(order *IBKROrder) error {
	if c.serverVersion >= minServerVerProtoBufPlaceOrder {
		msg, err := encodePlaceOrderProtoFrame(order)
		if err != nil {
			return err
		}
		return c.sendMessageWithType(msg, RequestTypeOrder)
	}

	fields := clonePlaceOrderFields()
	assignPlaceOrderFields(fields, order)

	interfaces := make([]any, len(fields))
	interfaces[0] = placeOrder
	for i := 1; i < len(fields); i++ {
		interfaces[i] = fields[i]
	}

	msg := c.encodeMsg(interfaces...)
	return c.sendMessageWithType(msg, RequestTypeOrder)
}

func orderWhatIfUnavailableResult(message string) OrderWhatIfResult {
	return OrderWhatIfResult{
		Status:  OrderWhatIfStatusUnavailable,
		Message: strings.TrimSpace(message),
	}
}

func sendOrderWhatIfResult(ch chan OrderWhatIfResult, result OrderWhatIfResult) {
	select {
	case ch <- result:
	default:
	}
}

func parseOrderWhatIfError(fields []string) (reqID, code int, message string, ok bool) {
	if len(fields) < 4 {
		return 0, 0, "", false
	}
	reqID, _ = strconv.Atoi(strings.TrimSpace(orderEventField(fields, 2)))
	code, _ = strconv.Atoi(strings.TrimSpace(orderEventField(fields, 3)))
	message = strings.TrimSpace(orderEventField(fields, 4))
	return reqID, code, message, code != 0
}

func parseOrderWhatIfSystemNotice(fields []string) (reqID, code int, message, advancedRejectJSON string, ok bool) {
	if len(fields) < 2 {
		return 0, 0, "", "", false
	}
	note, err := parseSystemNotificationPayload([]byte(fields[1]))
	if err != nil || note == nil || note.tickerID < 0 || note.code == 0 {
		return 0, 0, "", "", false
	}
	return int(note.tickerID), note.code, strings.TrimSpace(note.message), strings.TrimSpace(note.advancedOrderRejectJSON), true
}

func orderWhatIfInformationalError(code int) bool {
	switch code {
	case 2104, 2106, 2107, 2119, 2158, 2169:
		return true
	}
	return code >= 2100 && code < 2200
}

func orderWhatIfErrorMessage(code int, message, advancedRejectJSON string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = fmt.Sprintf("broker rejected WhatIf request with error code %d", code)
	} else {
		message = fmt.Sprintf("broker rejected WhatIf request with error code %d: %s", code, message)
	}
	if advancedRejectJSON = strings.TrimSpace(advancedRejectJSON); advancedRejectJSON != "" {
		message += " advanced_reject_json=" + advancedRejectJSON
	}
	return message
}

func parseOrderWhatIfOpenOrder(fields []string, orderID, serverVersion int) (OrderWhatIfResult, bool) {
	if len(fields) == 0 || strings.TrimSpace(fields[0]) != strconv.Itoa(msgOpenOrder) {
		return OrderWhatIfResult{}, false
	}
	if len(fields) > 1 && fields[1] == "protobuf" {
		return parseOrderWhatIfOpenOrderProto(fields, orderID)
	}
	start := 1
	if len(fields) > 2 && orderEventIntOK(fields[1]) {
		start = 2
	}
	if orderEventInt(orderEventField(fields, start)) != orderID {
		return OrderWhatIfResult{}, false
	}

	idx := findOrderWhatIfStateIndex(fields)
	if idx < 0 {
		return OrderWhatIfResult{}, false
	}
	brokerStatus := strings.TrimSpace(orderEventField(fields, idx+1))
	margin, rejectReason := parseOrderWhatIfMargin(fields, idx+2, serverVersion)
	status := OrderWhatIfStatusAccepted
	message := ""
	if orderWhatIfRejectedStatus(brokerStatus) || rejectReason != "" {
		status = OrderWhatIfStatusRejected
		message = rejectReason
		if message == "" {
			message = fmt.Sprintf("broker WhatIf status is %s", brokerStatus)
		}
	}
	return OrderWhatIfResult{
		OrderID:      orderID,
		Status:       status,
		BrokerStatus: brokerStatus,
		Message:      message,
		Margin:       margin,
	}, true
}

func parseOrderWhatIfOpenOrderProto(fields []string, orderID int) (OrderWhatIfResult, bool) {
	if orderEventInt(summaryFieldValue(fields, "orderId=")) != orderID {
		return OrderWhatIfResult{}, false
	}
	if !protoSummaryBool(fields, "whatIf=") {
		return OrderWhatIfResult{}, false
	}
	brokerStatus := strings.TrimSpace(summaryFieldValue(fields, "status="))
	if brokerStatus == "" {
		return OrderWhatIfResult{}, false
	}
	rejectReason := strings.TrimSpace(summaryFieldValue(fields, "rejectReason="))
	margin := OrderWhatIfMargin{
		InitialMarginBefore:     parseIBKRFloatPtr(summaryFieldValue(fields, "initMarginBefore=")),
		MaintenanceMarginBefore: parseIBKRFloatPtr(summaryFieldValue(fields, "maintMarginBefore=")),
		EquityWithLoanBefore:    parseIBKRFloatPtr(summaryFieldValue(fields, "equityWithLoanBefore=")),
		InitialMarginAfter:      parseIBKRFloatPtr(summaryFieldValue(fields, "initMarginAfter=")),
		MaintenanceMarginAfter:  parseIBKRFloatPtr(summaryFieldValue(fields, "maintMarginAfter=")),
		EquityWithLoanAfter:     parseIBKRFloatPtr(summaryFieldValue(fields, "equityWithLoanAfter=")),
		Commission:              parseIBKRFloatPtr(summaryFieldValue(fields, "commission=")),
		MinCommission:           parseIBKRFloatPtr(summaryFieldValue(fields, "minCommission=")),
		MaxCommission:           parseIBKRFloatPtr(summaryFieldValue(fields, "maxCommission=")),
		CommissionCurrency:      strings.TrimSpace(summaryFieldValue(fields, "commissionCurrency=")),
		Currency:                strings.TrimSpace(summaryFieldValue(fields, "marginCurrency=")),
		WarningText:             strings.TrimSpace(summaryFieldValue(fields, "warningText=")),
	}
	status := OrderWhatIfStatusAccepted
	message := ""
	if orderWhatIfRejectedStatus(brokerStatus) || rejectReason != "" {
		status = OrderWhatIfStatusRejected
		message = rejectReason
		if message == "" {
			message = fmt.Sprintf("broker WhatIf status is %s", brokerStatus)
		}
	}
	return OrderWhatIfResult{
		OrderID:      orderID,
		Status:       status,
		BrokerStatus: brokerStatus,
		Message:      message,
		Margin:       margin,
	}, true
}

func findOrderWhatIfStateIndex(fields []string) int {
	for i := 0; i+1 < len(fields); i++ {
		if !parseIBKRBool(fields[i]) {
			continue
		}
		if orderWhatIfStateStatus(fields[i+1]) {
			return i
		}
	}
	return -1
}

func orderWhatIfStateStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "pendingsubmit", "apisent", "apipending", "presubmitted", "submitted", "inactive", "cancelled", "apicancelled", "rejected":
		return true
	default:
		return false
	}
}

func orderWhatIfRejectedStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "inactive", "cancelled", "apicancelled", "rejected":
		return true
	default:
		return false
	}
}

func parseOrderWhatIfMargin(fields []string, idx, serverVersion int) (OrderWhatIfMargin, string) {
	var margin OrderWhatIfMargin
	if serverVersion >= 142 {
		margin.InitialMarginBefore = parseIBKRFloatPtr(orderEventField(fields, idx))
		margin.MaintenanceMarginBefore = parseIBKRFloatPtr(orderEventField(fields, idx+1))
		margin.EquityWithLoanBefore = parseIBKRFloatPtr(orderEventField(fields, idx+2))
		idx += 6 // skip before/change; the public shape exposes before/after.
	}
	margin.InitialMarginAfter = parseIBKRFloatPtr(orderEventField(fields, idx))
	margin.MaintenanceMarginAfter = parseIBKRFloatPtr(orderEventField(fields, idx+1))
	margin.EquityWithLoanAfter = parseIBKRFloatPtr(orderEventField(fields, idx+2))
	margin.Commission = parseIBKRFloatPtr(orderEventField(fields, idx+3))
	margin.MinCommission = parseIBKRFloatPtr(orderEventField(fields, idx+4))
	margin.MaxCommission = parseIBKRFloatPtr(orderEventField(fields, idx+5))
	margin.CommissionCurrency = strings.TrimSpace(orderEventField(fields, idx+6))
	idx += 7

	rejectReason := ""
	if serverVersion >= 195 {
		margin.Currency = strings.TrimSpace(orderEventField(fields, idx))
		idx += 11 // currency + outside-RTH fields + suggestedSize
		rejectReason = strings.TrimSpace(orderEventField(fields, idx))
		idx++
		allocationsCount := orderEventInt(orderEventField(fields, idx))
		idx++
		if allocationsCount > 0 {
			idx += allocationsCount * 7
		}
	}
	margin.WarningText = strings.TrimSpace(orderEventField(fields, idx))
	return margin, rejectReason
}

func parseIBKRFloatPtr(raw string) *float64 {
	s := strings.TrimSpace(strings.ReplaceAll(raw, ",", ""))
	if s == "" || strings.EqualFold(s, "N/A") || strings.EqualFold(s, "NaN") {
		return nil
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value == math.MaxFloat64 {
		return nil
	}
	return &value
}

func parseIBKRBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
