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
		{"VVIX", "IND", "CBOE", "USD", "CBOE"},
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
		// "ES" is Eversource Energy (S&P 500 utility), classified as
		// STK on SMART. A previous classifySymbol case mapped "ES" to
		// FUT/GLOBEX for the E-mini S&P futures, but no caller ever
		// used that path and it collided with the Eversource stock —
		// breadth's reqContractData returned code 200 "No security
		// definition has been found" for "ES" because the wire fields
		// said FUT but IBKR has no FUT with that bare symbol. Removed
		// in the breadth-spx convergence fix.
		{"ES", "STK", "SMART", "USD", ""},
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

// TestDualClassWireSymbol pins the S&P-ticker → IBKR-wire translation
// for dual-class shares. The S&P convention is dotted (BRK.B, BF.B)
// but IBKR's TWS API rejects that form with code 200 "No security
// definition has been found"; the canonical IBKR Symbol uses a space
// (BRK B, BF B). Without this mapping the breadth-spx fan-out silently
// drops Berkshire-B (a top-10 SPX member by weight) and Brown-Forman-B
// from every refresh.
//
// The translator matches the US dual-class convention by pattern (1–4
// uppercase letters + dot + single class letter), so a future SPX
// reconstitution that adds a dual-class name converts without a code
// change — and the test asserts on hypothetical examples (BRK.A,
// BIO.B, etc.) to lock in that future-proofing.
func TestDualClassWireSymbol(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// Current SPX members (the live cases).
		{"BRK.B", "BRK B"},
		{"BF.B", "BF B"},
		// Pattern-match cases not currently in SPX but matching the
		// dual-class convention — should translate too (future-proof).
		{"BRK.A", "BRK A"},
		{"BF.A", "BF A"},
		{"A.B", "A B"},       // 1-letter base, lower bound of regex
		{"WXYZ.A", "WXYZ A"}, // 4-letter base, upper bound of regex
		// Case-insensitive + whitespace tolerant.
		{"brk.b", "BRK B"},
		{" BRK.B ", "BRK B"},
		// Pass through: plain stocks, indices, ETFs.
		{"BRK", "BRK"},
		{"AAPL", "AAPL"},
		{"SPY", "SPY"},
		// Pass through: FX pairs (3-letter legs don't match the regex;
		// FxPair handles these separately).
		{"USD.JPY", "USD.JPY"},
		{"EUR.USD", "EUR.USD"},
		// Pass through: index probes with no dot (BPSPX, MMFI etc.).
		{"BPSPX", "BPSPX"},
		// Pass through: too long for the regex (5+ letter base).
		{"AAPLE.A", "AAPLE.A"},
		// Pass through: multi-character class suffix (not the
		// convention).
		{"BRK.BB", "BRK.BB"},
		// Empty + edge cases.
		{"", ""},
		{".", "."},
		{"BRK.", "BRK."},
	}
	for _, tc := range tests {
		if got := dualClassWireSymbol(tc.in); got != tc.want {
			t.Errorf("dualClassWireSymbol(%q) = %q, want %q", tc.in, got, tc.want)
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
		{"SPY", "OPT", "", "SPY"},
		{"VIX", "IND", "VIX", "VIX"},
		{"VVIX", "IND", "VVIX", "VVIX"},
		{"DXY", "IND", "DXY", "DXY"},
		{"ES", "STK", "", ""},
	}

	for _, tc := range tests {
		local, class := contractDisplayHints(tc.symbol, tc.sec)
		if local != tc.local || class != tc.class {
			t.Fatalf("contractDisplayHints(%q,%q) = %q,%q want %q,%q",
				tc.symbol, tc.sec, local, class, tc.local, tc.class)
		}
	}
}
