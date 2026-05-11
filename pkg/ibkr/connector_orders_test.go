package ibkr

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestConnectorOrderFlow tests the complete order flow through the Connector
func TestConnectorOrderFlow(t *testing.T) {
	// Skip if not running integration tests
	if testing.Short() {
		t.Skip("Skipping Gateway integration test")
	}
	skipIfLiveTrading(t)
	if os.Getenv("IBKR_RUN_ORDER_FLOW") != "1" {
		t.Skip("Skipping order flow integration test (set IBKR_RUN_ORDER_FLOW=1 to enable)")
	}

	ctx := context.Background()

	// Setup connector with known working configuration
	config := &ConnectorConfig{
		ServiceName:       "test-orders",
		PreferredClientID: 10, // Use different client ID for order tests
		PoolConfig: &PoolConfig{
			ClientIDs:        []int{10},
			MaxLeaseTime:     30 * time.Minute,
			HeartbeatTimeout: 2 * time.Minute,
			MonitorInterval:  10 * time.Second,
			BaseConfig: &ConnectionConfig{
				Host:     "127.0.0.1",
				Port:     4001,
				ClientID: 10,
			},
		},
	}

	connector := NewConnector(config)

	// Start connector
	t.Log("Starting IBKR connector for order tests...")
	err := connector.Start(ctx)
	if err != nil {
		// If connection fails, log but don't fail - Gateway might be busy
		t.Logf("Warning: Failed to start connector: %v", err)
		t.Skip("Cannot connect to Gateway, skipping order tests")
	}
	defer connector.Stop()

	// Wait for connection to stabilize
	time.Sleep(3 * time.Second)

	// Check if really connected
	if !connector.IsConnected() {
		t.Skip("Not connected to Gateway, skipping order tests")
	}

	t.Log("✅ Connected to IBKR Gateway")

	// Test 1: Place a limit order with very low price to avoid fill
	t.Run("PlaceLimitOrder", func(t *testing.T) {
		contract := &Contract{
			Symbol:   "AAPL",
			SecType:  "STK",
			Exchange: "SMART",
			Currency: "USD",
		}

		order := &RawOrder{
			Action:     "BUY",
			TotalQty:   1,
			OrderType:  "LMT",
			LmtPrice:   50.00, // Very low price for AAPL
			TIF:        "DAY",
			OrderRef:   fmt.Sprintf("test_limit_%d", time.Now().Unix()),
			OutsideRth: false,
		}

		t.Logf("Placing limit order: %s %d %s @ %.2f",
			order.Action, order.TotalQty, contract.Symbol, order.LmtPrice)

		err := connector.SubmitOrder(contract, order)
		if err != nil {
			// Check if it's a known error
			if strings.Contains(err.Error(), "not connected") {
				t.Skip("Connection lost during test")
			}
			t.Fatalf("Failed to submit order: %v", err)
		}

		t.Log("✅ Order submitted successfully")

		// Wait for order to be acknowledged
		time.Sleep(3 * time.Second)

		// Verify order is tracked
		connector.orderMu.RLock()
		trackedOrder, exists := connector.openOrders[order.OrderRef]
		connector.orderMu.RUnlock()

		if !exists {
			t.Error("Order not found in tracking after submission")
		} else {
			t.Logf("Order tracked with status: %s", trackedOrder.Status)
		}
	})

	// Test 2: Place and cancel an order
	t.Run("PlaceAndCancelOrder", func(t *testing.T) {
		contract := &Contract{
			Symbol:   "SPY",
			SecType:  "STK",
			Exchange: "SMART",
			Currency: "USD",
		}

		order := &RawOrder{
			Action:     "SELL",
			TotalQty:   1,
			OrderType:  "LMT",
			LmtPrice:   600.00, // High price for SPY to avoid fill
			TIF:        "GTC",
			OrderRef:   fmt.Sprintf("test_cancel_%d", time.Now().Unix()),
			OutsideRth: true,
		}

		t.Logf("Placing order to cancel: %s %d %s @ %.2f",
			order.Action, order.TotalQty, contract.Symbol, order.LmtPrice)

		// Submit the order
		err := connector.SubmitOrder(contract, order)
		if err != nil {
			t.Fatalf("Failed to submit order for cancellation test: %v", err)
		}

		// Get the order ID from the connection
		connector.mu.RLock()
		conn := connector.conn
		connector.mu.RUnlock()

		if conn == nil {
			t.Fatal("No connection available")
		}

		// Wait for order to be processed
		time.Sleep(2 * time.Second)

		// Find the order ID (would be set by PlaceOrder)
		var orderID int
		conn.ordersMu.RLock()
		for id, o := range conn.openOrders {
			if o.OrderRef == order.OrderRef {
				orderID = id
				break
			}
		}
		conn.ordersMu.RUnlock()

		if orderID == 0 {
			t.Log("Warning: Could not find order ID, may have been filled or rejected")
			return
		}

		t.Logf("Cancelling order ID: %d", orderID)

		// Cancel the order
		err = connector.CancelOrder(orderID)
		if err != nil {
			t.Errorf("Failed to cancel order: %v", err)
		} else {
			t.Log("✅ Cancel request sent successfully")
		}

		// Wait for cancellation to process
		time.Sleep(3 * time.Second)

		// Verify order is no longer in open orders
		conn.ordersMu.RLock()
		_, stillExists := conn.openOrders[orderID]
		conn.ordersMu.RUnlock()

		if stillExists {
			t.Error("Order still in open orders after cancel request")
		} else {
			t.Log("✅ Order removed from open orders")
		}
	})

	// Test 3: Test with options contract
	t.Run("PlaceOptionsOrder", func(t *testing.T) {
		// Get next Friday expiry
		now := time.Now()
		daysUntilFriday := (5 - int(now.Weekday()) + 7) % 7
		if daysUntilFriday == 0 {
			daysUntilFriday = 7
		}
		expiry := now.AddDate(0, 0, daysUntilFriday)
		expiryStr := expiry.Format("20060102")

		contract := &Contract{
			Symbol:   "SPY",
			SecType:  "OPT",
			Exchange: "SMART",
			Currency: "USD",
			Strike:   400.0,
			Right:    "C",
			Expiry:   expiryStr,
		}

		order := &RawOrder{
			Action:     "BUY",
			TotalQty:   1,
			OrderType:  "LMT",
			LmtPrice:   0.01, // Very low price
			TIF:        "DAY",
			OrderRef:   fmt.Sprintf("test_opt_%d", time.Now().Unix()),
			OutsideRth: false,
		}

		t.Logf("Placing options order: %s %d %s %s %.0f @ %.2f",
			order.Action, order.TotalQty, contract.Symbol,
			contract.Right, contract.Strike, order.LmtPrice)

		err := connector.SubmitOrder(contract, order)
		if err != nil {
			// Options might require additional market data subscriptions
			if strings.Contains(err.Error(), "market data") {
				t.Log("Options order rejected - may need market data subscription")
				return
			}
			t.Logf("Options order error (expected): %v", err)
		} else {
			t.Log("✅ Options order submitted")
		}
	})

	// Test 4: Request all open orders
	t.Run("RequestOpenOrders", func(t *testing.T) {
		connector.mu.RLock()
		conn := connector.conn
		connector.mu.RUnlock()

		if conn == nil {
			t.Skip("No connection available")
		}

		t.Log("Requesting all open orders...")
		err := conn.RequestOpenOrders()
		if err != nil {
			t.Errorf("Failed to request open orders: %v", err)
			return
		}

		// Wait for response
		time.Sleep(2 * time.Second)

		// Check how many open orders we have
		conn.ordersMu.RLock()
		openCount := len(conn.openOrders)
		conn.ordersMu.RUnlock()

		t.Logf("Found %d open orders in connection tracking", openCount)
	})
}

