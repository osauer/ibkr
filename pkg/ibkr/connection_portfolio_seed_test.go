package ibkr

import (
	"sync"
	"testing"
)

// TestHandlePortfolioValueSeedsOptionContractCache verifies that a held
// OPT position arriving via msgPortfolioValue populates optionContractCache
// so the next SubscribeOption call resolves via cache-hit rather than
// paying the 5 s × N-exchange-attempts reqContractData round-trip.
//
// Regression coverage for the v0.12.1 fix: before this, only the
// connector-side stock cache (contractCache, keyed by bare symbol) was
// seeded from portfolio data, and held options had to round-trip even
// though msgPortfolioValue already carries the full Contract spec with
// ConID. Under load this blew the 30 s positions deadline before the
// Greeks tick could even be requested.
func TestHandlePortfolioValueSeedsOptionContractCache(t *testing.T) {
	conn := &Connection{
		positions:           map[string]*RawPosition{},
		positionsMu:         sync.RWMutex{},
		optionContractCache: map[string]ContractDetailsLite{},
		optionContractMu:    sync.RWMutex{},
	}

	// Field layout matches handlePortfolioValue's expected 20-field
	// msgPortfolioValue payload. Field 9 is *primaryExchange* per IB API
	// (a wire quirk; the parsed Contract stores it under Exchange).
	fields := []string{
		"7",            // msgID
		"8",            // version
		"747397667",    // 2: contract.conId
		"AMZN",         // 3: contract.symbol
		"OPT",          // 4: contract.secType
		"20260618",     // 5: contract.expiry
		"305",          // 6: contract.strike
		"C",            // 7: contract.right
		"100",          // 8: contract.multiplier
		"AMEX",         // 9: contract.primaryExchange ← appears in parsed Contract.Exchange
		"USD",          // 10: contract.currency
		"AMZN 260618C", // 11: contract.localSymbol
		"AMZN",         // 12: contract.tradingClass
		"5",            // 13: position
		"1.27",         // 14: marketPrice
		"635.00",       // 15: marketValue
		"127.30",       // 16: averageCost
		"50.00",        // 17: unrealizedPNL
		"0.00",         // 18: realizedPNL
		"DU1234567",    // 19: accountName
	}

	conn.handlePortfolioValue(fields)

	// The OPRA-style cache key is built by optionContractKey from the
	// parsed Contract fields. SubscribeOption uses the same key shape.
	// TradingClass="AMZN" matches the test fixture field 12.
	cacheKey := optionContractKey("AMZN", "AMZN", "20260618", 305, "C")
	conn.optionContractMu.RLock()
	detail, ok := conn.optionContractCache[cacheKey]
	conn.optionContractMu.RUnlock()
	if !ok {
		t.Fatalf("optionContractCache missing entry for %q after portfolio seed", cacheKey)
	}
	if detail.ConID != 747397667 {
		t.Errorf("ConID = %d, want 747397667", detail.ConID)
	}
	// Exchange must stay blank so SubscribeOption's "SMART" default
	// persists through applyContractDetailLite (which only overwrites on
	// non-empty cache values). PrimaryExch holds the actual listing venue.
	if detail.Exchange != "" {
		t.Errorf("Exchange = %q, want empty (so SMART default survives)", detail.Exchange)
	}
	if detail.PrimaryExch != "AMEX" {
		t.Errorf("PrimaryExch = %q, want AMEX", detail.PrimaryExch)
	}
	if detail.TradingClass != "AMZN" {
		t.Errorf("TradingClass = %q, want AMZN", detail.TradingClass)
	}
}

// TestHandlePortfolioValueIgnoresStockForOptionCache verifies that stock
// positions do not pollute the option cache.
func TestHandlePortfolioValueIgnoresStockForOptionCache(t *testing.T) {
	conn := &Connection{
		positions:           map[string]*RawPosition{},
		positionsMu:         sync.RWMutex{},
		optionContractCache: map[string]ContractDetailsLite{},
		optionContractMu:    sync.RWMutex{},
	}

	fields := []string{
		"7", "8",
		"4391", "AMD", "STK", "", "0", "", "100",
		"NASDAQ", "USD", "AMD", "AMD",
		"100", "438.50", "43850.00", "374.01", "6448.99", "0.00", "DU1234567",
	}
	conn.handlePortfolioValue(fields)

	conn.optionContractMu.RLock()
	defer conn.optionContractMu.RUnlock()
	if len(conn.optionContractCache) != 0 {
		t.Fatalf("optionContractCache should be empty after STK position, got %d entries",
			len(conn.optionContractCache))
	}
}

func TestHandlePortfolioValueZeroQuantityDeletesCachedPosition(t *testing.T) {
	conn := &Connection{
		positions:           map[string]*RawPosition{},
		positionsMu:         sync.RWMutex{},
		optionContractCache: map[string]ContractDetailsLite{},
		optionContractMu:    sync.RWMutex{},
	}

	open := []string{
		"7", "8",
		"999001", "GME", "OPT", "20260618", "30", "C", "100",
		"AMEX", "USD", "GME 260618C30", "GME",
		"1", "0.14", "14.00", "0.00", "0.00", "-3899.40", "DU1234567",
	}
	closed := append([]string{}, open...)
	closed[13] = "0"
	closed[15] = "0.00"

	conn.handlePortfolioValue(open)
	if got := len(conn.GetPositions()); got != 1 {
		t.Fatalf("positions after open update = %d, want 1", got)
	}
	conn.handlePortfolioValue(closed)
	if got := len(conn.GetPositions()); got != 0 {
		t.Fatalf("positions after zero-quantity update = %d, want 0: %+v", got, conn.GetPositions())
	}
}

func TestHandlePositionZeroQuantityDeletesCachedPosition(t *testing.T) {
	conn := &Connection{
		positions:   map[string]*RawPosition{},
		positionsMu: sync.RWMutex{},
	}

	open := []string{
		"61", "3", "DU1234567", "265598", "AAPL", "STK", "1",
		"NASDAQ", "USD", "AAPL", "AAPL", "100", "195.00",
	}
	closed := append([]string{}, open...)
	closed[11] = "0"

	conn.handlePosition(open)
	if got := len(conn.GetPositions()); got != 1 {
		t.Fatalf("positions after open update = %d, want 1", got)
	}
	conn.handlePosition(closed)
	if got := len(conn.GetPositions()); got != 0 {
		t.Fatalf("positions after zero-quantity update = %d, want 0: %+v", got, conn.GetPositions())
	}
}
