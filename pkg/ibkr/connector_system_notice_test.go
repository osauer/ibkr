package ibkr

import "testing"

func TestProcessSystemNoticeSkipsDerivativeInactive(t *testing.T) {
	c := &Connector{}
	alias := reqAliasEntry{
		symbol:      "AMD",
		secType:     "OPT",
		localSymbol: "AMD 20251121C250",
	}
	note := &systemNotification{
		code:    200,
		message: "No security definition has been found for the request",
	}

	c.processSystemNotice(alias, note)

	if c.IsSymbolInactive("AMD") {
		t.Fatalf("expected AMD to remain active despite option system notice")
	}
}

func TestProcessSystemNoticeMarksStockInactive(t *testing.T) {
	c := &Connector{}
	alias := reqAliasEntry{
		symbol:  "HGENQ",
		secType: "STK",
	}
	note := &systemNotification{
		code:    200,
		message: "No security definition has been found for the request",
	}

	c.processSystemNotice(alias, note)

	if !c.IsSymbolInactive("HGENQ") {
		t.Fatalf("expected HGENQ to be marked inactive")
	}
}

func TestProcessSystemNoticeMarksRoutedStockInactiveByRoute(t *testing.T) {
	c := &Connector{}
	alias := reqAliasEntry{
		symbol:   "MBG",
		secType:  "STK",
		exchange: "SMART",
		currency: "USD",
	}
	note := &systemNotification{
		code:    200,
		message: "No security definition has been found for the request",
	}

	c.processSystemNotice(alias, note)

	if c.IsSymbolInactive("MBG") {
		t.Fatalf("bare MBG should remain usable for an explicit non-US route")
	}
	key := MarketDataKeyForContract(Contract{Symbol: "MBG", SecType: "STK", Exchange: "SMART", Currency: "USD"})
	if !c.IsSymbolInactive(key) {
		t.Fatalf("expected failed route %q to be marked inactive", key)
	}
}
