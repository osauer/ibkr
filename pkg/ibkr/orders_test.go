package ibkr

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"
)

// TestOrderPlacement tests placing and cancelling orders with live Gateway
func TestOrderPlacement(t *testing.T) {
	// Skip if not running integration tests
	if testing.Short() {
		t.Skip("Skipping integration test")
	}
	skipIfLiveTrading(t)

	// Create connection
	config := &ConnectionConfig{
		Host:     "127.0.0.1",
		Port:     4001, // Paper trading port
		ClientID: 5,    // Use unique client ID for testing
	}

	conn := NewConnection(config)

	// Connect to Gateway
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Log("Connecting to IBKR Gateway...")
	err := conn.Connect(ctx)
	if err != nil {
		t.Skipf("Failed to connect: %v (skipping)", err)
	}
	defer conn.Disconnect()

	// Wait for connection to stabilize
	time.Sleep(2 * time.Second)

	// Test 1: Place a limit order
	t.Run("PlaceLimitOrder", func(t *testing.T) {
		order := &IBKROrder{
			Symbol:    "AAPL",
			SecType:   "STK",
			Exchange:  "SMART",
			Currency:  "USD",
			Action:    "BUY",
			TotalQty:  1,
			OrderType: "LMT",
			LmtPrice:  100.00, // Low price to avoid fill
			TIF:       "DAY",
			OrderRef:  fmt.Sprintf("test_%d", time.Now().Unix()),
		}

		t.Logf("Placing order: %s %d %s @ %.2f", order.Action, order.TotalQty, order.Symbol, order.LmtPrice)

		err := conn.PlaceOrder(order)
		if err != nil {
			t.Fatalf("Failed to place order: %v", err)
		}

		t.Logf("Order placed with ID: %d", order.OrderID)

		// Wait for order status
		time.Sleep(3 * time.Second)

		// Check if order is in tracking
		conn.ordersMu.RLock()
		trackedOrder, exists := conn.openOrders[order.OrderID]
		conn.ordersMu.RUnlock()

		if exists {
			t.Logf("Order status: %s, Filled: %d, Remaining: %d",
				trackedOrder.Status, trackedOrder.Filled, trackedOrder.Remaining)
		}

		// Cancel the order
		t.Log("Cancelling order...")
		err = conn.CancelOrder(order.OrderID)
		if err != nil {
			t.Errorf("Failed to cancel order: %v", err)
		}

		// Wait for cancellation
		time.Sleep(2 * time.Second)

		// Verify order is cancelled
		conn.ordersMu.RLock()
		_, stillExists := conn.openOrders[order.OrderID]
		conn.ordersMu.RUnlock()

		if stillExists {
			t.Error("Order still exists after cancellation")
		} else {
			t.Log("Order successfully cancelled")
		}
	})

	// Test 2: Place a market order (be careful!)
	t.Run("PlaceMarketOrder", func(t *testing.T) {
		t.Skip("Skipping market order test to avoid unwanted fills")

		order := &IBKROrder{
			Symbol:    "SPY",
			SecType:   "STK",
			Exchange:  "SMART",
			Currency:  "USD",
			Action:    "BUY",
			TotalQty:  1,
			OrderType: "MKT",
			TIF:       "DAY",
			OrderRef:  fmt.Sprintf("test_mkt_%d", time.Now().Unix()),
		}

		err := conn.PlaceOrder(order)
		if err != nil {
			t.Fatalf("Failed to place market order: %v", err)
		}

		t.Logf("Market order placed with ID: %d", order.OrderID)

		// Wait for fill
		time.Sleep(5 * time.Second)

		conn.ordersMu.RLock()
		if trackedOrder, exists := conn.openOrders[order.OrderID]; exists {
			t.Logf("Order status: %s, Filled: %d, AvgPrice: %.2f",
				trackedOrder.Status, trackedOrder.Filled, trackedOrder.AvgFillPrice)
		}
		conn.ordersMu.RUnlock()
	})

	// Test 3: Request open orders
	t.Run("RequestOpenOrders", func(t *testing.T) {
		t.Log("Requesting open orders...")
		err := conn.RequestOpenOrders()
		if err != nil {
			t.Errorf("Failed to request open orders: %v", err)
		}

		// Wait for response
		time.Sleep(2 * time.Second)

		conn.ordersMu.RLock()
		openCount := len(conn.openOrders)
		conn.ordersMu.RUnlock()

		t.Logf("Found %d open orders", openCount)
	})
}