// TestOrderMessageEncoding tests the order message encoding
func TestOrderMessageEncoding(t *testing.T) {
	conn := &Connection{
		nextOrderID: 100,
		account:     "DU123456",
		openOrders:  make(map[int]*IBKROrder),
	}

	order := &IBKROrder{
		OrderID:    100,
		Symbol:     "MSFT",
		SecType:    "STK",
		Exchange:   "SMART",
		Currency:   "USD",
		Action:     "BUY",
		TotalQty:   50,
		OrderType:  "LMT",
		LmtPrice:   400.00,
		TIF:        "DAY",
		OrderRef:   "test_encode",
		OutsideRth: false,
	}

	// Test that we can create the message without errors
	fields := []interface{}{
		placeOrder,
		45, // version
		order.OrderID,
		0, // conID
		order.Symbol,
		order.SecType,
		"",  // expiry
		0.0, // strike
		"",  // right
		"",  // multiplier
		order.Exchange,
		"", // primaryExchange
		order.Currency,
	}

	msg := conn.encodeMsg(fields...)
	if len(msg) == 0 {
		t.Error("Encoded message is empty")
	}

	// Verify message contains expected content
	msgStr := string(msg)
	maxLen := 100
	if len(msgStr) < maxLen {
		maxLen = len(msgStr)
	}
	t.Logf("Encoded message (first %d chars): %q", maxLen, msgStr[:maxLen])

	if !strings.Contains(msgStr, "MSFT") {
		t.Error("Message doesn't contain symbol")
	}

	t.Logf("Encoded order message length: %d bytes", len(msg))
}

// TestOrderValidationExtended tests additional order validation scenarios
func TestOrderValidationExtended(t *testing.T) {
	tests := []struct {
		name      string
		order     *IBKROrder
		wantError bool
	}{
		{
			name: "StopLimitOrder",
			order: &IBKROrder{
				Symbol:    "QQQ",
				Action:    "SELL",
				TotalQty:  10,
				OrderType: "STP LMT",
				LmtPrice:  380.00,
				AuxPrice:  385.00, // Stop price
			},
			wantError: false,
		},
		{
			name: "MarketIfTouchedOrder",
			order: &IBKROrder{
				Symbol:    "IWM",
				Action:    "BUY",
				TotalQty:  100,
				OrderType: "MIT",
				AuxPrice:  200.00, // Touch price
			},
			wantError: false,
		},
		{
			name: "TrailingStopOrder",
			order: &IBKROrder{
				Symbol:    "TSLA",
				Action:    "SELL",
				TotalQty:  5,
				OrderType: "TRAIL",
				AuxPrice:  10.00, // Trailing amount
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOrder(tt.order)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidateOrder() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

// BenchmarkOrderPlacement benchmarks order placement
func BenchmarkOrderPlacement(b *testing.B) {
	// Create a mock connection
	conn := &Connection{
		status:      StatusConnected,
		nextOrderID: 1000,
		openOrders:  make(map[int]*IBKROrder),
		orderStatus: make(map[int]string),
		writer:      nil, // Would be set in real connection
	}

	order := &IBKROrder{
		Symbol:    "AAPL",
		Action:    "BUY",
		TotalQty:  100,
		OrderType: "LMT",
		LmtPrice:  150.00,
		TIF:       "DAY",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		order.OrderID = conn.GetNextOrderID()
		// Just test the order preparation, not actual sending
		conn.ordersMu.Lock()
		conn.openOrders[order.OrderID] = order
		conn.ordersMu.Unlock()
	}
}
