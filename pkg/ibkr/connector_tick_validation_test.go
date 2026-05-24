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

func TestParseTickSize_NormalizesDecimalEncodedVolume(t *testing.T) {
	t.Parallel()

	got, ok := parseTickSize(minServerVerSizeRules, 8, "41762007966821")
	if !ok {
		t.Fatal("parseTickSize returned !ok")
	}
	if got != 41762007 {
		t.Fatalf("decimal-encoded volume = %d, want 41762007", got)
	}

	got, ok = parseTickSize(minServerVerSizeRules, 8, "166")
	if !ok {
		t.Fatal("parseTickSize small returned !ok")
	}
	if got != 166 {
		t.Fatalf("small integer volume = %d, want 166", got)
	}

	got, ok = parseTickSize(minServerVerSizeRules-1, 8, "9876543")
	if !ok {
		t.Fatal("parseTickSize legacy returned !ok")
	}
	if got != 9876543 {
		t.Fatalf("legacy volume = %d, want 9876543", got)
	}
}

// TestHandleTickSize_OpenInterest pins tick types 27 (callOpenInterest) and
// 28 (putOpenInterest) into Subscription.OpenInt. The two ticks share the
// same slot because a given option-leg subscription is for exactly one
// right; callers fetch the OI by reading the leg's MarketData.OpenInt.
//
// The zero-gamma RPC depends on this — without the parser, the field stays
// at zero and the dealer GEX integral collapses silently.
func TestHandleTickSize_OpenInterest(t *testing.T) {
	for _, tt := range []struct {
		name     string
		tickType string
	}{
		{"call_oi", "27"},
		{"put_oi", "28"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := NewConnector(&ConnectorConfig{})
			key := "SPX 20260619 5000 " + tt.name
			c.subMu.Lock()
			c.reqIDMap[11] = key
			c.subscriptions[key] = &Subscription{Symbol: key}
			c.subMu.Unlock()

			c.handleTickSize([]string{"2", "6", "11", tt.tickType, "12345"})

			c.subMu.RLock()
			sub := c.subscriptions[key]
			c.subMu.RUnlock()

			if sub.OpenInt != 12345 {
				t.Errorf("OpenInt for tick %s: want 12345, got %d", tt.tickType, sub.OpenInt)
			}
			if !sub.Observed {
				t.Errorf("Observed flag not set after OI tick %s", tt.tickType)
			}

			// Round-trip: prove the OI also surfaces via GetMarketData,
			// which is the path Phase 2 (zero-gamma) reads from. Without
			// this, a regression on the connector→MarketData copy would
			// silently zero out every leg's OI in the GEX integral.
			md := c.GetMarketData()
			if md[key] == nil {
				t.Fatalf("GetMarketData missing entry for %q", key)
			}
			if md[key].OpenInt != 12345 {
				t.Errorf("MarketData.OpenInt: want 12345, got %d", md[key].OpenInt)
			}
		})
	}
}

// TestHandleTickPrice_WeekRangeCapture pins the new tick types added in
// v0.12 for scan-row enrichment. 13W/26W/52W highs/lows arrive as
// standard tickPrice messages (msg ID 1) with tick types 15-20; capture
// is load-bearing for the scanner's 52w column. A previous build silently
// dropped these into the default branch, so an absent test made the
// regression invisible.
func TestHandleTickPrice_WeekRangeCapture(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[42] = "AAPL"
	c.subscriptions["AAPL"] = &Subscription{Symbol: "AAPL"}
	c.subMu.Unlock()

	cases := []struct {
		tickType string
		price    string
		field    func(*Subscription) float64
		want     float64
		name     string
	}{
		{"15", "150.10", func(s *Subscription) float64 { return s.Week13Low }, 150.10, "13w low"},
		{"16", "240.55", func(s *Subscription) float64 { return s.Week13High }, 240.55, "13w high"},
		{"17", "140.00", func(s *Subscription) float64 { return s.Week26Low }, 140.00, "26w low"},
		{"18", "245.00", func(s *Subscription) float64 { return s.Week26High }, 245.00, "26w high"},
		{"19", "120.00", func(s *Subscription) float64 { return s.Week52Low }, 120.00, "52w low"},
		{"20", "260.50", func(s *Subscription) float64 { return s.Week52High }, 260.50, "52w high"},
	}
	for _, tc := range cases {
		c.handleTickPrice([]string{"1", "2", "42", tc.tickType, tc.price})
	}
	c.subMu.RLock()
	sub := c.subscriptions["AAPL"]
	c.subMu.RUnlock()
	for _, tc := range cases {
		if got := tc.field(sub); got != tc.want {
			t.Errorf("%s: got %.2f, want %.2f", tc.name, got, tc.want)
		}
	}
}