// TestOrderValidation tests order validation logic
func TestOrderValidation(t *testing.T) {
	tests := []struct {
		name      string
		order     *IBKROrder
		wantError bool
		errorMsg  string
	}{
		{
			name:      "NilOrder",
			order:     nil,
			wantError: true,
			errorMsg:  "order is nil",
		},
		{
			name: "MissingSymbol",
			order: &IBKROrder{
				Action:    "BUY",
				TotalQty:  100,
				OrderType: "LMT",
				LmtPrice:  50.00,
			},
			wantError: true,
			errorMsg:  "symbol is required",
		},
		{
			name: "InvalidQuantity",
			order: &IBKROrder{
				Symbol:    "AAPL",
				Action:    "BUY",
				TotalQty:  0,
				OrderType: "LMT",
				LmtPrice:  150.00,
			},
			wantError: true,
			errorMsg:  "quantity must be positive",
		},
		{
			name: "InvalidAction",
			order: &IBKROrder{
				Symbol:    "AAPL",
				Action:    "HOLD",
				TotalQty:  100,
				OrderType: "LMT",
				LmtPrice:  150.00,
			},
			wantError: true,
			errorMsg:  "action must be BUY or SELL",
		},
		{
			name: "MissingLimitPrice",
			order: &IBKROrder{
				Symbol:    "AAPL",
				Action:    "BUY",
				TotalQty:  100,
				OrderType: "LMT",
			},
			wantError: true,
			errorMsg:  "limit price required for limit orders",
		},
		{
			name: "MissingStopPrice",
			order: &IBKROrder{
				Symbol:    "AAPL",
				Action:    "SELL",
				TotalQty:  100,
				OrderType: "STP",
			},
			wantError: true,
			errorMsg:  "stop price required for stop orders",
		},
		{
			name: "ValidLimitOrder",
			order: &IBKROrder{
				Symbol:    "AAPL",
				Action:    "BUY",
				TotalQty:  100,
				OrderType: "LMT",
				LmtPrice:  150.00,
			},
			wantError: false,
		},
		{
			name: "ValidMarketOrder",
			order: &IBKROrder{
				Symbol:    "SPY",
				Action:    "SELL",
				TotalQty:  50,
				OrderType: "MKT",
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOrder(tt.order)

			if tt.wantError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if err.Error() != tt.errorMsg {
					t.Errorf("Expected error '%s', got '%s'", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				// Check that TIF was defaulted
				if tt.order != nil && tt.order.TIF == "" {
					t.Error("TIF should have been defaulted to DAY")
				}
			}
		})
	}
}

// TestOrderManager tests the order manager functionality
func TestOrderManager(t *testing.T) {
	om := NewOrderManager()

	// Test order ID generation
	t.Run("OrderIDGeneration", func(t *testing.T) {
		id1 := om.GetNextOrderID()
		id2 := om.GetNextOrderID()
		id3 := om.GetNextOrderID()

		if id2 != id1+1 || id3 != id2+1 {
			t.Errorf("Order IDs not sequential: %d, %d, %d", id1, id2, id3)
		}
	})

	// Test setting next order ID
	t.Run("SetNextOrderID", func(t *testing.T) {
		om.SetNextOrderID(100)
		nextID := om.GetNextOrderID()
		if nextID != 100 {
			t.Errorf("Expected next ID 100, got %d", nextID)
		}

		// Should not go backwards
		om.SetNextOrderID(50)
		nextID = om.GetNextOrderID()
		if nextID != 101 {
			t.Errorf("Expected next ID 101, got %d", nextID)
		}
	})

	// Test order tracking
	t.Run("OrderTracking", func(t *testing.T) {
		order := &IBKROrder{
			OrderID:   200,
			Symbol:    "TSLA",
			Action:    "BUY",
			TotalQty:  10,
			OrderType: "LMT",
			LmtPrice:  250.00,
			OrderRef:  "test_ref_123",
			Status:    "PendingSubmit",
		}

		om.AddOrder(order)

		// Get by ID
		retrieved, exists := om.GetOrder(200)
		if !exists {
			t.Error("Order not found by ID")
		}
		if retrieved.Symbol != "TSLA" {
			t.Errorf("Retrieved wrong order: %v", retrieved)
		}

		// Get by reference
		retrieved, exists = om.GetOrderByRef("test_ref_123")
		if !exists {
			t.Error("Order not found by reference")
		}
		if retrieved.OrderID != 200 {
			t.Errorf("Retrieved wrong order by ref: %v", retrieved)
		}
	})

	// Test order status updates
	t.Run("StatusUpdates", func(t *testing.T) {
		order := &IBKROrder{
			OrderID: 300,
			Symbol:  "GOOGL",
			Status:  "PendingSubmit",
		}
		om.AddOrder(order)

		// Update to submitted
		om.UpdateOrderStatus(300, "Submitted", 0, 100, 0)
		retrieved, _ := om.GetOrder(300)
		if retrieved.Status != "Submitted" {
			t.Errorf("Status not updated: %s", retrieved.Status)
		}
		if retrieved.SubmittedTime.IsZero() {
			t.Error("Submitted time not set")
		}

		// Update to filled
		om.UpdateOrderStatus(300, "Filled", 100, 0, 1500.50)
		retrieved, _ = om.GetOrder(300)
		if retrieved.Status != "Filled" {
			t.Errorf("Status not updated to Filled: %s", retrieved.Status)
		}
		if retrieved.FilledTime == nil {
			t.Error("Filled time not set")
		}
		if retrieved.AvgFillPrice != 1500.50 {
			t.Errorf("Avg fill price wrong: %.2f", retrieved.AvgFillPrice)
		}
	})

	// Test open orders retrieval
	t.Run("GetOpenOrders", func(t *testing.T) {
		om = NewOrderManager() // Reset

		// Add mix of orders
		om.AddOrder(&IBKROrder{OrderID: 1, Status: "Submitted"})
		om.AddOrder(&IBKROrder{OrderID: 2, Status: "Filled"})
		om.AddOrder(&IBKROrder{OrderID: 3, Status: "PendingSubmit"})
		om.AddOrder(&IBKROrder{OrderID: 4, Status: "Cancelled"})
		om.AddOrder(&IBKROrder{OrderID: 5, Status: "PreSubmitted"})

		openOrders := om.GetOpenOrders()
		expectedOpen := 3 // Submitted, PendingSubmit, PreSubmitted

		if len(openOrders) != expectedOpen {
			t.Errorf("Expected %d open orders, got %d", expectedOpen, len(openOrders))
			for _, o := range openOrders {
				t.Logf("  Open order %d: %s", o.OrderID, o.Status)
			}
		}
	})

	// Test fill tracking
	t.Run("FillTracking", func(t *testing.T) {
		fill1 := &OrderFill{
			OrderID: 400,
			ExecID:  "exec_1",
			Shares:  50,
			Price:   100.50,
		}
		fill2 := &OrderFill{
			OrderID: 400,
			ExecID:  "exec_2",
			Shares:  50,
			Price:   100.55,
		}

		om.AddFill(fill1)
		om.AddFill(fill2)

		fills := om.GetFills(400)
		if len(fills) != 2 {
			t.Errorf("Expected 2 fills, got %d", len(fills))
		}

		// Test last fill price update
		order := &IBKROrder{OrderID: 400}
		om.AddOrder(order)
		om.AddFill(fill2)

		retrieved, _ := om.GetOrder(400)
		if retrieved.LastFillPrice != 100.55 {
			t.Errorf("Last fill price not updated: %.2f", retrieved.LastFillPrice)
		}
	})
}

// BenchmarkOrderValidation benchmarks order validation
func BenchmarkOrderValidation(b *testing.B) {
	order := &IBKROrder{
		Symbol:    "AAPL",
		Action:    "BUY",
		TotalQty:  100,
		OrderType: "LMT",
		LmtPrice:  150.00,
	}

	for b.Loop() {
		_ = ValidateOrder(order)
	}
}

// BenchmarkOrderManagerAdd benchmarks adding orders
func BenchmarkOrderManagerAdd(b *testing.B) {
	om := NewOrderManager()

	for i := 0; b.Loop(); i++ {
		order := &IBKROrder{
			OrderID:  i,
			Symbol:   "TEST",
			Status:   "PendingSubmit",
			OrderRef: fmt.Sprintf("ref_%d", i),
		}
		om.AddOrder(order)
	}
}

// Helper function to setup test connection
func setupTestConnection(t *testing.T) *Connection {
	config := &ConnectionConfig{
		Host:     "127.0.0.1",
		Port:     4001,
		ClientID: 99, // Test client ID
	}

	conn := NewConnection(config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.Connect(ctx); err != nil {
		t.Skipf("Cannot connect to IBKR Gateway: %v", err)
	}

	return conn
}

// TestLiveOrderFlow tests the complete order flow with Gateway
func TestLiveOrderFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping live Gateway test")
	}

	conn := setupTestConnection(t)
	defer conn.Disconnect()

	// Wait for connection to stabilize
	time.Sleep(2 * time.Second)

	// Create a test order with very low limit to avoid fill
	order := &IBKROrder{
		Symbol:    "SPY",
		SecType:   "STK",
		Exchange:  "SMART",
		Currency:  "USD",
		Action:    "BUY",
		TotalQty:  1,
		OrderType: "LMT",
		LmtPrice:  1.00, // Very low price
		TIF:       "DAY",
		OrderRef:  fmt.Sprintf("flow_test_%d", time.Now().Unix()),
	}

	// Place order
	log.Printf("Placing test order...")
	err := conn.PlaceOrder(order)
	if err != nil {
		t.Fatalf("Failed to place order: %v", err)
	}

	orderID := order.OrderID
	log.Printf("Order placed with ID %d", orderID)

	// Wait for status updates
	var lastStatus string
	for range 10 {
		time.Sleep(1 * time.Second)

		conn.ordersMu.RLock()
		if tracked, exists := conn.openOrders[orderID]; exists {
			if tracked.Status != lastStatus {
				log.Printf("Order %d status: %s (filled: %d, remaining: %d)",
					orderID, tracked.Status, tracked.Filled, tracked.Remaining)
				lastStatus = tracked.Status
			}

			if tracked.Status == "Submitted" || tracked.Status == "PreSubmitted" {
				conn.ordersMu.RUnlock()
				break
			}
		}
		conn.ordersMu.RUnlock()
	}

	// Cancel the order
	log.Printf("Cancelling order %d...", orderID)
	err = conn.CancelOrder(orderID)
	if err != nil {
		t.Errorf("Failed to cancel order: %v", err)
	}

	// Wait for cancellation
	cancelled := false
	for range 10 {
		time.Sleep(1 * time.Second)

		conn.ordersMu.RLock()
		_, exists := conn.openOrders[orderID]
		conn.ordersMu.RUnlock()

		if !exists {
			log.Printf("Order %d cancelled successfully", orderID)
			cancelled = true
			break
		}
	}

	if !cancelled {
		t.Error("Order was not cancelled within timeout")
	}
}
