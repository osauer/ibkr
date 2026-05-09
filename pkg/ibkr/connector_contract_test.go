package ibkr

import "testing"

func TestParseContractDetailsLiteVersion1(t *testing.T) {
	fields := []string{
		"10", // message id
		"1",  // reqID (version omitted because serverVersion >= size rules)
		"GLD",
		"STK",
		"",  // last trade date / contract month
		"",  // strike
		"0", // right
		"",  // empty exchange placeholder (older payloads)
		"SMART",
		"USD",
		"GLD",
		"GLD",
		"GLD",
		"51529211",
		"0.01",
		"",                           // md size multiplier (deprecated)
		"ACTIVETIM,AD,ADDONT,ADJUST", // order types (partial)
	}

	lite, ok := parseContractDetailsLite(fields, 1, 203)
	if !ok {
		t.Fatalf("expected parseContractDetailsLite to succeed")
	}
	if lite.ConID != 51529211 {
		t.Fatalf("expected conID 51529211, got %d", lite.ConID)
	}
	if lite.LocalSymbol != "GLD" {
		t.Fatalf("expected local symbol GLD, got %q", lite.LocalSymbol)
	}
}

func TestNormalizeEquityRoutingUsesSmartExchange(t *testing.T) {
	contract := Contract{
		SecType:  "STK",
		Exchange: "NASDAQ",
	}

	normalizeEquityRouting(&contract, "")

	if contract.Exchange != "SMART" {
		t.Fatalf("expected exchange SMART, got %q", contract.Exchange)
	}
	if contract.PrimaryExch != "NASDAQ" {
		t.Fatalf("expected primary NASDAQ, got %q", contract.PrimaryExch)
	}
}

func TestNormalizeEquityRoutingRespectsFallback(t *testing.T) {
	contract := Contract{
		SecType:  "STK",
		Exchange: "SMART",
	}

	normalizeEquityRouting(&contract, "ARCA")

	if contract.Exchange != "SMART" {
		t.Fatalf("expected exchange SMART, got %q", contract.Exchange)
	}
	if contract.PrimaryExch != "ARCA" {
		t.Fatalf("expected primary ARCA, got %q", contract.PrimaryExch)
	}
}