// TestHandleTickGeneric_IVRoutesToSubscription pins the v0.12 change that
// also writes tick 106 (averaged option implied vol) into the per-symbol
// subscription struct — not just into the optIV map. Without this, the
// scan-row IV column would stay blank even when the gateway delivers the
// tick, because the enrichment path reads from GetMarketData() which is
// derived from subscriptions.
func TestHandleTickGeneric_IVRoutesToSubscription(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[7] = "AAPL"
	c.subscriptions["AAPL"] = &Subscription{Symbol: "AAPL"}
	c.subMu.Unlock()

	// Fraction-form: 0.234 → IV = 0.234 (23.4%)
	c.handleTickGeneric([]string{"45", "1", "7", "106", "0.234"})
	c.subMu.RLock()
	if got := c.subscriptions["AAPL"].IV; got != 0.234 {
		t.Errorf("fraction-form IV: got %.4f, want 0.234", got)
	}
	c.subMu.RUnlock()

	// Percent-form: 23.4 should normalize to 0.234.
	c.subMu.Lock()
	c.reqIDMap[8] = "MSFT"
	c.subscriptions["MSFT"] = &Subscription{Symbol: "MSFT"}
	c.subMu.Unlock()
	c.handleTickGeneric([]string{"45", "1", "8", "106", "23.4"})
	c.subMu.RLock()
	got := c.subscriptions["MSFT"].IV
	c.subMu.RUnlock()
	if got < 0.233 || got > 0.235 {
		t.Errorf("percent-form IV normalization: got %.4f, want ~0.234", got)
	}
}

// TestGetMarketData_SurfacesWeekRangeAndIV pins the daemon-facing read
// path: scan-row enrichment polls GetMarketData() and copies into the
// row. If a future refactor accidentally drops the new fields from the
// MarketData materialisation, this test catches it before the column
// silently goes blank.
func TestGetMarketData_SurfacesWeekRangeAndIV(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.subscriptions["AAPL"] = &Subscription{
		Symbol:     "AAPL",
		Week52Low:  120.00,
		Week52High: 260.50,
		Week26Low:  140.00,
		Week26High: 245.00,
		Week13Low:  150.10,
		Week13High: 240.55,
		IV:         0.234,
	}
	c.subMu.Unlock()

	md := c.GetMarketData()
	got, ok := md["AAPL"]
	if !ok {
		t.Fatal("AAPL not in GetMarketData() output")
	}
	if got.Week52Low != 120.00 || got.Week52High != 260.50 {
		t.Errorf("52w range: got %.2f..%.2f, want 120.00..260.50", got.Week52Low, got.Week52High)
	}
	if got.Week26Low != 140.00 || got.Week26High != 245.00 {
		t.Errorf("26w range: got %.2f..%.2f, want 140.00..245.00", got.Week26Low, got.Week26High)
	}
	if got.Week13Low != 150.10 || got.Week13High != 240.55 {
		t.Errorf("13w range: got %.2f..%.2f, want 150.10..240.55", got.Week13Low, got.Week13High)
	}
	if got.IV != 0.234 {
		t.Errorf("IV: got %.4f, want 0.234", got.IV)
	}
}
