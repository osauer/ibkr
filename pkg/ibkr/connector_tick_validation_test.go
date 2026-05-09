package ibkr

import (
	"testing"
	"time"
)

// TestHandleTickPrice_ZeroValueRejection verifies that zero prices do not overwrite valid prices.
// This test addresses the issue where IBKR sends tick updates with price=0.0 (indicating
// no quote available) which would incorrectly overwrite previously received valid prices.
func TestHandleTickPrice_ZeroValueRejection(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:    "SPY",
		Bid:       580.50,
		Ask:       580.52,
		LastPrice: 580.51,
	}
	connector.subMu.Unlock()

	// Simulate IBKR sending a zero bid price (no quote available)
	connector.handleTickPrice([]string{"1", "2", "42", "1", "0.0"})

	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	// Zero price should NOT overwrite the previous valid bid
	if sub.Bid != 580.50 {
		t.Errorf("zero bid price overwrote valid price: expected 580.50, got %.2f", sub.Bid)
	}

	// Simulate zero ask price
	connector.handleTickPrice([]string{"1", "2", "42", "2", "0"})
	connector.subMu.RLock()
	sub = connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Ask != 580.52 {
		t.Errorf("zero ask price overwrote valid price: expected 580.52, got %.2f", sub.Ask)
	}

	// Simulate zero last price
	connector.handleTickPrice([]string{"1", "2", "42", "4", "0.00"})
	connector.subMu.RLock()
	sub = connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.LastPrice != 580.51 {
		t.Errorf("zero last price overwrote valid price: expected 580.51, got %.2f", sub.LastPrice)
	}
}

// TestHandleTickPrice_ValidPriceUpdate verifies that valid non-zero prices ARE updated correctly.
func TestHandleTickPrice_ValidPriceUpdate(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:    "SPY",
		Bid:       580.50,
		Ask:       580.52,
		LastPrice: 580.51,
	}
	connector.subMu.Unlock()

	// Update bid with valid price
	connector.handleTickPrice([]string{"1", "2", "42", "1", "581.00"})
	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Bid != 581.00 {
		t.Errorf("valid bid price not updated: expected 581.00, got %.2f", sub.Bid)
	}

	// Update ask with valid price
	connector.handleTickPrice([]string{"1", "2", "42", "2", "581.02"})
	connector.subMu.RLock()
	sub = connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Ask != 581.02 {
		t.Errorf("valid ask price not updated: expected 581.02, got %.2f", sub.Ask)
	}

	// Update last with valid price
	connector.handleTickPrice([]string{"1", "2", "42", "4", "581.01"})
	connector.subMu.RLock()
	sub = connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.LastPrice != 581.01 {
		t.Errorf("valid last price not updated: expected 581.01, got %.2f", sub.LastPrice)
	}
}

