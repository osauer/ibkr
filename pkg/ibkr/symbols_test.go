package ibkr

import "testing"

func TestClassifySymbol_Table(t *testing.T) {
	tests := []struct {
		sym      string
		secType  string
		exchange string
		currency string
		primary  string
	}{
		{"VIX", "IND", "CBOE", "USD", "CBOE"},
		{"SPY", "STK", "SMART", "USD", "ARCA"},
		{"GLD", "STK", "SMART", "USD", "ARCA"},
		{"TLT", "STK", "SMART", "USD", "ARCA"},
		{"ES", "FUT", "GLOBEX", "USD", ""},
		{"NDX", "IND", "NASDAQ", "USD", "NASDAQ"},
		{"AAPL", "STK", "SMART", "USD", ""},
	}

	for _, tc := range tests {
		sec, exch, cur, prim := classifySymbol(tc.sym)
		if sec != tc.secType || exch != tc.exchange || cur != tc.currency || prim != tc.primary {
			t.Fatalf("%s mapping wrong: got %s,%s,%s,%s; want %s,%s,%s,%s",
				tc.sym, sec, exch, cur, prim, tc.secType, tc.exchange, tc.currency, tc.primary)
		}
	}
}

func TestContractDisplayHints(t *testing.T) {
	tests := []struct {
		symbol string
		sec    string
		local  string
		class  string
	}{
		{"SPY", "STK", "", ""},
		{"VIX", "IND", "VIX", "VIX"},
		{"DXY", "IND", "DXY", "DXY"},
		{"ES", "FUT", "", ""},
	}

	for _, tc := range tests {
		local, class := contractDisplayHints(tc.symbol, tc.sec)
		if local != tc.local || class != tc.class {
			t.Fatalf("contractDisplayHints(%q,%q) = %q,%q want %q,%q",
				tc.symbol, tc.sec, local, class, tc.local, tc.class)
		}
	}
}
