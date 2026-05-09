package ibkr

import "testing"

func TestSeedContractCacheFromPositions(t *testing.T) {
	conn := NewConnector(nil)

	positions := map[string]*RawPosition{
		"bb": {
			Contract: Contract{
				Symbol:       "bb",
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