// TestHandleTickPrice_InvalidParsing tests that malformed price strings are handled gracefully.
func TestHandleTickPrice_InvalidParsing(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:    "SPY",
		Bid:       580.50,
		Ask:       580.52,
		LastPrice: 580.51,
	}
	connector.subMu.Unlock()

	testCases := []struct {
		name      string
		fields    []string
		checkFunc func(*Subscription) error
	}{
		{
			name:   "invalid price string",
			fields: []string{"1", "2", "42", "1", "not-a-number"},
			checkFunc: func(sub *Subscription) error {
				// Invalid parse should result in 0.0, which should be rejected
				if sub.Bid != 580.50 {
					t.Errorf("invalid price string changed bid: expected 580.50, got %.2f", sub.Bid)
				}
				return nil
			},
		},
		{
			name:   "empty price field",
			fields: []string{"1", "2", "42", "2", ""},
			checkFunc: func(sub *Subscription) error {
				if sub.Ask != 580.52 {
					t.Errorf("empty price field changed ask: expected 580.52, got %.2f", sub.Ask)
				}
				return nil
			},
		},
		{
			name:   "invalid reqID",
			fields: []string{"1", "2", "not-a-number", "1", "590.00"},
			checkFunc: func(sub *Subscription) error {
				// Invalid reqID should prevent any update
				if sub.Bid != 580.50 {
					t.Errorf("invalid reqID caused update: expected 580.50, got %.2f", sub.Bid)
				}
				return nil
			},
		},
		{
			name:   "invalid tickType",
			fields: []string{"1", "2", "42", "invalid", "590.00"},
			checkFunc: func(sub *Subscription) error {
				// Invalid tickType should prevent any update (tickType would be 0, not 1/2/4)
				if sub.Bid != 580.50 || sub.Ask != 580.52 || sub.LastPrice != 580.51 {
					t.Errorf("invalid tickType caused updates")
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			connector.handleTickPrice(tc.fields)
			connector.subMu.RLock()
			sub := connector.subscriptions["SPY"]
			connector.subMu.RUnlock()

			if err := tc.checkFunc(sub); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestHandleTickPrice_ObservedFlag verifies that the Observed flag is set appropriately.
func TestHandleTickPrice_ObservedFlag(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:   "SPY",
		Observed: false,
	}
	connector.subMu.Unlock()

	// Send valid price
	connector.handleTickPrice([]string{"1", "2", "42", "1", "580.50"})

	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if !sub.Observed {
		t.Error("Observed flag not set after receiving valid tick")
	}
}

// TestHandleTickPrice_LastTimeUpdate verifies LastTime is updated even with rejected prices.
func TestHandleTickPrice_LastTimeUpdate(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	initialTime := time.Now().Add(-1 * time.Hour)
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:   "SPY",
		Bid:      580.50,
		LastTime: initialTime,
	}
	connector.subMu.Unlock()

	// Send zero price (should be rejected but LastTime should update)
	time.Sleep(10 * time.Millisecond)
	connector.handleTickPrice([]string{"1", "2", "42", "1", "0.0"})

	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	// Price should not change
	if sub.Bid != 580.50 {
		t.Errorf("bid changed unexpectedly: %.2f", sub.Bid)
	}

	// But LastTime should be updated to show we received a tick
	if !sub.LastTime.After(initialTime) {
		t.Error("LastTime not updated after receiving tick")
	}
}

// TestHandleTickPrice_NegativePrice verifies negative prices are also validated.
func TestHandleTickPrice_NegativePrice(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:    "SPY",
		Bid:       580.50,
		Ask:       580.52,
		LastPrice: 580.51,
	}
	connector.subMu.Unlock()

	// Negative prices should be rejected
	connector.handleTickPrice([]string{"1", "2", "42", "1", "-1.0"})
	connector.handleTickPrice([]string{"1", "2", "42", "2", "-0.5"})
	connector.handleTickPrice([]string{"1", "2", "42", "4", "-100"})

	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Bid != 580.50 {
		t.Errorf("negative bid overwrote price: %.2f", sub.Bid)
	}
	if sub.Ask != 580.52 {
		t.Errorf("negative ask overwrote price: %.2f", sub.Ask)
	}
	if sub.LastPrice != 580.51 {
		t.Errorf("negative last overwrote price: %.2f", sub.LastPrice)
	}
}

// TestHandleTickPrice_VerySmallPrice tests handling of very small but valid prices (like some penny stocks).
func TestHandleTickPrice_VerySmallPrice(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "PENNY"
	connector.subscriptions["PENNY"] = &Subscription{Symbol: "PENNY"}
	connector.subMu.Unlock()

	// Very small but valid price (0.0001 for penny stocks)
	connector.handleTickPrice([]string{"1", "2", "42", "1", "0.0001"})

	connector.subMu.RLock()
	sub := connector.subscriptions["PENNY"]
	connector.subMu.RUnlock()

	if sub.Bid != 0.0001 {
		t.Errorf("small valid price rejected: expected 0.0001, got %.4f", sub.Bid)
	}
}

// TestHandleTickSize_DispatchesByTickType verifies bid_size (0), ask_size (3),
// and volume (8) ticks land on the right Subscription field. Other tick types
// (e.g. 5=last_size) are intentionally dropped.
func TestHandleTickSize_DispatchesByTickType(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[7] = "AAPL"
	c.subscriptions["AAPL"] = &Subscription{Symbol: "AAPL"}
	c.subMu.Unlock()

	// Fields format: [msgID, version, reqID, tickType, size]
	c.handleTickSize([]string{"2", "6", "7", "0", "1500"})    // bid_size
	c.handleTickSize([]string{"2", "6", "7", "3", "2200"})    // ask_size
	c.handleTickSize([]string{"2", "6", "7", "8", "9876543"}) // volume
	c.handleTickSize([]string{"2", "6", "7", "5", "999"})     // last_size — ignored

	c.subMu.RLock()
	sub := c.subscriptions["AAPL"]
	c.subMu.RUnlock()

	if sub.BidSize != 1500 {
		t.Errorf("BidSize: want 1500, got %d", sub.BidSize)
	}
	if sub.AskSize != 2200 {
		t.Errorf("AskSize: want 2200, got %d", sub.AskSize)
	}
	if sub.Volume != 9876543 {
		t.Errorf("Volume: want 9876543, got %d", sub.Volume)
	}
}
