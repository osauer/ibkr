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
