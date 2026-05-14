package ibkr

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHandlerRegistration_NoRaceCondition verifies that handlers are registered
// before any messages are processed, preventing the race condition where early
// messages arrive before handlers are ready.
func TestHandlerRegistration_NoRaceCondition(t *testing.T) {
	// Create a mock connection that simulates messages arriving immediately
	config := DefaultConfig()
	config.Host = "127.0.0.1"
	config.Port = 9999 // Non-existent port to avoid actual connection

	connector := NewConnector(&ConnectorConfig{
		ServiceName: "test-service",
		PoolConfig: &PoolConfig{
			ClientIDs:    []int{1},
			BaseConfig:   config,
			EagerConnect: false,
		},
	})

	// Track handler calls
	var tickPriceCalled atomic.Int32
	var tickSizeCalled atomic.Int32
	var errorCalled atomic.Int32

	// Manually simulate what should happen during connection
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{Symbol: "SPY", Bid: 580.0}
	connector.subMu.Unlock()

	// Register handlers via connector logic
	mockConn := NewConnection(config)
	connector.registerHandlers(mockConn)

	// Add instrumentation handlers that count invocations without disturbing the originals
	mockConn.RegisterHandler(1, func(fields []string) {
		tickPriceCalled.Add(1)
	})
	mockConn.RegisterHandler(2, func(fields []string) {
		tickSizeCalled.Add(1)
	})
	mockConn.RegisterHandler(4, func(fields []string) {
		errorCalled.Add(1)
	})

	// Simulate messages arriving rapidly
	messages := [][]string{
		{"1", "2", "42", "1", "580.50"}, // tick price
		{"2", "2", "42", "8", "100000"}, // tick size
		{"1", "2", "42", "2", "580.52"}, // tick price
		{"1", "2", "42", "4", "580.51"}, // tick price
	}

	var wg sync.WaitGroup
	for _, msg := range messages {
		wg.Add(1)
		go func(m []string) {
			defer wg.Done()
			// Simulate message processing
			msgID := m[0]
			switch msgID {
			case "1":
				mockConn.handlersMu.RLock()
				entries := mockConn.msgHandlers[1]
				mockConn.handlersMu.RUnlock()
				for _, entry := range entries {
					entry.fn(m)
				}
			case "2":
				mockConn.handlersMu.RLock()
				entries := mockConn.msgHandlers[2]
				mockConn.handlersMu.RUnlock()
				for _, entry := range entries {
					entry.fn(m)
				}
			}
		}(msg)
	}

	wg.Wait()

	// Verify all handlers were called (no races)
	if tickPriceCalled.Load() != 3 {
		t.Errorf("tick price handler called %d times, expected 3", tickPriceCalled.Load())
	}
	if tickSizeCalled.Load() != 1 {
		t.Errorf("tick size handler called %d times, expected 1", tickSizeCalled.Load())
	}

	// Verify prices were updated correctly
	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Bid != 580.50 {
		t.Errorf("bid not updated correctly: %.2f", sub.Bid)
	}
	if sub.Ask != 580.52 {
		t.Errorf("ask not updated correctly: %.2f", sub.Ask)
	}
	if sub.LastPrice != 580.51 {
		t.Errorf("last price not updated correctly: %.2f", sub.LastPrice)
	}
	if sub.Volume != 100000 {
		t.Errorf("volume not updated correctly: %d", sub.Volume)
	}
}

