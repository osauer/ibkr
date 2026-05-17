package ibkr

// Symbol classification helpers.
//
// IBKR requires correct contract hints (secType, exchange, primary) for
// subscription stability, especially for indices and off-hours routing.
// classifySymbol encodes minimal, stable hints we use consistently across
// reqMktData and reqContractData.

// classifySymbol returns (secType, exchange, currency, primaryExchangeHint)
// for common indices/ETFs/stocks to keep contract requests and market data
// routing consistent across the codebase.
func classifySymbol(symbol string) (string, string, string, string) {
	// Defaults
	secType := "STK"
	exchange := "SMART"
	currency := "USD"
	primary := ""

	switch symbol {
	// Broad indices
	case "VIX":
		secType = "IND"
		exchange = "CBOE"
		primary = "CBOE"
	case "SPX":
		secType = "IND"
		exchange = "CBOE"
		primary = "CBOE"
	case "NDX":
		secType = "IND"
		exchange = "NASDAQ"
		primary = "NASDAQ"
	case "DJI", "DJX":
		secType = "IND"
		exchange = "CBOE"
		primary = "CBOE"

	// The S&P 500 stocks-above-50DMA breadth index (S5FI family on
	// S&P DJI; MMFI on TradingView; $SPXA50R on StockCharts) is NOT
	// catalogued in IBKR's contract database under any of the standard
	// names — verified via reqContractDetails probe against the CBOE
	// US Indexes subscription. IBKR's "CBOE US Indexes" feed covers
	// tradeable CBOE-listed indices (SPX, VIX, RUT, NDX, SKEW, VVIX,
	// …); S&P DJI's breadth statistics are calculated, not listed, so
	// they're a different data product that IBKR doesn't appear to
	// redistribute. The breadth.spx endpoint therefore needs either a
	// constituent-fan-out fallback (compute from 500 daily bars) or
	// the dashboard treats Indicator 5 as manual-entry. See
	// docs/specs/risk-regime-dashboard.md for the disposition.

	// Common ETFs
	case "SPY", "QQQ", "IWM", "DIA", "GLD", "TLT":
		secType = "STK"
		exchange = "SMART"
		primary = "ARCA" // Often better coverage OOH

	// Dollar index (ICE US)
	case "DXY":
		secType = "IND"
		exchange = "ICEUS"
		primary = "ICEUS"

	// Common futures (base symbol hints only; contract month handled elsewhere)
	case "ES":
		secType = "FUT"
		exchange = "GLOBEX" // CME Globex for E-mini S&P

	default:
		// leave defaults
	}

	return secType, exchange, currency, primary
}

func contractDisplayHints(symbol, secType string) (string, string) {
	switch secType {
	case "IND":
		switch symbol {
		case "VIX", "SPX", "NDX", "DJI", "DJX", "DXY":
			return symbol, symbol
		}
	case "CMDTY":
		return symbol, symbol
	}
	return "", ""
}
