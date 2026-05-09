package ibkr

import "testing"

func TestSeedContractCacheFromPositions(t *testing.T) {
	conn := NewConnector(nil)

	positions := map[string]*RawPosition{
		"bb": {
			Contract: Contract{
				Symbol:       "bb",
				SecType:      "STK",
				ConID:        12345,
				Exchange:     "NYSE",
				PrimaryExch:  "NYSE",
				LocalSymbol:  "BB",
				TradingClass: "NYSE",
			},
		},
		"duplicate": {
			Contract: Contract{
				Symbol:      "BB",
				SecType:     "STK",
				ConID:       12345,
				PrimaryExch: "SMART",
			},
		},
	}

	conn.seedContractCacheFromPositions(positions)

	conn.contractMu.RLock()
	detail, ok := conn.contractCache["BB"]
	conn.contractMu.RUnlock()
	if !ok {
		t.Fatalf("expected contract cache entry for BB")
	}
	if detail.ConID != 12345 {
		t.Fatalf("expected conID 12345, got %d", detail.ConID)
	}
	if detail.Exchange != "NYSE" {
		t.Fatalf("expected exchange NYSE, got %s", detail.Exchange)
	}
	if detail.PrimaryExch != "NYSE" {
		t.Fatalf("expected primary exchange NYSE, got %s", detail.PrimaryExch)
	}
	if detail.LocalSymbol != "BB" {
		t.Fatalf("expected local symbol BB, got %s", detail.LocalSymbol)
	}
	if detail.TradingClass != "NYSE" {
		t.Fatalf("expected trading class NYSE, got %s", detail.TradingClass)
	}
}

// Held option positions must not seed the bare-symbol cache; doing so
// caused `quote SPY` to return the option's pricing (~$4.89) instead
// of the ETF's (~$700) because prepareContract picked up the option's
// ConID for what was supposed to be a stock subscribe.
func TestSeedContractCacheSkipsNonStockPositions(t *testing.T) {
	conn := NewConnector(nil)

	positions := map[string]*RawPosition{
		"spy-put": {
			Contract: Contract{
				Symbol:       "SPY",
				SecType:      "OPT",
				Right:        "P",
				Strike:       700,
				Expiry:       "20260618",
				ConID:        7777777,
				Exchange:     "SMART",
				PrimaryExch:  "AMEX",
				LocalSymbol:  "SPY   260618P00700000",
				TradingClass: "SPY",
			},
		},
		"vix-call": {
			Contract: Contract{
				Symbol:  "VIX",
				SecType: "OPT",
				ConID:   888888,
			},
		},
	}

	conn.seedContractCacheFromPositions(positions)

	conn.contractMu.RLock()
	_, spyCached := conn.contractCache["SPY"]
	_, vixCached := conn.contractCache["VIX"]
	conn.contractMu.RUnlock()

	if spyCached {
		t.Fatalf("option position must not seed bare-symbol cache for SPY")
	}
	if vixCached {
		t.Fatalf("option position must not seed bare-symbol cache for VIX")
	}
}

// When a stock and an option for the same underlying are both held,
// only the stock's ConID may seed the cache. The merge order does not
// matter; the option must always be filtered out before merge.
func TestSeedContractCachePrefersStockOverOption(t *testing.T) {
	conn := NewConnector(nil)

	positions := map[string]*RawPosition{
		"amd-stock": {
			Contract: Contract{
				Symbol:      "AMD",
				SecType:     "STK",
				ConID:       111,
				PrimaryExch: "NASDAQ",
			},
		},
		"amd-call": {
			Contract: Contract{
				Symbol:  "AMD",
				SecType: "OPT",
				ConID:   222,
			},
		},
	}

	conn.seedContractCacheFromPositions(positions)

	conn.contractMu.RLock()
	detail, ok := conn.contractCache["AMD"]
	conn.contractMu.RUnlock()
	if !ok {
		t.Fatalf("expected stock position to seed AMD cache")
	}
	if detail.ConID != 111 {
		t.Fatalf("expected stock conID 111, got option's %d", detail.ConID)
	}
}

func TestFetchContractDetailsUsesCache(t *testing.T) {
	conn := NewConnector(nil)
	conn.contractMu.Lock()
	conn.contractCache["BB"] = ContractDetailsLite{
		Symbol:      "BB",
		ConID:       998877,
		Exchange:    "NYSE",
		PrimaryExch: "NYSE",
		LocalSymbol: "BB",
	}
	conn.contractMu.Unlock()

	details, err := conn.FetchContractDetails("bb", 0)
	if err != nil {
		t.Fatalf("FetchContractDetails returned error: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("expected 1 contract detail, got %d", len(details))
	}
	if details[0].ConID != 998877 {
		t.Fatalf("expected conID 998877, got %d", details[0].ConID)
	}
}