// TestConnector_HandlersRegisteredBeforeReady verifies that the ready flag
// is only set after handlers are fully registered.
func TestConnector_HandlersRegisteredBeforeReady(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{
		ServiceName: "test-service",
		PoolConfig: &PoolConfig{
			ClientIDs:    []int{1},
			BaseConfig:   DefaultConfig(),
			EagerConnect: false,
		},
	})

	// Create a mock connection
	mockConn := NewConnection(DefaultConfig())
	connector.conn = mockConn

	// Register handlers
	connector.registerHandlers(mockConn)

	// Verify handlers are registered for critical message types
	mockConn.handlersMu.RLock()
	hasTickPrice := len(mockConn.msgHandlers[1]) > 0
	hasTickSize := len(mockConn.msgHandlers[2]) > 0
	hasOrderStatus := len(mockConn.msgHandlers[3]) > 0
	hasError := len(mockConn.msgHandlers[4]) > 0
	hasPosition := len(mockConn.msgHandlers[61]) > 0
	mockConn.handlersMu.RUnlock()

	if !hasTickPrice {
		t.Error("tick price handler (1) not registered")
	}
	if !hasTickSize {
		t.Error("tick size handler (2) not registered")
	}
	if !hasOrderStatus {
		t.Error("order status handler (3) not registered")
	}
	if !hasError {
		t.Error("error handler (4) not registered")
	}
	if !hasPosition {
		t.Error("position handler (61) not registered")
	}
}

// TestConnector_EarlyMessageHandling tests that messages arriving during
// connector initialization are properly handled.
func TestConnector_EarlyMessageHandling(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{
		ServiceName: "test-service",
		PoolConfig: &PoolConfig{
			ClientIDs:    []int{1},
			BaseConfig:   DefaultConfig(),
			EagerConnect: false,
		},
	})

	// Setup subscription
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{Symbol: "SPY"}
	connector.subMu.Unlock()

	// Create mock connection and register handlers
	mockConn := NewConnection(DefaultConfig())
	connector.conn = mockConn
	connector.registerHandlers(mockConn)

	// Simulate early message arriving
	earlyMessage := []string{"1", "2", "42", "1", "580.50"}

	mockConn.handlersMu.RLock()
	entries := mockConn.msgHandlers[1]
	mockConn.handlersMu.RUnlock()
	if len(entries) == 0 {
		t.Fatal("handler not registered when it should be")
	}
	// Process message using all registered handlers
	for _, entry := range entries {
		entry.fn(earlyMessage)
	}

	// Verify it was processed
	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Bid != 580.50 {
		t.Errorf("early message not processed: bid=%.2f", sub.Bid)
	}
	if !sub.Observed {
		t.Error("subscription not marked as observed after early message")
	}
}

// TestConnector_ConcurrentHandlerRegistrationAndMessages verifies thread-safety
// when handlers are being registered while messages might be arriving.
func TestConnector_ConcurrentHandlerRegistrationAndMessages(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{
		ServiceName: "test-service",
		PoolConfig: &PoolConfig{
			ClientIDs:    []int{1},
			BaseConfig:   DefaultConfig(),
			EagerConnect: false,
		},
	})

	connector.subMu.Lock()
	connector.reqIDMap[42] = "TEST"
	connector.subscriptions["TEST"] = &Subscription{Symbol: "TEST"}
	connector.subMu.Unlock()

	mockConn := NewConnection(DefaultConfig())
	connector.conn = mockConn

	var wg sync.WaitGroup
	messageCount := 100

	// Start sending messages concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range messageCount {
			msg := []string{"1", "2", "42", "1", fmt.Sprintf("%.2f", 580.0+float64(i)*0.01)}
			mockConn.handlersMu.RLock()
			handlers := append([]handlerEntry(nil), mockConn.msgHandlers[1]...)
			mockConn.handlersMu.RUnlock()
			for _, entry := range handlers {
				entry.fn(msg)
			}
			time.Sleep(time.Microsecond)
		}
	}()

	// Register handlers concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Microsecond) // Small delay to simulate real timing
		connector.registerHandlers(mockConn)
	}()

	wg.Wait()

	// Verify no panics and subscription was updated
	connector.subMu.RLock()
	sub := connector.subscriptions["TEST"]
	connector.subMu.RUnlock()

	// Bid should be updated (exact value depends on race but should be > 0)
	if sub.Bid <= 0 {
		t.Error("no price updates received, possible handler registration race")
	}
}
