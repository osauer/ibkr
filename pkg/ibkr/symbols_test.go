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
		{"VIX3M", "IND", "CBOE", "USD", "CBOE"},
		{"SPY", "STK", "SMART", "USD", "ARCA"},
		{"GLD", "STK", "SMART", "USD", "ARCA"},
		{"TLT", "STK", "SMART", "USD", "ARCA"},
		// HYG (iShares iBoxx High Yield Corporate Bond ETF) is on
		// ArcaEdge like the other ETFs above. The regime dashboard's
		// HYG/SPY divergence row needs `FetchHistoricalDailyBars(HYG)`
		// to compute HYG's 50DMA; without primary="ARCA" the gateway
		// has no fast-lookup hint and the contract-details round trip
		// overruns the regime fetcher's budget, leaving hyg_50dma
		// null on every cold-start call.
		{"HYG", "STK", "SMART", "USD", "ARCA"},
		{"ES", "FUT", "GLOBEX", "USD", ""},
		{"NDX", "IND", "NASDAQ", "USD", "NASDAQ"},
		{"AAPL", "STK", "SMART", "USD", ""},
		// FX pairs route to IDEALPRO with the quote currency on
		// Currency. Both dotted (canonical wire form) and slash
		// (human-readable) inputs classify identically.
		{"USD.JPY", "CASH", "IDEALPRO", "JPY", "IDEALPRO"},
		{"USD/JPY", "CASH", "IDEALPRO", "JPY", "IDEALPRO"},
		{"EUR.USD", "CASH", "IDEALPRO", "USD", "IDEALPRO"},
		{"GBP.USD", "CASH", "IDEALPRO", "USD", "IDEALPRO"},
		{"AUD.NZD", "CASH", "IDEALPRO", "NZD", "IDEALPRO"},
		// Lookalikes that must NOT classify as FX: unknown 3-letter
		// codes (XYZ), non-3-letter legs (BRK.B), and tickers with a
		// dot/slash that aren't G10 pairs.
		{"BRK.B", "STK", "SMART", "USD", ""},
		{"XYZ.USD", "STK", "SMART", "USD", ""},
	}

	for _, tc := range tests {
		sec, exch, cur, prim := classifySymbol(tc.sym)
		if sec != tc.secType || exch != tc.exchange || cur != tc.currency || prim != tc.primary {
			t.Fatalf("%s mapping wrong: got %s,%s,%s,%s; want %s,%s,%s,%s",
				tc.sym, sec, exch, cur, prim, tc.secType, tc.exchange, tc.currency, tc.primary)
		}
	}
}

func TestFxPair(t *testing.T) {
	tests := []struct {
		sym   string
		base  string
		quote string
		ok    bool
	}{
		{"USD.JPY", "USD", "JPY", true},
		{"USD/JPY", "USD", "JPY", true},
		{"eur.usd", "EUR", "USD", true},
		{" GBP.USD ", "GBP", "USD", true},
		{"AUD.NZD", "AUD", "NZD", true},
		{"CHF.CAD", "CHF", "CAD", true},
		// Not FX: unknown legs, missing/extra separator, wrong length.
		{"BRK.B", "", "", false},
		{"XYZ.USD", "", "", false},
		{"USD.XYZ", "", "", false},
		{"AAPL", "", "", false},
		{"USD.JPY.EUR", "", "", false},
		{"USDJPY", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		base, quote, ok := FxPair(tc.sym)
		if base != tc.base || quote != tc.quote || ok != tc.ok {
			t.Fatalf("FxPair(%q) = (%q,%q,%v); want (%q,%q,%v)",
				tc.sym, base, quote, ok, tc.base, tc.quote, tc.ok)
		}
	}
}

func TestDefaultHistoricalWhat_FXIsMidpoint(t *testing.T) {
	// FX has no consolidated trade tape — reqHistoricalData on CASH
	// requires MIDPOINT. Pin this so a future refactor of
	// defaultHistoricalWhat doesn't silently revert it to TRADES.
	if got := defaultHistoricalWhat("CASH"); got != "MIDPOINT" {
		t.Fatalf("defaultHistoricalWhat(CASH) = %q; want MIDPOINT", got)
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
